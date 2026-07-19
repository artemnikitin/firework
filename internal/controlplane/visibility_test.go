package controlplane

import (
	"context"
	"encoding/json"
	"strings"
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
		SchemaVersion: 1, AgentVersion: "test", ObservedAt: now, Phase: statusmodel.PhaseFailed,
		DesiredRevision: "desired-1", PlacementRevision: "placement-1", ObservedRevision: "rendered-1", AppliedRevision: "rendered-1", Services: []statusmodel.ServiceStatus{
			{Name: "a-healthy", VMState: "running", Health: "healthy", NetworkAddress: "172.16.0.2"},
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
	if detail.DesiredImage != "a.ext4" || detail.DesiredKernel != "vmlinux" || detail.AppliedRevision != "rendered-1" || detail.NetworkAddress != "172.16.0.2" {
		t.Fatalf("unexpected detail: %#v", detail)
	}
	if detail.ServiceObservedAt.IsZero() || !detail.ServiceObservedAt.Equal(now) {
		t.Fatalf("service observation timestamp = %v, want %v", detail.ServiceObservedAt, now)
	}
}

func TestServiceDetailJSONUsesStableFieldNames(t *testing.T) {
	detail := ServiceDetail{
		ObservedAt:        time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC),
		ServiceObservedAt: time.Date(2025, time.December, 31, 12, 0, 0, 0, time.UTC),
		PortForwards: []config.PortForward{{
			HostPort: 8080,
			VMPort:   80,
		}},
	}
	payload, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(payload)
	for _, field := range []string{`"observed_at"`, `"service_observed_at"`, `"host_port"`, `"vm_port"`} {
		if !strings.Contains(encoded, field) {
			t.Errorf("JSON payload does not contain %s: %s", field, encoded)
		}
	}
	for _, field := range []string{`"HostPort"`, `"VMPort"`} {
		if strings.Contains(encoded, field) {
			t.Errorf("JSON payload contains unstable field %s: %s", field, encoded)
		}
	}
}

func TestServiceDetailUsesRetainedVolumeRecordWhilePlacementIsPending(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	cfg := validConfigForRole(RoleAPI)
	now := time.Now().UTC()
	desired := DesiredRevision{Revision: "desired-1", CreatedAt: now, Services: []config.ServiceConfig{{
		Name: "db", Volumes: []config.VolumeConfig{{
			Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data", SizeBytes: 20 * config.GiB,
		}},
	}}}
	placement := PlacementRevision{Revision: "placement-1", DesiredRevision: desired.Revision, CreatedAt: now, PendingServices: []PendingPlacement{{
		Service: "db", ReasonCode: "local_volume_node_unavailable", Message: "bound node unavailable",
	}}}
	putCurrentState(t, ctx, store, desired, placement, "rendered-1")
	record := VolumeRecord{
		LogicalID: "db/data", Type: config.VolumeTypeLocal, BoundNode: "node-gone",
		DesiredSizeBytes: 20 * config.GiB, AppliedSizeBytes: 10 * config.GiB,
		ResizeGeneration: 2, ResizeState: VolumeResizePending,
	}
	if _, err := store.PutJSON(ctx, mustVolumeRecordKey(cfg.State.Prefix, "db", "data"), record); err != nil {
		t.Fatal(err)
	}

	detail, found, err := NewVisibilityService(cfg, store).Service(ctx, "db")
	if err != nil || !found {
		t.Fatalf("service detail found=%v err=%v", found, err)
	}
	if len(detail.Volumes) != 1 || detail.Volumes[0].BoundNode != "node-gone" || detail.Volumes[0].AppliedSizeBytes != 10*config.GiB || detail.Volumes[0].ResizeGeneration != 2 {
		t.Fatalf("volume status did not use retained record: %#v", detail.Volumes)
	}
}

