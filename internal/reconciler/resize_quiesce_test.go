package reconciler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/vm"
)

func TestAcceptedVolumeResizeUsesResizeRemovalInBothStrategies(t *testing.T) {
	for _, strategy := range []string{"", "rolling"} {
		name := strategy
		if name == "" {
			name = "all-at-once"
		}
		t.Run(name, func(t *testing.T) {
			previous := volumeService("app", 8*config.MiB, 1)
			manager := newFakeVMManager()
			manager.instances[previous.Name] = &vm.Instance{Name: previous.Name, State: vm.StateRunning, Config: previous}
			reconciler := New(manager, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, strategy, 0).WithStateDir(t.TempDir())

			if err := reconciler.Reconcile(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{
				volumeService("app", 16*config.MiB, 2),
			}}); err != nil {
				t.Fatal(err)
			}
			if len(manager.removeForResizeCalls) != 1 || manager.removeForResizeCalls[0] != "app" {
				t.Fatalf("accepted resize did not use RemoveForResize: %v", manager.removeForResizeCalls)
			}
			if len(manager.removeCalls) != 0 || len(manager.startCalls) != 1 {
				t.Fatalf("unexpected lifecycle calls: ordinary removes=%v starts=%v", manager.removeCalls, manager.startCalls)
			}
		})
	}
}

func TestFailedResizeQuiesceKeepsOldVMAndSkipsReplacement(t *testing.T) {
	previous := volumeService("app", 8*config.MiB, 1)
	manager := newFakeVMManager()
	old := &vm.Instance{Name: previous.Name, State: vm.StateRunning, Config: previous}
	manager.instances[previous.Name] = old
	manager.removeForResizeErr = errors.New("guest quiesce timed out")
	reconciler := New(manager, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, "", 0).WithStateDir(t.TempDir())

	err := reconciler.Reconcile(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{
		volumeService("app", 16*config.MiB, 2),
	}})
	if err == nil {
		t.Fatal("expected failed guest quiesce to fail the resize update")
	}
	if manager.instances[previous.Name] != old {
		t.Fatalf("failed resize replaced or removed old VM: %#v", manager.instances[previous.Name])
	}
	if len(manager.startCalls) != 0 || len(manager.removeCalls) != 0 || len(manager.removeForResizeCalls) != 1 {
		t.Fatalf("unexpected calls after failed resize: starts=%v ordinary-removes=%v resize-removes=%v",
			manager.startCalls, manager.removeCalls, manager.removeForResizeCalls)
	}
}

func TestNoopAndSameSizeDoNotUseResizeRemoval(t *testing.T) {
	previous := volumeService("app", 8*config.MiB, 1)
	manager := newFakeVMManager()
	manager.instances[previous.Name] = &vm.Instance{Name: previous.Name, State: vm.StateRunning, Config: previous}
	reconciler := New(manager, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, "", 0).WithStateDir(t.TempDir())

	if err := reconciler.Reconcile(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{previous}}); err != nil {
		t.Fatal(err)
	}
	if len(manager.removeCalls) != 0 || len(manager.removeForResizeCalls) != 0 || len(manager.startCalls) != 0 {
		t.Fatalf("no-op caused lifecycle work: ordinary-removes=%v resize-removes=%v starts=%v",
			manager.removeCalls, manager.removeForResizeCalls, manager.startCalls)
	}

	updated := previous
	updated.Image = "/image-v2"
	if err := reconciler.Reconcile(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{updated}}); err != nil {
		t.Fatal(err)
	}
	if len(manager.removeCalls) != 1 || len(manager.removeForResizeCalls) != 0 {
		t.Fatalf("same-size update used resize removal: ordinary-removes=%v resize-removes=%v",
			manager.removeCalls, manager.removeForResizeCalls)
	}
}

type vmManagerWithoutResizeRemoval struct {
	manager *fakeVMManager
}

func (m vmManagerWithoutResizeRemoval) List() map[string]*vm.Instance { return m.manager.List() }
func (m vmManagerWithoutResizeRemoval) Start(ctx context.Context, svc config.ServiceConfig) error {
	return m.manager.Start(ctx, svc)
}
func (m vmManagerWithoutResizeRemoval) Remove(name string) error { return m.manager.Remove(name) }

func TestResizeFailsClosedWhenVMManagerCannotRequireQuiesce(t *testing.T) {
	previous := volumeService("app", 8*config.MiB, 1)
	manager := newFakeVMManager()
	manager.instances[previous.Name] = &vm.Instance{Name: previous.Name, State: vm.StateRunning, Config: previous}
	reconciler := New(vmManagerWithoutResizeRemoval{manager: manager}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, "", 0).WithStateDir(t.TempDir())
	action := Action{Type: ActionUpdate, Service: volumeService("app", 16*config.MiB, 2), PreviousService: &previous}

	if err := reconciler.Apply(context.Background(), []Action{action}); err == nil {
		t.Fatal("resize proceeded without a VM manager that enforces guest quiesce")
	}
	if len(manager.removeCalls) != 0 || len(manager.startCalls) != 0 {
		t.Fatalf("unsupported manager destructively updated service: removes=%v starts=%v", manager.removeCalls, manager.startCalls)
	}
}
