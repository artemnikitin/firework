package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/volume"
)

const recoveryInterventionCommand = "firework-agent clear-recovery <service>"

// Recover discovers Firecracker processes that survived an agent restart.
// It returns the names of safely adopted services so the reconciler can
// recreate idempotent host networking and health-monitor state.
func (m *Manager) Recover(ctx context.Context, desired config.NodeConfig) ([]string, error) {
	caller := ctx
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	root := filepath.Join(m.stateDir, "vms")
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// Leave the guard unarmed so a transient read failure retries instead of
		// disabling recovery, which would make every later start refuse the
		// durable state it never got to inspect.
		return nil, fmt.Errorf("read VM state directory: %w", err)
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	m.inventoryRecoveryEntries(root, entries)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	desiredByName := make(map[string]config.ServiceConfig, len(desired.Services))
	for _, service := range desired.Services {
		desiredByName[service.Name] = service
	}
	seenIDs := make(map[string]string)
	var adopted []string
	m.mu.Lock()
	cursor := m.recoveryCursor
	m.mu.Unlock()
	start := sort.Search(len(entries), func(i int) bool { return entries[i].Name() > cursor })
	for offset := range entries {
		if ctx.Err() != nil {
			return adopted, caller.Err()
		}
		entry := entries[(start+offset)%len(entries)]
		if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		name := entry.Name()
		m.mu.Lock()
		m.recoveryCursor = name
		m.mu.Unlock()
		if !m.reserveRecoveryDiscovery(name) {
			continue
		}
		func() {
			defer m.releaseRecoveryDiscovery(name)
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			m.mu.Lock()
			current, owned := m.instances[name]
			discoveryPending := owned && current.recoveryDiscoveryPending
			pendingRecovery := owned && current.State == StateRecoveryPending && !discoveryPending
			m.mu.Unlock()
			if pendingRecovery {
				result, err := m.resolveRecovery(ctx, name)
				if err == nil && (result.Outcome == RecoveryOutcomeReadopted || result.Outcome == RecoveryOutcomeCleared) {
					adopted = append(adopted, name)
				}
				if err != nil {
					m.recordRecoveryFailure(name, err)
				}
				return
			}
			if owned && !discoveryPending {
				// This process already tracks the service, so its VM is not a
				// survivor of an earlier agent and must not be adopted again.
				return
			}
			vmDir := filepath.Join(root, name)
			path := manifestPath(vmDir)
			manifest, readErr := readManifest(path)
			if readErr != nil {
				if errors.Is(readErr, os.ErrNotExist) {
					var handled bool
					manifest, handled, err = m.recoverLegacy(ctx, name, vmDir, desiredByName)
					if err != nil {
						m.recordRecoveryFailure(name, err)
					}
					if handled {
						return
					}
				} else {
					m.addRecoveryPending(name, &instanceManifest{
						SchemaVersion: manifestSchemaVersion, Service: name, VMDir: vmDir,
						SocketPath: filepath.Join(vmDir, "firecracker.sock"), ConfigPath: filepath.Join(vmDir, "vm-config.json"),
					}, fmt.Sprintf("cannot read ownership manifest: %v", readErr))
					return
				}
			}
			if errors.Is(readErr, os.ErrNotExist) {
				if err := m.writeRecoveryManifest(path, manifest); err != nil {
					m.addRecoveryPendingWithCandidate(name, manifest, manifest, "legacy migration persistence failed")
					m.recordRecoveryFailure(name, err)
					return
				}
			}
			if err := verifyRecoveryManifestPaths(name, vmDir, manifest); err != nil {
				m.addRecoveryPending(name, manifest, "ownership manifest paths do not match its state directory")
				return
			}
			if err := verifyRecoveryManifestIntegrity(manifest); err != nil {
				m.addRecoveryPending(name, manifest, "ownership manifest service configuration hash does not match")
				return
			}
			if other, duplicate := seenIDs[manifest.InstanceID]; duplicate {
				m.addRecoveryPending(name, manifest, fmt.Sprintf("instance ID is also owned by service %s", other))
				m.markExistingRecoveryPending(other, fmt.Sprintf("instance ID is also owned by service %s", name))
				return
			}
			seenIDs[manifest.InstanceID] = name
			// Publish quarantine first. Restart and explicit retry then use exactly
			// the same ownership, absence, persistence and shutdown policy.
			m.publishRecoveryPending(name, manifest, nil, "recovery verification pending", false)
			result, resolveErr := m.resolveRecovery(ctx, name)
			if resolveErr != nil {
				m.mu.Lock()
				if current := m.instances[name]; current != nil {
					current.LastError = resolveErr.Error()
				}
				m.mu.Unlock()

				m.logger.Warn("VM recovery remains pending", "service", name, "error", resolveErr)
				return
			}
			if result.Outcome == RecoveryOutcomeReadopted || result.Outcome == RecoveryOutcomeCleared {
				adopted = append(adopted, name)
			}
		}()

	}
	return adopted, nil
}

