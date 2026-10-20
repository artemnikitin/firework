package controlplane

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/scheduler"
)

const nodeTypeTestPrefix = "cp/v1/"

func nodeTypeController(t *testing.T) (*Controller, StateStore) {
	t.Helper()
	store := newBlobStateStore(newMemBlob())
	cfg := Config{
		State:          StateConfig{Prefix: nodeTypeTestPrefix},
		LeaderLeaseTTL: time.Minute,
		NodeStaleTTL:   time.Minute,
	}
	controller := NewController(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := controller.renewLeadership(context.Background()); err != nil || !controller.leader {
		t.Fatalf("controller did not acquire leadership: leader=%v err=%v", controller.leader, err)
	}
	return controller, store
}

func putReadyNode(t *testing.T, store StateStore, id string, labels ...string) {
	t.Helper()
	key, err := nodeRecordKey(nodeTypeTestPrefix, id)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := NodeRecord{
		NodeID: id, Generation: 1, State: NodeStateReady, Labels: labels,
		Capacity: Resources{VCPUs: 8, MemoryMB: 8192}, LastSeenAt: now, UpdatedAt: now,
	}
	if _, err := store.PutJSON(context.Background(), key, record); err != nil {
		t.Fatal(err)
	}
}

func publishDesired(t *testing.T, store StateStore, revision string, services ...config.ServiceConfig) {
	t.Helper()
	ctx := context.Background()
	desired := DesiredRevision{Revision: revision, CreatedAt: time.Now().UTC(), Services: services}
	if _, err := store.PutJSON(ctx, desiredRevisionKey(nodeTypeTestPrefix, revision), desired); err != nil {
		t.Fatal(err)
	}
	if err := upsertPointer(ctx, store, desiredCurrentKey(nodeTypeTestPrefix), revision); err != nil {
		t.Fatal(err)
	}
}

func nodeTypePlacement(t *testing.T, store StateStore) PlacementRevision {
	t.Helper()
	ctx := context.Background()
	var ptr RevisionPointer
	if _, exists, err := store.GetJSON(ctx, placementCurrentKey(nodeTypeTestPrefix), &ptr); err != nil || !exists {
		t.Fatalf("no current placement: exists=%v err=%v", exists, err)
	}
	var placement PlacementRevision
	if _, exists, err := store.GetJSON(ctx, placementRevisionKey(nodeTypeTestPrefix, ptr.Revision), &placement); err != nil || !exists {
		t.Fatalf("placement %s unreadable: exists=%v err=%v", ptr.Revision, exists, err)
	}
	return placement
}

func placementNodeOf(placement PlacementRevision, service string) string {
	for _, nc := range placement.NodeConfigs {
		for _, placed := range nc.Services {
			if placed.Name == service {
				return nc.Node
			}
		}
	}
	return ""
}

// End to end through the stored desired revision and registry records: the
// node_type must survive the JSON round trip and be matched against labels.
func TestReconcileEnforcesNodeTypeAgainstRegistryLabels(t *testing.T) {
	controller, store := nodeTypeController(t)
	putReadyNode(t, store, "node-web", "web")
	putReadyNode(t, store, "node-backend", "web", "backend")
	publishDesired(t, store, "desired-1",
		config.ServiceConfig{Name: "db", VCPUs: 1, MemoryMB: 256, NodeType: "backend"},
		config.ServiceConfig{Name: "gpu-job", VCPUs: 1, MemoryMB: 256, NodeType: "gpu"},
		config.ServiceConfig{Name: "legacy", VCPUs: 1, MemoryMB: 256},
	)

	controller.runReconcile(context.Background())
	placement := nodeTypePlacement(t, store)
	if got := placementNodeOf(placement, "db"); got != "node-backend" {
		t.Fatalf("db placed on %q, want node-backend", got)
	}
	if got := placementNodeOf(placement, "legacy"); got == "" {
		t.Fatal("a service without node_type must stay schedulable")
	}
	if len(placement.PendingServices) != 1 || placement.PendingServices[0].Service != "gpu-job" ||
		placement.PendingServices[0].ReasonCode != scheduler.ReasonNoMatchingNodeType {
		t.Fatalf("expected gpu-job pending %s, got %#v", scheduler.ReasonNoMatchingNodeType, placement.PendingServices)
	}

	// The node re-registers without the label. The desired revision is
	// unchanged, so only the labels in the scheduling signature can trigger
	// the pass that stops preserving the now-ineligible placement.
	putReadyNode(t, store, "node-backend", "web")
	putReadyNode(t, store, "node-web", "web", "backend")
	controller.runReconcile(context.Background())
	if got := placementNodeOf(nodeTypePlacement(t, store), "db"); got != "node-web" {
		t.Fatalf("after label removal db placed on %q, want node-web", got)
	}
}

func TestSchedulingInputSignatureTracksLabels(t *testing.T) {
	base := []scheduler.Node{{InstanceID: "n", CapacityVCPUs: 4, CapacityMemMB: 4096}}
	unlabeled, err := schedulingInputSignature("rev", base, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	labeledNodes := []scheduler.Node{{InstanceID: "n", Labels: []string{"web"}, CapacityVCPUs: 4, CapacityMemMB: 4096}}
	labeled, err := schedulingInputSignature("rev", labeledNodes, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if labeled == unlabeled {
		t.Fatal("a label change must change the scheduling input signature")
	}
}

func TestDiscoverActiveNodesNormalizesLabels(t *testing.T) {
	controller, store := nodeTypeController(t)
	putReadyNode(t, store, "n", "web", "backend", "web")
	nodes, _, _, err := controller.discoverActiveNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || len(nodes[0].Labels) != 2 || nodes[0].Labels[0] != "backend" || nodes[0].Labels[1] != "web" {
		t.Fatalf("expected sorted, de-duplicated labels, got %#v", nodes)
	}
}
