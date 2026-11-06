package reconciler

import (
	"context"
	"errors"
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

func TestDisabledNetworkingRecordsRemovalBeforeReadding(t *testing.T) {
	h := newCrashHost(t)
	a := crashService("a", "172.16.0.2", 8080)
	enabled := h.agent()
	h.converge(enabled, config.NodeConfig{Services: []config.ServiceConfig{a}})
	disabled := NewWithNetworkManager(h.vms, enabled.logger, nil, nil, "", 0).WithStateDir(h.stateDir)
	h.converge(disabled, config.NodeConfig{})
	if len(recordsIn(disabled, phaseDeparting)) == 0 {
		t.Fatal("the removed VM still has a running allocation")
	}
	if _, ok := h.kernel.Delivers(8080); !ok {
		t.Fatal("disabled networking released the old rules")
	}
	// The phase change must survive an agent restart, and the same service
	// may reclaim precisely its own address and ports while cleanup waits.
	disabled = NewWithNetworkManager(h.vms, enabled.logger, nil, nil, "", 0).WithStateDir(h.stateDir)
	h.converge(disabled, config.NodeConfig{Services: []config.ServiceConfig{a}})
	if h.vms.instances[a.Name] == nil {
		t.Fatal("re-adding the service remained blocked")
	}
}

func TestDisabledNetworkingForgetRecordsExistingTraces(t *testing.T) {
	h := newCrashHost(t)
	a := crashService("a", "172.16.0.2", 8080)
	enabled := h.agent()
	h.converge(enabled, config.NodeConfig{Services: []config.ServiceConfig{a}})
	delete(h.vms.instances, a.Name) // Model the disappearance requiring --forget.
	disabled := NewWithNetworkManager(h.vms, enabled.logger, nil, nil, "", 0).WithStateDir(h.stateDir)
	if found, err := disabled.HasUnresolvedTraces(a.Name); err != nil || !found {
		t.Fatalf("the operator command cannot discover the recorded VM: %v %v", found, err)
	}
	writer := disabled.journalWrite
	disabled.WithJournalWriter(func(string, []byte) error { return errors.New("disk unavailable") })
	if err := disabled.ForgetService(a.Name); err == nil || len(recordsIn(disabled, phaseRunning)) == 0 {
		t.Fatalf("a failed handoff lost the running record: %v", err)
	}
	disabled.WithJournalWriter(writer)
	if err := disabled.ForgetService(a.Name); err != nil {
		t.Fatal(err)
	}
	if len(recordsIn(disabled, phaseDeparting)) == 0 {
		t.Fatal("forget did not hand existing records to cleanup")
	}
	if _, ok := h.kernel.Delivers(8080); !ok || disabled.hostPortHolders[8080] != a.Name {
		t.Fatal("forget released rules or host-port history")
	}
	h.converge(disabled, config.NodeConfig{Services: []config.ServiceConfig{a}})
}

func TestDisabledNetworkingRecoveryRecordsExistingRemoval(t *testing.T) {
	h := newCrashHost(t)
	a := crashService("a", "172.16.0.2", 8080)
	enabled := h.agent()
	h.converge(enabled, config.NodeConfig{Services: []config.ServiceConfig{a}})
	manager := &teardownVMManager{fakeVMManager: h.vms.fakeVMManager}
	disabled := NewWithNetworkManager(manager, enabled.logger, nil, nil, "", 0).WithStateDir(h.stateDir)
	writer := disabled.journalWrite
	disabled.WithJournalWriter(func(string, []byte) error { return errors.New("disk unavailable") })
	if err := manager.clearRecovery(a); err == nil || h.vms.instances[a.Name] == nil {
		t.Fatalf("recovery removed the VM before recording its departure: %v", err)
	}
	disabled.WithJournalWriter(writer)
	if err := manager.clearRecovery(a); err != nil {
		t.Fatal(err)
	}
	if len(recordsIn(disabled, phaseDeparting)) == 0 {
		t.Fatal("recovery removal left a running allocation")
	}
	h.converge(disabled, config.NodeConfig{Services: []config.ServiceConfig{a}})
}

func TestDisabledNetworkingFailedRestartKeepsRemovalRetryable(t *testing.T) {
	h := newCrashHost(t)
	a := crashService("a", "172.16.0.2", 8080)
	enabled := h.agent()
	h.converge(enabled, config.NodeConfig{Services: []config.ServiceConfig{a}})
	disabled := NewWithNetworkManager(h.vms, enabled.logger, nil, nil, "", 0).WithStateDir(h.stateDir)
	h.vms.startErr = errors.New("volume preparation failed")
	h.vms.beforeStart = func(name string) { delete(h.vms.instances, name) }
	if err := disabled.Restart(context.Background(), a); err == nil {
		t.Fatal("setup: the restart unexpectedly succeeded")
	}
	h.vms.startErr, h.vms.beforeStart = nil, nil
	h.converge(disabled, config.NodeConfig{Services: []config.ServiceConfig{a}})
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