// DiscoverRecoveryState inventories runtime ownership before the agent allocates
// addresses or capacity. It performs no unit/process probes or cleanup. Unknown
// metadata remains visible and fenced until recovery can verify it.
func (m *Manager) DiscoverRecoveryState() error {
	root := filepath.Join(m.stateDir, "vms")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read VM state directory: %w", err)
	}
	m.inventoryRecoveryEntries(root, entries)
	return nil
}

func (m *Manager) inventoryRecoveryEntries(root string, entries []os.DirEntry) {
	// Fence every name before reading even the first manifest. A slow probe or
	// exhausted inspection budget must never make a survivor disappear from Plan.
	newEntries := make(map[string]*Instance)
	m.mu.Lock()
	for _, entry := range entries {
		name := entry.Name()
		if (!entry.IsDir() && entry.Type()&os.ModeSymlink == 0) || m.instances[name] != nil || m.recoveryReservations[name] {
			continue
		}
		vmDir := filepath.Join(root, name)
		partial := &instanceManifest{Service: name, VMDir: vmDir,
			SocketPath: filepath.Join(vmDir, "firecracker.sock"), ConfigPath: filepath.Join(vmDir, "vm-config.json")}
		instance := m.recoveryInstanceForService(name, partial, "recovery discovery pending")
		instance.recoveryDiscoveryPending = true
		m.instances[name] = instance
		newEntries[name] = instance
	}
	m.mu.Unlock()
	for name, placeholder := range newEntries {
		vmDir := filepath.Join(root, name)
		if verifyRecoveryDirectories(vmDir) != nil {
			continue
		}
		manifest, err := readManifest(manifestPath(vmDir))
		if err != nil || verifyRecoveryManifestPaths(name, vmDir, manifest) != nil || verifyRecoveryManifestIntegrity(manifest) != nil || manifest.Config.Name != name {
			continue
		}
		instance := m.recoveryInstanceForService(name, manifest, "recovery verification pending")
		instance.recoveryDiscoveryPending = true
		m.mu.Lock()
		if m.instances[name] == placeholder && !m.recoveryReservations[name] {
			m.instances[name] = instance
		}
		m.mu.Unlock()
	}
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
func (m *Manager) repairOwnership(ctx context.Context, name string, manifest *instanceManifest, cause error) bool {
	if errors.Is(cause, errIdentityNotRecorded) || manifest.Legacy || manifest.InstanceID == "" {
		return false
	}
	identity, err := inspectProcessContext(ctx, m.inspector, manifest.PID)
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

func (m *Manager) recoverLegacy(ctx context.Context, name, vmDir string, desired map[string]config.ServiceConfig) (*instanceManifest, bool, error) {
	service, wanted := desired[name]
	partial := &instanceManifest{
		SchemaVersion: manifestSchemaVersion, Service: name, VMDir: vmDir,
		SocketPath: filepath.Join(vmDir, "firecracker.sock"), ConfigPath: filepath.Join(vmDir, "vm-config.json"),
	}
	var matches []processIdentity
	var err error
	if scanner, ok := m.inspector.(interface {
		FindByArgumentsContext(context.Context, string, string) ([]processIdentity, error)
	}); ok {
		matches, err = scanner.FindByArgumentsContext(ctx, partial.SocketPath, partial.ConfigPath)
	} else {
		matches, err = m.inspector.FindByArguments(partial.SocketPath, partial.ConfigPath)
	}
	if err != nil {
		m.addRecoveryPending(name, partial, fmt.Sprintf("cannot inspect legacy VM processes: %v", err))
		return nil, true, nil
	}
	if len(matches) == 0 {
		if err := m.removeRecoveryStateContext(ctx, partial); err != nil {
			m.addRecoveryPending(name, partial, fmt.Sprintf("legacy runtime absence remains unproved: %v", err))
		} else {
			m.mu.Lock()
			delete(m.instances, name)
			m.mu.Unlock()
		}
		return nil, true, nil
	}
	if !wanted {
		m.addRecoveryPending(name, partial, "live legacy VM has no desired service configuration for adoption")
		return nil, true, nil
	}
	// Legacy launches have no instance ID. Their executable identity is
	// pinned from the live process, whose argv[0] must identify the configured
	// launch path. Resolving a symlink now would compare against the upgraded
	// binary instead of the executable that the surviving process launched.
	expectedExecutable, err := filepath.Abs(m.firecrackerBin)
	if err != nil {
		return nil, true, err
	}
	var owned []processIdentity
	for _, identity := range matches {
		if len(identity.CommandLine) > 0 && identity.CommandLine[0] == expectedExecutable {
			owned = append(owned, identity)
		}
	}
	if len(owned) == 0 {
		m.addRecoveryPending(name, partial, "legacy process arguments match but its executable is not the configured Firecracker binary")
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
	m.publishRecoveryPending(name, manifest, candidate, reason, true)
}

func (m *Manager) publishRecoveryPending(name string, manifest, candidate *instanceManifest, reason string, notify bool) {
	manifest.LastError = reason
	m.mu.Lock()
	instance := m.recoveryInstanceForService(name, manifest, reason)
	instance.recoveryCandidate = cloneInstanceManifest(candidate)
	m.instances[name] = instance
	m.mu.Unlock()
	if notify {
		m.logRecoveryIntervention(name, reason)
	}
}

func (m *Manager) logRecoveryIntervention(service string, reason any) {
	m.logger.Warn("VM recovery pending; verification will retry",
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
		m.waitRecoveryPublicationLocked(name)
		current := m.instances[name]
		if current != instance || current == nil || current.instanceID != instanceID || current.State != StateRunning {
			m.mu.Unlock()
			return
		}
		current.State = StateFailed
		current.exitedOwner = cloneInstanceManifest(current.manifest)
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
	m.waitRecoveryPublicationLocked(name)
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

func (m *Manager) resolveRecovery(ctx context.Context, service string) (RecoveryResult, error) {
	return m.resolveRecoveryReserved(ctx, service, false)
}

// Runtime identity is the directory key, even when a corrupt manifest claims
// another service. Keep the original manifest for verification, not planning.
func (m *Manager) recoveryInstanceForService(name string, manifest *instanceManifest, reason string) *Instance {
	instance := instanceFromManifest(manifest, StateRecoveryPending, reason)
	instance.Name = name
	instance.Config.Name = name
	instance.RecoveryConfigUnknown = manifest.Service != name || manifest.Config.Name != name ||
		verifyRecoveryManifestIntegrity(manifest) != nil || verifyRecoveryManifestPaths(name, filepath.Join(m.stateDir, "vms", name), manifest) != nil
	return instance
}

func (m *Manager) reserveRecoveryDiscovery(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.recoveryReservations == nil {
		m.recoveryReservations = make(map[string]bool)
	}
	if m.recoveryReservations[name] {
		return false
	}
	if current := m.instances[name]; current != nil && current.State != StateRecoveryPending {
		return false
	}
	m.recoveryReservations[name] = true
	return true
}
func (m *Manager) releaseRecoveryDiscovery(name string) {
	m.mu.Lock()
	delete(m.recoveryReservations, name)
	if m.recoveryCondition != nil {
		m.recoveryCondition.Broadcast()
	}
	m.mu.Unlock()
}

// Wait owns the process exit result but lets a reserved recovery transaction
// finish its filesystem commit before publishing it. Only this service waits;
// the condition releases Manager.mu while recovery performs disk I/O.
func (m *Manager) waitRecoveryPublicationLocked(name string) {
	for m.recoveryReservations[name] {
		if m.recoveryCondition == nil {
			m.recoveryCondition = sync.NewCond(&m.mu)
		}
		m.recoveryCondition.Wait()
	}
}

// Service-local failures remain retryable and do not block unrelated services.
func (m *Manager) recordRecoveryFailure(name string, err error) {
	m.mu.Lock()
	if instance := m.instances[name]; instance != nil && instance.State == StateRecoveryPending {
		instance.LastError = err.Error()
	}
	m.mu.Unlock()
	m.logger.Warn("VM recovery remains pending", "service", name, "error", err)
}
