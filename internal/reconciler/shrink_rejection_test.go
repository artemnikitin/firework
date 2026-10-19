package reconciler

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/vm"
	"github.com/artemnikitin/firework/internal/volume"
)

// rejectingVMManager refuses one volume's shrink at preflight, the way the
// advisory pre-stop measurement does.
type rejectingVMManager struct {
	*fakeVMManager
	rejections     []volume.Rejection
	preflightCalls int
}

func (f *rejectingVMManager) Preflight(context.Context, config.ServiceConfig) ([]volume.Rejection, error) {
	f.preflightCalls++
	return f.rejections, nil
}

func volumeService(name string, size int64, generation int64) config.ServiceConfig {
	return config.ServiceConfig{
		Name: name, Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128,
		Volumes: []config.VolumeConfig{{
			Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data",
			SizeBytes: size, BoundNode: "node-1", ResizeGeneration: generation,
		}},
	}
}

func rejectingReconciler(t *testing.T, running config.ServiceConfig, applied int64) (*Reconciler, *rejectingVMManager) {
	t.Helper()
	manager := &rejectingVMManager{
		fakeVMManager: newFakeVMManager(),
		rejections: []volume.Rejection{{
			LogicalID: running.Name + "/data", ResizeGeneration: 2, AppliedGeneration: 1,
			RequestedSizeBytes: 2 * config.MiB, AppliedSizeBytes: applied, MinimumSizeBytes: 4 * config.MiB,
		}},
	}
	manager.instances[running.Name] = &vm.Instance{Name: running.Name, State: vm.StateRunning, Config: running}
	r := New(manager, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, "", 0).WithStateDir(t.TempDir())
	return r, manager
}

// A preflight refusal is advisory and the VM is still live, so it must be made
// terminal by clamping rather than by failing: the desired configuration stops
// differing from the running one, so no further update is planned. Failing it
// instead would re-measure on every tick forever.
func TestPreflightRejectionIsTerminalAndLeavesTheVMRunning(t *testing.T) {
	for _, strategy := range []string{"", "rolling"} {
		name := strategy
		if name == "" {
			name = "all-at-once"
		}
		t.Run(name, func(t *testing.T) {
			running := volumeService("app", 16*config.MiB, 1)
			r, manager := rejectingReconciler(t, running, 16*config.MiB)
			r.updateStrategy = strategy

			// The desired revision asks for a shrink the node will refuse.
			desired := config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{
				volumeService("app", 2*config.MiB, 2),
			}}

			if err := r.Reconcile(context.Background(), desired); err != nil {
				t.Fatalf("a refused shrink must not fail reconciliation: %v", err)
			}
			if len(manager.removeCalls) != 0 {
				t.Fatalf("the VM was stopped for a refused shrink: %v", manager.removeCalls)
			}
			if len(manager.startCalls) != 0 {
				t.Fatalf("the VM was restarted for a refused shrink: %v", manager.startCalls)
			}
			if manager.instances["app"].State != vm.StateRunning {
				t.Fatal("the VM must stay running through a refused shrink")
			}

			// The following ticks must plan no further update. The agent-side
			// normalization is what makes this hold in production; here the
			// clamp inside the apply path already settles it.
			for tick := 0; tick < 2; tick++ {
				if err := r.Reconcile(context.Background(), desired); err != nil {
					t.Fatalf("tick %d: %v", tick, err)
				}
				if len(manager.removeCalls) != 0 || len(manager.startCalls) != 0 {
					t.Fatalf("tick %d stopped or restarted the service: removes=%v starts=%v",
						tick, manager.removeCalls, manager.startCalls)
				}
			}
		})
	}
}

// An update that changes the image *and* requests a refused shrink must still
// deploy the image. This falls out of clamping rather than blocking: failing
// the preflight would wedge every unrelated change behind a refused size.
func TestMixedUpdateStillDeploysTheImageWithNoResize(t *testing.T) {
	running := volumeService("app", 16*config.MiB, 1)
	r, manager := rejectingReconciler(t, running, 16*config.MiB)

	updated := volumeService("app", 2*config.MiB, 2)
	updated.Image = "/image-v2"
	desired := config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{updated}}

	if err := r.Reconcile(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	if len(manager.startCalls) != 1 {
		t.Fatalf("expected the image change to be deployed, got starts %v", manager.startCalls)
	}
	started := manager.instances["app"].Config
	if started.Image != "/image-v2" {
		t.Fatalf("the new image was not deployed: %q", started.Image)
	}
	if started.Volumes[0].SizeBytes != 16*config.MiB {
		t.Fatalf("the refused size reached the launch path: %d", started.Volumes[0].SizeBytes)
	}
}
