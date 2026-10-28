package vm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

func TestClearRecoveryPendingRemovesStateOnlyAfterProcessIsProvenGone(t *testing.T) {
	manager, manifest, inspector := recoveryFixture(t)
	inspector.errors[manifest.PID] = errors.New("temporary process inspection failure")
	manager.inspector = inspector
	if _, err := manager.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.instances[manifest.Service].manifest.Lifecycle = lifecycleStopping
	if err := writeManifest(manifestPath(manifest.VMDir), manager.instances[manifest.Service].manifest); err != nil {
		manager.mu.Unlock()
		t.Fatal(err)
	}
	manager.mu.Unlock()
	delete(inspector.errors, manifest.PID)
	delete(inspector.identities, manifest.PID)

	result, err := manager.ClearRecoveryPending(manifest.Service)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != RecoveryOutcomeCleared || result.State != StateStopped {
		t.Fatalf("unexpected recovery result: %#v", result)
	}
	if instance := manager.List()[manifest.Service]; instance != nil {
		t.Fatalf("cleared service remains in memory: %#v", instance)
	}
	if _, err := os.Stat(manifest.VMDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dead VM state remains on disk: %v", err)
	}
	if _, err := manager.ClearRecoveryPending(manifest.Service); err != nil {
		t.Fatalf("repeated clearance did not converge: %v", err)
	}
}

func TestClearRecoveryPendingDoesNotReadoptLiveStoppingVM(t *testing.T) {
	manager, manifest, inspector := recoveryFixture(t)
	manifest.Lifecycle = lifecycleStopping
	if err := writeManifest(manifestPath(manifest.VMDir), manifest); err != nil {
		t.Fatal(err)
	}
	manager.inspector = inspector
	manager.addRecoveryPending(manifest.Service, manifest, "stop acknowledgement was interrupted")
	launcher := &recordingLauncher{}
	manager.launcher = launcher

	result, err := manager.ClearRecoveryPending(manifest.Service)
	if err == nil || result.Outcome != RecoveryOutcomeAmbiguous || !strings.Contains(err.Error(), "shutdown was in progress") {
		t.Fatalf("live stopping VM was re-adopted: result=%#v err=%v", result, err)
	}
	if instance := manager.List()[manifest.Service]; instance == nil || instance.State != StateRecoveryPending {
		t.Fatalf("live stopping VM left recovery_pending: %#v", instance)
	}
	if launcher.stops != 0 {
		t.Fatalf("clear operation signalled a live stopping VM %d times", launcher.stops)
	}
}

func TestClearRecoveryPendingReadoptsOnlyARevalidatedLiveVM(t *testing.T) {
	manager, manifest, inspector := recoveryFixture(t)
	inspector.socketErr = os.ErrNotExist
	manager.inspector = inspector
	if _, err := manager.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if instance := manager.List()[manifest.Service]; instance == nil || instance.State != StateRecoveryPending {
		t.Fatalf("live VM was not quarantined before the socket recovered: %#v", instance)
	}
	inspector.socketErr = nil

	result, err := manager.ClearRecoveryPending(manifest.Service)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != RecoveryOutcomeReadopted || result.State != StateRunning {
		t.Fatalf("unexpected recovery result: %#v", result)
	}
	instance := manager.List()[manifest.Service]
	if instance == nil || instance.State != StateRunning || instance.PID != manifest.PID || instance.LastError != "" {
		t.Fatalf("manager did not publish the re-adopted VM: %#v", instance)
	}
	durable, err := readManifest(manifestPath(manifest.VMDir))
	if err != nil {
		t.Fatal(err)
	}
	if durable.Lifecycle != lifecycleRunning || durable.LastError != "" || validateOwnedProcess(inspector, durable) != nil {
		t.Fatalf("re-adopted VM was not persisted consistently: %#v", durable)
	}
	if repeated, err := manager.ClearRecoveryPending(manifest.Service); err != nil || repeated.Outcome != RecoveryOutcomeNoop {
		t.Fatalf("repeated readoption = %#v, %v", repeated, err)
	}

	restarted := NewManager(manager.firecrackerBin, manager.stateDir, manager.logger)
	restarted.inspector = inspector
	adopted, err := restarted.Recover(context.Background(), config.NodeConfig{})
	if err != nil || len(adopted) != 1 || adopted[0] != manifest.Service {
		t.Fatalf("manager restart did not reconstruct the re-adopted VM: adopted=%v err=%v", adopted, err)
	}
	if current := restarted.List()[manifest.Service]; current == nil || current.State != StateRunning || current.PID != manifest.PID {
		t.Fatalf("restarted manager disagrees with the durable re-adopted VM: %#v", current)
	}
}

