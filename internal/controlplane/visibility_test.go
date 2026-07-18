package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

func TestVisibilityDerivedStates(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	cfg := validConfigForRole(RoleAPI)
	cfg.NodeStaleTTL = time.Minute
	now := time.Now().UTC()

	desired := DesiredRevision{Revision: "desired-1", CreatedAt: now, Services: []config.ServiceConfig{
		{Name: "a-healthy", Image: "/images/a.ext4", Kernel: "/images/vmlinux", VCPUs: 2, MemoryMB: 512, HealthCheck: &config.HealthCheckConfig{Type: "http"}},
		{Name: "b-unhealthy", VCPUs: 1, MemoryMB: 256, HealthCheck: &config.HealthCheckConfig{Type: "tcp"}},
		{Name: "c-failed", VCPUs: 1, MemoryMB: 128},
		{Name: "d-unplaced", VCPUs: 1, MemoryMB: 128},
		{Name: "e-stale", VCPUs: 1, MemoryMB: 128},
		{Name: "f-old-agent", VCPUs: 1, MemoryMB: 128},
	}}
	placement := PlacementRevision{Revision: "placement-1", DesiredRevision: desired.Revision, CreatedAt: now, NodeConfigs: []config.NodeConfig{
		{Node: "node-1", Services: desired.Services[:3]},
		{Node: "node-stale", Services: desired.Services[4:5]},
		{Node: "node-old", Services: desired.Services[5:]},
	}}
	putCurrentState(t, ctx, store, desired, placement, "rendered-1")
	putNode(t, ctx, store, cfg, NodeRecord{NodeID: "node-1", State: NodeStateReady, Capacity: Resources{VCPUs: 8, MemoryMB: 4096}, LastSeenAt: now, AgentStatus: &statusmodel.AgentStatus{
		SchemaVersion: 1, AgentVersion: "test", ObservedAt: now, Phase: statusmodel.PhaseFailed, AppliedRevision: "rendered-1", Services: []statusmodel.ServiceStatus{
			{Name: "a-healthy", VMState: "running", Health: "healthy"},
			{Name: "b-unhealthy", VMState: "running", Health: "unhealthy", ReasonCode: "health_check_failed"},
			{Name: "c-failed", VMState: "failed", Health: "not_configured", ReasonCode: "vm_failed"},
		},
	}})
	putNode(t, ctx, store, cfg, NodeRecord{NodeID: "node-stale", State: NodeStateReady, Capacity: Resources{VCPUs: 2, MemoryMB: 512}, LastSeenAt: now.Add(-2 * time.Minute), AgentStatus: &statusmodel.AgentStatus{SchemaVersion: 1, ObservedAt: now.Add(-2 * time.Minute)}})
	putNode(t, ctx, store, cfg, NodeRecord{NodeID: "node-old", State: NodeStateDown, Capacity: Resources{VCPUs: 2, MemoryMB: 512}, LastSeenAt: now})

	service := NewVisibilityService(cfg, store)
	list, err := service.Services(ctx, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if list.Count != 6 {
		t.Fatalf("service count = %d, want 6", list.Count)
	}
	want := map[string][2]string{
		"a-healthy": {"running", "healthy"}, "b-unhealthy": {"running", "unhealthy"},
		"c-failed": {"failed", "not_configured"}, "d-unplaced": {"pending", "unknown"},
		"e-stale": {"unknown", "unknown"}, "f-old-agent": {"unknown", "unknown"},
	}
	for _, item := range list.Items {
		if got := [2]string{item.State, item.Health}; got != want[item.Name] {
			t.Errorf("%s state/health = %v, want %v", item.Name, got, want[item.Name])
		}
	}
	if list.Items[5].ReasonCode != "node_down" {
		t.Fatalf("down-node service reason = %q", list.Items[5].ReasonCode)
	}
	if list.Items[0].Name != "a-healthy" || list.Items[5].Name != "f-old-agent" {
		t.Fatalf("services are not deterministically sorted: %#v", list.Items)
	}

	nodes, err := service.Nodes(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	states := make(map[string]string)
	for _, node := range nodes.Items {
		states[node.NodeID] = node.State
	}
	if nodes.Count != 3 || nodes.Items[0].Allocated.VCPUs != 4 || nodes.Items[0].Available.VCPUs != 4 || states["node-stale"] != "stale" {
		t.Fatalf("unexpected node aggregation: %#v", nodes.Items)
	}

	unhealthy, err := service.Services(ctx, "running", "unhealthy", "node-1")
	if err != nil || unhealthy.Count != 1 || unhealthy.Items[0].Name != "b-unhealthy" {
		t.Fatalf("filters returned %#v, err %v", unhealthy, err)
	}

	detail, found, err := service.Service(ctx, "a-healthy")
	if err != nil || !found {
		t.Fatalf("service detail found=%v err=%v", found, err)
	}
	if detail.DesiredImage != "a.ext4" || detail.DesiredKernel != "vmlinux" || detail.AppliedRevision != "rendered-1" {
		t.Fatalf("unexpected detail: %#v", detail)
	}
}

func TestVisibilityStaleNodeRecovery(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	cfg := validConfigForRole(RoleAPI)
	cfg.NodeStaleTTL = time.Minute
	now := time.Now().UTC()
	desired := DesiredRevision{Revision: "d", Services: []config.ServiceConfig{{Name: "service", HealthCheck: &config.HealthCheckConfig{Type: "http"}}}}
	placement := PlacementRevision{Revision: "p", NodeConfigs: []config.NodeConfig{{Node: "node", Services: desired.Services}}}
	putCurrentState(t, ctx, store, desired, placement, "r")
	putNode(t, ctx, store, cfg, NodeRecord{NodeID: "node", State: NodeStateReady, LastSeenAt: now.Add(-2 * time.Minute)})
	service := NewVisibilityService(cfg, store)
	before, err := service.Services(ctx, "", "", "")
	if err != nil || before.Items[0].State != "unknown" {
		t.Fatalf("before recovery = %#v, err %v", before, err)
	}
	putNode(t, ctx, store, cfg, NodeRecord{NodeID: "node", State: NodeStateReady, LastSeenAt: time.Now().UTC(), AgentStatus: &statusmodel.AgentStatus{SchemaVersion: 1, ObservedAt: time.Now().UTC(), Services: []statusmodel.ServiceStatus{{Name: "service", VMState: "running", Health: "healthy"}}}})
	after, err := service.Services(ctx, "", "", "")
	if err != nil || after.Items[0].State != "running" || after.Items[0].Health != "healthy" {
		t.Fatalf("after recovery = %#v, err %v", after, err)
	}
}

func putCurrentState(t *testing.T, ctx context.Context, store StateStore, desired DesiredRevision, placement PlacementRevision, rendered string) {
	t.Helper()
	for key, value := range map[string]any{
		desiredRevisionKey("cp/v1", desired.Revision):     desired,
		desiredCurrentKey("cp/v1"):                        RevisionPointer{Revision: desired.Revision},
		placementRevisionKey("cp/v1", placement.Revision): placement,
		placementCurrentKey("cp/v1"):                      RevisionPointer{Revision: placement.Revision},
		renderedCurrentKey("cp/v1"):                       RevisionPointer{Revision: rendered},
	} {
		if _, err := store.PutJSON(ctx, key, value); err != nil {
			t.Fatal(err)
		}
	}
}

func putNode(t *testing.T, ctx context.Context, store StateStore, cfg Config, record NodeRecord) {
	t.Helper()
	key, err := nodeRecordKey(cfg.State.Prefix, record.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(ctx, key, record); err != nil {
		t.Fatal(err)
	}
}
