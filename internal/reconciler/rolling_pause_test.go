package reconciler

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/vm"
)

// The caller's lifecycle lock is released while a rolling update pauses, and
// the next update re-checks its target afterwards: a health restart that ran
// in the pause must not be undone or duplicated.
func TestRollingPauseReleasesCallerLockAndRechecksTarget(t *testing.T) {
	m := newFakeVMManager()
	r := NewWithNetworkManager(m, heldClaimsLogger(), nil, nil, "rolling", time.Minute)
	for _, name := range []string{"a", "b", "c"} {
		m.instances[name] = &vm.Instance{Name: name, Config: config.ServiceConfig{Name: name, Image: "/old"}, State: vm.StateRunning}
	}
	desired := config.NodeConfig{}
	for _, name := range []string{"a", "b", "c"} {
		desired.Services = append(desired.Services, config.ServiceConfig{Name: name, Image: "/new"})
	}
	var lifecycle sync.Mutex
	pauses := 0
	r.sleepFn = func(context.Context, time.Duration) error {
		if !lifecycle.TryLock() {
			t.Error("lifecycle lock held during the rolling pause")
			return nil
		}
		pauses++
		if pauses == 1 {
			// A restart in the pause already brought b to the desired config.
			m.instances["b"].Config.Image = "/new"
		}
		lifecycle.Unlock()
		return nil
	}
	lifecycle.Lock()
	ctx := WithPauseRelease(context.Background(), func() func() {
		lifecycle.Unlock()
		return lifecycle.Lock
	})
	if err := r.Reconcile(ctx, desired); err != nil {
		t.Fatal(err)
	}
	if lifecycle.TryLock() {
		t.Fatal("lifecycle lock was not reacquired after the pause")
	}
	if got := len(m.startCalls); got != 2 {
		t.Fatalf("starts=%v, want a and c only", m.startCalls)
	}
	for _, name := range m.startCalls {
		if name == "b" {
			t.Fatalf("b was replaced again after the pause: %v", m.startCalls)
		}
	}
}

// A VM that a failed health restart removed during a pause leaves the
// revision incomplete instead of being skipped and reported converged.
func TestRollingUpdateOfVanishedVMIsIncomplete(t *testing.T) {
	m := newFakeVMManager()
	r := NewWithNetworkManager(m, heldClaimsLogger(), nil, nil, "rolling", time.Minute)
	desired := config.NodeConfig{}
	for _, name := range []string{"a", "b"} {
		m.instances[name] = &vm.Instance{Name: name, Config: config.ServiceConfig{Name: name, Image: "/old"}, State: vm.StateRunning}
		desired.Services = append(desired.Services, config.ServiceConfig{Name: name, Image: "/new"})
	}
	r.sleepFn = func(context.Context, time.Duration) error {
		delete(m.instances, "b")
		return nil
	}
	err := r.Reconcile(context.Background(), desired)
	if !IsIncomplete(err) || !strings.Contains(err.Error(), "hold b") {
		t.Fatalf("Reconcile = %v, want b incomplete", err)
	}
}
