package agent

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/config"
	netpkg "github.com/artemnikitin/firework/internal/network"
	"github.com/artemnikitin/firework/internal/network/networktest"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/traefik"
	"github.com/artemnikitin/firework/internal/vm"
)

// C4: a health restart whose start fails drops the VM, as vm.Manager does.
// Its rule stays covered by a departing record, so its address is not given
// to another service while the rule may still forward there, and a later
// service's traffic never reaches the old guest.
// (Reproducer from the review of 5392d54.)
func TestC4FailedHealthRestartDoesNotLeaveAnUnownedRule(t *testing.T) {
	limit := capacity.NodeCapacity{VCPUs: 8, MemoryMB: 4096}
	cSvc := admissionService("c", 1, 64, 8080, "172.16.0.3")
	cSvc.Network.GuestMAC = guestMACForIndex(1)
	xSvc := admissionService("x", 1, 64, 0, "172.16.0.2")
	xSvc.Network.GuestMAC = guestMACForIndex(0)
	yamlC := "node: web\nservices:\n- name: c\n  image: /c\n  kernel: /k\n  vcpus: 1\n  memory_mb: 64\n  network: {}\n  port_forwards:\n  - host_port: 8080\n    vm_port: 80\n"
	a, m := admissionAgent(t, yamlC, limit, map[string]*vm.Instance{
		"c": {Name: "c", Config: cSvc, State: vm.StateRunning},
		"x": {Name: "x", Config: xSvc, State: vm.StateRunning},
	}, "172.16.0.0/24")
	network := &recoveryTestNetworkManager{}
	a.reconciler = testReconciler(t, m, network, "", 0)
	a.tick(context.Background()) // x is removed; c's rule is installed.
	if rule, ok := network.kernel.Delivers(8080); m.instances["x"] != nil || !ok || rule.GuestIP != "172.16.0.3" {
		t.Fatalf("setup: x=%v rules=%v", m.instances["x"], network.kernel.Rules())
	}
	m.startHook = func(name string) error {
		if name == "c" {
			delete(m.instances, "c")
			return errors.New("preparing volumes: transient")
		}
		return nil
	}
	if err := a.restartService(context.Background(), "c"); err == nil {
		t.Fatal("setup: the restart should fail")
	}
	m.startHook = nil
	if owner := a.reconciler.PendingTeardownGuestIPs()["172.16.0.3"]; owner != "c" {
		t.Fatalf("c's address is not held after the failed restart: %v", a.reconciler.PendingTeardownGuestIPs())
	}
	a.tick(context.Background()) // c is recreated.
	a.store.(*fakeStore).revision = "r3"
	a.store.(*fakeStore).data["web"] = []byte(yamlC + "- name: d\n  image: /d\n  kernel: /k\n  vcpus: 1\n  memory_mb: 64\n  network: {}\n")
	a.tick(context.Background())
	a.tick(context.Background())
	c, d := m.instances["c"], m.instances["d"]
	if c == nil || d == nil {
		t.Fatalf("c=%v d=%v starts=%v", c, d, m.startCalls)
	}
	if rule, ok := network.kernel.Delivers(8080); !ok || rule.GuestIP != c.Config.Network.GuestIP || rule.GuestIP == d.Config.Network.GuestIP {
		t.Fatalf("host port 8080 does not reach c alone: c=%s d=%s rules=%v", c.Config.Network.GuestIP, d.Config.Network.GuestIP, network.kernel.Rules())
	}
}

