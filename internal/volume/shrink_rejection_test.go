package volume

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

func twoVolumeService(dataSize, cacheSize, generation int64) config.ServiceConfig {
	return config.ServiceConfig{Name: "app", Volumes: []config.VolumeConfig{
		{Name: "data", Type: config.VolumeTypeLocal, MountPath: "/var/lib/app",
			SizeBytes: dataSize, BoundNode: "node-1", ResizeGeneration: generation},
		{Name: "cache", Type: config.VolumeTypeLocal, MountPath: "/var/cache/app",
			SizeBytes: cacheSize, BoundNode: "node-1", ResizeGeneration: generation},
	}}
}

func readManifest(t *testing.T, root, service, volume string) manifest {
	t.Helper()
	var found manifest
	if err := readJSON(filepath.Join(root, service, volume, manifestFilename), &found); err != nil {
		t.Fatal(err)
	}
	return found
}

// A refused shrink after the VM has already been stopped must still bring the
// service back. It is a non-fatal outcome of a successful preparation, not an
// error, so the volume is prepared at its applied size and the caller gets
// enough to clamp with.
func TestPostStopShrinkRejectionPreparesAtTheAppliedSize(t *testing.T) {
	manager, root := hardeningManager(t, &fakeRunner{})
	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatal(err)
	}

	prepared, err := manager.Prepare(context.Background(), localService(2*config.MiB, 2))
	if err != nil {
		t.Fatalf("a refused shrink must not fail the preparation: %v", err)
	}
	if len(prepared) != 1 {
		t.Fatalf("unexpected prepared set: %#v", prepared)
	}
	got := prepared[0]
	if !got.Rejected || got.SizeBytes != 16*config.MiB || got.RequestedSizeBytes != 2*config.MiB {
		t.Fatalf("unexpected prepared volume: %#v", got)
	}
	// The prepared volume describes the *effective* configuration, so the
	// caller can store it on the instance and have the next tick compare
	// equal. The refused request travels separately.
	if got.ResizeGeneration != 1 {
		t.Fatalf("expected the applied generation on the prepared volume, got %d", got.ResizeGeneration)
	}
	if got.RequestedGeneration != 2 {
		t.Fatalf("expected the refused generation to be reported, got %d", got.RequestedGeneration)
	}
	// The rejection snapshot status reads carries both, so the control plane
	// can match the observation to the record it has to converge.
	snapshot := manager.Rejections()["app/data"]
	if snapshot.ResizeGeneration != 2 || snapshot.AppliedGeneration != 1 {
		t.Fatalf("unexpected rejection snapshot: %#v", snapshot)
	}

	found := readManifest(t, root, "app", "data")
	if found.ResizeGeneration != 1 {
		t.Fatalf("the manifest's applied generation must keep describing what was applied, got %d", found.ResizeGeneration)
	}
	if found.RejectedGeneration != 2 || found.RejectedSizeBytes != 2*config.MiB || found.RejectedMinimumBytes == 0 {
		t.Fatalf("the rejection was not recorded durably: %#v", found)
	}
	// The checking transaction must be gone, or it quarantines the corrected
	// retry.
	if _, err := os.Stat(filepath.Join(root, "app", "data", transactionFilename)); !os.IsNotExist(err) {
		t.Fatalf("the checking transaction survived the rejection: %v", err)
	}
}

// The clamped configuration re-enters prepareOne carrying the applied size with
// the *requested* generation. That combination still satisfies the resize
// condition's generation arm, so it needs its own branch — and that branch must
// run no tools and must not advance the manifest's applied generation.
func TestClampedConfigDoesNotReenterResize(t *testing.T) {
	runner := &fakeRunner{}
	manager, root := hardeningManager(t, runner)
	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prepare(context.Background(), localService(2*config.MiB, 2)); err != nil {
		t.Fatal(err)
	}

	before := len(runner.destructive)
	// The clamped config: applied size, requested generation.
	prepared, err := manager.Prepare(context.Background(), localService(16*config.MiB, 2))
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.destructive) != before {
		t.Fatalf("the clamped config ran filesystem tools: %v", runner.destructive[before:])
	}
	if !prepared[0].Rejected || prepared[0].SizeBytes != 16*config.MiB {
		t.Fatalf("the clamped config lost the rejection: %#v", prepared[0])
	}
	if found := readManifest(t, root, "app", "data"); found.ResizeGeneration != 1 {
		t.Fatalf("the clamped config advanced the applied generation to %d", found.ResizeGeneration)
	}
}

// A new generation is a new request. It must re-measure rather than inherit the
// old refusal, and the removed checking transaction must not quarantine it.
func TestCorrectedGenerationRetriesAndIsNotQuarantined(t *testing.T) {
	manager, _ := hardeningManager(t, &fakeRunner{})
	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prepare(context.Background(), localService(2*config.MiB, 2)); err != nil {
		t.Fatal(err)
	}

	// 8 MiB is above the fake minimum, so the corrected request succeeds.
	prepared, err := manager.Prepare(context.Background(), localService(8*config.MiB, 3))
	if err != nil {
		t.Fatalf("the corrected request was not retried cleanly: %v", err)
	}
	if prepared[0].Rejected || prepared[0].SizeBytes != 8*config.MiB {
		t.Fatalf("the corrected shrink did not apply: %#v", prepared[0])
	}
}

