package vm

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

func directoryIntentCleanupFixture(t *testing.T, record string) (*Manager, *instanceManifest, *fakeProcessInspector, string) {
	t.Helper()
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	target := t.TempDir()
	preserved := filepath.Join(target, "preserved")
	if err := os.WriteFile(preserved, []byte("leave this target intact"), 0o600); err != nil {
		t.Fatal(err)
	}
	switch record {
	case "missing":
		if err := os.Remove(manifestPath(f.VMDir)); err != nil {
			t.Fatal(err)
		}
	case "corrupt":
		if err := os.WriteFile(manifestPath(f.VMDir), []byte("corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "symlink":
		if err := os.RemoveAll(f.VMDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, f.VMDir); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(m.intentPath(f.Service), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(m.intentPath(f.Service), "leave-target")); err != nil {
		t.Fatal(err)
	}
	return m, f, inspector, preserved
}

// C3: --forget repairs private bookkeeping for every runtime-state kind. A
// completed operation stays cleared across retries and a fresh manager.
func TestForgetRepairsDirectoryIntentWithRemainingState(t *testing.T) {
	for _, record := range []string{"trusted", "missing", "corrupt", "symlink"} {
		t.Run(record, func(t *testing.T) {
			m, f, inspector, preserved := directoryIntentCleanupFixture(t, record)
			delete(inspector.identities, f.PID)
			if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
				t.Fatal(err)
			}
			if info, err := os.Lstat(m.intentPath(f.Service)); err != nil || !info.IsDir() {
				t.Fatalf("automatic recovery repaired metadata without --forget: %v", err)
			}
			if result, err := m.ForgetRecoveryPending(f.Service); err != nil || result.Outcome != RecoveryOutcomeCleared {
				t.Fatalf("forget could not repair its private marker: %+v %v", result, err)
			}
			for pass := 0; pass < 2; pass++ {
				if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
					t.Fatal(err)
				}
			}
			if inst := m.List()[f.Service]; inst != nil {
				t.Fatalf("completed cleanup was quarantined again: %+v", inst)
			}
			for _, path := range []string{f.VMDir, m.intentPath(f.Service)} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("cleared state remains at %s: %v", path, err)
				}
			}
			if data, err := os.ReadFile(preserved); err != nil || string(data) != "leave this target intact" {
				t.Fatalf("cleanup followed a symlink: %q %v", data, err)
			}
			if result, err := m.ForgetRecoveryPending(f.Service); err != nil || result.Outcome != RecoveryOutcomeNoop {
				t.Fatalf("repeated forget did not settle: %+v %v", result, err)
			}
			restarted := NewManager(m.firecrackerBin, m.stateDir, m.logger)
			restarted.inspector = inspector
			if _, err := restarted.Recover(context.Background(), config.NodeConfig{}); err != nil || restarted.List()[f.Service] != nil {
				t.Fatalf("an agent restart recreated the quarantine: %+v %v", restarted.List()[f.Service], err)
			}
		})
	}
}

// C3: automatic recovery and a plain clear refuse a directory at the private
// marker path before recording any cleanup, and report that only --forget
// resolves it. --forget then records the teardown once and clears the VM.
// (Finding from the review of a2df89d.)
func TestPlainRecoveryRefusesDirectoryIntentBeforeJournaling(t *testing.T) {
	m, f, inspector, _ := directoryIntentCleanupFixture(t, "trusted")
	delete(inspector.identities, f.PID)
	calls := 0
	m.SetRecoveryCleanupRecorder(func(config.ServiceConfig) error { calls++; return nil })
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	result, err := m.ClearRecoveryPending(f.Service)
	if err == nil || result.Outcome != RecoveryOutcomeAmbiguous || !strings.Contains(result.Message, "--forget "+f.Service) {
		t.Fatalf("a plain clear did not refuse as needing --forget: %+v %v", result, err)
	}
	if calls != 0 {
		t.Fatalf("a refused recovery recorded its cleanup %d times", calls)
	}
	if inst := m.List()[f.Service]; inst == nil || inst.State != StateRecoveryPending {
		t.Fatalf("the refusal lost quarantine: %+v", inst)
	}
	if result, err := m.ForgetRecoveryPending(f.Service); err != nil || result.Outcome != RecoveryOutcomeCleared || calls != 1 {
		t.Fatalf("forget: %+v %v, cleanup recorded %d times", result, err, calls)
	}
}

