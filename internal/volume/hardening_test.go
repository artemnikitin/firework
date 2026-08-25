package volume

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

func hardeningManager(t *testing.T, runner CommandRunner) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	return NewManagerWithDependencies("node-1", config.StorageConfig{Local: &config.LocalStorageConfig{
		Path: root, CapacityBytes: 100 * config.MiB,
	}}, runner, acceptingMounts{}), root
}

func volumePaths(root string) (dir, image, manifest, marker string) {
	dir = filepath.Join(root, "app", "data")
	return dir, filepath.Join(dir, imageFilename), filepath.Join(dir, manifestFilename), filepath.Join(dir, creationMarkerFilename)
}

// crashingRunner stops the create sequence at a chosen command, standing in for
// a process that died partway through a first creation.
type crashingRunner struct {
	fakeRunner
	failOn string
}

func (r *crashingRunner) RunDestructive(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == r.failOn {
		return nil, errors.New("simulated crash")
	}
	return r.fakeRunner.RunDestructive(ctx, name, args...)
}

// A crash after the image is created but before the manifest is written leaves
// an empty volume. Nothing is protected by failing closed there, so the marker
// makes it recoverable without an operator rm.
func TestInterruptedCreationRecoversWithoutOperatorIntervention(t *testing.T) {
	runner := &crashingRunner{failOn: "mkfs.ext4"}
	manager, root := hardeningManager(t, runner)
	dir, image, manifest, marker := volumePaths(root)

	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err == nil {
		t.Fatal("expected the simulated crash to fail the first creation")
	}
	if _, err := os.Stat(image); err != nil {
		t.Fatalf("expected the partially created image to remain: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected a creation marker to record the interrupted attempt: %v", err)
	}

	manager, _ = hardeningManager(t, &fakeRunner{})
	manager.storage.Local.Path = root
	prepared, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1))
	if err != nil {
		t.Fatalf("expected the interrupted creation to be recoverable: %v", err)
	}
	if len(prepared) != 1 || prepared[0].SizeBytes != 16*config.MiB {
		t.Fatalf("unexpected recovered volume: %#v", prepared)
	}
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("expected a manifest after recovery: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("expected the marker to be removed after a successful creation: %v", err)
	}
	_ = dir
}

// A crash between the image being sized and mkfs running is the same case one
// step earlier, and must recover the same way.
func TestInterruptedCreationBeforeMkfsRecovers(t *testing.T) {
	manager, root := hardeningManager(t, &fakeRunner{})
	dir, image, _, _ := volumePaths(root)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := writeCreationMarker(dir, "app", localService(16*config.MiB, 1).Volumes[0], "node-1"); err != nil {
		t.Fatal(err)
	}
	if err := createSparseImage(image, 16*config.MiB); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatalf("expected recovery from a sized-but-unformatted image: %v", err)
	}
}

// The manifest was written but the process died before the marker was removed.
// This is ordinary reuse — and the marker must not survive it, because a later
// manifest loss would otherwise make a populated image look like an
// interrupted empty creation and authorize deleting it.
func TestSurvivingMarkerIsClearedOnReuse(t *testing.T) {
	manager, root := hardeningManager(t, &fakeRunner{})
	dir, _, _, marker := volumePaths(root)

	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatal(err)
	}
	// Recreate the state a crash between the manifest write and the marker
	// removal leaves behind.
	if err := writeCreationMarker(dir, "app", localService(16*config.MiB, 1).Volumes[0], "node-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatalf("reuse failed: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("expected reuse to clear a marker that outlived its condition: %v", err)
	}
}

// An image Firework did not create is exactly what failing closed is for, and
// a marker that names a different node or volume proves nothing about this one.
func TestUnprovenImageStaysQuarantined(t *testing.T) {
	tests := []struct {
		name   string
		marker *creationMarker
	}{
		{name: "no marker"},
		{name: "marker names another node", marker: &creationMarker{LogicalID: "app/data", NodeID: "node-2"}},
		{name: "marker names another volume", marker: &creationMarker{LogicalID: "other/data", NodeID: "node-1"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, root := hardeningManager(t, &fakeRunner{})
			dir, image, _, marker := volumePaths(root)
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := createSparseImage(image, 16*config.MiB); err != nil {
				t.Fatal(err)
			}
			if test.marker != nil {
				test.marker.CreatedAt = time.Now().UTC()
				if err := writeJSONAtomic(marker, *test.marker); err != nil {
					t.Fatal(err)
				}
			}
			_, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1))
			if err == nil || !strings.Contains(err.Error(), "quarantined") {
				t.Fatalf("expected the volume to stay quarantined, got %v", err)
			}
		})
	}
}

// Filesystem-mutating commands must not run on a context the agent's signal
// handler cancels, because exec.CommandContext cancellation is SIGKILL.
func TestDestructiveCommandsDoNotTakeTheCancellablePath(t *testing.T) {
	runner := &fakeRunner{}
	manager, _ := hardeningManager(t, runner)

	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prepare(context.Background(), localService(24*config.MiB, 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prepare(context.Background(), localService(20*config.MiB, 3)); err != nil {
		t.Fatal(err)
	}

	destructive := strings.Join(runner.destructive, "\n")
	for _, want := range []string{"mkfs.ext4", "e2fsck", "resize2fs"} {
		if !strings.Contains(destructive, want) {
			t.Fatalf("expected %s to run on the uncancellable path, got:\n%s", want, destructive)
		}
	}
	// The shrink measurement is read-only and must stay promptly cancellable.
	for _, call := range runner.destructive {
		if strings.HasPrefix(call, "resize2fs -P") || strings.HasPrefix(call, "tune2fs") {
			t.Fatalf("measurement command %q must not be detached from the caller's context", call)
		}
	}
}

// A cancelled parent context must not stop a destructive command, which is the
// whole reason RunDestructive exists.
func TestRunDestructiveSurvivesParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := (execRunner{}).RunDestructive(ctx, "true"); err != nil {
		t.Fatalf("destructive command was killed by the cancelled parent context: %v", err)
	}
	if _, err := (execRunner{}).Run(ctx, "true"); err == nil {
		t.Fatal("expected a read-only command to remain promptly cancellable")
	}
}
