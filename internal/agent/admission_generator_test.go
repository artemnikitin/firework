package agent

import (
	"context"
	"fmt"
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
func generatedRevision(r *rand.Rand) []generatedService {
	ports := []int{5050, 6060, 7070, 8080, 9090}
	r.Shuffle(len(ports), func(i, j int) { ports[i], ports[j] = ports[j], ports[i] })
	for {
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
// a VM still starting or stopping, and a quarantined survivor, each resolving
// after a few ticks, on a tight node and on one with headroom. No start may
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
			capVCPUs, capMemoryMB = max(capVCPUs, needVCPUs+2), max(capMemoryMB, needMemory+256)
		}
		headroom := needVCPUs <= capVCPUs && needMemory <= capMemoryMB
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
		a.reconciler = reconciler.NewWithNetworkManager(m, testLogger(), nil, &recoveryTestNetworkManager{}, strategy, time.Microsecond)
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
		if r.Intn(3) == 0 && usedVCPUs < capVCPUs && usedMemory+256 <= capMemoryMB {
			quarantineTicks = 1 + r.Intn(3)
			m.instances["q"] = &vm.Instance{Name: "q", State: vm.StateRecoveryPending, Config: config.ServiceConfig{
				Name: "q", VCPUs: min(1+r.Intn(2), capVCPUs-usedVCPUs), MemoryMB: 256,
				PortForwards: []config.PortForward{{HostPort: []int{5050, 6060, 7070, 8080, 9090, 1111}[r.Intn(6)], VMPort: 80}},
				Network:      &config.NetworkConfig{GuestIP: "172.16.0.6", GuestMAC: guestMACForIndex(4)},
			}}
		}
		store.revision = "r2"
		store.data["web"] = generatedYAML(to)
		var problem string
		appliedAt := -1 // the tick that applied the revision while quarantined
		for tick := 0; tick < len(from)+len(to)+3+lifecycleTicks && problem == ""; tick++ {
			if tick == quarantineTicks {
				delete(m.instances, "q")
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
			_, quarantined := m.instances["q"]
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
			t.Errorf("seed %d roomy=%v strategy=%q quarantine=%d lifecycle=%s/%d from=%v to=%v: %s", seed%seeds, roomy, strategy, quarantineTicks, lifecycle, lifecycleTicks, from, to, problem)
		}
	}
}

// One tick from any mix of running, starting, stopping, crashed and
// quarantined VMs, with sizes and host ports moving between services, ends
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
		if inst := instances[name]; inst != nil {
			out = append(out, fmt.Sprintf("%s:%s:%d/%d:%d", name, inst.State, inst.Config.VCPUs, inst.Config.MemoryMB, inst.Config.PortForwards[0].HostPort))
		}
	}
	return out
}
