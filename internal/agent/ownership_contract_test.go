package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// C1/C4/L4: a VM whose directory was removed by hand leaves its ownership
// record and rule behind. Its service stays held and the address and port
// stay fenced until clear-recovery --forget proves the VM absent. The
// command only authorizes cleanup: the address is released once the rule is
// gone, and the host port keeps its history (L6).
func TestForgetResolvesAHandRemovedVM(t *testing.T) {
	limit := capacity.NodeCapacity{VCPUs: 8, MemoryMB: 4096}
	web := addressedService("web", "172.16.0.2", 0)
	web.PortForwards = []config.PortForward{{HostPort: 8080, VMPort: 80}}
	yaml := "node: web\nservices:\n- name: web\n  image: /web\n  kernel: /k\n  vcpus: 1\n  memory_mb: 64\n  network: {}\n  port_forwards:\n  - host_port: 8080\n    vm_port: 80\n"
	a, m := admissionAgent(t, yaml, limit, map[string]*vm.Instance{"web": {Name: "web", Config: web, State: vm.StateRunning}}, "172.16.0.0/24")
	network := &recoveryTestNetworkManager{}
	writeJournal := func(path string, data []byte) error { return os.WriteFile(path, data, 0o644) }
	a.reconciler = testReconciler(t, m, network, "", 0)
	a.tick(context.Background())
	if _, ok := network.kernel.Delivers(8080); !ok {
		t.Fatal("setup: web's rule is not installed")
	}

	delete(m.instances, "web") // its directory was removed by hand; the VM may still run
	starts := len(m.startCalls)
	a.store.(*fakeStore).revision = "r3"
	a.tick(context.Background())
	if len(m.startCalls) != starts {
		t.Fatalf("a second VM started over the vanished one: %v", m.startCalls)
	}
	for _, service := range a.agentStatusSnapshot().Services {
		if service.Name == "web" && service.ReasonCode != reconciler.ActionVMStateMissing {
			t.Fatalf("web's status = %+v", service)
		}
	}

	live := errors.New("a live process uses the VM paths of web")
	a.proveServiceAbsent = func(context.Context, string) error { return live }
	if result, err := a.forgetRecoveryPending("web"); err == nil || result.Outcome != vm.RecoveryOutcomeAmbiguous {
		t.Fatalf("--forget released a possibly live VM: %#v %v", result, err)
	}
	a.proveServiceAbsent = func(context.Context, string) error { return nil }
	a.reconciler.WithJournalWriter(func(string, []byte) error { return errors.New("disk full") })
	if result, err := a.forgetRecoveryPending("web"); err == nil || result.Outcome != vm.RecoveryOutcomeIncomplete {
		t.Fatalf("a failed record write was hidden: %#v %v", result, err)
	}
	a.reconciler.WithJournalWriter(writeJournal)
	if _, ok := network.kernel.Delivers(8080); !ok || a.reconciler.PendingTeardownGuestIPs()["172.16.0.2"] != "web" {
		t.Fatal("a refused or failed --forget changed the fences")
	}

	result, err := a.forgetRecoveryPending("web")
	if err != nil || result.Outcome != vm.RecoveryOutcomeCleared {
		t.Fatalf("--forget: %#v %v", result, err)
	}
	// Authorized, not released: the rule and the address stay until cleanup.
	if _, ok := network.kernel.Delivers(8080); !ok || a.reconciler.PendingTeardownGuestIPs()["172.16.0.2"] != "web" {
		t.Fatal("--forget released the address before its rule was gone")
	}
	if again, err := a.forgetRecoveryPending("web"); err != nil || again.Outcome != vm.RecoveryOutcomeNoop {
		t.Fatalf("a repeated --forget: %#v %v", again, err)
	}

	a.applyRecoveryRequests(context.Background()) // the queued tick tears the rule down and recreates web
	a.tick(context.Background())
	if inst := m.instances["web"]; inst == nil || inst.Config.Network.GuestIP != "172.16.0.2" {
		t.Fatalf("web was not recreated at its address: %#v", inst)
	}
	if rule, ok := network.kernel.Delivers(8080); !ok || !rule.TaggedFor("web") || len(network.kernel.RulesTo("172.16.0.2")) != 1 {
		t.Fatalf("rules after cleanup: %v", network.kernel.Rules())
	}
	// Another service still never gets web's port.
	a.store.(*fakeStore).revision = "r4"
	a.store.(*fakeStore).data["web"] = []byte("node: web\nservices:\n- name: api\n  image: /api\n  kernel: /k\n  vcpus: 1\n  memory_mb: 64\n  network: {}\n  port_forwards:\n  - host_port: 8080\n    vm_port: 80\n")
	for tick := 0; tick < 2; tick++ {
		a.tick(context.Background())
	}
	if m.instances["api"] != nil {
		t.Fatal("api took web's host port after --forget")
	}
}

