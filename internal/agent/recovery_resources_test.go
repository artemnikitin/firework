package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/vm"
)

func retainedResourceAgent(t *testing.T) *Agent {
	t.Helper()
	cfg := testAgentConfig(t)
	cfg.VMSubnet = "172.16.0.0/24"
	cfg.VMGateway = "172.16.0.1"
	a := New(cfg, &fakeStore{}, testLogger())
	svc := config.ServiceConfig{Name: "removed", Image: "/image", Kernel: "/kernel", VCPUs: 2, MemoryMB: 256, Network: &config.NetworkConfig{GuestIP: "172.16.0.2", GuestMAC: guestMACForIndex(0)}}
	addRetainedResource(t, a, svc)
	return a
}

func addRetainedResource(t *testing.T, a *Agent, svc config.ServiceConfig) {
	t.Helper()
	body, err := json.Marshal(svc)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	dir := filepath.Join(a.cfg.StateDir, "vms", svc.Name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{"schema_version": 1, "service": svc.Name, "instance_id": "retained-" + svc.Name, "lifecycle": "running", "config": svc, "config_hash": hex.EncodeToString(hash[:]), "pid": 42, "host_boot_id": "old", "process_start_ticks": 1, "executable": "/firecracker", "executable_device": 1, "executable_inode": 2, "vm_dir": dir, "socket_path": filepath.Join(dir, "firecracker.sock"), "config_path": filepath.Join(dir, "vm-config.json"), "launcher": "direct"}
	body, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instance.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.vmManager.DiscoverRecoveryState(); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkAndCapacityReserveRemovedQuarantine(t *testing.T) {
	a := retainedResourceAgent(t)
	desired := []config.ServiceConfig{{Name: "new", VCPUs: 3, MemoryMB: 128, Network: &config.NetworkConfig{}}}
	a.assignNetworking(desired)
	if desired[0].Network.GuestIP == "172.16.0.2" || desired[0].Network.GuestMAC == guestMACForIndex(0) {
		t.Fatal("reused retained VM IP/MAC")
	}
	used, held := a.reserveCapacity(desired, capacity.NodeCapacity{VCPUs: 4, MemoryMB: 1024}, true)
	if held["new"].Reason == "" || used.VCPUs != 2 {
		t.Fatalf("retained VM capacity forgotten: used=%#v held=%v", used, held)
	}
	a.reconciler.SetHeldServices(held)
	if got := a.reconciler.Plan(config.NodeConfig{Services: desired}); len(got) != 0 {
		t.Fatalf("held VM entered plan: %#v", got)
	}
}

func TestHealthRestartWaitsForReconciliationLifecycleTransaction(t *testing.T) {
	a := New(testAgentConfig(t), &fakeStore{}, testLogger())
	a.lifecycleMu.Lock()
	done := make(chan struct{})
	go func() { _ = a.restartService(context.Background(), "missing"); close(done) }()
	select {
	case <-done:
		a.lifecycleMu.Unlock()
		t.Fatal("health restart bypassed lifecycle transaction")
	case <-time.After(20 * time.Millisecond):
	}
	a.lifecycleMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("health restart did not resume")
	}
}

// Recovery resolves ownership after admission checks but before the plan. Holds
// must survive that transition, including when the old VM is cleaned entirely.
type resolvingCapacityManager struct {
	instances   map[string]*vm.Instance
	clear       bool
	resolveName string
	started     []config.ServiceConfig
}

func (m *resolvingCapacityManager) List() map[string]*vm.Instance { return m.instances }
func (m *resolvingCapacityManager) Recover(context.Context, config.NodeConfig) ([]string, error) {
	name := m.resolveName
	if name == "" {
		name = "removed"
	}
	if m.clear {
		delete(m.instances, name)
	} else {
		m.instances[name].State = vm.StateRunning
	}
	return []string{name}, nil
}
func (m *resolvingCapacityManager) Remove(name string) error { delete(m.instances, name); return nil }
func (m *resolvingCapacityManager) Start(_ context.Context, svc config.ServiceConfig) error {
	m.started = append(m.started, svc)
	m.instances[svc.Name] = &vm.Instance{Name: svc.Name, Config: svc, State: vm.StateRunning}
	return nil
}
func TestCapacityHoldsSurviveSameTickRecovery(t *testing.T) {
	for _, clear := range []bool{false, true} {
		for _, memory := range []bool{false, true} {
			a := retainedResourceAgent(t)
			old := a.vmManager.List()["removed"].Config
			desired := old
			if memory {
				desired.MemoryMB = 2048
			} else {
				desired.VCPUs = 8
			}
			limit := capacity.NodeCapacity{VCPUs: 4, MemoryMB: 1024}
			independent := config.ServiceConfig{Name: "independent", VCPUs: 1, MemoryMB: 64}
			services := []config.ServiceConfig{desired, independent}
			_, held := a.reserveCapacity(services, limit, true)
			if held[desired.Name].Reason == "" {
				t.Fatalf("clear=%v memory=%v: desired growth escaped capacity admission", clear, memory)
			}
			manager := &resolvingCapacityManager{clear: clear, instances: map[string]*vm.Instance{old.Name: {Name: old.Name, Config: old, State: vm.StateRecoveryPending}}}
			r := reconciler.New(manager, testLogger(), nil, nil, "", 0)
			r.SetHeldServices(held)
			if err := r.Reconcile(context.Background(), config.NodeConfig{Services: services}); !reconciler.IsIncomplete(err) {
				t.Fatalf("capacity hold should remain incomplete: %v", err)
			}
			if len(manager.started) != 1 || manager.started[0].Name != independent.Name {
				t.Fatalf("held work admitted or independent blocked: %#v", manager.started)
			}
		}
	}
}

func TestCapacityReservesRetainedDesiredGrowthBeforeNewServices(t *testing.T) {
	a := retainedResourceAgent(t)
	desired := a.vmManager.List()["removed"].Config
	desired.VCPUs = 4
	next := config.ServiceConfig{Name: "new", VCPUs: 1, MemoryMB: 64}
	used, held := a.reserveCapacity([]config.ServiceConfig{desired, next}, capacity.NodeCapacity{VCPUs: 4, MemoryMB: 1024}, true)
	if used.VCPUs != 4 || held["new"].Reason == "" {
		t.Fatalf("growth not reserved: used=%#v held=%v", used, held)
	}
}

func TestCapacityIncludesRemovedOwnerWhenDesiredFitsAlone(t *testing.T) {
	a := retainedResourceAgent(t)
	old := config.ServiceConfig{Name: "wanted", Image: "/image", Kernel: "/kernel", VCPUs: 2, MemoryMB: 128}
	addRetainedResource(t, a, old)
	desired := old
	desired.VCPUs = 3
	limit := capacity.NodeCapacity{VCPUs: 4, MemoryMB: 1024}
	used, held := a.reserveCapacity([]config.ServiceConfig{desired}, limit, true)
	if held[old.Name].Reason == "" || used.VCPUs != 4 {
		t.Fatalf("desired fits alone but overcommits retained owners: used=%#v holds=%v", used, held)
	}
	manager := &resolvingCapacityManager{resolveName: old.Name, instances: a.vmManager.List()}
	r := reconciler.New(manager, testLogger(), nil, nil, "", 0)
	r.SetHeldServices(held)
	if err := r.Reconcile(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{desired}}); !reconciler.IsIncomplete(err) {
		t.Fatalf("same-tick recovery lost hold: %v", err)
	}
	if len(manager.started) != 0 {
		t.Fatal("overcommitted after ownership resolved")
	}
}

