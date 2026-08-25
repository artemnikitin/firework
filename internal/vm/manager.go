package vm

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/volume"
)

const maxKernelCommandLineBytes = 2047

const (
	// launchIdentityTimeout bounds how long a launch waits for its process to
	// exec into Firecracker before the start is abandoned. Warm launches are
	// observed to take ~110ms; a cold systemd-run takes ~1.2s.
	launchIdentityTimeout = 3 * time.Second
	// launchIdentityInterval is how often the launched PID is re-inspected while
	// waiting for the exec to complete.
	launchIdentityInterval = 25 * time.Millisecond
	// launchExitConfirmTimeout bounds how long an abandoned launch is given to
	// exit before its state directory is left in place for recovery. Both
	// budgets are spent holding the manager lock on the reconcile path, so they
	// are kept well inside one 30s reconcile interval and the API server's 10s
	// write timeout. Overrunning them is benign: the launch is retried on the
	// next tick.
	launchExitConfirmTimeout = 2 * time.Second
)

// State represents the lifecycle state of a microVM.
type State string

const (
	StateRunning  State = "running"
	StateStopping State = "stopping"
	StateStopped  State = "stopped"
	StateFailed   State = "failed"
	// StateRecoveryPending means durable state exists but ownership could not
	// be proved. Firework preserves the process and files and blocks duplicates.
	StateRecoveryPending State = "recovery_pending"
)

// Instance represents a running Firecracker microVM.
type Instance struct {
	// Name is the service name from the config.
	Name string
	// Config is the desired service configuration.
	Config config.ServiceConfig
	// State is the current lifecycle state.
	State State
	// PID is the Firecracker process ID (0 if not running).
	PID int
	// LastError is the bounded-at-publication process failure reported by Wait.
	LastError string
	// SocketPath is the path to the Firecracker API socket.
	SocketPath string
	// Volumes is the last successfully prepared persistent-volume set.
	Volumes []volume.PreparedVolume

	instanceID string
	manifest   *instanceManifest
}

// Manager manages the lifecycle of Firecracker microVMs on the local host.
type Manager struct {
	firecrackerBin string
	stateDir       string
	logger         *slog.Logger
	volumeManager  *volume.Manager
	launcher       processLauncher
	inspector      processInspector
	// identityTimeout bounds waiting for a launched process to exec, and
	// exitConfirmTimeout bounds waiting for an abandoned launch to go away.
	// They are fields so tests do not have to sleep out the real budgets.
	identityTimeout    time.Duration
	exitConfirmTimeout time.Duration

	mu           sync.Mutex
	instances    map[string]*Instance
	volumeErrors map[string]string
	recovered    bool
}

// NewManager creates a new VM manager.
func NewManager(firecrackerBin, stateDir string, logger *slog.Logger) *Manager {
	return NewManagerWithVolumes(firecrackerBin, stateDir, logger, nil)
}

// NewManagerWithVolumes creates a VM manager with persistent-volume support.
func NewManagerWithVolumes(firecrackerBin, stateDir string, logger *slog.Logger, volumeManager *volume.Manager) *Manager {
	return &Manager{
		firecrackerBin: firecrackerBin,
		stateDir:       stateDir,
		logger:         logger,
		volumeManager:  volumeManager,
		launcher:       chooseLauncher(firecrackerBin),
		inspector:      osProcessInspector{},

		identityTimeout:    launchIdentityTimeout,
		exitConfirmTimeout: launchExitConfirmTimeout,

		instances:    make(map[string]*Instance),
		volumeErrors: make(map[string]string),
	}
}

// Preflight validates persistent volumes without changing them. Reconciliation
// calls this before stopping an existing VM so a failed resize leaves it live.
func (m *Manager) Preflight(ctx context.Context, svc config.ServiceConfig) error {
	if len(svc.Volumes) == 0 {
		m.setVolumeError(svc.Name, nil)
		return nil
	}
	if err := validateVolumeKernelArgs(svc); err != nil {
		m.setVolumeError(svc.Name, err)
		return err
	}
	if m.volumeManager == nil {
		err := fmt.Errorf("service %s declares volumes but agent storage is not configured", svc.Name)
		m.setVolumeError(svc.Name, err)
		return err
	}
	err := m.volumeManager.Preflight(ctx, svc)
	m.setVolumeError(svc.Name, err)
	return err
}

