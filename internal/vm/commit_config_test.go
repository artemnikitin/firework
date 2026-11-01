package vm

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

// An in-place change committed to a running VM is written to its ownership
// manifest with a matching hash, so an agent restart adopts the VM with it
// instead of reverting to the launch-time configuration.
func TestCommitConfigPersistsAcrossAdoption(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	desired := config.NodeConfig{Services: []config.ServiceConfig{f.Config}}
	if _, err := m.Recover(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	if got := m.List()[f.Service]; got == nil || got.State != StateRunning {
		t.Fatalf("setup: not adopted: %#v", got)
	}
	committed := f.Config
	committed.Metadata = map[string]string{"host": "new.example.com"}
	committed.HealthCheck = &config.HealthCheckConfig{Type: "tcp", Port: 90}
	if err := m.CommitConfig(f.Service, committed); err != nil {
		t.Fatal(err)
	}
	if got := m.List()[f.Service].Config.Metadata["host"]; got != "new.example.com" {
		t.Fatalf("in-memory config not committed: %q", got)
	}
	durable, err := readManifest(manifestPath(f.VMDir))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyRecoveryManifestIntegrity(durable); err != nil || durable.Config.Metadata["host"] != "new.example.com" || durable.InstanceID != f.InstanceID {
		t.Fatalf("manifest not committed: %v %#v", err, durable.Config)
	}

	restarted := NewManager("/firecracker", m.stateDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	restarted.inspector = inspector
	if _, err := restarted.Recover(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	got := restarted.List()[f.Service]
	if got == nil || got.State != StateRunning || got.Config.Metadata["host"] != "new.example.com" || got.Config.HealthCheck.Port != 90 {
		t.Fatalf("restart did not adopt the committed config: %#v", got)
	}
}

// Only a running VM takes in-place changes; anything else is decided again
// by the next reconciliation.
func TestCommitConfigIgnoresNonRunningInstances(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	inspector.errors[f.PID] = errProcessNotFound
	committed := f.Config
	committed.Metadata = map[string]string{"host": "new.example.com"}
	if err := m.CommitConfig(f.Service, committed); err != nil {
		t.Fatal(err)
	}
	durable, err := readManifest(manifestPath(f.VMDir))
	if err != nil || durable.Config.Metadata != nil {
		t.Fatalf("untracked VM manifest changed: %v %#v", err, durable.Config)
	}
}
