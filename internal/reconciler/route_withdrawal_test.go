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

// fakeRoutes is a route directory and the Traefik process serving it.
type fakeRoutes struct {
	routes      []RouteTarget
	unreadable  []string
	boot, start string
	processErr  error
}

func (f *fakeRoutes) LocalRoutes() ([]RouteTarget, []string, error) {
	return f.routes, f.unreadable, nil
}
func (f *fakeRoutes) ServingProcess() (string, string, error) { return f.boot, f.start, f.processErr }
func (f *fakeRoutes) Unit() string                            { return "traefik.service" }

func newRouteHost(t *testing.T) *crashHost {
	h := newCrashHost(t)
	h.routes = &fakeRoutes{routes: []RouteTarget{{Service: "zeta", GuestIP: "172.16.0.2"}}, boot: "boot-1", start: "100"}
	return h
}

// next asks for zeta's old address on a port of its own.
var next = crashService("next", "172.16.0.2", 9090)

func (h *crashHost) holdOfNext(r *Reconciler) Hold {
	h.t.Helper()
	desired := config.NodeConfig{Services: []config.ServiceConfig{next}}
	_ = r.Reconcile(context.Background(), desired)
	if h.vms.instances["next"] != nil {
		h.t.Fatalf("next took zeta's address: routes %+v", h.routes)
	}
	return r.HeldServiceHolds(desired)["next"]
}

// L7: an agent crash after recording a withdrawal, before the route file
// changes, followed by a reboot, releases nothing: Traefik started at boot
// read the route file that was still there. The address is released only
// after the agent removes the file and a later Traefik start.
func TestL7CrashBeforeTheFileChangesSurvivesAReboot(t *testing.T) {
	h := newRouteHost(t)
	r := h.agent()
	if hold := h.holdOfNext(r); !strings.Contains(hold.Reason, "route of service zeta") {
		t.Fatalf("a live route did not hold the address: %+v", hold)
	}
	if err := r.BeginRouteWithdrawals([]RouteTarget{{Service: "zeta", GuestIP: "172.16.0.2"}}, false); err != nil {
		t.Fatal(err)
	}
	// Crash, then reboot: a new boot and a new Traefik that read the old file.
	h.routes.boot, h.routes.start = "boot-2", "5"
	restarted := h.agent()
	if hold := h.holdOfNext(restarted); hold.Reason == "" || !restarted.HasPendingCleanup() {
		t.Fatalf("a reboot released an incomplete withdrawal: %+v", hold)
	}
	h.routes.routes = nil // The restarted agent's publish removes the file.
	if hold := h.holdOfNext(restarted); hold.Action != ActionGuestAddressWithdrawalUnconfirmed || !strings.Contains(hold.Reason, "traefik.service") {
		t.Fatalf("hold = %+v", hold)
	}
	if restarted.HasPendingCleanup() {
		t.Fatal("a wait only an operator ends keeps forcing ticks")
	}
	h.routes.start = "9" // The operator restarts Traefik.
	h.converge(restarted, config.NodeConfig{Services: []config.ServiceConfig{next}})
	h.assertOwns(restarted, config.NodeConfig{Services: []config.ServiceConfig{next}})
}

// L7: a crash after the file change, before the withdrawal is recorded as
// complete, completes it on restart with the Traefik running then, which may
// still serve the route.
func TestL7CrashAfterTheFileChangeCompletesOnRestart(t *testing.T) {
	h := newRouteHost(t)
	r := h.agent()
	if err := r.BeginRouteWithdrawals([]RouteTarget{{Service: "zeta", GuestIP: "172.16.0.2"}}, false); err != nil {
		t.Fatal(err)
	}
	h.routes.routes = nil
	restarted := h.agent()
	if hold := h.holdOfNext(restarted); hold.Action != ActionGuestAddressWithdrawalUnconfirmed {
		t.Fatalf("hold = %+v", hold)
	}
	h.routes.start = "200"
	h.converge(restarted, config.NodeConfig{Services: []config.ServiceConfig{next}})
}

// L7: a stopped Traefik releases a withdrawal only once it is complete. While
// the route file may still be there, a Traefik started later would read it.
func TestL7InactiveUnitReleasesOnlyACompleteWithdrawal(t *testing.T) {
	h := newRouteHost(t)
	r := h.agent()
	if err := r.BeginRouteWithdrawals([]RouteTarget{{Service: "zeta", GuestIP: "172.16.0.2"}}, false); err != nil {
		t.Fatal(err)
	}
	h.routes.start = "" // Traefik is stopped.
	if hold := h.holdOfNext(r); hold.Reason == "" {
		t.Fatal("an inactive unit released an incomplete withdrawal")
	}
	h.routes.routes = nil
	h.converge(r, config.NodeConfig{Services: []config.ServiceConfig{next}})
}

