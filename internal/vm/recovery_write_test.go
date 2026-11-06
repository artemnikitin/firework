package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

// Simulate a failed write whose readback is unavailable, with the rename
// either committed or not. The manager cannot tell which until reads recover.
func unreadableRecoveryWrite(t *testing.T, m *Manager, f *instanceManifest, landed bool) func() {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("run as non-root so the ownership manifest can be made unreadable")
	}
	path := manifestPath(f.VMDir)
	m.recoveryManifestWriter = func(path string, updated *instanceManifest) error {
		if landed {
			if err := writeManifest(path, updated); err != nil {
				return err
			}
		}
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		return errors.New("write outcome is unknown and manifest reads are unavailable")
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	return func() {
		t.Helper()
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		m.recoveryManifestWriter = nil
	}
}

// C3/C5: storage recovering is sufficient to finish the agent's own write,
// whether it changed the lifecycle during adoption or repaired process identity.
// A surviving owner is adopted; an exited one is cleared by the absence proof.
func TestRecoveryRetriesAfterUnreadableWriteSnapshot(t *testing.T) {
	for _, repair := range []bool{false, true} {
		for _, landed := range []bool{false, true} {
			for _, live := range []bool{false, true} {
				t.Run(fmt.Sprintf("repair=%v/landed=%v/live=%v", repair, landed, live), func(t *testing.T) {
					m, f, inspector := recoveryFixture(t)
					m.inspector = inspector
					f.Lifecycle = lifecycleStopping
					if repair {
						f.Executable, f.ExecutableDev, f.ExecutableIno = "/systemd", 1, 2
					}
					if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
						t.Fatal(err)
					}
					m.addRecoveryPending(f.Service, f, "interrupted stop")
					restore := unreadableRecoveryWrite(t, m, f, landed)
					result, err := m.ClearRecoveryPending(f.Service)
					if err == nil || result.Outcome != RecoveryOutcomeIncomplete {
						t.Fatalf("expected an incomplete write: %+v %v", result, err)
					}
					if _, err := m.ClearRecoveryPending(f.Service); err == nil || m.List()[f.Service].State != StateRecoveryPending {
						t.Fatal("unreadable evidence released the quarantine")
					}
					restore()
					if !live {
						delete(inspector.identities, f.PID)
					}
					for tick := 0; tick < 3; tick++ {
						if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
							t.Fatal(err)
						}
					}
					inst := m.List()[f.Service]
					if !live {
						if inst != nil {
							t.Fatalf("the absent owner stayed held after storage recovered: %+v", inst)
						}
						if _, err := os.Stat(f.VMDir); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("absent owner's state remains: %v", err)
						}
						return
					}
					if inst == nil || inst.State != StateRunning {
						t.Fatalf("adoption stayed stuck after storage recovered: %+v", inst)
					}
					durable, err := readManifest(manifestPath(f.VMDir))
					if err != nil || !sameRecoveryIdentity(inst.manifest, durable) || validateOwnedProcess(inspector, durable) != nil {
						t.Fatalf("adoption did not record the validated owner: %+v %v", durable, err)
					}
					stopAdoptedMonitors(t, m)
				})
			}
		}
	}
}

// C1: an uncertain write only explains its exact before/after identities.
// A different durable owner must never be overwritten or adopted from it.
func TestRecoveryWriteRetryRefusesAnUnrelatedManifest(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	f.Lifecycle = lifecycleStopping
	if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
		t.Fatal(err)
	}
	m.addRecoveryPending(f.Service, f, "interrupted stop")
	restore := unreadableRecoveryWrite(t, m, f, true)
	if result, err := m.ClearRecoveryPending(f.Service); err == nil || result.Outcome != RecoveryOutcomeIncomplete {
		t.Fatalf("setup: %+v %v", result, err)
	}
	restore()
	unrelated := cloneInstanceManifest(f)
	unrelated.InstanceID = "another-owner"
	if err := writeManifest(manifestPath(f.VMDir), unrelated); err != nil {
		t.Fatal(err)
	}
	m.recoveryManifestWriter = func(string, *instanceManifest) error {
		t.Fatal("an unrelated ownership record was overwritten")
		return nil
	}
	for tick := 0; tick < 3; tick++ {
		if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
			t.Fatal(err)
		}
	}
	if inst := m.List()[f.Service]; inst == nil || inst.State != StateRecoveryPending {
		t.Fatalf("the unrelated record released the quarantine: %+v", inst)
	}
	durable, err := readManifest(manifestPath(f.VMDir))
	if err != nil || durable.InstanceID != unrelated.InstanceID {
		t.Fatalf("the unrelated owner was not preserved: %+v %v", durable, err)
	}
}