func TestNodeStorageSummariesUseDurableReservations(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	cfg := validConfigForRole(RoleAPI)
	now := time.Now().UTC()
	putCurrentState(t, ctx, store, DesiredRevision{Revision: "desired-1"}, PlacementRevision{Revision: "placement-1", DesiredRevision: "desired-1"}, "rendered-1")
	putNode(t, ctx, store, cfg, NodeRecord{
		NodeID: "node-1", State: NodeStateReady, LastSeenAt: now,
		Storage: StorageResources{LocalCapacityBytes: 100 * config.GiB, SharedBackendID: "shared-a", SharedCapacityBytes: 200 * config.GiB},
	})
	for _, record := range []VolumeRecord{
		{
			LogicalID: "removed/data", Type: config.VolumeTypeLocal, BoundNode: "node-1",
			DesiredSizeBytes: 20 * config.GiB, AppliedSizeBytes: 30 * config.GiB,
		},
		{
			LogicalID: "other/data", Type: config.VolumeTypeLocal, BoundNode: "node-2",
			DesiredSizeBytes: 50 * config.GiB,
		},
		{
			LogicalID: "shared/data", Type: config.VolumeTypeShared, SharedBackendID: "shared-a",
			DesiredSizeBytes: 60 * config.GiB,
		},
		{
			LogicalID: "shared-other/data", Type: config.VolumeTypeShared, SharedBackendID: "shared-b",
			DesiredSizeBytes: 80 * config.GiB,
		},
	} {
		parts := strings.Split(record.LogicalID, "/")
		if _, err := store.PutJSON(ctx, mustVolumeRecordKey(cfg.State.Prefix, parts[0], parts[1]), record); err != nil {
			t.Fatal(err)
		}
	}

	nodes, err := NewVisibilityService(cfg, store).Nodes(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes.Items) != 1 {
		t.Fatalf("nodes = %#v", nodes.Items)
	}
	storage := nodes.Items[0].Storage
	if storage.Local.CapacityBytes != 100*config.GiB || storage.Local.AllocatedBytes != 30*config.GiB || storage.Local.AvailableBytes != 70*config.GiB {
		t.Fatalf("local storage = %#v", storage.Local)
	}
	if storage.SharedBackendID != "shared-a" || storage.Shared.CapacityBytes != 200*config.GiB || storage.Shared.AllocatedBytes != 60*config.GiB || storage.Shared.AvailableBytes != 140*config.GiB {
		t.Fatalf("shared storage = %#v", storage)
	}
}

