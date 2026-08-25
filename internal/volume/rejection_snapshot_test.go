package volume

import (
	"context"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

// The control plane renders the applied size with the *refused*
// generation. Normalization must recognize that form, or the generation stays
// different from the running instance and the reconciler re-plans an update.
func TestNormalizeRecognizesTheControlPlaneClampedForm(t *testing.T) {
	manager, _ := hardeningManager(t, &fakeRunner{})
	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prepare(context.Background(), localService(2*config.MiB, 2)); err != nil {
		t.Fatal(err)
	}

	// What the control plane renders after acknowledging the rejection:
	// effective size, refused generation.
	rendered := localService(16*config.MiB, 2)
	services := []config.ServiceConfig{rendered}
	manager.NormalizeVolumes(services)

	got := services[0].Volumes[0]
	if got.SizeBytes != 16*config.MiB || got.ResizeGeneration != 1 {
		t.Fatalf("expected normalization to the effective (size, generation) = (16MiB, 1), got (%d, %d)",
			got.SizeBytes, got.ResizeGeneration)
	}
}

// After an agent restart the snapshot is empty. Normalization can
// clamp from the durable manifest and plan no action, so nothing ever
// repopulates it and the node falsely reports every size applied.
func TestRejectionSnapshotSurvivesAnAgentRestart(t *testing.T) {
	manager, root := hardeningManager(t, &fakeRunner{})
	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prepare(context.Background(), localService(2*config.MiB, 2)); err != nil {
		t.Fatal(err)
	}

	// A fresh manager over the same pool is exactly an agent restart.
	restarted, _ := hardeningManager(t, &fakeRunner{})
	restarted.storage.Local.Path = root
	if got := restarted.Rejections(); len(got) != 0 {
		t.Fatalf("precondition: a fresh manager starts empty, got %#v", got)
	}

	services := []config.ServiceConfig{localService(2*config.MiB, 2)}
	restarted.NormalizeVolumes(services)

	if got := restarted.Rejections(); len(got) != 1 {
		t.Fatalf("expected normalization to restore the durable rejection, got %#v", got)
	}
}

// A volume that is no longer declared must drop out of the
// snapshot, or VolumeSizesApplied stays false forever.
func TestRemovedVolumeDropsOutOfTheRejectionSnapshot(t *testing.T) {
	manager, _ := hardeningManager(t, &fakeRunner{})
	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prepare(context.Background(), localService(2*config.MiB, 2)); err != nil {
		t.Fatal(err)
	}
	if got := manager.Rejections(); len(got) != 1 {
		t.Fatalf("precondition: expected one rejection, got %#v", got)
	}

	// The service no longer declares any volume.
	manager.NormalizeVolumes([]config.ServiceConfig{{Name: "app"}})

	if got := manager.Rejections(); len(got) != 0 {
		t.Fatalf("expected the stale rejection to be pruned, got %#v", got)
	}
}

// After a refusal, asking for the size already running withdraws the request. The refusal must stop being reported — otherwise the node is
// degraded forever with no way out but a generation bump.
func TestWithdrawnRequestStopsBeingReported(t *testing.T) {
	manager, _ := hardeningManager(t, &fakeRunner{})
	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prepare(context.Background(), localService(2*config.MiB, 2)); err != nil {
		t.Fatal(err)
	}
	if len(manager.Rejections()) != 1 {
		t.Fatal("precondition: expected a standing rejection")
	}

	// The request is now the size already running, at the same generation.
	withdrawn := []config.ServiceConfig{localService(16*config.MiB, 2)}
	manager.NormalizeVolumes(withdrawn)

	// The clamp must still hold, so no restart is planned...
	got := withdrawn[0].Volumes[0]
	if got.SizeBytes != 16*config.MiB || got.ResizeGeneration != 1 {
		t.Fatalf("expected the effective configuration, got (%d, %d)", got.SizeBytes, got.ResizeGeneration)
	}
	// ...but nothing is being refused any more.
	if len(manager.Rejections()) != 0 {
		t.Fatalf("a withdrawn request is not a standing refusal: %#v", manager.Rejections())
	}
}
