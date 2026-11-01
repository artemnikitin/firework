package vm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

// A failed write of the stopping record leaves an exited VM in its previous
// state. Reporting it running would hide it from every later restart.
func TestFailedStopWriteKeepsPreviousState(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-firecracker")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
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
	m.recoveryManifestWriter = func(string, *instanceManifest) error { return errors.New("disk full") }
	if err := m.Stop("svc"); err == nil {
		t.Fatal("stop with a failing manifest write succeeded")
	}
	if got := m.List()["svc"]; got.State != StateFailed {
		t.Fatalf("failed stop left state %s, want %s", got.State, StateFailed)
	}
}

func waitForState(t *testing.T, m *Manager, name string, want State) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if inst := m.List()[name]; inst != nil && inst.State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("service %s did not reach %s: %#v", name, want, m.List()[name])
}
