package vm

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

// A health restart (Stop, then Start with the same config) keeps the crashed
// run's firecracker.log. The launcher appends, as it did before ownership
// manifests existed.
func TestHealthRestartKeepsCrashLog(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-firecracker")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho crash-marker\nsleep 0.2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := NewManager(binary, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	launcher := &specCapturingLauncher{inner: &directLauncher{binary: binary}}
	m.launcher = launcher
	m.inspector = &round3DirectInspector{launchedSpecInspector: &launchedSpecInspector{launcher: launcher, executable: binary}}
	svc := config.ServiceConfig{Name: "svc", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}
	if err := m.Start(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	waitForState(t, m, "svc", StateFailed)
	logPath := filepath.Join(dir, "vms", "svc", "firecracker.log")
	before, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(before), "crash-marker") {
		t.Fatalf("setup: crash log %q: %v", before, err)
	}
	if err := m.Stop("svc"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := m.Start(context.Background(), svc); err != nil {
		t.Fatalf("restart: %v", err)
	}
	waitForState(t, m, "svc", StateFailed)
	after, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(after), "crash-marker"); got != 2 {
		t.Fatalf("restart log has %d crash markers, want both runs: %q", got, after)
	}
	for _, name := range []string{manifestFileName, "vm-config.json"} {
		if _, err := os.Stat(filepath.Join(dir, "vms", "svc", name)); err != nil {
			t.Fatalf("restart did not rewrite %s: %v", name, err)
		}
	}
}