// VolumeError returns the latest persistent-volume preparation failure for a
// service. It remains visible until a later successful preflight or start.
func (m *Manager) VolumeError(service string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.volumeErrors[service]
}

func (m *Manager) setVolumeError(service string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		delete(m.volumeErrors, service)
		return
	}
	m.volumeErrors[service] = err.Error()
}

func validateVolumeKernelArgs(svc config.ServiceConfig) error {
	volumes := append([]config.VolumeConfig(nil), svc.Volumes...)
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
	guestVolumes := make([]guestVolume, 0, len(volumes))
	for i, volume := range volumes {
		device, err := guestBlockDevice(i)
		if err != nil {
			return err
		}
		guestVolumes = append(guestVolumes, guestVolume{
			Name: volume.Name, Device: device, MountPath: volume.MountPath, Type: volume.Type,
		})
	}
	payload, err := json.Marshal(guestVolumePayload{Version: 1, Volumes: guestVolumes})
	if err != nil {
		return fmt.Errorf("marshal guest volume payload: %w", err)
	}
	kernelArgs := svc.KernelArgs
	if kernelArgs == "" {
		kernelArgs = "console=ttyS0 reboot=k panic=1 pci=off"
	}
	kernelArgs = insertBeforeApplicationSeparator(kernelArgs, "firework.volumes64="+base64.RawURLEncoding.EncodeToString(payload))
	if len(kernelArgs) > maxKernelCommandLineBytes {
		return fmt.Errorf("service %s: kernel command line with volume payload is %d bytes; maximum is %d", svc.Name, len(kernelArgs), maxKernelCommandLineBytes)
	}
	return nil
}

// List returns a snapshot of all known VM instances.
func (m *Manager) List() map[string]*Instance {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make(map[string]*Instance, len(m.instances))
	for k, v := range m.instances {
		cp := *v
		result[k] = &cp
	}
	return result
}

