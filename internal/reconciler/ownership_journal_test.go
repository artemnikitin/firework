package reconciler

import (
	"context"
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

func journalReconciler(t *testing.T, vms VMManager, kernel *networktest.Kernel, dir string) *Reconciler {
	t.Helper()
	return NewWithNetworkManager(vms, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, kernel, "", 0).WithStateDir(dir)
}

// Ownership history is complete only on a host with no earlier firework
// state: no journal, no VM state directory and no tagged rule. Anything else
// is migrated, and its history stays incomplete across restarts.
func TestJournalInitializationRecordsWhetherHistoryIsComplete(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare  func(t *testing.T, dir string, kernel *networktest.Kernel)
		complete bool
	}{
		"clean host": {func(*testing.T, string, *networktest.Kernel) {}, true},
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
	if len(remaining) != 4 {
		// web's untagged rule, web's tagged re-assertion, and both rules no
		// record covers.
		t.Fatalf("rules after migration: %v", remaining)
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
