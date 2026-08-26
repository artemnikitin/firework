package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/scheduler"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

// when the previous placement cannot be read, a held service has no
// recoverable node — it would be classified as never-placed, left pending, and
// dropped from the rendered node configs, which the agent turns into a delete.
// Publishing anything in that state evicts a healthy workload over a transient
// read failure.
func TestUnreadablePlacementHoldsPublicationWhenServicesAreHeld(t *testing.T) {
	held := map[string]string{"running": scheduler.ReasonVolumeRecordInvalid}

	tests := []struct {
		name           string
		placementFound bool
		held           map[string]string
		wantHold       bool
	}{
		{name: "unrecoverable placement with a held service", held: held, wantHold: true},
		{name: "unrecoverable placement with nothing held", wantHold: false},
		{name: "recovered placement with a held service", placementFound: true, held: held, wantHold: false},
		{name: "recovered placement with nothing held", placementFound: true, wantHold: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := heldPlacementUnrecoverable(test.placementFound, test.held); got != test.wantHold {
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

	placement, found, err := controller.readExistingPlacement(ctx)
	if err == nil {
		t.Fatalf("expected a corrupt placement revision to be an error, got placement %#v", placement)
	}
	if found {
		t.Fatal("a failed read must not report the placement as found")
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

// The agent stops reporting a refusal once the rendered config carries the
// effective size, because those bytes are ambiguous to it. The record is not
// ambiguous, so the revision status has to carry that half — otherwise a
// cluster running a size nobody asked for reports plain convergence.
func TestStandingRecordRefusalDegradesTheRevision(t *testing.T) {
	refused := VolumeRecord{
		LogicalID: "db/data", Type: config.VolumeTypeLocal, BoundNode: "node-1",
		DesiredSizeBytes: 10 * config.GiB, AppliedSizeBytes: 10 * config.GiB,
		RequestedSizeBytes: 2 * config.GiB, ResizeGeneration: 2,
		ResizeState: VolumeResizeRejected, RejectedReason: "shrink_below_minimum",
	}
	// The desired revision still declares the volume — a refusal only matters
	// while something is asking for the size.
	snapshot := visibilitySnapshot{
		desired: DesiredRevision{
			Revision: "rev-1",
			Services: []config.ServiceConfig{serviceWithVolume("db", 2*config.GiB)},
		},
		placementCurrent: true,
		placement:        PlacementRevision{Revision: "placement-1"},
		volumeByID:       map[string]VolumeRecord{"db/data": refused},
	}

	status := snapshot.revisionStatus()
	if status.Phase != "degraded" {
		t.Fatalf("a standing refusal must not read as convergence, got %q", status.Phase)
	}
	if status.ReasonCode != "volume_size_rejected" {
		t.Fatalf("unexpected reason: %q", status.ReasonCode)
	}
	if !strings.Contains(status.Message, "db/data") {
		t.Fatalf("expected the refused volume to be named, got %q", status.Message)
	}

	// Once the refusal is cleared the revision converges again.
	cleared := refused
	cleared.clearRejection()
	cleared.ResizeState = VolumeResizeApplied
	snapshot.volumeByID = map[string]VolumeRecord{"db/data": cleared}
	if got := snapshot.revisionStatus(); got.Phase == "degraded" {
		t.Fatalf("a cleared refusal must stop degrading the revision: %#v", got)
	}
}

// A missing placement object is not a read error, so a guard keyed on the
// error alone never fires and a held running service is dropped.
func TestMissingPlacementObjectCountsAsUnrecoverable(t *testing.T) {
	ctx := context.Background()
	controller, store := admissionController(t)

	// The pointer exists and names a revision whose object is gone.
	if _, err := store.PutJSON(ctx, placementCurrentKey("cp/v1/"), RevisionPointer{Revision: "placement-1"}); err != nil {
		t.Fatal(err)
	}

	placement, found, err := controller.readExistingPlacement(ctx)
	if err != nil {
		t.Fatalf("a missing object is not a read error, got %v", err)
	}
	if placement != nil {
		t.Fatalf("expected no placement, got %#v", placement)
	}
	// The guard keys on found, not on err: a pointer naming a revision whose
	// object is gone is not "nothing has been placed yet".
	if found {
		t.Fatal("a missing placement object must not report as found")
	}
	if !heldPlacementUnrecoverable(found, map[string]string{"running": "volume_record_invalid"}) {
		t.Fatal("a missing placement object must count as unrecoverable for a held service")
	}
}

// An absent resize_state is an empty string, which is malformed rather than a
// state owned by a newer protocol.
func TestAbsentResizeStateIsQuarantinedAsMalformed(t *testing.T) {
	ctx := context.Background()
	controller, store := admissionController(t)
	record := appliedRecord("db/data", 10*config.GiB)
	record.ResizeState = ""
	putRecord(t, store, "db", "data", record)

	set, err := controller.loadVolumeRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, quarantined := set.Quarantined["db/data"]; !quarantined {
		t.Fatalf("an absent resize_state must be quarantined as malformed, got record %#v", set.Records["db/data"].Record)
	}
}

// The withdrawn-request shape is not direct-Git-only. In controller-managed mode, reverting the
// GitOps size: to the effective size clears the *record's* rejection — but the
// controller still renders that same (effective size, refused generation)
// shape, so the agent-side refusal survives on the identical bytes.
func TestControllerModeAlsoRendersTheWithdrawnShape(t *testing.T) {
	ctx := context.Background()
	controller, store := admissionController(t)
	putRecord(t, store, "db", "data", VolumeRecord{
		LogicalID: "db/data", Type: config.VolumeTypeLocal, BoundNode: "node-1",
		DesiredSizeBytes: 10 * config.GiB, AppliedSizeBytes: 10 * config.GiB,
		RequestedSizeBytes: 2 * config.GiB, ResizeGeneration: 2,
		ResizeState: VolumeResizeRejected, RejectedReason: "shrink_below_minimum",
	})

	set, err := controller.loadVolumeRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The operator reverts size: to the effective size.
	services := []config.ServiceConfig{serviceWithVolume("db", 10*config.GiB)}
	if _, err := controller.applyExistingVolumeRecords(ctx, services, set, poolNodes(100*config.GiB)); err != nil {
		t.Fatal(err)
	}
	if set.Records["db/data"].Record.rejectionStands() {
		t.Fatal("reverting to the effective size must clear the record's rejection")
	}
	// And the rendered shape is exactly the one the agent must not keep
	// refusing: the effective size at the refused generation.
	got := services[0].Volumes[0]
	if got.SizeBytes != 10*config.GiB || got.ResizeGeneration != 2 {
		t.Fatalf("expected the effective size at the refused generation, got (%d, %d)",
			got.SizeBytes, got.ResizeGeneration)
	}
}

// Retained records outlive their service by design. A refusal on a record
// whose service is no longer desired must not degrade every later revision.
func TestRefusalOnADeletedServiceDoesNotDegradeTheRevision(t *testing.T) {
	refused := VolumeRecord{
		LogicalID: "gone/data", Type: config.VolumeTypeLocal, BoundNode: "node-1",
		DesiredSizeBytes: 10 * config.GiB, AppliedSizeBytes: 10 * config.GiB,
		RequestedSizeBytes: 2 * config.GiB, ResizeGeneration: 2,
		ResizeState: VolumeResizeRejected, RejectedReason: "shrink_below_minimum",
	}
	// An empty desired revision: the service was deleted, its record retained.
	snapshot := visibilitySnapshot{
		desired:          DesiredRevision{Revision: "rev-2"},
		placementCurrent: true,
		placement:        PlacementRevision{Revision: "placement-2"},
		volumeByID:       map[string]VolumeRecord{"gone/data": refused},
	}
	if got := snapshot.revisionStatus(); got.Phase == "degraded" {
		t.Fatalf("a refusal for a service no longer desired must not degrade the revision: %#v", got)
	}
}

// Withdrawing a request must leave the record self-consistent. Clearing the
// rejection fields while ResizeState stays "rejected" makes status report
// state: rejected with rejected: false, and nothing later repairs it.
func TestWithdrawnRefusalLeavesNoContradictoryState(t *testing.T) {
	ctx := context.Background()
	controller, store := admissionController(t)
	putRecord(t, store, "db", "data", VolumeRecord{
		LogicalID: "db/data", Type: config.VolumeTypeLocal, BoundNode: "node-1",
		DesiredSizeBytes: 10 * config.GiB, AppliedSizeBytes: 10 * config.GiB,
		RequestedSizeBytes: 2 * config.GiB, ResizeGeneration: 2,
		ResizeState: VolumeResizeRejected, RejectedReason: "shrink_below_minimum",
		LastError: "below the safe minimum",
	})

	set, err := controller.loadVolumeRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	services := []config.ServiceConfig{serviceWithVolume("db", 10*config.GiB)}
	if _, err := controller.applyExistingVolumeRecords(ctx, services, set, poolNodes(100*config.GiB)); err != nil {
		t.Fatal(err)
	}
	got := set.Records["db/data"].Record
	if got.rejectionStands() {
		t.Fatalf("precondition: the rejection fields must clear, got %#v", got)
	}
	if got.ResizeState == VolumeResizeRejected {
		t.Fatalf("state stayed rejected while the rejection was cleared: %#v", got)
	}
	if got.LastError != "" {
		t.Fatalf("the refusal message outlived the refusal: %q", got.LastError)
	}
}

// A stale heartbeat at the same generation must not reopen a refusal the
// desired state has already withdrawn — that produces two durable writes per
// tick, and a crash between them leaves the degraded state behind.
func TestStaleRejectionDoesNotReopenAWithdrawnRefusal(t *testing.T) {
	ctx := context.Background()
	controller, store := admissionController(t)
	recordKey := mustVolumeRecordKey("cp/v1/", "db", "data")
	// The refusal was withdrawn: fields cleared, state settled back to applied.
	putRecord(t, store, "db", "data", VolumeRecord{
		LogicalID: "db/data", Type: config.VolumeTypeLocal, BoundNode: "node-1",
		DesiredSizeBytes: 10 * config.GiB, AppliedSizeBytes: 10 * config.GiB,
		ResizeGeneration: 2, ResizeState: VolumeResizeApplied,
	})

	// An agent heartbeat still carrying the old refusal at the same generation.
	nodeKey, err := nodeRecordKey("cp/v1/", "node-1")
	if err != nil {
		t.Fatal(err)
	}
	node := NodeRecord{NodeID: "node-1", AgentStatus: &statusmodel.AgentStatus{Services: []statusmodel.ServiceStatus{{
		Name: "db", Volumes: []statusmodel.VolumeStatus{{
			LogicalID: "db/data", Type: "local", BoundNode: "node-1",
			AppliedSizeBytes: 10 * config.GiB, RequestedSizeBytes: 2 * config.GiB,
			ResizeGeneration: 2, State: "rejected", Rejected: true,
			RejectedReason: "shrink_below_minimum",
		}},
	}}}}
	if _, err := store.PutJSON(ctx, nodeKey, node); err != nil {
		t.Fatal(err)
	}

	if err := controller.acknowledgeVolumeRecords(ctx); err != nil {
		t.Fatal(err)
	}
	var got VolumeRecord
	if _, exists, err := store.GetJSON(ctx, recordKey, &got); err != nil || !exists {
		t.Fatalf("read record: %v", err)
	}
	if got.rejectionStands() || got.ResizeState == VolumeResizeRejected {
		t.Fatalf("a stale heartbeat reopened a withdrawn refusal: %#v", got)
	}
}

// Zero volumes is valid prior state, not a missing snapshot. Substituting the
// desired configuration there reintroduces exactly the unvalidated volume
// config the hold exists to gate.
func TestZeroVolumePriorSnapshotIsNotAMissingOne(t *testing.T) {
	// The prior render legitimately had no volumes.
	prior := config.ServiceConfig{Name: "svc", VCPUs: 1, MemoryMB: 512}
	// The desired revision now adds one, which is what is being gated.
	desired := []config.ServiceConfig{serviceWithVolume("svc", 90*config.GiB)}
	admission := volumeAdmission{Held: map[string]string{"svc": scheduler.ReasonVolumeRecordInvalid}}
	placement := map[string]renderedPlacement{"svc": {Node: "node-1", Service: prior}}

	_, held, _ := splitHeldServices(desired, admission, placement)

	rendered := held["node-1"]
	if len(rendered) != 1 {
		t.Fatalf("the held service must still be rendered: %#v", held)
	}
	if len(rendered[0].Volumes) != 0 {
		t.Fatalf("the unvalidated desired volume config was rendered for a held service: %#v", rendered[0].Volumes)
	}
}
