package reconciler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/vm"
)

// forwardRecorder records the guest each host port is forwarded to.
type forwardRecorder struct{ forwards []string }

func (n *forwardRecorder) Setup(config.ServiceConfig) error { return nil }
func (n *forwardRecorder) DeleteTAP(string) error           { return nil }
func (n *forwardRecorder) DeleteBridge(string) error        { return nil }
func (n *forwardRecorder) SetupPortForward(hostPort int, guestIP string, vmPort int) error {
	n.forwards = append(n.forwards, fmt.Sprintf("%d->%s:%d", hostPort, guestIP, vmPort))
	return nil
}
func (n *forwardRecorder) TeardownPortForward(int, string, int) error { return nil }

func heldClaimsLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A running VM whose update is held still owns its host rules. They are
// re-asserted every tick, as for any running service, until the update lands.
func TestHeldRunningServiceKeepsDNATReasserted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*Reconciler, *fakeVMManager)
	}{
		{"capacity hold", func(r *Reconciler, _ *fakeVMManager) {
			r.SetHeldServices(map[string]Hold{"app": {Reason: "desired services exceed node capacity"}})
		}},
		{"unknown claims", func(_ *Reconciler, m *fakeVMManager) {
			m.instances["corrupt"] = &vm.Instance{Name: "corrupt", Config: config.ServiceConfig{Name: "corrupt"}, State: vm.StateRecoveryPending, RecoveryConfigUnknown: true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			net := &forwardRecorder{}
			m := newFakeVMManager()
			r := NewWithNetworkManager(m, heldClaimsLogger(), nil, net, "", 0)
			running := config.ServiceConfig{Name: "app", VCPUs: 1, Network: &config.NetworkConfig{GuestIP: "172.16.0.2"}, PortForwards: []config.PortForward{{HostPort: 8080, VMPort: 80}}}
			m.instances["app"] = &vm.Instance{Name: "app", Config: running, State: vm.StateRunning}
			tc.setup(r, m)
			desired := running
			desired.VCPUs = 2
			desired.PortForwards = []config.PortForward{{HostPort: 9090, VMPort: 80}}
			if err := r.SyncPortForwards(config.NodeConfig{Services: []config.ServiceConfig{desired}}); err != nil {
				t.Fatal(err)
			}
			if strings.Join(net.forwards, ",") != "8080->172.16.0.2:80" {
				t.Fatalf("forwards = %v, want the running VM's own 8080 rule", net.forwards)
			}
		})
	}
}

// A running VM whose update is held keeps its actual host port, guest IP and
// devices. A new service asking for that port waits instead of getting a second
// DNAT rule that the older one shadows.
func TestHeldRunningOwnerClaimsHoldNewService(t *testing.T) {
	net := &forwardRecorder{}
	m := newFakeVMManager()
	r := NewWithNetworkManager(m, heldClaimsLogger(), nil, net, "", 0)
	s := config.ServiceConfig{Name: "s", VCPUs: 1, Network: &config.NetworkConfig{GuestIP: "172.16.0.2"}, PortForwards: []config.PortForward{{HostPort: 8080, VMPort: 80}}}
	m.instances["s"] = &vm.Instance{Name: "s", Config: s, State: vm.StateRunning}
	desiredS := s
	desiredS.VCPUs = 4
	desiredS.PortForwards = []config.PortForward{{HostPort: 9090, VMPort: 80}}
	newT := config.ServiceConfig{Name: "t", VCPUs: 1, Network: &config.NetworkConfig{GuestIP: "172.16.0.3"}, PortForwards: []config.PortForward{{HostPort: 8080, VMPort: 80}}}
	desired := config.NodeConfig{Services: []config.ServiceConfig{desiredS, newT}}
	if err := config.ValidateNodePortClaims(desired); err != nil {
		t.Fatalf("agent preflight rejects the revision: %v", err)
	}
	r.SetHeldServices(map[string]Hold{"s": {Reason: "desired services exceed node capacity"}})
	err := r.Reconcile(context.Background(), desired)
	if !IsIncomplete(err) {
		t.Fatalf("Reconcile error = %v, want holds only", err)
	}
	if _, created := m.instances["t"]; created {
		t.Fatalf("t was created while held s still owns host port 8080; forwards=%v", net.forwards)
	}
	for _, forward := range net.forwards {
		if strings.HasPrefix(forward, "8080->172.16.0.3") {
			t.Fatalf("second DNAT rule for 8080 installed: %v", net.forwards)
		}
	}
	if reason := r.HeldServices(desired)["t"]; !strings.Contains(reason, "host port 8080") || !strings.Contains(reason, "s") {
		t.Fatalf("t hold reason = %q", reason)
	}
	// Once the owner's update is no longer held, both converge.
	r.SetHeldServices(nil)
	if err := r.Reconcile(context.Background(), desired); err != nil && !IsIncomplete(err) {
		t.Fatal(err)
	}
	if err := r.Reconcile(context.Background(), desired); err != nil {
		t.Fatalf("second reconcile after the hold cleared: %v", err)
	}
	if m.instances["t"] == nil || m.instances["s"].Config.PortForwards[0].HostPort != 9090 {
		t.Fatalf("services did not converge: t=%v s=%v", m.instances["t"], m.instances["s"].Config.PortForwards)
	}
}