// C4: a removed service whose rule's teardown failed comes back on another
// port at its old address, and another service asks for its old port.
// Neither starts while the old rule could deliver the second service's
// traffic to the first one's guest. The first starts once the rule is gone;
// the second never gets the port, which peers may still use for the first
// (L6). (Reproducer from the review of 5392d54.)
func TestC4ServiceRetakingItsAddressWaitsForItsOldRule(t *testing.T) {
	limit := capacity.NodeCapacity{VCPUs: 8, MemoryMB: 4096}
	xSvc := admissionService("x", 1, 64, 0, "172.16.0.2")
	xSvc.Network.GuestMAC = guestMACForIndex(0)
	aSvc := admissionService("a", 1, 64, 8080, "172.16.0.3")
	aSvc.Network.GuestMAC = guestMACForIndex(1)
	xYAML := "- name: x\n  image: /x\n  kernel: /k\n  vcpus: 1\n  memory_mb: 64\n  network: {}\n"
	a, m := admissionAgent(t, "node: web\nservices:\n"+xYAML, limit, map[string]*vm.Instance{
		"x": {Name: "x", Config: xSvc, State: vm.StateRunning},
		"a": {Name: "a", Config: aSvc, State: vm.StateRunning},
	}, "172.16.0.0/24")
	network := &recoveryTestNetworkManager{teardownErr: errors.New("iptables: resource temporarily unavailable")}
	a.reconciler = testReconciler(t, m, network, "", 0)
	network.kernel.Append(networktest.Rule(netpkg.PortForwardTag("a"), 8080, "172.16.0.3", 80)) // a runs under this journal.
	a.tick(context.Background())
	if m.instances["a"] != nil {
		t.Fatal("setup: a not removed")
	}
	var violations []string
	m.startHook = func(name string) error {
		if rule, ok := network.kernel.Delivers(8080); ok && rule.TaggedFor("a") {
			violations = append(violations, name+" started while a's old 8080 rule was installed")
		}
		return nil
	}
	a.store.(*fakeStore).revision = "r3"
	a.store.(*fakeStore).data["web"] = []byte("node: web\nservices:\n" +
		"- name: a\n  image: /a\n  kernel: /k\n  vcpus: 1\n  memory_mb: 64\n  network: {}\n  port_forwards:\n  - host_port: 9090\n    vm_port: 80\n" +
		"- name: b\n  image: /b\n  kernel: /k\n  vcpus: 1\n  memory_mb: 64\n  network: {}\n  port_forwards:\n  - host_port: 8080\n    vm_port: 80\n" + xYAML)
	for tick := 0; tick < 3; tick++ {
		a.tick(context.Background())
	}
	if m.instances["a"] != nil || m.instances["b"] != nil {
		t.Fatalf("a or b started while a's old rule was installed: a=%v b=%v", m.instances["a"], m.instances["b"])
	}
	network.teardownErr = nil
	for tick := 0; tick < 3; tick++ {
		a.tick(context.Background())
	}
	if m.instances["a"] == nil || m.instances["b"] != nil {
		t.Fatalf("a=%v b=%v once the rule was gone: %s", m.instances["a"], m.instances["b"], a.agentStatusSnapshot().Message)
	}
	if _, ok := network.kernel.Delivers(8080); ok {
		t.Fatalf("host port 8080 still forwards: %v", network.kernel.Rules())
	}
	if reconciled := conditionOf(a.agentStatusSnapshot(), "Reconciled"); reconciled.ReasonCode != reconciler.ActionHostPortReuseBlocked {
		t.Fatalf("status does not name the blocked port: %+v", reconciled)
	}
	for _, service := range a.agentStatusSnapshot().Services {
		if service.Name == "b" && service.ReasonCode != reconciler.ActionHostPortReuseBlocked {
			t.Fatalf("b's status = %+v", service)
		}
	}
	if len(violations) != 0 {
		t.Fatal(violations)
	}
}

// fakeRouteProcess is the process serving routes: a restart changes start.
type fakeRouteProcess struct {
	boot, start string
	err         error
}

func (p *fakeRouteProcess) Observe() (string, string, error) { return p.boot, p.start, p.err }

// useRoutes gives the agent a real route directory, served by a fake Traefik
// process, and returns that process.
func useRoutes(t *testing.T, a *Agent) *fakeRouteProcess {
	t.Helper()
	process := &fakeRouteProcess{boot: "boot-1", start: "1000"}
	useRoutesIn(a, t.TempDir(), process)
	return process
}

