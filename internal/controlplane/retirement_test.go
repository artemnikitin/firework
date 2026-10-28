package controlplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/objectstorage"
	"gopkg.in/yaml.v3"
)

type retirementDeleteFailureStore struct {
	StateStore
	fail bool
}

func (s *retirementDeleteFailureStore) DeleteIfMatch(ctx context.Context, key string, token objectstorage.WriteToken) (bool, error) {
	if s.fail {
		s.fail = false
		return false, errors.New("injected delete failure")
	}
	return s.StateStore.DeleteIfMatch(ctx, key, token)
}

type retirementRegistrationRaceStore struct {
	StateStore
	server *RegistryServer
	t      *testing.T
	key    string
	done   bool
}

func (s *retirementRegistrationRaceStore) PutJSONIfAbsent(ctx context.Context, key string, value any) (bool, objectstorage.WriteToken, error) {
	if key == s.key && !s.done {
		s.done = true
		code, errCode := registryCall(s.t, s.server, "node-1", "/v1/nodes/register", register("node-1", 8))
		if code != http.StatusOK || errCode != "" {
			s.t.Fatalf("newer registration during retirement = %d %q", code, errCode)
		}
	}
	return s.StateStore.PutJSONIfAbsent(ctx, key, value)
}

func retirementTestController(t *testing.T, store StateStore, enabled, dryRun bool) *Controller {
	t.Helper()
	cfg := validConfigForRole(RoleController)
	cfg.NodeDownRetention = time.Hour
	cfg.NodeStaleRetention = 2 * time.Hour
	cfg.NodeRetirementEnabled = enabled
	cfg.NodeRetirementDryRun = dryRun
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	controller := NewController(cfg, store, logger)
	controller.leader = true
	controller.epoch = 1
	if _, err := store.PutJSON(context.Background(), controllerLockKey(cfg.State.Prefix), LeaderLock{
		HolderID: controller.id, LeaderEpoch: 1, LeaseExpiresAt: time.Now().UTC().Add(time.Hour), RenewedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	return controller
}

func putRetirementCandidate(t *testing.T, store StateStore, cfg Config, state NodeState, generation int64, at time.Time) {
	t.Helper()
	_, err := store.PutJSON(context.Background(), mustNodeRecordKey(cfg.State.Prefix, "node-1"), NodeRecord{
		NodeID: "node-1", Generation: generation, State: state, LastSeenAt: at,
		UpdatedAt: at, RegisteredAt: at, StateChangedAt: at, DownAt: at,
		Capacity: Resources{VCPUs: 2, MemoryMB: 512},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func putRetirementEmptyConfig(t *testing.T, store StateStore, prefix, nodeID, revision string) {
	t.Helper()
	nc := config.NodeConfig{
		Node: nodeID, Services: []config.ServiceConfig{},
		DesiredRevision: "desired-1", PlacementRevision: "placement-1", RenderedRevision: revision,
	}
	data, err := yaml.Marshal(nc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutRaw(context.Background(), legacyNodeConfigKey(prefix, nodeID), data, "application/x-yaml"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutRaw(context.Background(), renderedNodeKey(prefix, revision, nodeID), data, "application/x-yaml"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(context.Background(), renderedCurrentKey(prefix), RevisionPointer{Revision: revision, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
}

func TestRetirementEligibilityUsesExplicitLifecycleAndRetentionFallback(t *testing.T) {
	cfg := DefaultConfig()
	cfg.NodeStaleTTL = time.Minute
	cfg.NodeDownRetention = time.Hour
	cfg.NodeStaleRetention = 2 * time.Hour
	now := time.Now().UTC()
	old := now.Add(-3 * time.Hour)
	tests := []struct {
		name   string
		record NodeRecord
		want   string
		ok     bool
	}{
		{name: "explicit down", record: NodeRecord{State: NodeStateDown, DownAt: old}, want: "explicit_down", ok: true},
		{name: "legacy down state changed", record: NodeRecord{State: NodeStateDown, StateChangedAt: old}, want: "explicit_down", ok: true},
		{name: "legacy down updated", record: NodeRecord{State: NodeStateDown, UpdatedAt: old}, want: "explicit_down", ok: true},
		{name: "stale ready", record: NodeRecord{State: NodeStateReady, LastSeenAt: old}, want: "stale_timeout", ok: true},
		{name: "stale draining", record: NodeRecord{State: NodeStateDraining, LastSeenAt: old}, want: "stale_timeout", ok: true},
		{name: "fresh down", record: NodeRecord{State: NodeStateDown, DownAt: now}, want: "explicit_down"},
		{name: "fresh heartbeat", record: NodeRecord{State: NodeStateReady, LastSeenAt: now}, want: "stale_timeout"},
		{name: "unknown state", record: NodeRecord{State: "unknown", LastSeenAt: old}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, ok := retirementEligibility(tt.record, now, cfg)
			if reason != tt.want || ok != tt.ok {
				t.Fatalf("retirementEligibility = (%q, %v), want (%q, %v)", reason, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestRetirementRequiresReadBackAndRetainsNodeConfigAndVolumeRecord(t *testing.T) {
	ctx := context.Background()
	base := newBlobStateStore(newMemBlob())
	controller := retirementTestController(t, base, true, false)
	old := time.Now().UTC().Add(-2 * time.Hour)
	putRetirementCandidate(t, base, controller.cfg, NodeStateDown, 7, old)
	volumeKey, _ := volumeRecordKey(controller.cfg.State.Prefix, "db", "data")
	if _, err := base.PutJSON(ctx, volumeKey, VolumeRecord{LogicalID: "db/data", Type: config.VolumeTypeLocal, BoundNode: "node-1"}); err != nil {
		t.Fatal(err)
	}
	controller.runNodeRetirementMaintenance(ctx)
	if _, exists := readNode(t, base, controller.cfg, "node-1"); !exists {
		t.Fatal("node retired without an explicit empty read-back")
	}
	if _, _, exists, err := base.GetRaw(ctx, volumeKey); err != nil || !exists {
		t.Fatalf("retirement removed retained volume record: exists=%v err=%v", exists, err)
	}

	putRetirementEmptyConfig(t, base, controller.cfg.State.Prefix, "node-1", "rendered-1")
	controller.runNodeRetirementMaintenance(ctx)
	if _, exists := readNode(t, base, controller.cfg, "node-1"); exists {
		t.Fatal("eligible node record remained after successful retirement")
	}
	var tombstone NodeTombstone
	if _, exists, err := base.GetJSON(ctx, mustNodeTombstoneKey(controller.cfg.State.Prefix, "node-1"), &tombstone); err != nil || !exists {
		t.Fatalf("tombstone exists=%v err=%v", exists, err)
	}
	if tombstone.NodeID != "node-1" || tombstone.MaxGeneration != 7 || tombstone.Reason != "explicit_down" {
		t.Fatalf("unexpected tombstone: %+v", tombstone)
	}
	var index NodeTombstoneIndex
	if _, exists, err := base.GetJSON(ctx, nodeTombstoneIndexKey(controller.cfg.State.Prefix), &index); err != nil || !exists {
		t.Fatalf("tombstone index exists=%v err=%v", exists, err)
	}
	if !validNodeTombstoneIndex(index) || len(index.NodeIDs) != 1 || index.NodeIDs[0] != "node-1" {
		t.Fatalf("unexpected tombstone index: %+v", index)
	}
	if _, _, exists, err := base.GetRaw(ctx, legacyNodeConfigKey(controller.cfg.State.Prefix, "node-1")); err != nil || !exists {
		t.Fatalf("retirement removed explicit node config: exists=%v err=%v", exists, err)
	}
}

func TestRetirementDeleteFailureRetriesFromTombstoneBoundary(t *testing.T) {
	ctx := context.Background()
	base := newBlobStateStore(newMemBlob())
	wrapped := &retirementDeleteFailureStore{StateStore: base, fail: true}
	controller := retirementTestController(t, wrapped, true, false)
	putRetirementCandidate(t, wrapped, controller.cfg, NodeStateDown, 7, time.Now().UTC().Add(-2*time.Hour))
	putRetirementEmptyConfig(t, wrapped, controller.cfg.State.Prefix, "node-1", "rendered-1")
	controller.runNodeRetirementMaintenance(ctx)
	if record, exists := readNode(t, wrapped, controller.cfg, "node-1"); !exists || record.RetirementPendingAt.IsZero() {
		t.Fatalf("failed delete did not leave a pending retry record: exists=%v record=%+v", exists, record)
	}
	var tombstone NodeTombstone
	if _, exists, err := wrapped.GetJSON(ctx, mustNodeTombstoneKey(controller.cfg.State.Prefix, "node-1"), &tombstone); err != nil || !exists {
		t.Fatalf("tombstone was not committed before delete retry: exists=%v err=%v", exists, err)
	}
	controller.runNodeRetirementMaintenance(ctx)
	if _, exists := readNode(t, wrapped, controller.cfg, "node-1"); exists {
		t.Fatal("retirement retry did not delete the pending record")
	}
}

func TestNewerRegistrationWinsRetirementCAS(t *testing.T) {
	ctx := context.Background()
	base := newBlobStateStore(newMemBlob())
	server, _ := newTestRegistry(t)
	racingStore := &retirementRegistrationRaceStore{StateStore: base, server: server, t: t}
	server.store = racingStore
	controller := retirementTestController(t, racingStore, true, false)
	tombstoneKey, _ := nodeTombstoneKey(controller.cfg.State.Prefix, "node-1")
	racingStore.key = tombstoneKey
	putRetirementCandidate(t, racingStore, controller.cfg, NodeStateDown, 7, time.Now().UTC().Add(-2*time.Hour))
	putRetirementEmptyConfig(t, racingStore, controller.cfg.State.Prefix, "node-1", "rendered-1")
	controller.runNodeRetirementMaintenance(ctx)
	record, exists := readNode(t, racingStore, controller.cfg, "node-1")
	if !exists || record.Generation != 8 || !record.RetirementPendingAt.IsZero() {
		t.Fatalf("newer registration was deleted or fenced: exists=%v record=%+v", exists, record)
	}
	var tombstone NodeTombstone
	if _, exists, err := racingStore.GetJSON(ctx, tombstoneKey, &tombstone); err != nil || !exists || tombstone.MaxGeneration != 7 {
		t.Fatalf("older retirement floor = %+v, exists=%v err=%v", tombstone, exists, err)
	}
	nodes, _, _, err := controller.discoverActiveNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].InstanceID != "node-1" {
		t.Fatalf("active generation above tombstone floor was eclipsed: %#v", nodes)
	}
}

func TestRetirementDisabledDoesNotWriteLifecycleState(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	controller := retirementTestController(t, store, false, true)
	putRetirementCandidate(t, store, controller.cfg, NodeStateDown, 7, time.Now().UTC().Add(-2*time.Hour))
	putRetirementEmptyConfig(t, store, controller.cfg.State.Prefix, "node-1", "rendered-1")
	controller.runNodeRetirementMaintenance(ctx)
	if _, exists := readNode(t, store, controller.cfg, "node-1"); !exists {
		t.Fatal("disabled retirement removed the active record")
	}
	if _, exists, err := store.GetJSON(ctx, mustNodeTombstoneKey(controller.cfg.State.Prefix, "node-1"), &NodeTombstone{}); err != nil || exists {
		t.Fatalf("disabled retirement wrote a tombstone: exists=%v err=%v", exists, err)
	}
	if _, exists, err := store.GetJSON(ctx, nodeTombstoneIndexKey(controller.cfg.State.Prefix), &NodeTombstoneIndex{}); err != nil || exists {
		t.Fatalf("disabled retirement wrote an index: exists=%v err=%v", exists, err)
	}
}

func TestReadBackExplicitEmptyRequiresMatchingRenderedCopy(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	controller := retirementTestController(t, store, true, false)
	putRetirementEmptyConfig(t, store, controller.cfg.State.Prefix, "node-1", "rendered-1")
	if !controller.readBackExplicitEmptyNodeConfig(ctx, "node-1") {
		t.Fatal("matching explicit empty config was not accepted")
	}
	if _, err := store.PutRaw(ctx, renderedNodeKey(controller.cfg.State.Prefix, "rendered-1", "node-1"), []byte("node: node-1\nservices: []\n"), "application/x-yaml"); err != nil {
		t.Fatal(err)
	}
	if controller.readBackExplicitEmptyNodeConfig(ctx, "node-1") {
		t.Fatal("current config differing from its immutable rendered copy was accepted")
	}
}
