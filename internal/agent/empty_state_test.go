package agent

import (
	"context"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/healthcheck"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/vm"
)

// statefulVMManager tracks started VMs so a later tick observes what an
// earlier one created and a delete actually removes it.
type statefulVMManager struct {
	running map[string]config.ServiceConfig
	removed []string
}

func newStatefulVMManager() *statefulVMManager {
	return &statefulVMManager{running: make(map[string]config.ServiceConfig)}
}

func (m *statefulVMManager) List() map[string]*vm.Instance {
	out := make(map[string]*vm.Instance, len(m.running))
	for name, svc := range m.running {
		out[name] = &vm.Instance{Name: name, State: vm.StateRunning, Config: svc}
	}
	return out
}

func (m *statefulVMManager) Start(_ context.Context, svc config.ServiceConfig) error {
	m.running[svc.Name] = svc
	return nil
}

func (m *statefulVMManager) Remove(name string) error {
	delete(m.running, name)
	m.removed = append(m.removed, name)
	return nil
}

// recordingNetwork records every host-resource teardown.
type recordingNetwork struct {
	fakeNetwork
	deletedTAPs       []string
	tornDownHostPorts []int
}

func (r *recordingNetwork) DeleteTAP(name string) error {
	r.deletedTAPs = append(r.deletedTAPs, name)
	return nil
}

func (r *recordingNetwork) TeardownPortForward(hostPort int, _ string, _ int) error {
	r.tornDownHostPorts = append(r.tornDownHostPorts, hostPort)
	return nil
}

const runningAPIConfig = `node: web
services:
  - name: api
    image: /img/api
    kernel: /kernel
    vcpus: 1
    memory_mb: 256
    network:
      interface: tap-api
    port_forwards:
      - host_port: 8080
        vm_port: 80
    health_check:
      type: tcp
      port: 80
      interval: 1h
`

type emptyStateHarness struct {
	agent  *Agent
	store  *fakeStore
	vms    *statefulVMManager
	net    *recordingNetwork
	health *healthcheck.Monitor
	routes *fakeRouteSyncer
}

// newRunningAPIHarness returns an agent that has applied rev-1, which runs
// one networked, port-forwarded, health-checked service.
func newRunningAPIHarness(t *testing.T) *emptyStateHarness {
	t.Helper()
	cfg := testAgentConfig(t)
	cfg.VMSubnet = "172.16.0.0/24"
	cfg.VMGateway = "172.16.0.1"
	h := &emptyStateHarness{
		store:  &fakeStore{data: map[string][]byte{"web": []byte(runningAPIConfig)}, revision: "rev-1"},
		vms:    newStatefulVMManager(),
		net:    &recordingNetwork{},
		health: healthcheck.NewMonitor(testLogger(), nil),
		routes: &fakeRouteSyncer{},
	}
	t.Cleanup(h.health.Stop)
	h.agent = New(cfg, h.store, testLogger())
	h.agent.reconciler = reconciler.NewWithNetworkManager(h.vms, testLogger(), h.health, h.net, "", 0).WithStateDir(cfg.StateDir)
	h.agent.traefikMgr = h.routes

	h.agent.tick(context.Background())
	if _, ok := h.vms.running["api"]; !ok {
		t.Fatalf("setup: api was not started; status %#v", h.agent.agentStatusSnapshot())
	}
	if _, ok := h.health.GetResult("api"); !ok {
		t.Fatal("setup: api health check was not registered")
	}
	if h.agent.lastRevision != "rev-1" {
		t.Fatalf("setup: rev-1 not applied, got %q", h.agent.lastRevision)
	}
	return h
}

func lastSyncedServices(routes *fakeRouteSyncer) []config.ServiceConfig {
	if len(routes.services) == 0 {
		return nil
	}
	return routes.services[len(routes.services)-1]
}