// Two running services swapping host ports, with nothing held, converge
// instead of waiting on each other forever.
func TestUnheldPortSwapConverges(t *testing.T) {
	m := newFakeVMManager()
	r := NewWithNetworkManager(m, heldClaimsLogger(), nil, &forwardRecorder{}, "", 0)
	a := config.ServiceConfig{Name: "a", VCPUs: 1, Network: &config.NetworkConfig{GuestIP: "172.16.0.2"}, PortForwards: []config.PortForward{{HostPort: 8080, VMPort: 80}}}
	b := config.ServiceConfig{Name: "b", VCPUs: 1, Network: &config.NetworkConfig{GuestIP: "172.16.0.3"}, PortForwards: []config.PortForward{{HostPort: 9090, VMPort: 80}}}
	m.instances["a"] = &vm.Instance{Name: "a", Config: a, State: vm.StateRunning}
	m.instances["b"] = &vm.Instance{Name: "b", Config: b, State: vm.StateRunning}
	a.PortForwards, b.PortForwards = b.PortForwards, a.PortForwards
	desired := config.NodeConfig{Services: []config.ServiceConfig{a, b}}
	if err := r.Reconcile(context.Background(), desired); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if m.instances["a"].Config.PortForwards[0].HostPort != 9090 || m.instances["b"].Config.PortForwards[0].HostPort != 8080 {
		t.Fatal("swap did not converge")
	}
}

// Identical state yields identical hold messages, so status does not churn.
func TestHoldErrorOrderIsStable(t *testing.T) {
	m := newFakeVMManager()
	r := NewWithNetworkManager(m, heldClaimsLogger(), nil, nil, "", 0)
	holds := map[string]Hold{}
	var services []config.ServiceConfig
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("s%d", i)
		holds[name] = Hold{Reason: "cap"}
		services = append(services, config.ServiceConfig{Name: name})
	}
	r.SetHeldServices(holds)
	first := r.Reconcile(context.Background(), config.NodeConfig{Services: services}).Error()
	for i := 0; i < 20; i++ {
		if got := r.Reconcile(context.Background(), config.NodeConfig{Services: services}).Error(); got != first {
			t.Fatalf("hold message changed between identical reconciles:\n%s\n%s", first, got)
		}
	}
}

// A service whose own start or stop is in flight reports that, rather than a
// recovery hold, even while unknown claims fence new allocations.
func TestLifecycleHoldTakesPrecedenceOverUnknownClaims(t *testing.T) {
	m := newFakeVMManager()
	r := NewWithNetworkManager(m, heldClaimsLogger(), nil, nil, "", 0)
	m.instances["corrupt"] = &vm.Instance{Name: "corrupt", Config: config.ServiceConfig{Name: "corrupt"}, State: vm.StateRecoveryPending, RecoveryConfigUnknown: true}
	app := config.ServiceConfig{Name: "app", VCPUs: 1}
	m.instances["app"] = &vm.Instance{Name: "app", Config: app, State: vm.StateStopping}
	desired := config.NodeConfig{Services: []config.ServiceConfig{app}}
	if got := r.holds(desired)["app"]; got.Recovery || got.Reason != "runtime lifecycle is in progress" {
		t.Fatalf("hold = %#v", got)
	}
}

// A service held only because a held running VM keeps its claim inherits that
// VM's cause. When the chain starts at a quarantined VM, every link is a
// recovery hold, so the revision can apply as degraded.
func TestHoldThroughHeldOwnerKeepsRecoveryCause(t *testing.T) {
	m := newFakeVMManager()
	r := NewWithNetworkManager(m, heldClaimsLogger(), nil, &forwardRecorder{}, "", 0)
	svc := func(name, ip string, port int) config.ServiceConfig {
		return config.ServiceConfig{Name: name, VCPUs: 1, Network: &config.NetworkConfig{GuestIP: ip}, PortForwards: []config.PortForward{{HostPort: port, VMPort: 80}}}
	}
	m.instances["x"] = &vm.Instance{Name: "x", Config: svc("x", "172.16.0.9", 7070), State: vm.StateRecoveryPending}
	m.instances["a"] = &vm.Instance{Name: "a", Config: svc("a", "172.16.0.2", 8080), State: vm.StateRunning}
	m.instances["b"] = &vm.Instance{Name: "b", Config: svc("b", "172.16.0.3", 9090), State: vm.StateRunning}
	desired := config.NodeConfig{Services: []config.ServiceConfig{svc("a", "172.16.0.2", 7070), svc("b", "172.16.0.3", 8080)}}
	err := r.Reconcile(context.Background(), desired)
	held := r.holds(desired)
	if !held["a"].Recovery || !held["b"].Recovery || !OnlyRecoveryHeld(err) {
		t.Fatalf("a=%+v b=%+v onlyRecovery=%v err=%v", held["a"], held["b"], OnlyRecoveryHeld(err), err)
	}
	// A chain that starts at an ordinary hold stays ordinary.
	r.SetHeldServices(map[string]Hold{"a": {Reason: "desired services exceed node capacity"}})
	delete(m.instances, "x")
	if held := r.holds(desired); held["b"].Reason == "" || held["b"].Recovery {
		t.Fatalf("b=%+v", held["b"])
	}
}

