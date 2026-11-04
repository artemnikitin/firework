package reconciler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/network"
	"github.com/artemnikitin/firework/internal/network/networktest"
	"github.com/artemnikitin/firework/internal/vm"
)

// errCrash marks a simulated agent crash.
var errCrash = errors.New("simulated agent crash")

// crashHost is the state that survives an agent crash: the kernel's rules and
// devices, the VMs and the state directory. Each reconciler built over it is
// one agent incarnation.
type crashHost struct {
	t        *testing.T
	stateDir string
	kernel   *networktest.Kernel
	vms      *crashVMs
	// crashOnWrite, when it returns true for a journal write, crashes the
	// agent: before the write when the second result is false, after it
	// otherwise.
	crashOnWrite func(data string) (crash, afterWrite bool)
}

// crashVMs is a VM manager whose VMs outlive the agent, with hooks that crash
// the agent at chosen points.
type crashVMs struct {
	*fakeVMManager
	beforeStart, afterStart, afterStop, afterRemove func(name string)
	// live names services whose paths a process still uses.
	live map[string]bool
}

func (m *crashVMs) Start(ctx context.Context, svc config.ServiceConfig) error {
	if m.beforeStart != nil {
		m.beforeStart(svc.Name)
	}
	err := m.fakeVMManager.Start(ctx, svc)
	if err == nil && m.afterStart != nil {
		m.afterStart(svc.Name)
	}
	return err
}

func (m *crashVMs) Stop(name string) error {
	if inst := m.instances[name]; inst != nil {
		inst.State = vm.StateStopped
	}
	if m.afterStop != nil {
		m.afterStop(name)
	}
	return nil
}

func (m *crashVMs) Remove(name string) error {
	err := m.fakeVMManager.Remove(name)
	if err == nil && m.afterRemove != nil {
		m.afterRemove(name)
	}
	return err
}

func (m *crashVMs) ProveServiceAbsent(_ context.Context, service string) error {
	if m.live[service] {
		return fmt.Errorf("a live process uses the VM paths of %s", service)
	}
	return nil
}

func newCrashHost(t *testing.T) *crashHost {
	return &crashHost{t: t, stateDir: t.TempDir(), kernel: &networktest.Kernel{},
		vms: &crashVMs{fakeVMManager: newFakeVMManager(), live: map[string]bool{}}}
}

// agent starts a new agent incarnation over the host.
func (h *crashHost) agent() *Reconciler {
	h.t.Helper()
	r := NewWithNetworkManager(h.vms, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, h.kernel, "", 0)
	r.WithJournalWriter(func(path string, data []byte) error {
		crash, after := false, false
		if h.crashOnWrite != nil {
			crash, after = h.crashOnWrite(string(data))
		}
		if crash && !after {
			panic(errCrash)
		}
		if err := atomicWriteFile(path, data, 0o644); err != nil {
			return err
		}
		if crash {
			panic(errCrash)
		}
		return nil
	})
	r.WithStateDir(h.stateDir)
	if !r.ownershipReady() {
		h.t.Fatal("ownership records were not initialized")
	}
	return r
}

// crashDuring runs fn and reports whether the simulated crash interrupted it.
// The crashed incarnation is abandoned, and every crash hook is cleared.
func (h *crashHost) crashDuring(fn func()) (crashed bool) {
	defer func() {
		h.crashOnWrite = nil
		h.vms.beforeStart, h.vms.afterStart, h.vms.afterStop, h.vms.afterRemove = nil, nil, nil, nil
		h.kernel.SetupErr, h.kernel.DeviceErr, h.kernel.DeleteErr = nil, nil, nil
		if recovered := recover(); recovered != nil {
			if recovered != errCrash {
				panic(recovered)
			}
			crashed = true
		}
	}()
	fn()
	return false
}

func crash(string) { panic(errCrash) }

// converge runs ticks of a fresh incarnation until the desired state is
// reached, and fails the test if it is not.
func (h *crashHost) converge(r *Reconciler, desired config.NodeConfig) {
	h.t.Helper()
	var err error
	for tick := 0; tick < 4; tick++ {
		if err = r.Reconcile(context.Background(), desired); err == nil {
			return
		}
	}
	h.t.Fatalf("did not converge: %v; rules %v", err, h.kernel.Rules())
}

