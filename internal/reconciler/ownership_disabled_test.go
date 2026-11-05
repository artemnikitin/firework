package reconciler

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/network"
)

// C4: turning off host networking cannot release an address whose old DNAT
// rule still delivers traffic. The journal keeps the fence until networking
// is enabled again and cleanup proves the rule gone. Other addresses proceed.
func TestDisabledNetworkingKeepsRecordedAddressFences(t *testing.T) {
	for _, withRoutes := range []bool{false, true} {
		t.Run(fmt.Sprintf("routes=%v", withRoutes), func(t *testing.T) {
			h := newCrashHost(t)
			r := h.agent()
			a := crashService("a", "172.16.0.2", 8080)
			h.converge(r, config.NodeConfig{Services: []config.ServiceConfig{a}})
			h.kernel.DeleteErr = func(network.PortForwardRule) error { return fmt.Errorf("iptables unavailable") }
			_ = r.Reconcile(context.Background(), config.NodeConfig{})
			if h.vms.instances["a"] != nil || len(recordsIn(r, phaseDeparting)) == 0 {
				t.Fatal("setup did not retain A's cleanup after its VM was removed")
			}
			h.kernel.DeleteErr = nil
			build := func() *Reconciler {
				disabled := NewWithNetworkManager(h.vms, r.logger, nil, nil, "", 0)
				if withRoutes {
					disabled.WithRouteEvidence(&fakeRoutes{boot: "boot", start: "100"})
				}
				return disabled.WithStateDir(h.stateDir)
			}
			b := crashService("b", "172.16.0.2", 9090)
			b.PortForwards = nil
			other := crashService("other", "172.16.0.3", 9091)
			other.PortForwards = nil
			desired := config.NodeConfig{Services: []config.ServiceConfig{b, other}}
			for restart := 0; restart < 2; restart++ {
				disabled := build()
				for tick := 0; tick < 2; tick++ {
					err := disabled.Reconcile(context.Background(), desired)
					rule, delivers := h.kernel.Delivers(8080)
					if !IsIncomplete(err) || h.vms.instances["b"] != nil || !delivers || rule.GuestIP != b.Network.GuestIP {
						t.Fatalf("A's address fence was lost: B=%+v rule=%+v err=%v", h.vms.instances["b"], rule, err)
					}
					if reason := disabled.HeldServices(desired)["b"]; !strings.Contains(reason, "recorded for service a") {
						t.Fatalf("the address hold is missing from status: %q", reason)
					}
					if h.vms.instances["other"] == nil {
						t.Fatal("an independent address was held")
					}
				}
			}
			// Enabling networking resumes the original cleanup, then B starts
			// on the unchanged desired config without inheriting A's traffic.
			h.converge(h.agent(), desired)
			if h.vms.instances["b"] == nil {
				t.Fatal("B did not start after cleanup resumed")
			}
			if _, delivers := h.kernel.Delivers(8080); delivers {
				t.Fatal("B inherited A's traffic")
			}
		})
	}
}

// C4/L6: turning off setup also keeps the known holder of a host port,
// including after its VM and rules are gone. Unrecorded ports remain usable
// in this mode, which installs no rules and records the interval as untracked.
func TestDisabledNetworkingKeepsRecordedPortHolders(t *testing.T) {
	h := newCrashHost(t)
	r := h.agent()
	h.converge(r, config.NodeConfig{Services: []config.ServiceConfig{crashService("a", "172.16.0.2", 8080)}})
	h.converge(r, config.NodeConfig{})
	disabled := NewWithNetworkManager(h.vms, r.logger, nil, nil, "", 0).WithStateDir(h.stateDir)
	b := crashService("b", "172.16.0.3", 8080)
	desired := config.NodeConfig{Services: []config.ServiceConfig{b}}
	_ = disabled.Reconcile(context.Background(), desired)
	if hold := disabled.HeldServiceHolds(desired)["b"]; h.vms.instances["b"] != nil || hold.Action != ActionHostPortReuseBlocked {
		t.Fatalf("B took A's known host port: B=%+v hold=%+v", h.vms.instances["b"], hold)
	}
	b.PortForwards[0].HostPort = 8081
	h.converge(disabled, config.NodeConfig{Services: []config.ServiceConfig{b}})
}
