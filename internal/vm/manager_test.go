package vm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/volume"
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

	// Process reaping can be delayed when the full suite is running in parallel,
	// especially under the race detector. Keep polling rather than making the
	// assertion depend on host load.
	deadline := time.Now().Add(10 * time.Second)
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

func TestWriteVMConfigAddsDeterministicVolumeDrivesAndPayload(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager("/bin/true", dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	path, err := manager.writeVMConfig(dir, config.ServiceConfig{
		Name: "app", Image: "/root.ext4", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128,
		KernelArgs: "console=ttyS0 init=/sbin/fc-init /bin/app -- flag",
	}, []volume.PreparedVolume{
		{LogicalID: "app/z", PathOnHost: "/z.ext4", MountPath: "/z", Type: config.VolumeTypeLocal},
		{LogicalID: "app/a", PathOnHost: "/a.ext4", MountPath: "/a", Type: config.VolumeTypeLocal},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg firecrackerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Drives) != 3 || cfg.Drives[1].PathOnHost != "/a.ext4" || cfg.Drives[2].PathOnHost != "/z.ext4" {
		t.Fatalf("unexpected drives: %#v", cfg.Drives)
	}
	fields := strings.Fields(cfg.BootSource.BootArgs)
	var encoded string
	for i, field := range fields {
		if strings.HasPrefix(field, "firework.volumes64=") {
			encoded = strings.TrimPrefix(field, "firework.volumes64=")
			if i+1 >= len(fields) || fields[i+1] != "--" {
				t.Fatalf("volume payload is not before application separator: %q", cfg.BootSource.BootArgs)
			}
		}
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var payload guestVolumePayload
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Volumes[0].Device != "/dev/vdb" || payload.Volumes[0].Name != "a" || payload.Volumes[1].Device != "/dev/vdc" {
		t.Fatalf("unexpected payload: %#v", payload)
	}
}

func TestValidateVolumeKernelArgsRejectsPayloadBeyondPortableLimit(t *testing.T) {
	err := validateVolumeKernelArgs(config.ServiceConfig{
		Name: "app", KernelArgs: strings.Repeat("x", maxKernelCommandLineBytes),
		Volumes: []config.VolumeConfig{{
			Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "kernel command line with volume payload") {
		t.Fatalf("expected command-line limit error, got %v", err)
	}
}

func TestPreflightRetainsVisibleVolumeError(t *testing.T) {
	manager := NewManager("/bin/true", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	service := config.ServiceConfig{Name: "app", Volumes: []config.VolumeConfig{{
		Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data",
	}}}
	if err := manager.Preflight(context.Background(), service); err == nil {
		t.Fatal("expected missing storage error")
	}
	if got := manager.VolumeError("app"); !strings.Contains(got, "storage is not configured") {
		t.Fatalf("unexpected retained volume error %q", got)
	}
}
