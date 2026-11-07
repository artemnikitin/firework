package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

// C3/C5: marker names must not occupy another valid service's VM directory.
func TestIntentMarkerCannotCollideWithVMDirectory(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	delete(inspector.identities, f.PID)
	other := cloneInstanceManifest(f)
	other.Service, other.Config.Name = ".firework-intent-app", ".firework-intent-app"
	if err := ValidateRecoveryServiceName(other.Service); err != nil {
		t.Fatal(err)
	}
	other.InstanceID, other.PID, other.ProcessStart = "other-owner", 43, 101
	other.VMDir = filepath.Join(m.stateDir, "vms", other.Service)
	other.SocketPath, other.ConfigPath = filepath.Join(other.VMDir, firecrackerSocketName), filepath.Join(other.VMDir, vmConfigName)
	var err error
	other.ConfigHash, err = serviceConfigHash(other.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(manifestPath(other.VMDir), other); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(manifestPath(other.VMDir))
	if err != nil {
		t.Fatal(err)
	}
	inspector.identities[43] = processIdentity{PID: 43, HostBootID: other.HostBootID, StartTicks: 101,
		Executable: other.Executable, ExecutableDev: other.ExecutableDev, ExecutableIno: other.ExecutableIno,
		CommandLine: []string{other.Executable, "--id", other.InstanceID, "--api-sock", other.SocketPath, "--config-file", other.ConfigPath}}
	stopAdoptedMonitors(t, m)
	for pass := 0; pass < 3; pass++ {
		if _, err := m.Recover(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{f.Config, other.Config}}); err != nil {
			t.Fatal(err)
		}
	}
	if inst := m.List()[f.Service]; inst != nil {
		t.Fatalf("another valid service blocks app's cleanup: %+v", inst)
	}
	if inst := m.List()[other.Service]; inst == nil || inst.State != StateRunning {
		t.Fatalf("the colliding service was disturbed: %+v", inst)
	}
	after, err := os.ReadFile(manifestPath(other.VMDir))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("another service's ownership record was changed: %v", err)
	}
	if m.hasIntent(f.Service) {
		t.Fatal("a VM directory was treated as app's intent marker")
	}
}