// Removing the final service from a node is an explicit empty config, and it
// must take everything the service had on the host with it: the VM, its
// health check, its port forwards, its tap device, and its Traefik route.
// Re-reading the same empty config afterwards changes nothing.
func TestTick_ExplicitEmptyConfigRemovesTheLastService(t *testing.T) {
	h := newRunningAPIHarness(t)

	h.store.data["web"] = []byte("node: web\nservices: []\n")
	h.store.revision = "rev-2"
	before := len(h.routes.services)
	h.agent.tick(context.Background())

	if len(h.vms.running) != 0 || len(h.vms.removed) != 1 || h.vms.removed[0] != "api" {
		t.Fatalf("api VM was not removed: running=%v removed=%v", h.vms.running, h.vms.removed)
	}
	if _, ok := h.health.GetResult("api"); ok {
		t.Fatal("api health check is still registered")
	}
	if len(h.net.tornDownHostPorts) != 1 || h.net.tornDownHostPorts[0] != 8080 {
		t.Fatalf("api port forward was not torn down: %v", h.net.tornDownHostPorts)
	}
	if len(h.net.deletedTAPs) != 1 || h.net.deletedTAPs[0] != "tap-api" {
		t.Fatalf("api tap device was not deleted: %v", h.net.deletedTAPs)
	}
	if len(h.routes.services) <= before || len(lastSyncedServices(h.routes)) != 0 {
		t.Fatalf("local routes were not synced to empty: %#v", h.routes.services)
	}
	if h.agent.lastRevision != "rev-2" {
		t.Fatalf("empty revision was not recorded as applied, got %q", h.agent.lastRevision)
	}

	syncs := len(h.routes.services)
	h.agent.tick(context.Background())
	if len(h.vms.removed) != 1 || len(h.net.deletedTAPs) != 1 || len(h.net.tornDownHostPorts) != 1 {
		t.Fatalf("repeated empty reconcile was not idempotent: removed=%v taps=%v ports=%v",
			h.vms.removed, h.net.deletedTAPs, h.net.tornDownHostPorts)
	}
	if len(h.routes.services) <= syncs || len(lastSyncedServices(h.routes)) != 0 {
		t.Fatalf("repeated empty reconcile did not keep routes empty: %#v", h.routes.services)
	}
	if h.agent.lastRevision != "rev-2" {
		t.Fatalf("repeated empty reconcile changed the applied revision to %q", h.agent.lastRevision)
	}
}

// Desired state that cannot be read is unknown, not empty. Every one of these
// must preserve the running service and its routes and leave the applied
// revision where it was, so the agent retries instead of stopping workloads.
func TestTick_UnknownDesiredStatePreservesRunningServices(t *testing.T) {
	tests := []struct {
		name string
		data *string
	}{
		{name: "fetch failure"},
		{name: "empty object", data: ptr("")},
		{name: "omitted services", data: ptr("node: web\n")},
		{name: "null services", data: ptr("node: web\nservices: null\n")},
		{name: "invalid yaml", data: ptr("{not yaml")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRunningAPIHarness(t)
			if tt.data == nil {
				delete(h.store.data, "web")
			} else {
				h.store.data["web"] = []byte(*tt.data)
			}
			h.store.revision = "rev-2"
			syncs := len(h.routes.services)

			h.agent.tick(context.Background())

			if _, ok := h.vms.running["api"]; !ok || len(h.vms.removed) != 0 {
				t.Fatalf("api was stopped on unknown desired state: running=%v removed=%v", h.vms.running, h.vms.removed)
			}
			if len(h.net.deletedTAPs) != 0 || len(h.net.tornDownHostPorts) != 0 {
				t.Fatalf("host resources were torn down: taps=%v ports=%v", h.net.deletedTAPs, h.net.tornDownHostPorts)
			}
			if _, ok := h.health.GetResult("api"); !ok {
				t.Fatal("api health check was deregistered")
			}
			if len(h.routes.services) != syncs || h.routes.localCalls != 0 {
				t.Fatalf("routes were re-synced from unknown desired state: %#v", h.routes.services)
			}
			if h.agent.lastRevision != "rev-1" {
				t.Fatalf("applied revision advanced to %q", h.agent.lastRevision)
			}
		})
	}
}

func ptr(s string) *string { return &s }