func TestClearRecoveryPendingRefusesLiveAmbiguityWithoutSignalling(t *testing.T) {
	manager, manifest, inspector := recoveryFixture(t)
	identity := inspector.identities[manifest.PID]
	identity.StartTicks++
	inspector.identities[manifest.PID] = identity
	inspector.find = []processIdentity{identity}
	manager.inspector = inspector
	launcher := &recordingLauncher{}
	manager.launcher = launcher
	if _, err := manager.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}

	result, err := manager.ClearRecoveryPending(manifest.Service)
	if err == nil || result.Outcome != RecoveryOutcomeAmbiguous {
		t.Fatalf("PID reuse with a live claimant was not refused: result=%#v err=%v", result, err)
	}
	if !strings.Contains(err.Error(), "live process") {
		t.Fatalf("refusal did not name the claimant: %v", err)
	}
	if instance := manager.List()[manifest.Service]; instance == nil || instance.State != StateRecoveryPending {
		t.Fatalf("ambiguous service was changed: %#v", instance)
	}
	if launcher.stops != 0 {
		t.Fatalf("clear operation signalled an unproved process %d times", launcher.stops)
	}
	if _, err := os.Stat(manifest.VMDir); err != nil {
		t.Fatalf("ambiguous runtime state was removed: %v", err)
	}
}

func TestClearRecoveryPendingRefusesIncompleteProcessScan(t *testing.T) {
	manager, manifest, inspector := recoveryFixture(t)
	inspector.errors[manifest.PID] = errors.New("temporary process inspection failure")
	manager.inspector = inspector
	if _, err := manager.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	delete(inspector.errors, manifest.PID)
	delete(inspector.identities, manifest.PID)
	inspector.findErr = errors.New("permission denied while scanning process table")

	result, err := manager.ClearRecoveryPending(manifest.Service)
	if err == nil || result.Outcome != RecoveryOutcomeAmbiguous {
		t.Fatalf("incomplete process scan was treated as proof of death: result=%#v err=%v", result, err)
	}
	if instance := manager.List()[manifest.Service]; instance == nil || instance.State != StateRecoveryPending {
		t.Fatalf("process scan failure changed the in-memory quarantine: %#v", instance)
	}
	if _, err := os.Stat(manifest.VMDir); err != nil {
		t.Fatalf("process scan failure removed VM state: %v", err)
	}
}

func TestClearRecoveryPendingClearsPIDReuseAfterProvingNoLiveClaimant(t *testing.T) {
	manager, manifest, inspector := recoveryFixture(t)
	identity := inspector.identities[manifest.PID]
	identity.StartTicks++
	identity.CommandLine = []string{"/usr/bin/other"}
	inspector.identities[manifest.PID] = identity
	manager.inspector = inspector
	launcher := &recordingLauncher{}
	manager.launcher = launcher
	if _, err := manager.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}

	result, err := manager.ClearRecoveryPending(manifest.Service)
	if err != nil || result.Outcome != RecoveryOutcomeCleared {
		t.Fatalf("PID reuse without a live VM claimant was not cleared: result=%#v err=%v", result, err)
	}
	if launcher.stops != 0 {
		t.Fatalf("clear operation signalled a recycled PID %d times", launcher.stops)
	}
	if instance := manager.List()[manifest.Service]; instance != nil {
		t.Fatalf("cleared PID reuse remains in memory: %#v", instance)
	}
}

