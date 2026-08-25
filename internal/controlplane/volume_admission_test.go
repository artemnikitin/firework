package controlplane

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/scheduler"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

func admissionController(t *testing.T) (*Controller, StateStore) {
	t.Helper()
	store := newBlobStateStore(newMemBlob())
	return NewController(Config{State: StateConfig{Prefix: "cp/v1/"}}, store, slog.New(slog.NewTextHandler(io.Discard, nil))), store
}

func putRecord(t *testing.T, store StateStore, service, volume string, record VolumeRecord) {
	t.Helper()
	if _, err := store.PutJSON(context.Background(), mustVolumeRecordKey("cp/v1/", service, volume), record); err != nil {
		t.Fatal(err)
	}
}

func appliedRecord(logicalID string, size int64) VolumeRecord {
	now := time.Now().UTC()
	return VolumeRecord{
		LogicalID: logicalID, Type: config.VolumeTypeLocal, BoundNode: "node-1",
		DesiredSizeBytes: size, AppliedSizeBytes: size, ResizeGeneration: 1,
		ResizeState: VolumeResizeApplied, CreatedAt: now, UpdatedAt: now,
	}
}

func serviceWithVolume(name string, size int64) config.ServiceConfig {
	return config.ServiceConfig{Name: name, VCPUs: 1, MemoryMB: 512, Volumes: []config.VolumeConfig{{
		Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data", SizeBytes: size,
	}}}
}

func poolNodes(capacity int64) []scheduler.Node {
	return []scheduler.Node{{
		InstanceID: "node-1", CapacityVCPUs: 8, CapacityMemMB: 8192, LocalCapacityBytes: capacity,
	}}
}

// A size request the pool cannot satisfy is not adopted, and — the part that
// does the real work — the rendered configuration is clamped to the last
// accepted size. Declining the record write alone would leave the infeasible
// size in the scheduling copy and render it to the agent regardless.
func TestInfeasibleRaiseIsRefusedAndClampedToTheEffectiveSize(t *testing.T) {
	ctx := context.Background()
	controller, store := admissionController(t)
	putRecord(t, store, "db", "data", appliedRecord("db/data", 10*config.GiB))

	set, err := controller.loadVolumeRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	services := []config.ServiceConfig{serviceWithVolume("db", 90*config.GiB)}
	admission, err := controller.applyExistingVolumeRecords(ctx, services, set, poolNodes(20*config.GiB))
	if err != nil {
		t.Fatal(err)
	}
	if len(admission.Held) != 0 {
		t.Fatalf("a refused resize must not hold the service: %#v", admission.Held)
	}
	volume := services[0].Volumes[0]
	if volume.SizeBytes != 10*config.GiB || volume.ResizeGeneration != 1 {
		t.Fatalf("expected the effective size and generation to render, got %#v", volume)
	}
	stored := set.Records["db/data"].Record
	if stored.DesiredSizeBytes != 10*config.GiB {
		t.Fatalf("the refused size must not become the effective size: %#v", stored)
	}
	if !stored.rejectionStands() || stored.RequestedSizeBytes != 90*config.GiB ||
		stored.RejectedReason != scheduler.ReasonNodeStorageExhausted {
		t.Fatalf("rejection was not recorded durably: %#v", stored)
	}
	if stored.ResizeGeneration != 1 {
		t.Fatalf("a refused request must not mint a generation, got %d", stored.ResizeGeneration)
	}
}

// A standing rejection must be idempotent. Comparing the incoming request
// against DesiredSizeBytes — which now holds the effective size — makes the
// unchanged request look new on every tick and mints a generation forever.
func TestStandingRejectionProducesNoFurtherWritesOrGenerations(t *testing.T) {
	ctx := context.Background()
	controller, store := admissionController(t)
	putRecord(t, store, "db", "data", appliedRecord("db/data", 10*config.GiB))

	var firstRejectedAt time.Time
	var digests []string
	for tick := 0; tick < 3; tick++ {
		set, err := controller.loadVolumeRecords(ctx)
		if err != nil {
			t.Fatal(err)
		}
		services := []config.ServiceConfig{serviceWithVolume("db", 90*config.GiB)}
		if _, err := controller.applyExistingVolumeRecords(ctx, services, set, poolNodes(20*config.GiB)); err != nil {
			t.Fatal(err)
		}
		stored := set.Records["db/data"].Record
		if stored.ResizeGeneration != 1 {
			t.Fatalf("tick %d minted a generation for a standing rejection: %d", tick, stored.ResizeGeneration)
		}
		if tick == 0 {
			firstRejectedAt = stored.RejectedAt
		} else if !stored.RejectedAt.Equal(firstRejectedAt) {
			t.Fatalf("tick %d restamped RejectedAt: %v vs %v", tick, stored.RejectedAt, firstRejectedAt)
		}
		digests = append(digests, volumeRecordsDigest(set))
	}
	if digests[1] != digests[2] {
		t.Fatal("a steady rejected request must leave the records digest stable")
	}
}

