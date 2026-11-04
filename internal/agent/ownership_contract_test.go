package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/artemnikitin/firework/internal/capacity"
	netpkg "github.com/artemnikitin/firework/internal/network"
	"github.com/artemnikitin/firework/internal/network/networktest"
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
// port at its old address, and another service takes its old port. Neither
// starts while the old rule could deliver the second service's traffic to
// the first one's guest; both start once it is gone.
// (Reproducer from the review of 5392d54.)
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
	network.kernel.Append(networktest.Rule(netpkg.PortForwardTag("a"), 8080, "172.16.0.3", 80))
	a.reconciler = testReconciler(t, m, network, "", 0)
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
	ai, bi := m.instances["a"], m.instances["b"]
	if ai == nil || bi == nil {
		t.Fatalf("a=%v b=%v did not start once the rule was gone", ai, bi)
	}
	if rule, ok := network.kernel.Delivers(8080); !ok || rule.GuestIP != bi.Config.Network.GuestIP || !rule.TaggedFor("b") {
		t.Fatalf("host port 8080 does not reach b: %v", network.kernel.Rules())
	}
	if len(violations) != 0 {
		t.Fatal(violations)
	}
}
