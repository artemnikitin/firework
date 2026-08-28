package vm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/volume"
)

func TestManagerClearsPIDAndRecordsProcessFailure(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-firecracker")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(binary, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// This test asserts failure bookkeeping, not launcher selection. Pin the
	// direct launcher so it never depends on whether the host running the suite
	// can create systemd transient units, and report the launched identity from
	// the recorded spec so the start does not depend on host /proc support.
	launcher := &specCapturingLauncher{inner: &directLauncher{binary: binary}}
	manager.launcher = launcher
	manager.inspector = &launchedSpecInspector{launcher: launcher, executable: binary}
	if err := manager.Start(context.Background(), config.ServiceConfig{Name: "service", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}); err != nil {
		t.Fatal(err)
	}

	// Process reaping can be delayed when the full suite is running in parallel,
	// especially under the race detector. Keep polling rather than making the
	// assertion depend on host load.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		instance := manager.List()["service"]
		if instance != nil && instance.State == StateFailed {
			if instance.PID != 0 {
				t.Fatalf("failed instance retained exited PID %d", instance.PID)
			}
			if instance.LastError == "" {
				t.Fatal("failed instance did not retain a process error")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("instance did not transition to failed: %#v", manager.List()["service"])
}

// specCapturingLauncher records the launch spec so a test inspector can report
// the identity of the process that spec describes.
type specCapturingLauncher struct {
	inner processLauncher
	mu    sync.Mutex
	spec  launchSpec
	stops int
}

func (l *specCapturingLauncher) Launch(ctx context.Context, spec launchSpec) (*launchedProcess, error) {
	l.mu.Lock()
	l.spec = spec
	l.mu.Unlock()
	return l.inner.Launch(ctx, spec)
}

func (l *specCapturingLauncher) Stop(manifest *instanceManifest, signal syscall.Signal) error {
	l.mu.Lock()
	l.stops++
	l.mu.Unlock()
	return l.inner.Stop(manifest, signal)
}

func (l *specCapturingLauncher) launchedSpec() launchSpec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.spec
}

type launchedSpecInspector struct {
	launcher   *specCapturingLauncher
	executable string
}

func (i *launchedSpecInspector) Inspect(pid int) (processIdentity, error) {
	spec := i.launcher.launchedSpec()
	if spec.InstanceID == "" {
		return processIdentity{}, errProcessNotFound
	}
	return processIdentity{
		PID: pid, HostBootID: "boot", StartTicks: 1, Executable: i.executable,
		ExecutableDev: 1, ExecutableIno: 1,
		CommandLine: []string{i.executable, "--id", spec.InstanceID, "--api-sock", spec.SocketPath, "--config-file", spec.ConfigPath},
	}, nil
}

func (i *launchedSpecInspector) FindByArguments(string, string) ([]processIdentity, error) {
	return nil, nil
}

func (*launchedSpecInspector) SocketReady(string) error { return nil }

// execRacingLauncher launches nothing and reports a PID the way systemd reports
// MainPID at fork: before the child has exec'd its command.
type execRacingLauncher struct {
	pid       int
	launchErr error
	onStop    func()
}

func (l *execRacingLauncher) Launch(_ context.Context, _ launchSpec) (*launchedProcess, error) {
	if l.launchErr != nil {
		return nil, l.launchErr
	}
	return &launchedProcess{PID: l.pid, Launcher: "systemd", Unit: "firework-vm-test.service"}, nil
}

func (l *execRacingLauncher) Stop(*instanceManifest, syscall.Signal) error {
	if l.onStop != nil {
		l.onStop()
	}
	return nil
}

// execRacingInspector reports the launcher's own identity until the exec is
// declared complete, which is exactly what a systemd MainPID looks like while
// the transient unit is still forking into Firecracker.
type execRacingInspector struct {
	launcher *specCapturingLauncher
	pid      int
	preExec  int
	binary   string

	// The adopted-process monitor inspects on its own goroutine, so the call
	// counter and the exit flag are shared state.
	mu          sync.Mutex
	inspections int
	exited      bool
}

func (i *execRacingInspector) exit() {
	i.mu.Lock()
	i.exited = true
	i.mu.Unlock()
}

func (i *execRacingInspector) Inspect(pid int) (processIdentity, error) {
	i.mu.Lock()
	exited := i.exited
	i.inspections++
	inspections := i.inspections
	i.mu.Unlock()
	if exited || pid != i.pid {
		return processIdentity{}, errProcessNotFound
	}
	if inspections <= i.preExec {
		return processIdentity{
			PID: pid, HostBootID: "boot", StartTicks: 41143, Executable: "/usr/lib/systemd/systemd",
			ExecutableDev: 66306, ExecutableIno: 8581641,
			CommandLine: []string{"/usr/lib/systemd/systemd"},
		}, nil
	}
	spec := i.launcher.launchedSpec()
	return processIdentity{
		PID: pid, HostBootID: "boot", StartTicks: 41143, Executable: i.binary,
		ExecutableDev: 66306, ExecutableIno: 473725,
		CommandLine: []string{i.binary, "--id", spec.InstanceID, "--api-sock", spec.SocketPath, "--config-file", spec.ConfigPath},
	}, nil
}

func (i *execRacingInspector) FindByArguments(string, string) ([]processIdentity, error) {
	return nil, nil
}

func (*execRacingInspector) SocketReady(string) error { return nil }

// stopAdoptedMonitors drops the manager's instances so the per-instance monitor
// goroutines return instead of polling a removed temporary directory for the
// rest of the test binary's life.
func stopAdoptedMonitors(t *testing.T, manager *Manager) {
	t.Helper()
	t.Cleanup(func() {
		manager.mu.Lock()
		manager.instances = make(map[string]*Instance)
		manager.mu.Unlock()
	})
}

func TestStartRecordsIdentityOnlyAfterTheLaunchedProcessExecs(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "firecracker")
	manager := NewManager(binary, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	launcher := &specCapturingLauncher{inner: &execRacingLauncher{pid: 3637}}
	inspector := &execRacingInspector{launcher: launcher, pid: 3637, preExec: 3, binary: binary}
	manager.launcher = launcher
	manager.inspector = inspector
	stopAdoptedMonitors(t, manager)

	service := config.ServiceConfig{Name: "app", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}
	if err := manager.Start(context.Background(), service); err != nil {
		t.Fatal(err)
	}

	manifest, err := readManifest(manifestPath(filepath.Join(dir, "vms", "app")))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Executable != binary || manifest.ExecutableIno != 473725 {
		t.Fatalf("manifest recorded the pre-exec identity: %#v", manifest)
	}
	if manifest.HostBootID == "" || manifest.ProcessStart == 0 || manifest.ExecutableDev == 0 {
		t.Fatalf("manifest persisted an incomplete identity: %#v", manifest)
	}
	// The recorded identity must validate immediately, which is what a raced
	// manifest could never do again.
	if err := validateOwnedProcess(inspector, manifest); err != nil {
		t.Fatalf("recorded identity does not validate: %v", err)
	}
}

func TestStartAbandonsALaunchWhoseIdentityIsNeverProvable(t *testing.T) {
	service := config.ServiceConfig{Name: "app", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}
	for _, testCase := range []struct {
		name            string
		exitsWhenKilled bool
		wantRetained    bool
	}{
		{name: "killed process is cleaned up", exitsWhenKilled: true},
		{name: "surviving process retains its state", wantRetained: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			binary := filepath.Join(dir, "firecracker")
			manager := NewManager(binary, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
			// preExec is never reached, so the PID keeps reporting the launcher.
			inner := &execRacingLauncher{pid: 3637}
			launcher := &specCapturingLauncher{inner: inner}
			inspector := &execRacingInspector{launcher: launcher, pid: 3637, preExec: 1 << 30, binary: binary}
			if testCase.exitsWhenKilled {
				inner.onStop = inspector.exit
			}
			manager.launcher = launcher
			manager.inspector = inspector
			manager.identityTimeout = 20 * time.Millisecond
			manager.exitConfirmTimeout = 20 * time.Millisecond

			err := manager.Start(context.Background(), service)
			if err == nil || !strings.Contains(err.Error(), "confirming launched process identity") {
				t.Fatalf("start did not fail on an unprovable identity: %v", err)
			}
			instance := manager.List()["app"]
			if !testCase.wantRetained && instance != nil {
				t.Fatalf("killed launch registered an instance: %#v", instance)
			}
			if launcher.stops == 0 {
				t.Fatal("abandoned launch was never signalled")
			}

			manifest, manifestErr := readManifest(manifestPath(filepath.Join(dir, "vms", "app")))
			if !testCase.wantRetained {
				if !errors.Is(manifestErr, os.ErrNotExist) {
					t.Fatalf("state of a killed launch was not removed: %v", manifestErr)
				}
				return
			}
			// A process that is still reported alive keeps its state: deleting it
			// would let the next reconcile start a duplicate Firecracker against
			// the same socket and TAP.
			if manifestErr != nil {
				t.Fatal(manifestErr)
			}
			if manifest.Lifecycle != lifecycleFailed || manifest.LastError == "" || manifest.PID != 3637 {
				t.Fatalf("retained state did not describe the surviving process: %#v", manifest)
			}
			if instance == nil || instance.State != StateRecoveryPending || instance.PID != 3637 || instance.LastError == "" {
				t.Fatalf("retained launch was not exposed as recovery_pending: %#v", instance)
			}
		})
	}
}

func TestStartReclaimsStateLeftByAFailedLaunch(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "firecracker")
	manager := NewManager(binary, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	failing := &execRacingLauncher{launchErr: errors.New("transient unit refused")}
	manager.launcher = &specCapturingLauncher{inner: failing}
	service := config.ServiceConfig{Name: "app", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}
	if err := manager.Start(context.Background(), service); err == nil {
		t.Fatal("expected the launch to fail")
	}

	// The failed launch owns no process, so it must not block later starts for
	// the life of the agent process.
	launcher := &specCapturingLauncher{inner: &execRacingLauncher{pid: 3637}}
	manager.launcher = launcher
	manager.inspector = &execRacingInspector{launcher: launcher, pid: 3637, binary: binary}
	stopAdoptedMonitors(t, manager)
	if err := manager.Start(context.Background(), service); err != nil {
		t.Fatalf("retry after a failed launch was blocked: %v", err)
	}
}

func TestStartDoesNotReclaimFailedSystemdLaunchWithoutProofItExited(t *testing.T) {
	dir := t.TempDir()
	vmDir := filepath.Join(dir, "vms", "app")
	service := config.ServiceConfig{Name: "app", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}
	hash, err := serviceConfigHash(service)
	if err != nil {
		t.Fatal(err)
	}
	manifest := &instanceManifest{
		SchemaVersion: manifestSchemaVersion,
		Service:       service.Name,
		InstanceID:    "failed-systemd-launch",
		Lifecycle:     lifecycleFailed,
		Config:        service,
		ConfigHash:    hash,
		SocketPath:    filepath.Join(vmDir, "firecracker.sock"),
		ConfigPath:    filepath.Join(vmDir, "vm-config.json"),
		VMDir:         vmDir,
		Launcher:      "systemd",
		LauncherUnit:  "firework-vm-failed-systemd-launch.service",
		StartedAt:     time.Now().UTC(),
		LastError:     "transient unit did not report a main PID",
	}
	if err := writeManifest(manifestPath(vmDir), manifest); err != nil {
		t.Fatal(err)
	}

	manager := NewManager("/usr/bin/firecracker", dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	err = manager.Start(context.Background(), service)
	if err == nil || !strings.Contains(err.Error(), "durable VM state") {
		t.Fatalf("ambiguous systemd launch state was reclaimed: %v", err)
	}
	if _, err := os.Stat(manifestPath(vmDir)); err != nil {
		t.Fatalf("ambiguous systemd launch state was removed: %v", err)
	}
}

func TestWriteVMConfigAddsDeterministicVolumeDrivesAndPayload(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager("/bin/true", dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	path, err := manager.writeVMConfig(dir, config.ServiceConfig{
		Name: "app", Image: "/root.ext4", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128,
		KernelArgs: "console=ttyS0 init=/sbin/fc-init /bin/app -- flag",
	}, []volume.PreparedVolume{
		{LogicalID: "app/z", PathOnHost: "/z.ext4", MountPath: "/z", Type: config.VolumeTypeLocal},
		{LogicalID: "app/a", PathOnHost: "/a.ext4", MountPath: "/a", Type: config.VolumeTypeLocal},
	})
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
	if len(cfg.Drives) != 3 || cfg.Drives[1].PathOnHost != "/a.ext4" || cfg.Drives[2].PathOnHost != "/z.ext4" {
		t.Fatalf("unexpected drives: %#v", cfg.Drives)
	}
	fields := strings.Fields(cfg.BootSource.BootArgs)
	var encoded string
	for i, field := range fields {
		if strings.HasPrefix(field, "firework.volumes64=") {
			encoded = strings.TrimPrefix(field, "firework.volumes64=")
			if i+1 >= len(fields) || fields[i+1] != "--" {
				t.Fatalf("volume payload is not before application separator: %q", cfg.BootSource.BootArgs)
			}
		}
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var payload guestVolumePayload
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Volumes[0].Device != "/dev/vdb" || payload.Volumes[0].Name != "a" || payload.Volumes[1].Device != "/dev/vdc" {
		t.Fatalf("unexpected payload: %#v", payload)
	}
}

func TestValidateVolumeKernelArgsRejectsPayloadBeyondPortableLimit(t *testing.T) {
	err := validateVolumeKernelArgs(config.ServiceConfig{
		Name: "app", KernelArgs: strings.Repeat("x", maxKernelCommandLineBytes),
		Volumes: []config.VolumeConfig{{
			Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "kernel command line with volume payload") {
		t.Fatalf("expected command-line limit error, got %v", err)
	}
}

func TestPreflightRetainsVisibleVolumeError(t *testing.T) {
	manager := NewManager("/bin/true", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	service := config.ServiceConfig{Name: "app", Volumes: []config.VolumeConfig{{
		Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data",
	}}}
	if _, err := manager.Preflight(context.Background(), service); err == nil {
		t.Fatal("expected missing storage error")
	}
	if got := manager.VolumeError("app"); !strings.Contains(got, "storage is not configured") {
		t.Fatalf("unexpected retained volume error %q", got)
	}
}
