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
	// StateStarting is published while a start has released the manager lock
	// to prepare volumes. It exists so List reports something truthful during
	// a multi-minute mkfs or resize rather than reporting nothing at all.
	StateStarting State = "starting"
	// StateStartAborting means a Stop or Remove arrived while a start was in
	// its unlocked preparation phase. The start's own final phase observes it
	// and cleans up without launching anything.
	StateStartAborting State = "start_aborting"
)

var (
	// ErrStartAborted reports that a start was cancelled by a concurrent Stop
	// or Remove before anything was launched. It is a benign race, not a
	// fault: the caller must retry rather than record a reconcile failure.
	ErrStartAborted = errors.New("start aborted by a concurrent stop or remove")
	// ErrStartInProgress reports that another start for the same service is
	// still in its preparation phase. Like ErrStartAborted this is a retry
	// signal, not a failure.
	ErrStartInProgress = errors.New("start already in progress")
)

// IsStartRace reports whether an error is one of the benign start-barrier
// races. Callers use it to classify a reconciliation as incomplete — retry on
// the next tick without advancing the applied revision — rather than failed.
func IsStartRace(err error) bool {
	return errors.Is(err, ErrStartAborted) || errors.Is(err, ErrStartInProgress)
}

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
	// startID identifies one Start attempt. Phase 3 validates against it
	// rather than against the service name, so a placeholder that was cleared
	// and replaced by a later attempt is never mistaken for one's own.
	startID string
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
// It also returns any size requests it refused. A rejection never goes through
// setVolumeError: it is a decision rather than a fault, and recording it there
// would set a volume_failed reason code and trigger the blanket overwrite that
// relabels every one of the service's volumes as errored.
func (m *Manager) Preflight(ctx context.Context, svc config.ServiceConfig) ([]volume.Rejection, error) {
	if len(svc.Volumes) == 0 {
		m.setVolumeError(svc.Name, nil)
		return nil, nil
	}
	if err := validateVolumeKernelArgs(svc); err != nil {
		m.setVolumeError(svc.Name, err)
		return nil, err
	}
	if m.volumeManager == nil {
		err := fmt.Errorf("service %s declares volumes but agent storage is not configured", svc.Name)
		m.setVolumeError(svc.Name, err)
		return nil, err
	}
	rejections, err := m.volumeManager.Preflight(ctx, svc)
	m.setVolumeError(svc.Name, err)
	return rejections, err
}

// VolumeRejections returns the agent's current per-volume refusal snapshot.
func (m *Manager) VolumeRejections() map[string]volume.Rejection {
	if m.volumeManager == nil {
		return nil
	}
	return m.volumeManager.Rejections()
}

// SeedVolumeRejectionsForTest installs a refusal snapshot without running a
// real filesystem operation, so the status and convergence paths can be
// exercised without a live pool.
func (m *Manager) SeedVolumeRejectionsForTest(rejections map[string]volume.Rejection) {
	if m.volumeManager == nil {
		return
	}
	m.volumeManager.SeedRejectionsForTest(rejections)
}

// NormalizeVolumes clamps a desired node configuration to the sizes the node
// is actually able to serve, before anything else in the tick reads it.
func (m *Manager) NormalizeVolumes(services []config.ServiceConfig) {
	if m.volumeManager == nil {
		return
	}
	m.volumeManager.NormalizeVolumes(services)
}

// VolumeError returns the latest persistent-volume preparation failure for a
// service. It remains visible until a later successful preflight or start.
func (m *Manager) VolumeError(service string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.volumeErrors[service]
}

