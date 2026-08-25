package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/statusmodel"
	"github.com/artemnikitin/firework/internal/vm"
)

// abortingVMManager fails every start the way a start aborted by a concurrent
// stop fails, and can be switched to a genuine failure or to success.
type abortingVMManager struct {
	starts int
	err    error
}

func (m *abortingVMManager) List() map[string]*vm.Instance { return map[string]*vm.Instance{} }
func (m *abortingVMManager) Start(context.Context, config.ServiceConfig) error {
	m.starts++
	return m.err
}
func (m *abortingVMManager) Remove(string) error { return nil }

func abortStore() *fakeStore {
	return &fakeStore{
		data: map[string][]byte{"web": []byte(`node: web
services:
  - name: app
    image: /image
    kernel: /kernel
    vcpus: 1
    memory_mb: 128
`)},
		revision: "rev-1",
	}
}

func agentWithVMManager(t *testing.T, s *fakeStore, manager reconciler.VMManager, strategy string) *Agent {
	t.Helper()
	cfg := testAgentConfig(t)
	cfg.UpdateStrategy = strategy
	a := New(cfg, s, testLogger())
	a.reconciler = reconciler.New(manager, slog.New(slog.NewTextHandler(discardWriter{}, nil)), nil, nil, strategy, 0).
		WithStateDir(cfg.StateDir)
	return a
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// An aborted start is a benign race, not a failure — but it is also not
// success. If the tick returned as though it had converged, lastRevision would
// advance and the next tick would take the unchanged-revision shortcut, leaving
// the service down until the revision itself changed.
func TestTick_AbortedStartDoesNotAdvanceRevisionAndReplans(t *testing.T) {
	for _, strategy := range []string{"", "rolling"} {
		name := strategy
		if name == "" {
			name = "all-at-once"
		}
		t.Run(name, func(t *testing.T) {
			manager := &abortingVMManager{err: fmt.Errorf("service app: %w", vm.ErrStartAborted)}
			a := agentWithVMManager(t, abortStore(), manager, strategy)

			a.tick(context.Background())
			if a.lastRevision != "" {
				t.Fatalf("aborted start advanced lastRevision to %q", a.lastRevision)
			}
			status := a.agentStatusSnapshot()
			if status.AppliedRevision != "" {
				t.Fatalf("aborted start claimed applied revision %q", status.AppliedRevision)
			}
			if status.Phase == statusmodel.PhaseFailed {
				t.Fatal("a benign start race must not publish the node as failed")
			}
			condition, ok := agentCondition(status, "Reconciled")
			if !ok || condition.Status != statusmodel.ConditionUnknown || condition.ReasonCode != "reconcile_incomplete" {
				t.Fatalf("expected an incomplete Reconciled condition, got %#v", condition)
			}

			// The next tick must re-plan rather than short-circuit on the
			// unchanged revision.
			a.tick(context.Background())
			if manager.starts != 2 {
				t.Fatalf("expected the next tick to retry the start, got %d starts", manager.starts)
			}

			// Once the race clears, the tick converges normally.
			manager.err = nil
			a.tick(context.Background())
			if a.lastRevision != "rev-1" {
				t.Fatalf("expected the revision to advance after a clean tick, got %q", a.lastRevision)
			}
		})
	}
}

// A batch that mixes an abort with a genuine failure is a failure. Both leave
// the revision unchanged; the difference is what the node reports.
func TestTick_GenuineStartFailureIsStillReportedAsFailed(t *testing.T) {
	manager := &abortingVMManager{err: errors.New("firecracker exited immediately")}
	a := agentWithVMManager(t, abortStore(), manager, "")

	a.tick(context.Background())

	status := a.agentStatusSnapshot()
	if status.Phase != statusmodel.PhaseFailed {
		t.Fatalf("expected a genuine start failure to publish a failed node, got %v", status.Phase)
	}
	condition, ok := agentCondition(status, "Reconciled")
	if !ok || condition.Status != statusmodel.ConditionFalse {
		t.Fatalf("expected a false Reconciled condition, got %#v", condition)
	}
	if a.lastRevision != "" {
		t.Fatalf("a failed tick must not advance lastRevision, got %q", a.lastRevision)
	}
}
