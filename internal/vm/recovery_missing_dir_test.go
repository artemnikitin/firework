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
// C2/L4: a Remove whose intent write fails has removed nothing, so it leaves
// no evidence of its own removal. Here the marker path is a directory: the
// VM directory is then removed by hand, a start fails in volume preparation
// and drops the tracked VM, and inventory quarantines the service from its
// marker entry. The missing state stays fenced until --forget.
// (Candidate #4 from the review of a2df89d.)
func TestFailedRemoveIntentLeavesNoOwnRemovalEvidence(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	delete(inspector.identities, f.PID)
	f.PID, f.Lifecycle = 0, lifecycleStopped
	if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.instances[f.Service] = instanceFromManifest(f, StateStopped, "")
	m.mu.Unlock()
	if err := os.Mkdir(m.intentPath(f.Service), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(f.Service); err == nil {
		t.Fatal("setup: Remove wrote its intent over a directory")
	}
	if err := os.RemoveAll(f.VMDir); err != nil {
		t.Fatal(err)
	}
	svc := f.Config
	svc.Volumes = []config.VolumeConfig{{Name: "data", SizeBytes: 1024}}
	if err := m.Start(context.Background(), svc); err == nil || m.List()[f.Service] != nil {
		t.Fatalf("setup: the start did not fail in volume preparation: %v", err)
	}
	if err := m.DiscoverRecoveryState(); err != nil {
		t.Fatal(err)
	}
	recorded := 0
	m.SetRecoveryCleanupRecorder(func(config.ServiceConfig) error { recorded++; return nil })
	for pass := 0; pass < 2; pass++ {
		if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
			t.Fatal(err)
		}
	}
	if inst := m.List()[f.Service]; inst == nil || inst.State != StateRecoveryPending || recorded != 0 {
		t.Fatalf("unexplained missing state was cleared without --forget: %+v recorded=%d", inst, recorded)
	}
	if info, err := os.Lstat(m.intentPath(f.Service)); err != nil || !info.IsDir() {
		t.Fatalf("automatic recovery retired the marker without --forget: %v", err)
	}
	if result, err := m.ForgetRecoveryPending(f.Service); err != nil || result.Outcome != RecoveryOutcomeCleared || recorded != 1 {
		t.Fatalf("forget: %+v %v recorded=%d", result, err, recorded)
	}
}

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
