package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/objectstorage"
	"gopkg.in/yaml.v3"
)

type failingRecoveryIndexStore struct {
	StateStore
	fail bool
}

type conflictingRecoveryDeleteStore struct {
	StateStore
	key string
}

func (s *conflictingRecoveryDeleteStore) DeleteIfMatch(ctx context.Context, key string, token objectstorage.WriteToken) (bool, error) {
	if key == s.key {
		return false, nil
	}
	return s.StateStore.DeleteIfMatch(ctx, key, token)
}

func (s *failingRecoveryIndexStore) PutJSONIfMatch(ctx context.Context, key string, token objectstorage.WriteToken, value any) (bool, objectstorage.WriteToken, error) {
	if s.fail && key == nodeTombstoneIndexKey(emptyStatePrefix) {
		return false, "", errors.New("injected index failure")
	}
	return s.StateStore.PutJSONIfMatch(ctx, key, token, value)
}

func nodeRevivalFixture(t *testing.T) (StateStore, NodeTombstoneRecoveryRequest) {
	t.Helper()
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	nodeID := "node-retired"
	const rendered = "rendered-empty"
	nodeConfig := config.NodeConfig{
		Node: nodeID, Services: []config.ServiceConfig{}, DesiredRevision: "desired-empty",
		PlacementRevision: "placement-empty", RenderedRevision: rendered,
	}
	data, err := yaml.Marshal(nodeConfig)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{legacyNodeConfigKey(emptyStatePrefix, nodeID), renderedNodeKey(emptyStatePrefix, rendered, nodeID)} {
		if _, err := store.PutRaw(ctx, key, data, "application/x-yaml"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.PutJSON(ctx, renderedCurrentKey(emptyStatePrefix), RevisionPointer{Revision: rendered}); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	tombstone := NodeTombstone{NodeID: nodeID, MaxGeneration: 55, RetiredAt: stamp, Reason: "stale_timeout"}
	if _, err := store.PutJSON(ctx, mustNodeTombstoneKey(emptyStatePrefix, nodeID), tombstone); err != nil {
		t.Fatal(err)
	}
	index := NodeTombstoneIndex{NodeIDs: []string{nodeID}, Count: 1, Revision: 1, UpdatedAt: stamp}
	if _, err := store.PutJSON(ctx, nodeTombstoneIndexKey(emptyStatePrefix), index); err != nil {
		t.Fatal(err)
	}
	return store, NodeTombstoneRecoveryRequest{
		NodeID: nodeID, ExpectedGeneration: 55, Reason: "host was replaced",
		ExternalFenceReference: "ticket-1234", RetainedPoolReference: "pool-disk-serial-abc",
		ConfirmedHostFenced: true,
	}
}

func TestClearNodeTombstoneRequiresExternalFenceAndPreservesData(t *testing.T) {
	ctx := context.Background()
	store, request := nodeRevivalFixture(t)
	if _, err := store.PutJSON(ctx, mustVolumeRecordKey(emptyStatePrefix, "db", "data"), VolumeRecord{
		LogicalID: "db/data", Type: config.VolumeTypeLocal, BoundNode: request.NodeID,
		DesiredSizeBytes: 8 * config.GiB, AppliedSizeBytes: 8 * config.GiB, ResizeGeneration: 1, ResizeState: VolumeResizeApplied,
	}); err != nil {
		t.Fatal(err)
	}

	missingFence := request
	missingFence.ConfirmedHostFenced = false
	if _, err := ClearNodeTombstone(ctx, store, emptyStatePrefix, missingFence); err == nil {
		t.Fatal("recovery without external host-fence confirmation succeeded")
	}
	missingPool := request
	missingPool.RetainedPoolReference = ""
	if _, err := ClearNodeTombstone(ctx, store, emptyStatePrefix, missingPool); err == nil {
		t.Fatal("recovery without retained-pool verification reference succeeded")
	}
	mismatchedGeneration := request
	mismatchedGeneration.ExpectedGeneration++
	if _, err := ClearNodeTombstone(ctx, store, emptyStatePrefix, mismatchedGeneration); err == nil {
		t.Fatal("recovery with a mismatched generation succeeded")
	}
	if _, err := ClearNodeTombstone(ctx, store, emptyStatePrefix, request); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.GetJSON(ctx, mustNodeTombstoneKey(emptyStatePrefix, request.NodeID), &NodeTombstone{}); err != nil || exists {
		t.Fatalf("tombstone after recovery exists=%v err=%v", exists, err)
	}
	var index NodeTombstoneIndex
	if _, exists, err := store.GetJSON(ctx, nodeTombstoneIndexKey(emptyStatePrefix), &index); err != nil || !exists || index.Count != 0 || index.Revision != 2 {
		t.Fatalf("index after recovery = %#v, exists=%v err=%v", index, exists, err)
	}
	var record VolumeRecord
	if _, exists, err := store.GetJSON(ctx, mustVolumeRecordKey(emptyStatePrefix, "db", "data"), &record); err != nil || !exists || record.BoundNode != request.NodeID {
		t.Fatalf("retained volume record changed: %#v exists=%v err=%v", record, exists, err)
	}
	keys, err := store.ListKeys(ctx, nodeRevivalAuditsPrefix(emptyStatePrefix, request.NodeID))
	if err != nil || len(keys) != 1 {
		t.Fatalf("revival audit keys = %v, err %v", keys, err)
	}
	var audit NodeRevivalAudit
	if _, exists, err := store.GetJSON(ctx, keys[0], &audit); err != nil || !exists {
		t.Fatalf("revival audit exists=%v err=%v", exists, err)
	}
	if audit.Outcome != "completed" || audit.ClearedGeneration != request.ExpectedGeneration || audit.ExternalFenceReference != request.ExternalFenceReference || audit.RetainedPoolReference != request.RetainedPoolReference || audit.TombstoneTokenSHA256 == "" {
		t.Fatalf("audit does not prove recovery intent/result: %#v", audit)
	}
	if _, ok := readExplicitEmptyNodeConfig(ctx, store, emptyStatePrefix, request.NodeID); !ok {
		t.Fatal("recovery removed the explicit empty node config")
	}
}

func TestClearNodeTombstonePropagatesConditionalDeleteConflict(t *testing.T) {
	ctx := context.Background()
	base, request := nodeRevivalFixture(t)
	store := &conflictingRecoveryDeleteStore{StateStore: base, key: mustNodeTombstoneKey(emptyStatePrefix, request.NodeID)}
	if _, err := ClearNodeTombstone(ctx, store, emptyStatePrefix, request); err == nil {
		t.Fatal("conditional-delete conflict was not returned")
	}
	var tombstone NodeTombstone
	if _, exists, err := base.GetJSON(ctx, mustNodeTombstoneKey(emptyStatePrefix, request.NodeID), &tombstone); err != nil || !exists {
		t.Fatalf("conflicting tombstone exists=%v err=%v", exists, err)
	}
	keys, err := base.ListKeys(ctx, nodeRevivalAuditsPrefix(emptyStatePrefix, request.NodeID))
	if err != nil || len(keys) != 1 {
		t.Fatalf("audit keys = %v, err %v", keys, err)
	}
	var audit NodeRevivalAudit
	if _, exists, err := base.GetJSON(ctx, keys[0], &audit); err != nil || !exists || audit.Outcome != "delete_conflict" || !audit.ClearedAt.IsZero() {
		t.Fatalf("conflict audit = %#v, exists=%v err=%v", audit, exists, err)
	}
}

func TestClearNodeTombstoneResumesAfterIndexWriteFailure(t *testing.T) {
	ctx := context.Background()
	base, request := nodeRevivalFixture(t)
	store := &failingRecoveryIndexStore{StateStore: base, fail: true}
	if _, err := ClearNodeTombstone(ctx, store, emptyStatePrefix, request); err == nil {
		t.Fatal("injected index failure was not returned")
	}
	var tombstone NodeTombstone
	if _, exists, err := base.GetJSON(ctx, mustNodeTombstoneKey(emptyStatePrefix, request.NodeID), &tombstone); err != nil || exists {
		t.Fatalf("tombstone after partial recovery exists=%v err=%v", exists, err)
	}
	store.fail = false
	audit, err := ClearNodeTombstone(ctx, store, emptyStatePrefix, request)
	if err != nil || audit.Outcome != "completed" {
		t.Fatalf("resume audit = %#v, err %v", audit, err)
	}
	var index NodeTombstoneIndex
	if _, exists, err := base.GetJSON(ctx, nodeTombstoneIndexKey(emptyStatePrefix), &index); err != nil || !exists || index.Count != 0 {
		t.Fatalf("index after resume = %#v, exists=%v err=%v", index, exists, err)
	}
	keys, err := base.ListKeys(ctx, nodeRevivalAuditsPrefix(emptyStatePrefix, request.NodeID))
	if err != nil || len(keys) != 1 {
		t.Fatalf("resume created another audit intent: keys=%v err=%v", keys, err)
	}
}

func TestClearNodeTombstoneRejectsActiveRecordAndNonEmptyConfig(t *testing.T) {
	ctx := context.Background()
	store, request := nodeRevivalFixture(t)
	if _, err := store.PutJSON(ctx, mustNodeRecordKey(emptyStatePrefix, request.NodeID), NodeRecord{NodeID: request.NodeID, Generation: request.ExpectedGeneration}); err != nil {
		t.Fatal(err)
	}
	if _, err := ClearNodeTombstone(ctx, store, emptyStatePrefix, request); err == nil {
		t.Fatal("recovery with an active registry record succeeded")
	}
	if err := store.Delete(ctx, mustNodeRecordKey(emptyStatePrefix, request.NodeID)); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(config.NodeConfig{
		Node: request.NodeID, Services: []config.ServiceConfig{{Name: "db"}},
		DesiredRevision: "desired-empty", PlacementRevision: "placement-empty", RenderedRevision: "rendered-empty",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutRaw(ctx, legacyNodeConfigKey(emptyStatePrefix, request.NodeID), data, "application/x-yaml"); err != nil {
		t.Fatal(err)
	}
	if _, err := ClearNodeTombstone(ctx, store, emptyStatePrefix, request); err == nil {
		t.Fatal("recovery with non-empty current config succeeded")
	}
	var tombstone NodeTombstone
	if _, exists, err := store.GetJSON(ctx, mustNodeTombstoneKey(emptyStatePrefix, request.NodeID), &tombstone); err != nil || !exists {
		t.Fatalf("rejected recovery removed tombstone: exists=%v err=%v", exists, err)
	}
}