// Start launches a new Firecracker microVM for the given service config.
func (m *Manager) Start(ctx context.Context, svc config.ServiceConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if inst, exists := m.instances[svc.Name]; exists {
		if inst.State == StateRecoveryPending {
			return fmt.Errorf("service %s has ambiguous surviving state: %s", svc.Name, inst.LastError)
		}
		if inst.State == StateRunning || inst.State == StateStopping {
			return fmt.Errorf("service %s is already active (pid %d, state %s)", svc.Name, inst.PID, inst.State)
		}
	}

	m.logger.Info("starting microVM", "service", svc.Name, "vcpus", svc.VCPUs, "memory_mb", svc.MemoryMB)

	vmDir := filepath.Join(m.stateDir, "vms", svc.Name)
	if err := m.reclaimUnownedState(svc.Name, vmDir); err != nil {
		return err
	}
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return fmt.Errorf("creating vm dir: %w", err)
	}

	socketPath := filepath.Join(vmDir, "firecracker.sock")
	// Remove stale socket if it exists.
	_ = os.Remove(socketPath)

	var prepared []volume.PreparedVolume
	var err error
	if len(svc.Volumes) > 0 {
		if m.volumeManager == nil {
			err := fmt.Errorf("service %s declares volumes but agent storage is not configured", svc.Name)
			m.volumeErrors[svc.Name] = err.Error()
			return err
		}
		prepared, err = m.volumeManager.Prepare(ctx, svc)
		if err != nil {
			m.volumeErrors[svc.Name] = err.Error()
			return fmt.Errorf("preparing volumes: %w", err)
		}
	}

	configPath, err := m.writeVMConfig(vmDir, svc, prepared)
	if err != nil {
		return fmt.Errorf("writing vm config: %w", err)
	}

	configHash, err := serviceConfigHash(svc)
	if err != nil {
		return err
	}
	instanceID, err := newInstanceID()
	if err != nil {
		return err
	}
	launcherKind, launcherUnit := startingLauncherMetadata(m.launcher, instanceID)
	manifest := &instanceManifest{
		SchemaVersion: manifestSchemaVersion, Service: svc.Name, InstanceID: instanceID,
		Lifecycle: lifecycleStarting, Config: svc, ConfigHash: configHash,
		SocketPath: socketPath, ConfigPath: configPath, VMDir: vmDir,
		Launcher: launcherKind, LauncherUnit: launcherUnit,
		StartedAt: time.Now().UTC(), Volumes: append([]volume.PreparedVolume(nil), prepared...),
	}
	if err := writeManifest(manifestPath(vmDir), manifest); err != nil {
		return err
	}
	launched, err := m.launcher.Launch(ctx, launchSpec{
		InstanceID: instanceID, SocketPath: socketPath, ConfigPath: configPath,
		LogPath: filepath.Join(vmDir, "firecracker.log"),
	})
	if err != nil {
		manifest.Lifecycle = lifecycleFailed
		manifest.LastError = err.Error()
		_ = writeManifest(manifestPath(vmDir), manifest)
		return fmt.Errorf("starting firecracker: %w", err)
	}
	manifest.PID = launched.PID
	manifest.Launcher = launched.Launcher
	manifest.LauncherUnit = launched.Unit
	if identityErr := m.recordLaunchedIdentity(manifest, launched); identityErr != nil {
		if m.abandonLaunch(svc.Name, manifest, launched, identityErr) {
			m.instances[svc.Name] = instanceFromManifest(manifest, StateRecoveryPending, manifest.LastError)
		}
		return fmt.Errorf("confirming launched process identity: %w", identityErr)
	}
	manifest.Lifecycle = lifecycleRunning
	if err := writeManifest(manifestPath(vmDir), manifest); err != nil {
		_ = m.launcher.Stop(manifest, syscall.SIGKILL)
		return err
	}

	m.instances[svc.Name] = &Instance{
		Name:       svc.Name,
		Config:     svc,
		State:      StateRunning,
		PID:        launched.PID,
		SocketPath: socketPath,
		Volumes:    append([]volume.PreparedVolume(nil), prepared...),
		instanceID: instanceID,
		manifest:   manifest,
	}
	delete(m.volumeErrors, svc.Name)

	// Monitor the process in a goroutine.
	if launched.Cmd != nil {
		go m.monitor(svc.Name, instanceID, launched)
	} else {
		go m.monitorAdopted(svc.Name, instanceID)
	}

	m.logger.Info("microVM started", "service", svc.Name, "pid", launched.PID, "launcher", launched.Launcher)
	return nil
}