// clampToPrepared substitutes the effective configuration — size and
// generation — for every volume whose request was refused, so the instance the
// next tick compares against describes what is actually running. The refused
// request is reported from the volume manager's rejection snapshot instead,
// which is what the control-plane acknowledgement matches on.
func clampToPrepared(svc config.ServiceConfig, prepared []volume.PreparedVolume) config.ServiceConfig {
	effective := make(map[string]volume.PreparedVolume, len(prepared))
	for _, preparedVolume := range prepared {
		if preparedVolume.Rejected {
			effective[preparedVolume.LogicalID] = preparedVolume
		}
	}
	if len(effective) == 0 {
		return svc
	}
	clamped := svc
	clamped.Volumes = append([]config.VolumeConfig(nil), svc.Volumes...)
	for i := range clamped.Volumes {
		if preparedVolume, ok := effective[svc.Name+"/"+clamped.Volumes[i].Name]; ok {
			clamped.Volumes[i].SizeBytes = preparedVolume.SizeBytes
			clamped.Volumes[i].ResizeGeneration = preparedVolume.ResizeGeneration
		}
	}
	return clamped
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

// defaultKernelArgs is the boot command line used when a service declares none.
const defaultKernelArgs = "console=ttyS0 reboot=k panic=1 pci=off"

// buildBootArgs composes a service's kernel command line and enforces the
// command-line length limit.
//
// It is the single place boot args are built. Preflight's early check and the
// launch path previously constructed the payload separately and drifted: the
// length check lived only in the update path, so ActionCreate could boot a VM
// with an over-long command line instead of failing with a clear error.
func buildBootArgs(svc config.ServiceConfig, guestVolumes []guestVolume) (string, error) {
	kernelArgs := svc.KernelArgs
	if kernelArgs == "" {
		kernelArgs = defaultKernelArgs
	}
	if len(guestVolumes) > 0 {
		payload, err := json.Marshal(guestVolumePayload{Version: 1, Volumes: guestVolumes})
		if err != nil {
			return "", fmt.Errorf("marshal guest volume payload: %w", err)
		}
		arg := "firework.volumes64=" + base64.RawURLEncoding.EncodeToString(payload)
		kernelArgs = insertBeforeApplicationSeparator(kernelArgs, arg)
	}
	if len(kernelArgs) > maxKernelCommandLineBytes {
		what := "kernel command line"
		if len(guestVolumes) > 0 {
			what = "kernel command line with volume payload"
		}
		return "", fmt.Errorf("service %s: %s is %d bytes; maximum is %d", svc.Name, what, len(kernelArgs), maxKernelCommandLineBytes)
	}
	return kernelArgs, nil
}

// guestVolumesFromConfig builds the guest payload entries from declared
// volumes, for the preflight that runs before anything has been prepared.
func guestVolumesFromConfig(volumes []config.VolumeConfig) ([]guestVolume, error) {
	ordered := append([]config.VolumeConfig(nil), volumes...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	guestVolumes := make([]guestVolume, 0, len(ordered))
	for i, declared := range ordered {
		device, err := guestBlockDevice(i)
		if err != nil {
			return nil, err
		}
		guestVolumes = append(guestVolumes, guestVolume{
			Name: declared.Name, Device: device, MountPath: declared.MountPath, Type: declared.Type,
		})
	}
	return guestVolumes, nil
}

func validateVolumeKernelArgs(svc config.ServiceConfig) error {
	guestVolumes, err := guestVolumesFromConfig(svc.Volumes)
	if err != nil {
		return err
	}
	_, err = buildBootArgs(svc, guestVolumes)
	return err
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
// Start launches a microVM for a service.
//
// It runs in three phases so the manager lock is not held across volume
// preparation, which can spend minutes in mkfs.ext4, e2fsck, or resize2fs.
// Holding the lock there stalled every reader of it — including the heartbeat
// goroutine, which reaches it through List — so a node went stale precisely
// while it was busy resizing its own services' volumes.
//
// Releasing the lock opens a window in which a Stop or Remove can arrive, so
// the phases are governed by a barrier rather than by extra branches:
//
//	absent             -> StateStarting       phase 1 publishes the placeholder
//	StateStarting      -> StateRunning        phase 3, own startID still present
//	StateStarting      -> StateStartAborting  Stop or Remove during phase 2
//	StateStartAborting -> absent              phase 3 observes the abort
//	StateStarting      -> absent              phase 2 failed
//
// Phase 3 confirms ownership before any side effect: no manifest is written
// and nothing is launched unless the placeholder is still this attempt's own.
func (m *Manager) Start(ctx context.Context, svc config.ServiceConfig) error {
	startID, vmDir, socketPath, err := m.beginStart(svc)
	if err != nil {
		return err
	}

	// Phase 2 runs without the manager lock. Its side effects — a created or
	// resized volume image — are durable, retained state by design, so they
	// are deliberately not rolled back when the start is aborted: the next
	// start reuses them.
	prepared, err := m.prepareVolumes(ctx, svc)
	if err != nil {
		m.discardStart(svc.Name, startID)
		return err
	}

	return m.finishStart(ctx, svc, startID, vmDir, socketPath, prepared)
}

// beginStart is phase 1: it takes the entry checks and publishes the starting
// placeholder under the manager lock.
func (m *Manager) beginStart(svc config.ServiceConfig) (startID, vmDir, socketPath string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if inst, exists := m.instances[svc.Name]; exists {
		switch inst.State {
		case StateRecoveryPending:
			return "", "", "", fmt.Errorf("service %s has ambiguous surviving state: %s", svc.Name, inst.LastError)
		case StateRunning, StateStopping:
			return "", "", "", fmt.Errorf("service %s is already active (pid %d, state %s)", svc.Name, inst.PID, inst.State)
		case StateStarting, StateStartAborting:
			// A start already holds this name. Rejecting here is what keeps
			// the agent API and shutdown paths from racing the reconcile loop.
			return "", "", "", fmt.Errorf("service %s is in state %s: %w", svc.Name, inst.State, ErrStartInProgress)
		}
	}

	m.logger.Info("starting microVM", "service", svc.Name, "vcpus", svc.VCPUs, "memory_mb", svc.MemoryMB)

	vmDir = filepath.Join(m.stateDir, "vms", svc.Name)
	if err := m.reclaimUnownedState(svc.Name, vmDir); err != nil {
		return "", "", "", err
	}
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return "", "", "", fmt.Errorf("creating vm dir: %w", err)
	}

	socketPath = filepath.Join(vmDir, "firecracker.sock")
	// Remove stale socket if it exists.
	_ = os.Remove(socketPath)

	startID, err = newInstanceID()
	if err != nil {
		return "", "", "", err
	}
	m.instances[svc.Name] = &Instance{
		Name: svc.Name, Config: svc, State: StateStarting,
		SocketPath: socketPath, startID: startID,
	}
	return startID, vmDir, socketPath, nil
}

// prepareVolumes is phase 2. It runs without the manager lock, so it records
// volume errors through setVolumeError rather than writing the map directly.
func (m *Manager) prepareVolumes(ctx context.Context, svc config.ServiceConfig) ([]volume.PreparedVolume, error) {
	if len(svc.Volumes) == 0 {
		return nil, nil
	}
	if m.volumeManager == nil {
		err := fmt.Errorf("service %s declares volumes but agent storage is not configured", svc.Name)
		m.setVolumeError(svc.Name, err)
		return nil, err
	}
	prepared, err := m.volumeManager.Prepare(ctx, svc)
	if err != nil {
		m.setVolumeError(svc.Name, err)
		return nil, fmt.Errorf("preparing volumes: %w", err)
	}
	return prepared, nil
}

// discardStart removes this attempt's placeholder after a phase-2 failure. It
// leaves a placeholder belonging to some later attempt alone.
func (m *Manager) discardStart(name, startID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if inst, exists := m.instances[name]; exists && inst.startID == startID {
		delete(m.instances, name)
	}
}

// finishStart is phase 3: it re-takes the manager lock, confirms this attempt
// still owns the placeholder, and only then writes durable state or launches.
func (m *Manager) finishStart(ctx context.Context, svc config.ServiceConfig, startID, vmDir, socketPath string, prepared []volume.PreparedVolume) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	placeholder, exists := m.instances[svc.Name]
	if !exists || placeholder.startID != startID || placeholder.State != StateStarting {
		if exists && placeholder.startID == startID {
			delete(m.instances, svc.Name)
		}
		m.logger.Info("start aborted before launch", "service", svc.Name)
		return fmt.Errorf("service %s: %w", svc.Name, ErrStartAborted)
	}

	// Clamp the service configuration to what was actually prepared. A refused
	// shrink prepares successfully at the applied size, and everything
	// downstream — the config hash, the Firecracker config, the ownership
	// manifest, and the instance the next tick compares against — must describe
	// that effective configuration. Storing it here is what makes needsUpdate
	// compare equal on the following tick rather than one convergence cycle
	// later.
	svc = clampToPrepared(svc, prepared)

	configPath, err := m.writeVMConfig(vmDir, svc, prepared)
	if err != nil {
		delete(m.instances, svc.Name)
		return fmt.Errorf("writing vm config: %w", err)
	}

	configHash, err := serviceConfigHash(svc)
	if err != nil {
		delete(m.instances, svc.Name)
		return err
	}
	instanceID, err := newInstanceID()
	if err != nil {
		delete(m.instances, svc.Name)
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
		delete(m.instances, svc.Name)
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
		delete(m.instances, svc.Name)
		return fmt.Errorf("starting firecracker: %w", err)
	}
	manifest.PID = launched.PID
	manifest.Launcher = launched.Launcher
	manifest.LauncherUnit = launched.Unit
	if identityErr := m.recordLaunchedIdentity(manifest, launched); identityErr != nil {
		if m.abandonLaunch(svc.Name, manifest, launched, identityErr) {
			m.instances[svc.Name] = instanceFromManifest(manifest, StateRecoveryPending, manifest.LastError)
		} else {
			delete(m.instances, svc.Name)
		}
		return fmt.Errorf("confirming launched process identity: %w", identityErr)
	}
	manifest.Lifecycle = lifecycleRunning
	if err := writeManifest(manifestPath(vmDir), manifest); err != nil {
		_ = m.launcher.Stop(manifest, syscall.SIGKILL)
		delete(m.instances, svc.Name)
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
		startID:    startID,
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
	// A start that is still preparing volumes has launched nothing, so there
	// is no process to signal. Mark the attempt aborted and return
	// immediately rather than waiting for a possibly multi-minute mkfs — not
	// stalling shutdown behind volume work is the point of the barrier.
	// Marking is idempotent so shutdown and the agent API can both issue a
	// stop without the loser seeing an error for work the winner already did.
	if inst.State == StateStarting || inst.State == StateStartAborting {
		inst.State = StateStartAborting
		m.mu.Unlock()
		m.logger.Info("aborting in-flight start instead of stopping", "service", name)
		return nil
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
	aborting := exists && (inst.State == StateStarting || inst.State == StateStartAborting)
	if aborting {
		inst.State = StateStartAborting
	}
	m.mu.Unlock()

	// Removing the VM state directory while phase 2 runs is safe: volume
	// preparation writes only under the storage pool, and writeVMConfig — the
	// only writer of this directory — lives in phase 3, which will abort. The
	// placeholder is left for phase 3 to clear, so a second Remove before then
	// takes this same branch and also succeeds.
	if aborting {
		m.logger.Info("aborting in-flight start instead of removing", "service", name)
		if err := os.RemoveAll(filepath.Join(m.stateDir, "vms", name)); err != nil {
			return fmt.Errorf("removing vm dir: %w", err)
		}
		return nil
	}

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
	// The same builder Preflight uses, so an over-long command line now fails
	// on create too rather than only on update.
	kernelArgs, err := buildBootArgs(svc, guestVolumes)
	if err != nil {
		return "", err
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
