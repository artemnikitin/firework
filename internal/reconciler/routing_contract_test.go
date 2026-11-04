package reconciler

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/network"
	"github.com/artemnikitin/firework/internal/network/networktest"
	"github.com/artemnikitin/firework/internal/vm"
)

// dnatTable models the host's live DNAT rules, so a test can see which guest
// a host port reaches at the moment another VM's rules are set up.
type dnatTable struct {
	fakeNetworkManager
	teardownFailFor map[int]bool
	// staleAtSetup records each live rule from another host port that already
	// targeted a guest IP when a rule for that guest was set up.
	staleAtSetup []string
}

// has reports whether a rule forwards hostPort to ip:vmPort.
func (d *dnatTable) has(hostPort int, ip string, vmPort int) bool {
	for _, rule := range d.host().Rules() {
		if rule.Targets(hostPort, ip, vmPort) {
			return true
		}
	}
	return false
}

func (d *dnatTable) SetupPortForward(service string, hostPort int, ip string, vmPort int) error {
	for _, rule := range d.host().RulesTo(ip) {
		if rule.HostPort != hostPort {
			d.staleAtSetup = append(d.staleAtSetup, fmt.Sprintf("%d->%s:%d", rule.HostPort, rule.GuestIP, rule.VMPort))
		}
	}
	return d.fakeNetworkManager.SetupPortForward(service, hostPort, ip, vmPort)
}

func (d *dnatTable) DeletePortForwardRule(rule network.PortForwardRule) error {
	if d.teardownFailFor[rule.HostPort] {
		d.teardownForwardCalls = append(d.teardownForwardCalls, rule.HostPort)
		return errors.New("iptables: resource temporarily unavailable")
	}
	return d.fakeNetworkManager.DeletePortForwardRule(rule)
}

// C4: remove A while an unknown-config quarantine defers its host cleanup,
// then offer A's guest IP to B. B must not start while A's host rule still
// forwards to that guest, however long the teardown keeps failing, and it
// starts once the rule is gone.
func TestC4RemovedServiceAddressWaitsForItsRulesToGo(t *testing.T) {
	for _, failTeardown := range []bool{false, true} {
		t.Run(fmt.Sprintf("teardown fails=%v", failTeardown), func(t *testing.T) {
			network := &dnatTable{teardownFailFor: map[int]bool{}}
			r, manager := newNetworkTestReconciler(network)
			unknown := &vm.Instance{Name: "unknown", Config: config.ServiceConfig{Name: "unknown"}, State: vm.StateRecoveryPending, RecoveryConfigUnknown: true}
			manager.instances[unknown.Name] = unknown
			a := config.ServiceConfig{Name: "a", Network: &config.NetworkConfig{GuestIP: "172.16.0.3"}, PortForwards: []config.PortForward{{HostPort: 8080, VMPort: 80}}}
			manager.instances[a.Name] = &vm.Instance{Name: a.Name, Config: a, State: vm.StateRunning}
			network.host().Append(networktest.Rule("", 8080, "172.16.0.3", 80))
			b := config.ServiceConfig{Name: "b", Network: &config.NetworkConfig{GuestIP: "172.16.0.3"}, PortForwards: []config.PortForward{{HostPort: 9090, VMPort: 80}}}
			_ = r.Reconcile(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{unknown.Config, b}})
			if manager.instances[a.Name] != nil || manager.instances[b.Name] != nil || !network.has(8080, "172.16.0.3", 80) {
				t.Fatalf("setup: a=%v b=%v rules=%v", manager.instances[a.Name], manager.instances[b.Name], network.host().Rules())
			}
			delete(manager.instances, unknown.Name) // Resolved, for example by --forget.
			network.teardownFailFor[8080] = failTeardown
			desired := config.NodeConfig{Services: []config.ServiceConfig{b}}
			for tick := 0; tick < 3; tick++ {
				_ = r.Reconcile(context.Background(), desired)
				if manager.instances[b.Name] != nil && network.has(8080, "172.16.0.3", 80) {
					t.Fatalf("tick %d: B started while A's 8080 rule still forwards to its guest", tick)
				}
			}
			network.teardownFailFor[8080] = false
			for tick := 0; tick < 2; tick++ {
				_ = r.Reconcile(context.Background(), desired)
			}
			if len(network.staleAtSetup) != 0 {
				t.Fatalf("B's rules were set up beside A's: %v", network.staleAtSetup)
			}
			if manager.instances[b.Name] == nil || network.has(8080, "172.16.0.3", 80) {
				t.Fatalf("did not converge: b=%v rules=%v", manager.instances[b.Name], network.host().Rules())
			}
		})
	}
}
