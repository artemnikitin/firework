package reconciler

import (
	"context"
	"sync"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/vm"
)

func TestRetainedResourcesHoldConflictingWorkAndRecover(t *testing.T) {
	for _, strategy := range []string{"rolling", ""} {
		for _, resource := range []string{"ip", "mac", "port", "tap"} {
			for _, operation := range []string{"create", "update", "old-runtime-update"} {
				t.Run(strategy+"/"+resource+"/"+operation, func(t *testing.T) {
					r := newTestReconciler(strategy, 0)
					manager := r.vmManager.(*fakeVMManager)
					owner := config.ServiceConfig{Name: "retained", Network: &config.NetworkConfig{GuestIP: "10.1.1.2", GuestMAC: "02:00:00:00:00:01", Interface: "tap-retained"}, PortForwards: []config.PortForward{{HostPort: 8080, VMPort: 80}}}
					manager.instances[owner.Name] = &vm.Instance{Name: owner.Name, Config: owner, State: vm.StateRecoveryPending}
					service := config.ServiceConfig{Name: "conflict", Image: "/new", Network: &config.NetworkConfig{GuestIP: "10.1.1.3", GuestMAC: "02:00:00:00:00:02", Interface: "tap-conflict"}}
					conflicting := service
					network := *service.Network
					conflicting.Network = &network
					switch resource {
					case "ip":
						conflicting.Network.GuestIP = owner.Network.GuestIP
					case "mac":
						conflicting.Network.GuestMAC = owner.Network.GuestMAC
					case "port":
						conflicting.PortForwards = []config.PortForward{{HostPort: 8080, VMPort: 90}}
					case "tap":
						conflicting.Network.Interface = owner.Network.Interface
					}
					if operation != "create" {
						old := service
						if operation == "old-runtime-update" {
							old = conflicting
						}
						old.Image = "/old"
						manager.instances[old.Name] = &vm.Instance{Name: old.Name, Config: old, State: vm.StateRunning}
					}
					if operation != "old-runtime-update" {
						service = conflicting
					}
					// The quarantined service's desired config deliberately no longer
					// names its actual claims; fences must use the retained runtime.
					desired := config.NodeConfig{Services: []config.ServiceConfig{{Name: owner.Name}, service, {Name: "independent", Image: "/new"}}}
					for tick := 0; tick < 2; tick++ {
						if err := r.Reconcile(context.Background(), desired); !IsIncomplete(err) {
							t.Fatalf("tick %d conflict must be incomplete: %v", tick, err)
						}
						if got := manager.instances["independent"]; got == nil || got.State != vm.StateRunning {
							t.Fatalf("independent service did not converge: %#v", got)
						}
						if len(manager.removeCalls) != 0 || len(manager.startCalls) != 1 || manager.startCalls[0] != "independent" {
							t.Fatalf("conflict reached runtime mutation: starts=%v removes=%v", manager.startCalls, manager.removeCalls)
						}
					}
					delete(manager.instances, owner.Name)
					desired.Services = desired.Services[1:]
					if err := r.Reconcile(context.Background(), desired); err != nil {
						t.Fatalf("resolved conflict failed to recover: %v", err)
					}
					if got := manager.instances[service.Name]; got == nil || got.Config.Image != "/new" {
						t.Fatalf("held work did not resume: %#v", got)
					}
				})
			}
		}
	}
}

func TestRetainedSharedDeviceCannotBeDeleted(t *testing.T) {
	r, manager := newNetworkTestReconciler(&fakeNetworkManager{})
	owner := forwardedService("retained")
	owner.Network.Interface = "shared-tap"
	old := forwardedService("old")
	old.Network.Interface = owner.Network.Interface
	manager.instances[owner.Name] = &vm.Instance{Name: owner.Name, Config: owner, State: vm.StateRecoveryPending}
	manager.instances[old.Name] = &vm.Instance{Name: old.Name, Config: old, State: vm.StateRunning}
	if err := r.Reconcile(context.Background(), config.NodeConfig{}); !IsIncomplete(err) {
		t.Fatalf("unsafe removal was not held: %v", err)
	}
	if len(manager.removeCalls) != 0 {
		t.Fatal("removed a runtime whose teardown would break retained resources")
	}
}

func TestCallerCapacityHoldRetriesWithoutMutatingHeldRuntime(t *testing.T) {
	r := newTestReconciler("", 0)
	manager := r.vmManager.(*fakeVMManager)
	desired := config.NodeConfig{Services: []config.ServiceConfig{{Name: "held"}, {Name: "independent"}}}
	r.SetHeldServices(map[string]string{"held": "retained CPU capacity"})
	if err := r.Reconcile(context.Background(), desired); !IsIncomplete(err) {
		t.Fatalf("capacity hold was reported successful: %v", err)
	}
	if manager.instances["held"] != nil || manager.instances["independent"] == nil {
		t.Fatal("capacity hold blocked independent work or allowed held work")
	}
	r.SetHeldServices(nil)
	if err := r.Reconcile(context.Background(), desired); err != nil || manager.instances["held"] == nil {
		t.Fatalf("capacity recovery did not resume held work: %v", err)
	}
}

