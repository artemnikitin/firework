package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
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
	for _, path := range []string{"/var/lib/fw/vms/app", "/var/lib/fw state/vms/app", "/var/lib/fw\tstate/vms/app", "/var/lib/fw\nstate/vms/app", `/var/lib/fw"state/vms/app`, `/var/lib/fw\state/vms/app`, "/var/lib/fw ; argv[]=state/vms/app"} {
		t.Run(path, func(t *testing.T) {
			socket, config := path+"/firecracker.sock", path+"/vm-config.json"
			type unit struct {
				id, state, pid, job string
				argv                []string
			}
			own := unit{"firework-vm-x.service", "active", "77", "", []string{"/bin/fc", "--id", "x", "--api-sock", socket, "--config-file", config}}
			other := unit{"firework-vm-y.service", "active", "78", "", []string{"/bin/fc", "--api-sock", "/other/firecracker.sock"}}
			queued := own
			queued.state = "inactive"
			queued.pid = "0"
			queued.job = "145"
			terminal := own
			terminal.state = "inactive"
			terminal.pid = "0"
			failed := terminal
			failed.state = "failed"
			deactivating := own
			deactivating.state = "deactivating"
			equals := own
			equals.argv = []string{"/bin/fc", "--api-sock=" + socket}
			for _, tc := range []struct {
				name   string
				units  []unit
				claims bool
			}{
				{"queued start job", []unit{queued}, true}, {"active", []unit{own}, true}, {"deactivating", []unit{deactivating}, true}, {"equals form", []unit{equals}, true},
				{"inactive without job", []unit{terminal}, false}, {"failed without job", []unit{failed}, false}, {"other service", []unit{other}, false}, {"second unit queued", []unit{other, queued}, true}, {"no units", nil, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					m, _, _ := recoveryFixture(t)
					m.launcher = &systemdLauncher{systemctl: "/usr/bin/systemctl"}
					m.recoveryCommandOutput = func(_ context.Context, name string, args ...string) ([]byte, error) {
						if name == "/usr/bin/systemctl" {
							if len(args) != 3 || args[0] != "show" || args[1] != "firework-vm-*.service" || args[2] != "--property=Id,ActiveState,MainPID,Job" {
								t.Fatalf("unexpected launch query: %s %v", name, args)
							}
							var out string
							for _, u := range tc.units {
								out += fmt.Sprintf("Id=%s\nActiveState=%s\nMainPID=%s\nJob=%s\n\n", u.id, u.state, u.pid, u.job)
							}
							return []byte(out), nil
						}
						if name != "busctl" || len(args) < 3 || args[0] != "--json=short" {
							t.Fatalf("unexpected argument query: %s %v", name, args)
						}
						if args[1] == "call" {
							for index, u := range tc.units {
								if args[len(args)-1] == u.id {
									out, _ := json.Marshal(map[string]any{"type": "o", "data": []string{fmt.Sprintf("/org/freedesktop/systemd1/unit/test%d", index)}})
									return out, nil
								}
							}
						}
						if args[1] == "get-property" {
							for index, u := range tc.units {
								if args[3] == fmt.Sprintf("/org/freedesktop/systemd1/unit/test%d", index) {
									out, _ := json.Marshal(map[string]any{"type": "a(sasbttttuii)", "data": []any{[]any{"/bin/fc", u.argv, false, 0, 0, 0, 0, 0, 0, 0}}})
									return out, nil
								}
							}
						}
						t.Fatalf("unexpected argument query: %s %v", name, args)
						return nil, nil
					}
					err := m.noClaimingUnitContext(context.Background(), &instanceManifest{SocketPath: socket, ConfigPath: config})
					if (err != nil) != tc.claims {
						t.Fatalf("claim=%v, want %v", err, tc.claims)
					}
				})
			}
		})
	}
}

func TestSystemdLaunchScanFailsClosedOnUnreadableArguments(t *testing.T) {
	for _, tc := range []struct {
		name, object, property string
		failure                bool
	}{
		{name: "busctl unavailable", failure: true},
		{name: "malformed object", object: "{"},
		{name: "wrong object type", object: `{"type":"s","data":["/org/freedesktop/systemd1/unit/x"]}`},
		{name: "missing object", object: `{"type":"o","data":[]}`},
		{name: "invalid object path", object: `{"type":"o","data":["/elsewhere"]}`},
		{name: "malformed property", property: "{"},
		{name: "wrong property type", property: `{"type":"s","data":["fc --api-sock /other"]}`},
		{name: "empty command array", property: `{"type":"a(sasbttttuii)","data":[]}`},
		{name: "malformed command", property: `{"type":"a(sasbttttuii)","data":[["/fc",["/fc"]]]}`},
		{name: "flattened arguments", property: `{"type":"a(sasbttttuii)","data":[["/fc","/fc --api-sock /other",false,0,0,0,0,0,0,0]]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := recoveryFixture(t)
			m.launcher = &systemdLauncher{systemctl: "systemctl"}
			m.recoveryCommandOutput = func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name == "systemctl" {
					return []byte("Id=firework-vm-x.service\nActiveState=inactive\nMainPID=0\nJob=145\n"), nil
				}
				if tc.failure {
					return nil, errors.New("busctl unavailable")
				}
				if args[1] == "call" {
					if tc.object != "" {
						return []byte(tc.object), nil
					}
					return []byte(`{"type":"o","data":["/org/freedesktop/systemd1/unit/x"]}`), nil
				}
				return []byte(tc.property), nil
			}
			if err := m.noClaimingUnitContext(context.Background(), &instanceManifest{SocketPath: "/a", ConfigPath: "/b"}); err == nil {
				t.Fatal("unreadable unit arguments authorized absence")
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