// C4/L7: a local service may be named remote-*, like the agent's peer route
// files. Its route still holds its address, and its withdrawal is recorded,
// through removal and an agent restart, until Traefik has restarted.
// (Finding from the review of 2f5aa27.)
func TestLocalServiceNamedLikeAPeerRouteKeepsItsAddress(t *testing.T) {
	limit := capacity.NodeCapacity{VCPUs: 8, MemoryMB: 4096}
	stateDir, routeDir := t.TempDir(), t.TempDir()
	process := &fakeRouteProcess{boot: "boot-1", start: "1000"}
	svc := addressedService("remote-app", "172.16.0.2", 0)
	svc.Metadata = map[string]string{"host": "app.example.com"}
	svc.PortForwards = []config.PortForward{{HostPort: 8080, VMPort: 80}}
	a, m := admissionAgent(t, "node: web\nservices: []\n", limit, map[string]*vm.Instance{
		"remote-app": {Name: "remote-app", Config: svc, State: vm.StateRunning},
	}, "172.16.0.0/24")
	a.reconciler = reconciler.NewWithNetworkManager(m, testLogger(), nil, &recoveryTestNetworkManager{}, "", 0).WithStateDir(stateDir)
	useRoutesIn(a, routeDir, process)
	if err := a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{svc}); err != nil {
		t.Fatal(err)
	}
	offered := func(a *Agent) string {
		next := []config.ServiceConfig{networkedService("beta")}
		a.assignNetworking(next)
		return next[0].Network.GuestIP
	}
	m.clear(t, "remote-app")
	if got := offered(a); got == "172.16.0.2" {
		t.Fatal("remote-app's address was offered while its route points there")
	}
	a.tick(context.Background()) // publishes without remote-app and tears down its rule
	if got := offered(a); got == "172.16.0.2" {
		t.Fatal("remote-app's address was offered while the Traefik that served its route still runs")
	}
	restarted, _ := admissionAgent(t, "node: web\nservices: []\n", limit, nil, "172.16.0.0/24")
	restarted.reconciler = reconciler.NewWithNetworkManager(m, testLogger(), nil, &recoveryTestNetworkManager{}, "", 0)
	useRoutesIn(restarted, routeDir, process)
	restarted.reconciler.WithStateDir(stateDir)
	if got := offered(restarted); got == "172.16.0.2" {
		t.Fatal("a restarted agent offered remote-app's address before Traefik restarted")
	}
	process.start = "2000"
	restarted.tick(context.Background())
	if got := offered(restarted); got != "172.16.0.2" {
		t.Fatalf("the address was not released after Traefik restarted: %s", got)
	}
}

