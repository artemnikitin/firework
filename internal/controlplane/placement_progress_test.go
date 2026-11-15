package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

func agePlacementNode(t *testing.T, store StateStore, c *Controller, node string, age time.Duration) {
	t.Helper()
	record, exists := readNode(t, store, c.cfg, node)
	if !exists {
		t.Fatal("missing node to age")
	}
	record.LastSeenAt = time.Now().UTC().Add(-age)
	if _, err := store.PutJSON(context.Background(), mustNodeRecordKey(c.cfg.State.Prefix, node), record); err != nil {
		t.Fatal(err)
	}
}

// Observations acknowledge configurations produced by actual controller passes.
func acknowledgePlacement(t *testing.T, store StateStore, c *Controller) {
	t.Helper()
	for _, nc := range currentPlacement(t, store).NodeConfigs {
		services := make([]statusmodel.ServiceStatus, 0, len(nc.Services))
		for _, service := range nc.Services {
			services = append(services, statusmodel.ServiceStatus{Name: service.Name, VMState: "running", Health: "not_configured"})
		}
		status := statusmodel.AgentStatus{
			SchemaVersion: statusmodel.SchemaVersion, NodeID: nc.Node, Phase: statusmodel.PhaseReady,
			ObservedAt: time.Now().UTC(), DesiredRevision: nc.DesiredRevision, PlacementRevision: nc.PlacementRevision,
			ObservedRevision: nc.RenderedRevision, AppliedRevision: nc.RenderedRevision,
			DesiredServices: len(services), ReadyServices: len(services), Services: services,
			ActualOnlyServices: []statusmodel.ServiceStatus{}, Conditions: trueBlockingConditions(time.Now().UTC()),
		}
		record, exists := readNode(t, store, c.cfg, nc.Node)
		if !exists {
			t.Fatal("missing placed node")
		}
		record.AgentStatus = &status
		record.LastSeenAt = time.Now().UTC()
		record.AgentStatusReceivedAt = record.LastSeenAt
		if _, err := store.PutJSON(context.Background(), mustNodeRecordKey(c.cfg.State.Prefix, nc.Node), record); err != nil {
			t.Fatal(err)
		}
	}
}

func movedOwnerFixture(t *testing.T, enabled bool) (*Controller, StateStore) {
	t.Helper()
	store := newBlobStateStore(newMemBlob())
	c := retirementTestController(t, store, enabled, !enabled)
	putDesired(t, store, "desired-1", testService("web"))
	putEmptyStateNode(t, store, "node-a", NodeStateReady, time.Now().UTC())
	c.runReconcile(context.Background())
	agePlacementNode(t, store, c, "node-a", time.Hour)
	putEmptyStateNode(t, store, "node-b", NodeStateReady, time.Now().UTC())
	c.runReconcile(context.Background())
	if len(currentPlacement(t, store).UnconfirmedPrevious) != 1 {
		t.Fatal("move did not retain the previous owner")
	}
	return c, store
}

func TestPreviousOwnerExpiryPrunesCachedPlacement(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "retirement off", true: "retirement on"}[enabled], func(t *testing.T) {
			c, store := movedOwnerFixture(t, enabled)
			acknowledgePlacement(t, store, c)
			before, err := NewVisibilityService(c.cfg, store).Revision(context.Background())
			if err != nil || before.Phase != "degraded" || before.ReasonCode != "previous_node_unconfirmed" {
				t.Fatalf("before expiry: %+v %v", before, err)
			}
			agePlacementNode(t, store, c, "node-a", 3*time.Hour)
			c.runReconcile(context.Background())
			if owners := currentPlacement(t, store).UnconfirmedPrevious; len(owners) != 0 {
				t.Fatalf("expired owners remain in durable placement: %+v", owners)
			}
			acknowledgePlacement(t, store, c)
			after, err := NewVisibilityService(c.cfg, store).Revision(context.Background())
			if err != nil || after.Phase != "converged" {
				t.Fatalf("after expiry: %+v %v", after, err)
			}
		})
	}
}

