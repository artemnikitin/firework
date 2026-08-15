package vm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

type fakeProcessInspector struct {
	identities map[int]processIdentity
	errors     map[int]error
	socketErr  error
	find       []processIdentity
	findErr    error
}

func (f *fakeProcessInspector) Inspect(pid int) (processIdentity, error) {
	if err := f.errors[pid]; err != nil {
		return processIdentity{}, err
	}
	identity, ok := f.identities[pid]
	if !ok {
		return processIdentity{}, errProcessNotFound
	}
	return identity, nil
}

func (f *fakeProcessInspector) FindByArguments(_, _ string) ([]processIdentity, error) {
	return append([]processIdentity(nil), f.find...), f.findErr
}

func (f *fakeProcessInspector) SocketReady(string) error { return f.socketErr }

type recordingLauncher struct {
	stops int
}

func (*recordingLauncher) Launch(context.Context, launchSpec) (*launchedProcess, error) {
	return nil, errors.New("not implemented")
}

func (l *recordingLauncher) Stop(*instanceManifest, syscall.Signal) error {
	l.stops++
	return nil
}

func TestRecoverAdoptsMatchingSurvivorOnce(t *testing.T) {
	manager, manifest, inspector := recoveryFixture(t)
	manager.inspector = inspector

	adopted, err := manager.Recover(context.Background(), config.NodeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(adopted) != 1 || adopted[0] != manifest.Service {
		t.Fatalf("adopted = %v", adopted)
	}
	instance := manager.List()[manifest.Service]
	if instance == nil || instance.State != StateRunning || instance.PID != manifest.PID {
		t.Fatalf("unexpected adopted instance: %#v", instance)
	}

	again, err := manager.Recover(context.Background(), config.NodeConfig{})
	if err != nil || len(again) != 0 {
		t.Fatalf("repeated recovery = %v, %v", again, err)
	}
}

func TestRecoverQuarantinesPIDReuseAndNeverSignalsIt(t *testing.T) {
	manager, manifest, inspector := recoveryFixture(t)
	identity := inspector.identities[manifest.PID]
	identity.StartTicks++
	inspector.identities[manifest.PID] = identity
	manager.inspector = inspector
	launcher := &recordingLauncher{}
	manager.launcher = launcher

	if _, err := manager.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	instance := manager.List()[manifest.Service]
	if instance == nil || instance.State != StateRecoveryPending {
		t.Fatalf("PID reuse was not quarantined: %#v", instance)
	}
	if err := manager.Stop(manifest.Service); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected safe stop refusal, got %v", err)
	}
	if launcher.stops != 0 {
		t.Fatalf("quarantined PID was signalled %d times", launcher.stops)
	}
}

func TestRecoverQuarantinesLiveProcessWithMissingSocket(t *testing.T) {
	manager, manifest, inspector := recoveryFixture(t)
	inspector.socketErr = os.ErrNotExist
	manager.inspector = inspector

	if _, err := manager.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	instance := manager.List()[manifest.Service]
	if instance == nil || instance.State != StateRecoveryPending || !strings.Contains(instance.LastError, "socket") {
		t.Fatalf("missing socket was not quarantined: %#v", instance)
	}
}

func TestRecoverCleansStateOnlyWhenProcessIsProvenDead(t *testing.T) {
	manager, manifest, inspector := recoveryFixture(t)
	delete(inspector.identities, manifest.PID)
	manager.inspector = inspector

	if _, err := manager.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if len(manager.List()) != 0 {
		t.Fatalf("dead process retained instance: %#v", manager.List())
	}
	if _, err := os.Stat(manifest.VMDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dead process state was not removed: %v", err)
	}
}

