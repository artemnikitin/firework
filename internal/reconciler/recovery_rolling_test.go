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

func TestQuarantineDoesNotBlockIndependentUpdate(t *testing.T) {
	for _, strategy := range []string{"rolling", ""} {
		t.Run(strategy, func(t *testing.T) {
			manager := &rejectingVMManager{fakeVMManager: newFakeVMManager()}
			oldA := config.ServiceConfig{Name: "quarantined", Image: "/old"}
			oldB := config.ServiceConfig{Name: "independent", Image: "/old"}
			pending := &vm.Instance{Name: oldA.Name, Config: oldA, State: vm.StateRecoveryPending, LastError: "ownership is unresolved"}
			manager.instances[oldA.Name] = pending
			manager.instances[oldB.Name] = &vm.Instance{Name: oldB.Name, Config: oldB, State: vm.StateRunning}
			desired := config.NodeConfig{Services: []config.ServiceConfig{
				{Name: oldA.Name, Image: "/new"},
				{Name: oldB.Name, Image: "/new"},
			}}
			r := New(manager, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, strategy, 0).WithStateDir(t.TempDir())

			for tick := 0; tick < 2; tick++ {
				err := r.Reconcile(context.Background(), desired)
				if err != nil {
					t.Fatalf("tick %d quarantine blocked node reconciliation: %v", tick, err)
				}
				if manager.instances[oldA.Name] != pending || pending.State != vm.StateRecoveryPending {
					t.Fatal("quarantined VM was changed")
				}
				if got := manager.instances[oldB.Name]; got == nil || got.Config.Image != "/new" || got.State != vm.StateRunning {
					t.Fatalf("independent update did not converge: %#v", got)
				}
				if manager.preflightCalls != 1 || len(manager.removeCalls) != 1 || manager.removeCalls[0] != oldB.Name ||
					len(manager.startCalls) != 1 || manager.startCalls[0] != oldB.Name {
					t.Fatalf("quarantine reached preflight or mutations, or healthy work repeated: preflight=%d remove=%v start=%v",
						manager.preflightCalls, manager.removeCalls, manager.startCalls)
				}
			}

			// Once recovery re-adopts A, the next tick updates it and can converge.
			pending.State = vm.StateRunning
			if err := r.Reconcile(context.Background(), desired); err != nil {
				t.Fatal(err)
			}
			if got := manager.instances[oldA.Name]; got == nil || got.Config.Image != "/new" || got.State != vm.StateRunning {
				t.Fatalf("resolved service did not converge: %#v", got)
			}
		})
	}
}

func TestRollingOrdinaryUpdateFailureStillStopsLaterUpdates(t *testing.T) {
	for _, failure := range []string{"remove", "start"} {
		t.Run(failure, func(t *testing.T) {
			r := newTestReconciler("rolling", 0).WithStateDir(t.TempDir())
			manager := r.vmManager.(*fakeVMManager)
			for _, name := range []string{"first", "later"} {
				manager.instances[name] = &vm.Instance{Name: name, Config: config.ServiceConfig{Name: name, Image: "/old"}, State: vm.StateRunning}
			}
			if failure == "remove" {
				manager.removeErr = errors.New("stop failed")
			} else {
				manager.startErr = errors.New("launch failed")
			}
			err := r.Reconcile(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{
				{Name: "first", Image: "/new"},
				{Name: "later", Image: "/new"},
			}})
			if err == nil {
				t.Fatal("ordinary update failure was lost")
			}
			if len(manager.removeCalls) != 1 || manager.removeCalls[0] != "first" {
				t.Fatalf("continued rolling updates after ordinary failure: %v", manager.removeCalls)
			}
			if got := manager.instances["later"]; got == nil || got.Config.Image != "/old" {
				t.Fatalf("later service changed after ordinary failure: %#v", got)
			}
		})
	}
}

func TestQuarantineExcludedFromCreateUpdateAndDeletePlans(t *testing.T) {
	for _, strategy := range []string{"rolling", ""} {
		t.Run(strategy, func(t *testing.T) {
			r := newTestReconciler(strategy, 0)
			manager := r.vmManager.(*fakeVMManager)
			old := config.ServiceConfig{Name: "pending", Image: "/old"}
			manager.instances[old.Name] = &vm.Instance{Name: old.Name, Config: old, State: vm.StateRecoveryPending}
			for _, desired := range []config.NodeConfig{{}, {Services: []config.ServiceConfig{{Name: old.Name, Image: "/new"}}}} {
				if actions := r.Plan(desired); len(actions) != 0 {
					t.Fatalf("quarantine entered plan: %#v", actions)
				}
				if err := r.Reconcile(context.Background(), desired); err != nil {
					t.Fatal(err)
				}
			}
			if len(manager.removeCalls) != 0 || len(manager.startCalls) != 0 {
				t.Fatal("quarantine was mutated")
			}
		})
	}
}
