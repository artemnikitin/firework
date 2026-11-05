//go:build linux

package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/artemnikitin/firework/internal/network"
	"github.com/artemnikitin/firework/internal/network/networktest"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/vm"
)

// forgetRetryAgent returns an agent with the real VM manager whose web VM is
// quarantined behind a corrupt manifest, with the networking it was given
// recorded or only installed, and a journal writer that fails until restored.
func forgetRetryAgent(t *testing.T, recorded bool) (*Agent, *recoveryTestNetworkManager, func(bool), string) {
	t.Helper()
	cfg := testAgentConfig(t)
	dir := filepath.Join(cfg.StateDir, "vms", "web")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instance.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	records := ""
	if recorded {
		records = `,"allocations":[{"id":1,"service":"web","phase":"running","guest_ip":"172.16.0.2","port_forwards":[{"host_port":8080,"vm_port":80}]}]`
	}
	journal := `{"version":2,"next_id":2,"history":{"complete":true,"initialized_at":"2026-11-05T00:00:00Z"},"host_ports":{"8080":"web"}` + records + `}`
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "pending_teardowns.json"), []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New(cfg, &fakeStore{revision: "r1", data: map[string][]byte{"web": []byte("node: web\nservices: []\n")}}, testLogger())
	kernel := &recoveryTestNetworkManager{}
	kernel.kernel.Append(networktest.Rule(network.PortForwardTag("web"), 8080, "172.16.0.2", 80))
	a.reconciler = reconciler.NewWithNetworkManager(a.vmManager, testLogger(), nil, kernel, "", 0).WithStateDir(cfg.StateDir)
	if err := a.vmManager.DiscoverRecoveryState(); err != nil {
		t.Fatal(err)
	}
	if inst := a.vmManager.List()["web"]; inst == nil || inst.State != vm.StateRecoveryPending {
		t.Fatalf("setup: web is not quarantined: %+v", inst)
	}
	storage := func(healthy bool) {
		if healthy {
			a.reconciler.WithJournalWriter(func(path string, data []byte) error { return os.WriteFile(path, data, 0o600) })
		} else {
			a.reconciler.WithJournalWriter(func(string, []byte) error { return errors.New("temporary disk full") })
		}
	}
	return a, kernel, storage, dir
}

// C3/C5/C6 (B5): an incomplete clear-recovery --forget is retried by the
// running agent whichever phase failed: clearing the quarantined VM state
// (its cleanup could not be recorded) or handing its host networking to
// cleanup after the state was cleared. Each retry proves the VM absent again.
// (Finding from the review of 7a89e07.)
func TestIncompleteForgetIsRetriedWithTheRealVMManager(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("run as a non-root user so the real VM manager uses its direct launcher")
	}
	for name, recorded := range map[string]bool{
		"failure during the VM state clear":  true,
		"failure after the VM state cleared": false,
	} {
		t.Run(name, func(t *testing.T) {
			a, kernel, storage, dir := forgetRetryAgent(t, recorded)
			storage(false)
			result, err := a.forgetRecoveryPending("web")
			if err == nil || result.Outcome != vm.RecoveryOutcomeIncomplete {
				t.Fatalf("forget: %+v %v", result, err)
			}
			if !a.forgetRetries["web"] {
				t.Fatalf("the incomplete --forget was not queued for retry: %s", result.Message)
			}
			storage(true)
			for tick := 0; tick < 4; tick++ {
				a.tick(context.Background())
				a.applyRecoveryRequests(context.Background())
			}
			if inst := a.vmManager.List()["web"]; inst != nil || len(kernel.kernel.Rules()) != 0 || len(a.forgetRetries) != 0 {
				t.Fatalf("the incomplete --forget did not finish: VM=%+v rules=%v queued=%v", inst, kernel.kernel.Rules(), a.forgetRetries)
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the quarantined state was not removed: %v", err)
			}
			if again, err := a.forgetRecoveryPending("web"); err != nil || again.Outcome != vm.RecoveryOutcomeNoop {
				t.Fatalf("a repeated --forget: %+v %v", again, err)
			}
		})
	}
}

