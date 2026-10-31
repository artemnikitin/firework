package reconciler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/vm"
)

type teardownVMManager struct {
	*fakeVMManager
	record func(config.ServiceConfig) error
}

func (m *teardownVMManager) SetRecoveryCleanupRecorder(record func(config.ServiceConfig) error) {
	m.record = record
}

// Model the recovery manager's proved-dead directory removal, after the
// recorder has durably accepted the old host-resource configuration.
func (m *teardownVMManager) clearRecovery(service config.ServiceConfig) error {
	if err := m.record(service); err != nil {
		return err
	}
	delete(m.instances, service.Name)
	return nil
}

func newRecoveryTeardownReconciler(t *testing.T, manager *teardownVMManager, network *fakeNetworkManager, stateDir string) *Reconciler {
	t.Helper()
	return NewWithNetworkManager(manager, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, network, "", 0).WithStateDir(stateDir)
}

func TestRecoveryTeardownCleansOldConfigAndProtectsReplacementClaims(t *testing.T) {
	old := config.ServiceConfig{
		Name: "app", Network: &config.NetworkConfig{Interface: "tap-old", HostDevName: "eth0", GuestIP: "172.16.0.2"},
		PortForwards: []config.PortForward{{HostPort: 8080, VMPort: 80}, {HostPort: 9090, VMPort: 90}},
	}
	replacement := old
	replacement.Network = &config.NetworkConfig{Interface: "tap-new", HostDevName: "eth0", GuestIP: "172.16.0.2"}
	replacement.PortForwards = []config.PortForward{{HostPort: 9090, VMPort: 90}, {HostPort: 8081, VMPort: 80}}
	manager := &teardownVMManager{fakeVMManager: newFakeVMManager()}
	network := &fakeNetworkManager{}
	r := newRecoveryTeardownReconciler(t, manager, network, t.TempDir())
	if err := manager.clearRecovery(old); err != nil {
		t.Fatal(err)
	}
	if err := r.Reconcile(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{replacement}}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(network.teardownForwardCalls, []int{8080}) || !reflect.DeepEqual(network.deleteTAPCalls, []string{"tap-old"}) || len(network.deleteBridgeCalls) != 0 {
		t.Fatalf("cleanup removed replacement claims or missed obsolete resources: ports=%v taps=%v bridges=%v",
			network.teardownForwardCalls, network.deleteTAPCalls, network.deleteBridgeCalls)
	}
	if got := manager.instances[old.Name]; got == nil || got.State != vm.StateRunning || got.Config.Network.Interface != "tap-new" {
		t.Fatalf("replacement did not converge: %#v", got)
	}
}

