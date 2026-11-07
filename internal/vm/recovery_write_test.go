package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
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

// C3 (L4 boundary): --forget after an uncertain write keeps the canonical
// absence proof, but the cleanup journal still receives the configuration the
// agent trusts in memory, before and as attempted by the write, so the VM's
// devices are torn down. Disk contents never contribute. A journal failure
// keeps the VM state and the quarantine until a retry records it.
// (Finding #1 from the review of 9167319.)
func TestForgetAfterUncertainWriteRecordsTrustedTeardown(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%v", missing), func(t *testing.T) {
			m, f, inspector := recoveryFixture(t)
			m.inspector = inspector
			f.Lifecycle = lifecycleStopping
			f.Config.Network = &config.NetworkConfig{Interface: "tap-old", HostDevName: "eth0", GuestIP: "172.16.0.2"}
			hash, err := serviceConfigHash(f.Config)
			if err != nil {
				t.Fatal(err)
			}
			f.ConfigHash = hash
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
			} else if err := os.WriteFile(manifestPath(f.VMDir), []byte(`{"service":"app","config":{"name":"app","network":{"interface":"tap-forged"}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			delete(inspector.identities, f.PID)
			m.SetRecoveryCleanupRecorder(func(config.ServiceConfig) error { return errors.New("journal unavailable") })
			if result, err := m.ForgetRecoveryPending(f.Service); err == nil || result.Outcome != RecoveryOutcomeIncomplete {
				t.Fatalf("forget succeeded without a durable cleanup intent: %+v %v", result, err)
			}
			if _, err := os.Stat(f.VMDir); err != nil {
				t.Fatalf("the VM state was removed before its cleanup was recorded: %v", err)
			}
			if inst := m.List()[f.Service]; inst == nil || inst.State != StateRecoveryPending {
				t.Fatalf("a failed journal write lost quarantine: %+v", inst)
			}
			var devices []string
			m.SetRecoveryCleanupRecorder(func(service config.ServiceConfig) error {
				if _, err := os.Stat(f.VMDir); err != nil {
					return err
				}
				if service.Network != nil {
					devices = append(devices, service.Network.Interface)
				}
				return nil
			})
			if result, err := m.ForgetRecoveryPending(f.Service); err != nil || result.Outcome != RecoveryOutcomeCleared {
				t.Fatalf("forget did not retry: %+v %v", result, err)
			}
			if len(devices) == 0 || slices.Contains(devices, "tap-forged") || slices.ContainsFunc(devices, func(d string) bool { return d != "tap-old" }) {
				t.Fatalf("the journal did not receive only the trusted devices: %v", devices)
			}
			if _, err := os.Stat(f.VMDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("forgotten state remains: %v", err)
			}
		})
	}
}

// C1/C3: the trusted teardown after an uncertain write holds for every kind of
// remaining state. Both in-memory snapshots are recorded when their devices
// differ; a configuration already unknown contributes none; a live original
// owner refuses before anything is recorded.
func TestForgetAfterUncertainWriteCoversEveryStateKind(t *testing.T) {
	for _, tc := range []struct {
		name         string
		state        string // "missing", "symlink" or "corrupt"
		candidateTap string
		unknown      bool
		live         bool
		want         []string
	}{
		{name: "missing directory", state: "missing", want: []string{"tap-old"}},
		{name: "symlink", state: "symlink", want: []string{"tap-old"}},
		{name: "differing snapshots", state: "corrupt", candidateTap: "tap-new", want: []string{"tap-old", "tap-new"}},
		{name: "configuration already unknown", state: "corrupt", unknown: true, want: []string{""}},
		{name: "live original owner", state: "corrupt", live: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f, inspector := recoveryFixture(t)
			m.inspector = inspector
			f.Config.Network = &config.NetworkConfig{Interface: "tap-old", HostDevName: "eth0", GuestIP: "172.16.0.2"}
			hash, err := serviceConfigHash(f.Config)
			if err != nil {
				t.Fatal(err)
			}
			f.ConfigHash = hash
			m.addRecoveryPending(f.Service, f, "uncertain write")
			candidate := cloneInstanceManifest(f)
			candidate.Lifecycle = lifecycleStopped
			if tc.candidateTap != "" {
				network := *candidate.Config.Network
				network.Interface = tc.candidateTap
				candidate.Config.Network = &network
				if candidate.ConfigHash, err = serviceConfigHash(candidate.Config); err != nil {
					t.Fatal(err)
				}
			}
			m.mu.Lock()
			m.instances[f.Service].recoveryWriteCandidate = candidate
			m.instances[f.Service].RecoveryConfigUnknown = tc.unknown
			m.mu.Unlock()
			if !tc.live {
				delete(inspector.identities, f.PID)
			}
			switch tc.state {
			case "missing":
				err = os.RemoveAll(f.VMDir)
			case "symlink":
				if err = os.RemoveAll(f.VMDir); err == nil {
					err = os.Symlink(t.TempDir(), f.VMDir)
				}
			case "corrupt":
				err = os.WriteFile(manifestPath(f.VMDir), []byte("corrupt"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			var devices []string
			m.SetRecoveryCleanupRecorder(func(service config.ServiceConfig) error {
				if _, err := os.Lstat(f.VMDir); tc.state != "missing" && err != nil {
					return err // Recorded only while the runtime state remains.
				}
				device := ""
				if service.Network != nil {
					device = service.Network.Interface
				}
				devices = append(devices, device)
				return nil
			})
			result, err := m.ForgetRecoveryPending(f.Service)
			if tc.live {
				if err == nil || result.Outcome != RecoveryOutcomeAmbiguous || len(devices) != 0 {
					t.Fatalf("forget released a live owner: %+v %v devices=%v", result, err, devices)
				}
				return
			}
			if err != nil || result.Outcome != RecoveryOutcomeCleared || !slices.Equal(devices, tc.want) {
				t.Fatalf("forget: %+v %v devices=%v, want %v", result, err, devices, tc.want)
			}
			if _, err := os.Lstat(f.VMDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("forgotten state remains: %v", err)
			}
		})
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
	recorded := 0
	m.SetRecoveryCleanupRecorder(func(config.ServiceConfig) error { recorded++; return nil })
	if result, err := m.ForgetRecoveryPending(f.Service); err == nil || result.Outcome != RecoveryOutcomeAmbiguous || recorded != 0 {
		t.Fatalf("forget ignored its possible post-write owner: %+v %v, cleanup recorded %d times", result, err, recorded)
	}
	if _, err := os.Stat(f.VMDir); err != nil {
		t.Fatalf("live candidate's state was removed: %v", err)
	}
}
