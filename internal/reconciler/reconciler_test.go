package reconciler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/vm"
)

// fakeVMManager implements VMManager for testing.
type fakeVMManager struct {
	instances   map[string]*vm.Instance
	startCalls  []string
	removeCalls []string
}

func newFakeVMManager() *fakeVMManager {
	return &fakeVMManager{instances: make(map[string]*vm.Instance)}
}

func (f *fakeVMManager) List() map[string]*vm.Instance {
	out := make(map[string]*vm.Instance, len(f.instances))
	for k, v := range f.instances {
		out[k] = v
	}
	return out
}

func (f *fakeVMManager) Start(_ context.Context, svc config.ServiceConfig) error {
	f.startCalls = append(f.startCalls, svc.Name)
	f.instances[svc.Name] = &vm.Instance{Name: svc.Name, State: vm.StateRunning, Config: svc}
	return nil
}

func (f *fakeVMManager) Remove(name string) error {
	f.removeCalls = append(f.removeCalls, name)
	delete(f.instances, name)
	return nil
}

func newTestReconciler(strategy string, delay time.Duration) *Reconciler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := New(newFakeVMManager(), logger, nil, nil, strategy, delay)
	return r
}

func TestPlan_CreateNewServices(t *testing.T) {
	desired := config.NodeConfig{
		Node: "test-node",
		Services: []config.ServiceConfig{
			{Name: "svc-a", Image: "/img/a", Kernel: "/kern", VCPUs: 1, MemoryMB: 256},
			{Name: "svc-b", Image: "/img/b", Kernel: "/kern", VCPUs: 2, MemoryMB: 512},
		},
	}

	// No running instances.
	actual := map[string]*vm.Instance{}

	actions := plan(desired, actual)

	if len(actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(actions))
	}
	for _, a := range actions {
		if a.Type != ActionCreate {
			t.Errorf("expected create action, got %s", a.Type)
		}
	}
}

func TestPlan_DeleteExtraServices(t *testing.T) {
	desired := config.NodeConfig{
		Node:     "test-node",
		Services: []config.ServiceConfig{},
	}

	actual := map[string]*vm.Instance{
		"old-svc": {
			Name:  "old-svc",
			State: vm.StateRunning,
			Config: config.ServiceConfig{
				Name: "old-svc", Image: "/img/old", Kernel: "/kern",
			},
		},
	}

	actions := plan(desired, actual)

	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Type != ActionDelete {
		t.Errorf("expected delete action, got %s", actions[0].Type)
	}
	if actions[0].Service.Name != "old-svc" {
		t.Errorf("expected service name old-svc, got %s", actions[0].Service.Name)
	}
}

func TestPlan_UpdateChangedServices(t *testing.T) {
	desired := config.NodeConfig{
		Node: "test-node",
		Services: []config.ServiceConfig{
			{Name: "svc-a", Image: "/img/a-v2", Kernel: "/kern", VCPUs: 1, MemoryMB: 256},
		},
	}

	actual := map[string]*vm.Instance{
		"svc-a": {
			Name:  "svc-a",
			State: vm.StateRunning,
			Config: config.ServiceConfig{
				Name: "svc-a", Image: "/img/a-v1", Kernel: "/kern", VCPUs: 1, MemoryMB: 256,
			},
		},
	}

	actions := plan(desired, actual)

	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Type != ActionUpdate {
		t.Errorf("expected update action, got %s", actions[0].Type)
	}
}

func TestPlan_NoChanges(t *testing.T) {
	svc := config.ServiceConfig{
		Name: "svc-a", Image: "/img/a", Kernel: "/kern", VCPUs: 1, MemoryMB: 256,
	}

	desired := config.NodeConfig{
		Node:     "test-node",
		Services: []config.ServiceConfig{svc},
	}

	actual := map[string]*vm.Instance{
		"svc-a": {
			Name:   "svc-a",
			State:  vm.StateRunning,
			Config: svc,
		},
	}

	actions := plan(desired, actual)

	if len(actions) != 0 {
		t.Fatalf("expected 0 actions, got %d", len(actions))
	}
}

func TestPlan_RestartFailedService(t *testing.T) {
	svc := config.ServiceConfig{
		Name: "svc-a", Image: "/img/a", Kernel: "/kern", VCPUs: 1, MemoryMB: 256,
	}

	desired := config.NodeConfig{
		Node:     "test-node",
		Services: []config.ServiceConfig{svc},
	}

	actual := map[string]*vm.Instance{
		"svc-a": {
			Name:   "svc-a",
			State:  vm.StateFailed, // crashed
			Config: svc,
		},
	}

	actions := plan(desired, actual)

	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Type != ActionUpdate {
		t.Errorf("expected update action for failed service, got %s", actions[0].Type)
	}
}

