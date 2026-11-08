package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/healthcheck"
	"github.com/artemnikitin/firework/internal/network"
	"github.com/artemnikitin/firework/internal/network/networktest"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/vm"
)

func TestRecoveryWakeBypassesUnchangedRevisionAndRetriesRestoration(t *testing.T) {
	nodeYAML := []byte(`node: web
services:
  - name: app
    image: /image
    kernel: /kernel
    vcpus: 1
    memory_mb: 128
    network:
      interface: tap-app
    port_forwards:
      - host_port: 8080
        vm_port: 80
    health_check:
      type: tcp
      target: 127.0.0.1:1
      interval: 1h
      timeout: 1s
`)
	store := &fakeStore{data: map[string][]byte{"web": nodeYAML}, revision: "rev-1"}
	a := New(testAgentConfig(t), store, testLogger())
	configData, err := config.ParseNodeConfig(nodeYAML)
	if err != nil {
		t.Fatal(err)
	}
	a.assignNetworking(configData.Services)
	service := configData.Services[0]
	manager := &recoveryTestVMManager{instances: map[string]*vm.Instance{
		service.Name: {Name: service.Name, Config: service, State: vm.StateRunning, PID: 42},
	}}
	network := &recoveryTestNetworkManager{}
	health := healthcheck.NewMonitor(testLogger(), func(context.Context, string) error { return nil })
	t.Cleanup(health.Stop)
	a.healthMon = health
	a.reconciler = reconciler.NewWithNetworkManager(manager, testLogger(), health, network, "", 0)

	a.reconciler.MarkRecovered(service.Name)
	a.tick(context.Background())
	if a.lastRevision != "rev-1" {
		t.Fatalf("baseline reconciliation did not apply revision: %q", a.lastRevision)
	}
	if len(network.setupCalls) != 1 {
		t.Fatalf("baseline recovery network setup calls = %v", network.setupCalls)
	}
	if _, ok := health.GetResult(service.Name); !ok {
		t.Fatal("baseline recovery did not populate health state")
	}
	baselineFetches := store.fetchCount
	baselineSetupCalls := len(network.setupCalls)

	network.setupErr = errors.New("transient network restore failure")
	a.queueRecoveryReconcile(service.Name, true)
	a.applyRecoveryRequests(context.Background())
	if !a.forceReconcile {
		t.Fatal("failed recovery restoration cleared the forced-reconcile state")
	}
	if store.fetchCount != baselineFetches+1 || len(network.setupCalls) != baselineSetupCalls+1 {
		t.Fatalf("recovery wake did not bypass the unchanged-revision cache: fetches=%d setup=%v", store.fetchCount, network.setupCalls)
	}
	if _, ok := health.GetResult(service.Name); !ok {
		t.Fatal("failed network restoration discarded the existing health result")
	}

	network.setupErr = nil
	a.tick(context.Background())
	if a.forceReconcile {
		t.Fatal("successful recovery restoration kept forcing reconciliation")
	}
	if store.fetchCount != baselineFetches+2 || len(network.setupCalls) != baselineSetupCalls+2 {
		t.Fatalf("retry did not restore the recovered service: fetches=%d setup=%v", store.fetchCount, network.setupCalls)
	}
	if _, ok := health.GetResult(service.Name); !ok {
		t.Fatal("successful retry did not retain health monitoring")
	}
	if len(manager.startCalls) != 0 || len(manager.removeCalls) != 0 {
		t.Fatalf("recovery retry restarted a VM whose config had not changed: starts=%v removes=%v", manager.startCalls, manager.removeCalls)
	}
}

type recoveryTestVMManager struct {
	instances   map[string]*vm.Instance
	startCalls  []string
	removeCalls []string
	// limit, when set, makes every start check C7: a VM may start only into
	// capacity free at that moment. overcommits records each start that left
	// a resource above the limit and above what the node used before the
	// operation began, so excess the node already had is not blamed on it.
	limit       *capacity.NodeCapacity
	overcommits []string
	// pending is the usage before the last Remove or Stop. A replacement or
	// restart starts the same service right after, completing that operation.
	pending *pendingStart
	// hidden is the size a quarantined VM of unknown configuration really
	// uses: the agent sees no size for it, but the node's usage includes it.
	hidden map[string]capacity.NodeCapacity
	// startHook runs before a start takes effect, to change the runtime
	// mid-tick as a health restart or a lifecycle transition would. An error
	// fails the start. removeHook does the same for a removal, which keeps
	// its VM when it fails.
	startHook  func(name string) error
	removeHook func(name string) error
	// record is the reconciler's recovery cleanup recorder.
	record func(config.ServiceConfig) error
}

func (m *recoveryTestVMManager) SetRecoveryCleanupRecorder(record func(config.ServiceConfig) error) {
	m.record = record
}

// clear removes a quarantined VM as clear-recovery does: a trusted
// configuration is recorded for cleanup before the VM's state goes.
func (m *recoveryTestVMManager) clear(t *testing.T, name string) {
	t.Helper()
	if instance := m.instances[name]; instance != nil && !instance.RecoveryConfigUnknown && m.record != nil {
		if err := m.record(instance.Config); err != nil {
			t.Fatalf("recording cleanup for %s: %v", name, err)
		}
	}
	delete(m.instances, name)
}

