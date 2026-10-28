package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/objectstorage"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

type nodeQuarantineDeleteFailureStore struct {
	StateStore
	key  string
	fail bool
}

func (s *nodeQuarantineDeleteFailureStore) DeleteIfMatch(ctx context.Context, key string, token objectstorage.WriteToken) (bool, error) {
	if key == s.key && s.fail {
		return false, errors.New("injected quarantine delete failure")
	}
	return s.StateStore.DeleteIfMatch(ctx, key, token)
}

func TestNodeQuarantineKeysHashRegistryKeysAndRejectInvalidIDs(t *testing.T) {
	prefix := "cp/v1/"
	first, err := nodeQuarantineKey(prefix, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := nodeQuarantineKey(prefix, "node-b")
	if err != nil {
		t.Fatal(err)
	}
	if first == second || strings.Contains(first, "node-a") || strings.Contains(second, "node-b") {
		t.Fatalf("quarantine keys expose or collide across node IDs: %q %q", first, second)
	}
	registryKey, err := nodeRecordKey(prefix, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(registryKey))
	if got, want := path.Base(first), fmt.Sprintf("%x.json", digest); got != want {
		t.Fatalf("quarantine key basename = %q, want object-key hash %q", got, want)
	}
	for _, invalidID := range []string{"", " ", "../node", strings.Repeat("x", statusmodel.MaxVolumeIDLen+1)} {
		if key, err := nodeQuarantineKey(prefix, invalidID); err == nil {
			t.Errorf("invalid node ID %q produced quarantine key %q", invalidID, key)
		}
	}
}

func TestDiscoverActiveNodesQuarantinesInvalidRecordsAndClearsAfterRepair(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "malformed json", raw: "{not-json"},
		{name: "identity mismatch", raw: `{"node_id":"somebody-else","generation":1,"state":"ready"}`},
		{name: "invalid state", raw: `{"node_id":"node-1","generation":1,"state":"unknown"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			controller, store := nodeTypeController(t)
			ctx := context.Background()
			sourceKey, err := nodeRecordKey(controller.cfg.State.Prefix, "node-1")
			if err != nil {
				t.Fatal(err)
			}
			sourceToken, err := store.PutRaw(ctx, sourceKey, []byte(test.raw), "application/json")
			if err != nil {
				t.Fatal(err)
			}

			nodes, _, known, err := controller.discoverActiveNodes(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(nodes) != 0 || len(known) != 0 {
				t.Fatalf("invalid registry record entered discovery: nodes=%#v known=%#v", nodes, known)
			}
			var quarantine NodeQuarantine
			if _, exists, err := store.GetJSON(ctx, mustNodeQuarantineKey(controller.cfg.State.Prefix, "node-1"), &quarantine); err != nil || !exists {
				t.Fatalf("quarantine exists=%v err=%v", exists, err)
			}
			if quarantine.NodeID != "node-1" || quarantine.ObjectToken != string(sourceToken) || quarantine.ReasonCode != nodeQuarantineReasonInvalid || quarantine.Message != nodeQuarantineMessage {
				t.Fatalf("unexpected quarantine metadata: %+v", quarantine)
			}

			repaired := NodeRecord{
				NodeID: "node-1", Generation: 1, State: NodeStateReady,
				Capacity: Resources{VCPUs: 2, MemoryMB: 1024}, LastSeenAt: time.Now().UTC(),
			}
			if _, err := store.PutJSON(ctx, sourceKey, repaired); err != nil {
				t.Fatal(err)
			}
			if nodes, _, _, err := controller.discoverActiveNodes(ctx); err != nil || len(nodes) != 1 {
				t.Fatalf("repaired record discovery returned %#v, err %v", nodes, err)
			}
			if _, _, exists, err := store.GetRaw(ctx, mustNodeQuarantineKey(controller.cfg.State.Prefix, "node-1")); err != nil || exists {
				t.Fatalf("stale quarantine remains after source repair: exists=%v err=%v", exists, err)
			}
		})
	}
}

func TestVisibilityProjectsQuarantineWithoutSourceTokenOrPayload(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	cfg := validConfigForRole(RoleAPI)
	now := time.Now().UTC()
	putCurrentState(t, ctx, store, DesiredRevision{Revision: "desired-1", CreatedAt: now}, PlacementRevision{Revision: "placement-1", DesiredRevision: "desired-1", CreatedAt: now}, "rendered-1")
	sourceKey, err := nodeRecordKey(cfg.State.Prefix, "node-bad")
	if err != nil {
		t.Fatal(err)
	}
	const rawPayload = `{"private":"do-not-project","bad":`
	if _, err := store.PutRaw(ctx, sourceKey, []byte(rawPayload), "application/json"); err != nil {
		t.Fatal(err)
	}
	_, sourceToken, exists, err := store.GetRaw(ctx, sourceKey)
	if err != nil || !exists {
		t.Fatalf("read invalid source: exists=%v err=%v", exists, err)
	}
	quarantineKey, err := nodeQuarantineKey(cfg.State.Prefix, "node-bad")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(ctx, quarantineKey, NodeQuarantine{
		NodeID: "node-bad", ObjectToken: string(sourceToken) + "-stale", DetectedAt: now,
		ReasonCode: nodeQuarantineReasonInvalid, Message: "untrusted message with " + "private payload",
	}); err != nil {
		t.Fatal(err)
	}

	cfg.APISnapshotTTL = 0
	service := NewVisibilityService(cfg, store)
	staleList, err := service.Nodes(ctx, "")
	if err != nil || staleList.Count != 0 {
		t.Fatalf("stale quarantine projected for a different source token: %#v err=%v", staleList, err)
	}
	if _, err := store.PutJSON(ctx, quarantineKey, NodeQuarantine{
		NodeID: "node-bad", ObjectToken: string(sourceToken), DetectedAt: now,
		ReasonCode: nodeQuarantineReasonInvalid, Message: "untrusted message with " + "private payload",
	}); err != nil {
		t.Fatal(err)
	}
	service = NewVisibilityService(cfg, store)
	list, err := service.Nodes(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if list.Count != 1 || list.Items[0].NodeID != "node-bad" || list.Items[0].State != "unknown" || !list.Items[0].Quarantined || list.Items[0].QuarantineReason != nodeQuarantineReasonInvalid || list.Items[0].QuarantineMessage != nodeQuarantineMessage {
		t.Fatalf("quarantined node list item = %#v", list.Items)
	}
	detail, found, err := service.Node(ctx, "node-bad")
	if err != nil || !found || !detail.Quarantined || detail.ReasonCode != "node_quarantined" {
		t.Fatalf("quarantined node detail found=%v err=%v detail=%#v", found, err, detail)
	}
	listJSON, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	response := string(listJSON) + string(detailJSON)
	for _, forbidden := range []string{rawPayload, "do-not-project", "untrusted message", `"object_token"`} {
		if strings.Contains(response, forbidden) {
			t.Fatalf("API response exposed %q: %s", forbidden, response)
		}
	}

	if _, err := store.PutRaw(ctx, sourceKey, []byte(`{"another":"malformed`), "application/json"); err != nil {
		t.Fatal(err)
	}
	changedSourceList, err := NewVisibilityService(cfg, store).Nodes(ctx, "")
	if err != nil || changedSourceList.Count != 0 {
		t.Fatalf("quarantine for prior source token projected after source changed: %#v err=%v", changedSourceList, err)
	}
}

func TestRetirementClearsNodeQuarantineBeforeRegistryDelete(t *testing.T) {
	ctx := context.Background()
	base := newBlobStateStore(newMemBlob())
	controller := retirementTestController(t, base, true, false)
	putRetirementCandidate(t, base, controller.cfg, NodeStateDown, 7, time.Now().UTC().Add(-2*time.Hour))
	registryKey := mustNodeRecordKey(controller.cfg.State.Prefix, "node-1")
	_, registryToken, exists, err := base.GetRaw(ctx, registryKey)
	if err != nil || !exists {
		t.Fatalf("registry record exists=%v err=%v", exists, err)
	}
	quarantineKey := mustNodeQuarantineKey(controller.cfg.State.Prefix, "node-1")
	if _, err := base.PutJSON(ctx, quarantineKey, NodeQuarantine{
		NodeID: "node-1", ObjectToken: string(registryToken), DetectedAt: time.Now().UTC(),
		ReasonCode: nodeQuarantineReasonInvalid, Message: nodeQuarantineMessage,
	}); err != nil {
		t.Fatal(err)
	}
	putRetirementEmptyConfig(t, base, controller.cfg.State.Prefix, "node-1", "rendered-1")
	controller.runNodeRetirementMaintenance(ctx)
	if _, _, exists, err := base.GetRaw(ctx, registryKey); err != nil || exists {
		t.Fatalf("registry record exists after retirement=%v err=%v", exists, err)
	}
	if _, _, exists, err := base.GetRaw(ctx, quarantineKey); err != nil || exists {
		t.Fatalf("quarantine remains after retirement=%v err=%v", exists, err)
	}
}

func TestRetirementDefersRegistryDeleteWhenQuarantineCleanupFails(t *testing.T) {
	ctx := context.Background()
	base := newBlobStateStore(newMemBlob())
	wrapped := &nodeQuarantineDeleteFailureStore{StateStore: base, fail: true}
	controller := retirementTestController(t, wrapped, true, false)
	quarantineKey := mustNodeQuarantineKey(controller.cfg.State.Prefix, "node-1")
	wrapped.key = quarantineKey
	putRetirementCandidate(t, wrapped, controller.cfg, NodeStateDown, 7, time.Now().UTC().Add(-2*time.Hour))
	registryKey := mustNodeRecordKey(controller.cfg.State.Prefix, "node-1")
	_, registryToken, exists, err := wrapped.GetRaw(ctx, registryKey)
	if err != nil || !exists {
		t.Fatalf("registry record exists=%v err=%v", exists, err)
	}
	if _, err := wrapped.PutJSON(ctx, quarantineKey, NodeQuarantine{
		NodeID: "node-1", ObjectToken: string(registryToken), DetectedAt: time.Now().UTC(),
		ReasonCode: nodeQuarantineReasonInvalid, Message: nodeQuarantineMessage,
	}); err != nil {
		t.Fatal(err)
	}
	putRetirementEmptyConfig(t, wrapped, controller.cfg.State.Prefix, "node-1", "rendered-1")
	controller.runNodeRetirementMaintenance(ctx)
	if record, exists := readNode(t, wrapped, controller.cfg, "node-1"); !exists || record.RetirementPendingAt.IsZero() {
		t.Fatalf("failed quarantine cleanup did not defer registry delete: exists=%v record=%+v", exists, record)
	}
	if _, _, exists, err := wrapped.GetRaw(ctx, quarantineKey); err != nil || !exists {
		t.Fatalf("quarantine lost after failed cleanup: exists=%v err=%v", exists, err)
	}

	wrapped.fail = false
	controller.runNodeRetirementMaintenance(ctx)
	if _, _, exists, err := wrapped.GetRaw(ctx, registryKey); err != nil || exists {
		t.Fatalf("retirement did not resume after quarantine cleanup: exists=%v err=%v", exists, err)
	}
}

func mustNodeQuarantineKey(prefix, nodeID string) string {
	key, err := nodeQuarantineKey(prefix, nodeID)
	if err != nil {
		panic(err)
	}
	return key
}