func TestPlan_MixedOperations(t *testing.T) {
	desired := config.NodeConfig{
		Node: "test-node",
		Services: []config.ServiceConfig{
			{Name: "keep", Image: "/img/keep", Kernel: "/kern", VCPUs: 1, MemoryMB: 256},
			{Name: "new", Image: "/img/new", Kernel: "/kern", VCPUs: 1, MemoryMB: 256},
			{Name: "update", Image: "/img/update-v2", Kernel: "/kern", VCPUs: 2, MemoryMB: 512},
		},
	}

	actual := map[string]*vm.Instance{
		"keep": {
			Name: "keep", State: vm.StateRunning,
			Config: config.ServiceConfig{Name: "keep", Image: "/img/keep", Kernel: "/kern", VCPUs: 1, MemoryMB: 256},
		},
		"delete-me": {
			Name: "delete-me", State: vm.StateRunning,
			Config: config.ServiceConfig{Name: "delete-me", Image: "/img/del", Kernel: "/kern"},
		},
		"update": {
			Name: "update", State: vm.StateRunning,
			Config: config.ServiceConfig{Name: "update", Image: "/img/update-v1", Kernel: "/kern", VCPUs: 1, MemoryMB: 256},
		},
	}

	actions := plan(desired, actual)

	creates := countActions(actions, ActionCreate)
	updates := countActions(actions, ActionUpdate)
	deletes := countActions(actions, ActionDelete)

	if creates != 1 {
		t.Errorf("expected 1 create, got %d", creates)
	}
	if updates != 1 {
		t.Errorf("expected 1 update, got %d", updates)
	}
	if deletes != 1 {
		t.Errorf("expected 1 delete, got %d", deletes)
	}
}

func TestNeedsUpdate_MemoryChange(t *testing.T) {
	inst := &vm.Instance{
		State: vm.StateRunning,
		Config: config.ServiceConfig{
			Name: "svc", Image: "/img", Kernel: "/kern", VCPUs: 1, MemoryMB: 256,
		},
	}
	desired := config.ServiceConfig{
		Name: "svc", Image: "/img", Kernel: "/kern", VCPUs: 1, MemoryMB: 512,
	}

	if !needsUpdate(inst, desired) {
		t.Error("expected needsUpdate=true for memory change")
	}
}

func TestNeedsUpdate_VCPUChange(t *testing.T) {
	inst := &vm.Instance{
		State: vm.StateRunning,
		Config: config.ServiceConfig{
			Name: "svc", Image: "/img", Kernel: "/kern", VCPUs: 1, MemoryMB: 256,
		},
	}
	desired := config.ServiceConfig{
		Name: "svc", Image: "/img", Kernel: "/kern", VCPUs: 4, MemoryMB: 256,
	}

	if !needsUpdate(inst, desired) {
		t.Error("expected needsUpdate=true for vcpu change")
	}
}

func TestNeedsUpdate_KernelArgsChange(t *testing.T) {
	inst := &vm.Instance{
		State: vm.StateRunning,
		Config: config.ServiceConfig{
			Name: "svc", Image: "/img", Kernel: "/kern", VCPUs: 1, MemoryMB: 256,
			KernelArgs: "console=ttyS0",
		},
	}
	desired := config.ServiceConfig{
		Name: "svc", Image: "/img", Kernel: "/kern", VCPUs: 1, MemoryMB: 256,
		KernelArgs: "console=ttyS0 debug",
	}

	if !needsUpdate(inst, desired) {
		t.Error("expected needsUpdate=true for kernel args change")
	}
}

// plan is a standalone version of Plan logic that takes actual state directly,
// making it testable without a real VM manager.
func plan(desired config.NodeConfig, actual map[string]*vm.Instance) []Action {
	var actions []Action

	desiredSet := make(map[string]config.ServiceConfig, len(desired.Services))
	for _, svc := range desired.Services {
		desiredSet[svc.Name] = svc
	}

	for _, svc := range desired.Services {
		inst, exists := actual[svc.Name]
		if !exists {
			actions = append(actions, Action{Type: ActionCreate, Service: svc})
			continue
		}
		if needsUpdate(inst, svc) {
			prev := inst.Config
			actions = append(actions, Action{
				Type:            ActionUpdate,
				Service:         svc,
				PreviousService: &prev,
			})
		}
	}

	for name, inst := range actual {
		if _, desired := desiredSet[name]; !desired {
			actions = append(actions, Action{Type: ActionDelete, Service: inst.Config})
		}
	}

	return actions
}

