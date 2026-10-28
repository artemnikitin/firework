package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/volume"
)

const recoveryInterventionCommand = "firework-agent clear-recovery <service>"

// Recover discovers Firecracker processes that survived an agent restart.
// It returns the names of safely adopted services so the reconciler can
// recreate idempotent host networking and health-monitor state.
func (m *Manager) Recover(_ context.Context, desired config.NodeConfig) ([]string, error) {
	m.mu.Lock()
	if m.recovered {
		m.mu.Unlock()
		return nil, nil
	}
	m.mu.Unlock()

	root := filepath.Join(m.stateDir, "vms")
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// Leave the guard unarmed so a transient read failure retries instead of
		// disabling recovery, which would make every later start refuse the
		// durable state it never got to inspect.
		return nil, fmt.Errorf("read VM state directory: %w", err)
	}
	// Arm the guard even when the directory does not exist yet. On a fresh node
	// it appears only once this process creates its first VM, and a later pass
	// would then "recover" VMs this very process owns and started.
	m.mu.Lock()
	m.recovered = true
	m.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	desiredByName := make(map[string]config.ServiceConfig, len(desired.Services))
	for _, service := range desired.Services {
		desiredByName[service.Name] = service
	}
	seenIDs := make(map[string]string)
	var adopted []string
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		m.mu.Lock()
		_, owned := m.instances[name]
		m.mu.Unlock()
		if owned {
			// This process already tracks the service, so its VM is not a
			// survivor of an earlier agent and must not be adopted again.
			continue
		}
		vmDir := filepath.Join(root, name)
		path := manifestPath(vmDir)
		manifest, readErr := readManifest(path)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				var handled bool
				manifest, handled, err = m.recoverLegacy(name, vmDir, desiredByName)
				if err != nil {
					errs = append(errs, err)
				}
				if handled {
					continue
				}
			} else {
				m.addRecoveryPending(name, &instanceManifest{
					SchemaVersion: manifestSchemaVersion, Service: name, VMDir: vmDir,
					SocketPath: filepath.Join(vmDir, "firecracker.sock"),
				}, fmt.Sprintf("cannot read ownership manifest: %v", readErr))
				continue
			}
		}
		if errors.Is(readErr, os.ErrNotExist) {
			if err := m.writeRecoveryManifest(path, manifest); err != nil {
				m.addRecoveryPendingWithCandidate(name, manifest, manifest, "legacy migration persistence failed")
				errs = append(errs, err)
				continue
			}
		}
		if err := verifyRecoveryManifestPaths(name, vmDir, manifest); err != nil {
			m.addRecoveryPending(name, manifest, "ownership manifest paths do not match its state directory")
			continue
		}
		if err := verifyRecoveryManifestIntegrity(manifest); err != nil {
			m.addRecoveryPending(name, manifest, "ownership manifest service configuration hash does not match")
			continue
		}
		if other, duplicate := seenIDs[manifest.InstanceID]; duplicate {
			m.addRecoveryPending(name, manifest, fmt.Sprintf("instance ID is also owned by service %s", other))
			m.markExistingRecoveryPending(other, fmt.Sprintf("instance ID is also owned by service %s", name))
			continue
		}
		seenIDs[manifest.InstanceID] = name
		// Publish quarantine first. Restart and explicit retry then use exactly
		// the same ownership, absence, persistence and shutdown policy.
		m.addRecoveryPending(name, manifest, "recovery verification pending")
		result, resolveErr := m.ClearRecoveryPending(name)
		if resolveErr != nil {
			m.mu.Lock()
			if current := m.instances[name]; current != nil {
				current.LastError = resolveErr.Error()
			}
			m.mu.Unlock()
			if strings.Contains(resolveErr.Error(), "persist") {
				errs = append(errs, resolveErr)
			}
			m.logger.Warn("VM recovery remains pending", "service", name, "error", resolveErr)
			continue
		}
		if result.Outcome == RecoveryOutcomeReadopted {
			adopted = append(adopted, name)
		}
	}
	if len(errs) > 0 {
		return adopted, fmt.Errorf("VM recovery had %d error(s): %v", len(errs), errs)
	}
	return adopted, nil
}

// repairOwnership re-proves ownership of a VM whose recorded identity cannot be
// validated, and rewrites the identity from the live process when it succeeds.
//
// A manifest can hold an identity that never described its Firecracker process:
// an inspection that raced the launcher's exec records either nothing or the
// launcher's own identity, and neither can ever validate again even though the
// VM is running and healthy. The command line settles that independently. It
// carries the instance ID, 128 bits generated once for this launch and never
// reused, so a process presenting it with this instance's socket and config
// paths is the process this manifest was written for.
func (m *Manager) repairOwnership(name string, manifest *instanceManifest, cause error) bool {
	if errors.Is(cause, errIdentityNotRecorded) || manifest.Legacy || manifest.InstanceID == "" {
		return false
	}
	identity, err := m.inspector.Inspect(manifest.PID)
	if err != nil || !matchesOwnedArguments(identity, manifest) {
		return false
	}
	// A recorded start time that disagrees is the PID-reuse signal the manifest
	// exists to catch, and it means this PID is a different incarnation from the
	// one described. Only an unrecorded identity, or one belonging to the same
	// incarnation before it exec'd, is repairable. A boot ID that disagrees is
	// already classified as a process that did not survive.
	if manifest.ProcessStart != 0 && identity.StartTicks != manifest.ProcessStart {
		return false
	}
	applyProcessIdentity(manifest, identity)
	m.logger.Warn("repaired the recorded identity of a surviving microVM",
		"service", name, "pid", manifest.PID, "error", cause)
	return true
}