func orphanedIntentFixture(t *testing.T, legacy bool) (*Manager, *instanceManifest, *fakeProcessInspector, string) {
	t.Helper()
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	delete(inspector.identities, f.PID)
	if err := os.RemoveAll(f.VMDir); err != nil {
		t.Fatal(err)
	}
	root := m.stateDir
	if legacy {
		root = filepath.Join(root, "vms")
	}
	path := filepath.Join(root, intentPrefix+f.Service)
	if err := os.WriteFile(path, []byte(f.Service+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return m, f, inspector, path
}

// C2/C3: an intent can outlive its VM directory. Old markers remain usable
// after an upgrade, and neither old nor new orphaned markers reserve the VM
// namespace forever once absence is proven.
func TestOrphanedIntentsRecoverAtEveryEntryPoint(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, entry := range []string{"startup", "retry", "command"} {
			t.Run(fmt.Sprintf("legacy=%v/%s", legacy, entry), func(t *testing.T) {
				m, f, _, path := orphanedIntentFixture(t, legacy)
				if entry == "command" {
					result, err := m.ClearRecoveryPending(f.Service)
					if err != nil || result.Outcome != RecoveryOutcomeCleared {
						t.Fatalf("orphaned intent did not clear: %+v %v", result, err)
					}
				} else {
					if entry == "retry" {
						if err := m.DiscoverRecoveryState(); err != nil {
							t.Fatal(err)
						}
					}
					if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) || m.List()[f.Service] != nil {
					t.Fatalf("orphaned operation remains: marker=%v VM=%+v", err, m.List()[f.Service])
				}
				prefixService := config.ServiceConfig{Name: intentPrefix + f.Service}
				id, _, _, err := m.beginStartContext(context.Background(), prefixService)
				if err != nil {
					t.Fatalf("a valid service is still blocked by the old marker path: %v", err)
				}
				m.discardStart(prefixService.Name, id)
			})
		}
	}
}

// C1: an orphaned intent still requires the complete absence proof. A
// conflicting service name waits without becoming a new untrusted quarantine.
func TestOrphanedIntentKeepsLiveClaimantsFenced(t *testing.T) {
	m, f, inspector, path := orphanedIntentFixture(t, true)
	inspector.find = []processIdentity{{PID: 77}}
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if inst := m.List()[f.Service]; inst == nil || inst.State != StateRecoveryPending {
		t.Fatalf("the live claimant was not fenced: %+v", inst)
	}
	prefixService := config.ServiceConfig{Name: intentPrefix + f.Service}
	if _, _, _, err := m.beginStartContext(context.Background(), prefixService); !errors.Is(err, ErrStartInProgress) || m.List()[prefixService.Name] != nil {
		t.Fatalf("the legacy-marker wait became a permanent quarantine: %v %+v", err, m.List()[prefixService.Name])
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the claimant's intent was removed: %v", err)
	}
	inspector.find = nil
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	id, _, _, err := m.beginStartContext(context.Background(), prefixService)
	if err != nil {
		t.Fatalf("the wait did not end after the claimant left: %v", err)
	}
	m.discardStart(prefixService.Name, id)
}

// C1/L4: a file with the old marker's name is not evidence of our operation
// unless its contents name that service. Untrusted state keeps its fence.
func TestMalformedLegacyIntentDoesNotAuthorizeCleanup(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	delete(inspector.identities, f.PID)
	if err := os.Remove(manifestPath(f.VMDir)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.stateDir, "vms", intentPrefix+f.Service)
	if err := os.WriteFile(path, []byte("not an intent for app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if inst := m.List()[f.Service]; inst == nil || inst.State != StateRecoveryPending {
		t.Fatalf("malformed metadata authorized cleanup: %+v", inst)
	}
	if _, err := os.Stat(f.VMDir); err != nil {
		t.Fatalf("untrusted state was removed: %v", err)
	}
}

func TestUntrustedOrphanedIntentNeedsForget(t *testing.T) {
	for _, kind := range []string{"damaged file", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			m, f, _, path := orphanedIntentFixture(t, false)
			var target string
			if kind == "directory" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				target = filepath.Join(t.TempDir(), "preserved")
				if err := os.WriteFile(target, []byte("not VM metadata"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(path, "preserved-link")); err != nil {
					t.Fatal(err)
				}
			} else if kind == "symlink" {
				target = filepath.Join(t.TempDir(), "preserved")
				if err := os.WriteFile(target, []byte(f.Service+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte("damaged"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
				t.Fatal(err)
			}
			if inst := m.List()[f.Service]; inst == nil || inst.State != StateRecoveryPending {
				t.Fatalf("untrusted intent did not keep its fence: %+v", inst)
			}
			if result, err := m.ClearRecoveryPending(f.Service); err == nil || result.Outcome != RecoveryOutcomeAmbiguous {
				t.Fatalf("plain clear trusted damaged evidence: %+v %v", result, err)
			}
			if result, err := m.ForgetRecoveryPending(f.Service); err != nil || result.Outcome != RecoveryOutcomeCleared {
				t.Fatalf("absence-proven forget did not clear the intent: %+v %v", result, err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("untrusted intent remains after forget: %v", err)
			}
			if target != "" {
				if _, err := os.Stat(target); err != nil {
					t.Fatalf("forget followed the symlink: %v", err)
				}
			}
		})
	}
}

func TestClearedIntentIsNotRepublishedFromAStaleListing(t *testing.T) {
	m, f, _, _ := orphanedIntentFixture(t, false)
	listed, err := os.ReadDir(m.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	// Another resolver completes after the inventory's ReadDir, before it
	// acquires the manager lock. The listing must not recreate its quarantine.
	if result, err := m.ClearRecoveryPending(f.Service); err != nil || result.Outcome != RecoveryOutcomeCleared {
		t.Fatalf("setup: %+v %v", result, err)
	}
	m.inventoryIntentEntries(listed, nil)
	if inst := m.List()[f.Service]; inst != nil {
		t.Fatalf("a stale listing recreated completed ownership: %+v", inst)
	}
}

func TestUnreadableLegacyIntentDoesNotInventAnotherVM(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("needs non-root file permissions")
	}
	m, f, _, legacy := orphanedIntentFixture(t, true)
	if err := os.Chmod(legacy, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(legacy, 0o600) })
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	prefixed := f.Config
	prefixed.Name = intentPrefix + f.Service
	if _, _, _, err := m.beginStartContext(context.Background(), prefixed); !errors.Is(err, ErrStartInProgress) || m.List()[prefixed.Name] != nil {
		t.Fatalf("the unreadable marker became another VM's state: %v %+v", err, m.List()[prefixed.Name])
	}
	if err := os.Chmod(legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 4; pass++ {
		if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(legacy); !errors.Is(err, os.ErrNotExist) || m.List()[f.Service] != nil {
		t.Fatalf("the restored legacy intent did not recover: marker=%v owner=%+v", err, m.List()[f.Service])
	}
	id, _, _, err := m.beginStartContext(context.Background(), prefixed)
	if err != nil {
		t.Fatalf("the transient read error still blocks a VM that never existed: %v", err)
	}
	m.discardStart(prefixed.Name, id)
}
