package agent

import (
	"context"
	"fmt"
	"math/rand"
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

// Random revisions over both update strategies, with crashed and stopped VMs
// and a quarantined survivor that resolves after a few ticks, must converge
// within a bounded number of ticks, never run more than the node holds between
// ticks, and never report a hold that recovery does not explain while only the
// quarantine stands in the way.
func TestRandomRevisionsConvergeWithinCapacity(t *testing.T) {
	seeds := int64(600)
	if testing.Short() {
		seeds = 100
	}
	failures := 0
	for seed := int64(0); seed < seeds && failures < 5; seed++ {
		r := rand.New(rand.NewSource(seed))
		from, to := generatedRevision(r), generatedRevision(r)
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
		a.capacityReader = &fakeCapacityReader{cap: capacity.NodeCapacity{VCPUs: generatedVCPUs, MemoryMB: generatedMemoryMB}}
		m := &recoveryTestVMManager{instances: map[string]*vm.Instance{}}
		a.reconciler = reconciler.NewWithNetworkManager(m, testLogger(), nil, &recoveryTestNetworkManager{}, strategy, time.Microsecond)
		a.tick(context.Background())
		if a.lastRevision != "r1" {
			t.Fatalf("seed %d: setup did not apply: %s", seed, a.agentStatusSnapshot().Message)
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
		// The survivor fits beside what already runs: the agent cannot evict a
		// live VM, so an overcommit present before the revision is not its doing.
		quarantineTicks := -1
		usedVCPUs, usedMemory := runtimeUsage(m)
		if r.Intn(3) == 0 && usedVCPUs < generatedVCPUs && usedMemory+256 <= generatedMemoryMB {
			quarantineTicks = 1 + r.Intn(3)
			m.instances["q"] = &vm.Instance{Name: "q", State: vm.StateRecoveryPending, Config: config.ServiceConfig{
				Name: "q", VCPUs: min(1+r.Intn(2), generatedVCPUs-usedVCPUs), MemoryMB: 256,
				PortForwards: []config.PortForward{{HostPort: []int{5050, 6060, 7070, 8080, 9090, 1111}[r.Intn(6)], VMPort: 80}},
				Network:      &config.NetworkConfig{GuestIP: "172.16.0.6", GuestMAC: guestMACForIndex(4)},
			}}
		}
		store.revision = "r2"
		store.data["web"] = generatedYAML(to)
		var problem string
		for tick := 0; tick < len(from)+len(to)+3 && problem == ""; tick++ {
			if tick == quarantineTicks {
				delete(m.instances, "q")
			}
			a.tick(context.Background())
			if vcpus, mem := runtimeUsage(m); vcpus > generatedVCPUs || mem > generatedMemoryMB {
				problem = fmt.Sprintf("tick %d runs %d vCPUs and %d MB", tick, vcpus, mem)
			}
			if _, quarantined := m.instances["q"]; quarantined && tick >= 1 {
				vms := conditionOf(a.agentStatusSnapshot(), "VMsReconciled")
				if strings.Contains(vms.Message, reconciler.ErrServiceHeld.Error()) {
					problem = fmt.Sprintf("tick %d reports a hold recovery does not explain: %s", tick, vms.Message)
				}
			}
		}
		for _, want := range to {
			if problem != "" {
				break
			}
			got := m.instances[want.name]
			if got == nil || got.State != vm.StateRunning || got.Config.VCPUs != want.vcpus || got.Config.MemoryMB != want.mem || got.Config.PortForwards[0].HostPort != want.hostPort {
				problem = fmt.Sprintf("%s did not converge: %+v", want.name, got)
			}
		}
		if problem == "" && a.lastRevision != "r2" {
			problem = "revision not applied: " + a.agentStatusSnapshot().Message
		}
		if problem != "" {
			failures++
			t.Errorf("seed %d strategy=%q quarantine=%d from=%v to=%v: %s", seed, strategy, quarantineTicks, from, to, problem)
		}
	}
}