// Reverting the request to a feasible size clears the rejection and mints
// exactly one generation. Reverting it to the effective size clears the
// rejection and mints none, because there is no resize to perform.
func TestRejectionRecovery(t *testing.T) {
	tests := []struct {
		name           string
		requested      int64
		wantGeneration int64
	}{
		{name: "feasible new size", requested: 15 * config.GiB, wantGeneration: 2},
		{name: "back to the effective size", requested: 10 * config.GiB, wantGeneration: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			controller, store := admissionController(t)
			putRecord(t, store, "db", "data", appliedRecord("db/data", 10*config.GiB))

			set, err := controller.loadVolumeRecords(ctx)
			if err != nil {
				t.Fatal(err)
			}
			refused := []config.ServiceConfig{serviceWithVolume("db", 90*config.GiB)}
			if _, err := controller.applyExistingVolumeRecords(ctx, refused, set, poolNodes(20*config.GiB)); err != nil {
				t.Fatal(err)
			}

			set, err = controller.loadVolumeRecords(ctx)
			if err != nil {
				t.Fatal(err)
			}
			corrected := []config.ServiceConfig{serviceWithVolume("db", test.requested)}
			if _, err := controller.applyExistingVolumeRecords(ctx, corrected, set, poolNodes(20*config.GiB)); err != nil {
				t.Fatal(err)
			}
			stored := set.Records["db/data"].Record
			if stored.rejectionStands() {
				t.Fatalf("expected the rejection to clear, got %#v", stored)
			}
			if stored.ResizeGeneration != test.wantGeneration {
				t.Fatalf("expected generation %d, got %d", test.wantGeneration, stored.ResizeGeneration)
			}
			if stored.DesiredSizeBytes != test.requested {
				t.Fatalf("expected effective size %d, got %d", test.requested, stored.DesiredSizeBytes)
			}
		})
	}
}

// Two raises that are each feasible on their own must not both be admitted
// when their combined reservation exceeds the pool, and the same one must win
// on every controller and after every leader change.
func TestBatchedRaisesAdmitOnlyWhatThePoolHolds(t *testing.T) {
	ctx := context.Background()
	for run := 0; run < 3; run++ {
		controller, store := admissionController(t)
		putRecord(t, store, "alpha", "data", appliedRecord("alpha/data", 10*config.GiB))
		putRecord(t, store, "beta", "data", appliedRecord("beta/data", 10*config.GiB))

		set, err := controller.loadVolumeRecords(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// Declared out of name order to prove the evaluation order is by name.
		services := []config.ServiceConfig{
			serviceWithVolume("beta", 30*config.GiB),
			serviceWithVolume("alpha", 30*config.GiB),
		}
		if _, err := controller.applyExistingVolumeRecords(ctx, services, set, poolNodes(45*config.GiB)); err != nil {
			t.Fatal(err)
		}
		alpha := set.Records["alpha/data"].Record
		beta := set.Records["beta/data"].Record
		if alpha.DesiredSizeBytes != 30*config.GiB {
			t.Fatalf("run %d: the first raise by name must be admitted, got %#v", run, alpha)
		}
		if beta.DesiredSizeBytes != 10*config.GiB || !beta.rejectionStands() {
			t.Fatalf("run %d: the second raise must be refused, got %#v", run, beta)
		}
	}
}

// A bound node that is not observable fails closed: the raise waits rather
// than being admitted against capacity nobody can verify, and the existing
// effective size keeps rendering so the service is not disturbed.
func TestRaiseOnAnAbsentNodeFailsClosed(t *testing.T) {
	ctx := context.Background()
	controller, store := admissionController(t)
	putRecord(t, store, "db", "data", appliedRecord("db/data", 10*config.GiB))

	set, err := controller.loadVolumeRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	services := []config.ServiceConfig{serviceWithVolume("db", 12*config.GiB)}
	if _, err := controller.applyExistingVolumeRecords(ctx, services, set, nil); err != nil {
		t.Fatal(err)
	}
	if got := services[0].Volumes[0].SizeBytes; got != 10*config.GiB {
		t.Fatalf("expected the effective size to keep rendering, got %d", got)
	}
	stored := set.Records["db/data"].Record
	if stored.RejectedReason != scheduler.ReasonStorageCapacityUnknown {
		t.Fatalf("expected an unknown-capacity refusal, got %#v", stored)
	}
}

// A capacity rejection is entirely control-plane-sourced: the agent is handed
// the clamped config and does not know one happened, so its observation
// carries none of the rejection fields. Whole-struct replacement in the merge
// would clear them on every cycle and make the rejection invisible in exactly
// the case it exists to make visible.
func TestCapacityRejectionSurvivesTheMergeAgainstAnUnawareAgent(t *testing.T) {
	base := []statusmodel.VolumeStatus{{
		LogicalID: "db/data", Type: "local", MountPath: "/data", BoundNode: "node-1",
		DesiredSizeBytes: 10 * config.GiB, AppliedSizeBytes: 10 * config.GiB, ResizeGeneration: 1,
		State: "applied", RequestedSizeBytes: 90 * config.GiB, Rejected: true,
		RejectedReason: scheduler.ReasonNodeStorageExhausted,
	}}
	observed := []statusmodel.VolumeStatus{{
		LogicalID: "db/data", Type: "local", MountPath: "/data", BoundNode: "node-1",
		DesiredSizeBytes: 10 * config.GiB, ResizeGeneration: 1, State: "prepared",
	}}

	merged := mergeVolumeStatuses(base, observed)
	if len(merged) != 1 {
		t.Fatalf("unexpected merge result: %#v", merged)
	}
	got := merged[0]
	if got.RequestedSizeBytes != 90*config.GiB || !got.Rejected || got.RejectedReason != scheduler.ReasonNodeStorageExhausted {
		t.Fatalf("the rejection was cleared by the merge: %#v", got)
	}
	// The agent reported no applied size because its VM is not running; the
	// durable value must not be erased.
	if got.AppliedSizeBytes != 10*config.GiB {
		t.Fatalf("the durable applied size was erased: %#v", got)
	}
	// The agent's own observation still wins where it has one.
	if got.State != "prepared" {
		t.Fatalf("the agent observation must still be authoritative for state: %#v", got)
	}
}
