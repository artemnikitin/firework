package scheduler

import (
	"reflect"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

func typed(name, nodeType string, vcpus, memMB int) config.ServiceConfig {
	service := svc(name, vcpus, memMB)
	service.NodeType = nodeType
	return service
}

func labeled(id string, capVCPUs, capMemMB int, labels ...string) Node {
	n := node(id, capVCPUs, capMemMB)
	n.Labels = labels
	return n
}

func pendingFor(pending []Pending, service string) (Pending, bool) {
	for _, item := range pending {
		if item.Service == service {
			return item, true
		}
	}
	return Pending{}, false
}

func TestScheduleWithStorageNodeTypePlacement(t *testing.T) {
	tests := []struct {
		name        string
		service     config.ServiceConfig
		nodes       []Node
		existing    map[string]string
		wantNode    string
		wantReason  string
		wantMessage string
	}{
		{
			name:     "only a matching node is eligible even with less free capacity",
			service:  typed("worker", "compute", 1, 256),
			nodes:    []Node{labeled("big", 32, 65536, "general-purpose"), labeled("small", 2, 1024, "compute")},
			wantNode: "small",
		},
		{
			name:     "a node may advertise several labels",
			service:  typed("worker", "compute", 1, 256),
			nodes:    []Node{labeled("multi", 4, 4096, "backend", "compute", "web")},
			wantNode: "multi",
		},
		{
			name:        "no node advertises the label",
			service:     typed("worker", "compute", 1, 256),
			nodes:       []Node{labeled("a", 8, 8192, "web"), node("b", 8, 8192)},
			wantReason:  ReasonNoMatchingNodeType,
			wantMessage: `node_type "compute"`,
		},
		{
			name:        "matching node lacks vCPU while a non-matching node has room",
			service:     typed("worker", "compute", 4, 256),
			nodes:       []Node{labeled("compute-1", 2, 8192, "compute"), labeled("web-1", 32, 65536, "web")},
			wantReason:  ReasonInsufficientCompute,
			wantMessage: `node_type "compute"`,
		},
		{
			name:        "matching node lacks memory while a non-matching node has room",
			service:     typed("worker", "compute", 1, 4096),
			nodes:       []Node{labeled("compute-1", 8, 1024, "compute"), labeled("web-1", 32, 65536, "web")},
			wantReason:  ReasonInsufficientCompute,
			wantMessage: `node_type "compute"`,
		},
		{
			name:     "existing placement kept while the label still matches",
			service:  typed("worker", "compute", 1, 256),
			nodes:    []Node{labeled("a", 2, 1024, "compute"), labeled("b", 32, 65536, "compute")},
			existing: map[string]string{"worker": "a"},
			wantNode: "a",
		},
		{
			name:     "existing placement dropped after its node loses the label",
			service:  typed("worker", "compute", 1, 256),
			nodes:    []Node{labeled("a", 32, 65536, "web"), labeled("b", 2, 1024, "compute")},
			existing: map[string]string{"worker": "a"},
			wantNode: "b",
		},
		{
			name:       "existing placement pending after the only matching node loses the label",
			service:    typed("worker", "compute", 1, 256),
			nodes:      []Node{labeled("a", 32, 65536, "web")},
			existing:   map[string]string{"worker": "a"},
			wantReason: ReasonNoMatchingNodeType,
		},
		{
			name:     "empty node_type from a legacy revision is unconstrained",
			service:  typed("worker", "", 1, 256),
			nodes:    []Node{node("unlabeled", 4, 4096)},
			wantNode: "unlabeled",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, pending := ScheduleWithStorage([]config.ServiceConfig{tt.service}, tt.nodes, tt.existing, StorageReservations{}, nil)
			got := placedOn(result, tt.service.Name)
			item, isPending := pendingFor(pending, tt.service.Name)
			if tt.wantReason == "" {
				if got != tt.wantNode || isPending {
					t.Fatalf("placed on %q (pending %#v), want %q", got, pending, tt.wantNode)
				}
				return
			}
			if got != "" || !isPending || item.ReasonCode != tt.wantReason {
				t.Fatalf("placed on %q, pending %#v; want pending %s", got, pending, tt.wantReason)
			}
			if !strings.Contains(item.Message, tt.wantMessage) {
				t.Fatalf("message %q does not name the constraint %q", item.Message, tt.wantMessage)
			}
		})
	}
}

// Anti-affinity is a preference evaluated among eligible nodes only: it must
// never spread a service onto a node that lacks its required label.
func TestScheduleWithStorageAntiAffinityStaysWithinEligibleNodes(t *testing.T) {
	a := typed("es-1", "backend", 1, 256)
	b := typed("es-2", "backend", 1, 256)
	a.AntiAffinityGroup, b.AntiAffinityGroup = "es", "es"
	nodes := []Node{labeled("backend-1", 8, 8192, "backend"), labeled("web-1", 8, 8192, "web")}

	result, pending := ScheduleWithStorage([]config.ServiceConfig{a, b}, nodes, nil, StorageReservations{}, nil)
	if len(pending) != 0 || len(result["backend-1"]) != 2 || len(result["web-1"]) != 0 {
		t.Fatalf("expected both on backend-1, got result=%#v pending=%#v", result, pending)
	}
}

