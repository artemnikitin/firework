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
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/agentconfig"
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/volume"
)

// blockingRunner parks inside the first filesystem-mutating command, which is
// what a multi-minute mkfs.ext4 or resize2fs looks like from the manager's
// point of view.
type blockingRunner struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	fail    error
}

func (r *blockingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name == "resize2fs" && len(args) > 0 && args[0] == "-P" {
		return []byte("Estimated minimum size of the filesystem: 1024\n"), nil
	}
	if name == "tune2fs" {
		return []byte("Block size: 4096\n"), nil
	}
	return nil, nil
}

func (r *blockingRunner) RunDestructive(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.once.Do(func() {
		close(r.entered)
		<-r.release
	})
	if r.fail != nil {
		return nil, r.fail
	}
	return r.Run(ctx, name, args...)
}

type acceptingMounts struct{}

func (acceptingMounts) Verify(string) error { return nil }

// fakeRunner reports a fixed filesystem minimum, so a small shrink target is
// refused and a large one is accepted.
type fakeRunner struct{}

func (fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name == "resize2fs" && len(args) > 0 && args[0] == "-P" {
		return []byte("Estimated minimum size of the filesystem: 1024\n"), nil
	}
	if name == "tune2fs" {
		return []byte("Block size: 4096\n"), nil
	}
	return nil, nil
}

func (r fakeRunner) RunDestructive(ctx context.Context, name string, args ...string) ([]byte, error) {
	return r.Run(ctx, name, args...)
}

// countingLauncher fails the test's purpose loudly: phase 3 must not launch
// anything after an abort, so any launch at all is the defect.
type countingLauncher struct {
	mu       sync.Mutex
	launches int
}

func (l *countingLauncher) Launch(context.Context, launchSpec) (*launchedProcess, error) {
	l.mu.Lock()
	l.launches++
	l.mu.Unlock()
	return nil, errors.New("launch should not have been reached")
}

func (l *countingLauncher) Stop(*instanceManifest, syscall.Signal) error { return nil }

func (l *countingLauncher) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.launches
}

func barrierManager(t *testing.T, runner volume.CommandRunner) (*Manager, *countingLauncher) {
	t.Helper()
	stateDir := t.TempDir()
	pool := t.TempDir()
	volumeMgr := volume.NewManagerWithDependencies("node-1", agentconfig.StorageConfig{
		Local: &agentconfig.LocalStorageConfig{Path: pool, CapacityBytes: 1 << 30},
	}, runner, acceptingMounts{})
	manager := NewManagerWithVolumes("/bin/true", stateDir, slog.New(slog.NewTextHandler(io.Discard, nil)), volumeMgr)
	launcher := &countingLauncher{}
	manager.launcher = launcher
	return manager, launcher
}

func barrierService() config.ServiceConfig {
	return config.ServiceConfig{
		Name: "app", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128,
		Volumes: []config.VolumeConfig{{
			Name: "data", Type: config.VolumeTypeLocal, MountPath: "/var/lib/app",
			SizeBytes: 16 * config.MiB, BoundNode: "node-1", ResizeGeneration: 1,
		}},
	}
}

// The whole point of releasing the lock: a reader must not block behind a
// multi-minute volume operation, and what it reads must be truthful.
func TestListReportsStartingWhilePrepareRuns(t *testing.T) {
	runner := &blockingRunner{entered: make(chan struct{}), release: make(chan struct{})}
	manager, launcher := barrierManager(t, runner)

	done := make(chan error, 1)
	go func() { done <- manager.Start(context.Background(), barrierService()) }()
	<-runner.entered

	instance := manager.List()["app"]
	if instance == nil || instance.State != StateStarting {
		t.Fatalf("expected a starting placeholder during Prepare, got %#v", instance)
	}

	close(runner.release)
	<-done
	if launcher.count() == 0 {
		t.Fatal("expected the start to proceed to launch after Prepare returned")
	}
}

