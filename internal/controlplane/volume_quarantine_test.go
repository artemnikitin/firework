package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/scheduler"
)

func putRaw(t *testing.T, store StateStore, service, volume string, body []byte) {
	t.Helper()
	if _, err := store.PutRaw(context.Background(), mustVolumeRecordKey("cp/v1/", service, volume), body, "application/json"); err != nil {
		t.Fatal(err)
	}
}

// One malformed record used to stop cluster-wide scheduling. It must now
// isolate to its own key while every other record stays usable.
func TestOneBadRecordDoesNotStopTheOthers(t *testing.T) {
	ctx := context.Background()
	controller, store := admissionController(t)
	putRecord(t, store, "good", "data", appliedRecord("good/data", 10*config.GiB))
	putRaw(t, store, "bad", "data", []byte("{not json"))

	set, err := controller.loadVolumeRecords(ctx)
	if err != nil {
		t.Fatalf("a malformed record must not fail the load: %v", err)
	}
	if _, ok := set.Records["good/data"]; !ok {
		t.Fatalf("the valid record was lost: %#v", set.Records)
	}
	if _, ok := set.Quarantined["bad/data"]; !ok {
		t.Fatalf("the malformed record was not quarantined: %#v", set.Quarantined)
	}
}

// Every tier must hold onto whatever capacity it can prove. Dropping a bad
// record silently releases its reservation and turns a hard failure into
// over-commit, which is the failure mode the admission check exists for.
func TestQuarantineTiersChargeWhatTheyCanProve(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name              string
		record            VolumeRecord
		wantTier          int
		wantReserved      int64
		wantNodeUnknown   bool
		wantLocalClassNew bool
	}{
		{
			name: "both sizes and a binding parse",
			record: VolumeRecord{
				LogicalID: "svc/data", Type: config.VolumeTypeLocal, BoundNode: "node-1",
				DesiredSizeBytes: 8 * config.GiB, AppliedSizeBytes: 10 * config.GiB,
				ResizeGeneration: 1, ResizeState: VolumeResizeApplied, UpdatedAt: now,
			},
			wantTier: quarantineTierExact, wantReserved: 10 * config.GiB,
		},
		{
			name: "binding parses but a size does not",
			record: VolumeRecord{
				LogicalID: "svc/data", Type: config.VolumeTypeLocal, BoundNode: "node-1",
				DesiredSizeBytes: 8 * config.GiB, ResizeGeneration: 0, UpdatedAt: now,
			},
			wantTier: quarantineTierPartial, wantReserved: 8 * config.GiB, wantNodeUnknown: true,
		},
		{
			name: "type parses but no binding does",
			record: VolumeRecord{
				LogicalID: "svc/data", Type: config.VolumeTypeLocal,
				DesiredSizeBytes: 8 * config.GiB, AppliedSizeBytes: 8 * config.GiB,
				ResizeGeneration: 1, UpdatedAt: now,
			},
			wantTier: quarantineTierUnattributable, wantLocalClassNew: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			controller, store := admissionController(t)
			putRecord(t, store, "svc", "data", test.record)

			set, err := controller.loadVolumeRecords(ctx)
			if err != nil {
				t.Fatal(err)
			}
			quarantine, ok := set.Quarantined["svc/data"]
			if !ok {
				t.Fatalf("expected a quarantine, got records %#v", set.Records)
			}
			if quarantine.Tier != test.wantTier {
				t.Fatalf("tier = %d, want %d (%#v)", quarantine.Tier, test.wantTier, quarantine)
			}
			if quarantine.ReservedBytes != test.wantReserved {
				t.Fatalf("reserved = %d, want %d", quarantine.ReservedBytes, test.wantReserved)
			}
			reservations := storageReservations(set)
			if reservations.LocalByNode["node-1"] != test.wantReserved {
				t.Fatalf("charged %d to the node, want %d", reservations.LocalByNode["node-1"], test.wantReserved)
			}
			if reservations.LocalUnknownByNode["node-1"] != test.wantNodeUnknown {
				t.Fatalf("node unknown flag = %v, want %v", reservations.LocalUnknownByNode["node-1"], test.wantNodeUnknown)
			}
			if reservations.LocalClassUnknown != test.wantLocalClassNew {
				t.Fatalf("class unknown flag = %v, want %v", reservations.LocalClassUnknown, test.wantLocalClassNew)
			}
		})
	}
}

// An unreadable object gives no class, because the key encodes only service
// and volume names. The block therefore covers both storage classes.
func TestUnreadableRecordBlocksBothStorageClasses(t *testing.T) {
	ctx := context.Background()
	controller, store := admissionController(t)
	putRaw(t, store, "svc", "data", []byte("{not json"))

	set, err := controller.loadVolumeRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reservations := storageReservations(set)
	if !reservations.LocalClassUnknown || !reservations.SharedClassUnknown {
		t.Fatalf("expected both classes blocked, got %#v", reservations)
	}
}

