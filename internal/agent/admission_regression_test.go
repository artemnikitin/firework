package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/statusmodel"
	"github.com/artemnikitin/firework/internal/vm"
)

func admissionAgent(t *testing.T, yaml string, limit capacity.NodeCapacity, instances map[string]*vm.Instance, subnet string) (*Agent, *recoveryTestVMManager) {
	t.Helper()
	cfg := testAgentConfig(t)
	enabled := true
	cfg.EnableCapacityCheck = &enabled
	if subnet != "" {
		cfg.VMSubnet, cfg.VMGateway = subnet, "172.16.0.1"
	}
	a := New(cfg, &fakeStore{revision: "r2", data: map[string][]byte{"web": []byte(yaml)}}, testLogger())
	a.capacityReader = &fakeCapacityReader{cap: limit}
	m := &recoveryTestVMManager{instances: instances}
	a.reconciler = reconciler.NewWithNetworkManager(m, testLogger(), nil, &recoveryTestNetworkManager{}, "", 0)
	return a, m
}

func admissionService(name string, vcpus, memoryMB, port int, ip string) config.ServiceConfig {
	s := config.ServiceConfig{Name: name, Image: "/" + name, Kernel: "/k", VCPUs: vcpus, MemoryMB: memoryMB}
	if port != 0 {
		s.PortForwards = []config.PortForward{{HostPort: port, VMPort: 80}}
	}
	if ip != "" {
		s.Network = &config.NetworkConfig{GuestIP: ip}
	}
	return s
}

// A running VM whose growth is held keeps its current size: a new service is
// not admitted into space it still uses, and the heartbeat reports it.
func TestHeldGrowthKeepsCurrentSize(t *testing.T) {
	yaml := "node: web\nservices:\n- name: a\n  image: /a\n  kernel: /k\n  vcpus: 3\n  memory_mb: 64\n- name: b\n  image: /b\n  kernel: /k\n  vcpus: 1\n  memory_mb: 64\n"
	a, m := admissionAgent(t, yaml, capacity.NodeCapacity{VCPUs: 4, MemoryMB: 4096}, map[string]*vm.Instance{
		"a": {Name: "a", Config: admissionService("a", 2, 64, 0, ""), State: vm.StateRunning},
		"x": {Name: "x", Config: admissionService("x", 2, 64, 0, ""), State: vm.StateRecoveryPending},
	}, "")
	a.tick(context.Background())
	if vcpus, _ := runtimeUsage(m); vcpus > 4 {
		t.Fatalf("node runs %d vCPUs on 4; starts=%v", vcpus, m.startCalls)
	}
	desired := []config.ServiceConfig{admissionService("a", 3, 64, 0, ""), admissionService("b", 1, 64, 0, "")}
	if used, _ := a.admit(desired, capacity.NodeCapacity{VCPUs: 4, MemoryMB: 4096}, true, nil); used.VCPUs < 4 {
		t.Fatalf("heartbeat used %d vCPUs, below the 4 that run", used.VCPUs)
	}
}

// A running VM whose shrink is held keeps its current size, so another
// service cannot grow into memory or CPU it still uses while the hold lasts.
func TestHeldShrinkDoesNotOvercommit(t *testing.T) {
	yaml := "node: web\nservices:\n- name: a\n  image: /a\n  kernel: /k\n  vcpus: 1\n  memory_mb: 512\n  port_forwards:\n  - host_port: 7070\n    vm_port: 80\n- name: b\n  image: /b\n  kernel: /k\n  vcpus: 3\n  memory_mb: 3072\n  port_forwards:\n  - host_port: 9090\n    vm_port: 80\n"
	a, m := admissionAgent(t, yaml, capacity.NodeCapacity{VCPUs: 5, MemoryMB: 4096}, map[string]*vm.Instance{
		"a": {Name: "a", Config: admissionService("a", 3, 3072, 8080, ""), State: vm.StateRunning},
		"b": {Name: "b", Config: admissionService("b", 1, 512, 9090, ""), State: vm.StateRunning},
		"x": {Name: "x", Config: admissionService("x", 1, 256, 7070, ""), State: vm.StateRecoveryPending},
	}, "")
	for tick := 0; tick < 3; tick++ {
		a.tick(context.Background())
		if vcpus, mem := runtimeUsage(m); vcpus > 5 || mem > 4096 {
			t.Fatalf("tick %d runs %d vCPUs and %d MB on 5 vCPUs and 4096 MB", tick, vcpus, mem)
		}
	}
	// Every hold traces back to the quarantine.
	if vms := conditionOf(a.agentStatusSnapshot(), "VMsReconciled"); strings.Contains(vms.Message, reconciler.ErrServiceHeld.Error()) {
		t.Fatalf("a hold the quarantine causes is not attributed to recovery: %s", vms.Message)
	}
}

