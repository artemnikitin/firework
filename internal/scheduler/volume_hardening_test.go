package scheduler

import (
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

// exhaustedNode has a pool whose retained reservations already exceed its
// configured capacity — the state an oversized `size:` edit produces, and the
// one that used to make the node reject every workload on it.
func exhaustedNode() ([]Node, StorageReservations) {
	nodes := []Node{{
		InstanceID: "i-1", CapacityVCPUs: 8, CapacityMemMB: 8192,
		LocalCapacityBytes: 100 * config.MiB,
	}}
	reservations := StorageReservations{
		LocalByNode:        map[string]int64{"i-1": 500 * config.MiB},
		SharedByBackend:    map[string]int64{},
		RecordedLogicalIDs: map[string]bool{"kept/data": true},
	}
	return nodes, reservations
}

func localVolumeService(name string, size int64) config.ServiceConfig {
	return config.ServiceConfig{
		Name: name, VCPUs: 1, MemoryMB: 512,
		Volumes: []config.VolumeConfig{{
			Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data", SizeBytes: size,
		}},
	}
}

func pendingReason(pending []Pending, service string) string {
	for _, item := range pending {
		if item.Service == service {
			return item.ReasonCode
		}
	}
	return ""
}

func placedOn(assignments map[string][]config.ServiceConfig, service string) string {
	for node, services := range assignments {
		for _, placed := range services {
			if placed.Name == service {
				return node
			}
		}
	}
	return ""
}

// A node whose retained reservations exceed its pool must still run the
// workloads that do not add to those reservations. Rejecting them cannot
// recover a single byte; it can only evict.
func TestOverReservedNodeStillAcceptsServicesThatAddNoReservation(t *testing.T) {
	nodes, reservations := exhaustedNode()
	services := []config.ServiceConfig{
		{Name: "stateless", VCPUs: 1, MemoryMB: 512},
		localVolumeService("kept", 16*config.MiB),
	}
	existing := map[string]string{"stateless": "i-1", "kept": "i-1"}

	assignments, pending := ScheduleWithStorage(services, nodes, existing, reservations, nil)

	if len(pending) != 0 {
		t.Fatalf("expected no pending services, got %#v", pending)
	}
	if placedOn(assignments, "stateless") != "i-1" || placedOn(assignments, "kept") != "i-1" {
		t.Fatalf("expected both services to stay on i-1, got %#v", assignments)
	}
}

// The capacity guard still holds for a genuinely new allocation, and now says
// which of the two very different storage causes applied.
func TestStorageRejectionReasonsAreDistinct(t *testing.T) {
	nodes, reservations := exhaustedNode()
	// An active node with no local pool at all: a volume bound there cannot
	// bind, which is a placement fact rather than a capacity one.
	nodes = append(nodes, Node{InstanceID: "i-poolless", CapacityVCPUs: 8, CapacityMemMB: 8192})

	tests := []struct {
		name    string
		service config.ServiceConfig
		want    string
	}{
		{
			name:    "new allocation on a full pool",
			service: localVolumeService("fresh", 16*config.MiB),
			want:    ReasonNodeStorageExhausted,
		},
		{
			name: "bound node has no local pool configured",
			service: func() config.ServiceConfig {
				svc := localVolumeService("elsewhere", 16*config.MiB)
				svc.Volumes[0].BoundNode = "i-poolless"
				return svc
			}(),
			want: ReasonVolumeCapacityUnavailable,
		},
		{
			name:    "compute only",
			service: config.ServiceConfig{Name: "huge", VCPUs: 64, MemoryMB: 65536},
			want:    ReasonInsufficientCompute,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, pending := ScheduleWithStorage([]config.ServiceConfig{test.service}, nodes, nil, reservations, nil)
			if got := pendingReason(pending, test.service.Name); got != test.want {
				t.Fatalf("expected reason %q, got %q (%#v)", test.want, got, pending)
			}
		})
	}
}

// A quarantined record whose reservation could not be read makes the node's
// remaining pool unknowable, so new volume-bearing placement waits for the
// repair rather than being allocated against capacity that may be occupied.
// Existing placements and stateless services are untouched.
func TestUnknownCapacityBlocksOnlyNewVolumeBearingPlacement(t *testing.T) {
	nodes, reservations := exhaustedNode()
	reservations.LocalByNode = map[string]int64{"i-1": 16 * config.MiB}
	reservations.LocalUnknownByNode = map[string]bool{"i-1": true}

	services := []config.ServiceConfig{
		{Name: "stateless", VCPUs: 1, MemoryMB: 512},
		localVolumeService("kept", 16*config.MiB),
		localVolumeService("fresh", 16*config.MiB),
	}
	assignments, pending := ScheduleWithStorage(services, nodes, map[string]string{"kept": "i-1"}, reservations, nil)

	if placedOn(assignments, "stateless") == "" || placedOn(assignments, "kept") == "" {
		t.Fatalf("existing and stateless workloads must keep running: %#v (%#v)", assignments, pending)
	}
	if got := pendingReason(pending, "fresh"); got != ReasonStorageCapacityUnknown {
		t.Fatalf("expected %q for a new volume-bearing service, got %q", ReasonStorageCapacityUnknown, got)
	}
}

// The eviction path end to end: an oversized size: edit raises the node's
// reservations above its pool, and every service on it must still be rendered.
// BuildNodeConfigs dropping a node's services is what the agent turns into a
// delete for each one, so an empty render here is the eviction itself.
func TestOversizedReservationDoesNotEmptyTheRenderedNodeConfig(t *testing.T) {
	nodes := []Node{{
		InstanceID: "i-1", CapacityVCPUs: 8, CapacityMemMB: 8192,
		LocalCapacityBytes: 100 * config.MiB,
	}}
	// The operator edited size: to something far above the pool; the record
	// was already retained, so it contributes its inflated reservation.
	reservations := StorageReservations{
		LocalByNode:        map[string]int64{"i-1": 900 * config.MiB},
		SharedByBackend:    map[string]int64{},
		RecordedLogicalIDs: map[string]bool{"db/data": true},
	}
	services := []config.ServiceConfig{
		{Name: "web", VCPUs: 1, MemoryMB: 512},
		{Name: "api", VCPUs: 1, MemoryMB: 512},
		localVolumeService("db", 900*config.MiB),
	}
	existing := map[string]string{"web": "i-1", "api": "i-1", "db": "i-1"}

	assignments, pending := ScheduleWithStorage(services, nodes, existing, reservations, nil)
	if len(pending) != 0 {
		t.Fatalf("expected no service to be evicted, got %#v", pending)
	}
	rendered := BuildNodeConfigs(assignments)
	if len(rendered) != 1 || len(rendered[0].Services) != 3 {
		t.Fatalf("expected all three services rendered for i-1, got %#v", rendered)
	}
}
