package vm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

func TestRound3LiveSystemdOwnerDoesNotDependOnUnitInspection(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	f.Launcher = "systemd"
	f.LauncherUnit = "firework-vm-" + f.InstanceID + ".service"
	if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
		t.Fatal(err)
	}
	m.recoveryUnitInspector = func(*instanceManifest) (int, bool, error) {
		return 0, false, errors.New("temporary systemctl bus failure")
	}
	adopted, err := m.Recover(context.Background(), config.NodeConfig{})
	if err != nil || len(adopted) != 1 || adopted[0] != f.Service {
		t.Fatalf("fully validated live owner was blocked by systemctl: adopted=%v err=%v instance=%#v", adopted, err, m.List()[f.Service])
	}
	if got := m.List()[f.Service]; got == nil || got.State != StateRunning || got.PID != f.PID {
		t.Fatalf("live systemd owner was not adopted: %#v", got)
	}
}

func TestRound3RecoverRetriesSocketAndUnitFailures(t *testing.T) {
	for _, failure := range []string{"socket unavailable", "systemctl unavailable", "unit still activating"} {
		t.Run(failure, func(t *testing.T) {
			m, f, inspector := recoveryFixture(t)
			m.inspector = inspector
			if failure == "socket unavailable" {
				inspector.socketErr = os.ErrNotExist
			} else {
				f.Launcher = "systemd"
				f.LauncherUnit = "firework-vm-" + f.InstanceID + ".service"
				if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
					t.Fatal(err)
				}
				delete(inspector.identities, f.PID)
				m.recoveryUnitInspector = func(*instanceManifest) (int, bool, error) {
					if failure == "systemctl unavailable" {
						return 0, false, errors.New("systemctl unavailable")
					}
					return 0, false, nil
				}
			}
			if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
				t.Fatal(err)
			}
			if got := m.List()[f.Service]; got == nil || got.State != StateRecoveryPending {
				t.Fatalf("uncertain owner did not stay pending: %#v", got)
			}
			if _, err := os.Stat(f.VMDir); err != nil {
				t.Fatalf("uncertain owner lost its state: %v", err)
			}
			inspector.socketErr = nil
			m.recoveryUnitInspector = func(*instanceManifest) (int, bool, error) { return 0, true, nil }
			if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
				t.Fatal(err)
			}
			if failure == "socket unavailable" {
				if got := m.List()[f.Service]; got == nil || got.State != StateRunning {
					t.Fatalf("next Recover did not readopt a ready survivor: %#v", got)
				}
			} else {
				if got := m.List()[f.Service]; got != nil {
					t.Fatalf("next Recover did not clear a proven absent owner: %#v", got)
				}
				if _, err := os.Stat(f.VMDir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("proven dead unit retained state: %v", err)
				}
			}
		})
	}
}

func TestRound3PIDZeroLaunchRecovery(t *testing.T) {
	for _, launcher := range []string{"direct", "systemd"} {
		t.Run(launcher, func(t *testing.T) {
			m, f, inspector := recoveryFixture(t)
			m.inspector = inspector
			f.PID, f.HostBootID, f.ProcessStart = 0, "", 0
			f.Executable, f.ExecutableDev, f.ExecutableIno = "", 0, 0
			f.Launcher = launcher
			f.Lifecycle = lifecycleStarting
			if launcher == "direct" {
				delete(inspector.identities, 42)
			} else {
				f.Lifecycle = lifecycleFailed
				f.LauncherUnit = "firework-vm-" + f.InstanceID + ".service"
				m.recoveryUnitInspector = func(*instanceManifest) (int, bool, error) { return 42, false, nil }
			}
			if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
				t.Fatal(err)
			}
			if launcher == "direct" {
				if got := m.List()[f.Service]; got != nil {
					t.Fatalf("unclaimed direct starting state was retained: %#v", got)
				}
				if _, err := os.Stat(f.VMDir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unclaimed direct starting directory remains: %v", err)
				}
				return
			}
			if got := m.List()[f.Service]; got == nil || got.State != StateRunning || got.PID != 42 {
				t.Fatalf("live failed systemd launch was not reconstructed: %#v", got)
			}
			durable, err := readManifest(manifestPath(f.VMDir))
			if err != nil || durable.Lifecycle != lifecycleRunning || validateOwnedProcess(inspector, durable) != nil {
				t.Fatalf("reconstructed systemd identity is not durable: manifest=%#v err=%v", durable, err)
			}
		})
	}
}

