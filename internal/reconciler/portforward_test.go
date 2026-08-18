package reconciler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

type fakeNetworkManager struct {
	setupForwardCalls    []int
	teardownForwardCalls []int
	setupForwardErr      error
	teardownForwardErr   error
}

func (f *fakeNetworkManager) Setup(config.ServiceConfig) error    { return nil }
func (f *fakeNetworkManager) Teardown(config.ServiceConfig) error { return nil }

func (f *fakeNetworkManager) SetupPortForward(hostPort int, _ string, _ int) error {
	f.setupForwardCalls = append(f.setupForwardCalls, hostPort)
	return f.setupForwardErr
}

func (f *fakeNetworkManager) TeardownPortForward(hostPort int, _ string, _ int) error {
	f.teardownForwardCalls = append(f.teardownForwardCalls, hostPort)
	return f.teardownForwardErr
}

func forwardedService(name string) config.ServiceConfig {
	return config.ServiceConfig{
		Name:         name,
		Network:      &config.NetworkConfig{GuestIP: "172.16.0.2"},
		PortForwards: []config.PortForward{{HostPort: 8080, VMPort: 80}},
	}
}

func newNetworkTestReconciler(net NetworkManager) (*Reconciler, *fakeVMManager) {
	vmMgr := newFakeVMManager()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewWithNetworkManager(vmMgr, logger, nil, net, "", 0), vmMgr
}

// The regression: a port forward that failed while creating the service was
// logged and dropped. An existing VM produces no further plan actions, so
// nothing retried it and the node kept reporting a converged revision while
// cross-node routing stayed broken.
func TestReconcile_RetriesPortForwardAfterCreateFailure(t *testing.T) {
	net := &fakeNetworkManager{setupForwardErr: errors.New("iptables busy")}
	r, _ := newNetworkTestReconciler(net)
	desired := config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{forwardedService("web")}}

	// First tick creates the VM; the port forward fails.
	if err := r.Reconcile(context.Background(), desired); err == nil {
		t.Fatal("expected the failed port forward to surface as a reconcile error")
	}

	// Second tick has no plan actions at all, and must still retry.
	before := len(net.setupForwardCalls)
	err := r.Reconcile(context.Background(), desired)
	if err == nil {
		t.Fatal("a persistently failing port forward must keep failing the tick, not converge")
	}
	if !strings.Contains(err.Error(), "port forward") {
		t.Fatalf("error does not identify the port forward: %v", err)
	}
	if len(net.setupForwardCalls) <= before {
		t.Fatal("port forward was not re-asserted on a tick with no plan actions")
	}
}

// Recovery must be automatic once the underlying cause clears, without the VM
// being destroyed and recreated.
func TestReconcile_ConvergesOncePortForwardRecovers(t *testing.T) {
	net := &fakeNetworkManager{setupForwardErr: errors.New("iptables busy")}
	r, vmMgr := newNetworkTestReconciler(net)
	desired := config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{forwardedService("web")}}

	if err := r.Reconcile(context.Background(), desired); err == nil {
		t.Fatal("expected first tick to fail")
	}
	startsAfterCreate := len(vmMgr.startCalls)

	net.setupForwardErr = nil
	if err := r.Reconcile(context.Background(), desired); err != nil {
		t.Fatalf("tick should converge once the port forward succeeds: %v", err)
	}
	if len(vmMgr.startCalls) != startsAfterCreate {
		t.Fatalf("VM was restarted to recover a port forward: %d starts, want %d",
			len(vmMgr.startCalls), startsAfterCreate)
	}
}