// assertOwns checks the observable end state: each desired service runs, its
// own rules are the only rules toward its address and for its host ports, no
// rule reaches an address nothing runs at, and no record waits for cleanup.
func (h *crashHost) assertOwns(r *Reconciler, desired config.NodeConfig) {
	h.t.Helper()
	wanted := make(map[string]config.ServiceConfig)
	for _, svc := range desired.Services {
		wanted[svc.Name] = svc
		if inst := h.vms.instances[svc.Name]; inst == nil || inst.State != vm.StateRunning {
			h.t.Fatalf("%s is not running: %#v", svc.Name, inst)
		}
	}
	for _, rule := range h.kernel.Rules() {
		owner := ""
		for name, svc := range wanted {
			if sameGuestIP(guestIPOf(svc), rule.GuestIP) {
				owner = name
			}
		}
		if owner == "" || !rule.TaggedFor(owner) {
			h.t.Fatalf("rule %s is not owned by the service at its address (%q)", rule, owner)
		}
		svc := wanted[owner]
		if !portForwardClaimed(svc, rule) {
			h.t.Fatalf("rule %s is not one of %s's port forwards", rule, owner)
		}
	}
	for _, svc := range desired.Services {
		for _, pf := range svc.PortForwards {
			rule, ok := h.kernel.Delivers(pf.HostPort)
			if !ok || !rule.Targets(pf.HostPort, guestIPOf(svc), pf.VMPort) {
				h.t.Fatalf("host port %d does not reach %s: %v", pf.HostPort, svc.Name, h.kernel.Rules())
			}
		}
	}
	if departing := recordsIn(r, phaseDeparting); len(departing) != 0 || len(recordsIn(r, phaseCreating)) != 0 {
		h.t.Fatalf("records still wait: departing %#v creating %#v", departing, recordsIn(r, phaseCreating))
	}
	if r.HasPendingCleanup() {
		h.t.Fatal("cleanup is still pending")
	}
}

func portForwardClaimed(svc config.ServiceConfig, rule network.PortForwardRule) bool {
	for _, pf := range svc.PortForwards {
		if rule.Targets(pf.HostPort, guestIPOf(svc), pf.VMPort) {
			return true
		}
	}
	return false
}

func crashService(name, ip string, port int) config.ServiceConfig {
	return config.ServiceConfig{Name: name, Network: &config.NetworkConfig{GuestIP: ip},
		PortForwards: []config.PortForward{{HostPort: port, VMPort: 80}}}
}

func phaseWrite(phase allocationPhase) func(string) (bool, bool) {
	return func(data string) (bool, bool) {
		return strings.Contains(data, `"phase":"`+string(phase)+`"`), true
	}
}

// C4: an agent crash at any step of creating a VM recovers without an
// operator. After the restart the VM is adopted or proven absent and its
// address, rules and devices are cleaned up, then the service converges with
// only its own rules installed.
func TestC4CrashDuringCreateRecoversAutomatically(t *testing.T) {
	web := crashService("web", "172.16.0.2", 8080)
	desired := config.NodeConfig{Services: []config.ServiceConfig{web}}
	for name, arm := range map[string]func(*crashHost){
		"after the record, before devices": func(h *crashHost) {
			h.kernel.DeviceErr = func(string) error { panic(errCrash) }
		},
		"after devices, before the VM starts": func(h *crashHost) { h.vms.beforeStart = crash },
		"after the VM starts, before rules":   func(h *crashHost) { h.kernel.SetupErr = func(string, int) error { panic(errCrash) } },
		"after rules, before the record says running": func(h *crashHost) {
			h.crashOnWrite = phaseWrite(phaseRunning)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCrashHost(t)
			first := h.agent()
			arm(h)
			if !h.crashDuring(func() { _ = first.Reconcile(context.Background(), desired) }) {
				t.Fatal("the crash point was not reached")
			}
			restarted := h.agent()
			h.converge(restarted, desired)
			h.assertOwns(restarted, desired)
			if !h.kernel.HasDevice("tap-web") {
				t.Fatal("the recreated VM lost its device")
			}
		})
	}
}

// C1/C4: an interrupted start whose paths a process may still use keeps its
// address, and the service does not start a second VM over it, until the
// absence proof succeeds. Then it converges without an operator.
func TestC4InterruptedStartWaitsForAbsenceProof(t *testing.T) {
	web := crashService("web", "172.16.0.2", 8080)
	other := crashService("other", "172.16.0.2", 9090)
	h := newCrashHost(t)
	first := h.agent()
	h.vms.beforeStart = crash
	if !h.crashDuring(func() {
		_ = first.Reconcile(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{web}})
	}) {
		t.Fatal("the crash point was not reached")
	}
	h.vms.live["web"] = true
	restarted := h.agent()
	desired := config.NodeConfig{Services: []config.ServiceConfig{web, other}}
	for tick := 0; tick < 3; tick++ {
		if err := restarted.Reconcile(context.Background(), desired); !IsIncomplete(err) {
			t.Fatalf("tick %d: unresolved start must hold, not fail or converge: %v", tick, err)
		}
		if len(h.vms.startCalls) != 0 {
			t.Fatalf("a VM started while an interrupted start was unresolved: %v", h.vms.startCalls)
		}
		if !restarted.HasPendingCleanup() {
			t.Fatal("an unresolved start must keep cleanup eligible for retry")
		}
	}
	held := restarted.HeldServices(desired)
	if !strings.Contains(held["web"], "interrupted start") || !strings.Contains(held["other"], "172.16.0.2") {
		t.Fatalf("holds do not say why: %v", held)
	}
	delete(h.vms.live, "web")
	final := config.NodeConfig{Services: []config.ServiceConfig{web}}
	h.converge(restarted, final)
	h.assertOwns(restarted, final)
}

