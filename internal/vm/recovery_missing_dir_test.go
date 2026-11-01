package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

// Inventory can read the directory list just before a concurrent owner removes
// an entry: a failed health-restart start, or a clear that released its
// reservation. The stale entry must not become a permanent unknown-claims
// placeholder that fences every allocation on the node.
func TestInventoryOfRemovedDirectoryLeavesNoPlaceholder(t *testing.T) {
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

// A quarantined record whose directory disappeared is retried by the periodic
// resolver, not only by an explicit clear.
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
	if got := m.List()[f.Service]; got != nil {
		t.Fatalf("missing-directory quarantine was not retried: %s %s", got.State, got.LastError)
	}
}

// Removing the whole vms directory leaves only in-memory quarantines. They are
// still retried and cleared once their absence is proved.
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
	if got := m.List()[f.Service]; got != nil {
		t.Fatalf("quarantine survived removal of the vms directory: %s %s", got.State, got.LastError)
	}
}
