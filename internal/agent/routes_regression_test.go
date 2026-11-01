package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/traefik"
	"github.com/artemnikitin/firework/internal/vm"
)

func readRoute(t *testing.T, dir, name string) (string, bool) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, name))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(body), true
}

// A health restart gives a held service's VM a new runtime identity but the
// same configuration and address. Its route stays published.
func TestHeldServiceKeepsRouteAcrossHealthRestart(t *testing.T) {
	routeDir := t.TempDir()
	original := routingPersistenceService()
	a, manager := routingPersistenceAgent(t, routeDir, original, "owner-old")
	if err := a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{original}); err != nil {
		t.Fatal(err)
	}
	desired := original
	desired.Image = "/new"
	holdRoutingReplacement(manager)
	manager.instances["app"] = &vm.Instance{Name: "app", Config: original, State: vm.StateRunning, InstanceID: "owner-restarted"}
	if err := a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{desired}); err != nil {
		t.Fatal(err)
	}
	requirePublishedRoute(t, routeDir, "original.example.com", "172.16.0.3:80")
}

// A route file written by an agent from before this change has no ownership
// header. A held service keeps its route after the upgrade.
func TestUpgradeKeepsHeldServiceRoute(t *testing.T) {
	routeDir := t.TempDir()
	original := routingPersistenceService()
	if err := traefik.NewManager(routeDir, "", testLogger()).Sync([]config.ServiceConfig{original}, nil); err != nil {
		t.Fatal(err)
	}
	a, manager := routingPersistenceAgent(t, routeDir, original, "owner-old")
	desired := original
	desired.Image = "/new"
	holdRoutingReplacement(manager)
	if err := a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{desired}); err != nil {
		t.Fatal(err)
	}
	requirePublishedRoute(t, routeDir, "original.example.com", "172.16.0.3:80")
}

// A live, trusted VM can be quarantined transiently, for example by a
// verification timeout. Its route stays published, and stays after it is
// re-adopted with a held update.
func TestTrustedRecoveryPendingKeepsRoute(t *testing.T) {
	routeDir := t.TempDir()
	original := routingPersistenceService()
	a, manager := routingPersistenceAgent(t, routeDir, original, "owner-old")
	manager.instances["app"].State = vm.StateRecoveryPending
	if err := a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{original}); err != nil {
		t.Fatal(err)
	}
	requirePublishedRoute(t, routeDir, "original.example.com", "172.16.0.3:80")
	manager.instances["app"].State = vm.StateRunning
	desired := original
	desired.Image = "/new"
	holdRoutingReplacement(manager)
	if err := a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{desired}); err != nil {
		t.Fatal(err)
	}
	requirePublishedRoute(t, routeDir, "original.example.com", "172.16.0.3:80")
}

// A hostname moves from a held service to another one. The desired owner gets
// it, the held service's stale route is dropped, and unrelated services keep
// converging instead of every route sync failing.
func TestHostnameMoveUnderHoldDoesNotFreezeRoutes(t *testing.T) {
	routeDir := t.TempDir()
	a := New(testAgentConfig(t), &fakeStore{}, testLogger())
	svcA := config.ServiceConfig{Name: "a", Image: "/a", Network: &config.NetworkConfig{GuestIP: "172.16.0.2"}, HealthCheck: &config.HealthCheckConfig{Port: 80}, Metadata: map[string]string{"host": "x.example.com"}}
	svcB := config.ServiceConfig{Name: "b", Image: "/b", Network: &config.NetworkConfig{GuestIP: "172.16.0.3"}, HealthCheck: &config.HealthCheckConfig{Port: 80}, Metadata: map[string]string{"host": "y.example.com"}}
	svcC := config.ServiceConfig{Name: "c", Image: "/c", Network: &config.NetworkConfig{GuestIP: "172.16.0.4"}, HealthCheck: &config.HealthCheckConfig{Port: 80}, Metadata: map[string]string{"host": "c.example.com"}}
	manager := &recoveryTestVMManager{instances: map[string]*vm.Instance{
		"a": {Name: "a", Config: svcA, State: vm.StateRunning},
		"b": {Name: "b", Config: svcB, State: vm.StateRunning},
		"c": {Name: "c", Config: svcC, State: vm.StateRunning},
	}}
	a.reconciler = reconciler.New(manager, testLogger(), nil, nil, "", 0)
	a.traefikMgr = traefik.NewManager(routeDir, "", testLogger())
	if err := a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{svcA, svcB, svcC}); err != nil {
		t.Fatal(err)
	}
	holdRoutingReplacement(manager)
	dA, dB, dC := svcA, svcB, svcC
	dA.Image, dA.Metadata = "/a2", map[string]string{"host": "z.example.com"}
	dB.Metadata = map[string]string{"host": "x.example.com"}
	dC.Metadata = map[string]string{"host": "c-new.example.com"}
	desired := []config.ServiceConfig{dA, dB, dC}
	_ = a.reconciler.Reconcile(context.Background(), config.NodeConfig{Services: desired})
	if err := a.syncTraefikConfigs(context.Background(), desired); err != nil {
		t.Fatalf("route sync failed: %v", err)
	}
	if body, _ := readRoute(t, routeDir, "b.yaml"); !strings.Contains(body, "Host(`x.example.com`)") || !strings.Contains(body, "172.16.0.3") {
		t.Fatalf("b did not get x:\n%s", body)
	}
	if body, _ := readRoute(t, routeDir, "c.yaml"); !strings.Contains(body, "Host(`c-new.example.com`)") {
		t.Fatalf("unrelated c was frozen:\n%s", body)
	}
	if body, ok := readRoute(t, routeDir, "a.yaml"); ok {
		t.Fatalf("held a kept a hostname desired state gives to b:\n%s", body)
	}
}

