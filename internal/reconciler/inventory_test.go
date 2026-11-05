package reconciler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/network"
	"github.com/artemnikitin/firework/internal/network/networktest"
	"github.com/artemnikitin/firework/internal/vm"
)

// G2, G3, G5: a rule firework did not install, toward a service's address or
// for one of its host ports, holds the start with a reason naming the rule
// and the operator's action. It is never deleted, and the start proceeds once
// the operator removes it.
func TestForeignRuleHoldsTheStartUntilTheOperatorRemovesIt(t *testing.T) {
	web := crashService("web", "172.16.0.2", 8080)
	for name, foreign := range map[string]network.PortForwardRule{
		"toward its address": networktest.Rule("", 9999, "172.16.0.2", 22),
		"for its host port":  networktest.Rule("", 8080, "10.9.0.7", 8080),
	} {
		t.Run(name, func(t *testing.T) {
			h := newCrashHost(t)
			r := h.agent()
			h.kernel.Append(foreign)
			desired := config.NodeConfig{Services: []config.ServiceConfig{web}}
			for tick := 0; tick < 2; tick++ {
				if err := r.Reconcile(context.Background(), desired); !IsIncomplete(err) {
					t.Fatalf("tick %d: %v", tick, err)
				}
			}
			reason := r.HeldServices(desired)["web"]
			if h.vms.instances["web"] != nil || !strings.Contains(reason, "firework did not install") || !strings.Contains(reason, foreign.String()) {
				t.Fatalf("web=%v hold=%q", h.vms.instances["web"], reason)
			}
			if len(h.kernel.Deleted) != 0 {
				t.Fatalf("a foreign rule was deleted: %v", h.kernel.Deleted)
			}
			if err := h.kernel.DeletePortForwardRule(foreign); err != nil { // the operator's cleanup
				t.Fatal(err)
			}
			h.converge(r, desired)
			h.assertOwns(r, desired)
		})
	}
}

// G1, G5: rules that cannot be read, or a DNAT rule whose destination cannot
// be read, hold every networked start. A service without networking starts.
func TestUnreadableRulesHoldNetworkedStarts(t *testing.T) {
	for name, arm := range map[string]func(*networktest.Kernel){
		"listing fails": func(k *networktest.Kernel) {
			k.ListErr = func() error { return errors.New("iptables: lock held") }
		},
		"unparsed DNAT": func(k *networktest.Kernel) {
			k.Unparsed = []string{"-A PREROUTING -p tcp -m tcp --dport 7000:7010 -j DNAT --to-destination 172.16.0.5"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCrashHost(t)
			r := h.agent()
			arm(h.kernel)
			desired := config.NodeConfig{Services: []config.ServiceConfig{crashService("web", "172.16.0.2", 8080), {Name: "plain"}}}
			_ = r.Reconcile(context.Background(), desired)
			if h.vms.instances["web"] != nil || h.vms.instances["plain"] == nil {
				t.Fatalf("web=%v plain=%v", h.vms.instances["web"], h.vms.instances["plain"])
			}
			if reason := r.HeldServices(desired)["web"]; reason == "" {
				t.Fatal("the hold is not reported")
			}
		})
	}
}

// A rolling update whose start is held after its old VM was removed does not
// stop the rest of the rollout.
func TestRollingUpdateContinuesPastAHeldStart(t *testing.T) {
	h := newCrashHost(t)
	r := NewWithNetworkManager(h.vms, h.agent().logger, nil, h.kernel, "rolling", 0).WithStateDir(t.TempDir())
	a := crashService("a", "172.16.0.2", 8080)
	b := crashService("b", "172.16.0.3", 9090)
	h.converge(r, config.NodeConfig{Services: []config.ServiceConfig{a, b}})
	// a moves to a new port while its old rule cannot be torn down; b changes
	// its image.
	h.kernel.DeleteErr = func(rule network.PortForwardRule) error {
		if rule.HostPort == 8080 {
			return networktest.ErrInjected
		}
		return nil
	}
	a.PortForwards = []config.PortForward{{HostPort: 8081, VMPort: 80}}
	b.Image = "/images/b-v2"
	desired := config.NodeConfig{Services: []config.ServiceConfig{a, b}}
	_ = r.Reconcile(context.Background(), desired)
	if h.vms.instances["a"] != nil {
		t.Fatal("a started while its old 8080 rule reached its address")
	}
	if inst := h.vms.instances["b"]; inst == nil || inst.Config.Image != "/images/b-v2" {
		t.Fatalf("b's update did not run after a's held start: %#v", inst)
	}
	h.kernel.DeleteErr = nil
	h.converge(r, desired)
	h.assertOwns(r, desired)
}

// A health restart waits, leaving its VM as it is, while a rule it does not
// own reaches its address.
func TestHealthRestartWaitsForAForeignRule(t *testing.T) {
	h := newCrashHost(t)
	r := h.agent()
	web := crashService("web", "172.16.0.2", 8080)
	h.converge(r, config.NodeConfig{Services: []config.ServiceConfig{web}})
	if hold := r.StartHold(web); hold.Reason != "" {
		t.Fatalf("the VM's own rules held its restart: %+v", hold)
	}
	h.kernel.Append(networktest.Rule("", 2222, "172.16.0.2", 22))
	if hold := r.StartHold(web); !strings.Contains(hold.Reason, "172.16.0.2") {
		t.Fatalf("hold = %+v", hold)
	}
}