func TestRecoverNeverTouchesVMsThisProcessStarted(t *testing.T) {
	// A fresh node has no VM state directory. Recovery must still be spent on
	// that pass, because the directory appears as soon as this process creates
	// its first VM and a later pass would then adopt VMs it started itself.
	stateDir := t.TempDir()
	binary := filepath.Join(stateDir, "firecracker")
	manager := NewManager(binary, stateDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	launcher := &specCapturingLauncher{inner: &execRacingLauncher{pid: 3637}}
	manager.launcher = launcher
	manager.inspector = &execRacingInspector{launcher: launcher, pid: 3637, binary: binary}
	stopAdoptedMonitors(t, manager)

	service := config.ServiceConfig{Name: "app", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}
	desired := config.NodeConfig{Services: []config.ServiceConfig{service}}
	if adopted, err := manager.Recover(context.Background(), desired); err != nil || len(adopted) != 0 {
		t.Fatalf("recovery on a fresh node = %v, %v", adopted, err)
	}
	if err := manager.Start(context.Background(), service); err != nil {
		t.Fatal(err)
	}

	adopted, err := manager.Recover(context.Background(), desired)
	if err != nil || len(adopted) != 0 {
		t.Fatalf("recovery adopted a VM this process started: %v, %v", adopted, err)
	}
	if instance := manager.List()[service.Name]; instance == nil || instance.State != StateRunning {
		t.Fatalf("running instance was disturbed by recovery: %#v", instance)
	}
}

func TestRecoverCleansStateAfterAHostReboot(t *testing.T) {
	manager, manifest, inspector := recoveryFixture(t)
	// The boot ID is host-global and read fresh on every inspection, so a
	// mismatch proves the recorded process cannot have survived.
	identity := inspector.identities[manifest.PID]
	identity.HostBootID = "boot-after-reboot"
	inspector.identities[manifest.PID] = identity
	manager.inspector = inspector

	if _, err := manager.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if instance := manager.List()[manifest.Service]; instance != nil {
		t.Fatalf("a rebooted host quarantined instead of cleaning up: %#v", instance)
	}
	if _, err := os.Stat(manifest.VMDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state from before the reboot was not removed: %v", err)
	}
}

func TestRecoverDistinguishesUnrecordedIdentityFromAMismatch(t *testing.T) {
	manager, manifest, inspector := recoveryFixture(t)
	// An inspection that raced the launcher's exec recorded nothing, and the
	// live process no longer presents this instance's command line either.
	manifest.HostBootID = ""
	manifest.ProcessStart = 0
	manifest.Executable = ""
	manifest.ExecutableDev = 0
	manifest.ExecutableIno = 0
	if err := writeManifest(manifestPath(manifest.VMDir), manifest); err != nil {
		t.Fatal(err)
	}
	identity := inspector.identities[manifest.PID]
	identity.CommandLine = []string{"/usr/lib/systemd/systemd"}
	inspector.identities[manifest.PID] = identity
	manager.inspector = inspector

	if _, err := manager.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	instance := manager.List()[manifest.Service]
	if instance == nil || instance.State != StateRecoveryPending {
		t.Fatalf("unrecorded identity was not quarantined: %#v", instance)
	}
	// "never recorded" and "does not match" are different facts and must not
	// share the boot-identity message that hid this defect for two incidents.
	if !strings.Contains(instance.LastError, errIdentityNotRecorded.Error()) {
		t.Fatalf("unrecorded identity reported as a mismatch: %q", instance.LastError)
	}
	if _, err := os.Stat(manifest.VMDir); err != nil {
		t.Fatalf("quarantined state was not preserved: %v", err)
	}
}

func TestRecoverRepairsIdentityCapturedBeforeTheLaunchedExec(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		mutate   func(*instanceManifest)
		wantKept bool
	}{
		{
			name: "inspection captured the launcher's identity",
			mutate: func(manifest *instanceManifest) {
				manifest.Executable = "/usr/lib/systemd/systemd"
				manifest.ExecutableDev = 66306
				manifest.ExecutableIno = 8581641
			},
		},
		{
			name: "inspection captured nothing at all",
			mutate: func(manifest *instanceManifest) {
				manifest.HostBootID = ""
				manifest.ProcessStart = 0
				manifest.Executable = ""
				manifest.ExecutableDev = 0
				manifest.ExecutableIno = 0
			},
		},
		{
			name: "a recycled PID is never repaired",
			mutate: func(manifest *instanceManifest) {
				manifest.ProcessStart = 999
			},
			wantKept: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			manager, manifest, inspector := recoveryFixture(t)
			testCase.mutate(manifest)
			if err := writeManifest(manifestPath(manifest.VMDir), manifest); err != nil {
				t.Fatal(err)
			}
			manager.inspector = inspector
			launcher := &recordingLauncher{}
			manager.launcher = launcher

			adopted, err := manager.Recover(context.Background(), config.NodeConfig{})
			if err != nil {
				t.Fatal(err)
			}
			instance := manager.List()[manifest.Service]
			if testCase.wantKept {
				if instance == nil || instance.State != StateRecoveryPending {
					t.Fatalf("PID reuse was repaired away: %#v", instance)
				}
				return
			}
			if len(adopted) != 1 || instance == nil || instance.State != StateRunning {
				t.Fatalf("a survivor with a raced identity was not re-adopted: adopted=%v instance=%#v", adopted, instance)
			}
			// The repaired identity is persisted, so the next pass validates
			// without needing the command-line proof again.
			repaired, err := readManifest(manifestPath(manifest.VMDir))
			if err != nil {
				t.Fatal(err)
			}
			if err := validateOwnedProcess(inspector, repaired); err != nil {
				t.Fatalf("repaired manifest still does not validate: %v", err)
			}
			if launcher.stops != 0 {
				t.Fatalf("a repaired survivor was signalled %d times", launcher.stops)
			}
		})
	}
}

