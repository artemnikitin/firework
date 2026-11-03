package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/vm"
)

// Tests named TestC7… and TestC6… check the acceptance contract of issue #48:
// C7 conservative capacity safety and C6 honest status.

type c7Service struct {
	name       string
	vcpus, mem int
	port       int
}

func c7YAML(services ...c7Service) string {
	var b strings.Builder
	b.WriteString("node: web\nservices:\n")
	for _, s := range services {
		fmt.Fprintf(&b, "- name: %s\n  image: /%s\n  kernel: /k\n  vcpus: %d\n  memory_mb: %d\n", s.name, s.name, s.vcpus, s.mem)
		if s.port != 0 {
			fmt.Fprintf(&b, "  port_forwards:\n  - host_port: %d\n    vm_port: 80\n", s.port)
		}
	}
	return b.String()
}

func c7Agent(t *testing.T, strategy string, delay time.Duration, limit capacity.NodeCapacity, instances map[string]*vm.Instance, services ...c7Service) (*Agent, *recoveryTestVMManager) {
	t.Helper()
	a, m := admissionAgent(t, c7YAML(services...), limit, instances, "")
	a.reconciler = reconciler.NewWithNetworkManager(m, testLogger(), nil, &recoveryTestNetworkManager{}, strategy, delay)
	return a, m
}

func c7Running(name string, vcpus, mem, port int) *vm.Instance {
	return &vm.Instance{Name: name, Config: admissionService(name, vcpus, mem, port, ""), State: vm.StateRunning}
}

// c7Converged reports the first desired service that does not run its
// desired size, or "".
func c7Converged(m *recoveryTestVMManager, services ...c7Service) string {
	for _, want := range services {
		got := m.instances[want.name]
		if got == nil || got.State != vm.StateRunning || got.Config.VCPUs != want.vcpus || got.Config.MemoryMB != want.mem {
			return fmt.Sprintf("%s: %+v", want.name, got)
		}
	}
	return ""
}

var c7Strategies = []string{"", "rolling"}

// C7: A uses 4 vCPUs and is to shrink to 2; B needs those 2. While A's
// shrink is held, fails or becomes quarantined, B must not start into them,
// and once the shrink succeeds B converges on the unchanged revision.
func TestC7ShrinkFreesNothingUntilItSucceeds(t *testing.T) {
	desired := []c7Service{{"b", 2, 64, 0}, {"o", 2, 64, 0}, {"s", 2, 64, 7070}}
	faultErr := errors.New("injected fault")
	for _, fault := range []string{"none", "held", "stop fails", "start fails", "quarantined mid-apply"} {
		for _, strategy := range c7Strategies {
			t.Run(fault+"/"+strategy, func(t *testing.T) {
				instances := map[string]*vm.Instance{
					"o": c7Running("o", 2, 64, 0),
					"s": c7Running("s", 4, 64, 8080),
				}
				if fault == "held" {
					// A quarantined VM still claims the port s moves to.
					instances["q"] = &vm.Instance{Name: "q", Config: admissionService("q", 0, 0, 7070, ""), State: vm.StateRecoveryPending}
				}
				a, m := c7Agent(t, strategy, 0, capacity.NodeCapacity{VCPUs: 6, MemoryMB: 4096}, instances, desired...)
				faulty := fault != "none"
				switch fault {
				case "stop fails":
					m.removeHook = func(name string) error {
						if faulty && name == "s" {
							return faultErr
						}
						return nil
					}
				case "start fails":
					m.startHook = func(name string) error {
						if faulty && name == "s" {
							return faultErr
						}
						return nil
					}
				case "quarantined mid-apply":
					m.removeHook = func(name string) error {
						if faulty && name == "s" {
							m.instances["s"].State = vm.StateRecoveryPending
							return faultErr
						}
						return nil
					}
				}
				for tick := 0; tick < 3 && faulty; tick++ {
					a.tick(context.Background())
					if len(m.overcommits) != 0 {
						t.Fatalf("tick %d: %v", tick, m.overcommits)
					}
					// A failed start leaves s without a VM, which does free
					// its capacity; a VM s still has keeps all of it.
					if s := m.instances["s"]; m.instances["b"] != nil && s != nil && s.Config.VCPUs != 2 {
						t.Fatalf("tick %d: b started while s still used %d vCPUs", tick, s.Config.VCPUs)
					}
				}
				// Resolve the fault: the hold's owner is gone, the quarantine
				// resolved, or the next attempt succeeds.
				faulty = false
				delete(m.instances, "q")
				if s := m.instances["s"]; s != nil && s.State == vm.StateRecoveryPending {
					s.State = vm.StateRunning
				}
				for tick := 0; tick < 3; tick++ {
					a.tick(context.Background())
				}
				if len(m.overcommits) != 0 {
					t.Fatal(m.overcommits)
				}
				if problem := c7Converged(m, desired...); problem != "" || a.lastRevision != "r2" {
					t.Fatalf("did not converge on the unchanged revision: %s; %s", problem, conditionOf(a.agentStatusSnapshot(), "VMsReconciled").Message)
				}
			})
		}
	}
}

