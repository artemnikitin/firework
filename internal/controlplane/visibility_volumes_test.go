package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

func TestVolumeProjectionIncludesDesiredRetainedUnavailableAndInvalid(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	cfg := validConfigForRole(RoleAPI)
	cfg.NodeStaleTTL = time.Minute
	now := time.Now().UTC()
	desiredService := config.ServiceConfig{Name: "db", Volumes: []config.VolumeConfig{
		{Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data", SizeBytes: 20 * config.GiB, BoundNode: "node-1", ResizeGeneration: 2},
		{Name: "cache", Type: config.VolumeTypeShared, MountPath: "/cache", SizeBytes: 5 * config.GiB, SharedBackendID: "shared-new", ResizeGeneration: 1},
		{Name: "lost", Type: config.VolumeTypeLocal, MountPath: "/lost", SizeBytes: 4 * config.GiB, BoundNode: "node-gone", ResizeGeneration: 1},
		{Name: "remote", Type: config.VolumeTypeShared, MountPath: "/remote", SizeBytes: 2 * config.GiB, SharedBackendID: "shared-missing", ResizeGeneration: 1},
		{Name: "wait", Type: config.VolumeTypeLocal, MountPath: "/wait", SizeBytes: 6 * config.GiB, BoundNode: "node-1", ResizeGeneration: 2},
	}}
	desired := DesiredRevision{Revision: "desired-1", CreatedAt: now, Services: []config.ServiceConfig{desiredService}}
	placement := PlacementRevision{Revision: "placement-1", DesiredRevision: desired.Revision, CreatedAt: now, NodeConfigs: []config.NodeConfig{{Node: "node-1", Services: []config.ServiceConfig{desiredService}}}}
	putCurrentState(t, ctx, store, desired, placement, "rendered-1")
	putNode(t, ctx, store, cfg, NodeRecord{NodeID: "node-1", Generation: 2, State: NodeStateReady, LastSeenAt: now, Storage: StorageResources{SharedBackendID: "shared-new"}})
	tombstone := NodeTombstone{NodeID: "node-old", MaxGeneration: 8, RetiredAt: now, Reason: "stale_timeout"}
	if _, err := store.PutJSON(ctx, mustNodeTombstoneKey(cfg.State.Prefix, tombstone.NodeID), tombstone); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(ctx, nodeTombstoneIndexKey(cfg.State.Prefix), NodeTombstoneIndex{NodeIDs: []string{"node-old"}, Count: 1, Revision: 1, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, record := range []VolumeRecord{
		{LogicalID: "db/data", Type: config.VolumeTypeLocal, BoundNode: "node-1", DesiredSizeBytes: 20 * config.GiB, AppliedSizeBytes: 20 * config.GiB, ResizeGeneration: 2, ResizeState: VolumeResizeApplied},
		{LogicalID: "db/cache", Type: config.VolumeTypeShared, SharedBackendID: "shared-new", DesiredSizeBytes: 5 * config.GiB, AppliedSizeBytes: 5 * config.GiB, ResizeGeneration: 1, ResizeState: VolumeResizeApplied},
		{LogicalID: "db/lost", Type: config.VolumeTypeLocal, BoundNode: "node-gone", DesiredSizeBytes: 4 * config.GiB, AppliedSizeBytes: 4 * config.GiB, ResizeGeneration: 1, ResizeState: VolumeResizeApplied},
		{LogicalID: "db/remote", Type: config.VolumeTypeShared, SharedBackendID: "shared-missing", DesiredSizeBytes: 2 * config.GiB, AppliedSizeBytes: 2 * config.GiB, ResizeGeneration: 1, ResizeState: VolumeResizeApplied},
		{LogicalID: "db/wait", Type: config.VolumeTypeLocal, BoundNode: "node-1", DesiredSizeBytes: 6 * config.GiB, AppliedSizeBytes: 5 * config.GiB, ResizeGeneration: 2, ResizeState: VolumeResizePending},
		{LogicalID: "removed/local", Type: config.VolumeTypeLocal, BoundNode: "node-old", DesiredSizeBytes: 10 * config.GiB, AppliedSizeBytes: 10 * config.GiB, ResizeGeneration: 1, ResizeState: VolumeResizeApplied},
		{LogicalID: "removed/shared", Type: config.VolumeTypeShared, SharedBackendID: "shared-old", DesiredSizeBytes: 3 * config.GiB, AppliedSizeBytes: 3 * config.GiB, ResizeGeneration: 1, ResizeState: VolumeResizeApplied, LastError: strings.Repeat("x", 400)},
	} {
		parts := strings.Split(record.LogicalID, "/")
		if _, err := store.PutJSON(ctx, mustVolumeRecordKey(cfg.State.Prefix, parts[0], parts[1]), record); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.PutRaw(ctx, mustVolumeRecordKey(cfg.State.Prefix, "broken", "data"), []byte("{"), "application/json"); err != nil {
		t.Fatal(err)
	}

	visibility := NewVisibilityService(cfg, store)
	page, err := visibility.Volumes(ctx, "", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if page.Count != 2 || page.Total != 8 || page.Offset != 1 || page.Limit != 2 || !page.HasMore {
		t.Fatalf("pagination = %#v", page)
	}
	all, err := visibility.Volumes(ctx, "", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]operatorVolumeProjection)
	for _, volume := range all.Items {
		byID[volume.LogicalID] = operatorVolumeProjection{state: volume.State, reason: volume.ReasonCode, desired: volume.Desired, lastError: volume.LastError}
	}
	want := map[string]operatorVolumeProjection{
		"db/data":        {state: "active", desired: true},
		"db/cache":       {state: "active", desired: true},
		"db/lost":        {state: "node_unavailable", reason: "node_unavailable", desired: true},
		"db/remote":      {state: "backend_unavailable", reason: "backend_unavailable", desired: true},
		"db/wait":        {state: "pending", reason: "volume_pending", desired: true},
		"removed/local":  {state: "orphaned", reason: "node_retired"},
		"removed/shared": {state: "orphaned", reason: "backend_removed"},
		"broken/data":    {state: "invalid", reason: "record_unreadable"},
	}
	for id, expected := range want {
		if got := byID[id]; got.state != expected.state || got.reason != expected.reason || got.desired != expected.desired {
			t.Errorf("%s projection = %#v, want %#v", id, got, expected)
		}
	}
	if len(all.Problems) != 1 || all.Problems[0].ID != "broken/data" || all.Problems[0].Kind != "volume" {
		t.Fatalf("malformed volume problems = %#v", all.Problems)
	}
	if len([]rune(byID["removed/shared"].lastError)) > 256 {
		t.Fatalf("retained error was not bounded: %d chars", len([]rune(byID["removed/shared"].lastError)))
	}
	filtered, err := visibility.Volumes(ctx, "orphaned", 0, 100)
	if err != nil || filtered.Total != 2 {
		t.Fatalf("orphaned filter = %#v, err %v", filtered, err)
	}
	detail, found, err := visibility.Volume(ctx, "broken", "data")
	if err != nil || !found || detail.State != "invalid" || detail.Message == "{" {
		t.Fatalf("invalid detail = %#v, found %v, err %v", detail, found, err)
	}
}

type operatorVolumeProjection struct {
	state     string
	reason    string
	desired   bool
	lastError string
}

func TestRetiredNodesAreExplicitAndExcludedFromDefaultList(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	cfg := validConfigForRole(RoleAPI)
	now := time.Now().UTC()
	putCurrentState(t, ctx, store, DesiredRevision{Revision: "desired-1"}, PlacementRevision{Revision: "placement-1", DesiredRevision: "desired-1"}, "rendered-1")
	tombstone := NodeTombstone{NodeID: "node-retired", MaxGeneration: 12, RetiredAt: now, Reason: "explicit_down"}
	if _, err := store.PutJSON(ctx, mustNodeTombstoneKey(cfg.State.Prefix, tombstone.NodeID), tombstone); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(ctx, nodeTombstoneIndexKey(cfg.State.Prefix), NodeTombstoneIndex{NodeIDs: []string{tombstone.NodeID}, Count: 1, Revision: 1, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	visibility := NewVisibilityService(cfg, store)
	active, err := visibility.Nodes(ctx, "")
	if err != nil || active.Count != 0 {
		t.Fatalf("default node list = %#v, err %v", active, err)
	}
	retired, err := visibility.Nodes(ctx, "retired")
	if err != nil || retired.Count != 1 || retired.Items[0].NodeID != tombstone.NodeID || retired.Items[0].State != "retired" {
		t.Fatalf("retired node list = %#v, err %v", retired, err)
	}
	detail, found, err := visibility.Node(ctx, tombstone.NodeID)
	if err != nil || !found || detail.State != "retired" || detail.MaxGeneration != tombstone.MaxGeneration || detail.RetirementReason != tombstone.Reason {
		t.Fatalf("retired node detail = %#v, found %v, err %v", detail, found, err)
	}
}

func TestActiveNodeDetailIncludesPriorRevivalAudit(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	cfg := validConfigForRole(RoleAPI)
	now := time.Now().UTC()
	putCurrentState(t, ctx, store, DesiredRevision{Revision: "desired-1"}, PlacementRevision{Revision: "placement-1", DesiredRevision: "desired-1"}, "rendered-1")
	if _, err := store.PutJSON(ctx, mustNodeRecordKey(cfg.State.Prefix, "node-reused"), NodeRecord{
		NodeID: "node-reused", Generation: 70, State: NodeStateReady, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	audit := NodeRevivalAudit{
		NodeID: "node-reused", ClearedGeneration: 42, ClearedAt: now.Add(-time.Minute),
		OperatorReason: "old host fenced", ExternalFenceReference: "incident-4",
		RetainedPoolReference: "disk-serial-4", Outcome: "completed",
	}
	key, err := nodeRevivalAuditKey(cfg.State.Prefix, audit.NodeID, "00000000000000000001-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(ctx, key, audit); err != nil {
		t.Fatal(err)
	}
	detail, found, err := NewVisibilityService(cfg, store).Node(ctx, audit.NodeID)
	if err != nil || !found || detail.State != "ready" || len(detail.RevivalHistory) != 1 || detail.RevivalHistory[0].ExternalFenceReference != "incident-4" {
		t.Fatalf("reused node detail = %#v, found=%v err=%v", detail, found, err)
	}
}
