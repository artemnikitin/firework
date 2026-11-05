package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

// Inventory can read the directory list just before a concurrent owner removes
// an entry: a failed health-restart start, or a clear that released its
// reservation. The stale entry must not become a permanent unknown-claims
// placeholder that fences every allocation on the node: the manager removed
// the directory itself, so its own operation explains the disappearance.
func TestInventoryOfRemovedDirectoryLeavesNoPlaceholder(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	delete(inspector.identities, f.PID)
	root := filepath.Join(m.stateDir, "vms")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	m.markRemovedDir(f.Service) // as the concurrent removal does
	if err := os.RemoveAll(f.VMDir); err != nil {
		t.Fatal(err)
	}
	m.inventoryRecoveryEntries(root, entries)
	for i := 0; i < 3; i++ {
		if err := m.DiscoverRecoveryState(); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.List()[f.Service]; got != nil {
		t.Fatalf("removed directory left %s (unknown=%v): %s", got.State, got.RecoveryConfigUnknown, got.LastError)
	}
	if m.HasPendingRecovery() {
		t.Fatal("removed directory left recovery pending")
	}
}

// A directory removed by hand under a listed entry is not explained by any
// operation the agent recorded, so its fence stays until --forget.
func TestInventoryOfHandRemovedDirectoryKeepsItsFence(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	delete(inspector.identities, f.PID)
	root := filepath.Join(m.stateDir, "vms")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(f.VMDir); err != nil {
		t.Fatal(err)
	}
	m.inventoryRecoveryEntries(root, entries)
	for i := 0; i < 3; i++ {
		if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.List()[f.Service]; got == nil || got.State != StateRecoveryPending {
		t.Fatalf("a hand-removed directory lost its fence: %#v", got)
	}
	if result, err := m.ForgetRecoveryPending(f.Service); err != nil || result.Outcome != RecoveryOutcomeCleared {
		t.Fatalf("--forget: %#v %v", result, err)
	}
}

// A quarantined record whose directory was removed by hand keeps its fence
// across retries, and --forget clears it once its owner is proven absent.
func TestRecoverRetriesQuarantineWhoseDirectoryWasRemoved(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	inspector.errors[f.PID] = errors.New("transient inspection failure")
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if got := m.List()[f.Service]; got == nil || got.State != StateRecoveryPending {
		t.Fatalf("setup: not quarantined: %#v", got)
	}
	delete(inspector.errors, f.PID)
	delete(inspector.identities, f.PID)
	if err := os.RemoveAll(f.VMDir); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := m.DiscoverRecoveryState(); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.List()[f.Service]; got == nil || !strings.Contains(got.LastError, "--forget") {
		t.Fatalf("hand-removed quarantine was released without --forget: %#v", got)
	}
	if result, err := m.ForgetRecoveryPending(f.Service); err != nil || result.Outcome != RecoveryOutcomeCleared {
		t.Fatalf("--forget: %#v %v", result, err)
	}
}

// Removing the whole vms directory leaves only in-memory quarantines. They
// keep their fences until --forget clears them after an absence proof.
func TestRecoverRetriesQuarantineWhenStateRootIsRemoved(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	inspector.errors[f.PID] = errors.New("transient inspection failure")
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if got := m.List()[f.Service]; got == nil || got.State != StateRecoveryPending {
		t.Fatalf("setup: not quarantined: %#v", got)
	}
	delete(inspector.errors, f.PID)
	delete(inspector.identities, f.PID)
	if err := os.RemoveAll(filepath.Join(m.stateDir, "vms")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := m.DiscoverRecoveryState(); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.List()[f.Service]; got == nil || got.State != StateRecoveryPending {
		t.Fatalf("removing the vms directory released the quarantine: %#v", got)
	}
	if result, err := m.ForgetRecoveryPending(f.Service); err != nil || result.Outcome != RecoveryOutcomeCleared {
		t.Fatalf("--forget: %#v %v", result, err)
	}
}