func TestClearRecoveryPendingRefusesMissingIdentityAndCorruptManifest(t *testing.T) {
	t.Run("missing identity", func(t *testing.T) {
		manager, manifest, inspector := recoveryFixture(t)
		manifest.PID = 0
		if err := writeManifest(manifestPath(manifest.VMDir), manifest); err != nil {
			t.Fatal(err)
		}
		manager.inspector = inspector
		manager.addRecoveryPending(manifest.Service, manifest, "process identity unavailable")

		result, err := manager.ClearRecoveryPending(manifest.Service)
		if err == nil || result.Outcome != RecoveryOutcomeAmbiguous {
			t.Fatalf("missing process identity was treated as gone: result=%#v err=%v", result, err)
		}
		if _, err := os.Stat(manifest.VMDir); err != nil {
			t.Fatalf("missing-identity state was removed: %v", err)
		}
	})

	t.Run("corrupt manifest", func(t *testing.T) {
		manager, manifest, _ := recoveryFixture(t)
		if err := os.WriteFile(manifestPath(manifest.VMDir), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		manager.addRecoveryPending(manifest.Service, &instanceManifest{
			SchemaVersion: manifestSchemaVersion, Service: manifest.Service, VMDir: manifest.VMDir,
			SocketPath: manifest.SocketPath, ConfigPath: manifest.ConfigPath,
		}, "cannot read ownership manifest")

		result, err := manager.ClearRecoveryPending(manifest.Service)
		if err == nil || result.Outcome != RecoveryOutcomeAmbiguous {
			t.Fatalf("corrupt manifest was cleared: result=%#v err=%v", result, err)
		}
		if _, err := os.Stat(manifest.VMDir); err != nil {
			t.Fatalf("corrupt-manifest state was removed: %v", err)
		}
	})

	t.Run("missing manifest", func(t *testing.T) {
		manager, manifest, _ := recoveryFixture(t)
		if err := os.Remove(manifestPath(manifest.VMDir)); err != nil {
			t.Fatal(err)
		}
		manager.addRecoveryPending(manifest.Service, manifest, "cannot read ownership manifest")

		result, err := manager.ClearRecoveryPending(manifest.Service)
		if err == nil || result.Outcome != RecoveryOutcomeAmbiguous {
			t.Fatalf("missing manifest was cleared: result=%#v err=%v", result, err)
		}
		if _, err := os.Stat(manifest.VMDir); err != nil {
			t.Fatalf("missing-manifest VM state was removed: %v", err)
		}
	})

	t.Run("durable config hash mismatch", func(t *testing.T) {
		manager, manifest, inspector := recoveryFixture(t)
		manager.inspector = inspector
		manager.addRecoveryPending(manifest.Service, manifest, "operator requested recovery")
		durable, err := readManifest(manifestPath(manifest.VMDir))
		if err != nil {
			t.Fatal(err)
		}
		durable.Config.MemoryMB++
		if err := writeManifest(manifestPath(manifest.VMDir), durable); err != nil {
			t.Fatal(err)
		}

		result, err := manager.ClearRecoveryPending(manifest.Service)
		if err == nil || result.Outcome != RecoveryOutcomeAmbiguous {
			t.Fatalf("durable config hash mismatch was adopted: result=%#v err=%v", result, err)
		}
		if instance := manager.List()[manifest.Service]; instance == nil || instance.State != StateRecoveryPending {
			t.Fatalf("config hash mismatch changed the in-memory quarantine: %#v", instance)
		}
	})
}

func TestClearRecoveryPendingWriteFailureKeepsQuarantineInMemory(t *testing.T) {
	manager, manifest, inspector := recoveryFixture(t)
	inspector.socketErr = os.ErrNotExist
	manager.inspector = inspector
	if _, err := manager.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	inspector.socketErr = nil
	if err := os.Chmod(manifest.VMDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(manifest.VMDir, 0o700) })

	result, err := manager.ClearRecoveryPending(manifest.Service)
	if err == nil || result.Outcome != RecoveryOutcomeAmbiguous {
		t.Fatalf("manifest write failure was not reported: result=%#v err=%v", result, err)
	}
	instance := manager.List()[manifest.Service]
	if instance == nil || instance.State != StateRecoveryPending {
		t.Fatalf("write failure cleared in-memory quarantine: %#v", instance)
	}
	if err := manager.Start(context.Background(), instance.Config); err == nil {
		t.Fatal("start proceeded after durable recovery write failed")
	}
}