// startsOnlyOverItsOwnRules fails the test if a service starts while a rule
// of another service reaches its address or one of its host ports: the
// invariant every start must keep.
func (h *crashHost) startsOnlyOverItsOwnRules(services ...config.ServiceConfig) {
	byName := make(map[string]config.ServiceConfig)
	for _, svc := range services {
		byName[svc.Name] = svc
	}
	h.vms.beforeStart = func(name string) {
		svc, ok := byName[name]
		if !ok {
			return
		}
		for _, rule := range h.kernel.Rules() {
			toAddress := sameGuestIP(rule.GuestIP, guestIPOf(svc))
			forPort := false
			for _, pf := range svc.PortForwards {
				forPort = forPort || rule.HostPort == pf.HostPort
			}
			if (toAddress || forPort) && !rule.TaggedFor(name) {
				h.t.Errorf("%s started while %s still delivers there", name, rule)
			}
		}
	}
}

// C4: an agent crash at any step of removing a VM finishes the removal after
// the restart. The address is released only once its rules are gone, and
// another service then takes it with only its own rule toward it.
func TestC4CrashDuringDeleteResumesAndReleasesTheAddress(t *testing.T) {
	web := crashService("web", "172.16.0.2", 8080)
	next := crashService("next", "172.16.0.2", 8080)
	for name, arm := range map[string]func(*crashHost){
		"after the departing record, before the VM stops": func(h *crashHost) { h.crashOnWrite = phaseWrite(phaseDeparting) },
		"after the VM stops, before teardown":             func(h *crashHost) { h.vms.afterRemove = crash },
		"after teardown, before the record is removed": func(h *crashHost) {
			h.crashOnWrite = func(string) (bool, bool) { return len(h.kernel.Rules()) == 0, false }
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCrashHost(t)
			first := h.agent()
			h.converge(first, config.NodeConfig{Services: []config.ServiceConfig{web}})
			arm(h)
			if !h.crashDuring(func() { _ = first.Reconcile(context.Background(), config.NodeConfig{}) }) {
				t.Fatal("the crash point was not reached")
			}
			restarted := h.agent()
			h.startsOnlyOverItsOwnRules(next)
			desired := config.NodeConfig{Services: []config.ServiceConfig{next}}
			h.converge(restarted, desired)
			h.assertOwns(restarted, desired)
		})
	}
}

// C4: a crash while cleaning up after a failed start resumes from the record,
// and the service is created again once its start succeeds.
func TestC4CrashDuringFailedStartCleanupResumes(t *testing.T) {
	web := crashService("web", "172.16.0.2", 8080)
	desired := config.NodeConfig{Services: []config.ServiceConfig{web}}
	h := newCrashHost(t)
	first := h.agent()
	h.vms.startErr = errors.New("firecracker refused to boot")
	h.crashOnWrite = phaseWrite(phaseDeparting)
	if !h.crashDuring(func() { _ = first.Reconcile(context.Background(), desired) }) {
		t.Fatal("the crash point was not reached")
	}
	h.vms.startErr = nil
	restarted := h.agent()
	h.converge(restarted, desired)
	h.assertOwns(restarted, desired)
}

// C4: a health restart whose start fails drops the VM but leaves its rule
// covered, so the address is not given to another service until the rule is
// gone. A crash between the stop and the start recovers the same way.
func TestC4FailedHealthRestartKeepsItsRuleCovered(t *testing.T) {
	web := crashService("web", "172.16.0.2", 8080)
	other := crashService("other", "172.16.0.2", 9090)
	for _, crashed := range []bool{false, true} {
		t.Run(fmt.Sprintf("crash before start=%v", crashed), func(t *testing.T) {
			h := newCrashHost(t)
			r := h.agent()
			h.converge(r, config.NodeConfig{Services: []config.ServiceConfig{web}})
			h.kernel.DeleteErr = func(network.PortForwardRule) error { return networktest.ErrInjected }
			if crashed {
				h.vms.afterStop = func(name string) {
					delete(h.vms.instances, name) // The VM process died with the agent.
					panic(errCrash)
				}
				if !h.crashDuring(func() { _ = r.Restart(context.Background(), web) }) {
					t.Fatal("the crash point was not reached")
				}
				h.kernel.DeleteErr = func(network.PortForwardRule) error { return networktest.ErrInjected }
				r = h.agent()
			} else {
				h.vms.beforeStart = func(name string) {
					delete(h.vms.instances, name)
					h.vms.startErr = errors.New("preparing volumes: transient")
				}
				if err := r.Restart(context.Background(), web); err == nil {
					t.Fatal("the restart should fail")
				}
				h.vms.beforeStart, h.vms.startErr = nil, nil
			}
			desired := config.NodeConfig{Services: []config.ServiceConfig{other}}
			h.startsOnlyOverItsOwnRules(other)
			for tick := 0; tick < 3; tick++ {
				_ = r.Reconcile(context.Background(), desired)
			}
			if h.vms.instances["other"] != nil {
				t.Fatalf("other took web's address while web's rule still reaches it: %v", h.kernel.Rules())
			}
			h.kernel.DeleteErr = nil
			h.converge(r, desired)
			h.assertOwns(r, desired)
		})
	}
}