func useRoutesIn(a *Agent, dir string, process *fakeRouteProcess) {
	a.cfg.TraefikUnit = "traefik.service"
	a.traefikMgr = traefik.NewManager(dir, "", testLogger())
	a.routeProcess = process
	a.reconciler.WithRouteEvidence(&agentRouteEvidence{agent: a})
}

// publishRoute writes the route of a service at guestIP, as an earlier tick
// would have.
func publishRoute(t *testing.T, a *Agent, service, guestIP string) {
	t.Helper()
	svc := addressedService(service, guestIP, 9)
	svc.Metadata = map[string]string{"host": service + ".example.com"}
	svc.PortForwards = []config.PortForward{{HostPort: 1, VMPort: 80}}
	if err := a.traefikMgr.SyncLocal([]config.ServiceConfig{svc}); err != nil {
		t.Fatal(err)
	}
}

func addressedService(name, ip string, index int) config.ServiceConfig {
	svc := admissionService(name, 1, 64, 0, ip)
	svc.Network.GuestMAC = guestMACForIndex(index)
	return svc
}

func networkedService(name string) config.ServiceConfig {
	return config.ServiceConfig{Name: name, Network: &config.NetworkConfig{}}
}

// C6: an address held back until a removed VM's rules or route are gone is a
// wait, not invalid address demand, even when its owner is still desired and
// gets another address meanwhile. (Reproducer from the review of 5392d54.)
func TestC6HeldBackAddressIsATransientWaitWhoeverOwnsIt(t *testing.T) {
	limit := capacity.NodeCapacity{VCPUs: 8, MemoryMB: 4096}
	// /29: guest addresses .2 to .6.
	a, _ := admissionAgent(t, "node: web\nservices: []\n", limit, map[string]*vm.Instance{
		"c": {Name: "c", Config: addressedService("c", "172.16.0.3", 1), State: vm.StateRunning},
		"d": {Name: "d", Config: addressedService("d", "172.16.0.5", 3), State: vm.StateRunning},
		"e": {Name: "e", Config: addressedService("e", "172.16.0.6", 4), State: vm.StateRunning},
	}, "172.16.0.0/29")
	useRoutes(t, a)
	publishRoute(t, a, "a", "172.16.0.4") // a's old VM's route is still published.
	services := []config.ServiceConfig{networkedService("a"), networkedService("c"), networkedService("d"), networkedService("e"), networkedService("f")}
	held := a.assignNetworking(services)
	hold, ok := held["f"]
	if !ok || hold.Admission != "" || hold.Recovery || hold.Reason != addressTransientReason {
		t.Fatalf("f's hold = %+v (held=%v)", hold, ok)
	}
}

// C6: a service keeps the address held back for it even when an earlier
// service in the allocation order had to skip that address.
// (Reproducer from the review of 5392d54.)
func TestHeldBackAddressStaysAvailableToItsOwner(t *testing.T) {
	limit := capacity.NodeCapacity{VCPUs: 8, MemoryMB: 4096}
	a, _ := admissionAgent(t, "node: web\nservices: []\n", limit, map[string]*vm.Instance{
		"c": {Name: "c", Config: addressedService("c", "172.16.0.2", 0), State: vm.StateRunning},
		"d": {Name: "d", Config: addressedService("d", "172.16.0.5", 3), State: vm.StateRunning},
		"e": {Name: "e", Config: addressedService("e", "172.16.0.6", 4), State: vm.StateRunning},
	}, "172.16.0.0/29")
	useRoutes(t, a)
	publishRoute(t, a, "zeta", "172.16.0.3") // zeta's old VM, route not yet withdrawn
	services := []config.ServiceConfig{networkedService("alpha"), networkedService("c"), networkedService("d"), networkedService("e"), networkedService("zeta")}
	held := a.assignNetworking(services)
	if len(held) != 0 {
		t.Fatalf("holds: %+v", held)
	}
	if got := services[4].Network.GuestIP; got != "172.16.0.3" {
		t.Fatalf("zeta got %q, not its own held-back address", got)
	}
	if got := services[0].Network.GuestIP; got != "172.16.0.4" {
		t.Fatalf("alpha got %q", got)
	}
}