func applyProcessIdentity(manifest *instanceManifest, identity processIdentity) {
	manifest.HostBootID = identity.HostBootID
	manifest.ProcessStart = identity.StartTicks
	manifest.Executable = identity.Executable
	manifest.ExecutableDev = identity.ExecutableDev
	manifest.ExecutableIno = identity.ExecutableIno
}

func cloneInstanceManifest(manifest *instanceManifest) *instanceManifest {
	if manifest == nil {
		return nil
	}
	clone := *manifest
	clone.Volumes = append([]volume.PreparedVolume(nil), manifest.Volumes...)
	return &clone
}

func (m *Manager) recoverLegacy(name, vmDir string, desired map[string]config.ServiceConfig) (*instanceManifest, bool, error) {
	service, wanted := desired[name]
	partial := &instanceManifest{
		SchemaVersion: manifestSchemaVersion, Service: name, VMDir: vmDir,
		SocketPath: filepath.Join(vmDir, "firecracker.sock"), ConfigPath: filepath.Join(vmDir, "vm-config.json"),
	}
	if !wanted {
		m.addRecoveryPending(name, partial, "legacy VM state has no ownership manifest and no desired service configuration")
		return nil, true, nil
	}
	matches, err := m.inspector.FindByArguments(partial.SocketPath, partial.ConfigPath)
	if err != nil {
		m.addRecoveryPending(name, partial, fmt.Sprintf("cannot inspect legacy VM processes: %v", err))
		return nil, true, nil
	}
	expectedExecutable, err := filepath.EvalSymlinks(m.firecrackerBin)
	if err != nil {
		m.addRecoveryPending(name, partial, fmt.Sprintf("cannot resolve configured Firecracker executable: %v", err))
		return nil, true, nil
	}
	expectedExecutable, err = filepath.Abs(expectedExecutable)
	if err != nil {
		return nil, true, fmt.Errorf("resolve Firecracker executable path: %w", err)
	}
	var owned []processIdentity
	for _, identity := range matches {
		if identity.Executable == expectedExecutable {
			owned = append(owned, identity)
		}
	}
	if len(owned) == 0 {
		if len(matches) > 0 {
			m.addRecoveryPending(name, partial, "legacy process arguments match but its executable is not the configured Firecracker binary")
			return nil, true, nil
		}
		if err := m.removeRecoveryState(partial); err != nil {
			return nil, true, fmt.Errorf("clean stale legacy state for %s: %w", name, err)
		}
		return nil, true, nil
	}
	if len(owned) != 1 {
		m.addRecoveryPending(name, partial, fmt.Sprintf("found %d matching legacy Firecracker processes", len(owned)))
		return nil, true, nil
	}
	identity := owned[0]
	instanceID, err := newInstanceID()
	if err != nil {
		return nil, true, err
	}
	hash, err := serviceConfigHash(service)
	if err != nil {
		return nil, true, err
	}
	return &instanceManifest{
		SchemaVersion: manifestSchemaVersion, Service: name, InstanceID: instanceID,
		Lifecycle: lifecycleRunning, Config: service, ConfigHash: hash, PID: identity.PID,
		HostBootID: identity.HostBootID, ProcessStart: identity.StartTicks,
		Executable: identity.Executable, ExecutableDev: identity.ExecutableDev, ExecutableIno: identity.ExecutableIno,
		SocketPath: partial.SocketPath, ConfigPath: partial.ConfigPath, VMDir: vmDir,
		Launcher: "direct", Legacy: true, StartedAt: time.Now().UTC(),
	}, false, nil
}

func instanceFromManifest(manifest *instanceManifest, state State, lastError string) *Instance {
	serviceConfig := manifest.Config
	if serviceConfig.Name == "" {
		serviceConfig.Name = manifest.Service
	}
	return &Instance{
		Name: manifest.Service, Config: serviceConfig, State: state, PID: manifest.PID,
		LastError: lastError, SocketPath: manifest.SocketPath,
		Volumes:    append([]volume.PreparedVolume(nil), manifest.Volumes...),
		instanceID: manifest.InstanceID, manifest: manifest,
	}
}