// A failed teardown leaves a DNAT rule pointing at a dead guest. It must be
// visible, but it must not abort the tick's other work.
func TestReconcile_SurfacesPortForwardTeardownFailure(t *testing.T) {
	net := &fakeNetworkManager{teardownForwardErr: errors.New("rule missing")}
	r, vmMgr := newNetworkTestReconciler(net)
	desired := config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{forwardedService("web")}}

	if err := r.Reconcile(context.Background(), desired); err != nil {
		t.Fatalf("initial create: %v", err)
	}

	// Drop the service so the next tick deletes it.
	empty := config.NodeConfig{Node: "node-1"}
	err := r.Reconcile(context.Background(), empty)
	if err == nil {
		t.Fatal("a failed port-forward teardown must surface")
	}
	if !strings.Contains(err.Error(), "teardown port forward") {
		t.Fatalf("error does not identify the teardown: %v", err)
	}
	if len(vmMgr.removeCalls) == 0 {
		t.Fatal("the VM should still have been removed")
	}

	// The regression: once the VM is gone, Plan has no further delete action
	// for "web" — nothing about the desired/actual diff changes on the next
	// tick. Without a retry path the obsolete DNAT rule is simply never
	// looked at again and the tick starts reporting success.
	teardownCallsAfterFirstTick := len(net.teardownForwardCalls)
	tick2Err := r.Reconcile(context.Background(), empty)
	if tick2Err == nil {
		t.Fatal("a still-failing teardown must keep failing every later tick, not be forgotten once the VM is gone")
	}
	if len(net.teardownForwardCalls) <= teardownCallsAfterFirstTick {
		t.Fatal("the obsolete port forward was not retried on a tick with no plan actions")
	}
	// The stage tag drives the agent's NetworkReady=false condition
	// (classifyReconcileFailure in internal/agent/agent.go); it must survive
	// the extra combineErrors/errors.Join nesting retryPendingTeardowns adds.
	if !HasFailureStage(tick2Err, FailureStageNetwork) {
		t.Fatalf("retried teardown error lost its network failure stage: %v", tick2Err)
	}

	// Once the underlying failure clears, the retry must actually finish the
	// cleanup and let the node converge again.
	net.teardownForwardErr = nil
	if err := r.Reconcile(context.Background(), empty); err != nil {
		t.Fatalf("tick should converge once the retried teardown succeeds: %v", err)
	}
	if err := r.Reconcile(context.Background(), empty); err != nil {
		t.Fatalf("a cleared teardown must not resurface on a later tick: %v", err)
	}
}

// The update path skips the recreate when deleteService errors, so a teardown
// failure must not leave the service deleted. The old and new configs here
// claim the identical port forward (only VCPUs changed), so once the update
// recreates the VM there is nothing obsolete left to clean up and the tick
// must converge — this is the case the previous version of this test
// discarded the returned error for instead of checking it.
func TestReconcile_TeardownFailureDoesNotBlockUpdate(t *testing.T) {
	net := &fakeNetworkManager{teardownForwardErr: errors.New("rule missing")}
	r, vmMgr := newNetworkTestReconciler(net)
	svc := forwardedService("web")
	desired := config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{svc}}

	if err := r.Reconcile(context.Background(), desired); err != nil {
		t.Fatalf("initial create: %v", err)
	}

	updated := svc
	updated.VCPUs = 4
	err := r.Reconcile(context.Background(), config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{updated}})
	if err != nil {
		t.Fatalf("update with unchanged port forwards must not fail: %v", err)
	}

	if _, running := vmMgr.instances["web"]; !running {
		t.Fatal("service was left deleted after a non-blocking teardown failure")
	}
}

