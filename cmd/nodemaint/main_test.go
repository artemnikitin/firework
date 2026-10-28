package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/controlplane"
)

func TestUsageAndUnknownCommand(t *testing.T) {
	var out bytes.Buffer
	if err := run(nil, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"clear-node-tombstone", "--expected-generation", "--confirm-host-fenced", "--retained-pool-reference"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage does not contain %q:\n%s", want, out.String())
		}
	}
	if err := run([]string{"clear-tombstone"}, &out); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("unknown command error = %v", err)
	}
}

func TestNodeMaintenanceExampleConfigValidatesWithoutServerCredentials(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "node-maintenance.yaml")
	cfg, err := controlplane.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Role != controlplane.RoleNodeMaintenance || cfg.State.Backend != "s3" || cfg.State.S3.Bucket == "" {
		t.Fatalf("maintenance config = %#v", cfg)
	}
}

func TestRecoveryCommandRequiresExplicitSafetyInputsBeforeLoadingConfig(t *testing.T) {
	for _, args := range [][]string{
		{"clear-node-tombstone", "--config", "/missing", "--node", "node-1"},
		{"clear-node-tombstone", "--config", "/missing", "--node", "node-1", "--expected-generation", "4", "--reason", "test", "--external-fence-reference", "ticket", "--retained-pool-reference", "none"},
	} {
		if err := run(args, &bytes.Buffer{}); err == nil || strings.Contains(err.Error(), "reading config") {
			t.Fatalf("args %v should be rejected before config load, got %v", args, err)
		}
	}
}
