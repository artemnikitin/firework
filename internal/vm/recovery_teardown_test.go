package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

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

func TestRecoveryCleanupJournalFailurePreventsRuntimeDeletion(t *testing.T) {
	m, manifest := teardownFixture(t)
	m.inspector = &fakeProcessInspector{}
	m.addRecoveryPending(manifest.Service, manifest, "dead process")
	m.SetRecoveryCleanupRecorder(func(config.ServiceConfig) error { return errors.New("journal unavailable") })
	if _, err := m.ClearRecoveryPending(manifest.Service); err == nil {
		t.Fatal("runtime deletion succeeded without a durable host cleanup intent")
	}
	if _, err := os.Stat(manifestPath(manifest.VMDir)); err != nil {
		t.Fatal("failed journal write removed ownership manifest", err)
	}
	if got := m.List()[manifest.Service]; got == nil || got.State != StateRecoveryPending {
		t.Fatalf("failed journal write lost quarantine: %#v", got)
	}
	var recorded config.ServiceConfig
	m.SetRecoveryCleanupRecorder(func(service config.ServiceConfig) error {
		if _, err := os.Stat(manifestPath(manifest.VMDir)); err != nil {
			return err
		}
		// The callback may consult List without taking the manager lock twice.
		m.List()
		recorded = service
		return nil
	})
	if result, err := m.ClearRecoveryPending(manifest.Service); err != nil || result.Outcome != RecoveryOutcomeCleared {
		t.Fatalf("journal recovery did not retry clearance: %#v %v", result, err)
	}
	if !reflect.DeepEqual(recorded, manifest.Config) {
		t.Fatalf("journal did not receive the complete old configuration: %#v", recorded)
	}
	if _, err := os.Stat(manifest.VMDir); !os.IsNotExist(err) {
		t.Fatalf("runtime files remain: %v", err)
	}
}

func TestRecoveryCleanupSupportsConfiguredStateRootSymlink(t *testing.T) {
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
	var called bool
	m.SetRecoveryCleanupRecorder(func(config.ServiceConfig) error { called = true; return nil })
	if err := m.removeRecoveryStateContext(context.Background(), manifest); err != nil {
		t.Fatalf("configured root symlink blocked safe cleanup: %v", err)
	}
	if !called {
		t.Fatal("cleanup bypassed the shared journal")
	}
}

func TestRecoveryCleanupRetainsRuntimeWhileNewClaimantExists(t *testing.T) {
	m, manifest := teardownFixture(t)
	manifest.PID = 0
	manifest.Lifecycle = lifecycleFailed
	if err := writeManifest(manifestPath(manifest.VMDir), manifest); err != nil {
		t.Fatal(err)
	}
	inspector := &fakeProcessInspector{identities: map[int]processIdentity{
		99: {PID: 99, CommandLine: []string{"other", "--config-file", manifest.ConfigPath}},
	}}
	m.inspector = inspector
	m.addRecoveryPending(manifest.Service, manifest, "old process exited")
	var calls int
	m.SetRecoveryCleanupRecorder(func(config.ServiceConfig) error { calls++; return nil })
	if _, err := m.ClearRecoveryPending(manifest.Service); err == nil {
		t.Fatal("cleared files used by a new claimant")
	}
	if calls != 0 {
		t.Fatal("recorded cleanup before complete absence proof")
	}
	delete(inspector.identities, 99)
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if m.List()[manifest.Service] != nil || calls != 1 {
		t.Fatal("absence proof did not converge through shared cleanup journal")
	}
}

func TestRecoveryCleanupIgnoresUnrelatedObsoleteOutbox(t *testing.T) {
	m, manifest := teardownFixture(t)
	m.inspector = &fakeProcessInspector{}
	m.addRecoveryPending(manifest.Service, manifest, "dead process")
	// Recovery relies on the reconciler journal. A leftover directory or
	// corrupt file from an earlier implementation is not another queue.
	if err := os.WriteFile(filepath.Join(m.stateDir, "recovery-teardowns"), []byte("stray invalid data"), 0600); err != nil {
		t.Fatal(err)
	}
	var called bool
	m.SetRecoveryCleanupRecorder(func(config.ServiceConfig) error { called = true; return nil })
	if result, err := m.ClearRecoveryPending(manifest.Service); err != nil || result.Outcome != RecoveryOutcomeCleared {
		t.Fatalf("obsolete outbox blocked clearance: %#v %v", result, err)
	}
	if !called {
		t.Fatal("shared journal was bypassed")
	}
}