// reclaimUnownedState decides whether a leftover state directory blocks a fresh
// start. Durable state that may still own a process is never reclaimed, but
// state whose process is recorded as gone is removed: otherwise a single failed
// launch blocks every later start of that service for the life of the process.
// A failed direct launch with no PID is also safe to reclaim because exec.Start
// did not create a child. The same is not true for systemd: systemd-run can
// create a unit and then fail while Firework is waiting for MainPID, so a
// missing recorded PID does not prove that no process exists.
func (m *Manager) reclaimUnownedState(service, vmDir string) error {
	if _, loaded := m.instances[service]; loaded {
		return nil
	}
	manifest, err := readManifest(manifestPath(vmDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil && manifest.PID == 0 &&
		(manifest.Lifecycle == lifecycleStopped ||
			(manifest.Lifecycle == lifecycleFailed && manifest.Launcher == "direct")) {
		if removeErr := os.RemoveAll(vmDir); removeErr != nil {
			return fmt.Errorf("removing exited VM state for %s: %w", service, removeErr)
		}
		return nil
	}
	return fmt.Errorf("service %s has durable VM state that must be recovered before start", service)
}

// recordLaunchedIdentity waits for the launched process to exec into Firecracker
// and records the identity of that exact process.
//
// systemd reports a transient unit's MainPID at fork, before the child has
// exec'd, so inspecting it immediately either fails or captures systemd's own
// identity. Both outcomes persist a running manifest describing a process that
// does not exist at that identity, which no later validation can ever accept.
// The launched command line is the signal that the exec completed: it carries
// this instance's unique --id together with its socket and config paths.
func (m *Manager) recordLaunchedIdentity(manifest *instanceManifest, launched *launchedProcess) error {
	identity, err := awaitLaunchedIdentity(m.inspector, manifest, launched.PID, m.launchIdentityTimeout(), launchIdentityInterval)
	if err != nil {
		if _, host := m.inspector.(osProcessInspector); host && !processInspectionSupported {
			// Development hosts without /proc cannot prove ownership of any
			// process. Leaving the identity fields empty keeps the manifest
			// honest; every ownership check then fails closed as it does today.
			m.logger.Warn("host cannot inspect process identity, so VM ownership is unverifiable",
				"service", manifest.Service, "pid", launched.PID, "error", err)
			return nil
		}
		return err
	}
	applyProcessIdentity(manifest, identity)
	return nil
}

// awaitLaunchedIdentity polls a launched PID until it is running the exact
// command line Firework launched for this instance, and returns the identity
// read from that same inspection so the manifest it populates validates by
// construction.
func awaitLaunchedIdentity(inspector processInspector, manifest *instanceManifest, pid int, timeout, interval time.Duration) (processIdentity, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		identity, err := inspector.Inspect(pid)
		switch {
		case errors.Is(err, errProcessNotFound):
			return processIdentity{}, fmt.Errorf("process %d exited before it ran Firecracker: %w", pid, err)
		case err != nil:
			lastErr = fmt.Errorf("inspect process %d: %w", pid, err)
		case matchesOwnedArguments(identity, manifest):
			return identity, nil
		default:
			lastErr = fmt.Errorf("process %d is running %q rather than the launched Firecracker command line", pid, identity.Executable)
		}
		if !time.Now().Before(deadline) {
			return processIdentity{}, lastErr
		}
		time.Sleep(interval)
	}
}

// abandonLaunch stops a launch whose process identity could not be confirmed.
// The state directory is removed only once the process is proven gone: deleting
// it while an unidentified process may still be alive would let the next
// reconcile start a duplicate Firecracker against the same socket and TAP. It
// reports whether the ambiguous launch was retained so Start can expose it as
// recovery_pending while it still holds the manager lock.
func (m *Manager) abandonLaunch(service string, manifest *instanceManifest, launched *launchedProcess, cause error) bool {
	m.logger.Error("abandoning microVM launch with unprovable process identity",
		"service", service, "pid", launched.PID, "error", cause)
	// Signalling is unit-scoped for systemd launches and, for direct launches,
	// targets a child this process has not yet reaped, so neither can reach an
	// unrelated process that recycled the PID.
	if err := m.launcher.Stop(manifest, syscall.SIGKILL); err != nil {
		m.logger.Warn("could not signal abandoned microVM launch",
			"service", service, "pid", launched.PID, "error", err)
	}
	gone := false
	if launched.Cmd != nil {
		_ = launched.Cmd.Wait()
		if launched.LogFile != nil {
			launched.LogFile.Close()
		}
		gone = true
	} else {
		gone = awaitProcessExit(m.inspector, launched.PID, m.launchExitTimeout(), launchIdentityInterval)
	}
	if gone {
		if err := os.RemoveAll(manifest.VMDir); err != nil {
			m.logger.Error("could not remove abandoned microVM state",
				"service", service, "error", err)
		}
		return false
	}
	manifest.Lifecycle = lifecycleFailed
	manifest.LastError = fmt.Sprintf("launch identity is unprovable and the process did not exit: %v", cause)
	_ = writeManifest(manifestPath(manifest.VMDir), manifest)
	m.logger.Error("abandoned microVM launch did not exit; its state is retained for recovery",
		"service", service, "pid", launched.PID)
	return true
}

// launchIdentityTimeout and launchExitTimeout fall back to the package
// defaults, so a Manager built without the constructor cannot silently collapse
// the identity wait to a single pre-exec inspection.
func (m *Manager) launchIdentityTimeout() time.Duration {
	if m.identityTimeout <= 0 {
		return launchIdentityTimeout
	}
	return m.identityTimeout
}

func (m *Manager) launchExitTimeout() time.Duration {
	if m.exitConfirmTimeout <= 0 {
		return launchExitConfirmTimeout
	}
	return m.exitConfirmTimeout
}

func awaitProcessExit(inspector processInspector, pid int, timeout, interval time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := inspector.Inspect(pid); errors.Is(err, errProcessNotFound) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(interval)
	}
}