func TestRecoveryTeardownJournalFailurePreservesRuntimeAndRetriesAfterRestart(t *testing.T) {
	old := forwardedService("removed")
	old.Network.Interface = "tap-old"
	stateDir := filepath.Join(t.TempDir(), "journal")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	manager := &teardownVMManager{fakeVMManager: newFakeVMManager()}
	manager.instances[old.Name] = &vm.Instance{Name: old.Name, Config: old, State: vm.StateRecoveryPending}
	network := &fakeNetworkManager{teardownForwardErr: errors.New("iptables unavailable"), teardownErr: errors.New("device busy")}
	r := newRecoveryTeardownReconciler(t, manager, network, stateDir)
	if err := os.Remove(stateDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateDir, []byte("blocks journal writes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := manager.clearRecovery(old); err == nil {
		t.Fatal("runtime removed without durable cleanup journal")
	}
	if manager.instances[old.Name] == nil || len(r.pendingPortForwards) != 0 || len(r.pendingNetworkDevices) != 0 {
		t.Fatal("journal failure removed runtime or changed pending cleanup")
	}
	if err := os.Remove(stateDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := manager.clearRecovery(old); err != nil {
		t.Fatal(err)
	}
	if err := r.Reconcile(context.Background(), config.NodeConfig{}); err == nil {
		t.Fatal("physical cleanup failure was hidden")
	}
	network.teardownForwardErr, network.teardownErr = nil, nil
	restarted := newRecoveryTeardownReconciler(t, manager, network, stateDir)
	if err := restarted.Reconcile(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if len(network.teardownForwardCalls) != 2 || len(network.deleteTAPCalls) != 2 || len(restarted.pendingPortForwards) != 0 || len(restarted.pendingNetworkDevices) != 0 {
		t.Fatalf("restart did not retry obsolete resources: ports=%v taps=%v", network.teardownForwardCalls, network.deleteTAPCalls)
	}
}

func TestRecoveryTeardownWithoutHostNetworkingDoesNotBlockConvergence(t *testing.T) {
	old := forwardedService("app")
	manager := &teardownVMManager{fakeVMManager: newFakeVMManager()}
	r := New(manager, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, "", 0).WithStateDir(t.TempDir())
	if err := manager.clearRecovery(old); err != nil {
		t.Fatal(err)
	}
	if err := r.Reconcile(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryTeardownRequiresDurableJournalWhenNetworkingEnabled(t *testing.T) {
	manager := &teardownVMManager{fakeVMManager: newFakeVMManager()}
	NewWithNetworkManager(manager, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, &fakeNetworkManager{}, "", 0)
	if err := manager.clearRecovery(forwardedService("app")); err == nil {
		t.Fatal("recovery deletion accepted an in-memory-only cleanup journal")
	}
}

func TestRecoveryCleanupRecorderSerializesWithTickCleanup(t *testing.T) {
	manager := &teardownVMManager{fakeVMManager: newFakeVMManager()}
	r := newRecoveryTeardownReconciler(t, manager, &fakeNetworkManager{}, t.TempDir())
	var group sync.WaitGroup
	for i := 0; i < 4; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for attempt := 0; attempt < 20; attempt++ {
				if err := manager.record(forwardedService("old")); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		if err := r.Reconcile(context.Background(), config.NodeConfig{}); err != nil {
			t.Fatal(err)
		}
	}
	group.Wait()
	if err := r.Reconcile(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryCleanupIntentSurvivesPartialRemovalAndRestart(t *testing.T) {
	old := forwardedService("app")
	manager := &teardownVMManager{fakeVMManager: newFakeVMManager()}
	stateDir := t.TempDir()
	network := &fakeNetworkManager{}
	r := newRecoveryTeardownReconciler(t, manager, network, stateDir)
	if err := manager.record(old); err != nil {
		t.Fatal(err)
	}
	// Removal failed after journalling. Its still-quarantined identity claims
	// the devices but must not erase the only durable cleanup record.
	manager.instances[old.Name] = &vm.Instance{Name: old.Name, Config: old, State: vm.StateRecoveryPending}
	if err := r.Reconcile(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if len(r.pendingPortForwards) == 0 || len(r.pendingNetworkDevices) == 0 {
		t.Fatal("quarantine discarded cleanup journal")
	}
	if len(network.deleteTAPCalls) != 0 || len(network.teardownForwardCalls) != 0 {
		t.Fatal("uncertain owner lost its host resources")
	}
	// A restart may recover a partially removed directory without the original
	// manifest. Once absence is proved, only this journal knows its old rules.
	delete(manager.instances, old.Name)
	restarted := newRecoveryTeardownReconciler(t, manager, network, stateDir)
	if err := restarted.Reconcile(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if len(network.deleteTAPCalls) != 1 || len(network.teardownForwardCalls) != 1 {
		t.Fatal("restart lost old cleanup configuration")
	}
}

func TestQuarantinedDesiredNetworkChangesAreNotProgrammed(t *testing.T) {
	old := forwardedService("app")
	manager := &teardownVMManager{fakeVMManager: newFakeVMManager()}
	manager.instances[old.Name] = &vm.Instance{Name: old.Name, Config: old, State: vm.StateRecoveryPending}
	network := &fakeNetworkManager{setupForwardErr: errors.New("desired rule conflicts with old VM")}
	r := newRecoveryTeardownReconciler(t, manager, network, t.TempDir())
	desired := old
	desired.PortForwards = []config.PortForward{{HostPort: 9090, VMPort: 90}}
	if err := r.Reconcile(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{desired}}); err != nil {
		t.Fatal(err)
	}
	if len(network.setupForwardCalls) != 0 {
		t.Fatal("programmed desired network rules for an unresolved VM")
	}
}