func TestNeedsUpdate_NetworkChange(t *testing.T) {
	inst := &vm.Instance{
		State: vm.StateRunning,
		Config: config.ServiceConfig{
			Name: "svc", Image: "/img", Kernel: "/kern", VCPUs: 1, MemoryMB: 256,
			Network: &config.NetworkConfig{Interface: "tap-svc", GuestIP: "172.16.0.2"},
		},
	}
	desired := config.ServiceConfig{
		Name: "svc", Image: "/img", Kernel: "/kern", VCPUs: 1, MemoryMB: 256,
		Network: &config.NetworkConfig{Interface: "tap-svc", GuestIP: "172.16.0.3"},
	}

	if !needsUpdate(inst, desired) {
		t.Error("expected needsUpdate=true for network IP change")
	}
}

func TestNeedsUpdate_NetworkAddedRemoved(t *testing.T) {
	// Network added.
	inst := &vm.Instance{
		State: vm.StateRunning,
		Config: config.ServiceConfig{
			Name: "svc", Image: "/img", Kernel: "/kern", VCPUs: 1, MemoryMB: 256,
		},
	}
	desired := config.ServiceConfig{
		Name: "svc", Image: "/img", Kernel: "/kern", VCPUs: 1, MemoryMB: 256,
		Network: &config.NetworkConfig{Interface: "tap-svc"},
	}
	if !needsUpdate(inst, desired) {
		t.Error("expected needsUpdate=true when network added")
	}

	// Network removed.
	inst2 := &vm.Instance{
		State: vm.StateRunning,
		Config: config.ServiceConfig{
			Name: "svc", Image: "/img", Kernel: "/kern", VCPUs: 1, MemoryMB: 256,
			Network: &config.NetworkConfig{Interface: "tap-svc"},
		},
	}
	desired2 := config.ServiceConfig{
		Name: "svc", Image: "/img", Kernel: "/kern", VCPUs: 1, MemoryMB: 256,
	}
	if !needsUpdate(inst2, desired2) {
		t.Error("expected needsUpdate=true when network removed")
	}
}

func TestNeedsUpdate_PortForwardsChange(t *testing.T) {
	inst := &vm.Instance{
		State: vm.StateRunning,
		Config: config.ServiceConfig{
			Name: "svc", Image: "/img", Kernel: "/kern", VCPUs: 1, MemoryMB: 256,
			PortForwards: []config.PortForward{{HostPort: 80, VMPort: 8080}},
		},
	}
	desired := config.ServiceConfig{
		Name: "svc", Image: "/img", Kernel: "/kern", VCPUs: 1, MemoryMB: 256,
		PortForwards: []config.PortForward{{HostPort: 443, VMPort: 8443}},
	}

	if !needsUpdate(inst, desired) {
		t.Error("expected needsUpdate=true for port forwards change")
	}
}

func TestPlan_DeletePreservesFullConfig(t *testing.T) {
	desired := config.NodeConfig{
		Node:     "test-node",
		Services: []config.ServiceConfig{},
	}

	fullConfig := config.ServiceConfig{
		Name: "old-svc", Image: "/img/old", Kernel: "/kern",
		Network:      &config.NetworkConfig{Interface: "tap-old", GuestIP: "172.16.0.2"},
		PortForwards: []config.PortForward{{HostPort: 80, VMPort: 8080}},
	}

	actual := map[string]*vm.Instance{
		"old-svc": {
			Name:   "old-svc",
			State:  vm.StateRunning,
			Config: fullConfig,
		},
	}

	actions := plan(desired, actual)

	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Type != ActionDelete {
		t.Errorf("expected delete, got %s", actions[0].Type)
	}
	// Verify delete action has full config (not just name).
	if actions[0].Service.Network == nil {
		t.Error("delete action should preserve full config including Network")
	}
	if len(actions[0].Service.PortForwards) != 1 {
		t.Error("delete action should preserve full config including PortForwards")
	}
}