// Stop gracefully shuts down a running microVM.
func (m *Manager) Stop(name string) error {
	m.mu.Lock()
	inst, exists := m.instances[name]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("service %s not found", name)
	}
	if inst.State == StateRecoveryPending {
		err := fmt.Errorf("refusing to stop service %s: process ownership is ambiguous: %s", name, inst.LastError)
		m.mu.Unlock()
		return err
	}
	manifest := inst.manifest
	if manifest == nil {
		m.mu.Unlock()
		return fmt.Errorf("service %s has no ownership manifest", name)
	}
	pid := manifest.PID
	socketPath := manifest.SocketPath
	inst.State = StateStopping
	manifest.Lifecycle = lifecycleStopping
	if err := writeManifest(manifestPath(manifest.VMDir), manifest); err != nil {
		inst.State = StateRunning
		m.mu.Unlock()
		return err
	}
	// Process monitoring may clear the live manifest as soon as Wait reaps the
	// child. Keep an immutable identity snapshot for every validation and
	// signal in this stop operation; PID 0 must never reach os.FindProcess.
	ownedManifest := *manifest
	m.mu.Unlock()

	m.logger.Info("stopping microVM", "service", name, "pid", pid)

	if err := validateOwnedProcess(m.inspector, &ownedManifest); err != nil {
		if !errors.Is(err, errProcessNotFound) {
			m.quarantine(name, manifest, fmt.Errorf("ownership validation failed before stop: %w", err))
			return fmt.Errorf("refusing to signal pid %d: %w", pid, err)
		}
	} else {
		launcher := launcherForManifest(m.firecrackerBin, &ownedManifest)
		if err := launcher.Stop(&ownedManifest, syscall.SIGTERM); err != nil {
			m.logger.Warn("SIGTERM failed, sending SIGKILL", "service", name, "error", err)
			_ = launcher.Stop(&ownedManifest, syscall.SIGKILL)
		}

		exited, waitErr := waitForOwnedProcessExit(m.inspector, &ownedManifest, 5*time.Second)
		if waitErr != nil {
			m.quarantine(name, manifest, fmt.Errorf("process identity changed while stopping: %w", waitErr))
			return fmt.Errorf("refusing to signal pid %d again: %w", pid, waitErr)
		}
		if !exited {
			m.logger.Warn("microVM did not exit after SIGTERM, sending SIGKILL", "service", name, "pid", pid)
			_ = launcher.Stop(&ownedManifest, syscall.SIGKILL)
			exited, waitErr = waitForOwnedProcessExit(m.inspector, &ownedManifest, 2*time.Second)
			if waitErr != nil {
				m.quarantine(name, manifest, fmt.Errorf("process identity changed after SIGKILL: %w", waitErr))
				return fmt.Errorf("process identity changed after SIGKILL: %w", waitErr)
			}
			if !exited {
				return fmt.Errorf("process %d did not exit after SIGKILL", pid)
			}
		}
	}

	m.mu.Lock()
	inst.State = StateStopped
	inst.PID = 0
	manifest.Lifecycle = lifecycleStopped
	manifest.PID = 0
	_ = writeManifest(manifestPath(manifest.VMDir), manifest)
	m.mu.Unlock()

	// Clean up socket.
	_ = os.Remove(socketPath)

	m.logger.Info("microVM stopped", "service", name)
	return nil
}

