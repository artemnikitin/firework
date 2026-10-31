package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

func systemdRecoveryOutput(state string) []byte {
	return []byte("LoadState=loaded\nActiveState=" + state + "\nMainPID=0\n")
}

func TestTrackedSystemdExitRestartDoesNotNeedCleanupJournalOrScan(t *testing.T) {
	for _, writeFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminal persisted", true: "terminal write failed"}[writeFailure], func(t *testing.T) {
			m, f, inspector := recoveryFixture(t)
			delete(inspector.identities, f.PID)
			inspector.findErr = errors.New("process scan unavailable")
			m.inspector = inspector
			f.Launcher = "systemd"
			f.LauncherUnit = "firework-vm-" + f.InstanceID + ".service"
			if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
				t.Fatal(err)
			}
			m.instances[f.Service] = instanceFromManifest(cloneInstanceManifest(f), StateRunning, "")
			m.SetRecoveryCleanupRecorder(func(config.ServiceConfig) error { t.Fatal("normal restart wrote recovery cleanup intent"); return nil })
			m.recoveryCommandOutput = func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name != "systemctl" || !reflect.DeepEqual(args, []string{"show", f.LauncherUnit, "--property=LoadState,ActiveState,MainPID,Job"}) {
					t.Fatalf("unexpected unit query: %s %v", name, args)
				}
				return systemdRecoveryOutput("inactive"), nil
			}
			if writeFailure {
				m.recoveryManifestWriter = func(path string, f *instanceManifest) error {
					if f.PID == 0 {
						return errors.New("stopped manifest write unavailable")
					}
					return writeManifest(path, f)
				}
			}
			stopErr := m.Stop(f.Service)
			if (stopErr != nil) != writeFailure {
				t.Fatalf("Stop = %v", stopErr)
			}
			if m.IsRunning(f.Service) {
				t.Fatal("exited VM reports running")
			}
			id, _, _, err := m.beginStartContext(context.Background(), f.Config)
			if err != nil {
				t.Fatalf("confirmed systemd exit did not restart: %v", err)
			}
			m.discardStart(f.Service, id)
		})
	}
}

func TestFailedSystemdReclaimRemainsPendingAndRecovers(t *testing.T) {
	for _, failure := range []string{"unit unavailable", "deactivating"} {
		t.Run(failure, func(t *testing.T) {
			m, f, inspector := recoveryFixture(t)
			delete(inspector.identities, f.PID)
			m.inspector = inspector
			f.Launcher = "systemd"
			f.LauncherUnit = "firework-vm-" + f.InstanceID + ".service"
			if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
				t.Fatal(err)
			}
			m.instances[f.Service] = instanceFromManifest(cloneInstanceManifest(f), StateRunning, "")
			// Keep the running durable manifest to cover a failed final stopped write.
			m.recoveryManifestWriter = func(path string, f *instanceManifest) error {
				if f.PID == 0 {
					return errors.New("stopped manifest write failed")
				}
				return writeManifest(path, f)
			}
			if err := m.Stop(f.Service); err == nil {
				t.Fatal("missing terminal write error")
			}
			m.recoveryManifestWriter = nil
			m.recoveryCommandOutput = func(context.Context, string, ...string) ([]byte, error) {
				if failure == "unit unavailable" {
					return nil, errors.New("systemctl failed")
				}
				return systemdRecoveryOutput("deactivating"), nil
			}
			if _, _, _, err := m.beginStartContext(context.Background(), f.Config); err == nil {
				t.Fatal("unverified unit reclaimed")
			}
			pending := m.List()[f.Service]
			if pending == nil || pending.State != StateRecoveryPending || pending.RecoveryConfigUnknown {
				t.Fatalf("failed reclaim lost known retryable ownership: %#v", pending)
			}
			if _, err := os.Stat(f.VMDir); err != nil {
				t.Fatal(err)
			}
			m.recoveryCommandOutput = func(context.Context, string, ...string) ([]byte, error) {
				return systemdRecoveryOutput("inactive"), nil
			}
			recovered, err := m.Recover(context.Background(), config.NodeConfig{})
			if err != nil || len(recovered) != 1 || m.List()[f.Service] != nil {
				t.Fatalf("same-revision retry failed: %v %v %#v", recovered, err, m.List()[f.Service])
			}
			id, _, _, err := m.beginStartContext(context.Background(), f.Config)
			if err != nil {
				t.Fatalf("start remained wedged: %v", err)
			}
			m.discardStart(f.Service, id)
		})
	}
}

func TestFailedUntrackedReclaimRetainsCleanupJournalRetry(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	delete(inspector.identities, f.PID)
	m.inspector = inspector
	f.PID = 0
	f.Lifecycle = lifecycleStopped
	if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
		t.Fatal(err)
	}
	m.SetRecoveryCleanupRecorder(func(config.ServiceConfig) error { return errors.New("cleanup journal unavailable") })
	if _, _, _, err := m.beginStartContext(context.Background(), f.Config); err == nil {
		t.Fatal("failed journal ignored")
	}
	if got := m.List()[f.Service]; got == nil || got.State != StateRecoveryPending {
		t.Fatalf("lost retry: %#v", got)
	}
	m.SetRecoveryCleanupRecorder(func(config.ServiceConfig) error { return nil })
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if m.List()[f.Service] != nil {
		t.Fatal("journal recovery did not clear pending state")
	}
}

