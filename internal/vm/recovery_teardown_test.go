package vm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

func TestRecoveryTeardownRetriesPartialRemovalAfterRestartWithFreshAbsenceProof(t *testing.T) {
	m, manifest := teardownFixture(t)
	manifest.PID = 0
	manifest.Lifecycle = lifecycleFailed
	if err := m.persistRecoveryTeardown(manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manifestPath(manifest.VMDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest.ConfigPath, []byte("partial cleanup"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted := NewManager("/firecracker", m.stateDir, m.logger)
	inspector := &fakeProcessInspector{identities: map[int]processIdentity{
		99: {PID: 99, CommandLine: []string{"other", "--config-file", manifest.ConfigPath}},
	}}
	restarted.inspector = inspector
	if _, err := restarted.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if got := restarted.List()[manifest.Service]; got == nil || got.State != StateRecoveryPending {
		t.Fatalf("saved deletion intent bypassed a new claimant: %#v", got)
	}
	if _, err := os.Stat(manifest.ConfigPath); err != nil {
		t.Fatal("new claimant's files were removed", err)
	}
	delete(inspector.identities, 99)
	if _, err := restarted.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if got := restarted.List()[manifest.Service]; got != nil {
		t.Fatalf("partial removal did not converge after claimant exited: %#v", got)
	}
	if _, err := os.Stat(manifest.VMDir); !os.IsNotExist(err) {
		t.Fatalf("partial directory remains: %v", err)
	}
	if pending, err := restarted.PendingRecoveryTeardowns(); err != nil || len(pending) != 1 {
		t.Fatalf("VM clearance discarded host cleanup: %#v %v", pending, err)
	}
}

func TestRecoveryTeardownPersistenceFailurePreventsRuntimeDeletion(t *testing.T) {
	m, manifest := teardownFixture(t)
	m.inspector = &fakeProcessInspector{}
	m.addRecoveryPending(manifest.Service, manifest, "dead process")
	if err := os.WriteFile(filepath.Join(m.stateDir, recoveryTeardownDir), []byte("blocks outbox creation"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ClearRecoveryPending(manifest.Service); err == nil {
		t.Fatal("runtime deletion succeeded without a durable host cleanup intent")
	}
	if _, err := os.Stat(manifestPath(manifest.VMDir)); err != nil {
		t.Fatal("failed outbox write removed ownership manifest", err)
	}
	if got := m.List()[manifest.Service]; got == nil || got.State != StateRecoveryPending {
		t.Fatalf("failed outbox write lost quarantine: %#v", got)
	}
	if err := os.Remove(filepath.Join(m.stateDir, recoveryTeardownDir)); err != nil {
		t.Fatal(err)
	}
	if result, err := m.ClearRecoveryPending(manifest.Service); err != nil || result.Outcome != RecoveryOutcomeCleared {
		t.Fatalf("outbox recovery did not retry clearance: %#v %v", result, err)
	}
}

func TestRecoveryTeardownSupportsConfiguredStateRootSymlink(t *testing.T) {
	m, manifest := teardownFixture(t)
	physicalRoot := m.stateDir
	configuredRoot := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(physicalRoot, configuredRoot); err != nil {
		t.Fatal(err)
	}
	m.stateDir = configuredRoot
	manifest.VMDir = strings.Replace(manifest.VMDir, physicalRoot, configuredRoot, 1)
	manifest.SocketPath = filepath.Join(manifest.VMDir, "firecracker.sock")
	manifest.ConfigPath = filepath.Join(manifest.VMDir, "vm-config.json")
	m.inspector = &fakeProcessInspector{}
	if err := writeManifest(manifestPath(manifest.VMDir), manifest); err != nil {
		t.Fatal(err)
	}
	if err := m.persistRecoveryTeardown(manifest); err != nil {
		t.Fatal(err)
	}
	if err := m.removeRecoveryState(manifest); err != nil {
		t.Fatalf("configured root symlink blocked safe cleanup: %v", err)
	}
	if pending, err := m.PendingRecoveryTeardowns(); err != nil || len(pending) != 1 {
		t.Fatalf("configured root lost cleanup outbox: %#v %v", pending, err)
	}
}

func teardownFixture(t *testing.T) (*Manager, *instanceManifest) {
	t.Helper()
	m, manifest, _ := recoveryFixture(t)
	manifest.Config.Network = &config.NetworkConfig{Interface: "tap-old", HostDevName: "eth0", GuestIP: "172.16.0.2"}
	manifest.Config.PortForwards = []config.PortForward{{HostPort: 8080, VMPort: 80}}
	hash, err := serviceConfigHash(manifest.Config)
	if err != nil {
		t.Fatal(err)
	}
	manifest.ConfigHash = hash
	if err := writeManifest(manifestPath(manifest.VMDir), manifest); err != nil {
		t.Fatal(err)
	}
	return m, manifest
}

func TestRecoveryTeardownSurvivesPartialRemovalAndRestart(t *testing.T) {
	m, manifest := teardownFixture(t)
	for attempt := 0; attempt < 2; attempt++ {
		if err := m.persistRecoveryTeardown(manifest); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(m.stateDir, recoveryTeardownDir))
	if err != nil || len(entries) != 1 {
		t.Fatalf("stable deletion intent was not persisted exactly once: %v %v", entries, err)
	}
	for path, mode := range map[string]os.FileMode{
		filepath.Join(m.stateDir, recoveryTeardownDir):                    0700,
		filepath.Join(m.stateDir, recoveryTeardownDir, entries[0].Name()): 0600,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("unsafe outbox permissions for %s: %v %v", path, info, err)
		}
	}
	// Model a partial RemoveAll: instance.json is gone, while VM files remain.
	if err := os.Remove(manifestPath(manifest.VMDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest.ConfigPath, []byte("old config"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted := NewManager("/firecracker", m.stateDir, m.logger)
	restored, err := restarted.recoveryTeardownManifest(manifest.Service)
	if err != nil || !sameRecoveryIdentity(restored, manifest) {
		t.Fatalf("partial removal lost ownership snapshot: %#v %v", restored, err)
	}
	if matches, err := restarted.hasRecoveryTeardown(manifest); err != nil || !matches {
		t.Fatalf("exact deletion intent was lost: %v %v", matches, err)
	}
	different := *manifest
	different.PID++
	if matches, err := restarted.hasRecoveryTeardown(&different); err != nil || matches {
		t.Fatalf("different ownership identity accepted: %v %v", matches, err)
	}
	pending, err := restarted.PendingRecoveryTeardowns()
	if err != nil || len(pending) != 1 {
		t.Fatalf("restart lost cleanup event: %#v %v", pending, err)
	}
	for id, service := range pending {
		if service.Name != manifest.Service || service.Network.Interface != "tap-old" || service.PortForwards[0].HostPort != 8080 {
			t.Fatalf("old host configuration changed: %#v", service)
		}
		if err := restarted.AcknowledgeRecoveryTeardown(id); err != nil {
			t.Fatal(err)
		}
		if err := restarted.AcknowledgeRecoveryTeardown(id); err != nil {
			t.Fatal(err)
		}
	}
	if manifest, err := restarted.recoveryTeardownManifest(manifest.Service); err != nil || manifest != nil {
		t.Fatalf("acknowledged event survived: %#v %v", manifest, err)
	}
}

func TestRecoveryTeardownRejectsAmbiguousSnapshots(t *testing.T) {
	m, manifest := teardownFixture(t)
	if err := m.persistRecoveryTeardown(manifest); err != nil {
		t.Fatal(err)
	}
	other := *manifest
	other.InstanceID += "-other"
	if err := m.persistRecoveryTeardown(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := m.recoveryTeardownManifest(manifest.Service); err == nil {
		t.Fatal("incompatible deletion intents selected an arbitrary incarnation")
	}
	if pending, err := m.PendingRecoveryTeardowns(); err != nil || len(pending) != 2 {
		t.Fatalf("independent cleanup snapshots were lost: %#v %v", pending, err)
	}
}

func TestRecoveryTeardownRejectsSymlinksAndCorruption(t *testing.T) {
	for _, kind := range []string{"directory symlink", "file symlink", "hash mismatch", "other service paths"} {
		t.Run(kind, func(t *testing.T) {
			m, manifest := teardownFixture(t)
			external := t.TempDir()
			if kind == "directory symlink" {
				if err := os.Symlink(external, filepath.Join(m.stateDir, recoveryTeardownDir)); err != nil {
					t.Fatal(err)
				}
				if err := m.persistRecoveryTeardown(manifest); err == nil {
					t.Fatal("wrote through an outbox directory symlink")
				}
				if files, _ := os.ReadDir(external); len(files) != 0 {
					t.Fatal("external directory was changed")
				}
				return
			}
			if kind == "other service paths" {
				manifest.Service = "other"
				if err := m.persistRecoveryTeardown(manifest); err == nil {
					t.Fatal("manifest redirected cleanup to another service")
				}
				return
			}
			if err := m.persistRecoveryTeardown(manifest); err != nil {
				t.Fatal(err)
			}
			id := (recoveryTeardown{Manifest: *manifest}).id()
			path := filepath.Join(m.stateDir, recoveryTeardownDir, id+".json")
			if kind == "file symlink" {
				target := filepath.Join(external, "target")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			} else {
				manifest.Config.Network.Interface = "tampered"
				event := recoveryTeardown{Version: 1, Manifest: *manifest}
				data, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.PendingRecoveryTeardowns(); err == nil {
				t.Fatal("invalid event accepted")
			}
			if err := m.AcknowledgeRecoveryTeardown(id); err == nil {
				t.Fatal("invalid event acknowledged")
			}
			if err := m.AcknowledgeRecoveryTeardown("../../target"); err == nil {
				t.Fatal("invalid acknowledgement path accepted")
			}
		})
	}
}

func TestRecoveryTeardownAckDoesNotOvertakeDiscovery(t *testing.T) {
	m, f := teardownFixture(t)
	if err := m.persistRecoveryTeardown(f); err != nil {
		t.Fatal(err)
	}
	pending, err := m.PendingRecoveryTeardowns()
	if err != nil {
		t.Fatal(err)
	}
	m.recoveryReservations = map[string]bool{f.Service: true}
	for id := range pending {
		if err := m.AcknowledgeRecoveryTeardown(id); err == nil {
			t.Fatal("ack overtook discovery")
		}
	}
	if pending, err := m.PendingRecoveryTeardowns(); err != nil || len(pending) != 1 {
		t.Fatalf("intent lost: %v %v", pending, err)
	}
}
