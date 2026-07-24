package volume

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

type fakeRunner struct{ calls []string }

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	if name == "resize2fs" && len(args) > 0 && args[0] == "-P" {
		return []byte("Estimated minimum size of the filesystem: 1024\n"), nil
	}
	if name == "tune2fs" {
		return []byte("Block size: 4096\n"), nil
	}
	return nil, nil
}

type acceptingMounts struct{}

func (acceptingMounts) Verify(string) error { return nil }

func localService(size int64, generation int64) config.ServiceConfig {
	return config.ServiceConfig{Name: "app", Volumes: []config.VolumeConfig{{
		Name: "data", Type: config.VolumeTypeLocal, MountPath: "/var/lib/app",
		SizeBytes: size, BoundNode: "node-1", ResizeGeneration: generation,
	}}}
}

func TestManagerCreatesReusesGrowsAndShrinksLocalVolume(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{}
	manager := NewManagerWithDependencies("node-1", config.StorageConfig{Local: &config.LocalStorageConfig{
		Path: root, CapacityBytes: 100 * config.MiB,
	}}, runner, acceptingMounts{})

	prepared, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 1 || prepared[0].SizeBytes != 16*config.MiB {
		t.Fatalf("unexpected prepared volume: %#v", prepared)
	}
	if info, err := os.Stat(prepared[0].PathOnHost); err != nil || info.Size() != 16*config.MiB {
		t.Fatalf("unexpected image size: %v, %v", info, err)
	}
	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatalf("reuse failed: %v", err)
	}
	if _, err := manager.Prepare(context.Background(), localService(24*config.MiB, 2)); err != nil {
		t.Fatalf("grow failed: %v", err)
	}
	if _, err := manager.Prepare(context.Background(), localService(12*config.MiB, 3)); err != nil {
		t.Fatalf("shrink failed: %v", err)
	}
	if info, err := os.Stat(prepared[0].PathOnHost); err != nil || info.Size() != 12*config.MiB {
		t.Fatalf("unexpected final image size: %v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(root, "app", "data", transactionFilename)); !os.IsNotExist(err) {
		t.Fatalf("resize transaction was not cleared: %v", err)
	}
	joined := strings.Join(runner.calls, "\n")
	for _, command := range []string{"mkfs.ext4 -F -m 0", "e2fsck -f -y", "resize2fs"} {
		if !strings.Contains(joined, command) {
			t.Fatalf("commands %q do not include %q", joined, command)
		}
	}
}

func TestManagerRejectsBindingCapacityAndSharedRuntime(t *testing.T) {
	root := t.TempDir()
	manager := NewManagerWithDependencies("node-1", config.StorageConfig{Local: &config.LocalStorageConfig{Path: root, CapacityBytes: 8 * config.MiB}}, &fakeRunner{}, acceptingMounts{})
	if err := manager.Preflight(context.Background(), localService(16*config.MiB, 1)); err == nil || !strings.Contains(err.Error(), "capacity exceeded") {
		t.Fatalf("expected capacity error, got %v", err)
	}
	wrong := localService(config.MiB, 1)
	wrong.Volumes[0].BoundNode = "node-2"
	if err := manager.Preflight(context.Background(), wrong); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected binding error, got %v", err)
	}
	shared := config.ServiceConfig{Name: "app", Volumes: []config.VolumeConfig{{Name: "data", Type: config.VolumeTypeShared, MountPath: "/data", SizeBytes: config.GiB}}}
	if err := manager.Preflight(context.Background(), shared); !errors.Is(err, ErrSharedUnsupported) && (err == nil || !strings.Contains(err.Error(), ErrSharedUnsupported.Error())) {
		t.Fatalf("expected shared safety gate, got %v", err)
	}
}

