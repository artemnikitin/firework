package controlplane

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

// failingListStore fails ListKeys while failures > 0.
type failingListStore struct {
	StateStore
	mu       sync.Mutex
	failures int
}

func (s *failingListStore) ListKeys(ctx context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	if s.failures > 0 {
		s.failures--
		s.mu.Unlock()
		return nil, errors.New("listing unavailable")
	}
	s.mu.Unlock()
	return s.StateStore.ListKeys(ctx, prefix)
}

func cachedVisibility(store StateStore) *VisibilityService {
	cfg := validConfigForRole(RoleAPI)
	cfg.APISnapshotTTL = time.Minute
	return NewVisibilityService(cfg, store)
}

// The overview issues /v1/status, /v1/nodes, and /v1/services together, and
// several operators may poll at once; all of them share one object-store scan.
func TestConcurrentVisibilityRequestsShareOneSnapshotBuild(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	putCurrentState(t, ctx, store, DesiredRevision{Revision: "desired-1"}, PlacementRevision{Revision: "placement-1", DesiredRevision: "desired-1"}, "rendered-1")
	service := cachedVisibility(store)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			switch i % 3 {
			case 0:
				_, err = service.Revision(ctx)
			case 1:
				_, err = service.Nodes(ctx, "")
			default:
				_, err = service.Services(ctx, "", "", "")
			}
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if service.builds != 1 {
		t.Fatalf("snapshot builds = %d, want 1", service.builds)
	}
}

// A failed build must not be served for a TTL: the next request retries.
func TestFailedSnapshotBuildIsNotCached(t *testing.T) {
	ctx := context.Background()
	store := &failingListStore{StateStore: newBlobStateStore(newMemBlob()), failures: 1}
	service := cachedVisibility(store)
	if _, err := service.Nodes(ctx, ""); err == nil {
		t.Fatal("expected the first build to fail")
	}
	if _, err := service.Nodes(ctx, ""); err != nil {
		t.Fatalf("second request served the cached failure: %v", err)
	}
	if service.builds != 2 {
		t.Fatalf("builds = %d, want 2", service.builds)
	}
}

func TestSnapshotTTLZeroRebuildsEveryRequest(t *testing.T) {
	ctx := context.Background()
	service := NewVisibilityService(validConfigForRole(RoleAPI), newBlobStateStore(newMemBlob()))
	for i := 0; i < 3; i++ {
		if _, err := service.Nodes(ctx, ""); err != nil {
			t.Fatal(err)
		}
	}
	if service.builds != 3 {
		t.Fatalf("builds = %d, want 3", service.builds)
	}
}

func putRawRecord(t *testing.T, store StateStore, key string, data string) {
	t.Helper()
	if _, err := store.PutRaw(context.Background(), key, []byte(data), "application/json"); err != nil {
		t.Fatal(err)
	}
}

// One malformed record degrades the projection; it must not take every
// endpoint offline.
func TestMalformedRecordsAreIsolated(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	cfg := validConfigForRole(RoleAPI)
	cfg.NodeStaleTTL = time.Minute
	now := time.Now().UTC()
	desired := DesiredRevision{Revision: "desired-1", Services: []config.ServiceConfig{{Name: "service"}}}
	placement := PlacementRevision{Revision: "placement-1", DesiredRevision: desired.Revision, NodeConfigs: []config.NodeConfig{{Node: "node-1", Services: desired.Services}}}
	putCurrentState(t, ctx, store, desired, placement, "rendered-1")
	putNode(t, ctx, store, cfg, NodeRecord{NodeID: "node-1", State: NodeStateReady, LastSeenAt: now, AgentStatus: currentAgentStatus(statusmodel.PhaseReady, "rendered-1")})
	badNode, _ := nodeRecordKey(cfg.State.Prefix, "node-broken")
	putRawRecord(t, store, badNode, "{not json")
	mismatched, _ := nodeRecordKey(cfg.State.Prefix, "node-renamed")
	putRawRecord(t, store, mismatched, `{"node_id":"someone-else"}`)
	putRawRecord(t, store, mustVolumeRecordKey(cfg.State.Prefix, "other", "data"), "{not json")

	service := NewVisibilityService(cfg, store)
	nodes, err := service.Nodes(ctx, "")
	if err != nil {
		t.Fatalf("a malformed record took the node list offline: %v", err)
	}
	if nodes.Count != 1 || nodes.Items[0].NodeID != "node-1" {
		t.Fatalf("nodes = %#v", nodes.Items)
	}
	want := map[string]string{"node/node-broken": "record_unreadable", "node/node-renamed": "record_invalid", "volume/other/data": "record_unreadable"}
	if len(nodes.Problems) != len(want) {
		t.Fatalf("problems = %#v", nodes.Problems)
	}
	for _, problem := range nodes.Problems {
		if want[problem.Kind+"/"+problem.ID] != problem.ReasonCode {
			t.Fatalf("unexpected problem %#v", problem)
		}
	}
	if _, err := service.Services(ctx, "", "", ""); err != nil {
		t.Fatalf("a malformed record took the service list offline: %v", err)
	}
	status, err := service.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// node-1 converged, but the fleet view rests on records it could not read.
	if status.Phase != "degraded" || status.ReasonCode != "projection_incomplete" || len(status.Problems) != 3 {
		t.Fatalf("status = %s/%s problems=%d, want degraded/projection_incomplete", status.Phase, status.ReasonCode, len(status.Problems))
	}
}

// With an empty desired revision every known node is relevant, so a node
// whose record cannot be read could be one still running old workloads.
func TestUnreadableNodeBlocksEmptyRevisionConvergence(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	cfg := validConfigForRole(RoleAPI)
	putCurrentState(t, ctx, store, DesiredRevision{Revision: "desired-1"}, PlacementRevision{Revision: "placement-1", DesiredRevision: "desired-1"}, "rendered-1")
	badNode, _ := nodeRecordKey(cfg.State.Prefix, "node-broken")
	putRawRecord(t, store, badNode, "{not json")

	status, err := NewVisibilityService(cfg, store).Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != "unknown" || status.ReasonCode != "node_record_unreadable" {
		t.Fatalf("status = %s/%s, want unknown/node_record_unreadable", status.Phase, status.ReasonCode)
	}
}
