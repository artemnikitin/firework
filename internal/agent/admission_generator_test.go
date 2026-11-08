package agent

import (
	"context"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/vm"
)

type generatedService struct {
	name       string
	vcpus, mem int
	hostPort   int
}

func generatedYAML(services []generatedService) []byte {
	if len(services) == 0 {
		return []byte("node: web\nservices: []\n")
	}
	var b strings.Builder
	b.WriteString("node: web\nservices:\n")
	for _, s := range services {
		fmt.Fprintf(&b, "- name: %s\n  image: /%s\n  kernel: /k\n  vcpus: %d\n  memory_mb: %d\n  network: {}\n  port_forwards:\n  - host_port: %d\n    vm_port: 80\n", s.name, s.name, s.vcpus, s.mem, s.hostPort)
	}
	return []byte(b.String())
}

const generatedVCPUs, generatedMemoryMB = 6, 2048

// generatedRevision returns up to four services with distinct host ports that
// fit the node together.
// generatedRevision draws each service's host port from its own pool, so a
// revision may move a service's port but never gives one service another's
// old port, which L6 does not support.
func generatedRevision(r *rand.Rand) []generatedService {
	for {
		ports := []int{5050 + r.Intn(2), 6060 + r.Intn(2), 7070 + r.Intn(2), 8080 + r.Intn(2)}
		var out []generatedService
		vcpus, mem := 0, 0
		for i, name := range []string{"a", "b", "c", "d"} {
			if r.Intn(4) == 0 {
				continue
			}
			s := generatedService{name, 1 + r.Intn(3), []int{256, 512, 1024}[r.Intn(3)], ports[i]}
			vcpus += s.vcpus
			mem += s.mem
			out = append(out, s)
		}
		if vcpus <= generatedVCPUs && mem <= generatedMemoryMB {
			return out
		}
	}
}