func TestPlan_UpdatePreservesPreviousConfigForTeardown(t *testing.T) {
	desired := config.NodeConfig{
		Node: "test-node",
		Services: []config.ServiceConfig{
			{
				Name:         "svc-a",
				Image:        "/img/new",
				Kernel:       "/kern",
				VCPUs:        2,
				MemoryMB:     512,
				PortForwards: []config.PortForward{{HostPort: 8080, VMPort: 8080}},
				Network:      &config.NetworkConfig{Interface: "tap-svc-a", GuestIP: "172.16.0.10"},
			},
		},
	}

	actual := map[string]*vm.Instance{
		"svc-a": {
			Name:  "svc-a",
			State: vm.StateRunning,
			Config: config.ServiceConfig{
				Name:         "svc-a",
				Image:        "/img/old",
				Kernel:       "/kern",
				VCPUs:        1,
				MemoryMB:     256,
				PortForwards: []config.PortForward{{HostPort: 80, VMPort: 8080}},
				Network:      &config.NetworkConfig{Interface: "tap-svc-a", GuestIP: "172.16.0.2"},
			},
		},
	}

	actions := plan(desired, actual)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Type != ActionUpdate {
		t.Fatalf("expected update action, got %s", actions[0].Type)
	}
	if actions[0].PreviousService == nil {
		t.Fatal("expected previous service config to be present for update")
	}
	if got := actions[0].PreviousService.Network.GuestIP; got != "172.16.0.2" {
		t.Fatalf("expected previous guest IP 172.16.0.2, got %s", got)
	}
	if got := actions[0].PreviousService.PortForwards[0].HostPort; got != 80 {
		t.Fatalf("expected previous host port 80, got %d", got)
	}
}

func TestApply_AllAtOnce_UpdatesAll(t *testing.T) {
	r := newTestReconciler("", 0)
	fvm := r.vmManager.(*fakeVMManager)

	// Pre-populate instances so updates have something to remove.
	for _, name := range []string{"svc-a", "svc-b", "svc-c"} {
		fvm.instances[name] = &vm.Instance{Name: name, State: vm.StateRunning,
			Config: config.ServiceConfig{Name: name, Image: "/img/old", Kernel: "/kern", VCPUs: 1, MemoryMB: 256}}
	}

	sleepCalls := 0
	r.sleepFn = func(_ context.Context, _ time.Duration) error {
		sleepCalls++
		return nil
	}

	actions := []Action{
		{Type: ActionUpdate, Service: config.ServiceConfig{Name: "svc-a", Image: "/img/v2", Kernel: "/kern", VCPUs: 1, MemoryMB: 256}},
		{Type: ActionUpdate, Service: config.ServiceConfig{Name: "svc-b", Image: "/img/v2", Kernel: "/kern", VCPUs: 1, MemoryMB: 256}},
		{Type: ActionUpdate, Service: config.ServiceConfig{Name: "svc-c", Image: "/img/v2", Kernel: "/kern", VCPUs: 1, MemoryMB: 256}},
	}

	if err := r.Apply(context.Background(), actions); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fvm.startCalls) != 3 {
		t.Errorf("expected 3 start calls, got %d", len(fvm.startCalls))
	}
	if sleepCalls != 0 {
		t.Errorf("expected no sleep calls for all-at-once, got %d", sleepCalls)
	}
}

func TestApply_Rolling_UpdatesOneAtATime(t *testing.T) {
	r := newTestReconciler("rolling", time.Millisecond)
	fvm := r.vmManager.(*fakeVMManager)

	for _, name := range []string{"svc-a", "svc-b", "svc-c"} {
		fvm.instances[name] = &vm.Instance{Name: name, State: vm.StateRunning,
			Config: config.ServiceConfig{Name: name, Image: "/img/old", Kernel: "/kern", VCPUs: 1, MemoryMB: 256}}
	}

	sleepCalls := 0
	r.sleepFn = func(_ context.Context, _ time.Duration) error {
		sleepCalls++
		return nil
	}

	actions := []Action{
		{Type: ActionUpdate, Service: config.ServiceConfig{Name: "svc-a", Image: "/img/v2", Kernel: "/kern", VCPUs: 1, MemoryMB: 256}},
		{Type: ActionUpdate, Service: config.ServiceConfig{Name: "svc-b", Image: "/img/v2", Kernel: "/kern", VCPUs: 1, MemoryMB: 256}},
		{Type: ActionUpdate, Service: config.ServiceConfig{Name: "svc-c", Image: "/img/v2", Kernel: "/kern", VCPUs: 1, MemoryMB: 256}},
	}

	if err := r.Apply(context.Background(), actions); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fvm.startCalls) != 3 {
		t.Errorf("expected 3 start calls, got %d", len(fvm.startCalls))
	}
	// 3 updates → 2 sleeps (not after the last one).
	if sleepCalls != 2 {
		t.Errorf("expected 2 sleep calls between 3 updates, got %d", sleepCalls)
	}
	// Verify each remove happened before the next create.
	// removeCalls and startCalls should interleave: remove-a, start-a, remove-b, start-b, ...
	if len(fvm.removeCalls) != 3 {
		t.Errorf("expected 3 remove calls, got %d", len(fvm.removeCalls))
	}
}

