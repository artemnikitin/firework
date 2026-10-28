package vm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/guestlifecycle"
	"github.com/artemnikitin/firework/internal/volume"
)

type guestQuiescerFunc func(context.Context, string, time.Duration) (guestlifecycle.Response, error)

func (f guestQuiescerFunc) Quiesce(ctx context.Context, path string, timeout time.Duration) (guestlifecycle.Response, error) {
	return f(ctx, path, timeout)
}

type lifecycleTestLauncher struct {
	signals []syscall.Signal
	onStop  func(syscall.Signal)
}

func (*lifecycleTestLauncher) Launch(context.Context, launchSpec) (*launchedProcess, error) {
	return nil, errors.New("not implemented")
}

func (l *lifecycleTestLauncher) Stop(_ *instanceManifest, signal syscall.Signal) error {
	l.signals = append(l.signals, signal)
	if l.onStop != nil {
		l.onStop(signal)
	}
	return nil
}

func TestRemoveForResizeKeepsLiveVMAndVolumeWhenGuestQuiesceFails(t *testing.T) {
	manager, instance, inspector, launcher, volumePath := lifecycleManagerFixture(t)
	manager.quiescer = guestQuiescerFunc(func(ctx context.Context, path string, appTimeout time.Duration) (guestlifecycle.Response, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("guest quiesce request has no overall deadline")
		}
		if path != instance.manifest.GuestVsockPath || appTimeout >= manager.guestQuiesceTimeout {
			t.Fatalf("unexpected guest quiesce parameters: path=%q app timeout=%s", path, appTimeout)
		}
		return guestlifecycle.Response{}, errors.New("application_timeout")
	})
	before, err := os.ReadFile(volumePath)
	if err != nil {
		t.Fatal(err)
	}

	err = manager.RemoveForResize(instance.Name)
	if err == nil || !strings.Contains(err.Error(), "guest quiesce required") {
		t.Fatalf("expected resize quiesce refusal, got %v", err)
	}
	if manager.List()[instance.Name].State != StateRunning {
		t.Fatalf("old VM did not return to running state: %#v", manager.List()[instance.Name])
	}
	if len(launcher.signals) != 0 {
		t.Fatalf("failed quiesce signalled Firecracker: %v", launcher.signals)
	}
	if err := validateOwnedProcess(inspector, instance.manifest); err != nil {
		t.Fatalf("old Firecracker process is no longer owned/live: %v", err)
	}
	after, err := os.ReadFile(volumePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("failed quiesce changed the old volume image: before=%q after=%q", before, after)
	}
	if _, err := os.Stat(manifestPath(instance.manifest.VMDir)); err != nil {
		t.Fatalf("failed quiesce removed VM ownership state: %v", err)
	}
}

func TestStopFallsBackToOwnedTerminationWhenOldGuestHasNoQuiesceChannel(t *testing.T) {
	manager, instance, inspector, launcher, _ := lifecycleManagerFixture(t)
	instance.manifest.GuestVsockPath = ""
	launcher.onStop = func(signal syscall.Signal) {
		if signal == syscall.SIGTERM || signal == syscall.SIGKILL {
			delete(inspector.identities, instance.manifest.PID)
		}
	}

	if err := manager.Stop(instance.Name); err != nil {
		t.Fatalf("ordinary Stop did not use the old-image fallback: %v", err)
	}
	if len(launcher.signals) != 1 || launcher.signals[0] != syscall.SIGTERM {
		t.Fatalf("unexpected fallback signals: %v", launcher.signals)
	}
	if state := manager.List()[instance.Name].State; state != StateStopped {
		t.Fatalf("ordinary stop ended in state %s", state)
	}
	if got := manager.VolumeError(instance.Name); !strings.Contains(got, "not configured") {
		t.Fatalf("old-image quiesce failure was not surfaced: %q", got)
	}
}

func TestStopPersistsGuestQuiesceBeforeSignallingFirecracker(t *testing.T) {
	manager, instance, inspector, launcher, _ := lifecycleManagerFixture(t)
	manager.quiescer = guestQuiescerFunc(func(_ context.Context, path string, appTimeout time.Duration) (guestlifecycle.Response, error) {
		if path != instance.manifest.GuestVsockPath || appTimeout <= 0 {
			t.Fatalf("unexpected quiesce request: path=%q timeout=%s", path, appTimeout)
		}
		return successfulQuiesceResponse(), nil
	})
	launcher.onStop = func(signal syscall.Signal) {
		if signal == syscall.SIGTERM {
			manifest, err := readManifest(manifestPath(instance.manifest.VMDir))
			if err != nil || !manifest.GuestQuiesced {
				t.Fatalf("Firecracker was signalled before quiesce confirmation persisted: manifest=%#v err=%v", manifest, err)
			}
			delete(inspector.identities, instance.manifest.PID)
		}
	}

	if err := manager.Stop(instance.Name); err != nil {
		t.Fatal(err)
	}
	if len(launcher.signals) != 1 || launcher.signals[0] != syscall.SIGTERM {
		t.Fatalf("unexpected stop signals: %v", launcher.signals)
	}
	if manager.List()[instance.Name].State != StateStopped {
		t.Fatalf("guest quiesce did not stop VM: %#v", manager.List()[instance.Name])
	}
	if got := manager.VolumeError(instance.Name); got != "" {
		t.Fatalf("successful guest quiesce left a volume error: %q", got)
	}
}