// A host-port conflict reason must describe a node the service may use, not
// a free port on a node it was never eligible for.
func TestScheduleWithStorageHostPortConflictAmongMatchingNodesOnly(t *testing.T) {
	first := withPorts("es-1", 1, 256, 9200)
	second := withPorts("es-2", 1, 256, 9200)
	first.NodeType, second.NodeType = "backend", "backend"
	nodes := []Node{labeled("backend-1", 8, 8192, "backend"), labeled("web-1", 8, 8192, "web")}

	result, pending := ScheduleWithStorage([]config.ServiceConfig{first, second}, nodes, nil, StorageReservations{}, nil)
	if placedOn(result, "es-1") != "backend-1" {
		t.Fatalf("es-1 not on backend-1: %#v", result)
	}
	if reason := pendingReason(pending, "es-2"); reason != ReasonHostPortConflict {
		t.Fatalf("es-2 reason = %q, want %s (pending %#v)", reason, ReasonHostPortConflict, pending)
	}
}

func TestScheduleWithStorageBoundLocalVolumeOnNodeWithoutLabel(t *testing.T) {
	service := localVolumeService("db", 10*config.MiB)
	service.NodeType = "backend"
	service.Volumes[0].BoundNode = "old"
	nodes := []Node{
		{InstanceID: "old", Labels: []string{"web"}, CapacityVCPUs: 8, CapacityMemMB: 8192, LocalCapacityBytes: config.GiB},
		{InstanceID: "new", Labels: []string{"backend"}, CapacityVCPUs: 8, CapacityMemMB: 8192, LocalCapacityBytes: config.GiB},
	}

	result, pending := ScheduleWithStorage([]config.ServiceConfig{service}, nodes, nil, StorageReservations{}, nil)
	item, ok := pendingFor(pending, "db")
	if placedOn(result, "db") != "" || !ok || item.ReasonCode != ReasonNoMatchingNodeType {
		t.Fatalf("expected db pending %s, got result=%#v pending=%#v", ReasonNoMatchingNodeType, result, pending)
	}
	if !strings.Contains(item.Message, "old") || !strings.Contains(item.Message, `"backend"`) {
		t.Fatalf("message %q must name the bound node and the missing node_type", item.Message)
	}
}

func TestScheduleWithStorageNodeTypeDeterministic(t *testing.T) {
	services := []config.ServiceConfig{
		typed("api", "web", 1, 256),
		typed("db", "backend", 2, 512),
		typed("ui", "web", 1, 256),
		typed("cache", "backend", 1, 256),
	}
	nodes := []Node{
		labeled("web-1", 4, 4096, "web"),
		labeled("web-2", 4, 4096, "web"),
		labeled("backend-1", 4, 4096, "backend"),
		labeled("backend-2", 4, 4096, "backend"),
	}
	want, pending := ScheduleWithStorage(services, nodes, nil, StorageReservations{}, nil)
	if len(pending) != 0 {
		t.Fatalf("unexpected pending: %#v", pending)
	}
	for _, service := range services {
		got := placedOn(want, service.Name)
		// Node IDs are prefixed with the single label each node advertises.
		if !strings.HasPrefix(got, service.NodeType+"-") {
			t.Fatalf("%s placed on %q, which does not advertise %q", service.Name, got, service.NodeType)
		}
	}

	reversedServices := append([]config.ServiceConfig(nil), services...)
	reversedNodes := append([]Node(nil), nodes...)
	for i, j := 0, len(reversedServices)-1; i < j; i, j = i+1, j-1 {
		reversedServices[i], reversedServices[j] = reversedServices[j], reversedServices[i]
	}
	for i, j := 0, len(reversedNodes)-1; i < j; i, j = i+1, j-1 {
		reversedNodes[i], reversedNodes[j] = reversedNodes[j], reversedNodes[i]
	}
	got, _ := ScheduleWithStorage(reversedServices, reversedNodes, nil, StorageReservations{}, nil)
	if !reflect.DeepEqual(BuildNodeConfigs(got), BuildNodeConfigs(want)) {
		t.Fatalf("placement depends on input order:\n got %#v\nwant %#v", BuildNodeConfigs(got), BuildNodeConfigs(want))
	}
}

func TestScheduleNodeType(t *testing.T) {
	nodes := []Node{labeled("web-1", 32, 65536, "web"), labeled("compute-1", 2, 1024, "compute")}

	result, err := Schedule([]config.ServiceConfig{typed("worker", "compute", 1, 256)}, nodes, map[string]string{"worker": "web-1"})
	if err != nil {
		t.Fatal(err)
	}
	if placedOn(result, "worker") != "compute-1" {
		t.Fatalf("worker must leave web-1, which lacks the compute label: %#v", result)
	}

	_, err = Schedule([]config.ServiceConfig{typed("worker", "gpu", 1, 256)}, nodes, nil)
	if err == nil || !strings.Contains(err.Error(), `"gpu"`) || !strings.Contains(err.Error(), "worker") {
		t.Fatalf("error must name the service and missing node_type, got %v", err)
	}

	_, err = Schedule([]config.ServiceConfig{typed("worker", "compute", 4, 256)}, nodes, nil)
	if err == nil || !strings.Contains(err.Error(), `node_type "compute"`) {
		t.Fatalf("capacity error must name the node_type, got %v", err)
	}
}