// A stop that arrives while volumes are being prepared must return at once —
// not wait out the mkfs — and the start must then launch nothing.
func TestStopDuringPrepareAbortsTheStart(t *testing.T) {
	runner := &blockingRunner{entered: make(chan struct{}), release: make(chan struct{})}
	manager, launcher := barrierManager(t, runner)

	done := make(chan error, 1)
	go func() { done <- manager.Start(context.Background(), barrierService()) }()
	<-runner.entered

	stopped := make(chan error, 1)
	go func() { stopped <- manager.Stop("app") }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("stopping a starting service should succeed, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop blocked behind volume preparation")
	}
	// A second stop must not see an error for work the first one already did.
	if err := manager.Stop("app"); err != nil {
		t.Fatalf("repeated stop while aborting should succeed, got %v", err)
	}

	close(runner.release)
	err := <-done
	if !errors.Is(err, ErrStartAborted) {
		t.Fatalf("expected ErrStartAborted, got %v", err)
	}
	if launcher.count() != 0 {
		t.Fatalf("aborted start launched %d process(es)", launcher.count())
	}
	if instance := manager.List()["app"]; instance != nil {
		t.Fatalf("expected the placeholder to be cleaned up, got %#v", instance)
	}
}

func TestRemoveDuringPrepareAbortsTheStartAndClearsState(t *testing.T) {
	runner := &blockingRunner{entered: make(chan struct{}), release: make(chan struct{})}
	manager, launcher := barrierManager(t, runner)
	vmDir := filepath.Join(manager.stateDir, "vms", "app")

	done := make(chan error, 1)
	go func() { done <- manager.Start(context.Background(), barrierService()) }()
	<-runner.entered

	removed := make(chan error, 1)
	go func() { removed <- manager.Remove("app") }()
	select {
	case err := <-removed:
		if err != nil {
			t.Fatalf("removing a starting service should succeed, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Remove blocked behind volume preparation")
	}
	if err := manager.Remove("app"); err != nil {
		t.Fatalf("repeated remove while aborting should succeed, got %v", err)
	}
	if _, err := os.Stat(vmDir); !os.IsNotExist(err) {
		t.Fatalf("expected the VM state directory to be removed, got %v", err)
	}

	close(runner.release)
	if err := <-done; !errors.Is(err, ErrStartAborted) {
		t.Fatalf("expected ErrStartAborted, got %v", err)
	}
	if launcher.count() != 0 {
		t.Fatalf("aborted start launched %d process(es)", launcher.count())
	}
	if instance := manager.List()["app"]; instance != nil {
		t.Fatalf("expected the placeholder to be cleaned up, got %#v", instance)
	}
}

// Phase 3 validates the startID, not the service name, so a placeholder that
// was cleared and replaced by a later attempt is never mistaken for its own.
func TestPhaseThreeIgnoresAPlaceholderFromAnotherAttempt(t *testing.T) {
	runner := &blockingRunner{entered: make(chan struct{}), release: make(chan struct{})}
	manager, launcher := barrierManager(t, runner)

	done := make(chan error, 1)
	go func() { done <- manager.Start(context.Background(), barrierService()) }()
	<-runner.entered

	// Simulate the placeholder being taken over by a later start.
	manager.mu.Lock()
	manager.instances["app"] = &Instance{Name: "app", State: StateStarting, startID: "someone-else"}
	manager.mu.Unlock()

	close(runner.release)
	if err := <-done; !errors.Is(err, ErrStartAborted) {
		t.Fatalf("expected ErrStartAborted, got %v", err)
	}
	if launcher.count() != 0 {
		t.Fatalf("start touched the launcher despite losing its placeholder (%d launches)", launcher.count())
	}
	if instance := manager.List()["app"]; instance == nil || instance.startID != "someone-else" {
		t.Fatalf("expected the other attempt's placeholder to survive, got %#v", instance)
	}
}

func TestFailedPrepareLeavesNoPlaceholder(t *testing.T) {
	runner := &blockingRunner{entered: make(chan struct{}), release: make(chan struct{}), fail: errors.New("mkfs failed")}
	manager, launcher := barrierManager(t, runner)

	done := make(chan error, 1)
	go func() { done <- manager.Start(context.Background(), barrierService()) }()
	<-runner.entered
	close(runner.release)

	err := <-done
	if err == nil || !strings.Contains(err.Error(), "preparing volumes") {
		t.Fatalf("expected a volume preparation failure, got %v", err)
	}
	if instance := manager.List()["app"]; instance != nil {
		t.Fatalf("expected no placeholder after a failed Prepare, got %#v", instance)
	}
	if launcher.count() != 0 {
		t.Fatalf("failed Prepare still launched %d process(es)", launcher.count())
	}
	if manager.VolumeError("app") == "" {
		t.Fatal("expected the preparation failure to stay visible as a volume error")
	}
}

// The phase-1 entry check treats both starting states as active, so the agent
// API and the shutdown path cannot start a second VM for the same service.
func TestConcurrentStartIsRejectedWhileStarting(t *testing.T) {
	runner := &blockingRunner{entered: make(chan struct{}), release: make(chan struct{})}
	manager, _ := barrierManager(t, runner)

	done := make(chan error, 1)
	go func() { done <- manager.Start(context.Background(), barrierService()) }()
	<-runner.entered

	second := manager.Start(context.Background(), barrierService())
	if !errors.Is(second, ErrStartInProgress) {
		t.Fatalf("expected ErrStartInProgress, got %v", second)
	}
	if !IsStartRace(second) {
		t.Fatal("a rejected concurrent start must classify as a benign race")
	}

	close(runner.release)
	<-done
}

// An over-long command line must fail on create, not only on update: the
// launch path and Preflight now build the args through the same function.
func TestOverlongKernelCommandLineFailsOnCreate(t *testing.T) {
	manager, _ := barrierManager(t, &fakeVolumeRunner{})
	svc := barrierService()
	svc.KernelArgs = strings.Repeat("x", maxKernelCommandLineBytes)

	_, err := manager.writeVMConfig(t.TempDir(), svc, []volume.PreparedVolume{{
		LogicalID: "app/data", PathOnHost: "/pool/app/data/volume.ext4",
		MountPath: "/var/lib/app", Type: config.VolumeTypeLocal, SizeBytes: 16 * config.MiB,
	}})
	if err == nil || !strings.Contains(err.Error(), "kernel command line") {
		t.Fatalf("expected the create path to enforce the command-line limit, got %v", err)
	}
}

// Preflight rejects before a running VM is touched, and the launch path
// rejects before it boots something unbootable. They must agree exactly.
func TestPreflightAndWriteVMConfigBuildIdenticalBootArgs(t *testing.T) {
	svc := barrierService()
	svc.Volumes = append(svc.Volumes, config.VolumeConfig{
		Name: "cache", Type: config.VolumeTypeLocal, MountPath: "/var/cache/app",
		SizeBytes: 8 * config.MiB, BoundNode: "node-1", ResizeGeneration: 1,
	})

	declared, err := guestVolumesFromConfig(svc.Volumes)
	if err != nil {
		t.Fatal(err)
	}
	fromConfig, err := buildBootArgs(svc, declared)
	if err != nil {
		t.Fatal(err)
	}

	manager, _ := barrierManager(t, &fakeVolumeRunner{})
	vmDir := t.TempDir()
	prepared := []volume.PreparedVolume{
		{LogicalID: "app/data", PathOnHost: "/pool/app/data/volume.ext4", MountPath: "/var/lib/app", Type: config.VolumeTypeLocal},
		{LogicalID: "app/cache", PathOnHost: "/pool/app/cache/volume.ext4", MountPath: "/var/cache/app", Type: config.VolumeTypeLocal},
	}
	if _, err := manager.writeVMConfig(vmDir, svc, prepared); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(filepath.Join(vmDir, "vm-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), jsonEscape(fromConfig)) {
		t.Fatalf("preflight and launch boot args differ.\npreflight: %s\nwritten: %s", fromConfig, written)
	}
}

// jsonEscape renders a boot-args string the way it appears inside the
// Firecracker config document.
func jsonEscape(value string) string {
	return strings.ReplaceAll(value, `"`, `\"`)
}

// fakeVolumeRunner satisfies the runner interface for tests that never reach a
// real filesystem operation.
type fakeVolumeRunner struct{}

func (fakeVolumeRunner) Run(context.Context, string, ...string) ([]byte, error) { return nil, nil }
func (fakeVolumeRunner) RunDestructive(context.Context, string, ...string) ([]byte, error) {
	return nil, nil
}

// The acknowledged form of a refused shrink must converge.
//
// After the control plane acknowledges a rejection it renders the *effective*
// size with the *refused* generation — it keeps that generation so the
// acknowledgement can match its own record. The running instance, meanwhile,
// carries the applied generation. needsUpdate compares whole VolumeConfig
// structs, so unless normalization reconciles the two the reconciler plans an
// update, stops the VM, and restarts it — every tick that reaches Plan.
func TestAcknowledgedRejectionConvergesWithTheRunningConfig(t *testing.T) {
	manager, _ := barrierManager(t, &fakeRunner{})
	svc := barrierService()

	if _, err := manager.volumeManager.Prepare(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	// A shrink the fake measurement refuses, at generation 2.
	refused := svc
	refused.Volumes = append([]config.VolumeConfig(nil), svc.Volumes...)
	refused.Volumes[0].SizeBytes = 2 * config.MiB
	refused.Volumes[0].ResizeGeneration = 2
	prepared, err := manager.volumeManager.Prepare(context.Background(), refused)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared[0].Rejected {
		t.Fatalf("precondition: expected the shrink to be refused, got %#v", prepared[0])
	}

	// What Start stores on the instance: the effective configuration.
	running := clampToPrepared(refused, prepared)

	// What the control plane renders once it has acknowledged the rejection.
	acknowledged := svc
	acknowledged.Volumes = append([]config.VolumeConfig(nil), svc.Volumes...)
	acknowledged.Volumes[0].SizeBytes = 16 * config.MiB
	acknowledged.Volumes[0].ResizeGeneration = 2

	services := []config.ServiceConfig{acknowledged}
	manager.NormalizeVolumes(services)

	// volumesEqual compares whole VolumeConfig structs, so this equality is
	// exactly the condition under which no ActionUpdate is planned.
	if services[0].Volumes[0] != running.Volumes[0] {
		t.Fatalf("the rendered config does not match the running one, so an update would be re-planned:\nrendered %#v\nrunning  %#v",
			services[0].Volumes[0], running.Volumes[0])
	}

	// The agent stops reporting a refusal here, and that is deliberate. These
	// bytes are exactly what a direct-Git operator writes to *withdraw* the
	// request, so the agent cannot tell a standing request from a withdrawn
	// one and must not degrade the node forever on the ambiguity. Only the
	// record still knows the operator's request, so that half of the
	// visibility is the control plane's — see refusedVolumes there.
	if got := manager.VolumeRejections(); len(got) != 0 {
		t.Fatalf("the acknowledged shape must not keep reporting a refusal: %#v", got)
	}
}

// The ownership manifest carries the prepared volume set, and an adopted VM is
// reconstructed from it. A rejected volume's effective size must survive, or
// adoption resurrects the refused configuration.
func TestPreparedVolumeSurvivesTheOwnershipManifest(t *testing.T) {
	original := &instanceManifest{
		SchemaVersion: manifestSchemaVersion, Service: "app", InstanceID: "i-1",
		Volumes: []volume.PreparedVolume{{
			LogicalID: "app/data", PathOnHost: "/pool/app/data/volume.ext4",
			MountPath: "/var/lib/app", Type: config.VolumeTypeLocal,
			SizeBytes: 16 * config.MiB, ResizeGeneration: 1,
			Rejected: true, RequestedGeneration: 2, RequestedSizeBytes: 2 * config.MiB,
			MinimumSizeBytes: 4 * config.MiB,
		}},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var round instanceManifest
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatal(err)
	}
	got := round.Volumes[0]
	if got.SizeBytes != 16*config.MiB || got.ResizeGeneration != 1 {
		t.Fatalf("the effective configuration did not survive adoption: %#v", got)
	}
	if !got.Rejected || got.RequestedSizeBytes != 2*config.MiB || got.RequestedGeneration != 2 {
		t.Fatalf("the refusal did not survive adoption: %#v", got)
	}
}

// Only the rejected volumes are clamped; a healthy sibling keeps exactly what
// was asked for.
func TestClampTouchesOnlyRejectedVolumes(t *testing.T) {
	svc := config.ServiceConfig{Name: "app", Volumes: []config.VolumeConfig{
		{Name: "data", Type: config.VolumeTypeLocal, MountPath: "/d", SizeBytes: 2 * config.MiB, ResizeGeneration: 2},
		{Name: "cache", Type: config.VolumeTypeLocal, MountPath: "/c", SizeBytes: 8 * config.MiB, ResizeGeneration: 2},
	}}
	prepared := []volume.PreparedVolume{
		{LogicalID: "app/data", SizeBytes: 16 * config.MiB, ResizeGeneration: 1, Rejected: true, RequestedSizeBytes: 2 * config.MiB},
		{LogicalID: "app/cache", SizeBytes: 8 * config.MiB, ResizeGeneration: 2},
	}

	clamped := clampToPrepared(svc, prepared)
	if clamped.Volumes[0].SizeBytes != 16*config.MiB || clamped.Volumes[0].ResizeGeneration != 1 {
		t.Fatalf("the rejected volume was not clamped: %#v", clamped.Volumes[0])
	}
	if clamped.Volumes[1] != svc.Volumes[1] {
		t.Fatalf("a healthy volume was altered: %#v", clamped.Volumes[1])
	}
	// The input must not be mutated: the caller still holds the desired config.
	if svc.Volumes[0].SizeBytes != 2*config.MiB {
		t.Fatal("clampToPrepared mutated its input")
	}
}

// The barrier, the volume manager's refusal snapshot, and the volume-error map
// are three pieces of shared state touched by phases that deliberately do not
// hold the same lock. Drive them concurrently under -race.
func TestBarrierAndSnapshotUnderConcurrentPressure(t *testing.T) {
	runner := &blockingRunner{entered: make(chan struct{}), release: make(chan struct{})}
	manager, launcher := barrierManager(t, runner)
	svc := barrierService()

	started := make(chan error, 1)
	go func() { started <- manager.Start(context.Background(), svc) }()
	<-runner.entered

	// While phase 2 holds no lock, hammer every reader and both aborters.
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 6 {
			case 0:
				_ = manager.List()
			case 1:
				_ = manager.VolumeError(svc.Name)
			case 2:
				_ = manager.VolumeRejections()
			case 3:
				manager.NormalizeVolumes([]config.ServiceConfig{svc})
			case 4:
				_ = manager.Stop(svc.Name)
			case 5:
				_ = manager.Remove(svc.Name)
			}
		}(i)
	}
	wg.Wait()
	close(runner.release)

	err := <-started
	// Stop and Remove both ran, so the start must have aborted without
	// launching anything.
	if !errors.Is(err, ErrStartAborted) {
		t.Fatalf("expected the start to abort, got %v", err)
	}
	if launcher.count() != 0 {
		t.Fatalf("an aborted start launched %d process(es)", launcher.count())
	}
	if inst := manager.List()[svc.Name]; inst != nil {
		t.Fatalf("the placeholder outlived the aborted start: %#v", inst)
	}
	// Repeated aborts after the fact stay idempotent.
	if err := manager.Stop(svc.Name); err == nil {
		t.Fatal("stopping an absent service should report not found")
	}
}