// C7: a rolling update that fails stops the rollout before a later update's
// shrink. A create admitted against that shrink would leave the node over
// capacity for as long as the failure lasts.
func TestC7RollingAbortLeavesNoCreditedShrink(t *testing.T) {
	desired := []c7Service{{"b", 2, 64, 0}, {"s", 2, 64, 0}, {"u", 2, 64, 0}}
	a, m := c7Agent(t, "rolling", 0, capacity.NodeCapacity{VCPUs: 6, MemoryMB: 4096}, map[string]*vm.Instance{
		"s": c7Running("s", 4, 64, 0),
		"u": {Name: "u", Config: config.ServiceConfig{Name: "u", Image: "/old", Kernel: "/k", VCPUs: 2, MemoryMB: 64}, State: vm.StateRunning},
	}, desired...)
	// Rolling updates run in desired order; put u's failing update first.
	a.store.(*fakeStore).data["web"] = []byte(c7YAML(desired[0], desired[2], desired[1]))
	failing := true
	m.startHook = func(name string) error {
		if failing && name == "u" {
			return errors.New("injected start failure")
		}
		return nil
	}
	for tick := 0; tick < 3; tick++ {
		a.tick(context.Background())
		if len(m.overcommits) != 0 {
			t.Fatalf("tick %d: %v", tick, m.overcommits)
		}
		if vcpus, _ := runtimeUsage(m); vcpus > 6 {
			t.Fatalf("tick %d ends at %d vCPUs on 6", tick, vcpus)
		}
	}
	failing = false
	for tick := 0; tick < 4; tick++ {
		a.tick(context.Background())
	}
	if problem := c7Converged(m, desired...); problem != "" || len(m.overcommits) != 0 || a.lastRevision != "r2" {
		t.Fatalf("after the fault: %s; overcommits=%v; %s", problem, m.overcommits, a.agentStatusSnapshot().Message)
	}
}

// C7: a health restart of a failed VM takes its resources again. While
// admission holds that VM for capacity, the restart must wait, between ticks
// and during a rolling pause alike, rather than take capacity another
// service was admitted to.
func TestC7HealthRestartWaitsForCapacity(t *testing.T) {
	t.Run("between ticks", func(t *testing.T) {
		desired := []c7Service{{"a", 2, 64, 0}, {"f", 2, 64, 0}, {"r", 2, 64, 0}}
		a, m := c7Agent(t, "", 0, capacity.NodeCapacity{VCPUs: 4, MemoryMB: 4096}, map[string]*vm.Instance{
			"f": {Name: "f", Config: admissionService("f", 2, 64, 0, ""), State: vm.StateFailed},
			"r": c7Running("r", 2, 64, 0),
		}, desired...)
		a.tick(context.Background())
		if m.instances["a"] == nil || m.instances["f"].State != vm.StateFailed {
			t.Fatalf("admission: a=%+v f=%+v", m.instances["a"], m.instances["f"])
		}
		if err := a.restartService(context.Background(), "f"); err == nil || m.instances["f"].State != vm.StateFailed {
			t.Fatalf("restart took capacity admission gave a: err=%v f=%+v", err, m.instances["f"])
		}
		if len(m.overcommits) != 0 {
			t.Fatal(m.overcommits)
		}
		// A running VM restarts at the size it already uses.
		if err := a.restartService(context.Background(), "r"); err != nil || len(m.overcommits) != 0 {
			t.Fatalf("same-size restart: err=%v overcommits=%v", err, m.overcommits)
		}
	})
	t.Run("during a rolling pause", func(t *testing.T) {
		desired := []c7Service{{"a", 2, 64, 0}, {"b", 4, 64, 0}, {"f", 2, 64, 0}}
		a, m := c7Agent(t, "rolling", 300*time.Millisecond, capacity.NodeCapacity{VCPUs: 6, MemoryMB: 4096}, map[string]*vm.Instance{
			"a": {Name: "a", Config: config.ServiceConfig{Name: "a", Image: "/old", Kernel: "/k", VCPUs: 2, MemoryMB: 64}, State: vm.StateRunning},
			"b": c7Running("b", 2, 64, 0),
			"f": {Name: "f", Config: admissionService("f", 2, 64, 0, ""), State: vm.StateFailed},
		}, desired...)
		paused := make(chan struct{})
		m.startHook = func(name string) error {
			if name == "a" {
				close(paused) // a's update is done; the rollout pauses before b grows.
			}
			return nil
		}
		restarted := make(chan error, 1)
		go func() {
			<-paused
			restarted <- a.restartService(context.Background(), "f")
		}()
		a.tick(context.Background())
		select {
		case err := <-restarted:
			if err == nil {
				t.Fatal("restart during the pause took capacity b was admitted to grow into")
			}
		case <-time.After(time.Second):
			t.Fatal("restart did not run")
		}
		if len(m.overcommits) != 0 {
			t.Fatal(m.overcommits)
		}
		if got := m.instances["b"].Config.VCPUs; got != 4 {
			t.Fatalf("b did not grow: %d", got)
		}
	})
}