func (m *recoveryTestVMManager) usage() capacity.NodeCapacity {
	vcpus, memoryMB := runtimeUsage(m)
	return capacity.NodeCapacity{VCPUs: vcpus, MemoryMB: memoryMB}
}

type pendingStart struct {
	name  string
	usage capacity.NodeCapacity
}

func (m *recoveryTestVMManager) beginReplacement(name string) {
	m.pending = &pendingStart{name, m.usage()}
}

func (m *recoveryTestVMManager) Stop(name string) error {
	m.beginReplacement(name)
	if instance := m.instances[name]; instance != nil {
		instance.State = vm.StateStopped
	}
	return nil
}

func (m *recoveryTestVMManager) List() map[string]*vm.Instance {
	instances := make(map[string]*vm.Instance, len(m.instances))
	for name, instance := range m.instances {
		copy := *instance
		if copy.InstanceID == "" {
			copy.InstanceID = fmt.Sprintf("fake-runtime-%p", instance)
		}
		instances[name] = &copy
	}
	return instances
}

func (m *recoveryTestVMManager) Start(_ context.Context, service config.ServiceConfig) error {
	if m.startHook != nil {
		if err := m.startHook(service.Name); err != nil {
			return err
		}
	}
	before := m.usage()
	if m.pending != nil && m.pending.name == service.Name {
		before = m.pending.usage
	}
	m.pending = nil
	m.startCalls = append(m.startCalls, service.Name)
	m.instances[service.Name] = &vm.Instance{Name: service.Name, Config: service, State: vm.StateRunning}
	if m.limit != nil {
		after := m.usage()
		if after.VCPUs > m.limit.VCPUs && after.VCPUs > before.VCPUs || after.MemoryMB > m.limit.MemoryMB && after.MemoryMB > before.MemoryMB {
			m.overcommits = append(m.overcommits, fmt.Sprintf("start %s: %d vCPUs/%d MB, was %d/%d, limit %d/%d",
				service.Name, after.VCPUs, after.MemoryMB, before.VCPUs, before.MemoryMB, m.limit.VCPUs, m.limit.MemoryMB))
		}
	}
	return nil
}

// CommitConfig mirrors vm.Manager: in-place changes land on running VMs only.
func (m *recoveryTestVMManager) CommitConfig(name string, service config.ServiceConfig) error {
	if instance := m.instances[name]; instance != nil && instance.State == vm.StateRunning {
		instance.Config = service
	}
	return nil
}

func (m *recoveryTestVMManager) Remove(service string) error {
	m.beginReplacement(service)
	if m.removeHook != nil {
		if err := m.removeHook(service); err != nil {
			return err
		}
	}
	m.removeCalls = append(m.removeCalls, service)
	delete(m.instances, service)
	return nil
}

type recoveryTestNetworkManager struct {
	setupCalls      []string
	portForwardCall []int
	setupErr        error
	// teardownErr fails every port-forward teardown while set.
	teardownErr error
	kernel      networktest.Kernel
}

func (m *recoveryTestNetworkManager) Setup(service config.ServiceConfig) error {
	m.setupCalls = append(m.setupCalls, service.Name)
	return m.setupErr
}

func (*recoveryTestNetworkManager) Teardown(config.ServiceConfig) error { return nil }

func (*recoveryTestNetworkManager) DeleteTAP(string) error { return nil }

func (*recoveryTestNetworkManager) DeleteBridge(string) error { return nil }

func (m *recoveryTestNetworkManager) SetupPortForward(service string, hostPort int, guestIP string, vmPort int) error {
	m.portForwardCall = append(m.portForwardCall, hostPort)
	return m.kernel.SetupPortForward(service, hostPort, guestIP, vmPort)
}

func (m *recoveryTestNetworkManager) ListPortForwards() (network.PortForwardListing, error) {
	return m.kernel.ListPortForwards()
}

func (m *recoveryTestNetworkManager) DeletePortForwardRule(rule network.PortForwardRule) error {
	if m.teardownErr != nil {
		return m.teardownErr
	}
	return m.kernel.DeletePortForwardRule(rule)
}

func TestClearedRemovedServiceDropsHealthProbe(t *testing.T) {
	a := New(testAgentConfig(t), &fakeStore{data: map[string][]byte{"web": []byte("node: web\nservices: []\n")}, revision: "empty"}, testLogger())
	manager := &recoveryTestVMManager{instances: map[string]*vm.Instance{}}
	health := healthcheck.NewMonitor(testLogger(), func(context.Context, string) error { return nil })
	defer health.Stop()
	service := config.ServiceConfig{Name: "old", HealthCheck: &config.HealthCheckConfig{Type: "tcp", Target: "127.0.0.1:1", Interval: time.Hour, Timeout: time.Second}}
	health.Register(context.Background(), service)
	a.reconciler = reconciler.NewWithNetworkManager(manager, testLogger(), health, &recoveryTestNetworkManager{}, "", 0)
	a.queueRecoveryReconcile("old", false)
	a.applyRecoveryRequests(context.Background())
	if _, ok := health.GetResult("old"); ok {
		t.Fatal("cleared service retained its health probe")
	}
}