// The pending write is in memory only. A fresh manager recovers from the
// durable record using the same full ownership checks, without needing it.
func TestRecoveryRestartsAfterUnreadableWriteSnapshot(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	f.Lifecycle = lifecycleStopping
	if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
		t.Fatal(err)
	}
	m.addRecoveryPending(f.Service, f, "interrupted stop")
	restore := unreadableRecoveryWrite(t, m, f, true)
	if result, err := m.ClearRecoveryPending(f.Service); err == nil || result.Outcome != RecoveryOutcomeIncomplete {
		t.Fatalf("setup: %+v %v", result, err)
	}
	restore()
	restarted := NewManager(m.firecrackerBin, m.stateDir, m.logger)
	restarted.inspector = inspector
	stopAdoptedMonitors(t, restarted)
	if _, err := restarted.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if inst := restarted.List()[f.Service]; inst == nil || inst.State != StateRunning {
		t.Fatalf("the restart did not recover its durable owner: %+v", inst)
	}
}

// An unresolved write constrains adoption, not the supported --forget path.
// If its manifest later disappears or is corrupt, absence-proven forget must
// work in the same agent process. A live before/after owner still refuses it.
func TestForgetAfterUncertainWriteAndLostMetadata(t *testing.T) {
	for _, missing := range []bool{false, true} {
		for _, live := range []bool{false, true} {
			t.Run(fmt.Sprintf("missing=%v/live=%v", missing, live), func(t *testing.T) {
				m, f, inspector := recoveryFixture(t)
				m.inspector = inspector
				f.Lifecycle = lifecycleStopping
				if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
					t.Fatal(err)
				}
				m.addRecoveryPending(f.Service, f, "interrupted stop")
				restore := unreadableRecoveryWrite(t, m, f, true)
				if result, err := m.ClearRecoveryPending(f.Service); err == nil || result.Outcome != RecoveryOutcomeIncomplete {
					t.Fatalf("setup: %+v %v", result, err)
				}
				restore()
				if missing {
					if err := os.Remove(manifestPath(f.VMDir)); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(manifestPath(f.VMDir), []byte("corrupt"), 0o600); err != nil {
					t.Fatal(err)
				}
				if !live {
					delete(inspector.identities, f.PID)
				}
				if result, err := m.ClearRecoveryPending(f.Service); err == nil || result.Outcome != RecoveryOutcomeAmbiguous {
					t.Fatalf("plain clear trusted lost metadata: %+v %v", result, err)
				}
				result, err := m.ForgetRecoveryPending(f.Service)
				if live {
					if err == nil || result.Outcome != RecoveryOutcomeAmbiguous {
						t.Fatalf("forget released a live owner: %+v %v", result, err)
					}
					return
				}
				if err != nil || result.Outcome != RecoveryOutcomeCleared || m.List()[f.Service] != nil {
					t.Fatalf("forget requires an agent restart: %+v %v", result, err)
				}
				if _, err := os.Stat(f.VMDir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("forgotten state remains: %v", err)
				}
			})
		}
	}
}

func TestForgetProvesBothUncertainWriteOwnersAbsent(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	m.addRecoveryPending(f.Service, f, "uncertain identity reconstruction")
	candidate := cloneInstanceManifest(f)
	candidate.PID, candidate.ProcessStart = 77, 200
	m.instances[f.Service].recoveryWriteCandidate = candidate
	delete(inspector.identities, f.PID)
	// This known candidate remains a claimant even if a process scan no
	// longer sees the canonical arguments.
	inspector.identities[77] = processIdentity{PID: 77, HostBootID: candidate.HostBootID, StartTicks: 200}
	if err := os.Remove(manifestPath(f.VMDir)); err != nil {
		t.Fatal(err)
	}
	if result, err := m.ForgetRecoveryPending(f.Service); err == nil || result.Outcome != RecoveryOutcomeAmbiguous {
		t.Fatalf("forget ignored its possible post-write owner: %+v %v", result, err)
	}
	if _, err := os.Stat(f.VMDir); err != nil {
		t.Fatalf("live candidate's state was removed: %v", err)
	}
}