// C7: creates and growths compete for what is free now. A removal frees its
// VM's capacity only once it has happened, so growth that needs it waits for
// the next tick, and indefinitely while the removal keeps failing.
func TestC7CompetingCreatesAndGrowths(t *testing.T) {
	desired := []c7Service{{"c1", 2, 256, 0}, {"c2", 2, 256, 0}, {"g", 4, 256, 0}}
	for _, removal := range []string{"succeeds", "fails"} {
		for _, strategy := range c7Strategies {
			t.Run(removal+"/"+strategy, func(t *testing.T) {
				a, m := c7Agent(t, strategy, 0, capacity.NodeCapacity{VCPUs: 8, MemoryMB: 1024}, map[string]*vm.Instance{
					"g": c7Running("g", 2, 256, 0),
					"x": c7Running("x", 1, 256, 0),
				}, desired...)
				failing := removal == "fails"
				m.removeHook = func(name string) error {
					if failing && name == "x" {
						return errors.New("injected removal failure")
					}
					return nil
				}
				a.tick(context.Background())
				if m.instances["c1"] == nil || m.instances["c2"] == nil {
					t.Fatalf("free capacity was not used: %v", m.startCalls)
				}
				if got := m.instances["g"].Config.VCPUs; got != 2 {
					t.Fatalf("g grew into capacity x still used: %d", got)
				}
				for tick := 0; tick < 2 && failing; tick++ {
					a.tick(context.Background())
					if got := m.instances["g"].Config.VCPUs; got != 2 {
						t.Fatalf("g grew while x's removal kept failing: %d", got)
					}
				}
				failing = false
				a.tick(context.Background())
				a.tick(context.Background())
				if problem := c7Converged(m, desired...); problem != "" || len(m.overcommits) != 0 || a.lastRevision != "r2" {
					t.Fatalf("%s; overcommits=%v; %s", problem, m.overcommits, a.agentStatusSnapshot().Message)
				}
			})
		}
	}
}

// C7 does not repair a node already over capacity, and the excess does not
// stop changes that add nothing to it: a shrink, a same-size restart and
// growth of a resource that is not over proceed, while a new allocation of
// the exceeded resource waits.
func TestC7PreExistingExcessAllowsOnlyWhatDoesNotGrowIt(t *testing.T) {
	desired := []c7Service{{"c", 1, 64, 0}, {"m", 1, 128, 0}, {"x", 3, 128, 0}}
	a, m := c7Agent(t, "", 0, capacity.NodeCapacity{VCPUs: 8, MemoryMB: 1024}, map[string]*vm.Instance{
		"q": {Name: "q", Config: admissionService("q", 1, 768, 0, ""), State: vm.StateRecoveryPending},
		"m": c7Running("m", 2, 256, 0),
		"x": c7Running("x", 1, 128, 0),
	}, desired...)
	a.tick(context.Background())
	if got := m.instances["m"].Config; got.MemoryMB != 128 {
		t.Fatalf("shrink waited: %+v", got)
	}
	if got := m.instances["x"].Config; got.VCPUs != 3 {
		t.Fatalf("CPU growth waited for memory: %+v", got)
	}
	if m.instances["c"] != nil {
		t.Fatal("c took memory the node does not have")
	}
	if err := a.restartService(context.Background(), "m"); err != nil {
		t.Fatalf("same-size restart refused: %v", err)
	}
	a.tick(context.Background())
	if m.instances["c"] != nil || len(m.overcommits) != 0 {
		t.Fatalf("c=%+v overcommits=%v", m.instances["c"], m.overcommits)
	}
	// Only the quarantine keeps c waiting now; a capacity wait keeps the
	// revision reconciling and names the quarantine (C6).
	if vms := conditionOf(a.agentStatusSnapshot(), "VMsReconciled"); !strings.Contains(vms.Message, "hold c ("+capacityOwnershipReason) || a.lastRevision == "r2" {
		t.Fatalf("lastRevision=%q status=%s", a.lastRevision, vms.Message)
	}
}