// Random revisions over both update strategies, with crashed and stopped VMs,
// a VM still starting or stopping, and a quarantined survivor that may be
// desired and whose configuration may be unknown, each resolving after a few
// ticks, on a tight node and on one with headroom. No start may
// take capacity that is not free at that moment (C7). A revision with
// transition headroom, where every service fits at the larger of its old and
// new size, must converge within a bounded number of ticks once the
// quarantine resolves (C5); without headroom it may wait (L2), but must not
// report itself reconciled. While the quarantine
// stands, a tick that changes nothing must not report a wait the quarantine
// does not explain, and must apply the revision as degraded once every
// service has the capacity it needs (C6).
func TestRandomRevisionsConvergeWithinCapacity(t *testing.T) {
	seeds := int64(600)
	if testing.Short() {
		seeds = 100
	}
	failures := 0
	for seed := int64(0); seed < seeds*2 && failures < 5; seed++ {
		// Every seed runs on the node the revisions were drawn for, then
		// again on one with headroom for the transition and the survivor.
		roomy := seed >= seeds
		r := rand.New(rand.NewSource(seed % seeds))
		from, to := generatedRevision(r), generatedRevision(r)
		// A quarantined survivor may be desired, with a configuration known or
		// unknown to the agent, and resolves by going away or by adoption,
		// after its configuration is restored if it was unknown.
		qWanted, qUnknown, qAdopt := r.Intn(2) == 0, r.Intn(2) == 0, r.Intn(2) == 0
		qDesired := generatedService{"q", 1 + r.Intn(2), []int{256, 512}[r.Intn(2)], 1111}
		need := map[string][2]int{}
		for _, s := range append(slices.Clone(from), to...) {
			need[s.name] = [2]int{max(need[s.name][0], s.vcpus), max(need[s.name][1], s.mem)}
		}
		var needVCPUs, needMemory int
		for _, n := range need {
			needVCPUs, needMemory = needVCPUs+n[0], needMemory+n[1]
		}
		capVCPUs, capMemoryMB := generatedVCPUs, generatedMemoryMB
		if roomy {
			capVCPUs, capMemoryMB = max(capVCPUs, needVCPUs+2), max(capMemoryMB, needMemory+512)
		}
		strategy := ""
		if r.Intn(2) == 0 {
			strategy = "rolling"
		}
		cfg := testAgentConfig(t)
		enabled := true
		cfg.EnableCapacityCheck = &enabled
		cfg.VMSubnet, cfg.VMGateway = "172.16.0.0/29", "172.16.0.1"
		store := &fakeStore{revision: "r1", data: map[string][]byte{"web": generatedYAML(from)}}
		a := New(cfg, store, testLogger())
		limit := capacity.NodeCapacity{VCPUs: capVCPUs, MemoryMB: capMemoryMB}
		a.capacityReader = &fakeCapacityReader{cap: limit}
		m := &recoveryTestVMManager{instances: map[string]*vm.Instance{}, limit: &limit}
		a.reconciler = testReconciler(t, m, &recoveryTestNetworkManager{}, strategy, time.Microsecond)
		a.tick(context.Background())
		if a.lastRevision != "r1" {
			t.Fatalf("seed %d: setup did not apply: %s", seed%seeds, a.agentStatusSnapshot().Message)
		}
		for _, name := range []string{"a", "b", "c", "d"} {
			if inst := m.instances[name]; inst != nil {
				switch r.Intn(4) {
				case 0:
					inst.State = vm.StateFailed
				case 1:
					inst.State = vm.StateStopped
				}
			}
		}
		// A desired VM can still be starting or stopping when the revision
		// arrives. It keeps its size, and its claims, until that resolves.
		lifecycle, lifecycleTicks := "", 0
		for _, name := range []string{"a", "b", "c", "d"} {
			if inst := m.instances[name]; inst != nil && inst.State == vm.StateRunning && lifecycle == "" && r.Intn(3) == 0 {
				inst.State = []vm.State{vm.StateStarting, vm.StateStopping}[r.Intn(2)]
				lifecycle, lifecycleTicks = name, 1+r.Intn(3)
			}
		}
		// The survivor fits beside what already runs: the agent cannot evict a
		// live VM, so an overcommit present before the revision is not its doing.
		quarantineTicks := -1
		usedVCPUs, usedMemory := runtimeUsage(m)
		var qConfig config.ServiceConfig
		if r.Intn(3) == 0 && usedVCPUs < capVCPUs && usedMemory+256 <= capMemoryMB {
			quarantineTicks = 1 + r.Intn(3)
			qConfig = config.ServiceConfig{
				Name: "q", Image: "/q", Kernel: "/k", VCPUs: min(1+r.Intn(2), capVCPUs-usedVCPUs), MemoryMB: 256,
				PortForwards: []config.PortForward{{HostPort: []int{1111, 1112}[r.Intn(2)], VMPort: 80}},
				Network:      &config.NetworkConfig{GuestIP: "172.16.0.6", GuestMAC: guestMACForIndex(4)},
			}
			q := &vm.Instance{Name: "q", State: vm.StateRecoveryPending, Config: qConfig}
			if qUnknown {
				q.Config, q.RecoveryConfigUnknown = config.ServiceConfig{Name: "q"}, true
				m.hidden = map[string]capacity.NodeCapacity{"q": {VCPUs: qConfig.VCPUs, MemoryMB: qConfig.MemoryMB}}
			}
			m.instances["q"] = q
		}
		if quarantineTicks < 0 {
			qWanted, qUnknown, qAdopt = false, false, false
		}
		if qWanted {
			to = append(slices.Clone(to), qDesired)
			needVCPUs += max(qConfig.VCPUs, qDesired.vcpus)
			needMemory += max(qConfig.MemoryMB, qDesired.mem)
		}
		headroom := needVCPUs <= capVCPUs && needMemory <= capMemoryMB
		store.revision = "r2"
		store.data["web"] = generatedYAML(to)
		var problem string
		appliedAt := -1 // the tick that applied the revision while quarantined
		for tick := 0; tick < len(from)+len(to)+5+lifecycleTicks && problem == ""; tick++ {
			if q := m.instances["q"]; q != nil && tick == quarantineTicks {
				// Resolve it as clear-recovery does: change the runtime, then
				// apply the recovery wake, which forces the next tick.
				delete(m.hidden, "q")
				if qAdopt {
					q.Config, q.RecoveryConfigUnknown, q.State = qConfig, false, vm.StateRunning
					a.reconciler.MarkRecovered("q")
				} else {
					m.clear(t, "q")
					a.reconciler.ForgetRecovered("q")
				}
				a.forceReconcile = true
			}
			if inst := m.instances[lifecycle]; inst != nil && tick == lifecycleTicks {
				if inst.State == vm.StateStarting {
					inst.State = vm.StateRunning
				} else {
					inst.State = vm.StateStopped
				}
			}
			actions := len(m.startCalls) + len(m.removeCalls)
			a.tick(context.Background())
			if vcpus, mem := runtimeUsage(m); vcpus > capVCPUs || mem > capMemoryMB {
				problem = fmt.Sprintf("tick %d runs %d vCPUs and %d MB", tick, vcpus, mem)
			}
			if len(m.overcommits) != 0 {
				problem = fmt.Sprintf("tick %d: %v", tick, m.overcommits)
			}
			vms := conditionOf(a.agentStatusSnapshot(), "VMsReconciled")
			quarantined := m.instances["q"] != nil && m.instances["q"].State == vm.StateRecoveryPending
			idle := actions == len(m.startCalls)+len(m.removeCalls)
			// Every desired service already runs at least its desired size, so
			// nothing waits for capacity.
			sized := true
			for _, want := range to {
				if got := m.instances[want.name]; got == nil || !reconciler.UsesCapacity(got.State) || got.Config.VCPUs < want.vcpus || got.Config.MemoryMB < want.mem {
					sized = false
				}
			}
			if problem == "" && quarantined && tick > lifecycleTicks {
				switch {
				case appliedAt >= 0 && !idle:
					problem = fmt.Sprintf("tick %d still changes VMs after tick %d applied the revision as degraded", tick, appliedAt)
				case idle && headroom && (strings.Contains(vms.Message, "no pending change frees") || strings.Contains(vms.Message, "still used by VMs that are starting") || strings.Contains(vms.Message, "lifecycle is in progress")):
					problem = fmt.Sprintf("tick %d changes nothing but reports a hold the quarantine does not explain: %s", tick, vms.Message)
				case idle && sized && a.lastRevision != "r2":
					problem = fmt.Sprintf("tick %d needs no capacity and holds only for recovery, but did not apply the revision as degraded: %s", tick, vms.Message)
				}
				if appliedAt < 0 && a.lastRevision == "r2" {
					appliedAt = tick
				}
			}
		}
		for _, want := range to {
			if problem != "" || !headroom {
				break
			}
			got := m.instances[want.name]
			if got == nil || got.State != vm.StateRunning || got.Config.VCPUs != want.vcpus || got.Config.MemoryMB != want.mem || got.Config.PortForwards[0].HostPort != want.hostPort {
				problem = fmt.Sprintf("%s did not converge: %+v", want.name, got)
			}
		}
		if problem == "" && headroom && a.lastRevision != "r2" {
			problem = "revision not applied: " + a.agentStatusSnapshot().Message
		}
		// Without headroom the transition may wait for good. A revision
		// applied as degraded while only recovery held stays applied, but
		// once recovery resolved the node must not report it reconciled.
		if vms := conditionOf(a.agentStatusSnapshot(), "VMsReconciled"); problem == "" && !headroom && vms.Status == "true" {
			for _, want := range to {
				if got := m.instances[want.name]; got == nil || got.Config.VCPUs != want.vcpus || got.Config.MemoryMB != want.mem {
					problem = fmt.Sprintf("reports VMs reconciled while %s did not converge: %+v", want.name, got)
				}
			}
		}
		if problem != "" {
			failures++
			t.Errorf("seed %d roomy=%v strategy=%q quarantine=%d (desired=%v unknown=%v adopted=%v) lifecycle=%s/%d from=%v to=%v: %s", seed%seeds, roomy, strategy, quarantineTicks, qWanted, qUnknown, qAdopt, lifecycle, lifecycleTicks, from, to, problem)
		}
	}
}