// C4/L7: the agent dies after removing a routed VM, before withdrawing its
// route. After a restart, with publishing still failing, the route file on
// disk holds its address, so a new service cannot start where Traefik still
// routes the removed one's hostname. Once a publish removes the file, the
// address waits for a Traefik restart. (Reproducer from the review of
// 5392d54.)
func TestC4RestartedAgentKeepsAPublishedRouteAddress(t *testing.T) {
	limit := capacity.NodeCapacity{VCPUs: 8, MemoryMB: 4096}
	stateDir, routeDir := t.TempDir(), t.TempDir()
	process := &fakeRouteProcess{boot: "boot-1", start: "1000"}
	zeta := addressedService("zeta", "172.16.0.2", 0)
	zeta.Metadata = map[string]string{"host": "zeta.example.com"}
	zeta.HealthCheck = &config.HealthCheckConfig{Type: "tcp", Port: 80}
	first, m := admissionAgent(t, "node: web\nservices: []\n", limit, map[string]*vm.Instance{
		"zeta": {Name: "zeta", Config: zeta, State: vm.StateRunning},
	}, "172.16.0.0/24")
	first.reconciler = reconciler.NewWithNetworkManager(m, testLogger(), nil, &recoveryTestNetworkManager{}, "", 0).WithStateDir(stateDir)
	useRoutesIn(first, routeDir, process)
	if err := first.syncTraefikConfigs(context.Background(), []config.ServiceConfig{zeta}); err != nil {
		t.Fatal(err)
	}
	_ = first.reconciler.Reconcile(context.Background(), config.NodeConfig{}) // zeta goes; the agent dies before its route sync.
	if m.instances["zeta"] != nil {
		t.Fatal("setup: zeta was not removed")
	}

	yaml := "node: web\nservices:\n- name: alpha\n  image: /alpha\n  kernel: /k\n  vcpus: 1\n  memory_mb: 64\n  network: {}\n  metadata:\n    host: alpha.example.com\n  health_check:\n    type: tcp\n    port: 80\n"
	second, _ := admissionAgent(t, yaml, limit, nil, "172.16.0.0/24")
	second.reconciler = reconciler.NewWithNetworkManager(m, testLogger(), nil, &recoveryTestNetworkManager{}, "", 0)
	useRoutesIn(second, routeDir, process)
	second.reconciler.WithStateDir(stateDir)
	if err := os.Chmod(routeDir, 0o555); err != nil { // publishing keeps failing
		t.Fatal(err)
	}
	second.tick(context.Background())
	if alpha := m.instances["alpha"]; alpha != nil && alpha.Config.Network.GuestIP == "172.16.0.2" {
		t.Fatal("alpha was given zeta's address while zeta's route file still exists")
	}
	if err := os.Chmod(routeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	second.tick(context.Background()) // The publish removes zeta's route.
	if routes, _, _ := traefik.ReadLocalRoutes(routeDir); slices.ContainsFunc(routes, func(r traefik.RouteFile) bool { return r.Service == "zeta" }) {
		t.Fatalf("zeta's route survived a successful publish: %v", routes)
	}
	next := []config.ServiceConfig{networkedService("beta")}
	second.assignNetworking(next)
	if next[0].Network.GuestIP == "172.16.0.2" {
		t.Fatal("beta was offered 172.16.0.2 while the Traefik that served zeta's route still runs")
	}
	process.start = "2000"
	second.tick(context.Background())
	next = []config.ServiceConfig{networkedService("beta")}
	second.assignNetworking(next)
	if next[0].Network.GuestIP != "172.16.0.2" {
		t.Fatalf("172.16.0.2 not released after Traefik restarted: %s", next[0].Network.GuestIP)
	}
}