// C7 and L2: two VMs trading sizes on a full node fit only if one grows
// before the other shrinks. That needs headroom, so neither moves, and the
// status says so instead of claiming progress. With headroom both converge.
func TestC7SizeSwapWaitsForHeadroom(t *testing.T) {
	desired := []c7Service{{"a", 2, 256, 0}, {"b", 1, 512, 0}}
	for name, limit := range map[string]capacity.NodeCapacity{
		"full node":     {VCPUs: 3, MemoryMB: 768},
		"with headroom": {VCPUs: 4, MemoryMB: 1024},
	} {
		t.Run(name, func(t *testing.T) {
			a, m := c7Agent(t, "", 0, limit, map[string]*vm.Instance{
				"a": c7Running("a", 1, 512, 0),
				"b": c7Running("b", 2, 256, 0),
			}, desired...)
			a.tick(context.Background())
			a.tick(context.Background())
			if len(m.overcommits) != 0 {
				t.Fatal(m.overcommits)
			}
			problem := c7Converged(m, desired...)
			if name == "with headroom" {
				if problem != "" || a.lastRevision != "r2" {
					t.Fatalf("did not converge: %s", problem)
				}
				return
			}
			vms := conditionOf(a.agentStatusSnapshot(), "VMsReconciled")
			if len(m.startCalls) != 0 || a.lastRevision == "r2" || !strings.Contains(vms.Message, capacityBlockedReason) {
				t.Fatalf("starts=%v lastRevision=%q status=%s", m.startCalls, a.lastRevision, vms.Message)
			}
		})
	}
}

// C7: replacing a service on a full node waits for the removal to succeed,
// then converges on the next tick.
func TestC7ReplacementOnFullNodeWaitsForRemoval(t *testing.T) {
	desired := []c7Service{{"next", 4, 64, 0}}
	a, m := c7Agent(t, "", 0, capacity.NodeCapacity{VCPUs: 4, MemoryMB: 4096}, map[string]*vm.Instance{
		"old": c7Running("old", 4, 64, 0),
	}, desired...)
	a.tick(context.Background())
	if m.instances["old"] != nil || m.instances["next"] != nil || a.lastRevision == "r2" {
		t.Fatalf("first tick: old=%v next=%v lastRevision=%q", m.instances["old"], m.instances["next"], a.lastRevision)
	}
	if vms := conditionOf(a.agentStatusSnapshot(), "VMsReconciled"); !strings.Contains(vms.Message, capacityTransientReason) {
		t.Fatalf("wait for the removal not reported as transient: %s", vms.Message)
	}
	a.tick(context.Background())
	if problem := c7Converged(m, desired...); problem != "" || a.lastRevision != "r2" || len(m.overcommits) != 0 {
		t.Fatalf("second tick: %s lastRevision=%q overcommits=%v", problem, a.lastRevision, m.overcommits)
	}
}

// C6: a service waiting for a shrink this tick performs is temporary
// waiting. A quarantined VM elsewhere must not let the revision apply as
// degraded while it waits; once only the quarantine remains, it does.
func TestC6WaitForShrinkKeepsRevisionIncompleteBesideQuarantine(t *testing.T) {
	desired := []c7Service{{"b", 2, 64, 0}, {"s", 2, 64, 0}}
	a, m := c7Agent(t, "", 0, capacity.NodeCapacity{VCPUs: 6, MemoryMB: 4096}, map[string]*vm.Instance{
		"q": {Name: "q", Config: admissionService("q", 1, 64, 0, ""), State: vm.StateRecoveryPending},
		"s": c7Running("s", 4, 64, 0),
	}, desired...)
	a.tick(context.Background())
	vms := conditionOf(a.agentStatusSnapshot(), "VMsReconciled")
	if m.instances["b"] != nil || a.lastRevision == "r2" || !strings.Contains(vms.Message, capacityTransientReason) {
		t.Fatalf("first tick: b=%v lastRevision=%q status=%s", m.instances["b"], a.lastRevision, vms.Message)
	}
	a.tick(context.Background())
	if problem := c7Converged(m, desired...); problem != "" || a.lastRevision != "r2" {
		t.Fatalf("second tick: %s lastRevision=%q", problem, a.lastRevision)
	}
}