// C1: the operator flag cannot repair even private metadata until the VM's
// complete absence is proven. Keep both the runtime entry and marker intact.
func TestForgetDirectoryIntentRefusesALiveClaimant(t *testing.T) {
	for _, record := range []string{"trusted", "missing", "corrupt", "symlink"} {
		t.Run(record, func(t *testing.T) {
			m, f, _, preserved := directoryIntentCleanupFixture(t, record)
			m.addRecoveryPending(f.Service, f, "ownership needs verification")
			if result, err := m.ForgetRecoveryPending(f.Service); err == nil || result.Outcome != RecoveryOutcomeAmbiguous {
				t.Fatalf("forget ignored the live owner: %+v %v", result, err)
			}
			for _, path := range []string{f.VMDir, m.intentPath(f.Service), preserved} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("refused cleanup changed %s: %v", path, err)
				}
			}
		})
	}
}

// C3: the VM's files survive a journal failure or an intent-write failure.
// After storage recovers the same command finishes without an agent restart.
func TestForgetMarkerCleanupPreservesVMOnPersistenceFailure(t *testing.T) {
	for _, stage := range []string{"journal", "intent write"} {
		t.Run(stage, func(t *testing.T) {
			if stage == "intent write" && os.Geteuid() == 0 {
				t.Skip("needs non-root directory permissions")
			}
			m, f, inspector, _ := directoryIntentCleanupFixture(t, "trusted")
			delete(inspector.identities, f.PID)
			m.addRecoveryPending(f.Service, f, "cleanup needs persistence")
			before, err := os.ReadFile(manifestPath(f.VMDir))
			if err != nil {
				t.Fatal(err)
			}
			var restore func()
			if stage == "journal" {
				m.SetRecoveryCleanupRecorder(func(config.ServiceConfig) error { return errors.New("journal unavailable") })
				restore = func() { m.SetRecoveryCleanupRecorder(nil) }
			} else {
				if err := os.RemoveAll(m.intentPath(f.Service)); err != nil {
					t.Fatal(err)
				}
				if err := m.markIntent(f.Service); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(m.stateDir)
				if err != nil {
					t.Fatal(err)
				}
				restore = func() { _ = os.Chmod(m.stateDir, info.Mode().Perm()) }
				if err := os.Chmod(m.stateDir, 0o555); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(restore)
			if result, err := m.ForgetRecoveryPending(f.Service); err == nil || result.Outcome != RecoveryOutcomeIncomplete {
				t.Fatalf("persistence failure was hidden: %+v %v", result, err)
			}
			after, err := os.ReadFile(manifestPath(f.VMDir))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("VM files changed before deletion intent was durable: %v", err)
			}
			if stage == "journal" {
				if info, err := os.Lstat(m.intentPath(f.Service)); err != nil || !info.IsDir() {
					t.Fatalf("private metadata changed before the journal accepted cleanup: %v", err)
				}
			}
			restore()
			if result, err := m.ForgetRecoveryPending(f.Service); err != nil || result.Outcome != RecoveryOutcomeCleared {
				t.Fatalf("cleanup did not retry after storage recovered: %+v %v", result, err)
			}
		})
	}
}

// The symlink may already be unlinked when private marker cleanup fails. That
// is incomplete, and a retry must finish the remaining work after a new proof.
func TestForgetSymlinkRetriesPrivateMarkerCleanup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("needs non-root directory permissions")
	}
	m, f, inspector, _ := directoryIntentCleanupFixture(t, "symlink")
	delete(inspector.identities, f.PID)
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(m.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	restore := func() { _ = os.Chmod(m.stateDir, info.Mode().Perm()) }
	t.Cleanup(restore)
	if err := os.Chmod(m.stateDir, 0o555); err != nil {
		t.Fatal(err)
	}
	if result, err := m.ForgetRecoveryPending(f.Service); err == nil || result.Outcome != RecoveryOutcomeIncomplete {
		t.Fatalf("partial symlink cleanup reported completion: %+v %v", result, err)
	}
	if _, err := os.Lstat(f.VMDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup: the runtime symlink was not removed: %v", err)
	}
	restore()
	if result, err := m.ForgetRecoveryPending(f.Service); err != nil || result.Outcome != RecoveryOutcomeCleared {
		t.Fatalf("partial cleanup did not retry: %+v %v", result, err)
	}
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil || m.List()[f.Service] != nil {
		t.Fatalf("completed retry recreated recovery state: %+v %v", m.List()[f.Service], err)
	}
}