func TestStartAbsenceProofUsesCallerDeadline(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	delete(inspector.identities, f.PID)
	m.inspector = inspector
	f.PID = 0
	f.Lifecycle = lifecycleStopped
	f.Launcher = "systemd"
	f.LauncherUnit = "firework-vm-" + f.InstanceID + ".service"
	if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
		t.Fatal(err)
	}
	m.recoveryCommandOutput = func(ctx context.Context, _ string, _ ...string) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	began := time.Now()
	if err := m.Start(ctx, f.Config); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start = %v", err)
	}
	if time.Since(began) > time.Second {
		t.Fatal("absence proof escaped caller deadline")
	}
	if got := m.List()[f.Service]; got == nil || got.State != StateRecoveryPending {
		t.Fatalf("deadline lost durable ownership: %#v", got)
	}
}

func TestRecoveryInventoriesAllDirectoriesBeforeFirstProbe(t *testing.T) {
	m, original, inspector := recoveryFixture(t)
	delete(inspector.identities, original.PID)
	m.inspector = inspector
	if err := os.RemoveAll(original.VMDir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "z"} {
		f := cloneInstanceManifest(original)
		f.Service = name
		f.Config.Name = name
		f.Config.MemoryMB = 321
		f.ConfigHash, _ = serviceConfigHash(f.Config)
		f.VMDir = filepath.Join(m.stateDir, "vms", name)
		f.SocketPath = filepath.Join(f.VMDir, "firecracker.sock")
		f.ConfigPath = filepath.Join(f.VMDir, "vm-config.json")
		f.InstanceID = name + "-instance"
		f.Launcher = "systemd"
		f.LauncherUnit = "firework-vm-" + f.InstanceID + ".service"
		if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
			t.Fatal(err)
		}
	}
	unknown := filepath.Join(m.stateDir, "vms", "unknown")
	if err := os.MkdirAll(unknown, 0755); err != nil {
		t.Fatal(err)
	}
	m.recoveryCommandOutput = func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		got := m.List()
		for _, name := range []string{"a", "b", "z"} {
			if instance := got[name]; instance == nil || instance.State != StateRecoveryPending || instance.Config.MemoryMB != 321 || instance.RecoveryConfigUnknown {
				t.Errorf("probe started before inventory for %s: %#v", name, instance)
			}
		}
		if instance := got["unknown"]; instance == nil || !instance.RecoveryConfigUnknown {
			t.Errorf("unknown metadata was not fenced: %#v", instance)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := m.DiscoverRecoveryState(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := m.Recover(ctx, config.NodeConfig{}); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if got := m.List()["z"]; got == nil || got.State != StateRecoveryPending {
		t.Fatalf("budget exhaustion left last VM invisible: %#v", got)
	}
}

func TestConcurrentRecoverStartStopAndClear(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	delete(inspector.identities, f.PID)
	m.inspector = inspector
	m.launcher = &recordingLauncher{}
	f.PID = 0
	f.Lifecycle = lifecycleStopped
	if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			for range 20 {
				_, _ = m.Recover(context.Background(), config.NodeConfig{})
				_ = m.Start(context.Background(), f.Config)
				_ = m.Stop(f.Service)
				_, _ = m.ClearRecoveryPending(f.Service)
			}
		}()
	}
	group.Wait()
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if got := m.List()[f.Service]; got != nil && got.State == StateRecoveryPending {
		t.Fatalf("racing operations left recoverable state wedged: %#v", got)
	}
}

func TestSystemdMonitorConfirmedExitRestartsAfterTerminalWriteFailure(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	delete(inspector.identities, f.PID)
	inspector.findErr = errors.New("host scan unavailable")
	m.inspector = inspector
	f.Launcher = "systemd"
	f.LauncherUnit = "firework-vm-" + f.InstanceID + ".service"
	if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
		t.Fatal(err)
	}
	m.instances[f.Service] = instanceFromManifest(cloneInstanceManifest(f), StateRunning, "")
	m.recoveryManifestWriter = func(string, *instanceManifest) error { return errors.New("terminal write unavailable") }
	done := make(chan struct{})
	go func() { m.monitorAdopted(f.Service, f.InstanceID); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not record exited owner")
	}
	if got := m.List()[f.Service]; got.State != StateFailed || got.exitedOwner == nil {
		t.Fatalf("monitor lost confirmed exit: %#v", got)
	}
	m.recoveryCommandOutput = func(context.Context, string, ...string) ([]byte, error) {
		return systemdRecoveryOutput("inactive"), nil
	}
	m.SetRecoveryCleanupRecorder(func(config.ServiceConfig) error { t.Fatal("routine restart wrote cleanup journal"); return nil })
	id, _, _, err := m.beginStartContext(context.Background(), f.Config)
	if err != nil {
		t.Fatalf("monitor-observed exit did not restart: %v", err)
	}
	m.discardStart(f.Service, id)
}