// C6 and L2: once q resolves, name-order admission takes b's growth before
// d's create, then holds d, whose stale port keeps a and b waiting, so d
// never fits on this node without headroom. Recovery does not explain that
// wait, so while q is quarantined the revision must not apply as degraded,
// and after it resolves the status says what the wait needs.
func TestC6AdmissionWaitRecoveryWouldNotEndIsNotRecovery(t *testing.T) {
	desired := []c7Service{{"a", 2, 256, 8080}, {"b", 2, 1024, 5050}, {"d", 2, 256, 7070}}
	a, m := c7Agent(t, "", 0, capacity.NodeCapacity{VCPUs: 6, MemoryMB: 2048}, map[string]*vm.Instance{
		"a": c7Running("a", 3, 512, 5050),
		"b": c7Running("b", 1, 256, 9090),
		"d": {Name: "d", Config: admissionService("d", 2, 512, 8080, ""), State: vm.StateStopped},
		"q": {Name: "q", Config: admissionService("q", 1, 256, 1111, ""), State: vm.StateRecoveryPending},
	}, desired...)
	for tick := 0; tick < 3; tick++ {
		if tick == 1 {
			delete(m.instances, "q")
		}
		a.tick(context.Background())
		vms := conditionOf(a.agentStatusSnapshot(), "VMsReconciled")
		if a.lastRevision == "r2" || !strings.Contains(vms.Message, "hold d (capacity is used by VMs no pending change frees") || len(m.overcommits) != 0 {
			t.Fatalf("tick %d: lastRevision=%q overcommits=%v status=%s", tick, a.lastRevision, m.overcommits, vms.Message)
		}
	}
}

// C6: two VMs trading sizes on a full node wait for headroom whatever happens
// to recovery. A quarantined VM with unknown claims also holds both, but that
// hold must not let the revision apply as degraded over a wait that
// resolving the quarantine would not end.
func TestC6RecoveryHoldDoesNotHideCapacityWait(t *testing.T) {
	desired := []c7Service{{"a", 2, 256, 0}, {"b", 1, 512, 0}}
	a, m := c7Agent(t, "", 0, capacity.NodeCapacity{VCPUs: 3, MemoryMB: 768}, map[string]*vm.Instance{
		"a": c7Running("a", 1, 512, 0),
		"b": c7Running("b", 2, 256, 0),
		"q": {Name: "q", Config: config.ServiceConfig{Name: "q"}, State: vm.StateRecoveryPending, RecoveryConfigUnknown: true},
	}, desired...)
	for tick := 0; tick < 2; tick++ {
		if tick == 1 {
			delete(m.instances, "q")
		}
		a.tick(context.Background())
		vms := conditionOf(a.agentStatusSnapshot(), "VMsReconciled")
		if a.lastRevision == "r2" || !strings.Contains(vms.Message, "no pending change frees") || len(m.startCalls) != 0 {
			t.Fatalf("tick %d: lastRevision=%q starts=%v status=%s", tick, a.lastRevision, m.startCalls, vms.Message)
		}
	}
}

// C7 and L1: a quarantined VM whose configuration is unknown may still use
// any amount of capacity, so a health restart of a failed VM must not start
// into capacity it is presumed not to use.
func TestC7UnknownQuarantineCapacityIsNotFree(t *testing.T) {
	a, m := c7Agent(t, "", 0, capacity.NodeCapacity{VCPUs: 4, MemoryMB: 1024}, map[string]*vm.Instance{
		"f": {Name: "f", Config: admissionService("f", 4, 1024, 0, ""), State: vm.StateFailed},
		"q": {Name: "q", Config: config.ServiceConfig{Name: "q"}, State: vm.StateRecoveryPending, RecoveryConfigUnknown: true},
	}, c7Service{"f", 4, 1024, 0})
	a.tick(context.Background())
	if err := a.restartService(context.Background(), "f"); err == nil || m.instances["f"].State != vm.StateFailed {
		t.Fatalf("restart started f beside a VM of unknown size: err=%v f=%+v", err, m.instances["f"])
	}
	// Once the quarantine resolves, the unchanged revision recreates f.
	delete(m.instances, "q")
	a.tick(context.Background())
	if problem := c7Converged(m, c7Service{"f", 4, 1024, 0}); problem != "" || len(m.overcommits) != 0 {
		t.Fatalf("f did not recover: %s overcommits=%v", problem, m.overcommits)
	}
}

