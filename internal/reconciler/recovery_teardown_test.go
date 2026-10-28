package reconciler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/vm"
)

type teardownVMManager struct {
	*fakeVMManager
	events      map[string]config.ServiceConfig
	ackCalls    int
	ackErr      error
	journalPath string
}

func (m *teardownVMManager) PendingRecoveryTeardowns() (map[string]config.ServiceConfig, error) {
	result := make(map[string]config.ServiceConfig, len(m.events))
	for id, service := range m.events {
		result[id] = service
	}
	return result, nil
}

func (m *teardownVMManager) AcknowledgeRecoveryTeardown(id string) error {
	m.ackCalls++
	if m.ackErr != nil {
		return m.ackErr
	}
	if m.journalPath != "" {
		if _, err := os.ReadFile(m.journalPath); err != nil {
			return err
		}
	}
	delete(m.events, id)
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
	stateDir := t.TempDir()
	manager := &teardownVMManager{
		fakeVMManager: newFakeVMManager(), events: map[string]config.ServiceConfig{"old": old},
		journalPath: filepath.Join(stateDir, pendingTeardownsFile),
	}
	network := &fakeNetworkManager{}
	r := newRecoveryTeardownReconciler(t, manager, network, stateDir)
	if err := r.Reconcile(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{replacement}}); err != nil {
		t.Fatal(err)
	}
	if manager.ackCalls != 1 || len(manager.events) != 0 {
		t.Fatal("old cleanup was not durably handed off before acknowledgement")
	}
	if !reflect.DeepEqual(network.teardownForwardCalls, []int{8080}) || !reflect.DeepEqual(network.deleteTAPCalls, []string{"tap-old"}) || len(network.deleteBridgeCalls) != 0 {
		t.Fatalf("cleanup removed replacement claims or missed obsolete resources: ports=%v taps=%v bridges=%v",
			network.teardownForwardCalls, network.deleteTAPCalls, network.deleteBridgeCalls)
	}
	if got := manager.instances[old.Name]; got == nil || got.State != vm.StateRunning || got.Config.Network.Interface != "tap-new" {
		t.Fatalf("replacement did not converge: %#v", got)
	}
}

func TestRecoveryTeardownPreservesOutboxOnJournalFailureThenRetriesAfterRestart(t *testing.T) {
	old := forwardedService("removed")
	old.Network.Interface = "tap-old"
	stateDir := filepath.Join(t.TempDir(), "journal")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	manager := &teardownVMManager{fakeVMManager: newFakeVMManager(), events: map[string]config.ServiceConfig{"old": old}}
	network := &fakeNetworkManager{teardownForwardErr: errors.New("iptables unavailable"), teardownErr: errors.New("device busy")}
	r := newRecoveryTeardownReconciler(t, manager, network, stateDir)
	if err := os.Remove(stateDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateDir, []byte("blocks journal writes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.Reconcile(context.Background(), config.NodeConfig{}); err == nil || !HasFailureStage(err, FailureStageNetwork) {
		t.Fatalf("journal failure was hidden: %v", err)
	}
	if manager.ackCalls != 0 || len(manager.events) != 1 || len(network.teardownForwardCalls) != 0 || len(network.deleteTAPCalls) != 0 {
		t.Fatal("cleanup intent was acknowledged or used before journal persistence")
	}
	if err := os.Remove(stateDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := r.Reconcile(context.Background(), config.NodeConfig{}); err == nil {
		t.Fatal("physical cleanup failure was hidden")
	}
	if manager.ackCalls != 1 || len(manager.events) != 0 {
		t.Fatal("successful journal transfer did not acknowledge outbox")
	}
	// The outbox is gone; only the durable journal can retry on restart.
	network.teardownForwardErr, network.teardownErr = nil, nil
	restarted := newRecoveryTeardownReconciler(t, manager, network, stateDir)
	if err := restarted.Reconcile(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if len(network.teardownForwardCalls) != 2 || len(network.deleteTAPCalls) != 2 || len(restarted.pendingPortForwards) != 0 || len(restarted.pendingNetworkDevices) != 0 {
		t.Fatalf("restart did not retry obsolete resources: ports=%v taps=%v", network.teardownForwardCalls, network.deleteTAPCalls)
	}
}

func TestRecoveryTeardownWaitsForCompletedClearance(t *testing.T) {
	old := forwardedService("app")
	manager := &teardownVMManager{fakeVMManager: newFakeVMManager(), events: map[string]config.ServiceConfig{"old": old}}
	manager.instances[old.Name] = &vm.Instance{Name: old.Name, Config: old, State: vm.StateRecoveryPending}
	network := &fakeNetworkManager{}
	r := newRecoveryTeardownReconciler(t, manager, network, t.TempDir())
	if err := r.acceptRecoveryTeardowns(); err == nil || !HasFailureStage(err, FailureStageVM) {
		t.Fatalf("partial runtime deletion was reported complete: %v", err)
	}
	if manager.ackCalls != 0 || len(manager.events) != 1 {
		t.Fatal("partially removed runtime lost its cleanup intent")
	}
	delete(manager.instances, old.Name)
	if err := r.Reconcile(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if len(network.teardownForwardCalls) != 1 || manager.ackCalls != 1 {
		t.Fatal("completed clearance did not trigger old network cleanup")
	}
}

func TestRecoveryTeardownRetainsIntentWhenHostNetworkingIsDisabled(t *testing.T) {
	old := forwardedService("app")
	manager := &teardownVMManager{fakeVMManager: newFakeVMManager(), events: map[string]config.ServiceConfig{"old": old}}
	r := New(manager, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, "", 0).WithStateDir(t.TempDir())
	if err := r.Reconcile(context.Background(), config.NodeConfig{}); err == nil || !HasFailureStage(err, FailureStageNetwork) {
		t.Fatalf("unattempted host cleanup was reported complete: %v", err)
	}
	if manager.ackCalls != 0 || len(manager.events) != 1 {
		t.Fatal("disabled networking discarded cleanup intent")
	}
}

func TestRecoveryTeardownRetriesFailedAcknowledgement(t *testing.T) {
	old := forwardedService("removed")
	manager := &teardownVMManager{
		fakeVMManager: newFakeVMManager(), events: map[string]config.ServiceConfig{"old": old},
		ackErr: errors.New("outbox acknowledgement unavailable"),
	}
	network := &fakeNetworkManager{}
	r := newRecoveryTeardownReconciler(t, manager, network, t.TempDir())
	if err := r.Reconcile(context.Background(), config.NodeConfig{}); err == nil {
		t.Fatal("acknowledgement failure was hidden")
	}
	if len(manager.events) != 1 || len(network.teardownForwardCalls) != 1 {
		t.Fatal("failed acknowledgement lost intent or prevented independent cleanup")
	}
	manager.ackErr = nil
	if err := r.Reconcile(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if len(manager.events) != 0 || manager.ackCalls != 2 || len(network.teardownForwardCalls) != 2 {
		t.Fatal("acknowledgement retry did not converge idempotently")
	}
}
