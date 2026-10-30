package vm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

type round4NoScanInspector struct{ *round3DirectInspector }

func (*round4NoScanInspector) FindByAnyArgument(string, string) ([]processIdentity, error) {
	panic("tracked reaped child restart must not scan the process table")
}

func TestTrackedDirectChildRestartsAfterTerminalManifestWriteFailure(t *testing.T) {
	for _, stop := range []bool{true, false} {
		t.Run(map[bool]string{true: "Stop", false: "Wait"}[stop], func(t *testing.T) {
			stateDir := t.TempDir()
			binary := filepath.Join(stateDir, "firecracker")
			if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec sleep 30\n"), 0755); err != nil {
				t.Fatal(err)
			}
			m := NewManager(binary, stateDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
			launcher := &specCapturingLauncher{inner: &directLauncher{binary: binary}}
			m.launcher = launcher
			m.inspector = &round4NoScanInspector{&round3DirectInspector{launchedSpecInspector: &launchedSpecInspector{launcher: launcher, executable: binary}}}
			service := config.ServiceConfig{Name: "app", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}
			if err := m.Start(context.Background(), service); err != nil {
				t.Fatal(err)
			}
			old := m.List()[service.Name]
			t.Cleanup(func() {
				for _, instance := range m.List() {
					if instance.PID > 0 {
						_ = syscall.Kill(instance.PID, syscall.SIGKILL)
					}
				}
				deadline := time.Now().Add(time.Second)
				for time.Now().Before(deadline) {
					if got := m.List()[service.Name]; got == nil || got.PID == 0 {
						return
					}
					time.Sleep(time.Millisecond)
				}
				t.Error("real child was not reaped")
			})
			m.mu.Lock()
			m.recoveryManifestWriter = func(path string, f *instanceManifest) error {
				if f.PID == 0 {
					return errors.New("terminal manifest write failed")
				}
				return writeManifest(path, f)
			}
			m.mu.Unlock()
			if stop {
				if err := m.Stop(service.Name); err == nil {
					t.Fatal("Stop did not report final write failure")
				}
			} else if err := syscall.Kill(old.PID, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			for {
				got := m.List()[service.Name]
				if got.PID == 0 && (got.State == StateStopped || got.State == StateFailed) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("Wait did not record confirmed child exit: %#v", got)
				}
				time.Sleep(time.Millisecond)
			}
			m.mu.Lock()
			m.recoveryManifestWriter = nil
			m.mu.Unlock()
			if err := m.Start(context.Background(), service); err != nil {
				t.Fatalf("known exited child blocked restart: %v", err)
			}
			replacement := m.List()[service.Name]
			if old.startID == "" || replacement.startID == "" || replacement.instanceID == old.instanceID || replacement.PID == old.PID {
				t.Fatal("restart did not create a new real child")
			}
		})
	}
}

