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
	// teardownForwardErrByPort overrides teardownForwardErr for a specific
	// host port, so a test can make one port's teardown persistently fail
	// while others succeed normally.
	teardownForwardErrByPort map[int]error
	teardownCalls            []string
	teardownErr              error
	// teardownErrByTap overrides teardownErr for a specific tap device name,
	// so a test can make one generation's device teardown persistently fail
	// while others succeed normally.
	teardownErrByTap map[string]error
}

func (f *fakeNetworkManager) Setup(config.ServiceConfig) error { return nil }
func (f *fakeNetworkManager) Teardown(svc config.ServiceConfig) error {
	tap := ""
	if svc.Network != nil {
		tap = svc.Network.Interface
	}
	f.teardownCalls = append(f.teardownCalls, tap)
	if err, ok := f.teardownErrByTap[tap]; ok {
		return err
	}
	return f.teardownErr
}

func (f *fakeNetworkManager) SetupPortForward(hostPort int, _ string, _ int) error {
	f.setupForwardCalls = append(f.setupForwardCalls, hostPort)
	return f.setupForwardErr
}

func (f *fakeNetworkManager) TeardownPortForward(hostPort int, _ string, _ int) error {
	f.teardownForwardCalls = append(f.teardownForwardCalls, hostPort)
	if err, ok := f.teardownForwardErrByPort[hostPort]; ok {
		return err
	}
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

// The regression: pendingPortForwards used to be a single map keyed by
// service name, so a service passing through several configs before an
// earlier teardown ever recovered (A -> B -> C) would have the second
// generation's delete either overwrite or erase the first generation's still
// -pending entry. Keying by the rule's own (hostPort, guestIP, vmPort)
// identity instead means each generation's obsolete rule gets its own entry,
// independent of what happens to any other generation's.
func TestReconcile_ConsecutiveUpdatesTrackEachGenerationsStaleRuleIndependently(t *testing.T) {
	net := &fakeNetworkManager{teardownForwardErrByPort: map[int]error{8080: errors.New("rule missing")}}
	r, vmMgr := newNetworkTestReconciler(net)

	// A: port 8080.
	a := forwardedService("web")
	if err := r.Reconcile(context.Background(), config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{a}}); err != nil {
		t.Fatalf("initial create: %v", err)
	}

	// A -> B: port 9090. Tearing down A's 8080 fails and must be tracked.
	b := a
	b.PortForwards = []config.PortForward{{HostPort: 9090, VMPort: 80}}
	toB := config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{b}}
	if err := r.Reconcile(context.Background(), toB); err == nil {
		t.Fatal("expected A's stale 8080 rule to surface")
	}
	if len(r.pendingPortForwards) != 1 {
		t.Fatalf("expected exactly one pending port forward after A->B, got %d: %#v", len(r.pendingPortForwards), r.pendingPortForwards)
	}

	// B -> C: port 9191. Tearing down B's 9090 succeeds. The bug: this used
	// to overwrite or delete the single per-name entry, losing A's still-
	// pending 8080 rule.
	c := a
	c.PortForwards = []config.PortForward{{HostPort: 9191, VMPort: 80}}
	toC := config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{c}}
	err := r.Reconcile(context.Background(), toC)
	if err == nil {
		t.Fatal("A's 8080 rule is still pending and must keep surfacing through the B->C update")
	}
	if !strings.Contains(err.Error(), "8080") {
		t.Fatalf("error does not identify A's stale port: %v", err)
	}

	// A's entry must have survived, and only A's.
	if len(r.pendingPortForwards) != 1 {
		t.Fatalf("expected A's entry to be the only one still pending, got %d: %#v", len(r.pendingPortForwards), r.pendingPortForwards)
	}
	for _, entry := range r.pendingPortForwards {
		if entry.PortForward.HostPort != 8080 {
			t.Fatalf("wrong entry survived: %#v", entry)
		}
	}

	// One more tick with nothing changed: 8080 must still be retried, and
	// neither 9090 nor 9191 (both live-and-valid, or already resolved) may
	// be touched again by the retry path.
	callsBefore := len(net.teardownForwardCalls)
	if err := r.Reconcile(context.Background(), toC); err == nil {
		t.Fatal("A's stale rule must keep failing the tick")
	}
	for _, hp := range net.teardownForwardCalls[callsBefore:] {
		if hp != 8080 {
			t.Fatalf("retry touched an unrelated port forward: %d", hp)
		}
	}

	// Once A's rule is finally torn down, the tick converges and C keeps
	// running on its own port.
	delete(net.teardownForwardErrByPort, 8080)
	if err := r.Reconcile(context.Background(), toC); err != nil {
		t.Fatalf("tick should converge once A's stale rule is torn down: %v", err)
	}
	if len(r.pendingPortForwards) != 0 {
		t.Fatalf("expected no pending port forwards left, got %#v", r.pendingPortForwards)
	}
	inst, running := vmMgr.instances["web"]
	if !running || len(inst.Config.PortForwards) != 1 || inst.Config.PortForwards[0].HostPort != 9191 {
		t.Fatalf("service C not left running on its own port: %#v", vmMgr.instances["web"])
	}
}