// L6: a host port goes only to the service that first held it on this node,
// across restarts and after that service is gone. A port this node never
// used is free.
func TestHostPortHoldersSurviveRestartsAndRemoval(t *testing.T) {
	h := newCrashHost(t)
	r := h.agent()
	web := crashService("web", "172.16.0.2", 8080)
	h.converge(r, config.NodeConfig{Services: []config.ServiceConfig{web}})
	h.converge(r, config.NodeConfig{})
	restarted := h.agent()
	api := crashService("api", "172.16.0.3", 8080)
	desired := config.NodeConfig{Services: []config.ServiceConfig{api}}
	err := restarted.Reconcile(context.Background(), desired)
	var action *ActionRequiredError
	if !errors.As(err, &action) || action.Code != ActionHostPortReuseBlocked || !strings.Contains(action.Reason, "web") {
		t.Fatalf("api took web's old port: %v", err)
	}
	if hold := restarted.HeldServiceHolds(desired)["api"]; hold.Action != ActionHostPortReuseBlocked {
		t.Fatalf("hold = %+v", hold)
	}
	// web may come back on its own port; api converges on a fresh one.
	api.PortForwards = []config.PortForward{{HostPort: 8081, VMPort: 80}}
	both := config.NodeConfig{Services: []config.ServiceConfig{web, api}}
	h.converge(restarted, both)
	h.assertOwns(restarted, both)
}

// L6 with incomplete history: services keep the ports they already have, and
// a port this node has no record of waits, because an earlier service may
// still be referenced on it.
func TestIncompleteHistoryBlocksOnlyUnrecordedPorts(t *testing.T) {
	h := newCrashHost(t)
	if err := os.Mkdir(filepath.Join(h.stateDir, "vms"), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := crashService("existing", "172.16.0.2", 8080)
	h.vms.instances[existing.Name] = &vm.Instance{Name: existing.Name, Config: existing, State: vm.StateRunning}
	r := h.agent()
	if r.history.Complete {
		t.Fatal("a host with VM state claimed complete history")
	}
	updated := existing
	updated.Image = "/images/v2"
	fresh := crashService("fresh", "172.16.0.3", 9090)
	desired := config.NodeConfig{Services: []config.ServiceConfig{updated, fresh}}
	err := r.Reconcile(context.Background(), desired)
	var action *ActionRequiredError
	if !errors.As(err, &action) || action.Code != ActionNetworkHistoryIncomplete {
		t.Fatalf("fresh took an unrecorded port: %v", err)
	}
	if h.vms.instances["fresh"] != nil || h.vms.instances["existing"].Config.Image != "/images/v2" {
		t.Fatalf("fresh=%v existing=%#v", h.vms.instances["fresh"], h.vms.instances["existing"])
	}
}

type scanningVMs struct {
	*crashVMs
	unaccounted []string
	err         error
}

func (s *scanningVMs) UnaccountedVMs(context.Context) ([]string, error) { return s.unaccounted, s.err }

// G5: a VM process under this node's state that nothing tracks may hold any
// address, so no service gets a new address while it runs. A running service
// keeps its own, and the hold names the process.
func TestUnaccountedVMHoldsNewAddresses(t *testing.T) {
	h := newCrashHost(t)
	vms := &scanningVMs{crashVMs: h.vms}
	r := NewWithNetworkManager(vms, h.agent().logger, nil, h.kernel, "", 0).WithStateDir(t.TempDir())
	running := crashService("running", "172.16.0.2", 8080)
	h.converge(r, config.NodeConfig{Services: []config.ServiceConfig{running}})
	vms.unaccounted = []string{"process 4242 (VM directory of gone)"}
	updated := running
	updated.Image = "/images/v2"
	desired := config.NodeConfig{Services: []config.ServiceConfig{updated, crashService("fresh", "172.16.0.3", 9090)}}
	err := r.Reconcile(context.Background(), desired)
	var action *ActionRequiredError
	if !errors.As(err, &action) || action.Code != ActionUnaccountedVM || !strings.Contains(action.Reason, "4242") {
		t.Fatalf("fresh started beside an untracked VM: %v", err)
	}
	if h.vms.instances["fresh"] != nil || h.vms.instances["running"].Config.Image != "/images/v2" {
		t.Fatalf("fresh=%v running=%#v", h.vms.instances["fresh"], h.vms.instances["running"])
	}
	vms.unaccounted, vms.err = nil, errors.New("process table unreadable")
	if err := r.Reconcile(context.Background(), desired); !IsIncomplete(err) || h.vms.instances["fresh"] != nil {
		t.Fatalf("an unreadable scan did not hold new addresses: %v", err)
	}
	vms.err = nil
	h.converge(r, desired)
}

// Without host networking there is nothing to forget, and asking is safe.
func TestForgetServiceWithoutHostNetworking(t *testing.T) {
	r := New(newFakeVMManager(), heldClaimsLogger(), nil, nil, "", 0)
	if err := r.ForgetService("web"); err != nil {
		t.Fatal(err)
	}
	if traces, err := r.HasUnresolvedTraces("web"); err != nil || traces {
		t.Fatalf("traces=%v err=%v", traces, err)
	}
}
