package volume

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/agentconfig"
	"github.com/artemnikitin/firework/internal/config"
)

func hardeningManager(t *testing.T, runner CommandRunner) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	return NewManagerWithDependencies("node-1", agentconfig.StorageConfig{Local: &agentconfig.LocalStorageConfig{
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

// A retained manifest carrying a non-positive applied size subtracts from the
// pool's reserved total, admitting a volume the pool cannot hold.
func TestMalformedRetainedSizeCannotBypassPoolCapacity(t *testing.T) {
	manager, root := hardeningManager(t, &fakeRunner{})
	// A neighbouring service's retained manifest with a negative applied size.
	dir := filepath.Join(root, "other", "data")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(dir, manifestFilename), manifest{
		LogicalID: "other/data", Type: config.VolumeTypeLocal, BoundNode: "node-1",
		Filesystem: "ext4", AppliedSizeBytes: -100 * config.MiB, ResizeGeneration: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// The pool is 100 MiB; 150 MiB must not fit regardless of the bad record.
	_, err := manager.Preflight(context.Background(), localService(150*config.MiB, 1))
	if err == nil {
		t.Fatal("a negative retained size let an oversized volume into the pool")
	}
	if !strings.Contains(err.Error(), "capacity") && !strings.Contains(err.Error(), "quarantined") {
		t.Fatalf("expected a capacity or quarantine failure, got %v", err)
	}
}

// The agent derives a filesystem path from the service name, so a name that is
// not a safe path component fails at preflight. configcheck must reject it too.
func TestServiceNameIsValidatedByTheExportedValidator(t *testing.T) {
	nc := config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{{
		Name: "bad/name", Image: "/i", Kernel: "/k", VCPUs: 1, MemoryMB: 128,
		Volumes: []config.VolumeConfig{{
			Name: "data", Type: config.VolumeTypeLocal, MountPath: "/var/lib/app",
			SizeBytes: config.MiB, BoundNode: "node-1", ResizeGeneration: 1,
		}},
	}}}
	if err := ValidateNodeVolumes(nc); err == nil {
		t.Fatal("a service name that is not a safe path component must be rejected")
	}
}

// readRetained keys the pool's reservation map by the manifest's *declared*
// LogicalID rather than by its path. Two manifests claiming the same logical ID
// therefore collapse to one entry, and the other volume's bytes vanish from the
// reserved total — a capacity bypass from state the node already holds.
func TestRetainedManifestCannotMaskAnotherReservation(t *testing.T) {
	manager, root := hardeningManager(t, &fakeRunner{})

	// Two real, separate retained volumes, each 40 MiB in a 100 MiB pool.
	for _, svc := range []string{"alpha", "beta"} {
		dir := filepath.Join(root, svc, "data")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		// beta's manifest lies about its identity and claims alpha's.
		logicalID := svc + "/data"
		if svc == "beta" {
			logicalID = "alpha/data"
		}
		if err := writeJSONAtomic(filepath.Join(dir, manifestFilename), manifest{
			LogicalID: logicalID, Type: config.VolumeTypeLocal, BoundNode: "node-1",
			Filesystem: "ext4", AppliedSizeBytes: 40 * config.MiB, ResizeGeneration: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 80 MiB is genuinely retained. A new 40 MiB volume would need 120 MiB of a
	// 100 MiB pool and must be refused.
	_, err := manager.Preflight(context.Background(), localService(40*config.MiB, 1))
	if err == nil {
		t.Fatal("a mislabelled retained manifest masked another volume's reservation")
	}
}

// Normalization runs on every desired service every tick, including shapes it
// must leave alone. None may panic or corrupt the config.
func TestNormalizeIgnoresWhatItMustNotTouch(t *testing.T) {
	manager, _ := hardeningManager(t, &fakeRunner{})
	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatal(err)
	}

	services := []config.ServiceConfig{
		{Name: "app", Volumes: []config.VolumeConfig{
			// shared: not this manager's business
			{Name: "s", Type: config.VolumeTypeShared, MountPath: "/s", SizeBytes: config.MiB, ResizeGeneration: 1},
			// name that is not a path component
			{Name: "bad/name", Type: config.VolumeTypeLocal, MountPath: "/b", SizeBytes: config.MiB, ResizeGeneration: 1},
			// no manifest on disk
			{Name: "absent", Type: config.VolumeTypeLocal, MountPath: "/a", SizeBytes: config.MiB, ResizeGeneration: 1},
		}},
		{Name: "no-volumes"},
	}
	before := append([]config.VolumeConfig(nil), services[0].Volumes...)
	manager.NormalizeVolumes(services)
	for i := range before {
		if services[0].Volumes[i] != before[i] {
			t.Fatalf("normalization altered a volume it should not touch: %#v -> %#v", before[i], services[0].Volumes[i])
		}
	}
	if len(manager.Rejections()) != 0 {
		t.Fatalf("no refusal exists, got %#v", manager.Rejections())
	}
}

// A retained manifest at an unexpected depth has no recoverable identity, so
// it must fail closed rather than being silently skipped from capacity.
func TestMisplacedRetainedManifestFailsClosed(t *testing.T) {
	manager, root := hardeningManager(t, &fakeRunner{})
	stray := filepath.Join(root, "stray")
	if err := os.MkdirAll(stray, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(stray, manifestFilename), manifest{
		LogicalID: "stray/data", Type: config.VolumeTypeLocal, BoundNode: "node-1",
		Filesystem: "ext4", AppliedSizeBytes: 90 * config.MiB, ResizeGeneration: 1,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := manager.Preflight(context.Background(), localService(50*config.MiB, 1))
	if err == nil {
		t.Fatal("a retained manifest with no recoverable identity must fail closed")
	}
	if !strings.Contains(err.Error(), "quarantined") {
		t.Fatalf("expected a quarantine, got %v", err)
	}
}

// The exported validator must reject more volumes than the agent will run.
func TestValidatorEnforcesTheVolumeCountCap(t *testing.T) {
	volumes := make([]config.VolumeConfig, config.MaxServiceVolumes+1)
	for i := range volumes {
		volumes[i] = config.VolumeConfig{
			Name: "v" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			Type: config.VolumeTypeLocal, MountPath: "/mnt/" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			SizeBytes: config.MiB, BoundNode: "node-1", ResizeGeneration: 1,
		}
	}
	nc := config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{{Name: "app", Volumes: volumes}}}
	if err := ValidateNodeVolumes(nc); err == nil {
		t.Fatalf("expected the %d-volume cap to be enforced", config.MaxServiceVolumes)
	}
}

// The agent-side half of the same lifecycle, driven as consecutive ticks with
// an agent restart in the middle. Each tick is normalize -> prepare, which is
// the real order, and the invariant checked throughout is that no tick ever
// produces a configuration differing from what is running.
func TestAgentLifecycleAcrossARestart(t *testing.T) {
	manager, root := hardeningManager(t, &fakeRunner{})

	// A tick: normalize the desired config, then prepare it.
	tick := func(m *Manager, size, generation int64) (config.VolumeConfig, PreparedVolume) {
		t.Helper()
		services := []config.ServiceConfig{localService(size, generation)}
		m.NormalizeVolumes(services)
		prepared, err := m.Prepare(context.Background(), services[0])
		if err != nil {
			t.Fatalf("prepare failed for (%d, %d): %v", size, generation, err)
		}
		return services[0].Volumes[0], prepared[0]
	}

	// Create, then refuse a shrink.
	tick(manager, 16*config.MiB, 1)
	tick(manager, 2*config.MiB, 2)
	if len(manager.Rejections()) != 1 {
		t.Fatalf("expected a standing refusal, got %#v", manager.Rejections())
	}

	// Two more ticks at the same request must converge on the effective config
	// and keep reporting the refusal.
	for i := 0; i < 2; i++ {
		rendered, prepared := tick(manager, 2*config.MiB, 2)
		if rendered.SizeBytes != 16*config.MiB || rendered.ResizeGeneration != 1 {
			t.Fatalf("tick %d did not converge: %#v", i, rendered)
		}
		if prepared.SizeBytes != 16*config.MiB {
			t.Fatalf("tick %d prepared the refused size: %#v", i, prepared)
		}
		if len(manager.Rejections()) != 1 {
			t.Fatalf("tick %d lost the refusal", i)
		}
	}

	// Restart the agent over the same pool: the refusal must come back from
	// disk on the first tick, not a tick later.
	restarted, _ := hardeningManager(t, &fakeRunner{})
	restarted.storage.Local.Path = root
	rendered, _ := tick(restarted, 2*config.MiB, 2)
	if rendered.SizeBytes != 16*config.MiB || rendered.ResizeGeneration != 1 {
		t.Fatalf("the restarted agent did not converge: %#v", rendered)
	}
	if len(restarted.Rejections()) != 1 {
		t.Fatalf("the restarted agent lost the refusal: %#v", restarted.Rejections())
	}

	// The control plane acknowledges: it now renders the effective size at the
	// refused generation. That must converge and stop reporting a refusal.
	rendered, _ = tick(restarted, 16*config.MiB, 2)
	if rendered.SizeBytes != 16*config.MiB || rendered.ResizeGeneration != 1 {
		t.Fatalf("the acknowledged shape did not converge: %#v", rendered)
	}
	if len(restarted.Rejections()) != 0 {
		t.Fatalf("the withdrawn request still reports a refusal: %#v", restarted.Rejections())
	}

	// A corrected, feasible shrink then applies for real.
	rendered, prepared := tick(restarted, 8*config.MiB, 3)
	if prepared.Rejected || prepared.SizeBytes != 8*config.MiB {
		t.Fatalf("the corrected shrink did not apply: %#v", prepared)
	}
	if rendered.SizeBytes != 8*config.MiB {
		t.Fatalf("the corrected shrink was clamped: %#v", rendered)
	}
	if len(restarted.Rejections()) != 0 {
		t.Fatalf("an applied resize left a refusal behind: %#v", restarted.Rejections())
	}

	// And the pool's accounting followed the effective size the whole way.
	retained, err := readRetained(root)
	if err != nil {
		t.Fatal(err)
	}
	if retained["app/data"] != 8*config.MiB {
		t.Fatalf("retained accounting drifted: %#v", retained)
	}
}