func TestUpdatingARepairedSurvivorNoLongerStalls(t *testing.T) {
	// The reported stall: reconciliation plans an update for a service whose VM
	// is quarantined, Remove refuses, and the identical failure repeats forever.
	manager, manifest, inspector := recoveryFixture(t)
	manifest.Executable = "/usr/lib/systemd/systemd"
	manifest.ExecutableDev = 66306
	manifest.ExecutableIno = 8581641
	if err := writeManifest(manifestPath(manifest.VMDir), manifest); err != nil {
		t.Fatal(err)
	}
	manager.inspector = inspector
	manager.launcher = &recordingLauncher{}

	if _, err := manager.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	instance := manager.List()[manifest.Service]
	if instance == nil || instance.State != StateRunning || instance.LastError != "" {
		t.Fatalf("survivor is still quarantined, so every update would fail: %#v", instance)
	}

	// Stopping a VM whose process is already gone takes Stop's proven-dead path,
	// which keeps this assertion about the quarantine refusal rather than about
	// signalling a PID the test does not own.
	delete(inspector.identities, manifest.PID)
	if err := manager.Remove(manifest.Service); err != nil {
		t.Fatalf("update could not replace the recovered VM: %v", err)
	}
	if instance := manager.List()[manifest.Service]; instance != nil {
		t.Fatalf("removed service is still tracked: %#v", instance)
	}
	if _, err := os.Stat(manifest.VMDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed VM state was left behind: %v", err)
	}
}

func TestRecoverMigratesExactlyOneLegacyFirecrackerProcess(t *testing.T) {
	stateDir := t.TempDir()
	vmDir := filepath.Join(stateDir, "vms", "app")
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(stateDir, "firecracker")
	if err := os.WriteFile(binary, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolvedBinary, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(vmDir, "firecracker.sock")
	configPath := filepath.Join(vmDir, "vm-config.json")
	identity := processIdentity{
		PID: 51, HostBootID: "boot", StartTicks: 200, Executable: resolvedBinary,
		ExecutableDev: 11, ExecutableIno: 12,
		CommandLine: []string{binary, "--api-sock", socketPath, "--config-file", configPath},
	}
	inspector := &fakeProcessInspector{
		identities: map[int]processIdentity{identity.PID: identity}, errors: make(map[int]error),
		find: []processIdentity{identity},
	}
	manager := NewManager(binary, stateDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager.inspector = inspector
	service := config.ServiceConfig{Name: "app", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}

	adopted, err := manager.Recover(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{service}})
	if err != nil {
		t.Fatal(err)
	}
	instance := manager.List()[service.Name]
	if len(adopted) != 1 || instance == nil || instance.State != StateRunning || !instance.manifest.Legacy {
		t.Fatalf("legacy process was not migrated: adopted=%v instance=%#v", adopted, instance)
	}
	if _, err := os.Stat(manifestPath(vmDir)); err != nil {
		t.Fatalf("legacy ownership manifest was not written: %v", err)
	}
	manager.mu.Lock()
	manager.instances = make(map[string]*Instance)
	manager.mu.Unlock()
}

func recoveryFixture(t *testing.T) (*Manager, *instanceManifest, *fakeProcessInspector) {
	t.Helper()
	stateDir := t.TempDir()
	vmDir := filepath.Join(stateDir, "vms", "app")
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	service := config.ServiceConfig{Name: "app", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}
	hash, err := serviceConfigHash(service)
	if err != nil {
		t.Fatal(err)
	}
	manifest := &instanceManifest{
		SchemaVersion: manifestSchemaVersion, Service: service.Name, InstanceID: "owned-instance",
		Lifecycle: lifecycleRunning, Config: service, ConfigHash: hash, PID: 42,
		HostBootID: "boot", ProcessStart: 100, Executable: "/firecracker",
		ExecutableDev: 7, ExecutableIno: 9, VMDir: vmDir,
		SocketPath: filepath.Join(vmDir, "firecracker.sock"), ConfigPath: filepath.Join(vmDir, "vm-config.json"),
		Launcher: "direct",
	}
	if err := writeManifest(manifestPath(vmDir), manifest); err != nil {
		t.Fatal(err)
	}
	inspector := &fakeProcessInspector{identities: map[int]processIdentity{
		42: {
			PID: 42, HostBootID: "boot", StartTicks: 100, Executable: "/firecracker",
			ExecutableDev: 7, ExecutableIno: 9,
			CommandLine: []string{"/firecracker", "--id", "owned-instance", "--api-sock", manifest.SocketPath, "--config-file", manifest.ConfigPath},
		},
	}, errors: make(map[int]error)}
	manager := NewManager("/firecracker", stateDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		manager.mu.Lock()
		manager.instances = make(map[string]*Instance)
		manager.mu.Unlock()
	})
	return manager, manifest, inspector
}