// C7: a health restart waits for capacity under a desired state that fits,
// here behind a shrink whose stop keeps failing, and the service converges
// once the shrink succeeds. Regression coverage for the start-time check.
func TestC7HealthRestartWaitsForFailingShrink(t *testing.T) {
	desired := []c7Service{{"f", 2, 64, 0}, {"s", 2, 64, 0}}
	a, m := c7Agent(t, "", 0, capacity.NodeCapacity{VCPUs: 4, MemoryMB: 4096}, map[string]*vm.Instance{
		"f": {Name: "f", Config: admissionService("f", 2, 64, 0, ""), State: vm.StateFailed},
		"s": c7Running("s", 4, 64, 0),
	}, desired...)
	failing := true
	m.removeHook = func(name string) error {
		if failing && name == "s" {
			return errors.New("injected stop failure")
		}
		return nil
	}
	a.tick(context.Background())
	if err := a.restartService(context.Background(), "f"); err == nil || m.instances["f"].State != vm.StateFailed {
		t.Fatalf("restart took capacity s still uses: err=%v f=%+v", err, m.instances["f"])
	}
	failing = false
	for tick := 0; tick < 3; tick++ {
		a.tick(context.Background())
	}
	if problem := c7Converged(m, desired...); problem != "" || len(m.overcommits) != 0 || a.lastRevision != "r2" {
		t.Fatalf("%s; overcommits=%v; %s", problem, m.overcommits, a.agentStatusSnapshot().Message)
	}
}