// A non-recovery hold leaves the revision incomplete while independent
// deletes and creates still run. Routes follow those VMs: a deleted service's
// hostname must not keep routing to the address a new service then takes.
func TestRoutesFollowVMsWhileOtherWorkIsHeld(t *testing.T) {
	routeDir := t.TempDir()
	cfg := testAgentConfig(t)
	enabled := true
	cfg.EnableCapacityCheck = &enabled
	store := &fakeStore{revision: "r1", data: map[string][]byte{"web": []byte(
		"node: web\nservices:\n- name: d\n  image: /d\n  kernel: /k\n  vcpus: 1\n  memory_mb: 64\n  network:\n    guest_ip: 172.16.0.2\n  health_check:\n    type: tcp\n    port: 80\n  metadata:\n    host: d.example.com\n")}}
	a := New(cfg, store, testLogger())
	a.capacityReader = &fakeCapacityReader{cap: capacity.NodeCapacity{VCPUs: 2, MemoryMB: 4096}}
	parsed, err := config.ParseNodeConfig(store.data["web"])
	if err != nil {
		t.Fatal(err)
	}
	manager := &recoveryTestVMManager{instances: map[string]*vm.Instance{
		"d":     {Name: "d", Config: parsed.Services[0], State: vm.StateRunning},
		"stuck": {Name: "stuck", Config: config.ServiceConfig{Name: "stuck"}, State: vm.StateStopping},
	}}
	a.reconciler = reconciler.NewWithNetworkManager(manager, testLogger(), nil, &recoveryTestNetworkManager{}, "", 0)
	a.traefikMgr = traefik.NewManager(routeDir, "", testLogger())
	a.tick(context.Background())
	if body, ok := readRoute(t, routeDir, "d.yaml"); !ok || !strings.Contains(body, "172.16.0.2") {
		t.Fatalf("setup: d route not published:\n%s", body)
	}
	store.revision = "r2"
	store.data["web"] = []byte("node: web\nservices:\n- name: z\n  image: /z\n  kernel: /k\n  vcpus: 3\n  memory_mb: 64\n  network:\n    guest_ip: 172.16.0.9\n")
	a.tick(context.Background())
	store.revision = "r3"
	store.data["web"] = []byte("node: web\nservices:\n- name: k\n  image: /k\n  kernel: /k\n  vcpus: 1\n  memory_mb: 64\n  network:\n    guest_ip: 172.16.0.2\n  health_check:\n    type: tcp\n    port: 80\n  metadata:\n    host: k.example.com\n- name: z\n  image: /z\n  kernel: /k\n  vcpus: 3\n  memory_mb: 64\n  network:\n    guest_ip: 172.16.0.9\n")
	a.tick(context.Background())
	if manager.instances["d"] != nil || manager.instances["k"] == nil {
		t.Fatalf("setup: d=%v k=%v", manager.instances["d"], manager.instances["k"])
	}
	if body, ok := readRoute(t, routeDir, "d.yaml"); ok {
		t.Fatalf("deleted d still routes:\n%s", body)
	}
	if body, ok := readRoute(t, routeDir, "k.yaml"); !ok || !strings.Contains(body, "Host(`k.example.com`)") {
		t.Fatalf("k has no route:\n%s", body)
	}
	if status := a.agentStatusSnapshot(); status.Phase != "failed" || status.ReasonCode != "capacity_exceeded" {
		t.Fatalf("over-capacity revision reported %s/%s", status.Phase, status.ReasonCode)
	}
}

// A hostname changed without replacing the VM stays published on the
// unchanged-revision path that follows the full tick.
func TestRouteOnlyTickKeepsCommittedHostname(t *testing.T) {
	yaml := "node: web\nservices:\n- name: app\n  image: /image\n  kernel: /kernel\n  env:\n    FOO: bar\n  network:\n    guest_ip: 172.16.0.2\n  port_forwards:\n  - host_port: 8080\n    vm_port: 80\n  metadata:\n    host: new.example.com\n"
	store := &fakeStore{revision: "r2", data: map[string][]byte{"web": []byte(yaml)}}
	a := New(testAgentConfig(t), store, testLogger())
	running := config.ServiceConfig{Name: "app", Image: "/image", Kernel: "/kernel", Env: map[string]string{"FOO": "bar"}, KernelArgs: "firework.env.FOO=bar", Network: &config.NetworkConfig{GuestIP: "172.16.0.2"}, PortForwards: []config.PortForward{{HostPort: 8080, VMPort: 80}}, Metadata: map[string]string{"host": "old.example.com"}}
	manager := &recoveryTestVMManager{instances: map[string]*vm.Instance{"app": {Name: "app", Config: running, State: vm.StateRunning}}}
	a.reconciler = reconciler.NewWithNetworkManager(manager, testLogger(), nil, &recoveryTestNetworkManager{}, "", 0)
	routeDir := t.TempDir()
	a.traefikMgr = traefik.NewManager(routeDir, "", testLogger())
	a.lastRevision = "r1"
	for tick := 0; tick < 2; tick++ {
		a.tick(context.Background())
		if body, _ := readRoute(t, routeDir, "app.yaml"); !strings.Contains(body, "Host(`new.example.com`)") {
			t.Fatalf("tick %d published:\n%s", tick, body)
		}
	}
	if len(manager.startCalls) != 0 || len(manager.removeCalls) != 0 {
		t.Fatalf("metadata-only change recreated the VM: starts=%v removes=%v", manager.startCalls, manager.removeCalls)
	}
}
