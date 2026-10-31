package vm

import (
	"context"
	"errors"
	"github.com/artemnikitin/firework/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecoveryPolicySurvivesRestart(t *testing.T) {
	for _, tc := range []string{"missing identity", "dead with claimant"} {
		t.Run(tc, func(t *testing.T) {
			m, f, i := recoveryFixture(t)
			m.inspector = i
			switch tc {
			case "live stopping":
				f.Lifecycle = lifecycleStopping
			case "dead stopping":
				f.Lifecycle = lifecycleStopping
				delete(i.identities, f.PID)
			case "missing identity":
				f.HostBootID = ""
				f.ProcessStart = 0
				f.ExecutableIno = 0
			default:
				delete(i.identities, f.PID)
				i.find = []processIdentity{{PID: 77}}
			}
			if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
				t.Fatal(err)
			}
			if got := m.List()[f.Service]; got == nil || got.State != StateRecoveryPending {
				t.Fatalf("restart bypassed policy: %#v", got)
			}
			if _, err := m.ClearRecoveryPending(f.Service); err == nil {
				t.Fatal("command bypassed policy")
			}
			if _, err := os.Stat(f.VMDir); err != nil {
				t.Fatal(err)
			}
			delete(m.instances, f.Service)
			if err := m.reclaimUnownedStateContext(context.Background(), f.Service, f.VMDir); err == nil {
				t.Fatal("start bypassed policy")
			}
		})
	}
}

func TestUntrackedAdoptionWriteFailureCanRetry(t *testing.T) {
	m, f, i := recoveryFixture(t)
	m.inspector = i
	m.recoveryManifestWriter = func(string, *instanceManifest) error { return errors.New("disk unavailable") }
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal("one service persistence failure blocked recovery", err)
	}
	if m.List()[f.Service] == nil {
		t.Fatal("failed adoption not tracked")
	}
	m.recoveryManifestWriter = nil
	if r, err := m.ClearRecoveryPending(f.Service); err != nil || r.Outcome != RecoveryOutcomeReadopted {
		t.Fatalf("retry: %#v %v", r, err)
	}
}

func TestRecoveryLockDeadlineDoesNotApplyActionLater(t *testing.T) {
	m, f, i := recoveryFixture(t)
	m.inspector = i
	m.addRecoveryPending(f.Service, f, "pending")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	m.mu.Lock()
	done := make(chan error, 1)
	go func() { _, err := m.clearRecoveryPending(ctx, f.Service); done <- err }()
	err := <-done
	m.mu.Unlock()
	if err == nil {
		t.Fatal("deadline not honored")
	}
	if m.List()[f.Service].State != StateRecoveryPending {
		t.Fatal("action committed after deadline")
	}
}

func TestSystemdUnitBlocksDeadPIDCleanupAndRetriesStarting(t *testing.T) {
	m, f, i := recoveryFixture(t)
	m.inspector = i
	f.Launcher = "systemd"
	f.LauncherUnit = "firework-vm-" + f.InstanceID + ".service"
	delete(i.identities, f.PID)
	m.recoveryUnitInspectorContext = func(context.Context, *instanceManifest) (int, bool, error) { return 77, false, nil }
	withoutSystemdLaunches(m)
	if err := m.proveRecoveryAbsentContext(context.Background(), f); err == nil {
		t.Fatal("active unit ignored")
	}
	m.recoveryUnitInspectorContext = func(context.Context, *instanceManifest) (int, bool, error) {
		return 0, false, errors.New("unavailable")
	}
	if err := m.proveRecoveryAbsentContext(context.Background(), f); err == nil {
		t.Fatal("unit error ignored")
	}
	m.recoveryUnitInspectorContext = func(context.Context, *instanceManifest) (int, bool, error) { return 0, true, nil }
	f.PID = 0
	f.Lifecycle = lifecycleStarting
	if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
		t.Fatal(err)
	}
	m.addRecoveryPending(f.Service, f, "interrupted start")
	if r, err := m.ClearRecoveryPending(f.Service); err != nil || r.Outcome != RecoveryOutcomeCleared {
		t.Fatalf("inactive starting unit retry: %#v %v", r, err)
	}
}

func TestDeletedExecutableAndEqualsArgumentsPreserveIdentity(t *testing.T) {
	_, f, i := recoveryFixture(t)
	v := i.identities[f.PID]
	v.Executable += " (deleted)"
	v.CommandLine = []string{"firecracker", "--id=" + f.InstanceID, "--api-sock=" + f.SocketPath, "--config-file=" + f.ConfigPath}
	i.identities[f.PID] = v
	if err := validateOwnedProcess(i, f); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.Executable, "deleted") {
		t.Fatal("fixture")
	}
}

func TestStartCannotBypassLegacyQuarantine(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	if err := os.Remove(manifestPath(f.VMDir)); err != nil {
		t.Fatal(err)
	}
	if err := m.reclaimUnownedStateContext(context.Background(), f.Service, f.VMDir); err == nil {
		t.Fatal("missing manifest silently authorized Start")
	}
}

