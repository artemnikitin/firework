package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/objectstorage"
)

func retirementPublishingFixture(t *testing.T, store StateStore) *Controller {
	t.Helper()
	c := retirementTestController(t, store, true, false)
	putDesired(t, store, "desired-1", testService("web"))
	putEmptyStateNode(t, store, "node-live", NodeStateReady, time.Now().UTC())
	putRetirementCandidate(t, store, c.cfg, NodeStateDown, 7, time.Now().UTC().Add(-3*time.Hour))
	putNodeConfig(t, store, "node-1", nodeConfigYAML(t, "node-1", "web"))
	c.runReconcile(context.Background())
	requireExplicitEmpty(t, store, "node-1", currentPlacement(t, store))
	return c
}

func TestRetiredNodeClearAfterLaterPublishingPasses(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	c := retirementPublishingFixture(t, store)
	c.runReconcile(ctx)
	if _, exists := readNode(t, store, c.cfg, "node-1"); exists {
		t.Fatal("node was not retired")
	}
	for _, rev := range []string{"desired-2", "desired-3"} {
		putDesired(t, store, rev, testService("web"))
		c.runReconcile(ctx)
	}
	request := NodeTombstoneRecoveryRequest{
		NodeID: "node-1", ExpectedGeneration: 7, Reason: "restore retained host",
		ExternalFenceReference: "fence-1", RetainedPoolReference: "pool-1", ConfirmedHostFenced: true,
	}
	if _, err := ClearNodeTombstone(ctx, store, c.cfg.State.Prefix, request); err != nil {
		t.Fatalf("clear after later publications: %v", err)
	}
	server, _ := newTestRegistry(t)
	server.store = store
	if code, reason := registryCall(t, server, "node-1", "/v1/nodes/register", register("node-1", 8)); code != 200 {
		t.Fatalf("registration after clear: %d %s", code, reason)
	}
}

type retirementStepFailureStore struct {
	StateStore
	operation, key string
	armed, failed  bool
	skip           int
}

func (s *retirementStepFailureStore) fail(operation, key string) bool {
	if s.armed && !s.failed && s.operation == operation && s.key == key {
		if s.skip > 0 {
			s.skip--
			return false
		}
		s.failed = true
		return true
	}
	return false
}

func (s *retirementStepFailureStore) GetJSON(ctx context.Context, key string, out any) (objectstorage.WriteToken, bool, error) {
	if s.fail("read", key) {
		return "", false, errors.New("injected retirement read failure")
	}
	return s.StateStore.GetJSON(ctx, key, out)
}

func (s *retirementStepFailureStore) PutJSONIfAbsent(ctx context.Context, key string, value any) (bool, objectstorage.WriteToken, error) {
	if s.fail("create", key) {
		return false, "", errors.New("injected retirement create failure")
	}
	return s.StateStore.PutJSONIfAbsent(ctx, key, value)
}

func (s *retirementStepFailureStore) PutJSONIfMatch(ctx context.Context, key string, token objectstorage.WriteToken, value any) (bool, objectstorage.WriteToken, error) {
	if s.fail("update", key) {
		return false, "", errors.New("injected retirement update failure")
	}
	return s.StateStore.PutJSONIfMatch(ctx, key, token, value)
}

func (s *retirementStepFailureStore) DeleteIfMatch(ctx context.Context, key string, token objectstorage.WriteToken) (bool, error) {
	if s.fail("delete", key) {
		return false, errors.New("injected retirement delete failure")
	}
	return s.StateStore.DeleteIfMatch(ctx, key, token)
}

func TestRetirementSingleFaultRecoversThroughPublishingPasses(t *testing.T) {
	for _, step := range []struct{ name, operation, key string }{
		{"pending mark", "update", mustNodeRecordKey(emptyStatePrefix, "node-1")},
		{"tombstone", "create", mustNodeTombstoneKey(emptyStatePrefix, "node-1")},
		{"index", "update", nodeTombstoneIndexKey(emptyStatePrefix)},
		{"before delete", "read", mustNodeRecordKey(emptyStatePrefix, "node-1")},
		{"delete", "delete", mustNodeRecordKey(emptyStatePrefix, "node-1")},
	} {
		t.Run(step.name, func(t *testing.T) {
			store := &retirementStepFailureStore{StateStore: newBlobStateStore(newMemBlob()), operation: step.operation, key: step.key}
			c := retirementPublishingFixture(t, store)
			store.armed = true
			for i := 0; i < 6; i++ {
				c.runReconcile(context.Background())
			}
			if !store.failed {
				t.Fatal("fault was not exercised")
			}
			if record, exists := readNode(t, store, c.cfg, "node-1"); exists {
				t.Fatalf("retirement did not finish after one failed %s: %+v", step.name, record)
			}
		})
	}
}

func TestWinningRegistrationRetainsVolumeWithoutFalseOrphanState(t *testing.T) {
	ctx := context.Background()
	base := newBlobStateStore(newMemBlob())
	server, _ := newTestRegistry(t)
	store := &retirementRegistrationRaceStore{StateStore: base, server: server, t: t}
	server.store = store
	c := retirementPublishingFixture(t, store)
	store.key = mustNodeTombstoneKey(c.cfg.State.Prefix, "node-1")
	_, err := store.PutJSON(ctx, mustVolumeRecordKey(c.cfg.State.Prefix, "removed", "data"), VolumeRecord{
		LogicalID: "removed/data", Type: config.VolumeTypeLocal, BoundNode: "node-1",
		DesiredSizeBytes: config.GiB, AppliedSizeBytes: config.GiB, ResizeGeneration: 1, ResizeState: VolumeResizeApplied,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.runReconcile(ctx)
	if rec, exists := readNode(t, store, c.cfg, "node-1"); !exists || rec.Generation != 8 {
		t.Fatalf("registration did not win: %+v", rec)
	}
	volume, exists, err := NewVisibilityService(c.cfg, store).Volume(ctx, "removed", "data")
	if err != nil || !exists || volume.State != "retained" {
		t.Fatalf("winning incarnation's volume: %+v exists=%v err=%v", volume, exists, err)
	}
}

func TestRetirementFinishesAfterControllerRestart(t *testing.T) {
	ctx := context.Background()
	store := &retirementStepFailureStore{StateStore: newBlobStateStore(newMemBlob()), operation: "delete", key: mustNodeRecordKey(emptyStatePrefix, "node-1")}
	c := retirementPublishingFixture(t, store)
	store.armed = true
	c.runReconcile(ctx)
	if !store.failed {
		t.Fatal("delete failure was not exercised")
	}
	if _, err := store.PutJSON(ctx, controllerLockKey(c.cfg.State.Prefix), LeaderLock{
		HolderID: c.id, LeaderEpoch: c.epoch, LeaseExpiresAt: time.Now().UTC().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	restarted := NewController(c.cfg, store, c.logger)
	if err := restarted.renewLeadership(ctx); err != nil || !restarted.leader {
		t.Fatalf("restart leadership: %v", err)
	}
	restarted.runReconcile(ctx)
	if record, exists := readNode(t, store, c.cfg, "node-1"); exists {
		t.Fatalf("restart stranded retirement: %+v", record)
	}
}
