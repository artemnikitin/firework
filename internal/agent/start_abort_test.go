package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/statusmodel"
	"github.com/artemnikitin/firework/internal/vm"
	"github.com/artemnikitin/firework/internal/volume"
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

// A standing rejection must not read as ordinary convergence. The tick runs
// clean to the end — nothing failed — so without a distinct signal the node
// reports ready while running a size nobody asked for.
func TestTick_StandingVolumeRejectionDegradesRatherThanConverges(t *testing.T) {
	a := agentWithVMManager(t, abortStore(), &abortingVMManager{}, "")

	// No rejection: the condition is true and the node converges.
	a.tick(context.Background())
	clean := a.agentStatusSnapshot()
	if condition, ok := agentCondition(clean, "VolumeSizesApplied"); !ok || condition.Status != statusmodel.ConditionTrue {
		t.Fatalf("expected a satisfied volume-size condition, got %#v", condition)
	}
	if clean.Phase != statusmodel.PhaseReady {
		t.Fatalf("expected a clean tick to be ready, got %v", clean.Phase)
	}

	// A standing rejection degrades the node without failing it.
	a.vmManager.SeedVolumeRejectionsForTest(map[string]volume.Rejection{"app/data": {
		LogicalID: "app/data", ResizeGeneration: 2, AppliedGeneration: 1,
		RequestedSizeBytes: 2 << 20, AppliedSizeBytes: 16 << 20, MinimumSizeBytes: 4 << 20,
	}})
	a.lastRevision = ""
	a.tick(context.Background())

	status := a.agentStatusSnapshot()
	condition, ok := agentCondition(status, "VolumeSizesApplied")
	if !ok || condition.Status != statusmodel.ConditionFalse || condition.ReasonCode != "volume_size_rejected" {
		t.Fatalf("expected a degrading volume-size condition, got %#v", condition)
	}
	if !strings.Contains(condition.Message, "app/data") {
		t.Fatalf("expected the refused volume to be named, got %q", condition.Message)
	}
	// The workload is healthy, so this must degrade rather than fail: failing
	// the node over a wrong quota would be the worse outcome.
	if statusmodel.IsBlockingCondition("VolumeSizesApplied") {
		t.Fatal("a standing rejection must not be a blocking failure")
	}
	if status.Phase == statusmodel.PhaseFailed {
		t.Fatal("a standing rejection must not publish the node as failed")
	}
}