func TestClearRecoveryPendingKeepsStartBlockedWithoutHoldingManagerLock(t *testing.T) {
	manager, manifest, inspector := recoveryFixture(t)
	blocking := &blockingRecoveryInspector{fakeProcessInspector: inspector, entered: make(chan struct{}), release: make(chan struct{})}
	manager.inspector = blocking
	manager.launcher = &recordingLauncher{}
	manager.addRecoveryPending(manifest.Service, manifest, "operator requested recovery")
	startService := manifest.Config
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blocking.release) }) }
	defer release()

	clearDone := make(chan error, 1)
	go func() {
		_, err := manager.ClearRecoveryPending(manifest.Service)
		clearDone <- err
	}()
	select {
	case <-blocking.entered:
	case <-time.After(time.Second):
		t.Fatal("clearance did not inspect ownership")
	}
	startDone := make(chan error, 1)
	go func() { startDone <- manager.Start(context.Background(), startService) }()
	select {
	case err := <-startDone:
		if err == nil || !strings.Contains(err.Error(), "ambiguous surviving state") {
			t.Fatalf("start did not refuse the quarantined service: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("start blocked on process inspection while recovery held the manager lock")
	}
	release()
	if err := <-clearDone; err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background(), startService); err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("start did not run after the dead state was removed: %v", err)
	}
}

func TestAdoptedMonitorDoesNotQuarantineInstanceReplacedDuringInspection(t *testing.T) {
	manager, manifest, _ := recoveryFixture(t)
	oldInstance := instanceFromManifest(manifest, StateRunning, "")
	manager.instances[manifest.Service] = oldInstance
	inspector := &delayedRecoveryInspector{
		entered: make(chan struct{}, 1), release: make(chan struct{}), err: errors.New("stale ownership inspection"),
	}
	manager.inspector = inspector

	finished := make(chan struct{})
	go func() {
		manager.monitorAdopted(manifest.Service, manifest.InstanceID)
		close(finished)
	}()
	select {
	case <-inspector.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("adopted monitor did not begin process inspection")
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(inspector.release) }) }
	defer release()

	newInstance := instanceFromManifest(manifest, StateRunning, "")
	manager.mu.Lock()
	manager.instances[manifest.Service] = newInstance
	manager.mu.Unlock()
	release()

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("adopted monitor did not finish after its stale inspection")
	}
	manager.mu.Lock()
	current := manager.instances[manifest.Service]
	manager.mu.Unlock()
	if current != newInstance || current.State != StateRunning || current.LastError != "" {
		t.Fatalf("stale inspection changed the replacement instance: %#v", current)
	}
	durable, err := readManifest(manifestPath(manifest.VMDir))
	if err != nil {
		t.Fatal(err)
	}
	if durable.LastError != "" || durable.Lifecycle != lifecycleRunning {
		t.Fatalf("stale inspection changed durable ownership state: %#v", durable)
	}
}

func TestClearRecoveryPendingRejectsPathTraversalBeforeManagerLookup(t *testing.T) {
	manager := NewManager("/firecracker", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, service := range []string{"", ".", "..", "../other", `a\b`, "/tmp/other"} {
		t.Run(service, func(t *testing.T) {
			if _, err := manager.ClearRecoveryPending(service); err == nil {
				t.Fatalf("invalid service name %q was accepted", service)
			}
		})
	}
}

type blockingRecoveryInspector struct {
	*fakeProcessInspector
	entered chan struct{}
	release chan struct{}
}

type delayedRecoveryInspector struct {
	fakeProcessInspector
	entered chan struct{}
	release chan struct{}
	err     error
}

func (d *delayedRecoveryInspector) Inspect(int) (processIdentity, error) {
	select {
	case d.entered <- struct{}{}:
	default:
	}
	<-d.release
	return processIdentity{}, d.err
}

func (b *blockingRecoveryInspector) Inspect(pid int) (processIdentity, error) {
	select {
	case <-b.entered:
	default:
		close(b.entered)
	}
	<-b.release
	return processIdentity{}, errProcessNotFound
}

func TestClearRecoveryPendingUsesOnlyTheNamedVMDirectory(t *testing.T) {
	manager, manifest, inspector := recoveryFixture(t)
	otherDir := filepath.Join(manager.stateDir, "vms", "other")
	if err := os.MkdirAll(otherDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, "kept"), []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspector.errors[manifest.PID] = errors.New("temporary process inspection failure")
	manager.inspector = inspector
	if _, err := manager.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	delete(inspector.errors, manifest.PID)
	delete(inspector.identities, manifest.PID)
	if _, err := manager.ClearRecoveryPending(manifest.Service); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(otherDir, "kept")); err != nil {
		t.Fatalf("clearance touched another service directory: %v", err)
	}
}