func TestRemovedServiceKeepsPreviousOwnerUntilExpiry(t *testing.T) {
	c, store := movedOwnerFixture(t, false)
	ctx := context.Background()
	putDesired(t, store, "desired-empty")
	c.runReconcile(ctx)
	acknowledgePlacement(t, store, c)
	c.runReconcile(ctx) // Resolve node-b's empty observation, retaining node-a.
	acknowledgePlacement(t, store, c)
	status, err := NewVisibilityService(c.cfg, store).Revision(ctx)
	if err != nil || status.Services.Desired != 0 || status.Phase != "degraded" || len(status.PreviousNodeUnconfirmedServices) != 1 || status.PreviousNodeUnconfirmedServices[0] != "web" {
		t.Fatalf("removed service lost unresolved execution: %+v %v", status, err)
	}
	agePlacementNode(t, store, c, "node-a", 3*time.Hour)
	c.runReconcile(ctx)
	acknowledgePlacement(t, store, c)
	status, err = NewVisibilityService(c.cfg, store).Revision(ctx)
	if err != nil || status.Phase != "converged" || len(currentPlacement(t, store).UnconfirmedPrevious) != 0 {
		t.Fatalf("removed service did not expire: %+v %v", status, err)
	}
}

func TestPerServiceAbsenceClearsOwnerWhileNodeKeepsLocalWork(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	c := retirementTestController(t, store, false, true)
	db := testService("db")
	db.Volumes = []config.VolumeConfig{{Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data", SizeBytes: config.GiB, BoundNode: "node-a", ResizeGeneration: 1}}
	putDesired(t, store, "desired-1", db, testService("web"))
	putEmptyStateNode(t, store, "node-a", NodeStateReady, time.Now().UTC())
	a, _ := readNode(t, store, c.cfg, "node-a")
	a.Storage.LocalCapacityBytes = 8 * config.GiB
	if _, err := store.PutJSON(ctx, mustNodeRecordKey(c.cfg.State.Prefix, "node-a"), a); err != nil {
		t.Fatal(err)
	}
	c.runReconcile(ctx)
	agePlacementNode(t, store, c, "node-a", time.Hour)
	putEmptyStateNode(t, store, "node-b", NodeStateReady, time.Now().UTC())
	c.runReconcile(ctx)
	agePlacementNode(t, store, c, "node-a", 0)
	c.runReconcile(ctx)
	acknowledgePlacement(t, store, c)
	c.runReconcile(ctx)
	for _, owner := range currentPlacement(t, store).UnconfirmedPrevious {
		if owner.Service == "web" && owner.Node == "node-a" {
			t.Fatal("node-a's db prevents web's positive absence proof")
		}
	}
	placement := placementServices(currentPlacement(t, store))
	if placement["db"].Node != "node-a" || placement["web"].Node != "node-b" {
		t.Fatalf("test did not exercise separate current owners: %+v", placement)
	}
}

func TestUnreadablePlacementPausesAndRetriesWithoutNewInputs(t *testing.T) {
	ctx := context.Background()
	store := &retirementStepFailureStore{StateStore: newBlobStateStore(newMemBlob()), operation: "read", key: placementCurrentKey(emptyStatePrefix)}
	c := retirementPublishingFixture(t, store)
	c.cfg.NodeRetirementEnabled = false
	before := currentPlacement(t, store)
	putDesired(t, store, "desired-next", testService("web"))
	store.armed = true
	store.skip = 1 // Volume acknowledgement reads the pointer before publication does.
	c.runReconcile(ctx)
	if !store.failed || currentPlacement(t, store).Revision != before.Revision {
		t.Fatal("unreadable placement did not preserve publication")
	}
	c.runReconcile(ctx)
	if next := currentPlacement(t, store); next.Revision == before.Revision || next.DesiredRevision != "desired-next" {
		t.Fatalf("read recovery did not resume publication: %+v", next)
	}
}
