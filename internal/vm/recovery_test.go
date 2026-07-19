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