// L7: when the unit cannot be inspected, nothing is released and the hold
// says why.
func TestL7UnreadableUnitHoldsTheAddress(t *testing.T) {
	h := newRouteHost(t)
	r := h.agent()
	if err := r.BeginRouteWithdrawals([]RouteTarget{{Service: "zeta", GuestIP: "172.16.0.2"}}, false); err != nil {
		t.Fatal(err)
	}
	h.routes.routes, h.routes.processErr = nil, errors.New("Unit traefik.service could not be found")
	for tick := 0; tick < 2; tick++ {
		if hold := h.holdOfNext(r); hold.Reason == "" {
			t.Fatal("released without inspecting the serving process")
		}
	}
	h.routes.processErr, h.routes.start = nil, ""
	h.converge(r, config.NodeConfig{Services: []config.ServiceConfig{next}})
}

// L7 baseline: routes an earlier agent removed are unknown, so a migrated
// node holds every address for a new service until Traefik restarts. A
// service keeps its own address meanwhile.
func TestL7MigrationHoldsEveryAddressUntilTraefikRestarts(t *testing.T) {
	h := newRouteHost(t)
	h.routes.routes = nil
	if err := os.Mkdir(filepath.Join(h.stateDir, "vms"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := h.agent()
	if r.history.Complete {
		t.Fatal("a host with VM state claimed complete history")
	}
	fresh := crashService("fresh", "172.16.0.7", 7070)
	desired := config.NodeConfig{Services: []config.ServiceConfig{fresh}}
	_ = r.Reconcile(context.Background(), desired)
	if hold := r.HeldServiceHolds(desired)["fresh"]; h.vms.instances["fresh"] != nil || hold.Reason == "" {
		t.Fatalf("fresh started on a migrated node before Traefik restarted: %+v", hold)
	}
	h.routes.start = "300"
	// The port is still unrecorded on a node with incomplete history (L6);
	// the address is no longer held.
	_ = r.Reconcile(context.Background(), desired)
	if hold := r.HeldServiceHolds(desired)["fresh"]; hold.Action != ActionNetworkHistoryIncomplete {
		t.Fatalf("after the restart: %+v", hold)
	}
}

// An unreadable route file may route to any address, so it holds every
// networked start.
func TestUnreadableRouteFileHoldsStarts(t *testing.T) {
	h := newRouteHost(t)
	h.routes.routes, h.routes.unreadable = nil, []string{"extra.toml"}
	r := h.agent()
	if hold := h.holdOfNext(r); !strings.Contains(hold.Reason, "extra.toml") {
		t.Fatalf("hold = %+v", hold)
	}
	h.routes.unreadable = nil
	h.converge(r, config.NodeConfig{Services: []config.ServiceConfig{next}})
}

// C4/L7 without host network setup: the agent manages no TAPs or rules, but
// Traefik still routes to guest addresses. A static address another service's
// route still points at, or was withdrawn from without proof, waits.
// (Finding from the review of 2f5aa27.)
func TestRouteOwnershipHoldsWithoutHostNetworking(t *testing.T) {
	routes := &fakeRoutes{routes: []RouteTarget{{Service: "zeta", GuestIP: "172.16.0.2"}}, boot: "boot-1", start: "100"}
	vms := newFakeVMManager()
	r := New(vms, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, "", 0).WithRouteEvidence(routes).WithStateDir(t.TempDir())
	desired := config.NodeConfig{Services: []config.ServiceConfig{next}}
	_ = r.Reconcile(context.Background(), desired)
	if vms.instances["next"] != nil {
		t.Fatal("next started where zeta's route still points")
	}
	if err := r.BeginRouteWithdrawals([]RouteTarget{{Service: "zeta", GuestIP: "172.16.0.2"}}, false); err != nil {
		t.Fatal(err)
	}
	routes.routes = nil
	_ = r.Reconcile(context.Background(), desired)
	if hold := r.HeldServiceHolds(desired)["next"]; vms.instances["next"] != nil || hold.Action != ActionGuestAddressWithdrawalUnconfirmed {
		t.Fatalf("next started before Traefik restarted: %+v", hold)
	}
	routes.start = "200"
	for tick := 0; tick < 2; tick++ {
		_ = r.Reconcile(context.Background(), desired)
	}
	if vms.instances["next"] == nil {
		t.Fatalf("next did not start after Traefik restarted: %v", r.HeldServices(desired))
	}
}

// L7 without host network setup: on a host with earlier VM state, routes an
// earlier agent removed are unknown, so a new address waits for a Traefik
// restart here too.
func TestMigrationBaselineHoldsWithoutHostNetworking(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "vms"), 0o755); err != nil {
		t.Fatal(err)
	}
	routes := &fakeRoutes{boot: "boot-1", start: "100"}
	vms := newFakeVMManager()
	r := New(vms, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, "", 0).WithRouteEvidence(routes).WithStateDir(dir)
	desired := config.NodeConfig{Services: []config.ServiceConfig{next}}
	for tick := 0; tick < 2; tick++ {
		_ = r.Reconcile(context.Background(), desired)
	}
	if hold := r.HeldServiceHolds(desired)["next"]; vms.instances["next"] != nil || hold.Action != ActionGuestAddressWithdrawalUnconfirmed {
		t.Fatalf("a migrated node gave out an address before Traefik restarted: %+v", hold)
	}
	routes.start = "200"
	for tick := 0; tick < 2; tick++ {
		_ = r.Reconcile(context.Background(), desired)
	}
	if vms.instances["next"] == nil {
		t.Fatalf("next did not start after Traefik restarted: %v", r.HeldServices(desired))
	}
}
