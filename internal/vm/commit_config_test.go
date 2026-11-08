package vm

import (
	"context"
	"errors"
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

// Committing the same configuration again, including one that went through
// the manifest's JSON round trip, does not rewrite the manifest.
func TestCommitConfigWritesOnlyOnChange(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	if _, err := m.Recover(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{f.Config}}); err != nil {
		t.Fatal(err)
	}
	writes := 0
	m.recoveryManifestWriter = func(path string, manifest *instanceManifest) error {
		writes++
		return writeManifest(path, manifest)
	}
	committed := f.Config
	committed.Metadata = map[string]string{"host": "new.example.com"}
	for i := 0; i < 3; i++ {
		if err := m.CommitConfig(f.Service, committed); err != nil {
			t.Fatal(err)
		}
	}
	durable, err := readManifest(manifestPath(f.VMDir))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CommitConfig(f.Service, durable.Config); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("manifest written %d times, want once", writes)
	}
}

// A failed write leaves memory and the manifest describing the same config,
// and the next commit retries: success, transient failure and recovery in
// sequence, including a failure after the manifest was already replaced.
func TestCommitConfigFailureThenRetry(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	if _, err := m.Recover(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{f.Config}}); err != nil {
		t.Fatal(err)
	}
	durableHost := func() string {
		t.Helper()
		durable, err := readManifest(manifestPath(f.VMDir))
		if err != nil {
			t.Fatal(err)
		}
		if err := verifyRecoveryManifestIntegrity(durable); err != nil {
			t.Fatal(err)
		}
		return durable.Config.Metadata["host"]
	}
	commit := func(host string) error {
		svc := f.Config
		svc.Metadata = map[string]string{"host": host}
		return m.CommitConfig(f.Service, svc)
	}
	if err := commit("first.example.com"); err != nil {
		t.Fatal(err)
	}
	// Nothing reaches the disk: memory keeps the committed config.
	m.recoveryManifestWriter = func(string, *instanceManifest) error { return errors.New("disk full") }
	if err := commit("second.example.com"); err == nil {
		t.Fatal("failed write reported success")
	}
	if got := m.List()[f.Service].Config.Metadata["host"]; got != "first.example.com" || durableHost() != got {
		t.Fatalf("memory %q and disk %q disagree after a failed write", got, durableHost())
	}
	// The manifest is replaced, then a later step fails: memory follows the disk.
	m.recoveryManifestWriter = func(path string, manifest *instanceManifest) error {
		if err := writeManifest(path, manifest); err != nil {
			return err
		}
		return errors.New("sync directory: input/output error")
	}
	if err := commit("third.example.com"); err == nil {
		t.Fatal("failed sync reported success")
	}
	if got := m.List()[f.Service].Config.Metadata["host"]; got != "third.example.com" || durableHost() != got {
		t.Fatalf("memory %q and disk %q disagree after a late failure", got, durableHost())
	}
	// Recovery: the next commit writes normally.
	m.recoveryManifestWriter = nil
	if err := commit("fourth.example.com"); err != nil {
		t.Fatal(err)
	}
	if got := m.List()[f.Service].Config.Metadata["host"]; got != "fourth.example.com" || durableHost() != got {
		t.Fatalf("retry did not converge: memory %q disk %q", got, durableHost())
	}
}