// C6: two VMs trading sizes on a full node wait for headroom however the
// quarantine resolves: when another change frees an unrelated resource
// meanwhile, when the quarantined VM is adopted at its current size rather
// than removed, also after its unknown configuration is restored, and when
// the same tick's growth of an unrelated service takes the headroom the swap
// would have had. Recovery therefore does not explain the wait, and the
// revision must not apply as degraded, before or after it resolves.
func TestC6RecoveryHoldDoesNotHideWaitThatOutlastsRecovery(t *testing.T) {
	adopt := func(m *recoveryTestVMManager, tick int) {
		if tick == 1 {
			m.instances["q"].State = vm.StateRunning
		}
	}
	cases := map[string]struct {
		limit     capacity.NodeCapacity
		instances func() map[string]*vm.Instance
		desired   []c7Service
		resolve   func(m *recoveryTestVMManager, tick int)
	}{
		"another shrink frees memory meanwhile": {
			limit: capacity.NodeCapacity{VCPUs: 4, MemoryMB: 896},
			instances: func() map[string]*vm.Instance {
				return map[string]*vm.Instance{
					"a": c7Running("a", 1, 512, 0),
					"b": c7Running("b", 2, 256, 0),
					"c": c7Running("c", 1, 128, 0),
					"q": {Name: "q", Config: config.ServiceConfig{Name: "q"}, State: vm.StateRecoveryPending, RecoveryConfigUnknown: true},
				}
			},
			desired: []c7Service{{"a", 2, 256, 0}, {"b", 1, 512, 0}, {"c", 1, 64, 0}},
			resolve: func(m *recoveryTestVMManager, tick int) {
				if tick == 1 {
					delete(m.instances, "q")
				}
			},
		},
		"quarantined VM is adopted": {
			limit: capacity.NodeCapacity{VCPUs: 3, MemoryMB: 768},
			instances: func() map[string]*vm.Instance {
				return map[string]*vm.Instance{
					"b": c7Running("b", 2, 256, 0),
					"q": {Name: "q", Config: admissionService("q", 1, 512, 0, ""), State: vm.StateRecoveryPending},
				}
			},
			desired: []c7Service{{"b", 1, 512, 0}, {"q", 2, 256, 0}},
			resolve: adopt,
		},
		"configuration restored, then adopted": {
			limit: capacity.NodeCapacity{VCPUs: 3, MemoryMB: 768},
			instances: func() map[string]*vm.Instance {
				return map[string]*vm.Instance{
					"b": c7Running("b", 2, 256, 0),
					"q": {Name: "q", Config: admissionService("q", 1, 512, 0, ""), State: vm.StateRecoveryPending, RecoveryConfigUnknown: true},
				}
			},
			desired: []c7Service{{"b", 1, 512, 0}, {"q", 2, 256, 0}},
			resolve: func(m *recoveryTestVMManager, tick int) {
				switch tick {
				case 1:
					m.instances["q"].RecoveryConfigUnknown = false
				case 2:
					m.instances["q"].State = vm.StateRunning
				}
			},
		},
		"undesired quarantine restored, adopted and removed": {
			limit: capacity.NodeCapacity{VCPUs: 7, MemoryMB: 1536},
			instances: func() map[string]*vm.Instance {
				return map[string]*vm.Instance{
					"a": c7Running("a", 3, 256, 0),
					"d": c7Running("d", 1, 768, 0),
					"q": {Name: "q", Config: admissionService("q", 1, 256, 0, ""), State: vm.StateRecoveryPending, RecoveryConfigUnknown: true},
				}
			},
			desired: []c7Service{{"a", 2, 768, 0}, {"b", 2, 256, 0}, {"c", 1, 256, 0}, {"d", 2, 256, 0}},
			resolve: func(m *recoveryTestVMManager, tick int) {
				if q := m.instances["q"]; tick == 1 && q != nil {
					q.RecoveryConfigUnknown, q.State = false, vm.StateRunning
				}
			},
		},
		"desired quarantine of unknown size needs the shrink it blocks": {
			limit: capacity.NodeCapacity{VCPUs: 4, MemoryMB: 4096},
			instances: func() map[string]*vm.Instance {
				return map[string]*vm.Instance{
					"s": c7Running("s", 3, 64, 7070),
					"q": {Name: "q", Config: admissionService("q", 1, 64, 8080, ""), State: vm.StateRecoveryPending, RecoveryConfigUnknown: true},
				}
			},
			desired: []c7Service{{"q", 3, 64, 9090}, {"s", 1, 64, 8080}},
			resolve: func(m *recoveryTestVMManager, tick int) {
				switch tick {
				case 1:
					m.instances["q"].RecoveryConfigUnknown = false
				case 2:
					m.instances["q"].State = vm.StateRunning
				}
			},
		},
		"two desired quarantines of unknown size trade sizes": {
			limit: capacity.NodeCapacity{VCPUs: 4, MemoryMB: 4096},
			instances: func() map[string]*vm.Instance {
				return map[string]*vm.Instance{
					"q1": {Name: "q1", Config: admissionService("q1", 3, 64, 7070, ""), State: vm.StateRecoveryPending, RecoveryConfigUnknown: true},
					"q2": {Name: "q2", Config: admissionService("q2", 1, 64, 8080, ""), State: vm.StateRecoveryPending, RecoveryConfigUnknown: true},
				}
			},
			desired: []c7Service{{"q1", 1, 64, 8080}, {"q2", 3, 64, 0}},
			resolve: func(m *recoveryTestVMManager, tick int) {
				for _, name := range []string{"q1", "q2"} {
					switch tick {
					case 1:
						m.instances[name].RecoveryConfigUnknown = false
					case 2:
						m.instances[name].State = vm.StateRunning
					}
				}
			},
		},
		"same-tick growth takes the headroom": {
			limit: capacity.NodeCapacity{VCPUs: 5, MemoryMB: 1088},
			instances: func() map[string]*vm.Instance {
				q := admissionService("q", 1, 64, 0, "")
				q.PortForwards = []config.PortForward{{HostPort: 80, VMPort: 80}, {HostPort: 81, VMPort: 80}}
				return map[string]*vm.Instance{
					"a": c7Running("a", 1, 512, 0),
					"b": c7Running("b", 2, 256, 0),
					"q": {Name: "q", Config: q, State: vm.StateRecoveryPending},
				}
			},
			desired: []c7Service{{"a", 2, 256, 80}, {"b", 1, 512, 81}, {"q", 1, 64, 0}, {"x", 1, 256, 0}},
			resolve: adopt,
		},
	}
	for name, tc := range cases {
		for _, strategy := range c7Strategies {
			t.Run(name+"/"+strategy, func(t *testing.T) {
				a, m := c7Agent(t, strategy, 0, tc.limit, tc.instances(), tc.desired...)
				for tick := 0; tick < 4; tick++ {
					tc.resolve(m, tick)
					a.tick(context.Background())
					if applied := a.agentStatusSnapshot().AppliedRevision; a.lastRevision == "r2" || applied == "r2" || len(m.overcommits) != 0 {
						t.Fatalf("tick %d: lastRevision=%q applied=%q overcommits=%v status=%s", tick, a.lastRevision, applied, m.overcommits, conditionOf(a.agentStatusSnapshot(), "VMsReconciled").Message)
					}
				}
			})
		}
	}
}