func (m *Manager) addRecoveryPending(name string, manifest *instanceManifest, reason string) {
	m.addRecoveryPendingWithCandidate(name, manifest, nil, reason)
}

func (m *Manager) addRecoveryPendingWithCandidate(name string, manifest, candidate *instanceManifest, reason string) {
	manifest.LastError = reason
	m.mu.Lock()
	instance := instanceFromManifest(manifest, StateRecoveryPending, reason)
	instance.recoveryCandidate = cloneInstanceManifest(candidate)
	m.instances[name] = instance
	m.mu.Unlock()
	m.logRecoveryIntervention(name, reason)
}

func (m *Manager) logRecoveryIntervention(service string, reason any) {
	m.logger.Error("VM recovery requires operator intervention",
		"service", service, "error", reason, "command", recoveryInterventionCommand)
}

func (m *Manager) persistReconstructedIdentity(service, vmDir, path string, original, candidate *instanceManifest) error {
	if err := verifyRecoveryManifestPaths(service, vmDir, original); err != nil {
		return err
	}
	if err := verifyRecoveryManifestPaths(service, vmDir, candidate); err != nil {
		return err
	}
	if err := verifyRecoveryManifestIntegrity(original); err != nil {
		return err
	}
	if err := verifyRecoveryManifestIntegrity(candidate); err != nil {
		return err
	}
	if err := verifyRecoveryStateFiles(vmDir, path); err != nil {
		return err
	}
	durable, err := readManifest(path)
	if err != nil {
		return fmt.Errorf("read durable ownership manifest before identity repair: %w", err)
	}
	if !sameRecoveryIdentity(original, durable) {
		return fmt.Errorf("durable ownership identity changed before identity repair")
	}
	if m.recoveryManifestWriter != nil {
		return m.recoveryManifestWriter(path, candidate)
	}
	return m.writeRecoveryManifest(path, candidate)
}

func verifyRecoveryManifestIntegrity(manifest *instanceManifest) error {
	hash, err := serviceConfigHash(manifest.Config)
	if err != nil || hash != manifest.ConfigHash {
		return fmt.Errorf("ownership manifest service configuration hash does not match")
	}
	return nil
}

func (m *Manager) markExistingRecoveryPending(name, reason string) {
	m.mu.Lock()
	found := false
	if instance := m.instances[name]; instance != nil {
		found = true
		instance.State = StateRecoveryPending
		instance.LastError = reason
		if instance.manifest != nil {
			instance.manifest.LastError = reason
			_ = m.writeRecoveryManifest(manifestPath(instance.manifest.VMDir), instance.manifest)
		}
	}
	m.mu.Unlock()
	if found {
		m.logRecoveryIntervention(name, reason)
	}
}

// monitorAdopted observes a process without requiring it to remain a child of
// the current agent. Identity mismatch is quarantined instead of interpreted
// as exit, which prevents PID reuse from mutating or killing another process.
func (m *Manager) monitorAdopted(name, instanceID string) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		m.mu.Lock()
		instance := m.instances[name]
		if instance == nil || instance.instanceID != instanceID || instance.State != StateRunning {
			m.mu.Unlock()
			return
		}
		manifest := *instance.manifest
		m.mu.Unlock()

		err := validateOwnedProcess(m.inspector, &manifest)
		if err == nil {
			continue
		}
		if !errors.Is(err, errProcessNotFound) {
			m.quarantineIfCurrent(name, instance, instanceID,
				fmt.Errorf("process identity changed while monitoring: %w", err))
			return
		}

		m.mu.Lock()
		current := m.instances[name]
		if current != instance || current == nil || current.instanceID != instanceID || current.State != StateRunning {
			m.mu.Unlock()
			return
		}
		current.State = StateFailed
		current.PID = 0
		current.LastError = "adopted Firecracker process exited"
		if current.manifest != nil {
			current.manifest.PID = 0
			current.manifest.Lifecycle = lifecycleFailed
			current.manifest.LastError = current.LastError
			if err := m.writeRecoveryManifest(manifestPath(current.manifest.VMDir), current.manifest); err != nil {
				m.logger.Error("failed to persist exited adopted VM state", "service", name, "error", err)
			}
		}
		m.mu.Unlock()
		return
	}
}

func (m *Manager) quarantineIfCurrent(name string, expected *Instance, instanceID string, reason error) bool {
	m.mu.Lock()
	instance := m.instances[name]
	if instance != expected || instance == nil || instance.instanceID != instanceID || instance.State != StateRunning {
		m.mu.Unlock()
		return false
	}
	if instance.manifest != nil {
		instance.manifest.LastError = reason.Error()
		if err := m.writeRecoveryManifest(manifestPath(instance.manifest.VMDir), instance.manifest); err != nil {
			reason = fmt.Errorf("%w; persisting quarantine state: %v", reason, err)
		}
	}
	instance.State = StateRecoveryPending
	instance.LastError = reason.Error()
	m.mu.Unlock()
	m.logRecoveryIntervention(name, reason)
	return true
}
