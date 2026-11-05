package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/network"
	"github.com/artemnikitin/firework/internal/network/networktest"
	"github.com/artemnikitin/firework/internal/vm"
)

// journalReconciler builds a reconciler as the agent does, with the guest
// subnet 172.16.0.0/24 and route files under <dir>/routes.
func journalReconciler(t *testing.T, vms VMManager, kernel *networktest.Kernel, dir string) *Reconciler {
	t.Helper()
	routes := func() (bool, error) {
		entries, err := os.ReadDir(filepath.Join(dir, "routes"))
		if os.IsNotExist(err) {
			return false, nil
		}
		return len(entries) > 0, err
	}
	return NewWithNetworkManager(vms, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, kernel, "", 0).
		WithHostHistoryEvidence("172.16.0.0/24", routes).WithStateDir(dir)
}

// Initialization that cannot read its evidence waits rather than guessing:
// new networked starts are held, and the next tick tries again.
func TestJournalInitializationWaitsForUnreadableEvidence(t *testing.T) {
	dir, kernel := t.TempDir(), &networktest.Kernel{}
	failing := true
	vms := newFakeVMManager()
	r := NewWithNetworkManager(vms, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, kernel, "", 0).
		WithHostHistoryEvidence("172.16.0.0/24", func() (bool, error) {
			if failing {
				return false, errors.New("permission denied")
			}
			return false, nil
		}).WithStateDir(dir)
	desired := config.NodeConfig{Services: []config.ServiceConfig{crashService("web", "172.16.0.2", 8080)}}
	if err := r.Reconcile(context.Background(), desired); err == nil || vms.instances["web"] != nil {
		t.Fatalf("started without initialized records: %v", err)
	}
	failing = false
	if err := r.Reconcile(context.Background(), desired); err != nil || vms.instances["web"] == nil || !r.history.Complete {
		t.Fatalf("did not initialize once the evidence was readable: %v", err)
	}
}