// C6: a hold that only recovery causes and that needs no capacity, here a
// port change held by the unknown claims of a quarantined desired VM, still
// lets the revision apply as degraded.
func TestC6RecoveryOnlyClaimHoldAppliesAsDegraded(t *testing.T) {
	a, m := c7Agent(t, "", 0, capacity.NodeCapacity{VCPUs: 8, MemoryMB: 4096}, map[string]*vm.Instance{
		"b": c7Running("b", 1, 64, 0),
		"q": {Name: "q", Config: config.ServiceConfig{Name: "q"}, State: vm.StateRecoveryPending, RecoveryConfigUnknown: true},
	}, c7Service{"b", 1, 64, 8080}, c7Service{"q", 1, 64, 0})
	a.tick(context.Background())
	if applied := a.agentStatusSnapshot().AppliedRevision; applied != "r2" || len(m.startCalls) != 0 {
		t.Fatalf("applied=%q starts=%v status=%s", applied, m.startCalls, conditionOf(a.agentStatusSnapshot(), "VMsReconciled").Message)
	}
}

// C6 honest status: a capacity wait names the quarantine only when it would
// end with it, counting removals recovery holds as gone. Two VMs trading
// sizes beside an unrelated quarantine still ask for headroom, and a service
// that has room but waits on a quarantined VM's claims names those claims.
func TestC6CapacityWaitNamesItsCause(t *testing.T) {
	for name, tc := range map[string]struct {
		limit     capacity.NodeCapacity
		instances map[string]*vm.Instance
		desired   []c7Service
		want      string
	}{
		"swap beside an unrelated quarantine": {
			limit: capacity.NodeCapacity{VCPUs: 4, MemoryMB: 1024},
			instances: map[string]*vm.Instance{
				"a": c7Running("a", 1, 512, 0),
				"b": c7Running("b", 2, 256, 0),
				"q": {Name: "q", Config: admissionService("q", 1, 256, 0, ""), State: vm.StateRecoveryPending},
			},
			desired: []c7Service{{"a", 2, 256, 0}, {"b", 1, 512, 0}, {"q", 1, 256, 0}},
			want:    "hold a (" + capacityBlockedReason,
		},
		"growth behind a removal recovery holds": {
			limit: capacity.NodeCapacity{VCPUs: 4, MemoryMB: 4096},
			instances: map[string]*vm.Instance{
				"b": c7Running("b", 1, 64, 0),
				"x": c7Running("x", 2, 64, 7070),
				"q": {Name: "q", Config: admissionService("q", 1, 64, 7070, ""), State: vm.StateRecoveryPending},
			},
			desired: []c7Service{{"b", 3, 64, 0}},
			want:    "hold b (" + capacityOwnershipReason,
		},
		"new service held only by a quarantined VM's port": {
			limit: capacity.NodeCapacity{VCPUs: 16, MemoryMB: 8192},
			instances: map[string]*vm.Instance{
				"x": {Name: "x", Config: admissionService("x", 1, 64, 7070, ""), State: vm.StateRecoveryPending},
			},
			desired: []c7Service{{"n", 1, 64, 7070}},
			want:    "hold n (host port 7070",
		},
		"room left beside a quarantine of unknown size": {
			limit: capacity.NodeCapacity{VCPUs: 16, MemoryMB: 8192},
			instances: map[string]*vm.Instance{
				"b": c7Running("b", 1, 64, 0),
				"q": {Name: "q", Config: config.ServiceConfig{Name: "q"}, State: vm.StateRecoveryPending, RecoveryConfigUnknown: true},
			},
			desired: []c7Service{{"b", 2, 64, 0}, {"q", 1, 64, 0}},
			want:    "hold b (runtime resource claims are unknown for q",
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := c7Agent(t, "", 0, tc.limit, tc.instances, tc.desired...)
			a.tick(context.Background())
			if vms := conditionOf(a.agentStatusSnapshot(), "VMsReconciled"); a.lastRevision == "r2" || !strings.Contains(vms.Message, tc.want) {
				t.Fatalf("lastRevision=%q status=%s", a.lastRevision, vms.Message)
			}
		})
	}
}

// C6: desired state that cannot fit the node fails the revision, also when
// every service already runs, over capacity, and one of them is quarantined.
func TestC6ImpossibleDesiredCapacityFailsWhenNothingWaits(t *testing.T) {
	a, _ := c7Agent(t, "", 0, capacity.NodeCapacity{VCPUs: 4, MemoryMB: 4096}, map[string]*vm.Instance{
		"a": c7Running("a", 3, 64, 0),
		"q": {Name: "q", Config: admissionService("q", 2, 64, 0, ""), State: vm.StateRecoveryPending},
	}, c7Service{"a", 3, 64, 0}, c7Service{"q", 2, 64, 0})
	a.tick(context.Background())
	if status := a.agentStatusSnapshot(); status.Phase != "failed" || status.ReasonCode != "capacity_exceeded" || status.AppliedRevision == "r2" {
		t.Fatalf("phase=%s reason=%s applied=%q", status.Phase, status.ReasonCode, status.AppliedRevision)
	}
}