// A crashed VM's DNAT rules stay installed until it is replaced. While its
// update is held, it keeps its claims: a new service asking for its port waits
// instead of getting a second rule that the crashed VM's rule shadows.
func TestHeldFailedVMKeepsItsClaims(t *testing.T) {
	m := newFakeVMManager()
	net := &forwardRecorder{}
	r := NewWithNetworkManager(m, heldClaimsLogger(), nil, net, "", 0)
	svc := func(name, ip string, port int) config.ServiceConfig {
		return config.ServiceConfig{Name: name, VCPUs: 1, Network: &config.NetworkConfig{GuestIP: ip}, PortForwards: []config.PortForward{{HostPort: port, VMPort: 80}}}
	}
	m.instances["x"] = &vm.Instance{Name: "x", Config: svc("x", "172.16.0.2", 8080), State: vm.StateFailed}
	desiredX := svc("x", "172.16.0.2", 9090)
	desiredX.VCPUs = 4
	desired := config.NodeConfig{Services: []config.ServiceConfig{svc("k", "172.16.0.3", 8080), desiredX}}
	r.SetHeldServices(map[string]Hold{"x": {Reason: "desired services exceed node capacity"}})
	_ = r.Reconcile(context.Background(), desired)
	if m.instances["k"] != nil {
		t.Fatalf("k was created on the port held failed x still forwards: %v", net.forwards)
	}
	if reason := r.HeldServices(desired)["k"]; !strings.Contains(reason, "host port 8080 retained by x") {
		t.Fatalf("k hold = %q", reason)
	}
}

// Replacing a service with one that takes its host port converges in one
// all-at-once apply: the delete releases the port before the create.
func TestReplacementOnSamePortConvergesInOneApply(t *testing.T) {
	m := newFakeVMManager()
	r := NewWithNetworkManager(m, heldClaimsLogger(), nil, &forwardRecorder{}, "", 0)
	svc := func(name, ip string) config.ServiceConfig {
		return config.ServiceConfig{Name: name, VCPUs: 1, Network: &config.NetworkConfig{GuestIP: ip}, PortForwards: []config.PortForward{{HostPort: 8080, VMPort: 80}}}
	}
	m.instances["d"] = &vm.Instance{Name: "d", Config: svc("d", "172.16.0.2"), State: vm.StateRunning}
	desired := config.NodeConfig{Services: []config.ServiceConfig{svc("k", "172.16.0.3")}}
	if err := r.Reconcile(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	if m.instances["d"] != nil || m.instances["k"] == nil {
		t.Fatalf("d=%v k=%v", m.instances["d"], m.instances["k"])
	}
}

// A create waiting on a VM whose removal recovery holds inherits the
// recovery cause, so the revision can apply as degraded.
func TestCreateWaitingOnRecoveryHeldRemovalIsRecoveryHold(t *testing.T) {
	m := newFakeVMManager()
	r := NewWithNetworkManager(m, heldClaimsLogger(), nil, &forwardRecorder{}, "", 0)
	m.instances["x"] = &vm.Instance{Name: "x", Config: config.ServiceConfig{Name: "x", Network: &config.NetworkConfig{GuestIP: "172.16.0.9"}}, State: vm.StateRecoveryPending}
	m.instances["d"] = &vm.Instance{Name: "d", Config: config.ServiceConfig{Name: "d", Network: &config.NetworkConfig{GuestIP: "172.16.0.9"}, PortForwards: []config.PortForward{{HostPort: 8080, VMPort: 80}}}, State: vm.StateRunning}
	desired := config.NodeConfig{Services: []config.ServiceConfig{{Name: "k", PortForwards: []config.PortForward{{HostPort: 8080, VMPort: 80}}}}}
	for i := 0; i < 2; i++ {
		if err := r.Reconcile(context.Background(), desired); !OnlyRecoveryHeld(err) {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if m.instances["k"] != nil {
		t.Fatal("k created on the port d still uses")
	}
}
