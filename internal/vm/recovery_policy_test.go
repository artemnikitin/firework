package vm

import (
	"context"
	"errors"
	"github.com/artemnikitin/firework/internal/config"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRecoveryPolicySurvivesRestart(t *testing.T) {
	for _, tc := range []string{"live stopping", "dead stopping", "missing identity", "dead with claimant"} {
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
			if err := m.reclaimUnownedState(f.Service, f.VMDir); err == nil {
				t.Fatal("start bypassed policy")
			}
		})
	}
}

func TestUntrackedAdoptionWriteFailureCanRetry(t *testing.T) {
	m, f, i := recoveryFixture(t)
	m.inspector = i
	m.recoveryManifestWriter = func(string, *instanceManifest) error { return errors.New("disk unavailable") }
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err == nil {
		t.Fatal("missing persistence failure")
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
	m.recoveryUnitInspector = func(*instanceManifest) (int, bool, error) { return 77, false, nil }
	if err := m.proveRecoveryAbsent(f); err == nil {
		t.Fatal("active unit ignored")
	}
	m.recoveryUnitInspector = func(*instanceManifest) (int, bool, error) { return 0, false, errors.New("unavailable") }
	if err := m.proveRecoveryAbsent(f); err == nil {
		t.Fatal("unit error ignored")
	}
	m.recoveryUnitInspector = func(*instanceManifest) (int, bool, error) { return 0, true, nil }
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
	m, f, _ := recoveryFixture(t)
	if err := os.Remove(manifestPath(f.VMDir)); err != nil {
		t.Fatal(err)
	}
	if err := m.reclaimUnownedState(f.Service, f.VMDir); err == nil {
		t.Fatal("missing manifest silently authorized Start")
	}
}