// One tick from any mix of running, starting, stopping, crashed and
// quarantined VMs, known or of unknown configuration, with sizes and host ports moving between services, ends
// within capacity whenever the node started within it and the apply succeeds.
// Holds feed back into admission through the claims of held VMs, so this
// checks that admission settles on the holds it installs.
func TestRandomTickEndsWithinCapacity(t *testing.T) {
	const capVCPUs, capMemoryMB = 8, 768
	seeds := int64(20000)
	if testing.Short() {
		seeds = 2000
	}
	states := []vm.State{vm.StateRunning, vm.StateRunning, vm.StateRunning, vm.StateStarting, vm.StateStopping, vm.StateFailed, vm.StateRecoveryPending}
	failures := 0
	for seed := int64(0); seed < seeds && failures < 5; seed++ {
		r := rand.New(rand.NewSource(seed))
		ports := []int{5050, 6060, 7070, 8080, 9090}
		instances := map[string]*vm.Instance{}
		r.Shuffle(len(ports), func(i, j int) { ports[i], ports[j] = ports[j], ports[i] })
		for i, name := range []string{"a", "b", "c", "d", "e"} {
			if r.Intn(4) != 0 {
				instances[name] = &vm.Instance{Name: name, State: states[r.Intn(len(states))], Config: admissionService(name, 1+r.Intn(3), 64*(1+r.Intn(4)), ports[i], "")}
			}
		}
		manager := &recoveryTestVMManager{instances: instances, limit: &capacity.NodeCapacity{VCPUs: capVCPUs, MemoryMB: capMemoryMB}}
		// A quarantined VM's configuration may be unknown to the agent while
		// its process still uses what it was given.
		for _, name := range slices.Sorted(maps.Keys(instances)) {
			if inst := instances[name]; inst.State == vm.StateRecoveryPending && r.Intn(2) == 0 {
				if manager.hidden == nil {
					manager.hidden = map[string]capacity.NodeCapacity{}
				}
				manager.hidden[name] = capacity.NodeCapacity{VCPUs: inst.Config.VCPUs, MemoryMB: inst.Config.MemoryMB}
				inst.Config, inst.RecoveryConfigUnknown = config.ServiceConfig{Name: name}, true
			}
		}
		if vcpus, mem := runtimeUsage(manager); vcpus > capVCPUs || mem > capMemoryMB {
			continue
		}
		r.Shuffle(len(ports), func(i, j int) { ports[i], ports[j] = ports[j], ports[i] })
		var b strings.Builder
		b.WriteString("node: web\nservices:\n")
		for i, name := range []string{"a", "b", "c", "d", "e"} {
			if r.Intn(4) != 0 {
				fmt.Fprintf(&b, "- name: %s\n  image: /%s\n  kernel: /k\n  vcpus: %d\n  memory_mb: %d\n  port_forwards:\n  - host_port: %d\n    vm_port: 80\n", name, name, 1+r.Intn(3), 64*(1+r.Intn(4)), ports[i])
			}
		}
		strategy := ""
		if r.Intn(2) == 0 {
			strategy = "rolling"
		}
		before := fmt.Sprint(describeInstances(instances))
		a, _ := admissionAgent(t, b.String(), capacity.NodeCapacity{VCPUs: capVCPUs, MemoryMB: capMemoryMB}, instances, "")
		a.reconciler = reconciler.NewWithNetworkManager(manager, testLogger(), nil, &recoveryTestNetworkManager{}, strategy, time.Microsecond)
		a.tick(context.Background())
		if vcpus, mem := runtimeUsage(manager); vcpus > capVCPUs || mem > capMemoryMB || len(manager.overcommits) != 0 {
			failures++
			t.Errorf("seed %d strategy=%q: %d vCPUs and %d MB after one tick, %v\nbefore: %s\ndesired:\n%s", seed, strategy, vcpus, mem, manager.overcommits, before, b.String())
		}
	}
}

func describeInstances(instances map[string]*vm.Instance) []string {
	var out []string
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		switch inst := instances[name]; {
		case inst == nil:
		case inst.RecoveryConfigUnknown:
			out = append(out, fmt.Sprintf("%s:%s:unknown", name, inst.State))
		default:
			out = append(out, fmt.Sprintf("%s:%s:%d/%d:%d", name, inst.State, inst.Config.VCPUs, inst.Config.MemoryMB, inst.Config.PortForwards[0].HostPort))
		}
	}
	return out
}
