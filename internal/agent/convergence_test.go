package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/vm"
)

// runtimeUsage sums what live VMs use between ticks.
func runtimeUsage(m *recoveryTestVMManager) (vcpus, memoryMB int) {
	for _, inst := range m.instances {
		switch inst.State {
		case vm.StateRunning, vm.StateStarting, vm.StateStopping, vm.StateRecoveryPending:
			vcpus += inst.Config.VCPUs
			memoryMB += inst.Config.MemoryMB
		}
	}
	return vcpus, memoryMB
}

type convergenceService struct {
	name       string
	vcpus      int
	hostPort   int
	guestOctet int
}

func convergenceYAML(services []convergenceService) []byte {
	var b strings.Builder
	b.WriteString("node: web\nservices:\n")
	for _, s := range services {
		fmt.Fprintf(&b, "- name: %s\n  image: /%s\n  kernel: /k\n  vcpus: %d\n  memory_mb: 64\n  network:\n    guest_ip: 172.16.0.%d\n  port_forwards:\n  - host_port: %d\n    vm_port: 80\n",
			s.name, s.name, s.vcpus, s.guestOctet, s.hostPort)
	}
	return []byte(b.String())
}

// converge applies from, then switches to to, and reports why to has not
// converged within three ticks, or "" once every VM runs its desired size and
// port and the revision is applied.
func converge(t *testing.T, cap int, from, to []convergenceService) string {
	t.Helper()
	cfg := testAgentConfig(t)
	enabled := true
	cfg.EnableCapacityCheck = &enabled
	store := &fakeStore{revision: "r1", data: map[string][]byte{"web": convergenceYAML(from)}}
	a := New(cfg, store, testLogger())
	a.capacityReader = &fakeCapacityReader{cap: capacity.NodeCapacity{VCPUs: cap, MemoryMB: 4096}}
	manager := &recoveryTestVMManager{instances: map[string]*vm.Instance{}}
	a.reconciler = reconciler.NewWithNetworkManager(manager, testLogger(), nil, &recoveryTestNetworkManager{}, "", 0)
	a.tick(context.Background())
	if a.lastRevision != "r1" {
		return "setup revision did not apply"
	}
	store.revision = "r2"
	store.data["web"] = convergenceYAML(to)
	for tick := 0; tick < 3; tick++ {
		a.tick(context.Background())
		if used, _ := runtimeUsage(manager); used > cap {
			return fmt.Sprintf("tick %d runs %d vCPUs on a %d vCPU node", tick, used, cap)
		}
	}
	for _, want := range to {
		got := manager.instances[want.name]
		if got == nil || got.State != vm.StateRunning || got.Config.VCPUs != want.vcpus || got.Config.PortForwards[0].HostPort != want.hostPort {
			return fmt.Sprintf("%s did not converge: %+v; status %s", want.name, got, a.agentStatusSnapshot().Message)
		}
	}
	if a.lastRevision != "r2" {
		return "revision not applied: " + a.agentStatusSnapshot().Message
	}
	return ""
}

// Any revision whose services fit the node and claim distinct host ports
// converges, whatever sizes and ports the running VMs move from: holds may
// order work across ticks, but no combination of capacity and claim holds may
// wait on itself.
func TestFittingRevisionsConverge(t *testing.T) {
	const capVCPUs = 4
	ports := []int{7070, 8080, 9090}
	sizes := []int{1, 2, 3}
	failures := 0
	check := func(from, to []convergenceService) {
		if failures >= 5 {
			return
		}
		if reason := converge(t, capVCPUs, from, to); reason != "" {
			failures++
			t.Errorf("%+v -> %+v: %s", from, to, reason)
		}
	}
	// Two services: every size and every pair of distinct ports, before and
	// after, including revisions whose old sizes together exceed the node.
	for _, oldA := range sizes {
		for _, oldB := range sizes {
			for _, newA := range sizes {
				for _, newB := range sizes {
					if oldA+oldB > capVCPUs || newA+newB > capVCPUs {
						continue
					}
					for _, oldPorts := range [][2]int{{ports[0], ports[1]}, {ports[1], ports[0]}, {ports[1], ports[2]}, {ports[2], ports[1]}} {
						for _, newPorts := range [][2]int{{ports[0], ports[1]}, {ports[1], ports[0]}, {ports[0], ports[2]}, {ports[2], ports[0]}, {ports[1], ports[2]}, {ports[2], ports[1]}} {
							check(
								[]convergenceService{{"a", oldA, oldPorts[0], 2}, {"b", oldB, oldPorts[1], 3}},
								[]convergenceService{{"a", newA, newPorts[0], 2}, {"b", newB, newPorts[1], 3}},
							)
						}
					}
				}
			}
		}
	}
	// Three services rotating their ports while capacity moves between them.
	for _, sizesBefore := range [][3]int{{2, 1, 1}, {1, 2, 1}, {1, 1, 2}} {
		for _, sizesAfter := range [][3]int{{2, 1, 1}, {1, 2, 1}, {1, 1, 2}} {
			for _, rotation := range [][3]int{{8080, 9090, 7070}, {9090, 7070, 8080}} {
				check(
					[]convergenceService{{"a", sizesBefore[0], 7070, 2}, {"b", sizesBefore[1], 8080, 3}, {"c", sizesBefore[2], 9090, 4}},
					[]convergenceService{{"a", sizesAfter[0], rotation[0], 2}, {"b", sizesAfter[1], rotation[1], 3}, {"c", sizesAfter[2], rotation[2], 4}},
				)
			}
		}
	}
}