func TestSystemdLaunchWithoutMainPIDRemainsTrackedAndRecovers(t *testing.T) {
	stateDir := t.TempDir()
	binary := filepath.Join(stateDir, "firecracker")
	run := filepath.Join(stateDir, "systemd-run")
	ctl := filepath.Join(stateDir, "systemctl")
	for _, path := range []string{run, ctl} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho 0\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	m := NewManager(binary, stateDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.launcher = &systemdLauncher{binary: binary, systemdRun: run, systemctl: ctl, pollInterval: time.Millisecond}
	service := config.ServiceConfig{Name: "app", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := m.Start(ctx, service); err == nil {
		t.Fatal("expected MainPID timeout")
	}
	pending := m.List()[service.Name]
	if pending == nil || pending.State != StateRecoveryPending || pending.PID != 0 || pending.manifest.Launcher != "systemd" {
		t.Fatalf("unproved launch disappeared from tracked state: %#v", pending)
	}
	f := pending.manifest
	i := &fakeProcessInspector{identities: map[int]processIdentity{42: {PID: 42, HostBootID: "boot", StartTicks: 5, Executable: binary, ExecutableDev: 1, ExecutableIno: 2, CommandLine: []string{binary, "--id", f.InstanceID, "--api-sock", f.SocketPath, "--config-file", f.ConfigPath}}}, errors: make(map[int]error)}
	m.inspector = i
	m.recoveryUnitInspectorContext = func(context.Context, *instanceManifest) (int, bool, error) { return 42, false, nil }
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	stopAdoptedMonitors(t, m)
	if got := m.List()[service.Name]; got == nil || got.State != StateRunning || got.PID != 42 {
		t.Fatalf("launch did not recover once MainPID appeared: %#v", got)
	}
}

func TestRecoveryDeadlineAndCursorDoNotStarveReadyServices(t *testing.T) {
	m, original, i := recoveryFixture(t)
	m.inspector = i
	delete(i.identities, original.PID)
	if err := os.RemoveAll(original.VMDir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "z"} {
		f := *cloneInstanceManifest(original)
		f.Service = name
		f.Config.Name = name
		f.InstanceID = name + "-instance"
		f.ConfigHash, _ = serviceConfigHash(f.Config)
		f.VMDir = filepath.Join(m.stateDir, "vms", name)
		f.SocketPath = filepath.Join(f.VMDir, "firecracker.sock")
		f.ConfigPath = filepath.Join(f.VMDir, "vm-config.json")
		f.Launcher = "systemd"
		f.LauncherUnit = "firework-vm-" + f.InstanceID + ".service"
		f.Lifecycle = lifecycleStarting
		f.PID = 0
		if name == "z" {
			f.PID = 42
			f.Lifecycle = lifecycleRunning
			i.identities[42] = processIdentity{PID: 42, HostBootID: f.HostBootID, StartTicks: f.ProcessStart, Executable: f.Executable, ExecutableDev: f.ExecutableDev, ExecutableIno: f.ExecutableIno, CommandLine: []string{f.Executable, "--id", f.InstanceID, "--api-sock", f.SocketPath, "--config-file", f.ConfigPath}}
		}
		if err := writeManifest(manifestPath(f.VMDir), &f); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	m.recoveryUnitInspectorContext = func(ctx context.Context, _ *instanceManifest) (int, bool, error) {
		calls++
		<-ctx.Done()
		return 0, false, ctx.Err()
	}
	for range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		began := time.Now()
		_, err := m.Recover(ctx, config.NodeConfig{})
		cancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		if time.Since(began) > time.Second {
			t.Fatal("unit inspection escaped recovery context")
		}
	}
	stopAdoptedMonitors(t, m)
	if calls < 2 {
		t.Fatal("fixture did not exercise blocked unit retries")
	}
	if got := m.List()["z"]; got == nil || got.State != StateRunning {
		t.Fatalf("slow earlier services starved ready survivor: %#v", got)
	}
}

func TestLegacyRecoveryUsesOriginalLaunchPathAfterBinarySymlinkUpgrade(t *testing.T) {
	m, f, i := recoveryFixture(t)
	m.inspector = i
	if err := os.Remove(manifestPath(f.VMDir)); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(m.stateDir, "firecracker-old")
	newer := filepath.Join(m.stateDir, "firecracker-new")
	link := filepath.Join(m.stateDir, "firecracker")
	for _, path := range []string{old, newer} {
		if err := os.WriteFile(path, []byte("binary"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(newer, link); err != nil {
		t.Fatal(err)
	}
	m.firecrackerBin = link
	identity := i.identities[f.PID]
	identity.Executable = old + " (deleted)"
	identity.CommandLine = []string{link, "--api-sock", f.SocketPath, "--config-file", f.ConfigPath}
	i.identities[f.PID] = identity
	i.find = []processIdentity{identity}
	if _, err := m.Recover(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{f.Config}}); err != nil {
		t.Fatal(err)
	}
	stopAdoptedMonitors(t, m)
	if got := m.List()[f.Service]; got == nil || got.State != StateRunning || got.manifest.Executable != identity.Executable {
		t.Fatalf("upgrade hid surviving legacy VM: %#v", got)
	}
}

func TestRecoveryPersistenceDoesNotBlockListOrLoseChildExit(t *testing.T) {
	for _, exit := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "exited"}[exit], func(t *testing.T) {
			dir := t.TempDir()
			binary := filepath.Join(dir, "firecracker")
			if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec sleep 30\n"), 0755); err != nil {
				t.Fatal(err)
			}
			m := NewManager(binary, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
			launcher := &specCapturingLauncher{inner: &directLauncher{binary: binary}}
			m.launcher = launcher
			m.inspector = &round3DirectInspector{launchedSpecInspector: &launchedSpecInspector{launcher: launcher, executable: binary}}
			svc := config.ServiceConfig{Name: "app", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}
			if err := m.Start(context.Background(), svc); err != nil {
				t.Fatal(err)
			}
			started := m.List()[svc.Name]
			defer func() {
				_ = syscall.Kill(started.PID, syscall.SIGKILL)
				deadline := time.Now().Add(time.Second)
				for time.Now().Before(deadline) {
					if m.List()[svc.Name].PID == 0 {
						return
					}
					time.Sleep(time.Millisecond)
				}
				t.Error("child monitor did not finish")
			}()
			m.quarantine(svc.Name, started.manifest, errors.New("temporary identity error"))
			entered, release := make(chan struct{}), make(chan struct{})
			m.recoveryManifestWriter = func(path string, f *instanceManifest) error {
				if f.Lifecycle == lifecycleRunning && f.PID > 0 {
					close(entered)
					<-release
				}
				return writeManifest(path, f)
			}
			done := make(chan error, 1)
			go func() { _, err := m.ClearRecoveryPending(svc.Name); done <- err }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("adoption did not reach persistence")
			}
			listed := make(chan struct{})
			go func() { m.List(); close(listed) }()
			select {
			case <-listed:
			case <-time.After(time.Second):
				close(release)
				t.Fatal("persistence blocked global List")
			}
			if exit {
				if err := syscall.Kill(started.PID, syscall.SIGKILL); err != nil {
					close(release)
					t.Fatal(err)
				}
				deadline := time.Now().Add(time.Second)
				for !errors.Is(syscall.Kill(started.PID, 0), syscall.ESRCH) {
					if time.Now().After(deadline) {
						close(release)
						t.Fatal("Wait did not reap child during persistence")
					}
					time.Sleep(time.Millisecond)
				}
			}
			close(release)
			select {
			case err := <-done:
				if exit && err == nil {
					t.Fatal("adopted exited child")
				}
				if !exit && err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("recovery did not finish")
			}
			deadline := time.Now().Add(time.Second)
			for exit && m.List()[svc.Name].PID != 0 {
				if time.Now().After(deadline) {
					t.Fatal("Wait lost exit after reservation")
				}
				time.Sleep(time.Millisecond)
			}
			if exit && m.List()[svc.Name].State != StateRecoveryPending {
				t.Fatal("exit lost retry eligibility")
			}
		})
	}
}

func TestStartReclaimsInterruptedPreparationWithoutRecoveryTick(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	delete(inspector.identities, f.PID)
	if err := os.Remove(manifestPath(f.VMDir)); err != nil {
		t.Fatal(err)
	}
	id, _, _, err := m.beginStartContext(context.Background(), f.Config)
	if err != nil {
		t.Fatalf("empty preparation required another tick: %v", err)
	}
	m.discardStart(f.Service, id)
	if _, err := os.Stat(f.VMDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial directory remains: %v", err)
	}
}

// quarantine is fixture-only; production quarantine uses guarded lifecycle paths.
func (m *Manager) quarantine(name string, manifest *instanceManifest, err error) {
	m.mu.Lock()
	manifest.LastError = err.Error()
	_ = m.writeRecoveryManifest(manifestPath(manifest.VMDir), manifest)
	if inst := m.instances[name]; inst != nil {
		inst.State = StateRecoveryPending
		inst.LastError = err.Error()
	}
	m.mu.Unlock()
	m.logRecoveryIntervention(name, err)
}