func TestRound3RecoveryNeverDeletesAnotherServicesDirectory(t *testing.T) {
	m, victim, inspector := recoveryFixture(t)
	m.inspector = inspector
	delete(inspector.identities, victim.PID)
	victim.Lifecycle, victim.PID = lifecycleStopped, 0
	otherDir := filepath.Join(m.stateDir, "vms", "other")
	if err := writeManifest(manifestPath(otherDir), victim); err != nil {
		t.Fatal(err)
	}
	// Keep the victim out of discovery, while the foreign manifest still names
	// its directory. Neither startup recovery nor explicit clearance may trust it.
	if err := os.Remove(manifestPath(victim.VMDir)); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(victim.VMDir, "must-survive")
	if err := os.WriteFile(marker, []byte("victim"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	for _, resolve := range []func(string) (RecoveryResult, error){m.ClearRecoveryPending, m.ForgetRecoveryPending} {
		if _, err := resolve("other"); err == nil {
			t.Fatal("foreign manifest was accepted")
		}
		if contents, err := os.ReadFile(marker); err != nil || string(contents) != "victim" {
			t.Fatalf("foreign manifest deleted victim state: contents=%q err=%v", contents, err)
		}
	}
}

func TestRound3RecoveryCannotDeleteStateThroughSymlink(t *testing.T) {
	for _, link := range []string{"vms parent", "service directory"} {
		t.Run(link, func(t *testing.T) {
			m, f, inspector := recoveryFixture(t)
			m.inspector = inspector
			delete(inspector.identities, f.PID)
			marker := filepath.Join(f.VMDir, "protected-data")
			if err := os.WriteFile(marker, []byte("must survive"), 0o600); err != nil {
				t.Fatal(err)
			}
			linkedPath := f.VMDir
			if link == "vms parent" {
				linkedPath = filepath.Dir(f.VMDir)
			}
			target := filepath.Join(t.TempDir(), "protected-state")
			if err := os.Rename(linkedPath, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, linkedPath); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
				t.Fatal(err)
			}
			for _, resolve := range []func(string) (RecoveryResult, error){m.ClearRecoveryPending, m.ForgetRecoveryPending} {
				if _, err := resolve(f.Service); err == nil {
					t.Fatal("symlinked runtime directory was accepted for deletion")
				}
				if contents, err := os.ReadFile(marker); err != nil || string(contents) != "must survive" {
					t.Fatalf("recovery deleted state behind a symlink: contents=%q err=%v", contents, err)
				}
			}
		})
	}
}

func TestRound3CorrectedMalformedManifestRetriesAutomatically(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	if err := os.WriteFile(manifestPath(f.VMDir), []byte("{incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if got := m.List()[f.Service]; got == nil || got.State != StateRecoveryPending {
		t.Fatalf("malformed manifest was not quarantined: %#v", got)
	}
	if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
		t.Fatal(err)
	}
	inspector.socketErr = os.ErrNotExist
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if got := m.List()[f.Service]; got == nil || got.State != StateRecoveryPending || got.PID != f.PID {
		t.Fatalf("corrected durable identity did not replace the malformed placeholder: %#v", got)
	}
	inspector.socketErr = nil
	adopted, err := m.Recover(context.Background(), config.NodeConfig{})
	if err != nil || len(adopted) != 1 || adopted[0] != f.Service {
		t.Fatalf("corrected manifest never became eligible for automatic adoption: adopted=%v err=%v", adopted, err)
	}
	if got := m.List()[f.Service]; got == nil || got.State != StateRunning || got.instanceID != f.InstanceID {
		t.Fatalf("corrected live VM was not adopted: %#v", got)
	}
}

func TestRound3LegacyWithoutDesiredCanClearAfterClaimantExits(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	if err := os.Remove(manifestPath(f.VMDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ClearRecoveryPending(f.Service); err == nil {
		t.Fatal("legacy state with a live claimant was cleared")
	}
	delete(inspector.identities, f.PID)
	result, err := m.ClearRecoveryPending(f.Service)
	if err != nil || result.Outcome != RecoveryOutcomeCleared {
		t.Fatalf("absent legacy state could not be cleared without desired config: %#v %v", result, err)
	}
	if _, err := os.Stat(f.VMDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy state remains after absence proof: %v", err)
	}
}

func TestRound3LegacyMigrationCanClearIfProcessDiesBeforePersistenceRetry(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	if err := os.Remove(manifestPath(f.VMDir)); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(m.stateDir, "firecracker")
	if err := os.WriteFile(binary, []byte("test binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.firecrackerBin = binary
	executable, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	identity := inspector.identities[f.PID]
	identity.Executable = executable
	identity.CommandLine = []string{binary, "--api-sock", f.SocketPath, "--config-file", f.ConfigPath}
	inspector.identities[f.PID] = identity
	inspector.find = []processIdentity{identity}
	m.recoveryManifestWriter = func(string, *instanceManifest) error { return errors.New("disk temporarily unavailable") }
	if _, err := m.Recover(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{f.Config}}); err == nil {
		t.Fatal("migration persistence failure was not returned")
	}
	if got := m.List()[f.Service]; got == nil || got.State != StateRecoveryPending || got.recoveryCandidate == nil {
		t.Fatalf("migration did not retain its pending candidate: %#v", got)
	}
	delete(inspector.identities, f.PID)
	inspector.find = nil
	m.recoveryManifestWriter = nil
	result, err := m.ClearRecoveryPending(f.Service)
	if err != nil || result.Outcome != RecoveryOutcomeCleared {
		t.Fatalf("dead legacy candidate stalled persistence retry: result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(f.VMDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dead migration directory remains: %v", err)
	}
}

type round3BlockingScanner struct {
	*fakeProcessInspector
	entered chan struct{}
	release chan struct{}
}

func (i *round3BlockingScanner) FindByAnyArgument(socket, config string) ([]processIdentity, error) {
	close(i.entered)
	<-i.release
	return i.fakeProcessInspector.FindByAnyArgument(socket, config)
}

func TestRound3ClearDoesNotHoldManagerLockDuringExternalInspection(t *testing.T) {
	for _, inspection := range []string{"systemd", "process scan"} {
		t.Run(inspection, func(t *testing.T) {
			m, f, inspector := recoveryFixture(t)
			delete(inspector.identities, f.PID)
			m.inspector = inspector
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			if inspection == "systemd" {
				f.Launcher = "systemd"
				f.LauncherUnit = "firework-vm-" + f.InstanceID + ".service"
				m.recoveryUnitInspector = func(*instanceManifest) (int, bool, error) {
					close(entered)
					<-release
					return 0, true, nil
				}
			} else {
				m.inspector = &round3BlockingScanner{fakeProcessInspector: inspector, entered: entered, release: release}
			}
			if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
				t.Fatal(err)
			}
			m.addRecoveryPending(f.Service, f, "pending inspection")
			cleared := make(chan error, 1)
			go func() { _, err := m.ClearRecoveryPending(f.Service); cleared <- err }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("Clear did not begin external inspection")
			}
			listed := make(chan struct{})
			go func() { m.List(); close(listed) }()
			select {
			case <-listed:
			case <-time.After(time.Second):
				unblock()
				t.Fatal("external recovery inspection blocked heartbeat/list access to Manager.mu")
			}
			unblock()
			select {
			case err := <-cleared:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("Clear did not complete after inspection was released")
			}
		})
	}
}

func TestRound3ForgetStoppingRequiresCompleteAbsenceProof(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	f.Lifecycle = lifecycleStopping
	if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
		t.Fatal(err)
	}
	m.addRecoveryPending(f.Service, f, "shutdown acknowledgement interrupted")
	if _, err := m.ForgetRecoveryPending(f.Service); err == nil {
		t.Fatal("Forget accepted a live owner")
	}
	delete(inspector.identities, f.PID)
	inspector.findErr = errors.New("process scan unavailable")
	if _, err := m.ForgetRecoveryPending(f.Service); err == nil {
		t.Fatal("Forget accepted an incomplete claimant scan")
	}
	inspector.findErr = nil
	if _, err := m.ClearRecoveryPending(f.Service); err == nil {
		t.Fatal("ordinary Clear discarded unresolved shutdown acknowledgement")
	}
	if _, err := os.Stat(f.VMDir); err != nil {
		t.Fatalf("failed absence checks removed shutdown metadata: %v", err)
	}
	result, err := m.ForgetRecoveryPending(f.Service)
	if err != nil || result.Outcome != RecoveryOutcomeCleared || result.State != StateStopped {
		t.Fatalf("Forget did not clear a proven absent stopping owner: %#v %v", result, err)
	}
	if _, err := os.Stat(f.VMDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Forget retained proven absent state: %v", err)
	}
}

func TestRound3FailedAndAbortedCreationCanLaunchOnRetry(t *testing.T) {
	for _, failure := range []string{"volume preparation", "configuration", "manifest write", "aborted preparation"} {
		t.Run(failure, func(t *testing.T) {
			stateDir := t.TempDir()
			binary := filepath.Join(stateDir, "firecracker")
			m := NewManager(binary, stateDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
			launcher := &specCapturingLauncher{inner: &execRacingLauncher{pid: 3637}}
			m.launcher = launcher
			m.inspector = &execRacingInspector{launcher: launcher, pid: 3637, binary: binary}
			stopAdoptedMonitors(t, m)
			service := config.ServiceConfig{Name: "app", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}
			switch failure {
			case "volume preparation":
				invalid := service
				invalid.Volumes = []config.VolumeConfig{{Name: "data"}}
				if err := m.Start(context.Background(), invalid); err == nil {
					t.Fatal("expected missing storage configuration to reject preparation")
				}
			case "configuration":
				invalid := service
				invalid.KernelArgs = string(make([]byte, 4096))
				if err := m.Start(context.Background(), invalid); err == nil {
					t.Fatal("expected invalid config to reject creation")
				}
			case "manifest write":
				m.recoveryManifestWriter = func(string, *instanceManifest) error { return errors.New("disk full") }
				if err := m.Start(context.Background(), service); err == nil {
					t.Fatal("expected manifest persistence to reject creation")
				}
				m.recoveryManifestWriter = nil
			case "aborted preparation":
				id, dir, socket, err := m.beginStart(service)
				if err != nil {
					t.Fatal(err)
				}
				if err := m.Stop(service.Name); err != nil {
					t.Fatal(err)
				}
				if err := m.finishStart(context.Background(), service, id, dir, socket, nil); !errors.Is(err, ErrStartAborted) {
					t.Fatalf("aborted phase 3 returned %v", err)
				}
			}
			if _, err := os.Stat(filepath.Join(stateDir, "vms", service.Name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("pre-launch failure left runtime state that can quarantine retry: %v", err)
			}
			if err := m.Start(context.Background(), service); err != nil {
				t.Fatalf("retry did not reach a successful launch: %v", err)
			}
			if got := m.List()[service.Name]; got == nil || got.State != StateRunning {
				t.Fatalf("retry did not become running: %#v", got)
			}
		})
	}
}

// The direct launcher and Wait monitor are real. Only /proc identity and socket
// readiness are simulated so these regressions run on development hosts too.
type round3DirectInspector struct {
	*launchedSpecInspector
	mu  sync.Mutex
	err error
}

func (i *round3DirectInspector) Inspect(pid int) (processIdentity, error) {
	i.mu.Lock()
	err := i.err
	i.mu.Unlock()
	if err != nil {
		return processIdentity{}, err
	}
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return processIdentity{}, errProcessNotFound
	}
	return i.launchedSpecInspector.Inspect(pid)
}

func (i *round3DirectInspector) setError(err error) {
	i.mu.Lock()
	i.err = err
	i.mu.Unlock()
}

func TestRound3DirectChildRetainsWaitMonitorThroughRecovery(t *testing.T) {
	for _, quarantine := range []string{"running identity uncertainty", "interrupted stop"} {
		t.Run(quarantine, func(t *testing.T) {
			stateDir := t.TempDir()
			binary := filepath.Join(stateDir, "firecracker")
			if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			m := NewManager(binary, stateDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
			launcher := &specCapturingLauncher{inner: &directLauncher{binary: binary}}
			m.launcher = launcher
			inspector := &round3DirectInspector{launchedSpecInspector: &launchedSpecInspector{launcher: launcher, executable: binary}}
			m.inspector = inspector
			service := config.ServiceConfig{Name: "app", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}
			if err := m.Start(context.Background(), service); err != nil {
				t.Fatal(err)
			}
			started := m.List()[service.Name]
			pid := started.PID
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			if started.startID == "" {
				t.Fatal("real Start did not publish its direct-child startID")
			}
			if quarantine == "interrupted stop" {
				inspector.setError(errors.New("temporary process inspection failure"))
				if err := m.Stop(service.Name); err == nil {
					t.Fatal("uncertain Stop did not quarantine the live direct child")
				}
				inspector.setError(nil)
				if _, err := m.ClearRecoveryPending(service.Name); err == nil {
					t.Fatal("interrupted Stop was incorrectly readopted")
				}
				if _, err := m.ForgetRecoveryPending(service.Name); err == nil {
					t.Fatal("live direct child was forgotten")
				}
			} else {
				m.quarantine(service.Name, started.manifest, errors.New("temporary identity uncertainty"))
				result, err := m.ClearRecoveryPending(service.Name)
				if err != nil || result.Outcome != RecoveryOutcomeReadopted {
					t.Fatalf("live direct child retry = %#v %v", result, err)
				}
				if got := m.List()[service.Name]; got.startID != started.startID {
					t.Fatalf("readoption lost ownership of the existing Wait monitor: startID=%q want=%q", got.startID, started.startID)
				}
			}
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				got := m.List()[service.Name]
				if got.PID == 0 && got.State == StateFailed {
					durable, err := readManifest(manifestPath(got.manifest.VMDir))
					if err != nil || durable.PID != 0 || durable.Lifecycle != lifecycleFailed {
						t.Fatalf("real Wait monitor did not persist the exit: manifest=%#v err=%v", durable, err)
					}
					return
				}
				time.Sleep(time.Millisecond)
			}
			t.Fatalf("real direct child lost its Wait monitor after quarantine: %#v", m.List()[service.Name])
		})
	}
}
