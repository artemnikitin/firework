package reconciler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/vm"
)

func resizeSequenceFixture(t *testing.T, strategy string) (*Reconciler, *fakeVMManager, config.ServiceConfig, config.ServiceConfig) {
	t.Helper()
	old := volumeService("app", 8*config.MiB, 1)
	wanted := volumeService("app", 16*config.MiB, 2)
	manager := newFakeVMManager()
	manager.instances[old.Name] = &vm.Instance{Name: old.Name, InstanceID: "old-owner", State: vm.StateRunning, Config: old}
	manager.removeForResizeErr = &vm.GuestQuiesceRequiredError{Cause: errors.New("guest unavailable")}
	r := New(manager, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, strategy, 0).WithStateDir(t.TempDir())
	return r, manager, old, wanted
}

func TestResizeRefusalRetriesSameRevisionAndIndependentWorkContinues(t *testing.T) {
	for _, strategy := range []string{"", "rolling"} {
		t.Run("strategy="+strategy, func(t *testing.T) {
			r, manager, old, wanted := resizeSequenceFixture(t, strategy)
			other := config.ServiceConfig{Name: "other", Image: "/old", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}
			manager.instances[other.Name] = &vm.Instance{Name: other.Name, InstanceID: "other-owner", State: vm.StateRunning, Config: other}
			other.Image = "/new"
			desired := config.NodeConfig{RenderedRevision: "unchanged-revision", Services: []config.ServiceConfig{wanted, other}}
			first := r.Reconcile(context.Background(), desired)
			if !IsIncomplete(first) || !errors.Is(first, ErrServiceHeld) || errors.Is(first, ErrRecoveryHeld) {
				t.Fatalf("refusal must remain ordinary incomplete work: %v", first)
			}
			if manager.instances[old.Name].Config.Volumes[0].SizeBytes != old.Volumes[0].SizeBytes || !slices.Contains(manager.startCalls, other.Name) {
				t.Fatalf("old runtime lost or unrelated update stalled: starts=%v actual=%#v", manager.startCalls, manager.instances)
			}
			if reason := r.HeldServiceHolds(desired)[old.Name].Reason; !strings.Contains(reason, "guest_quiesce_required") {
				t.Fatalf("named refusal absent: %q", reason)
			}
			if planned := r.Plan(desired); len(planned) != 1 || planned[0].Service.Name != old.Name {
				t.Fatalf("reported refusal suppressed its retry: %#v", planned)
			}
			manager.removeForResizeErr = nil
			if err := r.Reconcile(context.Background(), desired); err != nil {
				t.Fatalf("same revision retry failed: %v", err)
			}
			if len(manager.removeForResizeCalls) != 2 || manager.instances[old.Name].Config.Volumes[0].SizeBytes != wanted.Volumes[0].SizeBytes {
				t.Fatalf("resize never completed: calls=%v instance=%#v", manager.removeForResizeCalls, manager.instances[old.Name])
			}
			if hold := r.HeldServiceHolds(desired)[old.Name]; hold.Reason != "" || hold.ApplicationExited {
				t.Fatalf("success retained old result: %#v", hold)
			}
		})
	}
}

func TestResizeAttemptClearsForChangedRequestAndOwner(t *testing.T) {
	for _, change := range []string{"withdraw", "new-size", "new-generation", "remove", "replace"} {
		t.Run(change, func(t *testing.T) {
			r, manager, old, wanted := resizeSequenceFixture(t, "rolling")
			desired := config.NodeConfig{Services: []config.ServiceConfig{wanted}}
			if err := r.Reconcile(context.Background(), desired); !errors.Is(err, ErrServiceHeld) {
				t.Fatal(err)
			}
			switch change {
			case "withdraw":
				desired.Services[0] = old
			case "new-size":
				desired.Services[0].Volumes = slices.Clone(wanted.Volumes)
				desired.Services[0].Volumes[0].SizeBytes *= 2
			case "new-generation":
				desired.Services[0].Volumes = slices.Clone(wanted.Volumes)
				desired.Services[0].Volumes[0].ResizeGeneration++
			case "remove":
				desired.Services = nil
			case "replace":
				replacement := *manager.instances[old.Name]
				replacement.InstanceID = "replacement"
				manager.instances[old.Name] = &replacement
			}
			r.pruneResizeAttempts(desired)
			if len(r.resizeAttempts) != 0 {
				t.Fatalf("stale request survived %s: %#v", change, r.resizeAttempts)
			}
			if change == "withdraw" {
				if err := r.Reconcile(context.Background(), desired); err != nil {
					t.Fatal(err)
				}
				if len(manager.startCalls) != 0 || len(manager.removeForResizeCalls) != 1 {
					t.Fatalf("withdrawal restarted VM: starts=%v resizes=%v", manager.startCalls, manager.removeForResizeCalls)
				}
			}
		})
	}
}