// C3: when --forget cannot record the service's host networking for cleanup,
// the agent retries the absence proof and hand-off itself, as the incomplete
// outcome says, and the service recovers once the journal can be written.
// (Finding from the review of a3cbc29.)
func TestIncompleteForgetIsRetriedByTheAgent(t *testing.T) {
	limit := capacity.NodeCapacity{VCPUs: 8, MemoryMB: 4096}
	web := addressedService("web", "172.16.0.2", 0)
	web.PortForwards = []config.PortForward{{HostPort: 8080, VMPort: 80}}
	yaml := "node: web\nservices:\n- name: web\n  image: /web\n  kernel: /k\n  vcpus: 1\n  memory_mb: 64\n  network: {}\n  port_forwards:\n  - host_port: 8080\n    vm_port: 80\n"
	a, m := admissionAgent(t, yaml, limit, map[string]*vm.Instance{"web": {Name: "web", Config: web, State: vm.StateRunning}}, "172.16.0.0/24")
	a.reconciler = testReconciler(t, m, &recoveryTestNetworkManager{}, "", 0)
	a.tick(context.Background())
	delete(m.instances, "web") // its directory was removed by hand
	a.store.(*fakeStore).revision = "r3"
	a.tick(context.Background())
	a.proveServiceAbsent = func(context.Context, string) error { return nil }
	a.reconciler.WithJournalWriter(func(string, []byte) error { return errors.New("disk full") })
	result, err := a.forgetRecoveryPending("web")
	if err == nil || result.Outcome != vm.RecoveryOutcomeIncomplete || !strings.Contains(result.Message, "retries") {
		t.Fatalf("forget: %#v %v", result, err)
	}
	a.reconciler.WithJournalWriter(func(path string, data []byte) error { return os.WriteFile(path, data, 0o600) })
	for tick := 0; tick < 3; tick++ {
		a.tick(context.Background())
		a.applyRecoveryRequests(context.Background())
	}
	if inst := m.instances["web"]; inst == nil || inst.Config.Network.GuestIP != "172.16.0.2" {
		t.Fatalf("the incomplete --forget was not retried: %#v holds=%v", inst, a.reconciler.HeldServices(config.NodeConfig{Services: []config.ServiceConfig{web}}))
	}
	if again, err := a.forgetRecoveryPending("web"); err != nil || again.Outcome != vm.RecoveryOutcomeNoop {
		t.Fatalf("a repeated --forget after the retry: %#v %v", again, err)
	}
}

// C3/C5: unavailable absence evidence keeps an accepted --forget pending.
// Nothing is released until inspection works again, and no new command or
// revision is needed once the transient failure is gone.
func TestIncompleteForgetRetriesUnavailableAbsenceEvidence(t *testing.T) {
	for name, inspectionErr := range map[string]error{
		"inspection deadline":   context.DeadlineExceeded,
		"inspection unreadable": os.ErrPermission,
	} {
		t.Run(name, func(t *testing.T) {
			web := addressedService("web", "172.16.0.2", 0)
			web.PortForwards = []config.PortForward{{HostPort: 8080, VMPort: 80}}
			yaml := "node: web\nservices:\n- name: web\n  image: /web\n  kernel: /k\n  vcpus: 1\n  memory_mb: 64\n  network: {}\n  port_forwards:\n  - host_port: 8080\n    vm_port: 80\n"
			a, m := admissionAgent(t, yaml, capacity.NodeCapacity{VCPUs: 8, MemoryMB: 4096},
				map[string]*vm.Instance{"web": {Name: "web", Config: web, State: vm.StateRunning}}, "172.16.0.0/24")
			network := &recoveryTestNetworkManager{}
			a.reconciler = testReconciler(t, m, network, "", 0)
			a.tick(context.Background())
			delete(m.instances, "web")
			a.store.(*fakeStore).revision = "r3"
			a.tick(context.Background())
			a.proveServiceAbsent = func(context.Context, string) error { return nil }
			a.reconciler.WithJournalWriter(func(string, []byte) error { return errors.New("disk full") })
			if result, err := a.forgetRecoveryPending("web"); err == nil || result.Outcome != vm.RecoveryOutcomeIncomplete {
				t.Fatalf("setup: forget=%+v err=%v", result, err)
			}
			a.reconciler.WithJournalWriter(func(path string, data []byte) error { return os.WriteFile(path, data, 0o600) })
			starts := len(m.startCalls)
			a.proveServiceAbsent = func(context.Context, string) error { return inspectionErr }
			a.tick(context.Background())
			if !a.forgetRetries["web"] {
				t.Fatal("unavailable absence evidence discarded the pending --forget")
			}
			if len(m.startCalls) != starts || a.reconciler.PendingTeardownGuestIPs()["172.16.0.2"] != "web" {
				t.Fatal("unavailable absence evidence released the VM's fence")
			}
			if _, ok := network.kernel.Delivers(8080); !ok {
				t.Fatal("unavailable absence evidence authorized rule deletion")
			}
			a.proveServiceAbsent = func(context.Context, string) error { return nil }
			for tick := 0; tick < 4; tick++ {
				a.tick(context.Background())
				a.applyRecoveryRequests(context.Background())
			}
			if m.instances["web"] == nil || len(a.forgetRetries) != 0 || a.lastRevision != "r3" || len(m.startCalls) != starts+1 {
				t.Fatalf("inspection recovery did not finish the original --forget: VM=%+v retry=%v revision=%q starts=%v",
					m.instances["web"], a.forgetRetries, a.lastRevision, m.startCalls)
			}
		})
	}
}

