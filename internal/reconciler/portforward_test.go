package reconciler

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
	return newWithNetworkManager(vmMgr, logger, nil, net, "", 0), vmMgr
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
	err := r.Reconcile(context.Background(), config.NodeConfig{Node: "node-1"})
	if err == nil {
		t.Fatal("a failed port-forward teardown must surface")
	}
	if !strings.Contains(err.Error(), "teardown port forward") {
		t.Fatalf("error does not identify the teardown: %v", err)
	}
	if len(vmMgr.removeCalls) == 0 {
		t.Fatal("the VM should still have been removed")
	}
}

// The update path skips the recreate when deleteService errors, so a teardown
// failure must not leave the service deleted.
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
	_ = r.Reconcile(context.Background(), config.NodeConfig{Node: "node-1", Services: []config.ServiceConfig{updated}})

	if _, running := vmMgr.instances["web"]; !running {
		t.Fatal("service was left deleted after a non-blocking teardown failure")
	}
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