// The regression: an update that also changes a service's port forwards used
// to let the stale rule for the old port disappear silently once the
// same-named VM was recreated — retryPendingTeardowns saw the name running
// again and dropped the pending entry unconditionally. It must instead
// recognize the old rule is distinct from whatever the new config claims,
// keep failing the tick until the old rule is actually torn down, and never
// touch the new port's live rule while doing so.
func TestReconcile_UpdateWithChangedPortKeepsRetryingStaleRule(t *testing.T) {
	net := &fakeNetworkManager{}
	r, vmMgr := newNetworkTestReconciler(net)
	svc := forwardedService("web") // HostPort: 8080
	desired := config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{svc}}

	if err := r.Reconcile(context.Background(), desired); err != nil {
		t.Fatalf("initial create: %v", err)
	}

	// The update moves the service to a different host port; tearing down
	// the old one fails.
	net.teardownForwardErr = errors.New("rule missing")
	updated := svc
	updated.PortForwards = []config.PortForward{{HostPort: 9090, VMPort: 80}}
	moved := config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{updated}}

	err := r.Reconcile(context.Background(), moved)
	if err == nil {
		t.Fatal("a stale rule from a changed port must surface, not be dropped because the name is running again")
	}
	if !strings.Contains(err.Error(), "teardown port forward") {
		t.Fatalf("error does not identify the stale teardown: %v", err)
	}
	if !HasFailureStage(err, FailureStageNetwork) {
		t.Fatalf("stale-rule error lost its network failure stage: %v", err)
	}
	if !containsInt(net.setupForwardCalls, 9090) {
		t.Fatal("the new port forward was not set up for the updated service")
	}

	// The stale 8080 teardown must keep being retried every tick, without
	// ever touching 9090's live rule.
	teardownCallsBefore := len(net.teardownForwardCalls)
	if err := r.Reconcile(context.Background(), moved); err == nil {
		t.Fatal("the stale rule must keep failing the tick until it is actually torn down")
	}
	retried := net.teardownForwardCalls[teardownCallsBefore:]
	if containsInt(retried, 9090) {
		t.Fatal("retry tore down the new port's live rule instead of the stale one")
	}
	if !containsInt(retried, 8080) {
		t.Fatal("the stale 8080 rule was not retried")
	}

	// Once the old rule's teardown succeeds, the tick converges and the
	// updated service keeps running on the new port.
	net.teardownForwardErr = nil
	if err := r.Reconcile(context.Background(), moved); err != nil {
		t.Fatalf("tick should converge once the stale rule is torn down: %v", err)
	}
	inst, running := vmMgr.instances["web"]
	if !running || len(inst.Config.PortForwards) != 1 || inst.Config.PortForwards[0].HostPort != 9090 {
		t.Fatalf("updated service not left running on the new port: %#v", vmMgr.instances["web"])
	}
}

// The regression: pendingTeardowns previously lived in memory only, so an
// agent restart between a failed delete's teardown and its retry silently
// forgot the obsolete host rule with nothing left to notice it. WithStateDir
// must make that entry survive the restart and get retried by the new
// process.
func TestWithStateDir_PersistsPendingTeardownAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	net1 := &fakeNetworkManager{teardownForwardErr: errors.New("rule missing")}
	r1 := NewWithNetworkManager(newFakeVMManager(), logger, nil, net1, "", 0).WithStateDir(dir)
	desired := config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{forwardedService("web")}}
	if err := r1.Reconcile(context.Background(), desired); err != nil {
		t.Fatalf("initial create: %v", err)
	}
	if err := r1.Reconcile(context.Background(), config.NodeConfig{Node: "node-1"}); err == nil {
		t.Fatal("expected the failed teardown to surface")
	}

	persisted := filepath.Join(dir, pendingTeardownsFile)
	if _, err := os.Stat(persisted); err != nil {
		t.Fatalf("pending teardown was not persisted to %s: %v", persisted, err)
	}

	// Simulate a restart: a fresh Reconciler, fresh fakeVMManager (the VM was
	// already gone before the crash — only the host cleanup was pending),
	// pointed at the same state dir. It must load the persisted entry rather
	// than starting with nothing to retry.
	net2 := &fakeNetworkManager{}
	r2 := NewWithNetworkManager(newFakeVMManager(), logger, nil, net2, "", 0).WithStateDir(dir)
	if err := r2.Reconcile(context.Background(), config.NodeConfig{Node: "node-1"}); err != nil {
		t.Fatalf("restarted reconciler should have converged once the persisted teardown succeeded: %v", err)
	}
	if !containsInt(net2.teardownForwardCalls, 8080) {
		t.Fatal("restarted reconciler did not retry the persisted teardown; the entry was lost across restart")
	}
	if _, err := os.Stat(persisted); !os.IsNotExist(err) {
		t.Fatalf("persisted file should have been removed once cleared, stat err = %v", err)
	}
}

func containsInt(vals []int, want int) bool {
	for _, v := range vals {
		if v == want {
			return true
		}
	}
	return false
}

// A nil *network.Manager must not become a non-nil interface, or every guard
// passes and the first call dereferences nil.
func TestNew_NilNetworkManagerStaysNil(t *testing.T) {
	r := New(newFakeVMManager(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, "", 0)
	if r.networkMgr != nil {
		t.Fatal("nil *network.Manager was stored as a non-nil interface")
	}
	desired := config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{forwardedService("web")}}
	if err := r.Reconcile(context.Background(), desired); err != nil {
		t.Fatalf("reconcile without a network manager: %v", err)
	}
}