// C4/L7: an address routed under an earlier guest subnet keeps its route
// ownership after the subnet changes: removing the route records its
// withdrawal, and restoring the subnet does not give the address to another
// service before Traefik restarts. (Finding from the review of a3cbc29.)
func TestRouteOwnershipSurvivesASubnetChange(t *testing.T) {
	limit := capacity.NodeCapacity{VCPUs: 8, MemoryMB: 4096}
	stateDir, routeDir := t.TempDir(), t.TempDir()
	process := &fakeRouteProcess{boot: "boot-1", start: "1000"}
	network := &recoveryTestNetworkManager{}
	var m *recoveryTestVMManager
	newAgent := func(subnet, yaml string) *Agent {
		a, fresh := admissionAgent(t, yaml, limit, map[string]*vm.Instance{}, subnet)
		if m == nil {
			m = fresh
		}
		a.reconciler = reconciler.NewWithNetworkManager(m, testLogger(), nil, network, "", 0)
		useRoutesIn(a, routeDir, process)
		a.reconciler.WithHostHistoryEvidence(subnet, func() (bool, error) { return traefik.HasLocalRouteFiles(routeDir) }).WithStateDir(stateDir)
		return a
	}
	alpha := "node: web\nservices:\n- name: alpha\n  image: /alpha\n  kernel: /k\n  vcpus: 1\n  memory_mb: 64\n  network: {}\n  metadata:\n    host: alpha.example.com\n  health_check:\n    type: tcp\n    port: 80\n"
	newAgent("172.16.0.0/24", alpha).tick(context.Background())
	if routes, _, err := traefik.ReadLocalRoutes(routeDir); err != nil || len(routes) != 1 || routes[0].GuestIP != "172.16.0.2" {
		t.Fatalf("setup: routes=%v err=%v", routes, err)
	}
	newAgent("10.9.0.0/24", "node: web\nservices: []\n").tick(context.Background())
	if m.instances["alpha"] != nil {
		t.Fatal("setup: alpha was not removed")
	}
	beta := "node: web\nservices:\n- name: beta\n  image: /beta\n  kernel: /k\n  vcpus: 1\n  memory_mb: 64\n  network: {}\n"
	third := newAgent("172.16.0.0/24", beta)
	third.tick(context.Background())
	if inst := m.instances["beta"]; inst != nil && inst.Config.Network.GuestIP == "172.16.0.2" {
		t.Fatal("beta took alpha's routed address without a Traefik restart")
	}
}

