package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/statusmodel"
	"github.com/artemnikitin/firework/internal/vm"
)

func classificationAgent(t *testing.T, yaml string, instances map[string]*vm.Instance, vcpus int) (*Agent, *recoveryTestVMManager) {
	t.Helper()
	cfg := testAgentConfig(t)
	enabled := true
	cfg.EnableCapacityCheck = &enabled
	a := New(cfg, &fakeStore{revision: "r1", data: map[string][]byte{"web": []byte(yaml)}}, testLogger())
	a.capacityReader = &fakeCapacityReader{cap: capacity.NodeCapacity{VCPUs: vcpus, MemoryMB: 4096}}
	manager := &recoveryTestVMManager{instances: instances}
	a.reconciler = reconciler.NewWithNetworkManager(manager, testLogger(), nil, &recoveryTestNetworkManager{}, "", 0)
	return a, manager
}

func conditionOf(status statusmodel.AgentStatus, kind string) statusmodel.Condition {
	for _, condition := range status.Conditions {
		if condition.Type == kind {
			return condition
		}
	}
	return statusmodel.Condition{}
}

// Desired services that do not fit fail the revision the same way whether or
// not any VM happens to be retained, while the services that fit converge.
func TestOverCapacityFailsRevisionWhateverIsRetained(t *testing.T) {
	yaml := "node: web\nservices:\n- name: a\n  image: /a\n  kernel: /k\n  vcpus: 2\n  memory_mb: 64\n- name: b\n  image: /b\n  kernel: /k\n  vcpus: 3\n  memory_mb: 64\n"
	for _, retained := range []bool{false, true} {
		t.Run(map[bool]string{false: "nothing retained", true: "a VM stopping"}[retained], func(t *testing.T) {
			instances := map[string]*vm.Instance{}
			if retained {
				instances["stuck"] = &vm.Instance{Name: "stuck", Config: config.ServiceConfig{Name: "stuck"}, State: vm.StateStopping}
			}
			a, manager := classificationAgent(t, yaml, instances, 4)
			a.tick(context.Background())
			if manager.instances["a"] == nil || manager.instances["b"] != nil {
				t.Fatalf("admission: starts=%v", manager.startCalls)
			}
			status := a.agentStatusSnapshot()
			if status.Phase != statusmodel.PhaseFailed || status.ReasonCode != "capacity_exceeded" || a.lastRevision != "" {
				t.Fatalf("phase=%s reason=%s lastRevision=%q", status.Phase, status.ReasonCode, a.lastRevision)
			}
			if c := conditionOf(status, "CapacityReady"); c.Status != statusmodel.ConditionFalse || !strings.Contains(c.Message, "b") {
				t.Fatalf("CapacityReady = %#v", c)
			}
			if c := conditionOf(status, "VMsReconciled"); c.ReasonCode != "resource_held" {
				t.Fatalf("VMsReconciled = %#v", c)
			}
		})
	}
}

// A running VM the plan removes does not keep its allocation: replacing a
// service on a full node happens within one tick, as on main.
func TestReplacementOnFullNodeConvergesInOneTick(t *testing.T) {
	yaml := "node: web\nservices:\n- name: next\n  image: /n\n  kernel: /k\n  vcpus: 4\n  memory_mb: 64\n"
	old := config.ServiceConfig{Name: "old", Image: "/o", Kernel: "/k", VCPUs: 4, MemoryMB: 64}
	a, manager := classificationAgent(t, yaml, map[string]*vm.Instance{"old": {Name: "old", Config: old, State: vm.StateRunning}}, 4)
	a.tick(context.Background())
	if manager.instances["old"] != nil || manager.instances["next"] == nil || a.lastRevision != "r1" {
		t.Fatalf("old=%v next=%v lastRevision=%q", manager.instances["old"], manager.instances["next"], a.lastRevision)
	}
}

// Capacity still allocated to a VM that is stopping is a transient hold: the
// revision keeps reconciling, then applies once the VM is gone.
func TestCapacityOfStoppingVMIsATransientHold(t *testing.T) {
	yaml := "node: web\nservices:\n- name: next\n  image: /n\n  kernel: /k\n  vcpus: 4\n  memory_mb: 64\n"
	old := config.ServiceConfig{Name: "old", Image: "/o", Kernel: "/k", VCPUs: 4, MemoryMB: 64}
	a, manager := classificationAgent(t, yaml, map[string]*vm.Instance{"old": {Name: "old", Config: old, State: vm.StateStopping}}, 4)
	a.tick(context.Background())
	status := a.agentStatusSnapshot()
	if manager.instances["next"] != nil {
		t.Fatal("next started while the stopping VM still holds the capacity")
	}
	if status.Phase != statusmodel.PhaseReconciling || conditionOf(status, "CapacityReady").Status != statusmodel.ConditionTrue ||
		conditionOf(status, "VMsReconciled").ReasonCode != "resource_held" {
		t.Fatalf("transient hold reported phase=%s capacity=%#v vms=%#v", status.Phase, conditionOf(status, "CapacityReady"), conditionOf(status, "VMsReconciled"))
	}
	delete(manager.instances, "old") // the stop completed
	a.tick(context.Background())
	if manager.instances["next"] == nil || a.lastRevision != "r1" {
		t.Fatalf("second tick: next=%v lastRevision=%q", manager.instances["next"], a.lastRevision)
	}
}

// A subnet without a free address fails the revision too.
func TestExhaustedSubnetFailsRevision(t *testing.T) {
	yaml := "node: web\nservices:\n- name: a\n  image: /a\n  kernel: /k\n  network: {}\n- name: b\n  image: /b\n  kernel: /k\n  network: {}\n"
	a, manager := classificationAgent(t, yaml, map[string]*vm.Instance{}, 8)
	a.cfg.VMSubnet, a.cfg.VMGateway = "172.16.0.0/30", "172.16.0.1/30"
	a.tick(context.Background())
	if manager.instances["a"] == nil || manager.instances["b"] != nil {
		t.Fatalf("admission: starts=%v", manager.startCalls)
	}
	status := a.agentStatusSnapshot()
	if status.Phase != statusmodel.PhaseFailed || status.ReasonCode != "guest_addresses_exhausted" {
		t.Fatalf("phase=%s reason=%s", status.Phase, status.ReasonCode)
	}
}