// Unavailable evidence keeps the original --forget pending through both
// phases: a quarantined manifest that cannot be read, or an absence scan
// that times out after the VM state has been cleared.
func TestRetriedForgetRecoversWhenInspectionBecomesAvailable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("run as a non-root user so the real VM manager uses its direct launcher")
	}
	for name, recorded := range map[string]bool{
		"quarantined manifest unreadable":  true,
		"untracked absence scan timed out": false,
	} {
		t.Run(name, func(t *testing.T) {
			a, kernel, storage, dir := forgetRetryAgent(t, recorded)
			storage(false)
			if result, err := a.forgetRecoveryPending("web"); err == nil || result.Outcome != vm.RecoveryOutcomeIncomplete {
				t.Fatalf("forget: %+v %v", result, err)
			}
			storage(true)
			path := filepath.Join(dir, "instance.json")
			if recorded {
				if err := os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
			} else {
				a.proveServiceAbsent = func(context.Context, string) error { return context.DeadlineExceeded }
			}
			a.tick(context.Background())
			if !a.forgetRetries["web"] || len(kernel.kernel.Rules()) != 1 {
				t.Fatalf("unavailable evidence cancelled the retry or released its rule: queued=%v rules=%v", a.forgetRetries, kernel.kernel.Rules())
			}
			if recorded {
				if inst := a.vmManager.List()["web"]; inst == nil || inst.State != vm.StateRecoveryPending {
					t.Fatalf("unavailable evidence released quarantine: %+v", inst)
				}
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				a.proveServiceAbsent = nil // Use the real process-absence proof again.
			}
			for tick := 0; tick < 4; tick++ {
				a.tick(context.Background())
				a.applyRecoveryRequests(context.Background())
			}
			if inst := a.vmManager.List()["web"]; inst != nil || len(kernel.kernel.Rules()) != 0 || len(a.forgetRetries) != 0 {
				t.Fatalf("restored inspection did not finish the original --forget: VM=%+v rules=%v queued=%v", inst, kernel.kernel.Rules(), a.forgetRetries)
			}
		})
	}
}

// A retried --forget is refused like the command when a process uses the
// VM's paths again: the state stays held, the retry ends, and it is not
// cleared on its own once the process exits, in either phase.
func TestRetriedForgetIsRefusedByALiveClaimant(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("run as a non-root user so the real VM manager uses its direct launcher")
	}
	for name, recorded := range map[string]bool{"quarantined VM": true, "untracked VM": false} {
		t.Run(name, func(t *testing.T) {
			a, kernel, storage, dir := forgetRetryAgent(t, recorded)
			storage(false)
			if result, err := a.forgetRecoveryPending("web"); err == nil || result.Outcome != vm.RecoveryOutcomeIncomplete {
				t.Fatalf("forget: %+v %v", result, err)
			}
			claimant := exec.Command("/bin/sh", "-c", "sleep 300", "fc", "--api-sock", filepath.Join(dir, "firecracker.sock"))
			if err := claimant.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = claimant.Process.Kill(); _ = claimant.Wait() })
			assertHeld := func() {
				t.Helper()
				inst := a.vmManager.List()["web"]
				if recorded && (inst == nil || inst.State != vm.StateRecoveryPending) || !recorded && inst != nil || len(a.forgetRetries) != 0 || len(kernel.kernel.Rules()) != 1 {
					t.Fatalf("live claimant's fence was released: VM=%+v queued=%v rules=%v", inst, a.forgetRetries, kernel.kernel.Rules())
				}
			}
			storage(true)
			a.tick(context.Background())
			assertHeld()
			_ = claimant.Process.Kill()
			_ = claimant.Wait()
			for tick := 0; tick < 3; tick++ {
				a.tick(context.Background())
			}
			assertHeld()
		})
	}
}