// The regression: pendingNetworkTeardowns was previously keyed by service
// name, on the (false) premise that a tap device's path is derived purely
// from the name. It is not: svc.Network.Interface, when set explicitly,
// overrides the default tap-<name> path. So a service moving through two
// consecutive generations before an earlier device's teardown ever succeeds
// (A -> B -> C, each naming a distinct tap) could lose track of A's still-
// pending tap the same way finding 1 showed for port forwards: B's delete
// (success or failure) overwrote or erased the single per-name entry.
func TestReconcile_ConsecutiveUpdatesTrackEachGenerationsStaleNetworkDeviceIndependently(t *testing.T) {
	net := &fakeNetworkManager{teardownErrByTap: map[string]error{"tap-a": errors.New("device busy")}}
	r, vmMgr := newNetworkTestReconciler(net)

	// A: tap-a.
	a := config.ServiceConfig{Name: "web", Network: &config.NetworkConfig{Interface: "tap-a", GuestIP: "172.16.0.2"}}
	if err := r.Reconcile(context.Background(), config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{a}}); err != nil {
		t.Fatalf("initial create: %v", err)
	}

	// A -> B: tap-b. Tearing down A's tap-a fails and must be tracked.
	b := a
	b.Network = &config.NetworkConfig{Interface: "tap-b", GuestIP: "172.16.0.2"}
	toB := config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{b}}
	if err := r.Reconcile(context.Background(), toB); err == nil {
		t.Fatal("expected A's stale tap-a device to surface")
	}
	if len(r.pendingNetworkTeardowns) != 1 {
		t.Fatalf("expected exactly one pending network teardown after A->B, got %d: %#v", len(r.pendingNetworkTeardowns), r.pendingNetworkTeardowns)
	}

	// B -> C: tap-c. Tearing down B's tap-b succeeds. The bug: this used to
	// overwrite or delete the single per-name entry, losing A's still-
	// pending tap-a device.
	c := a
	c.Network = &config.NetworkConfig{Interface: "tap-c", GuestIP: "172.16.0.2"}
	toC := config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{c}}
	err := r.Reconcile(context.Background(), toC)
	if err == nil {
		t.Fatal("A's tap-a device is still pending and must keep surfacing through the B->C update")
	}
	if !strings.Contains(err.Error(), "tap-a") {
		t.Fatalf("error does not identify A's stale device: %v", err)
	}

	// A's entry must have survived, and only A's.
	if len(r.pendingNetworkTeardowns) != 1 {
		t.Fatalf("expected A's entry to be the only one still pending, got %d: %#v", len(r.pendingNetworkTeardowns), r.pendingNetworkTeardowns)
	}
	for key := range r.pendingNetworkTeardowns {
		if key.tapName != "tap-a" {
			t.Fatalf("wrong entry survived: %#v", key)
		}
	}

	// One more tick with nothing changed: tap-a must still be retried, and
	// neither tap-b nor tap-c (both live-and-valid, or already resolved) may
	// be touched again by the retry path.
	callsBefore := len(net.teardownCalls)
	if err := r.Reconcile(context.Background(), toC); err == nil {
		t.Fatal("A's stale device must keep failing the tick")
	}
	for _, tap := range net.teardownCalls[callsBefore:] {
		if tap != "tap-a" {
			t.Fatalf("retry touched an unrelated network device: %q", tap)
		}
	}

	// Once A's device is finally torn down, the tick converges and C keeps
	// running on its own tap.
	delete(net.teardownErrByTap, "tap-a")
	if err := r.Reconcile(context.Background(), toC); err != nil {
		t.Fatalf("tick should converge once A's stale device is torn down: %v", err)
	}
	if len(r.pendingNetworkTeardowns) != 0 {
		t.Fatalf("expected no pending network teardowns left, got %#v", r.pendingNetworkTeardowns)
	}
	inst, running := vmMgr.instances["web"]
	if !running || inst.Config.Network == nil || inst.Config.Network.Interface != "tap-c" {
		t.Fatalf("service C not left running on its own tap: %#v", vmMgr.instances["web"])
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

// The regression: os.WriteFile truncates the target before writing, so a
// crash between the truncate and the write (or a partial write) can leave
// corrupt JSON on disk. atomicWriteFile must instead write to a temp file
// and rename it into place, so the target is always either the previous
// complete content or the new complete content, with no in-between state —
// and it must leave no stray temp file behind once it succeeds.
func TestAtomicWriteFile_ReplacesContentAndCleansUpTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.json")

	if err := atomicWriteFile(path, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := atomicWriteFile(path, []byte(`{"a":2}`), 0o644); err != nil {
		t.Fatalf("second write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":2}` {
		t.Fatalf("got %q, want the latest write to have fully replaced the file", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "journal.json" {
			t.Fatalf("stray temp file left behind after a successful write: %s", e.Name())
		}
	}
}

// The regression: a journal that fails to parse (whether from a genuine
// crash despite the atomic write above, disk corruption, or manual
// tampering) used to be logged as a warning and silently treated as an empty
// pending set — losing track of whatever was actually pending, possibly a
// still-live obsolete DNAT rule, with the node free to report converged
// regardless. It must instead permanently fail every Reconcile call and
// refuse to overwrite the file, so the evidence survives for an operator to
// inspect.
func TestWithStateDir_CorruptJournalBlocksConvergencePermanently(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	journalPath := filepath.Join(dir, pendingTeardownsFile)
	if err := os.WriteFile(journalPath, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewWithNetworkManager(newFakeVMManager(), logger, nil, &fakeNetworkManager{}, "", 0).WithStateDir(dir)
	if !r.journalCorrupt {
		t.Fatal("expected journalCorrupt to be set after loading a corrupt journal")
	}

	desired := config.NodeConfig{Node: "node-1"}
	if err := r.Reconcile(context.Background(), desired); err == nil {
		t.Fatal("a corrupt journal must fail Reconcile even on an otherwise-trivial tick")
	}
	if err := r.Reconcile(context.Background(), desired); err == nil {
		t.Fatal("a corrupt journal must keep failing every later tick, not just the first")
	}

	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	r.persistPendingTeardowns()
	after, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("corrupt journal was overwritten instead of left for inspection: before=%q after=%q", before, after)
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