// Without vm_subnet, an unchanged peer route is not taken for a withdrawn
// local route on every publish. (Finding from the review of a3cbc29.)
func TestPeerRoutesAreNotWithdrawnWithoutASubnet(t *testing.T) {
	limit := capacity.NodeCapacity{VCPUs: 8, MemoryMB: 4096}
	a, m := admissionAgent(t, "node: web\nservices: []\n", limit, map[string]*vm.Instance{}, "")
	a.reconciler = reconciler.NewWithNetworkManager(m, testLogger(), nil, &recoveryTestNetworkManager{}, "", 0)
	routeDir := t.TempDir()
	useRoutesIn(a, routeDir, &fakeRouteProcess{boot: "boot-1", start: "100"})
	a.reconciler.WithStateDir(t.TempDir())
	peer := config.NodeConfig{Node: "other", HostIP: "10.0.0.8", Services: []config.ServiceConfig{{Name: "peer",
		Metadata: map[string]string{"host": "peer.example.com"}, PortForwards: []config.PortForward{{HostPort: 8080, VMPort: 80}}}}}
	for publish := 0; publish < 3; publish++ {
		if err := a.syncRuntimeRoutes(nil, []config.NodeConfig{peer}, false); err != nil {
			t.Fatal(err)
		}
	}
	if routes, _, err := traefik.ReadLocalRoutes(routeDir); err != nil || len(routes) != 1 || !routes[0].Peer {
		t.Fatalf("peer route: %v %v", routes, err)
	}
	if held, _, _ := a.reconciler.RouteHeldGuestIPs(); a.reconciler.HasPendingCleanup() || len(held) != 0 {
		t.Fatalf("a live peer route was recorded as withdrawn: held=%v", held)
	}
}

// C4: a route file with a tcp or udp section may route to a guest address this
// agent cannot read, so it holds the address like an unreadable file rather
// than counting as a file without routes. (Finding from the review of
// a3cbc29.)
func TestTransportRouteFilesFenceAddresses(t *testing.T) {
	for _, protocol := range []string{"tcp", "udp"} {
		t.Run(protocol, func(t *testing.T) {
			limit := capacity.NodeCapacity{VCPUs: 8, MemoryMB: 4096}
			yaml := "node: web\nservices:\n- name: beta\n  image: /beta\n  kernel: /k\n  vcpus: 1\n  memory_mb: 64\n  network: {}\n"
			a, m := admissionAgent(t, yaml, limit, map[string]*vm.Instance{}, "172.16.0.0/24")
			a.reconciler = reconciler.NewWithNetworkManager(m, testLogger(), nil, &recoveryTestNetworkManager{}, "", 0)
			routeDir := t.TempDir()
			useRoutesIn(a, routeDir, &fakeRouteProcess{boot: "boot-1", start: "100"})
			a.reconciler.WithStateDir(t.TempDir())
			rule := ""
			if protocol == "tcp" {
				rule = "      rule: HostSNI(`*`)\n"
			}
			route := fmt.Sprintf("%s:\n  routers:\n    legacy:\n%s      service: legacy\n  services:\n    legacy:\n      loadBalancer:\n        servers:\n        - address: 172.16.0.2:8080\n", protocol, rule)
			path := filepath.Join(routeDir, "legacy.yaml")
			if err := os.WriteFile(path, []byte(route), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, unreadable, err := traefik.ReadLocalRoutes(routeDir); err != nil || len(unreadable) != 1 {
				t.Fatalf("a %s route file was read as having no routes: %v %v", protocol, unreadable, err)
			}
			m.startHook = func(string) error {
				if _, err := os.Stat(path); err == nil {
					t.Errorf("beta started while the %s route file still existed", protocol)
				}
				return nil
			}
			a.tick(context.Background())
			if inst := m.instances["beta"]; inst != nil {
				t.Fatalf("beta started beside a %s route file: %s", protocol, inst.Config.Network.GuestIP)
			}
		})
	}
}