type blockingImageSyncer struct {
	entered chan struct{}
	release chan struct{}
}

func (s *blockingImageSyncer) Sync(context.Context, []config.ServiceConfig) error {
	close(s.entered)
	<-s.release
	return nil
}
func (s *blockingImageSyncer) Close() error { return nil }

func TestHealthRestartDoesNotWaitForImageSync(t *testing.T) {
	s := &fakeStore{data: map[string][]byte{"web": []byte("node: web\nservices:\n- name: service\n  image: /img/service\n  kernel: /kern\n  vcpus: 1\n  memory_mb: 128\n")}, revision: "one"}
	a := New(testAgentConfig(t), s, testLogger())
	images := &blockingImageSyncer{entered: make(chan struct{}), release: make(chan struct{})}
	a.imageSyncer = images
	ticked := make(chan struct{})
	go func() { a.tick(context.Background()); close(ticked) }()
	<-images.entered
	restarted := make(chan struct{})
	go func() { _ = a.restartService(context.Background(), "missing"); close(restarted) }()
	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("health restart waited for image sync")
	}
	close(images.release)
	<-ticked
}

// A full subnet must hold the new service, never strip existing services of
// their addresses: Reconcile would recreate those healthy VMs without networking.
func TestAddressExhaustionHoldsNewServiceAndKeepsExistingAssignments(t *testing.T) {
	cfg := testAgentConfig(t)
	cfg.VMSubnet = "172.16.0.0/29"
	cfg.VMGateway = "172.16.0.1"
	a := New(cfg, &fakeStore{}, testLogger())
	_, ipNet, err := net.ParseCIDR(cfg.VMSubnet)
	if err != nil {
		t.Fatal(err)
	}
	existing := []string{"b", "c", "d", "e", "f"}
	for i, name := range existing {
		ip, ok := guestIPv4ForIndex(ipNet, i)
		if !ok {
			t.Fatalf("fixture subnet too small for %s", name)
		}
		addRetainedResource(t, a, config.ServiceConfig{Name: name, Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 64,
			Network: &config.NetworkConfig{GuestIP: ip, GuestMAC: guestMACForIndex(i)}})
	}
	if _, ok := guestIPv4ForIndex(ipNet, len(existing)); ok {
		t.Fatal("fixture subnet is not full")
	}
	// b was removed but stays quarantined; a sorts before every existing service.
	desired := []config.ServiceConfig{{Name: "a", Network: &config.NetworkConfig{}}}
	for _, name := range existing[1:] {
		desired = append(desired, config.ServiceConfig{Name: name, Network: &config.NetworkConfig{}})
	}
	held := a.assignNetworking(desired)
	if len(held) != 1 || held["a"] == "" {
		t.Fatalf("exhausted new service was not held: %v", held)
	}
	defer func() {
		// A full tick turns the address hold into a reconciler hold with a reason.
		a.store = &fakeStore{data: map[string][]byte{"web": []byte("node: web\nservices:\n- name: a\n  image: /img\n  kernel: /kern\n  vcpus: 1\n  memory_mb: 64\n  network: {}\n")}, revision: "one"}
		a.tick(context.Background())
		status := a.agentStatusSnapshot()
		for _, service := range status.Services {
			if service.Name == "a" && (service.ReasonCode != "resource_held" || !strings.Contains(service.Message, "no free guest address")) {
				t.Fatalf("exhausted service was not reported as held: %#v", service)
			}
		}
		// An exhausted subnet is not explained by pending recovery, so the
		// revision stays incomplete instead of being applied as degraded.
		if status.AppliedRevision == "one" {
			t.Fatal("address hold was applied as if recovery caused it")
		}
	}()
	if desired[0].Network.GuestIP != "" {
		t.Fatalf("held service got an address: %s", desired[0].Network.GuestIP)
	}
	for i, svc := range desired[1:] {
		want, _ := guestIPv4ForIndex(ipNet, i+1)
		if svc.Network.GuestIP != want || svc.Network.GuestMAC != guestMACForIndex(i+1) || !strings.Contains(svc.KernelArgs, "ip="+want+"::") {
			t.Fatalf("existing service %s lost its assignment: %#v kernel=%q", svc.Name, svc.Network, svc.KernelArgs)
		}
	}
}

func TestCapacityHoldIsAttributedToRecoveryOnlyWhenResolutionWouldFit(t *testing.T) {
	limit := capacity.NodeCapacity{VCPUs: 4, MemoryMB: 1024}
	for _, tc := range []struct {
		vcpus    int
		recovery bool
	}{{3, true}, {6, false}} {
		a := retainedResourceAgent(t) // a removed 2-vCPU VM is recovery_pending
		_, held := a.reserveCapacity([]config.ServiceConfig{{Name: "new", VCPUs: tc.vcpus, MemoryMB: 64}}, limit, true)
		hold, ok := held["new"]
		if !ok || hold.Recovery != tc.recovery {
			t.Fatalf("%d vCPUs: hold=%#v, want recovery=%v", tc.vcpus, hold, tc.recovery)
		}
		if !tc.recovery && hold.Reason != "desired services exceed node capacity" {
			t.Fatalf("genuine over-capacity blamed on recovery: %q", hold.Reason)
		}
	}
}