func TestStopNeverSignalsAReplacementProcessAfterQuiesce(t *testing.T) {
	manager, instance, inspector, launcher, _ := lifecycleManagerFixture(t)
	manager.quiescer = guestQuiescerFunc(func(_ context.Context, _ string, _ time.Duration) (guestlifecycle.Response, error) {
		identity := inspector.identities[instance.manifest.PID]
		identity.StartTicks++
		inspector.identities[instance.manifest.PID] = identity
		return successfulQuiesceResponse(), nil
	})

	err := manager.Stop(instance.Name)
	if err == nil || !strings.Contains(err.Error(), "after guest quiesce because ownership changed") {
		t.Fatalf("expected ownership change to block termination: %v", err)
	}
	if len(launcher.signals) != 0 {
		t.Fatalf("replacement process received signals: %v", launcher.signals)
	}
	if state := manager.List()[instance.Name].State; state != StateRecoveryPending {
		t.Fatalf("identity change was not quarantined: %s", state)
	}
}

func TestStopRevalidatesOwnershipBeforeSIGKILL(t *testing.T) {
	manager, instance, inspector, launcher, _ := lifecycleManagerFixture(t)
	manager.quiescer = guestQuiescerFunc(func(_ context.Context, _ string, _ time.Duration) (guestlifecycle.Response, error) {
		return successfulQuiesceResponse(), nil
	})
	launcher.onStop = func(signal syscall.Signal) {
		if signal == syscall.SIGTERM {
			identity := inspector.identities[instance.manifest.PID]
			identity.StartTicks++
			inspector.identities[instance.manifest.PID] = identity
			// Model a launcher error so Stop takes its immediate SIGKILL path.
			launcher.onStop = nil
		}
	}
	launcherError := &identityChangingErrorLauncher{lifecycleTestLauncher: launcher}
	manager.launcher = launcherError

	err := manager.Stop(instance.Name)
	if err == nil || !strings.Contains(err.Error(), "with SIGKILL because ownership changed") {
		t.Fatalf("expected changed identity to block SIGKILL: %v", err)
	}
	if len(launcher.signals) != 1 || launcher.signals[0] != syscall.SIGTERM {
		t.Fatalf("unexpected signals after identity changed: %v", launcher.signals)
	}
}

type identityChangingErrorLauncher struct {
	*lifecycleTestLauncher
}

func (l *identityChangingErrorLauncher) Stop(manifest *instanceManifest, signal syscall.Signal) error {
	_ = l.lifecycleTestLauncher.Stop(manifest, signal)
	if signal == syscall.SIGTERM {
		return errors.New("simulated TERM launcher error")
	}
	return nil
}

func TestWriteVMConfigIncludesFirecrackerVsockDevice(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager("/bin/true", dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	path, err := manager.writeVMConfig(dir, config.ServiceConfig{
		Name: "app", Image: "/root.ext4", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg firecrackerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Vsock.GuestCID != 3 || cfg.Vsock.UDSPath != filepath.Join(dir, "guest.vsock") {
		t.Fatalf("unexpected guest vsock config: %#v", cfg.Vsock)
	}
}

func successfulQuiesceResponse() guestlifecycle.Response {
	return guestlifecycle.Response{
		Version: guestlifecycle.Version, Operation: guestlifecycle.QuiesceOperation,
		AppExited: true, SyncDone: true,
		Mounts: []guestlifecycle.MountResult{{Name: "data", Path: "/data", Unmounted: true}},
	}
}

func lifecycleManagerFixture(t *testing.T) (*Manager, *Instance, *fakeProcessInspector, *lifecycleTestLauncher, string) {
	t.Helper()
	stateDir := t.TempDir()
	vmDir := filepath.Join(stateDir, "vms", "app")
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	volumePath := filepath.Join(stateDir, "volume.img")
	if err := os.WriteFile(volumePath, []byte("old-volume-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := config.ServiceConfig{
		Name: "app", Image: "/root.ext4", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128,
		Volumes: []config.VolumeConfig{{Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data", SizeBytes: 8 * config.MiB}},
	}
	hash, err := serviceConfigHash(service)
	if err != nil {
		t.Fatal(err)
	}
	manifest := &instanceManifest{
		SchemaVersion: manifestSchemaVersion, Service: service.Name, InstanceID: "test-instance",
		Lifecycle: lifecycleRunning, Config: service, ConfigHash: hash,
		PID: 1234, HostBootID: "boot", ProcessStart: 11, Executable: "/usr/bin/firecracker",
		ExecutableDev: 1, ExecutableIno: 2,
		SocketPath:     filepath.Join(vmDir, "firecracker.sock"),
		GuestVsockPath: filepath.Join(vmDir, "guest.vsock"),
		ConfigPath:     filepath.Join(vmDir, "vm-config.json"), VMDir: vmDir,
		Launcher: "direct", StartedAt: time.Now().UTC(),
		Volumes: []volume.PreparedVolume{{
			LogicalID: "app/data", PathOnHost: volumePath, MountPath: "/data",
			Type: config.VolumeTypeLocal, SizeBytes: 8 * config.MiB,
		}},
	}
	if err := writeManifest(manifestPath(vmDir), manifest); err != nil {
		t.Fatal(err)
	}
	identity := processIdentity{
		PID: manifest.PID, HostBootID: manifest.HostBootID, StartTicks: manifest.ProcessStart,
		Executable: manifest.Executable, ExecutableDev: manifest.ExecutableDev, ExecutableIno: manifest.ExecutableIno,
		CommandLine: []string{manifest.Executable, "--id", manifest.InstanceID, "--api-sock", manifest.SocketPath, "--config-file", manifest.ConfigPath},
	}
	inspector := &fakeProcessInspector{identities: map[int]processIdentity{manifest.PID: identity}}
	manager := NewManager("/usr/bin/firecracker", stateDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager.inspector = inspector
	launcher := &lifecycleTestLauncher{}
	manager.launcher = launcher
	instance := instanceFromManifest(manifest, StateRunning, "")
	manager.instances[service.Name] = instance
	return manager, instance, inspector, launcher, volumePath
}