func TestKnownApplicationExitSurvivesRetryAndWithdrawal(t *testing.T) {
	r, manager, old, wanted := resizeSequenceFixture(t, "rolling")
	desired := config.NodeConfig{Services: []config.ServiceConfig{wanted}}
	manager.removeForResizeErr = &vm.GuestQuiesceRequiredError{Cause: errors.New("unmount busy"), ApplicationExited: true}
	if err := r.Reconcile(context.Background(), desired); !errors.Is(err, ErrServiceHeld) {
		t.Fatal(err)
	}
	manager.removeForResizeErr = &vm.GuestQuiesceRequiredError{Cause: errors.New("transport timed out")}
	if err := r.Reconcile(context.Background(), desired); !errors.Is(err, ErrServiceHeld) {
		t.Fatal(err)
	}
	if !r.HeldServiceHolds(desired)[old.Name].ApplicationExited {
		t.Fatal("uninformative retry erased known exit")
	}
	desired.Services[0] = old
	if err := r.Reconcile(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	observation := r.HeldServiceHolds(desired)[old.Name]
	if observation.Reason != "" || !observation.ApplicationExited {
		t.Fatalf("withdrawal falsely restored application: %#v", observation)
	}
	manager.instances[old.Name].State = vm.StateRecoveryPending
	r.pruneResizeAttempts(desired)
	manager.instances[old.Name].State = vm.StateRunning
	if !r.HeldServiceHolds(desired)[old.Name].ApplicationExited {
		t.Fatal("readoption of the same VM erased known application exit")
	}
	manager.instances[old.Name] = &vm.Instance{Name: old.Name, InstanceID: "new-owner", State: vm.StateRunning, Config: old}
	r.pruneResizeAttempts(desired)
	if r.HeldServiceHolds(desired)[old.Name].ApplicationExited {
		t.Fatal("old exit attached to new owner")
	}
}

func TestResizeRefusalRetainsClaimsForSiblingUpdateAndDelete(t *testing.T) {
	for _, strategy := range []string{"", "rolling"} {
		for _, deletion := range []bool{false, true} {
			name := "update"
			if deletion {
				name = "delete"
			}
			t.Run(strategy+"/"+name, func(t *testing.T) {
				r, manager, old, wanted := resizeSequenceFixture(t, strategy)
				old.Network = &config.NetworkConfig{Interface: "tap-shared"}
				wanted.Network = &config.NetworkConfig{Interface: "tap-next"}
				manager.instances[old.Name].Config = old
				sibling := config.ServiceConfig{Name: "sibling", Image: "/old", Network: &config.NetworkConfig{Interface: "tap-shared"}}
				if !deletion {
					sibling.Network = &config.NetworkConfig{Interface: "tap-other"}
				}
				manager.instances[sibling.Name] = &vm.Instance{Name: sibling.Name, InstanceID: "sibling-owner", State: vm.StateRunning, Config: sibling}
				desired := config.NodeConfig{Services: []config.ServiceConfig{wanted, sibling}}
				if err := r.Reconcile(context.Background(), desired); !errors.Is(err, ErrServiceHeld) {
					t.Fatal(err)
				}
				if deletion {
					desired.Services = desired.Services[:1]
				} else {
					sibling.Image = "/new"
					sibling.Network = &config.NetworkConfig{Interface: "tap-shared"}
					desired.Services[1] = sibling
				}
				if err := r.Reconcile(context.Background(), desired); !errors.Is(err, ErrServiceHeld) {
					t.Fatal(err)
				}
				if slices.Contains(manager.removeCalls, "sibling") || manager.instances["sibling"] == nil {
					t.Fatalf("retained claimant was removed: %v", manager.removeCalls)
				}
				if len(manager.removeForResizeCalls) != 2 {
					t.Fatalf("holder could not retry: %v", manager.removeForResizeCalls)
				}
			})
		}
	}
}