// An undesired VM whose removal is held keeps running and keeps its size.
func TestHeldRemovalKeepsCapacity(t *testing.T) {
	yaml := "node: web\nservices:\n- name: k\n  image: /k\n  kernel: /k\n  vcpus: 3\n  memory_mb: 64\n  port_forwards:\n  - host_port: 9090\n    vm_port: 80\n"
	a, m := admissionAgent(t, yaml, capacity.NodeCapacity{VCPUs: 4, MemoryMB: 4096}, map[string]*vm.Instance{
		"d": {Name: "d", Config: admissionService("d", 3, 64, 8080, ""), State: vm.StateRunning},
		"x": {Name: "x", Config: admissionService("x", 1, 64, 8080, ""), State: vm.StateRecoveryPending},
	}, "")
	for tick := 0; tick < 3; tick++ {
		a.tick(context.Background())
		if vcpus, _ := runtimeUsage(m); vcpus > 4 {
			t.Fatalf("tick %d runs %d vCPUs on 4: removes=%v starts=%v", tick, vcpus, m.removeCalls, m.startCalls)
		}
	}
}

// An address kept by an undesired VM whose removal recovery holds is a
// recovery hold, and so is a MAC kept by a quarantined VM outside a changed
// subnet. Neither fails the node.
func TestAddressKeptThroughRecoveryIsARecoveryHold(t *testing.T) {
	yaml := "node: web\nservices:\n- name: k\n  image: /k\n  kernel: /k\n  vcpus: 1\n  memory_mb: 64\n  network: {}\n"
	for name, instances := range map[string]map[string]*vm.Instance{
		"held removal": {
			"d": {Name: "d", Config: admissionService("d", 1, 64, 8080, "172.16.0.2"), State: vm.StateRunning},
			"x": {Name: "x", Config: admissionService("x", 0, 0, 8080, ""), State: vm.StateRecoveryPending},
		},
		"MAC outside the subnet": {
			"x": {Name: "x", Config: config.ServiceConfig{Name: "x", Network: &config.NetworkConfig{GuestIP: "10.9.0.2", GuestMAC: guestMACForIndex(0)}}, State: vm.StateRecoveryPending},
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := admissionAgent(t, yaml, capacity.NodeCapacity{VCPUs: 4, MemoryMB: 4096}, instances, "172.16.0.0/30")
			k := admissionService("k", 1, 64, 0, "")
			k.Network = &config.NetworkConfig{}
			held := a.assignNetworking([]config.ServiceConfig{k})
			if hold := held["k"]; !hold.Recovery || hold.Admission != "" {
				t.Fatalf("k hold = %+v", hold)
			}
			a.tick(context.Background())
			if status := a.agentStatusSnapshot(); status.Phase == statusmodel.PhaseFailed {
				t.Fatalf("node failed: %s %s", status.ReasonCode, status.Message)
			}
		})
	}
}

// After an agent restart, remote route files from the previous process stay
// on disk while the first peer listing fails. A held service's stale
// hostname is not published beside them: no listing proves it free.
func TestStaleRouteAfterRestartWhilePeersUnavailable(t *testing.T) {
	routeDir := t.TempDir()
	original := routingPersistenceService()
	store := &fakeStore{listResult: []config.NodeConfig{{Node: "peer", HostIP: "10.0.0.9", Services: []config.ServiceConfig{
		{Name: "other", Metadata: map[string]string{"host": "original.example.com"}, PortForwards: []config.PortForward{{HostPort: 8080, VMPort: 80}}},
	}}}}
	a, manager := routingPersistenceAgent(t, routeDir, original, "owner-old")
	a.store = store
	desired := original
	desired.Image = "/new"
	desired.Metadata = map[string]string{"host": "moved.example.com"}
	holdRoutingReplacement(manager)
	if err := a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{desired}); err != nil {
		t.Fatal(err)
	}
	restarted, restartedManager := routingPersistenceAgent(t, routeDir, original, "owner-old")
	restarted.store = store
	holdRoutingReplacement(restartedManager)
	store.listErr = os.ErrPermission
	if err := restarted.syncTraefikConfigs(context.Background(), []config.ServiceConfig{desired}); err != nil {
		t.Fatal(err)
	}
	if body, ok := readRoute(t, routeDir, "app.yaml"); ok {
		t.Fatalf("restarted agent republished a stale hostname while peers are unknown:\n%s", body)
	}
	if _, ok := readRoute(t, routeDir, "remote-other.yaml"); !ok {
		t.Fatal("preserved peer route was removed")
	}
}

// Before the first peer listing, a held service keeps its route when its
// hostname is unchanged: desired state gives that hostname to this very
// service, and a local claim wins over any peer's.
func TestHeldRouteWithUnchangedHostnameSurvivesUnknownPeers(t *testing.T) {
	routeDir := t.TempDir()
	original := routingPersistenceService()
	a, manager := routingPersistenceAgent(t, routeDir, original, "owner-old")
	a.store = &fakeStore{listErr: os.ErrPermission}
	desired := original
	desired.Image = "/new"
	desired.Metadata = map[string]string{"host": "original.example.com", "team": "payments"}
	holdRoutingReplacement(manager)
	if err := a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{desired}); err != nil {
		t.Fatal(err)
	}
	if _, ok := readRoute(t, routeDir, "app.yaml"); !ok {
		t.Fatal("a held running service lost the route its unchanged hostname still gives it")
	}
}