// Ownership history is complete only on a host with no earlier firework
// state: no journal, no VM state directory, no DNAT rule toward the guest
// subnet, tagged or not, and no local route file. Anything else is migrated,
// and its history stays incomplete across restarts.
func TestJournalInitializationRecordsWhetherHistoryIsComplete(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare  func(t *testing.T, dir string, kernel *networktest.Kernel)
		complete bool
	}{
		"clean host": {func(*testing.T, string, *networktest.Kernel) {}, true},
		"an untagged rule toward the guest subnet": {func(_ *testing.T, _ string, kernel *networktest.Kernel) {
			kernel.Append(networktest.Rule("", 8080, "172.16.0.2", 80))
		}, false},
		"an unrelated rule outside the guest subnet": {func(_ *testing.T, _ string, kernel *networktest.Kernel) {
			kernel.Append(networktest.Rule("", 8080, "10.9.0.2", 80))
		}, true},
		"a DNAT rule whose destination cannot be read": {func(_ *testing.T, _ string, kernel *networktest.Kernel) {
			kernel.Unparsed = []string{"-A PREROUTING -p tcp -m tcp --dport 7000:7010 -j DNAT --to-destination 172.16.0.5"}
		}, false},
		"a local route file": {func(t *testing.T, dir string, _ *networktest.Kernel) {
			if err := os.WriteFile(filepath.Join(dir, "routes", "old.yaml"), []byte("http: {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, false},
		"only peer route files": {func(t *testing.T, dir string, _ *networktest.Kernel) {
			// A local service may be named like a peer route file.
			if err := os.WriteFile(filepath.Join(dir, "routes", "remote-peer.yaml"), []byte("http: {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, false},
		"VM state directory exists": {func(t *testing.T, dir string, _ *networktest.Kernel) {
			if err := os.Mkdir(filepath.Join(dir, "vms"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, false},
		"a tagged rule exists": {func(_ *testing.T, _ string, kernel *networktest.Kernel) {
			kernel.Append(networktest.Rule(network.PortForwardTag("old"), 8080, "172.16.0.2", 80))
		}, false},
		"version 1 journal": {func(t *testing.T, dir string, _ *networktest.Kernel) {
			if err := os.WriteFile(filepath.Join(dir, pendingTeardownsFile), []byte(`{"version":1}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			dir, kernel := t.TempDir(), &networktest.Kernel{}
			if err := os.Mkdir(filepath.Join(dir, "routes"), 0o755); err != nil {
				t.Fatal(err)
			}
			tc.prepare(t, dir, kernel)
			r := journalReconciler(t, newFakeVMManager(), kernel, dir)
			if !r.ownershipReady() || r.history.Complete != tc.complete {
				t.Fatalf("ready=%v complete=%v, want complete=%v", r.ownershipReady(), r.history.Complete, tc.complete)
			}
			// Deleting the VM state later does not make history complete.
			_ = os.RemoveAll(filepath.Join(dir, "vms"))
			restarted := journalReconciler(t, newFakeVMManager(), kernel, dir)
			if restarted.history.Complete != tc.complete {
				t.Fatalf("history became complete=%v after a restart", restarted.history.Complete)
			}
		})
	}
}

// Migration from a version 1 journal: its pending teardowns become departing
// records that may delete the matching untagged rules, a running VM gets a
// record and keeps its untagged rule, and an untagged rule nothing accounts
// for is left in place.
func TestVersion1JournalMigratesIntoRecords(t *testing.T) {
	dir, kernel := t.TempDir(), &networktest.Kernel{}
	legacy := `{"version":1,"port_forwards":[{"service_name":"gone","guest_ip":"172.16.0.3","port_forward":{"host_port":9090,"vm_port":80}}],` +
		`"network":[{"service_name":"gone","kind":"tap","name":"tap-gone"}]}`
	if err := os.WriteFile(filepath.Join(dir, pendingTeardownsFile), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	kernel.Append(networktest.Rule("", 9090, "172.16.0.3", 80))  // gone's, from before tagging
	kernel.Append(networktest.Rule("", 8080, "172.16.0.2", 80))  // web's, from before tagging
	kernel.Append(networktest.Rule("", 7070, "172.16.0.9", 70))  // nobody's
	kernel.Append(networktest.Rule("", 9090, "172.16.0.3", 443)) // same address, another VM port
	vms := newFakeVMManager()
	web := forwardedService("web")
	vms.instances[web.Name] = &vm.Instance{Name: web.Name, Config: web, State: vm.StateRunning}
	r := journalReconciler(t, vms, kernel, dir)
	if err := r.Reconcile(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{web}}); err != nil {
		t.Fatal(err)
	}
	if r.history.Complete {
		t.Fatal("a migrated journal claims complete history")
	}
	var remaining []string
	for _, rule := range kernel.Rules() {
		remaining = append(remaining, rule.String())
	}
	if len(remaining) != 3 {
		// web's tagged re-assertion, which replaced its untagged rule, and
		// both rules no record covers.
		t.Fatalf("rules after migration: %v", remaining)
	}
	if rule, ok := kernel.Delivers(8080); !ok || !rule.TaggedFor("web") {
		t.Fatalf("web's rule was not adopted: %v", remaining)
	}
	for _, rule := range kernel.Rules() {
		if rule.Targets(9090, "172.16.0.3", 80) {
			t.Fatalf("gone's untagged rule survived: %v", remaining)
		}
	}
	if running := recordsIn(r, phaseRunning); len(running) != 1 || running[0].Service != "web" || !running[0].Untagged {
		t.Fatalf("web's record: %#v", running)
	}
	if len(recordsIn(r, phaseDeparting)) != 0 || r.HasPendingCleanup() {
		t.Fatalf("migrated cleanup did not finish: %#v", recordsIn(r, phaseDeparting))
	}
}

// G1: a corrupt journal holds every networked start, writes nothing over the
// evidence, and leaves running VMs and services without networking alone.
func TestCorruptJournalHoldsNetworkedStarts(t *testing.T) {
	dir, kernel := t.TempDir(), &networktest.Kernel{}
	path := filepath.Join(dir, pendingTeardownsFile)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	vms := newFakeVMManager()
	running := forwardedService("running")
	vms.instances[running.Name] = &vm.Instance{Name: running.Name, Config: running, State: vm.StateRunning}
	r := journalReconciler(t, vms, kernel, dir)
	desired := config.NodeConfig{Services: []config.ServiceConfig{running, crashService("new", "172.16.0.9", 9090), {Name: "plain"}}}
	if err := r.Reconcile(context.Background(), desired); err == nil {
		t.Fatal("a corrupt journal must fail the tick")
	}
	if vms.instances["new"] != nil || vms.instances["plain"] == nil || vms.instances["running"] == nil {
		t.Fatalf("new=%v plain=%v running=%v", vms.instances["new"], vms.instances["plain"], vms.instances["running"])
	}
	if held := r.HeldServices(desired); held["new"] == "" {
		t.Fatalf("the networked start is not reported as held: %v", held)
	}
	if data, _ := os.ReadFile(path); string(data) != "{not json" {
		t.Fatalf("the corrupt journal was overwritten: %q", data)
	}
}

// A create whose record cannot be written starts nothing, so nothing exists
// on the host without a record.
func TestCreateWithoutADurableRecordStartsNothing(t *testing.T) {
	h := newCrashHost(t)
	r := h.agent()
	r.WithJournalWriter(func(string, []byte) error { return errors.New("disk full") })
	web := crashService("web", "172.16.0.2", 8080)
	if err := r.Reconcile(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{web}}); err == nil {
		t.Fatal("the failed record write was hidden")
	}
	if len(h.vms.startCalls) != 0 || len(h.kernel.Rules()) != 0 || h.kernel.HasDevice("tap-web") {
		t.Fatalf("something was created without a record: starts=%v rules=%v", h.vms.startCalls, h.kernel.Rules())
	}
}

// Without host networking, a version 1 journal's pending cleanup is migrated,
// not dropped: an agent that later manages host networking still deletes the
// old rule and TAP. Ports went unrecorded meanwhile, so an unrecorded port
// waits (L6). (Finding from the review of a3cbc29.)
func TestLegacyCleanupSurvivesDisabledHostNetworking(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"version":1,"port_forwards":[{"service_name":"gone","guest_ip":"172.16.0.2","port_forward":{"host_port":8080,"vm_port":80}}],"network":[{"service_name":"gone","kind":"tap","name":"tap-gone"}]}`
	if err := os.WriteFile(filepath.Join(dir, pendingTeardownsFile), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	routes := &fakeRoutes{boot: "boot-1", start: "100"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	disabled := New(newFakeVMManager(), logger, nil, nil, "", 0).WithRouteEvidence(routes).WithStateDir(dir)
	if !disabled.ownershipReady() {
		t.Fatal("the agent without host networking did not initialize")
	}
	kernel := &networktest.Kernel{}
	if err := kernel.Setup(config.ServiceConfig{Name: "gone", Network: &config.NetworkConfig{GuestIP: "172.16.0.2"}}); err != nil {
		t.Fatal(err)
	}
	kernel.Append(networktest.Rule("", 8080, "172.16.0.2", 80))
	vms := newFakeVMManager()
	enabled := NewWithNetworkManager(vms, logger, nil, kernel, "", 0).WithRouteEvidence(routes).WithStateDir(dir)
	routes.start = "200" // Traefik restarted, so only L6 can hold a start.
	fresh := crashService("fresh", "172.16.0.5", 9090)
	desired := config.NodeConfig{Services: []config.ServiceConfig{fresh}}
	for tick := 0; tick < 3; tick++ {
		_ = enabled.Reconcile(context.Background(), desired)
	}
	if kernel.HasDevice("tap-gone") || len(kernel.Rules()) != 0 {
		t.Fatalf("the migrated cleanup was lost: tap=%v rules=%v", kernel.HasDevice("tap-gone"), kernel.Rules())
	}
	if hold := enabled.HeldServiceHolds(desired)["fresh"]; vms.instances["fresh"] != nil || hold.Action != ActionNetworkHistoryIncomplete {
		t.Fatalf("an unrecorded port was given out after ports went untracked: %+v", hold)
	}
}

// Without host networking, a clean node records its initialization, so its
// own VM state on a later restart is not taken for earlier history.
// (Finding from the review of a3cbc29.)
func TestCleanInitializationIsDurableWithoutHostNetworking(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	routes := &fakeRoutes{boot: "boot-1", start: "100"}
	build := func(vms VMManager) *Reconciler {
		return New(vms, logger, nil, nil, "", 0).WithRouteEvidence(routes).WithStateDir(dir)
	}
	_ = build(newFakeVMManager())
	data, err := os.ReadFile(filepath.Join(dir, pendingTeardownsFile))
	if err != nil {
		t.Fatalf("clean initialization was not recorded: %v", err)
	}
	var journal persistedJournal
	if err := json.Unmarshal(data, &journal); err != nil || !journal.History.Complete {
		t.Fatalf("clean initialization is not complete: %s (%v)", data, err)
	}
	if err := os.Mkdir(filepath.Join(dir, "vms"), 0o755); err != nil { // this agent's own VMs
		t.Fatal(err)
	}
	vms := newFakeVMManager()
	restarted := build(vms)
	svc := config.ServiceConfig{Name: "fresh", Image: "/image", Kernel: "/kernel", Network: &config.NetworkConfig{GuestIP: "172.16.0.3"}}
	desired := config.NodeConfig{Services: []config.ServiceConfig{svc}}
	for tick := 0; tick < 2; tick++ {
		_ = restarted.Reconcile(context.Background(), desired)
	}
	if vms.instances[svc.Name] == nil {
		t.Fatalf("the restart took the node's own state for earlier history: %v", restarted.HeldServiceHolds(desired))
	}
}

// L1: a route file that cannot be read may route to any address, so even an
// update at the service's own address waits, keeping the old VM, and resumes
// once the file is gone.
func TestUnreadableRouteFileConservativelyHoldsAnOwnAddressUpdate(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	vms := newFakeVMManager()
	svc := config.ServiceConfig{Name: "web", Image: "/v1", Network: &config.NetworkConfig{GuestIP: "172.16.0.2"}}
	vms.instances[svc.Name] = &vm.Instance{Name: svc.Name, Config: svc, State: vm.StateRunning}
	routes := &fakeRoutes{boot: "boot-1", start: "100"}
	r := New(vms, logger, nil, nil, "", 0).WithRouteEvidence(routes).WithStateDir(t.TempDir())
	routes.unreadable = []string{"middlewares.yml"}
	updated := svc
	updated.Image = "/v2"
	desired := config.NodeConfig{Services: []config.ServiceConfig{updated}}
	_ = r.Reconcile(context.Background(), desired)
	if inst := vms.instances[svc.Name]; inst == nil || inst.Config.Image != "/v1" || r.HeldServiceHolds(desired)[svc.Name].Reason == "" {
		t.Fatalf("unreadable route evidence did not keep the VM and report the hold: %#v", inst)
	}
	routes.unreadable = nil
	for tick := 0; tick < 2; tick++ {
		_ = r.Reconcile(context.Background(), desired)
	}
	if inst := vms.instances[svc.Name]; inst == nil || inst.Config.Image != "/v2" {
		t.Fatal("the update did not resume once the file was gone")
	}
}