func TestServiceStorageSummaryAndDetailPreserveDurableVolumes(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	cfg := validConfigForRole(RoleAPI)
	cfg.NodeStaleTTL = time.Minute
	now := time.Now().UTC()
	serviceConfig := config.ServiceConfig{Name: "db", Volumes: []config.VolumeConfig{
		{Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data", SizeBytes: 20 * config.GiB, BoundNode: "node-1", ResizeGeneration: 2},
		{Name: "shared", Type: config.VolumeTypeShared, MountPath: "/shared", SizeBytes: 10 * config.GiB, SharedBackendID: "shared-a", ResizeGeneration: 1},
	}}
	desired := DesiredRevision{Revision: "desired-1", Services: []config.ServiceConfig{serviceConfig}}
	placement := PlacementRevision{Revision: "placement-1", DesiredRevision: desired.Revision, NodeConfigs: []config.NodeConfig{{Node: "node-1", Services: []config.ServiceConfig{serviceConfig}}}}
	putCurrentState(t, ctx, store, desired, placement, "rendered-1")
	for _, record := range []VolumeRecord{
		{LogicalID: "db/data", Type: config.VolumeTypeLocal, BoundNode: "node-1", DesiredSizeBytes: 20 * config.GiB, AppliedSizeBytes: 15 * config.GiB, ResizeGeneration: 2, ResizeState: VolumeResizePending},
		{LogicalID: "db/shared", Type: config.VolumeTypeShared, SharedBackendID: "shared-a", DesiredSizeBytes: 10 * config.GiB, AppliedSizeBytes: 10 * config.GiB, ResizeGeneration: 1, ResizeState: VolumeResizeApplied},
	} {
		parts := strings.Split(record.LogicalID, "/")
		if _, err := store.PutJSON(ctx, mustVolumeRecordKey(cfg.State.Prefix, parts[0], parts[1]), record); err != nil {
			t.Fatal(err)
		}
	}
	putNode(t, ctx, store, cfg, NodeRecord{NodeID: "node-1", State: NodeStateReady, LastSeenAt: now, AgentStatus: &statusmodel.AgentStatus{
		SchemaVersion: 1, ObservedAt: now, DesiredRevision: "desired-1", PlacementRevision: "placement-1", ObservedRevision: "rendered-1", AppliedRevision: "rendered-1",
		Services: []statusmodel.ServiceStatus{{Name: "db", VMState: "running", Health: "not_configured", Volumes: []statusmodel.VolumeStatus{{
			LogicalID: "db/data", Type: string(config.VolumeTypeLocal), MountPath: "/data", BoundNode: "node-1",
			DesiredSizeBytes: 20 * config.GiB, AppliedSizeBytes: 20 * config.GiB, ResizeGeneration: 2, State: "prepared",
		}}}},
	}})

	visibility := NewVisibilityService(cfg, store)
	services, err := visibility.Services(ctx, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	storage := services.Items[0].Storage
	if storage.Local.Count != 1 || storage.Local.DesiredBytes != 20*config.GiB || storage.Local.AppliedBytes != 15*config.GiB || storage.Local.AllocatedBytes != 20*config.GiB {
		t.Fatalf("local service storage = %#v", storage.Local)
	}
	if storage.Shared.Count != 1 || storage.Shared.AppliedBytes != 10*config.GiB || storage.Shared.AllocatedBytes != 10*config.GiB {
		t.Fatalf("shared service storage = %#v", storage.Shared)
	}

	detail, found, err := visibility.Service(ctx, "db")
	if err != nil || !found {
		t.Fatalf("service detail found=%v err=%v", found, err)
	}
	if len(detail.Volumes) != 2 {
		t.Fatalf("partial agent observation erased durable volumes: %#v", detail.Volumes)
	}
	if detail.Storage.Local.AppliedBytes != 20*config.GiB || detail.Storage.Shared.AppliedBytes != 10*config.GiB {
		t.Fatalf("detail storage did not merge agent and durable state: %#v", detail.Storage)
	}
}

func TestServiceDetailPublicURL(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	cfg := validConfigForRole(RoleAPI)
	cfg.IngressDomain = "gcp.example.com"
	desired := DesiredRevision{Revision: "desired-1", Services: []config.ServiceConfig{
		{Name: "portable", Metadata: map[string]string{"subdomain": "tenant-1"}},
		{Name: "exact", Metadata: map[string]string{"host": "Custom.Example.net."}},
		{Name: "private"},
	}}
	putCurrentState(t, ctx, store, desired, PlacementRevision{Revision: "placement-1", DesiredRevision: "desired-1"}, "rendered-1")

	service := NewVisibilityService(cfg, store)
	for name, want := range map[string][2]string{
		"portable": {"tenant-1.gcp.example.com", "https://tenant-1.gcp.example.com"},
		"exact":    {"custom.example.net", "https://custom.example.net"},
		"private":  {"", ""},
	} {
		detail, found, err := service.Service(ctx, name)
		if err != nil || !found {
			t.Fatalf("service %s found=%v err=%v", name, found, err)
		}
		if got := [2]string{detail.RoutingHostname, detail.PublicURL}; got != want {
			t.Errorf("service %s route = %v, want %v", name, got, want)
		}
	}
}

func TestVisibilityStaleNodeRecovery(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	cfg := validConfigForRole(RoleAPI)
	cfg.NodeStaleTTL = time.Minute
	now := time.Now().UTC()
	desired := DesiredRevision{Revision: "d", Services: []config.ServiceConfig{{Name: "service", HealthCheck: &config.HealthCheckConfig{Type: "http"}}}}
	placement := PlacementRevision{Revision: "p", DesiredRevision: "d", NodeConfigs: []config.NodeConfig{{Node: "node", Services: desired.Services}}}
	putCurrentState(t, ctx, store, desired, placement, "r")
	putNode(t, ctx, store, cfg, NodeRecord{NodeID: "node", State: NodeStateReady, LastSeenAt: now.Add(-2 * time.Minute)})
	service := NewVisibilityService(cfg, store)
	before, err := service.Services(ctx, "", "", "")
	if err != nil || before.Items[0].State != "unknown" {
		t.Fatalf("before recovery = %#v, err %v", before, err)
	}
	putNode(t, ctx, store, cfg, NodeRecord{NodeID: "node", State: NodeStateReady, LastSeenAt: time.Now().UTC(), AgentStatus: &statusmodel.AgentStatus{SchemaVersion: 1, ObservedAt: time.Now().UTC(), DesiredRevision: "d", PlacementRevision: "p", ObservedRevision: "r", AppliedRevision: "r", Services: []statusmodel.ServiceStatus{{Name: "service", VMState: "running", Health: "healthy"}}}})
	after, err := service.Services(ctx, "", "", "")
	if err != nil || after.Items[0].State != "running" || after.Items[0].Health != "healthy" {
		t.Fatalf("after recovery = %#v, err %v", after, err)
	}
}

func TestVisibilityFailsClosedAcrossRevisionTransitions(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	cfg := validConfigForRole(RoleAPI)
	cfg.NodeStaleTTL = time.Minute
	now := time.Now().UTC()
	desired := DesiredRevision{Revision: "desired-2", Services: []config.ServiceConfig{{Name: "service", VCPUs: 1, MemoryMB: 128}}}
	oldPlacement := PlacementRevision{Revision: "placement-1", DesiredRevision: "desired-1", NodeConfigs: []config.NodeConfig{{Node: "node", Services: desired.Services}}}
	putCurrentState(t, ctx, store, desired, oldPlacement, "rendered-1")
	putNode(t, ctx, store, cfg, NodeRecord{NodeID: "node", State: NodeStateReady, LastSeenAt: now, AgentStatus: &statusmodel.AgentStatus{
		SchemaVersion: 1, ObservedAt: now, DesiredRevision: "desired-1", PlacementRevision: "placement-1", ObservedRevision: "rendered-1", AppliedRevision: "rendered-1",
		Services: []statusmodel.ServiceStatus{{Name: "service", VMState: "running", Health: "healthy"}},
	}})

	service := NewVisibilityService(cfg, store)
	list, err := service.Services(ctx, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := list.Items[0]; got.State != "pending" || got.Health != "unknown" || got.Node != "" || got.ReasonCode != "placement_pending" {
		t.Fatalf("mismatched placement did not fail closed: %#v", got)
	}

	currentPlacement := PlacementRevision{Revision: "placement-2", DesiredRevision: "desired-2", NodeConfigs: []config.NodeConfig{{Node: "node", Services: desired.Services}}}
	putCurrentState(t, ctx, store, desired, currentPlacement, "rendered-2")
	putNode(t, ctx, store, cfg, NodeRecord{NodeID: "node", State: NodeStateReady, LastSeenAt: now, AgentStatus: &statusmodel.AgentStatus{
		SchemaVersion: 1, ObservedAt: now, DesiredRevision: "desired-2", PlacementRevision: "placement-2", ObservedRevision: "rendered-2", AppliedRevision: "rendered-1",
		Services: []statusmodel.ServiceStatus{{Name: "service", VMState: "running", Health: "healthy"}},
	}})
	list, err = service.Services(ctx, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := list.Items[0]; got.State != "unknown" || got.Health != "unknown" || got.Node != "node" || got.ReasonCode != "agent_status_revision_mismatch" {
		t.Fatalf("unapplied agent status did not fail closed: %#v", got)
	}
	detail, found, err := service.Service(ctx, "service")
	if err != nil || !found {
		t.Fatalf("service detail found=%v err=%v", found, err)
	}
	if detail.ActualNode != "" || detail.AppliedRevision != "" || detail.PID != 0 {
		t.Fatalf("unapplied runtime fields leaked into service detail: %#v", detail)
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