// Remove stops (if running) and removes all state for a service.
func (m *Manager) Remove(name string) error {
	m.mu.Lock()
	inst, exists := m.instances[name]
	m.mu.Unlock()

	if exists && (inst.State == StateRunning || inst.State == StateStopping) {
		if err := m.Stop(name); err != nil {
			return fmt.Errorf("stopping VM during remove: %w", err)
		}
	}
	if exists && inst.State == StateRecoveryPending {
		return fmt.Errorf("refusing to remove service %s while recovery is pending: %s", name, inst.LastError)
	}

	m.mu.Lock()
	delete(m.instances, name)
	m.mu.Unlock()

	vmDir := filepath.Join(m.stateDir, "vms", name)
	if err := os.RemoveAll(vmDir); err != nil {
		return fmt.Errorf("removing vm dir: %w", err)
	}

	return nil
}

// IsRunning checks if the process for a service is still alive.
func (m *Manager) IsRunning(name string) bool {
	m.mu.Lock()
	inst, exists := m.instances[name]
	m.mu.Unlock()

	if !exists || inst.State != StateRunning {
		return false
	}
	if inst.manifest == nil {
		return false
	}
	manifest := *inst.manifest
	return validateOwnedProcess(m.inspector, &manifest) == nil
}

// monitor waits for the firecracker process to exit and updates state.
func (m *Manager) monitor(name, instanceID string, launched *launchedProcess) {
	defer launched.LogFile.Close()

	err := launched.Cmd.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()

	inst, exists := m.instances[name]
	if !exists || inst.instanceID != instanceID {
		return
	}

	if err != nil {
		// Stop() marks instances as stopped before process exit. In that case
		// a non-zero Wait result is expected and should not flip state to failed.
		if inst.State == StateStopped || inst.State == StateStopping {
			m.logger.Debug("microVM exited after stop", "service", name)
			inst.State = StateStopped
			inst.LastError = ""
		} else {
			m.logger.Error("microVM exited with error", "service", name, "error", err)
			inst.State = StateFailed
			inst.LastError = err.Error()
		}
	} else {
		m.logger.Info("microVM exited cleanly", "service", name)
		inst.State = StateStopped
	}
	inst.PID = 0
	if inst.manifest != nil {
		inst.manifest.PID = 0
		inst.manifest.LastError = inst.LastError
		if inst.State == StateFailed {
			inst.manifest.Lifecycle = lifecycleFailed
		} else {
			inst.manifest.Lifecycle = lifecycleStopped
		}
		_ = writeManifest(manifestPath(inst.manifest.VMDir), inst.manifest)
	}
}

func waitForOwnedProcessExit(inspector processInspector, manifest *instanceManifest, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := validateOwnedProcess(inspector, manifest); err != nil {
			if errors.Is(err, errProcessNotFound) {
				return true, nil
			}
			return false, err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false, nil
}

func newInstanceID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate instance ID: %w", err)
	}
	return hex.EncodeToString(data), nil
}

func (m *Manager) quarantine(name string, manifest *instanceManifest, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	manifest.LastError = err.Error()
	_ = writeManifest(manifestPath(manifest.VMDir), manifest)
	if inst := m.instances[name]; inst != nil {
		inst.State = StateRecoveryPending
		inst.LastError = err.Error()
	}
}

