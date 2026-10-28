package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/healthcheck"
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
}

func (m *recoveryTestVMManager) List() map[string]*vm.Instance {
	instances := make(map[string]*vm.Instance, len(m.instances))
	for name, instance := range m.instances {
		instances[name] = instance
	}
	return instances
}

func (m *recoveryTestVMManager) Start(_ context.Context, service config.ServiceConfig) error {
	m.startCalls = append(m.startCalls, service.Name)
	m.instances[service.Name] = &vm.Instance{Name: service.Name, Config: service, State: vm.StateRunning}
	return nil
}

func (m *recoveryTestVMManager) Remove(service string) error {
	m.removeCalls = append(m.removeCalls, service)
	delete(m.instances, service)
	return nil
}

type recoveryTestNetworkManager struct {
	setupCalls      []string
	portForwardCall []int
	setupErr        error
}

func (m *recoveryTestNetworkManager) Setup(service config.ServiceConfig) error {
	m.setupCalls = append(m.setupCalls, service.Name)
	return m.setupErr
}

func (*recoveryTestNetworkManager) Teardown(config.ServiceConfig) error { return nil }

func (*recoveryTestNetworkManager) DeleteTAP(string) error { return nil }

func (*recoveryTestNetworkManager) DeleteBridge(string) error { return nil }

func (m *recoveryTestNetworkManager) SetupPortForward(hostPort int, _ string, _ int) error {
	m.portForwardCall = append(m.portForwardCall, hostPort)
	return nil
}

func (*recoveryTestNetworkManager) TeardownPortForward(int, string, int) error { return nil }

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