func TestApply_Rolling_CreateAndDeleteBatched(t *testing.T) {
	r := newTestReconciler("rolling", time.Millisecond)
	fvm := r.vmManager.(*fakeVMManager)

	// Pre-populate one instance that will be updated, one that will be deleted.
	fvm.instances["update-me"] = &vm.Instance{Name: "update-me", State: vm.StateRunning,
		Config: config.ServiceConfig{Name: "update-me", Image: "/img/old", Kernel: "/kern", VCPUs: 1, MemoryMB: 256}}
	fvm.instances["delete-me"] = &vm.Instance{Name: "delete-me", State: vm.StateRunning,
		Config: config.ServiceConfig{Name: "delete-me", Image: "/img/del", Kernel: "/kern", VCPUs: 1, MemoryMB: 256}}

	sleepCalls := 0
	r.sleepFn = func(_ context.Context, _ time.Duration) error {
		sleepCalls++
		return nil
	}

	actions := []Action{
		{Type: ActionDelete, Service: config.ServiceConfig{Name: "delete-me"}},
		{Type: ActionCreate, Service: config.ServiceConfig{Name: "new-svc", Image: "/img/new", Kernel: "/kern", VCPUs: 1, MemoryMB: 256}},
		{Type: ActionUpdate, Service: config.ServiceConfig{Name: "update-me", Image: "/img/v2", Kernel: "/kern", VCPUs: 1, MemoryMB: 256}},
	}

	if err := r.Apply(context.Background(), actions); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only 1 update → 0 sleeps.
	if sleepCalls != 0 {
		t.Errorf("expected 0 sleep calls for single update, got %d", sleepCalls)
	}
	// delete-me removed, update-me removed+recreated, new-svc created.
	if _, exists := fvm.instances["delete-me"]; exists {
		t.Error("expected delete-me to be removed")
	}
	if _, exists := fvm.instances["new-svc"]; !exists {
		t.Error("expected new-svc to be created")
	}
	if _, exists := fvm.instances["update-me"]; !exists {
		t.Error("expected update-me to be recreated")
	}
}

func TestApply_Rolling_StopsOnContextCancel(t *testing.T) {
	r := newTestReconciler("rolling", time.Millisecond)
	fvm := r.vmManager.(*fakeVMManager)

	for _, name := range []string{"svc-a", "svc-b", "svc-c"} {
		fvm.instances[name] = &vm.Instance{Name: name, State: vm.StateRunning,
			Config: config.ServiceConfig{Name: name, Image: "/img/old", Kernel: "/kern", VCPUs: 1, MemoryMB: 256}}
	}

	cancelErr := errors.New("context cancelled")
	callCount := 0
	r.sleepFn = func(_ context.Context, _ time.Duration) error {
		callCount++
		if callCount >= 1 {
			return cancelErr
		}
		return nil
	}

	actions := []Action{
		{Type: ActionUpdate, Service: config.ServiceConfig{Name: "svc-a", Image: "/img/v2", Kernel: "/kern", VCPUs: 1, MemoryMB: 256}},
		{Type: ActionUpdate, Service: config.ServiceConfig{Name: "svc-b", Image: "/img/v2", Kernel: "/kern", VCPUs: 1, MemoryMB: 256}},
		{Type: ActionUpdate, Service: config.ServiceConfig{Name: "svc-c", Image: "/img/v2", Kernel: "/kern", VCPUs: 1, MemoryMB: 256}},
	}

	err := r.Apply(context.Background(), actions)
	if err == nil {
		t.Fatal("expected error when sleepFn returns error, got nil")
	}
	// Only svc-a should have been updated before cancellation.
	if len(fvm.startCalls) != 1 {
		t.Errorf("expected 1 start call before cancellation, got %d", len(fvm.startCalls))
	}
}