func TestManagerRecoversInterruptedGrowAndShrink(t *testing.T) {
	for _, tc := range []struct {
		name, direction, phase string
		oldSize, desiredSize   int64
	}{
		{name: "grow after file extension", direction: "grow", phase: "file_extended", oldSize: 16 * config.MiB, desiredSize: 24 * config.MiB},
		{name: "shrink after file truncation", direction: "shrink", phase: "filesystem_shrunk", oldSize: 24 * config.MiB, desiredSize: 16 * config.MiB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			manager := NewManagerWithDependencies("node-1", config.StorageConfig{Local: &config.LocalStorageConfig{
				Path: root, CapacityBytes: 100 * config.MiB,
			}}, &fakeRunner{}, acceptingMounts{})
			prepared, err := manager.Prepare(context.Background(), localService(tc.oldSize, 1))
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Dir(prepared[0].PathOnHost)
			if err := writeJSONAtomic(filepath.Join(dir, transactionFilename), resizeTransaction{
				OldSizeBytes: tc.oldSize, DesiredSizeBytes: tc.desiredSize, Generation: 2,
				Direction: tc.direction, Phase: tc.phase,
			}); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(prepared[0].PathOnHost, tc.desiredSize); err != nil {
				t.Fatal(err)
			}

			got, err := manager.Prepare(context.Background(), localService(tc.desiredSize, 2))
			if err != nil {
				t.Fatal(err)
			}
			if got[0].SizeBytes != tc.desiredSize || got[0].ResizeGeneration != 2 {
				t.Fatalf("unexpected recovered volume: %#v", got[0])
			}
			if _, err := os.Stat(filepath.Join(dir, transactionFilename)); !os.IsNotExist(err) {
				t.Fatalf("resize transaction was not cleared: %v", err)
			}
		})
	}
}

func TestManagerClearsTransactionLeftAfterManifestCommit(t *testing.T) {
	root := t.TempDir()
	manager := NewManagerWithDependencies("node-1", config.StorageConfig{Local: &config.LocalStorageConfig{
		Path: root, CapacityBytes: 100 * config.MiB,
	}}, &fakeRunner{}, acceptingMounts{})
	prepared, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(prepared[0].PathOnHost)
	if err := writeJSONAtomic(filepath.Join(dir, transactionFilename), resizeTransaction{
		OldSizeBytes: 8 * config.MiB, DesiredSizeBytes: 16 * config.MiB,
		Generation: 1, Direction: "grow", Phase: "filesystem_resized",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, transactionFilename)); !os.IsNotExist(err) {
		t.Fatalf("committed transaction was not cleared: %v", err)
	}
}

func TestManagerQuarantinesAmbiguousRetainedState(t *testing.T) {
	t.Run("image without manifest", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "app", "data")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := createSparseImage(filepath.Join(dir, imageFilename), 16*config.MiB); err != nil {
			t.Fatal(err)
		}
		manager := NewManagerWithDependencies("node-1", config.StorageConfig{Local: &config.LocalStorageConfig{Path: root, CapacityBytes: 100 * config.MiB}}, &fakeRunner{}, acceptingMounts{})
		if err := manager.Preflight(context.Background(), localService(16*config.MiB, 1)); err == nil || !strings.Contains(err.Error(), "quarantined") {
			t.Fatalf("expected quarantine error, got %v", err)
		}
	})

	t.Run("truncated image without transaction", func(t *testing.T) {
		root := t.TempDir()
		manager := NewManagerWithDependencies("node-1", config.StorageConfig{Local: &config.LocalStorageConfig{Path: root, CapacityBytes: 100 * config.MiB}}, &fakeRunner{}, acceptingMounts{})
		prepared, err := manager.Prepare(context.Background(), localService(24*config.MiB, 1))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(prepared[0].PathOnHost, 16*config.MiB); err != nil {
			t.Fatal(err)
		}
		if err := manager.Preflight(context.Background(), localService(16*config.MiB, 2)); err == nil || !strings.Contains(err.Error(), "quarantined") {
			t.Fatalf("expected quarantine error, got %v", err)
		}
	})
}

func TestManagerRejectsShrinkBelowSafeMinimum(t *testing.T) {
	root := t.TempDir()
	manager := NewManagerWithDependencies("node-1", config.StorageConfig{Local: &config.LocalStorageConfig{
		Path: root, CapacityBytes: 100 * config.MiB,
	}}, &fakeRunner{}, acceptingMounts{})
	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatal(err)
	}
	if err := manager.Preflight(context.Background(), localService(4*config.MiB, 2)); err == nil || !strings.Contains(err.Error(), "below safe minimum") {
		t.Fatalf("expected safe-minimum error, got %v", err)
	}
}