func TestFailedPreparationAndConfigCanRetryWithoutRestart(t *testing.T) {
	m, f, _ := recoveryFixture(t)
	os.RemoveAll(f.VMDir)
	f.Config.KernelArgs = strings.Repeat("x", 4096)
	if err := m.Start(context.Background(), f.Config); err == nil {
		t.Fatal("expected config failure")
	}
	id, dir, _, err := m.beginStartContext(context.Background(), f.Config)
	if err != nil {
		t.Fatal("retry wedged", err)
	}
	m.discardStart(f.Service, id)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preparation created orphan directory: %v", err)
	}
	f.Config.Volumes = []config.VolumeConfig{{Name: "data"}}
	if err := m.Start(context.Background(), f.Config); err == nil {
		t.Fatal("expected volume failure")
	}
	id, _, _, err = m.beginStartContext(context.Background(), f.Config)
	if err != nil {
		t.Fatal("prepare retry wedged", err)
	}
	m.discardStart(f.Service, id)
}

func TestReclaimCannotDeleteOtherService(t *testing.T) {
	m, f, _ := recoveryFixture(t)
	f.Lifecycle = lifecycleStopped
	f.PID = 0
	other := filepath.Join(m.stateDir, "vms", "other")
	os.MkdirAll(other, 0755)
	if err := writeManifest(manifestPath(other), f); err != nil {
		t.Fatal(err)
	}
	if err := m.reclaimUnownedStateContext(context.Background(), "other", other); err == nil {
		t.Fatal("accepted other service manifest")
	}
	if _, err := os.Stat(f.VMDir); err != nil {
		t.Fatal("deleted victim", err)
	}
}

// withoutSystemdLaunches fakes a host whose systemd lists no Firework units.
func withoutSystemdLaunches(m *Manager) {
	m.recoveryCommandOutput = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
}

func TestSystemdLaunchScanFindsQueuedAndLiveClaimants(t *testing.T) {
	socket, config := "/var/lib/fw/vms/app/firecracker.sock", "/var/lib/fw/vms/app/vm-config.json"
	unit := func(id, state, pid, job, argv string) string {
		return "MainPID=" + pid + "\nExecStart={ path=/bin/fc ; argv[]=" + argv + " ; ignore_errors=no ; start_time=[n/a] }\nId=" + id + "\nActiveState=" + state + "\nJob=" + job + "\n"
	}
	own := "/bin/fc --id x --api-sock " + socket + " --config-file " + config
	for _, tc := range []struct {
		name   string
		output string
		claims bool
	}{
		{"queued start job", unit("firework-vm-x.service", "inactive", "0", "145", own), true},
		{"active", unit("firework-vm-x.service", "active", "77", "", own), true},
		{"deactivating", unit("firework-vm-x.service", "deactivating", "77", "", own), true},
		{"equals form", unit("firework-vm-x.service", "active", "77", "", "/bin/fc --api-sock="+socket), true},
		{"inactive without job", unit("firework-vm-x.service", "inactive", "0", "", own), false},
		{"failed without job", unit("firework-vm-x.service", "failed", "0", "", own), false},
		{"other service", unit("firework-vm-y.service", "active", "78", "", "/bin/fc --api-sock /var/lib/fw/vms/app2/firecracker.sock"), false},
		{"second unit queued", unit("firework-vm-y.service", "active", "78", "", "/bin/fc --api-sock /other") + "\n" + unit("firework-vm-x.service", "inactive", "0", "9", own), true},
		{"no units", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := recoveryFixture(t)
			m.launcher = &systemdLauncher{systemctl: "/usr/bin/systemctl"}
			m.recoveryCommandOutput = func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name != "/usr/bin/systemctl" || len(args) < 2 || args[0] != "show" || args[1] != "firework-vm-*.service" {
					t.Fatalf("unexpected launch query: %s %v", name, args)
				}
				return []byte(tc.output), nil
			}
			err := m.noClaimingUnitContext(context.Background(), &instanceManifest{SocketPath: socket, ConfigPath: config})
			if (err != nil) != tc.claims {
				t.Fatalf("claim = %v, want %v", err, tc.claims)
			}
		})
	}
}

func TestRecordedSystemdUnitWithQueuedJobIsNotAbsent(t *testing.T) {
	m, f, _ := recoveryFixture(t)
	f.Launcher = "systemd"
	f.LauncherUnit = "firework-vm-" + f.InstanceID + ".service"
	for _, tc := range []struct {
		job    string
		absent bool
	}{{"145", false}, {"", true}} {
		m.recoveryCommandOutput = func(context.Context, string, ...string) ([]byte, error) {
			return []byte("LoadState=loaded\nActiveState=inactive\nMainPID=0\nJob=" + tc.job + "\n"), nil
		}
		if _, absent, err := m.inspectRecoveryUnitContext(context.Background(), f); err != nil || absent != tc.absent {
			t.Fatalf("job %q: absent=%v err=%v, want %v", tc.job, absent, err, tc.absent)
		}
	}
}