func TestLifecycleTransitionDoesNotEnterPlanOrLoseCleanupIntent(t *testing.T) {
	for _, state := range []vm.State{vm.StateStarting, vm.StateStartAborting, vm.StateStopping} {
		t.Run(string(state), func(t *testing.T) {
			network := &fakeNetworkManager{}
			r, manager := newNetworkTestReconciler(network)
			service := forwardedService("app")
			manager.instances[service.Name] = &vm.Instance{Name: service.Name, Config: service, State: state}
			if err := r.journalIntendedTeardowns(service); err != nil {
				t.Fatal(err)
			}
			if actions := r.Plan(config.NodeConfig{}); len(actions) != 0 {
				t.Fatalf("in-progress lifecycle entered deletion plan: %#v", actions)
			}
			if err := r.retryPendingTeardowns(); err != nil {
				t.Fatal(err)
			}
			if !r.HasPendingCleanup() {
				t.Fatal("retained cleanup intent no longer triggers another retry")
			}
			delete(manager.instances, service.Name)
			if err := r.retryPendingTeardowns(); err != nil {
				t.Fatal(err)
			}
			if r.HasPendingCleanup() || len(network.deleteTAPCalls) != 1 || len(network.teardownForwardCalls) != 1 {
				t.Fatal("cleanup did not resume after lifecycle completion")
			}
		})
	}
}

func TestUnknownRuntimeClaimsHoldMutationsAndCleanupUntilResolved(t *testing.T) {
	network := &fakeNetworkManager{}
	r, manager := newNetworkTestReconciler(network)
	unknown := &vm.Instance{Name: "unknown", Config: config.ServiceConfig{Name: "unknown"}, State: vm.StateRecoveryPending, RecoveryConfigUnknown: true}
	manager.instances[unknown.Name] = unknown
	if err := r.Reconcile(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{{Name: unknown.Name}}}); err != nil {
		t.Fatalf("unknown quarantine blocked its own excluded service: %v", err)
	}
	old := forwardedService("old")
	manager.instances[old.Name] = &vm.Instance{Name: old.Name, Config: old, State: vm.StateRunning}
	if err := r.journalIntendedTeardowns(forwardedService("gone")); err != nil {
		t.Fatal(err)
	}
	desired := config.NodeConfig{Services: []config.ServiceConfig{forwardedService("new")}}
	for tick := 0; tick < 2; tick++ {
		if err := r.Reconcile(context.Background(), desired); !IsIncomplete(err) {
			t.Fatalf("unknown claims must hold work: %v", err)
		}
		if len(manager.startCalls) != 0 || len(manager.removeCalls) != 0 || len(network.deleteTAPCalls) != 0 || len(network.teardownForwardCalls) != 0 || len(network.setupForwardCalls) != 0 {
			t.Fatal("unknown claims allowed runtime or host mutations")
		}
		if !r.HasPendingCleanup() {
			t.Fatal("unknown ownership discarded cleanup intent")
		}
	}
	delete(manager.instances, unknown.Name)
	if err := r.Reconcile(context.Background(), desired); err != nil {
		t.Fatalf("resolved ownership did not resume work: %v", err)
	}
	if manager.instances["new"] == nil || manager.instances[old.Name] != nil || r.HasPendingCleanup() {
		t.Fatal("resolved ownership failed to finish held work and cleanup")
	}
}

func TestCapacityHoldPublicationSupportsConcurrentTelemetry(t *testing.T) {
	r := newTestReconciler("", 0)
	desired := config.NodeConfig{Services: []config.ServiceConfig{{Name: "app"}}}
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for i := 0; i < 100; i++ {
			r.SetHeldServices(map[string]string{"app": "retained capacity"})
			r.SetHeldServices(nil)
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 100; i++ {
			_ = r.HeldServices(desired)
		}
	}()
	workers.Wait()
}

func TestFailedUpdateDoesNotProgramDesiredGuestOrPorts(t *testing.T) {
	network := &fakeNetworkManager{}
	r, manager := newNetworkTestReconciler(network)
	old := forwardedService("app")
	old.PortForwards = []config.PortForward{{HostPort: 8080, VMPort: 80}}
	manager.instances[old.Name] = &vm.Instance{Name: old.Name, Config: old, State: vm.StateRunning}
	desired := old
	desired.Network = nil
	desired.PortForwards = nil
	manager.removeErr = ErrServiceHeld
	if err := r.Reconcile(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{desired}}); err == nil {
		t.Fatal("update unexpectedly succeeded")
	}
	if len(network.setupForwardCalls) != 1 || network.setupForwardCalls[0] != 8080 {
		t.Fatalf("running config's forwarding was not preserved: %v", network.setupForwardCalls)
	}
}

func TestPendingBridgeCleanupProtectsRetainedTAPWithTheSameName(t *testing.T) {
	network := &fakeNetworkManager{}
	r, manager := newNetworkTestReconciler(network)
	old := config.ServiceConfig{Name: "old", Network: &config.NetworkConfig{HostDevName: "eth0"}}
	if err := r.journalIntendedTeardowns(old); err != nil {
		t.Fatal(err)
	}
	owner := config.ServiceConfig{Name: "retained", Network: &config.NetworkConfig{Interface: "br-old"}}
	manager.instances[owner.Name] = &vm.Instance{Name: owner.Name, Config: owner, State: vm.StateRecoveryPending}
	if err := r.retryPendingNetworkDevices(); err != nil {
		t.Fatal(err)
	}
	if len(network.deleteBridgeCalls) != 0 || !r.HasPendingCleanup() {
		t.Fatal("bridge cleanup removed or forgot a name now retained by a TAP")
	}
	delete(manager.instances, owner.Name)
	if err := r.retryPendingNetworkDevices(); err != nil {
		t.Fatal(err)
	}
	if len(network.deleteBridgeCalls) != 1 || r.HasPendingCleanup() {
		t.Fatal("bridge cleanup did not resume after retained TAP resolved")
	}
}