// writeVMConfig writes a Firecracker JSON config file for the given service.
func (m *Manager) writeVMConfig(vmDir string, svc config.ServiceConfig, prepared []volume.PreparedVolume) (string, error) {
	kernelArgs := svc.KernelArgs
	if kernelArgs == "" {
		kernelArgs = "console=ttyS0 reboot=k panic=1 pci=off"
	}

	sort.Slice(prepared, func(i, j int) bool { return prepared[i].LogicalID < prepared[j].LogicalID })
	drives := []firecrackerDrive{{DriveID: "rootfs", PathOnHost: svc.Image, IsRootDevice: true, IsReadOnly: false}}
	guestVolumes := make([]guestVolume, 0, len(prepared))
	for i, preparedVolume := range prepared {
		device, err := guestBlockDevice(i)
		if err != nil {
			return "", err
		}
		drives = append(drives, firecrackerDrive{
			DriveID: fmt.Sprintf("volume-%d", i), PathOnHost: preparedVolume.PathOnHost,
			IsRootDevice: false, IsReadOnly: false,
		})
		guestVolumes = append(guestVolumes, guestVolume{
			Name: filepath.Base(preparedVolume.LogicalID), Device: device,
			MountPath: preparedVolume.MountPath, Type: preparedVolume.Type,
		})
	}
	if len(guestVolumes) > 0 {
		payload, err := json.Marshal(guestVolumePayload{Version: 1, Volumes: guestVolumes})
		if err != nil {
			return "", fmt.Errorf("marshal guest volume payload: %w", err)
		}
		arg := "firework.volumes64=" + base64.RawURLEncoding.EncodeToString(payload)
		kernelArgs = insertBeforeApplicationSeparator(kernelArgs, arg)
	}

	var networkInterfaces []firecrackerNetworkInterface
	if svc.Network != nil {
		guestMAC := svc.Network.GuestMAC
		if guestMAC == "" {
			guestMAC = "AA:FC:00:00:00:01"
		}
		networkInterfaces = []firecrackerNetworkInterface{{IfaceID: "eth0", GuestMAC: guestMAC, HostDevName: svc.Network.Interface}}
	}

	vmConfig := firecrackerConfig{
		BootSource:        firecrackerBootSource{KernelImagePath: svc.Kernel, BootArgs: kernelArgs},
		Drives:            drives,
		MachineConfig:     firecrackerMachineConfig{VCPUCount: svc.VCPUs, MemSizeMiB: svc.MemoryMB},
		NetworkInterfaces: networkInterfaces,
		// The VirtIO-RNG device prevents guests without a usable hardware
		// random source from blocking application startup on /dev/random.
		// This matters for arm64 guests nested inside Lima/VZ in particular.
		Entropy: &firecrackerEntropyDevice{},
	}
	configJSON, err := json.MarshalIndent(vmConfig, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal firecracker config: %w", err)
	}

	configPath := filepath.Join(vmDir, "vm-config.json")
	if err := os.WriteFile(configPath, append(configJSON, '\n'), 0o644); err != nil {
		return "", err
	}
	return configPath, nil
}

type firecrackerConfig struct {
	BootSource        firecrackerBootSource         `json:"boot-source"`
	Drives            []firecrackerDrive            `json:"drives"`
	MachineConfig     firecrackerMachineConfig      `json:"machine-config"`
	NetworkInterfaces []firecrackerNetworkInterface `json:"network-interfaces,omitempty"`
	Entropy           *firecrackerEntropyDevice     `json:"entropy,omitempty"`
}

type firecrackerBootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type firecrackerDrive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type firecrackerMachineConfig struct {
	VCPUCount  int `json:"vcpu_count"`
	MemSizeMiB int `json:"mem_size_mib"`
}

type firecrackerEntropyDevice struct{}

type firecrackerNetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	GuestMAC    string `json:"guest_mac"`
	HostDevName string `json:"host_dev_name"`
}

type guestVolumePayload struct {
	Version int           `json:"version"`
	Volumes []guestVolume `json:"volumes"`
}

type guestVolume struct {
	Name      string            `json:"name"`
	Device    string            `json:"device"`
	MountPath string            `json:"mount_path"`
	Type      config.VolumeType `json:"type"`
}

func guestBlockDevice(index int) (string, error) {
	if index < 0 || index >= 25 {
		return "", fmt.Errorf("too many additional drives: %d", index+1)
	}
	return fmt.Sprintf("/dev/vd%c", 'b'+rune(index)), nil
}

func insertBeforeApplicationSeparator(args, value string) string {
	fields := strings.Fields(args)
	for i, field := range fields {
		if field == "--" {
			out := append([]string(nil), fields[:i]...)
			out = append(out, value)
			out = append(out, fields[i:]...)
			return strings.Join(out, " ")
		}
	}
	if args == "" {
		return value
	}
	return args + " " + value
}
