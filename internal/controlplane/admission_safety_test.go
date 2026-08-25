package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/scheduler"
)

// when the previous placement cannot be read, a held service has no
// recoverable node — it would be classified as never-placed, left pending, and
// dropped from the rendered node configs, which the agent turns into a delete.
// Publishing anything in that state evicts a healthy workload over a transient
// read failure.
func TestUnreadablePlacementHoldsPublicationWhenServicesAreHeld(t *testing.T) {
	held := map[string]string{"running": scheduler.ReasonVolumeRecordInvalid}

	tests := []struct {
		name                 string
		placementUnavailable bool
		held                 map[string]string
		wantHold             bool
	}{
		{name: "unreadable placement with a held service", placementUnavailable: true, held: held, wantHold: true},
		{name: "unreadable placement with nothing held", placementUnavailable: true, wantHold: false},
		{name: "readable placement with a held service", held: held, wantHold: false},
		{name: "readable placement with nothing held", wantHold: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := heldPlacementUnrecoverable(test.placementUnavailable, test.held); got != test.wantHold {
				t.Fatalf("heldPlacementUnrecoverable = %v, want %v", got, test.wantHold)
			}
		})
	}
}

// An unreadable placement revision must actually
// surface as an error, or the guard above is never reached.
func TestCorruptPlacementRevisionIsReportedAsAnError(t *testing.T) {
	ctx := context.Background()
	controller, store := admissionController(t)

	if _, err := store.PutJSON(ctx, placementCurrentKey("cp/v1/"), RevisionPointer{Revision: "placement-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutRaw(ctx, placementRevisionKey("cp/v1/", "placement-1"), []byte("{not json"), "application/json"); err != nil {
		t.Fatal(err)
	}

	placement, err := controller.readExistingPlacement(ctx)
	if err == nil {
		t.Fatalf("expected a corrupt placement revision to be an error, got placement %#v", placement)
	}
	if placement != nil {
		t.Fatalf("a failed read must not return a partial placement: %#v", placement)
	}
}

// a shrink does not raise the reservation, so it must not be
// subject to capacity admission. Otherwise a pool reconfigured smaller than
// its applied volumes refuses the very shrink that would restore it.
func TestShrinkRecoversAnOverCapacityPool(t *testing.T) {
	ctx := context.Background()
	controller, store := admissionController(t)
	// 20 GiB applied, in a pool now configured for only 10 GiB.
	putRecord(t, store, "db", "data", appliedRecord("db/data", 20*config.GiB))

	set, err := controller.loadVolumeRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	services := []config.ServiceConfig{serviceWithVolume("db", 5*config.GiB)}
	if _, err := controller.applyExistingVolumeRecords(ctx, services, set, poolNodes(10*config.GiB)); err != nil {
		t.Fatal(err)
	}
	stored := set.Records["db/data"].Record
	if stored.rejectionStands() {
		t.Fatalf("a shrink cannot over-commit a pool and must be admitted, got %#v", stored)
	}
	if stored.DesiredSizeBytes != 5*config.GiB {
		t.Fatalf("expected the shrink to be accepted, got desired %d", stored.DesiredSizeBytes)
	}
}

// an unrecognized resize_state is tolerated for forward
// compatibility, but the admission path still rewrites the record — so an
// older controller destroys state owned by a newer resize protocol.
func TestUnknownResizeStateIsNotOverwrittenByAdmission(t *testing.T) {
	ctx := context.Background()
	controller, store := admissionController(t)
	record := appliedRecord("db/data", 10*config.GiB)
	record.ResizeState = VolumeResizeState("quiescing")
	record.UpdatedAt = time.Now().UTC()
	putRecord(t, store, "db", "data", record)

	set, err := controller.loadVolumeRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	services := []config.ServiceConfig{serviceWithVolume("db", 20*config.GiB)}
	if _, err := controller.applyExistingVolumeRecords(ctx, services, set, poolNodes(100*config.GiB)); err != nil {
		t.Fatal(err)
	}
	stored := set.Records["db/data"].Record
	if stored.ResizeState != VolumeResizeState("quiescing") {
		t.Fatalf("an older controller overwrote a newer protocol's state: %q", stored.ResizeState)
	}
	if stored.ResizeGeneration != 1 {
		t.Fatalf("an older controller advanced a newer protocol's generation to %d", stored.ResizeGeneration)
	}
	// The rendered configuration must follow the record, not the request.
	if got := services[0].Volumes[0].SizeBytes; got != 10*config.GiB {
		t.Fatalf("expected the record's size to render, got %d", got)
	}
}
