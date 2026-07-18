package vm

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

func TestManagerClearsPIDAndRecordsProcessFailure(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-firecracker")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(binary, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := manager.Start(context.Background(), config.ServiceConfig{Name: "service", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		instance := manager.List()["service"]
		if instance != nil && instance.State == StateFailed {
			if instance.PID != 0 {
				t.Fatalf("failed instance retained exited PID %d", instance.PID)
			}
			if instance.LastError == "" {
				t.Fatal("failed instance did not retain a process error")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("instance did not transition to failed: %#v", manager.List()["service"])
}