// A record written by a newer control plane must not brick scheduling on an
// older one during a rollback.
func TestUnknownResizeStateIsToleratedNotQuarantined(t *testing.T) {
	ctx := context.Background()
	controller, store := admissionController(t)
	record := appliedRecord("db/data", 10*config.GiB)
	record.ResizeState = VolumeResizeState("quiescing")
	putRecord(t, store, "db", "data", record)

	set, err := controller.loadVolumeRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, quarantined := set.Quarantined["db/data"]; quarantined {
		t.Fatalf("an unknown resize state must be carried through, not quarantined: %#v", set.Quarantined)
	}
	if got := set.Records["db/data"].Record.ResizeState; got != VolumeResizeState("quiescing") {
		t.Fatalf("the unknown state was not carried through: %q", got)
	}
}

// A quarantined outcome must move the scheduling signature. Otherwise a
// partial repair, a changed binding, or the full repair an operator is waiting
// on all leave the cached signature unchanged and suppress the reconcile that
// would act on them.
func TestQuarantinedOutcomesEnterTheSchedulingDigest(t *testing.T) {
	ctx := context.Background()
	controller, store := admissionController(t)

	digest := func() string {
		set, err := controller.loadVolumeRecords(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return volumeRecordsDigest(set)
	}

	// Tier 3: unreadable.
	putRaw(t, store, "db", "data", []byte("{not json"))
	tier3 := digest()

	// Tier 2: partially repaired — a binding now parses.
	partial := VolumeRecord{
		LogicalID: "db/data", Type: config.VolumeTypeLocal, BoundNode: "node-1",
		DesiredSizeBytes: 8 * config.GiB, ResizeGeneration: 0,
	}
	putRecord(t, store, "db", "data", partial)
	tier2 := digest()
	if tier2 == tier3 {
		t.Fatal("a tier transition must change the records digest")
	}

	// A changed binding moves which node is blocked.
	partial.BoundNode = "node-2"
	putRecord(t, store, "db", "data", partial)
	rebound := digest()
	if rebound == tier2 {
		t.Fatal("a changed binding on a quarantined record must change the digest")
	}

	// An unchanged invalid record must leave it stable.
	if digest() != rebound {
		t.Fatal("an unchanged quarantined record must leave the digest stable")
	}

	// The full repair.
	putRecord(t, store, "db", "data", appliedRecord("db/data", 10*config.GiB))
	if digest() == rebound {
		t.Fatal("repairing a record must change the digest")
	}
}

// Blocking an owner must not evict it. A service that is already placed keeps
// running at its last effective configuration; one that was never placed is
// withheld, because there is nothing running to disturb.
func TestBlockedOwnersAreHeldOrPendingButNeverDropped(t *testing.T) {
	held := config.ServiceConfig{Name: "running", VCPUs: 1, MemoryMB: 512, Volumes: []config.VolumeConfig{{
		Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data",
		SizeBytes: 10 * config.GiB, BoundNode: "node-1", ResizeGeneration: 3,
	}}}
	desired := []config.ServiceConfig{
		// The desired revision asks for a size the unreadable record cannot gate.
		{Name: "running", VCPUs: 1, MemoryMB: 512, Volumes: []config.VolumeConfig{{
			Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data", SizeBytes: 90 * config.GiB,
		}}},
		serviceWithVolume("fresh", 10*config.GiB),
	}
	admission := volumeAdmission{Held: map[string]string{
		"running": scheduler.ReasonVolumeRecordInvalid,
		"fresh":   scheduler.ReasonVolumeRecordInvalid,
	}}
	placement := map[string]renderedPlacement{"running": {Node: "node-1", Service: held}}

	schedulable, heldByNode, pending := splitHeldServices(desired, admission, placement)

	if len(schedulable) != 0 {
		t.Fatalf("both services are blocked, got %#v", schedulable)
	}
	rendered := heldByNode["node-1"]
	if len(rendered) != 1 || rendered[0].Name != "running" {
		t.Fatalf("the placed service must keep being rendered, got %#v", heldByNode)
	}
	if got := rendered[0].Volumes[0].SizeBytes; got != 10*config.GiB {
		t.Fatalf("the held service must re-render its last effective size, got %d", got)
	}
	if len(pending) != 1 || pending[0].Service != "fresh" || pending[0].ReasonCode != scheduler.ReasonVolumeRecordInvalid {
		t.Fatalf("the unplaced service must be pending, got %#v", pending)
	}
}

// Re-rendering a held service outside the scheduler must not let the scheduler
// hand its compute to something else.
func TestHeldServicesKeepTheirComputeReserved(t *testing.T) {
	nodes := []scheduler.Node{{InstanceID: "node-1", CapacityVCPUs: 4, CapacityMemMB: 4096}}
	held := map[string][]config.ServiceConfig{"node-1": {{Name: "running", VCPUs: 3, MemoryMB: 3072}}}

	adjusted := reserveHeldCapacity(nodes, held)
	if adjusted[0].CapacityVCPUs != 1 || adjusted[0].CapacityMemMB != 1024 {
		t.Fatalf("held compute was not reserved: %#v", adjusted[0])
	}
	if nodes[0].CapacityVCPUs != 4 {
		t.Fatal("reserveHeldCapacity must not mutate its input")
	}
}
