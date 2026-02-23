package scheduler

import (
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

func svc(name string, vcpus, memMB int) config.ServiceConfig {
	return config.ServiceConfig{Name: name, VCPUs: vcpus, MemoryMB: memMB}
}

func node(id string, capVCPUs, capMemMB int) Node {
	return Node{InstanceID: id, CapacityVCPUs: capVCPUs, CapacityMemMB: capMemMB}
}

func TestSchedule_PlacesServiceOnNodeWithSufficientCapacity(t *testing.T) {
	services := []config.ServiceConfig{svc("a", 2, 512)}
	nodes := []Node{node("i-001", 8, 4096)}

	result, err := Schedule(services, nodes, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result["i-001"]) != 1 || result["i-001"][0].Name != "a" {
		t.Errorf("expected service a on i-001, got %+v", result)
	}
}

func TestSchedule_ReturnsErrorWhenNoCapacity(t *testing.T) {
	services := []config.ServiceConfig{svc("heavy", 64, 65536)}
	nodes := []Node{node("i-001", 8, 4096)}

	_, err := Schedule(services, nodes, nil)
	if err == nil {
		t.Fatal("expected error when no node has capacity")
	}
}

func TestSchedule_ReturnsErrorWhenNoNodes(t *testing.T) {
	services := []config.ServiceConfig{svc("a", 1, 256)}

	_, err := Schedule(services, nil, nil)
	if err == nil {
		t.Fatal("expected error when no nodes available")
	}
}

func TestSchedule_EmptyServicesEmptyResult(t *testing.T) {
	nodes := []Node{node("i-001", 8, 4096)}

	result, err := Schedule(nil, nodes, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// i-001 entry exists but has no services.
	if svcs, ok := result["i-001"]; ok && len(svcs) != 0 {
		t.Errorf("expected empty assignment, got %v", svcs)
	}
}

func TestSchedule_RespectsExistingPlacement(t *testing.T) {
	services := []config.ServiceConfig{
		svc("a", 1, 256),
		svc("b", 1, 256),
	}
	nodes := []Node{
		node("i-001", 4, 2048),
		node("i-002", 4, 2048),
	}
	existing := map[string]string{
		"a": "i-002",
		"b": "i-001",
	}

	result, err := Schedule(services, nodes, existing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result["i-002"]) != 1 || result["i-002"][0].Name != "a" {
		t.Errorf("expected a to stay on i-002")
	}
	if len(result["i-001"]) != 1 || result["i-001"][0].Name != "b" {
		t.Errorf("expected b to stay on i-001")
	}
}

func TestSchedule_RebalancesWhenExistingNodeDisappears(t *testing.T) {
	services := []config.ServiceConfig{svc("a", 1, 256)}
	// Node i-old is gone.
	nodes := []Node{node("i-new", 4, 2048)}
	existing := map[string]string{"a": "i-old"}

	result, err := Schedule(services, nodes, existing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result["i-new"]) != 1 || result["i-new"][0].Name != "a" {
		t.Errorf("expected a to be placed on i-new, got %+v", result)
	}
}

func TestSchedule_RebalancesWhenExistingNodeLacksCapacity(t *testing.T) {
	services := []config.ServiceConfig{
		svc("a", 2, 512),
		svc("b", 2, 512), // already filling i-001
	}
	nodes := []Node{
		node("i-001", 2, 512), // only fits one service
		node("i-002", 4, 2048),
	}
	// Both were on i-001 previously — only one can stay.
	existing := map[string]string{
		"a": "i-001",
		"b": "i-001",
	}

	result, err := Schedule(services, nodes, existing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	totalA := len(result["i-001"]) + len(result["i-002"])
	if totalA != 2 {
		t.Errorf("expected 2 services placed, got %d", totalA)
	}
}

func TestSchedule_BestFitPrefersNodeWithMostFreeCapacity(t *testing.T) {
	services := []config.ServiceConfig{svc("a", 1, 256)}
	nodes := []Node{
		node("i-small", 2, 512),
		node("i-large", 8, 4096),
	}

	result, err := Schedule(services, nodes, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result["i-large"]) != 1 {
		t.Errorf("expected service placed on i-large (most free capacity), got %+v", result)
	}
}

func TestSchedule_AntiAffinity_SpreadAcrossNodes(t *testing.T) {
	services := []config.ServiceConfig{
		{Name: "es-1", VCPUs: 2, MemoryMB: 512, AntiAffinityGroup: "es"},
		{Name: "es-2", VCPUs: 2, MemoryMB: 512, AntiAffinityGroup: "es"},
	}
	nodes := []Node{
		node("i-001", 8, 4096),
		node("i-002", 8, 4096),
	}

	result, err := Schedule(services, nodes, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Each node should have exactly one service (anti-affinity spreads them).
	if len(result["i-001"]) != 1 || len(result["i-002"]) != 1 {
		t.Errorf("expected one service per node due to anti-affinity, got i-001=%d i-002=%d",
			len(result["i-001"]), len(result["i-002"]))
	}
}

func TestSchedule_AntiAffinity_FallbackWhenSingleNode(t *testing.T) {
	services := []config.ServiceConfig{
		{Name: "es-1", VCPUs: 2, MemoryMB: 512, AntiAffinityGroup: "es"},
		{Name: "es-2", VCPUs: 2, MemoryMB: 512, AntiAffinityGroup: "es"},
	}
	nodes := []Node{node("i-001", 16, 8192)}

	result, err := Schedule(services, nodes, nil)
	if err != nil {
		t.Fatalf("unexpected error (should fall back to same node): %v", err)
	}

	// Both services should land on the single available node.
	if len(result["i-001"]) != 2 {
		t.Errorf("expected both services on i-001, got %d", len(result["i-001"]))
	}
}

func TestSchedule_AntiAffinity_RebalancesViolatingExistingAssignment(t *testing.T) {
	// Simulate: both ES data nodes were placed on i-001 when it was the only node.
	// Now i-002 has joined. Phase 1 should re-check anti-affinity and move the
	// second service to i-002.
	services := []config.ServiceConfig{
		{Name: "es-1", VCPUs: 4, MemoryMB: 4096, AntiAffinityGroup: "es-tenant-3"},
		{Name: "es-2", VCPUs: 4, MemoryMB: 4096, AntiAffinityGroup: "es-tenant-3"},
	}
	nodes := []Node{
		node("i-001", 64, 65536),
		node("i-002", 64, 65536),
	}
	// Both were co-located on i-001 previously (anti-affinity violation).
	existing := map[string]string{
		"es-1": "i-001",
		"es-2": "i-001",
	}

	result, err := Schedule(services, nodes, existing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Each node should now host exactly one service.
	if len(result["i-001"]) != 1 || len(result["i-002"]) != 1 {
		t.Errorf("expected anti-affinity rebalance to spread services across nodes, got i-001=%d i-002=%d",
			len(result["i-001"]), len(result["i-002"]))
	}
}

func TestBuildNodeConfigs_SkipsEmptyNodes(t *testing.T) {
	assignment := map[string][]config.ServiceConfig{
		"i-001": {svc("a", 1, 256)},
		"i-002": nil, // no services
	}

	ncs := BuildNodeConfigs(assignment)
	if len(ncs) != 1 {
		t.Errorf("expected 1 NodeConfig (empty nodes skipped), got %d", len(ncs))
	}
	if ncs[0].Node != "i-001" {
		t.Errorf("expected i-001, got %s", ncs[0].Node)
	}
}

func TestBuildNodeConfigs_DeterministicOrder(t *testing.T) {
	assignment := map[string][]config.ServiceConfig{
		"i-003": {svc("c", 1, 256)},
		"i-001": {svc("a", 1, 256)},
		"i-002": {svc("b", 1, 256)},
	}

	ncs := BuildNodeConfigs(assignment)
	if len(ncs) != 3 {
		t.Fatalf("expected 3, got %d", len(ncs))
	}
	if ncs[0].Node != "i-001" || ncs[1].Node != "i-002" || ncs[2].Node != "i-003" {
		t.Errorf("expected sorted order, got %v %v %v", ncs[0].Node, ncs[1].Node, ncs[2].Node)
	}
}