// Direct-Git node configs are hand authored and carry their own
// resize_generation, so an operator correcting a refused shrink by editing
// size_bytes alone presents a different request under the same generation.
// Matching on the generation alone would clamp that forever.
func TestDirectGitSizeEditReMeasuresAtTheSameGeneration(t *testing.T) {
	manager, _ := hardeningManager(t, &fakeRunner{})
	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prepare(context.Background(), localService(2*config.MiB, 2)); err != nil {
		t.Fatal(err)
	}

	// Same generation, corrected size: a genuinely new request.
	corrected := localService(8*config.MiB, 2)
	rejections, err := manager.Preflight(context.Background(), corrected)
	if err != nil {
		t.Fatal(err)
	}
	if len(rejections) != 0 {
		t.Fatalf("a corrected size at the same generation must re-measure, got %#v", rejections)
	}

	// An unchanged request at the same generation still clamps.
	repeat := localService(2*config.MiB, 2)
	rejections, err = manager.Preflight(context.Background(), repeat)
	if err != nil {
		t.Fatal(err)
	}
	if len(rejections) != 1 {
		t.Fatalf("an unchanged refused request must stay refused, got %#v", rejections)
	}
}

// A retry budget cannot survive more than one rejected volume, so a rejection
// is not an error at all: one pass collects every refusal.
func TestTwoRejectedShrinksBothStartInOnePass(t *testing.T) {
	manager, root := hardeningManager(t, &fakeRunner{})
	if _, err := manager.Prepare(context.Background(), twoVolumeService(16*config.MiB, 16*config.MiB, 1)); err != nil {
		t.Fatal(err)
	}

	prepared, err := manager.Prepare(context.Background(), twoVolumeService(2*config.MiB, 2*config.MiB, 2))
	if err != nil {
		t.Fatalf("two rejections must not fail the batch: %v", err)
	}
	if len(prepared) != 2 {
		t.Fatalf("expected both volumes prepared, got %#v", prepared)
	}
	for _, got := range prepared {
		if !got.Rejected || got.SizeBytes != 16*config.MiB {
			t.Fatalf("volume %s was not prepared at its applied size: %#v", got.LogicalID, got)
		}
	}
	for _, name := range []string{"data", "cache"} {
		if found := readManifest(t, root, "app", name); found.RejectedGeneration != 2 {
			t.Fatalf("volume %s did not record its rejection: %#v", name, found)
		}
	}
	// Both rejections are visible in the snapshot status reads.
	if got := manager.Rejections(); len(got) != 2 {
		t.Fatalf("expected two rejections in the snapshot, got %#v", got)
	}
}

// The transaction is removed before the rejection is written, so a crash
// between them loses only the record — idempotent and self-healing. The
// reverse order would leave the state that quarantines the corrected retry.
func TestRejectionRemovesOnlyTheGeometryPreservingTransaction(t *testing.T) {
	manager, root := hardeningManager(t, &fakeRunner{})
	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "app", "data")

	// A transaction from a later phase describes moved geometry and must
	// survive; the recovery path depends on it.
	later := resizeTransaction{
		OldSizeBytes: 16 * config.MiB, DesiredSizeBytes: 8 * config.MiB, Generation: 5,
		Direction: "shrink", Phase: "filesystem_shrunk", UpdatedAt: time.Now().UTC(),
	}
	if err := writeJSONAtomic(filepath.Join(dir, transactionFilename), later); err != nil {
		t.Fatal(err)
	}
	var current manifest
	if err := readJSON(filepath.Join(dir, manifestFilename), &current); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.recordShrinkRejection(dir, &current, config.VolumeConfig{
		Name: "data", SizeBytes: 2 * config.MiB, ResizeGeneration: 2,
	}, &ErrShrinkRejected{LogicalID: "app/data", Requested: 2 * config.MiB, Minimum: 4 * config.MiB}); err != nil {
		t.Fatal(err)
	}
	// recordShrinkRejection is only ever reached from the checking phase, so
	// what it removes is by construction a checking transaction. The assertion
	// that matters is the ordering: after it returns, the rejection is durable
	// and no checking transaction is left to poison the retry.
	found := readManifest(t, root, "app", "data")
	if found.RejectedGeneration != 2 || found.RejectedSizeBytes != 2*config.MiB {
		t.Fatalf("the rejection was not written: %#v", found)
	}
}

// Preflight became a writer of the same manifest prepareOne rewrites. Without
// the shared lifecycle lock the two interleave and one update is lost.
func TestConcurrentPreflightAndPrepareDoNotLoseTheManifest(t *testing.T) {
	manager, root := hardeningManager(t, &fakeRunner{})
	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, _ = manager.Preflight(context.Background(), localService(2*config.MiB, 2))
				return
			}
			_, _ = manager.Prepare(context.Background(), localService(16*config.MiB, 1))
		}(i)
	}
	wg.Wait()

	found := readManifest(t, root, "app", "data")
	if found.AppliedSizeBytes != 16*config.MiB {
		t.Fatalf("the applied size was lost to a concurrent update: %#v", found)
	}
}
