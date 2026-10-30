package vm

import (
	"context"
	"os"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

func TestUntrustedRecoveryReleasesClaimsAndRequiresForgetToDelete(t *testing.T) {
	for _, tc := range []struct {
		name    string
		corrupt func(*testing.T, *instanceManifest)
	}{
		{"unreadable manifest", func(t *testing.T, f *instanceManifest) {
			if err := os.WriteFile(manifestPath(f.VMDir), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"config hash mismatch", func(t *testing.T, f *instanceManifest) {
			changed := cloneInstanceManifest(f)
			changed.Config.MemoryMB++
			if err := writeManifest(manifestPath(f.VMDir), changed); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		for _, live := range []bool{false, true} {
			name := tc.name + "/absent owner"
			if live {
				name = tc.name + "/live owner"
			}
			t.Run(name, func(t *testing.T) {
				m, f, inspector := recoveryFixture(t)
				m.inspector = inspector
				if !live {
					delete(inspector.identities, f.PID)
				}
				tc.corrupt(t, f)
				if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
					t.Fatal(err)
				}
				if got := m.List()[f.Service]; got == nil || !got.RecoveryConfigUnknown {
					t.Fatalf("untrusted state was not fenced: %#v", got)
				}

				if result, err := m.ClearRecoveryPending(f.Service); err == nil || result.Outcome != RecoveryOutcomeAmbiguous {
					t.Fatalf("untrusted metadata was resolved without --forget: result=%#v err=%v", result, err)
				}
				got := m.List()[f.Service]
				if got == nil || got.State != StateRecoveryPending || got.RecoveryOwnerAbsent == live {
					t.Fatalf("owner absence was not recorded correctly: %#v", got)
				}
				if _, err := os.Stat(manifestPath(f.VMDir)); err != nil {
					t.Fatalf("untrusted metadata was not preserved for diagnosis: %v", err)
				}

				result, err := m.ForgetRecoveryPending(f.Service)
				if live {
					if err == nil || result.Outcome != RecoveryOutcomeAmbiguous {
						t.Fatalf("live untrusted state was forgotten: result=%#v err=%v", result, err)
					}
					if _, err := os.Stat(f.VMDir); err != nil {
						t.Fatalf("live untrusted state was removed: %v", err)
					}
					return
				}
				if err != nil || result.Outcome != RecoveryOutcomeCleared {
					t.Fatalf("absent untrusted state was not forgotten: result=%#v err=%v", result, err)
				}
				if got := m.List()[f.Service]; got != nil {
					t.Fatalf("forgotten state is still tracked: %#v", got)
				}
				if _, err := os.Stat(f.VMDir); !os.IsNotExist(err) {
					t.Fatalf("forgotten state remains: %v", err)
				}
			})
		}
	}
}

func TestUntrustedRecoveryUsesRestoredTrustedRecord(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	if err := os.WriteFile(manifestPath(f.VMDir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	// The documented remedy: an operator restores the trusted record.
	if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
		t.Fatal(err)
	}
	result, err := m.ClearRecoveryPending(f.Service)
	if err != nil || result.Outcome != RecoveryOutcomeReadopted {
		t.Fatalf("restored record was not used without an agent restart: result=%#v err=%v", result, err)
	}
	if got := m.List()[f.Service]; got == nil || got.State != StateRunning || got.RecoveryConfigUnknown {
		t.Fatalf("restored owner was not adopted: %#v", got)
	}
}

func TestRecoveryClearsRemovedStateDirectoryAfterAbsenceProof(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	inspector.socketErr = os.ErrNotExist
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if got := m.List()[f.Service]; got == nil || got.State != StateRecoveryPending {
		t.Fatalf("fixture was not quarantined: %#v", got)
	}
	var recorded []config.ServiceConfig
	m.SetRecoveryCleanupRecorder(func(service config.ServiceConfig) error {
		recorded = append(recorded, service)
		return nil
	})
	if err := os.RemoveAll(f.VMDir); err != nil {
		t.Fatal(err)
	}

	// The owner is still live: nothing may be forgotten.
	if result, err := m.ClearRecoveryPending(f.Service); err == nil || result.Outcome != RecoveryOutcomeAmbiguous {
		t.Fatalf("missing state of a live owner was cleared: result=%#v err=%v", result, err)
	}
	delete(inspector.identities, f.PID)
	result, err := m.ClearRecoveryPending(f.Service)
	if err != nil || result.Outcome != RecoveryOutcomeCleared {
		t.Fatalf("removed state of an absent owner was not cleared: result=%#v err=%v", result, err)
	}
	if got := m.List()[f.Service]; got != nil {
		t.Fatalf("cleared state is still tracked: %#v", got)
	}
	if len(recorded) != 1 || recorded[0].Name != f.Service || recorded[0].MemoryMB != f.Config.MemoryMB {
		t.Fatalf("trusted host cleanup was not journaled: %#v", recorded)
	}
}

func TestUntrustedRecoveryWaitsForQueuedSystemdLaunch(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	delete(inspector.identities, f.PID)
	m.launcher = &systemdLauncher{systemctl: "systemctl"}
	if err := os.WriteFile(manifestPath(f.VMDir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	queued := "Id=firework-vm-lost.service\nActiveState=inactive\nMainPID=0\nJob=145\nExecStart={ path=/fc ; argv[]=/fc --id lost --api-sock " + f.SocketPath + " ; ignore_errors=no }\n"
	units := queued
	m.recoveryCommandOutput = func(context.Context, string, ...string) ([]byte, error) { return []byte(units), nil }
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if result, err := m.ForgetRecoveryPending(f.Service); err == nil || result.Outcome != RecoveryOutcomeAmbiguous {
		t.Fatalf("queued launch was forgotten: result=%#v err=%v", result, err)
	}
	if got := m.List()[f.Service]; got == nil || got.RecoveryOwnerAbsent {
		t.Fatalf("queued launch released its unknown claims: %#v", got)
	}
	units = ""
	if result, err := m.ForgetRecoveryPending(f.Service); err != nil || result.Outcome != RecoveryOutcomeCleared {
		t.Fatalf("absent launch was not forgotten: result=%#v err=%v", result, err)
	}
}

func TestAbsentUntrustedStateJournalsReadableClaimsBeforeReleasing(t *testing.T) {
	for _, journalFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "journaled", true: "journal failure keeps claims"}[journalFails], func(t *testing.T) {
			m, f, inspector := recoveryFixture(t)
			m.inspector = inspector
			delete(inspector.identities, f.PID)
			changed := cloneInstanceManifest(f)
			changed.Config.Network = &config.NetworkConfig{GuestIP: "172.16.0.9", Interface: "tap-old"}
			changed.Config.PortForwards = []config.PortForward{{HostPort: 8080, VMPort: 80}}
			if err := writeManifest(manifestPath(f.VMDir), changed); err != nil { // hash no longer matches
				t.Fatal(err)
			}
			var recorded []config.ServiceConfig
			m.SetRecoveryCleanupRecorder(func(service config.ServiceConfig) error {
				recorded = append(recorded, service)
				if journalFails {
					return os.ErrPermission
				}
				return nil
			})
			if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if result, err := m.ClearRecoveryPending(f.Service); err == nil || result.Outcome != RecoveryOutcomeAmbiguous {
					t.Fatalf("untrusted metadata was resolved without --forget: result=%#v err=%v", result, err)
				}
			}
			got := m.List()[f.Service]
			if got == nil || got.ReleasedRecoveryClaims() == journalFails {
				t.Fatalf("claims released=%v despite journal failure=%v: %#v", got != nil && got.ReleasedRecoveryClaims(), journalFails, got)
			}
			if _, err := os.Stat(manifestPath(f.VMDir)); err != nil {
				t.Fatalf("diagnostic record was removed: %v", err)
			}
			want := 1
			if journalFails {
				want = 2 // retried until the cleanup intent is durable
			}
			if len(recorded) != want || recorded[0].PortForwards[0].HostPort != 8080 || recorded[0].Network.Interface != "tap-old" {
				t.Fatalf("readable claims were not journaled once before release: %#v", recorded)
			}
		})
	}
}
