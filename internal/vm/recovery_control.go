package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RecoveryOutcome describes the state transition made by ClearRecoveryPending.
type RecoveryOutcome string

const (
	RecoveryOutcomeNoop      RecoveryOutcome = "no_op"
	RecoveryOutcomeCleared   RecoveryOutcome = "cleared"
	RecoveryOutcomeReadopted RecoveryOutcome = "readopted"
	RecoveryOutcomeAmbiguous RecoveryOutcome = "still_ambiguous"
)

// RecoveryResult is the bounded result of resolving one service's quarantine.
type RecoveryResult struct {
	Service string          `json:"service"`
	Outcome RecoveryOutcome `json:"outcome"`
	State   State           `json:"state,omitempty"`
	Message string          `json:"message,omitempty"`
}

// ClearRecoveryPending rechecks one quarantined service and either safely
// adopts its live VM, clears its dead VM state, or leaves it quarantined.
func (m *Manager) ClearRecoveryPending(service string) (RecoveryResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.resolveRecoveryMode(ctx, service, false)
}

func (m *Manager) clearRecoveryPending(ctx context.Context, service string) (RecoveryResult, error) {
	return m.resolveRecoveryMode(ctx, service, false)
}

// ForgetRecoveryPending permits deletion of unresolved shutdown metadata only
// after the same complete unit/process absence proof. It never adopts a VM.
func (m *Manager) ForgetRecoveryPending(service string) (RecoveryResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.resolveRecoveryMode(ctx, service, true)
}

func (m *Manager) resolveRecoveryMode(ctx context.Context, service string, forget bool) (RecoveryResult, error) {
	if err := ValidateRecoveryServiceName(service); err != nil {
		return RecoveryResult{Service: service, Outcome: RecoveryOutcomeNoop}, err
	}
	if err := m.lockRecovery(ctx); err != nil {
		return recoveryAmbiguous(RecoveryResult{Service: service}, err.Error())
	}
	if m.recoveryReservations[service] {
		m.mu.Unlock()
		return recoveryAmbiguous(RecoveryResult{Service: service}, "recovery verification in progress")
	}
	if m.recoveryReservations == nil {
		m.recoveryReservations = make(map[string]bool)
	}
	m.recoveryReservations[service] = true
	m.mu.Unlock()
	defer m.releaseRecoveryDiscovery(service)
	return m.resolveRecoveryReserved(ctx, service, forget)
}

// The caller reserves the service for the whole transaction, so slow disk
// cleanup need not hold the manager lock or block unrelated VM operations.
var errRecoveryRetry = errors.New("recovery ownership snapshot advanced")

func (m *Manager) resolveRecoveryReserved(ctx context.Context, service string, forget bool) (RecoveryResult, error) {
	for {
		result, err := m.resolveRecoveryAttempt(ctx, service, forget)
		if !errors.Is(err, errRecoveryRetry) {
			return result, err
		}
	}
}

func (m *Manager) resolveRecoveryAttempt(ctx context.Context, service string, forget bool) (RecoveryResult, error) {
	result := RecoveryResult{Service: service, Outcome: RecoveryOutcomeNoop}
	if err := ValidateRecoveryServiceName(service); err != nil {
		return result, err
	}

	if err := m.lockRecovery(ctx); err != nil {
		return recoveryAmbiguous(result, err.Error())
	}
	instance := m.instances[service]
	if instance == nil {
		m.mu.Unlock()
		vmDir, path := m.recoveryManifestPath(service)
		manifest, err := readManifest(path)
		if errors.Is(err, os.ErrNotExist) {
			if _, statErr := os.Lstat(vmDir); errors.Is(statErr, os.ErrNotExist) {
				result.Message = "service has no runtime state"
				return result, nil
			}
		}
		if err != nil {
			if _, statErr := os.Lstat(vmDir); errors.Is(statErr, os.ErrNotExist) {
				result.Message = "service has no runtime state"
				return result, nil
			}
			if !errors.Is(err, os.ErrNotExist) {
				return recoveryAmbiguous(result, fmt.Sprintf("invalid durable runtime state: %v", err))
			}
			manifest = &instanceManifest{Service: service, VMDir: vmDir, SocketPath: filepath.Join(vmDir, "firecracker.sock"), ConfigPath: filepath.Join(vmDir, "vm-config.json")}

		}
		if err := m.lockRecovery(ctx); err != nil {
			return recoveryAmbiguous(result, err.Error())
		}
		if m.instances[service] == nil {
			// The disk read happened unlocked. Another resolver may have
			// completed clearance or discovery may now own this name.
			if _, err := os.Lstat(vmDir); errors.Is(err, os.ErrNotExist) {
				m.mu.Unlock()
				result.Message = "service has no runtime state"
				return result, nil
			}
			if err := m.verifyRecoverySnapshot(path, manifest); err != nil {
				m.mu.Unlock()
				return recoveryAmbiguous(result, err.Error())
			}
			if err := ctx.Err(); err != nil {
				m.mu.Unlock()
				return recoveryAmbiguous(result, err.Error())
			}
			m.instances[service] = m.recoveryInstanceForService(service, manifest, "retrying durable recovery")
		}
		m.mu.Unlock()
		return result, errRecoveryRetry
	}
	result.State = instance.State
	if instance.State != StateRecoveryPending {
		m.mu.Unlock()
		result.Message = "service is not recovery_pending"
		return result, nil
	}
	result.Outcome = RecoveryOutcomeAmbiguous
	result.State = StateRecoveryPending
	if instance.manifest == nil {
		m.mu.Unlock()
		return recoveryAmbiguous(result, "the in-memory ownership manifest is unavailable")
	}
	initialManifest := *instance.manifest
	manifest := initialManifest
	recoveryCandidate := cloneInstanceManifest(instance.recoveryCandidate)
	instanceID := instance.instanceID
	instancePID := instance.PID
	untrusted := instance.RecoveryConfigUnknown
	m.mu.Unlock()
	vmDir, manifestPath := m.recoveryManifestPath(service)
	if untrusted || verifyRecoveryDirectories(vmDir) != nil {
		return m.resolveUntrustedRecovery(ctx, result, instance, &initialManifest, untrusted, forget)
	}
	if err := verifyRecoveryManifestPaths(service, vmDir, &manifest); err != nil {
		return recoveryAmbiguous(result, err.Error())
	}
	if err := verifyRecoveryDirectories(vmDir); err != nil {
		return recoveryAmbiguous(result, err.Error())
	}
	if initialManifest.InstanceID == "" {
		if durable, err := readManifest(manifestPath); err == nil {
			if err := verifyRecoveryManifestPaths(service, vmDir, durable); err != nil {
				return recoveryAmbiguous(result, err.Error())
			}
			if err := verifyRecoveryManifestIntegrity(durable); err != nil {
				return recoveryAmbiguous(result, err.Error())
			}
			if err := m.lockRecovery(ctx); err != nil {
				return recoveryAmbiguous(result, err.Error())
			}
			if m.instances[service] != instance {
				m.mu.Unlock()
				return recoveryAmbiguous(result, "ownership changed during manifest repair")
			}
			m.instances[service] = instanceFromManifest(durable, StateRecoveryPending, "retrying repaired manifest")
			m.mu.Unlock()
			return result, errRecoveryRetry
		}
	}
	absenceErr := m.proveRecoveryAbsentContext(ctx, &manifest)
	if absenceErr == nil {
		if err := m.lockRecovery(ctx); err != nil {
			return recoveryAmbiguous(result, err.Error())
		}
		if !m.isCurrentRecoveryInstanceLocked(service, instance, instanceID, &initialManifest) {
			m.mu.Unlock()
			return recoveryAmbiguous(result, "service ownership changed while recovery was being checked")
		}
		if err := m.verifyRecoverySnapshot(manifestPath, &initialManifest); err != nil {
			m.mu.Unlock()
			return recoveryAmbiguous(result, err.Error())
		}
		// Reservation prevents a replacement start. Removing the old map entry
		// also prevents its Wait monitor from recreating files during deletion.
		delete(m.instances, service)
		m.mu.Unlock()
		if err := m.removeProvenRecoveryState(ctx, &manifest); err != nil {
			m.mu.Lock()
			m.instances[service] = instance
			m.mu.Unlock()
			return result, fmt.Errorf("remove stale VM state for %s: %w", service, err)
		}
		result.Outcome = RecoveryOutcomeCleared
		result.State = StateStopped
		result.Message = "proved the old process is gone and removed its VM runtime state"
		m.logger.Info("cleared stale VM recovery state", "service", service)
		return result, nil
	}
	if forget {
		return recoveryAmbiguous(result, "cannot forget a VM whose absence is unproved")
	}
	if instanceID == "" || instanceID != initialManifest.InstanceID || instancePID != initialManifest.PID {
		return recoveryAmbiguous(result, "in-memory ownership identities do not match")
	}
	if recoveryCandidate != nil && recoveryCandidate.Legacy {
		if _, err := os.Lstat(manifestPath); errors.Is(err, os.ErrNotExist) {
			if err := validateOwnedProcessContext(ctx, m.inspector, recoveryCandidate); err != nil {
				return recoveryAmbiguous(result, err.Error())
			}
			if err := m.writeRecoveryManifest(manifestPath, recoveryCandidate); err != nil {
				return recoveryAmbiguous(result, err.Error())
			}
		}
	}
	_, err := readAndVerifyRecoveryManifest(manifestPath, &initialManifest)
	if err != nil {
		return recoveryAmbiguous(result, err.Error())
	}
	if recoveryCandidate != nil {
		if !sameRecoveryCandidateMetadata(&initialManifest, recoveryCandidate) {
			return recoveryAmbiguous(result, "reconstructed process identity no longer matches its durable ownership record")
		}
		if err := verifyRecoveryManifestPaths(service, vmDir, recoveryCandidate); err != nil {
			return recoveryAmbiguous(result, err.Error())
		}
		if err := verifyRecoveryManifestIntegrity(recoveryCandidate); err != nil {
			return recoveryAmbiguous(result, err.Error())
		}
		manifest = *recoveryCandidate
	}
	if manifest.PID <= 0 && manifest.Launcher == "systemd" && (manifest.Lifecycle == lifecycleStarting || manifest.Lifecycle == lifecycleFailed) {
		pid, inactive, err := m.inspectRecoveryUnitContext(ctx, &manifest)
		if err != nil {
			return recoveryAmbiguous(result, err.Error())
		}
		if !inactive && pid > 0 {
			identity, err := awaitLaunchedIdentityContext(ctx, m.inspector, &manifest, pid, m.launchIdentityTimeout(), launchIdentityInterval)
			if err != nil {
				return recoveryAmbiguous(result, err.Error())
			}
			manifest.PID = pid
			applyProcessIdentity(&manifest, identity)
		} else if !inactive {
			return recoveryAmbiguous(result, "systemd launch is still pending")
		}
	}
	if err := verifyRecoveryStateFiles(vmDir, manifestPath); err != nil {
		return recoveryAmbiguous(result, err.Error())
	}
	if manifest.PID <= 0 && manifest.Lifecycle != lifecycleStopped && manifest.Lifecycle != lifecycleFailed && manifest.Launcher != "systemd" {
		return recoveryAmbiguous(result, "ownership manifest has no recorded process identity")
	}
	if _, ok := m.inspector.(osProcessInspector); ok && !processInspectionSupported {
		return recoveryAmbiguous(result, "this host cannot inspect process ownership")
	}

	processErr := validateOwnedProcessContext(ctx, m.inspector, &manifest)
	switch {

	case errors.Is(processErr, errProcessNotFound):
		return recoveryAmbiguous(result, fmt.Sprintf("process exited but absence remains unproved: %v", absenceErr))
	case processErr != nil:
		if errors.Is(processErr, errIdentityNotRecorded) {
			return recoveryAmbiguous(result, fmt.Sprintf("refusing to reconstruct process ownership identity that was never recorded: %v", processErr))
		}
		if !m.repairOwnership(ctx, service, &manifest, processErr) {
			return recoveryAmbiguous(result, fmt.Sprintf("cannot prove surviving process ownership: %v; absence proof: %v", processErr, absenceErr))
		}
		if err := validateOwnedProcessContext(ctx, m.inspector, &manifest); err != nil {
			return recoveryAmbiguous(result, fmt.Sprintf("repaired process identity does not validate: %v", err))
		}
	}
	if !sameRecoveryIdentity(&initialManifest, &manifest) {
		if err := m.lockRecovery(ctx); err != nil {
			return recoveryAmbiguous(result, err.Error())
		}
		if !m.isCurrentRecoveryInstanceLocked(service, instance, instanceID, &initialManifest) {
			m.mu.Unlock()
			return recoveryAmbiguous(result, "ownership changed during reconstruction")
		}
		m.mu.Unlock()
		writeErr := m.persistReconstructedIdentity(service, vmDir, manifestPath, &initialManifest, &manifest)
		snapshot := recoverySnapshotAfterWrite(manifestPath, &initialManifest, &manifest, writeErr)
		m.mu.Lock()
		if !m.isCurrentRecoveryInstanceLocked(service, instance, instanceID, &initialManifest) {
			m.mu.Unlock()
			return recoveryAmbiguous(result, "ownership changed during reconstruction persistence")
		}
		instance.manifest = snapshot
		instance.PID = snapshot.PID
		if writeErr != nil {
			instance.recoveryCandidate = cloneInstanceManifest(&manifest)
			m.mu.Unlock()
			return result, fmt.Errorf("persist reconstructed identity: %w", writeErr)
		}
		instance.recoveryCandidate = nil
		m.mu.Unlock()
		return result, errRecoveryRetry
	}
	if err := validateOwnedSocket(m.inspector, &manifest); err != nil {
		return recoveryAmbiguous(result, fmt.Sprintf("surviving process API socket is not ready: %v", err))
	}
	if err := verifyRecoveryStateFiles(vmDir, manifestPath); err != nil {
		return recoveryAmbiguous(result, err.Error())
	}
	if _, err := readAndVerifyRecoveryManifest(manifestPath, &initialManifest); err != nil {
		return recoveryAmbiguous(result, err.Error())
	}
	if err := m.lockRecovery(ctx); err != nil {
		return recoveryAmbiguous(result, err.Error())
	}
	if !m.isCurrentRecoveryInstanceLocked(service, instance, instanceID, &initialManifest) {
		m.mu.Unlock()
		return recoveryAmbiguous(result, "service ownership changed while recovery was being checked")
	}
	for name, other := range m.instances {
		if name != service && other.instanceID == instanceID {
			m.mu.Unlock()
			return recoveryAmbiguous(result, "instance ID is also tracked by another service")
		}
	}
	manifest.Lifecycle = lifecycleRunning
	manifest.LastError = ""
	m.mu.Unlock()
	writeErr := m.writeRecoveryManifest(manifestPath, &manifest)
	snapshot := recoverySnapshotAfterWrite(manifestPath, &initialManifest, &manifest, writeErr)
	// A child can exit while fsync is blocked. Its Wait monitor is waiting on
	// the reservation, so revalidate before publishing a running instance.
	processErr = validateOwnedProcessContext(ctx, m.inspector, &manifest)
	m.mu.Lock()
	if !m.isCurrentRecoveryInstanceLocked(service, instance, instanceID, &initialManifest) {
		m.mu.Unlock()
		return recoveryAmbiguous(result, "service ownership changed during adoption persistence")
	}
	instance.manifest = snapshot
	instance.PID = snapshot.PID
	if writeErr != nil {
		m.mu.Unlock()
		return result, fmt.Errorf("persist recovery adoption for %s: %w", service, writeErr)
	}
	if processErr != nil {
		m.mu.Unlock()
		return recoveryAmbiguous(result, fmt.Sprintf("process changed during adoption persistence: %v", processErr))
	}
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		return recoveryAmbiguous(result, err.Error())
	}
	adopted := instanceFromManifest(&manifest, StateRunning, "")
	if instance.startID != "" && manifest.Launcher == "direct" {
		// A direct child launched by this manager still has its Wait monitor.
		adopted.startID = instance.startID
	} else {
		go m.monitorAdopted(service, manifest.InstanceID)
	}
	m.instances[service] = adopted
	m.mu.Unlock()
	result.Outcome = RecoveryOutcomeReadopted
	result.State = StateRunning
	result.Message = "revalidated process identity and API socket and adopted the live VM"
	m.logger.Info("re-adopted microVM after recovery verification", "service", service, "pid", manifest.PID)
	return result, nil
}

// Rename may have succeeded before directory fsync reported an error. Keep
// memory aligned with either trusted disk snapshot so the next retry can
// validate ownership instead of becoming stuck on a stale lifecycle or PID.
func recoverySnapshotAfterWrite(path string, before, after *instanceManifest, writeErr error) *instanceManifest {
	if writeErr == nil {
		return cloneInstanceManifest(after)
	}
	if durable, err := readManifest(path); err == nil &&
		(sameRecoveryIdentity(durable, before) || sameRecoveryIdentity(durable, after)) {
		return durable
	}
	return cloneInstanceManifest(before)
}

func readAndVerifyRecoveryManifest(path string, expected *instanceManifest) (*instanceManifest, error) {
	durable, err := readManifest(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read ownership manifest: %w", err)
	}
	if !sameRecoveryIdentity(expected, durable) {
		return nil, fmt.Errorf("in-memory and durable ownership identities do not match")
	}
	if expected.Config.Name != expected.Service || durable.Config.Name != durable.Service {
		return nil, fmt.Errorf("ownership manifest service configuration does not match its service")
	}
	for _, manifest := range []*instanceManifest{expected, durable} {
		if err := verifyRecoveryManifestIntegrity(manifest); err != nil {
			return nil, err
		}
	}
	return durable, nil
}

func recoveryAmbiguous(result RecoveryResult, message string) (RecoveryResult, error) {
	result.Outcome = RecoveryOutcomeAmbiguous
	result.State = StateRecoveryPending
	result.Message = message
	return result, &recoveryPendingError{fmt.Errorf("service %s remains recovery_pending: %s", result.Service, message)}
}

// ValidateRecoveryServiceName rejects service identifiers that are not one
// safe path component.
func ValidateRecoveryServiceName(service string) error {
	if service == "" || service == "." || service == ".." || strings.ContainsRune(service, 0) ||
		strings.ContainsAny(service, `/\\`) || filepath.Base(service) != service || filepath.Clean(service) != service {
		return fmt.Errorf("invalid service name %q", service)
	}
	return nil
}

func (m *Manager) recoveryManifestPath(service string) (vmDir, path string) {
	vmDir = filepath.Join(m.stateDir, "vms", service)
	path = manifestPath(vmDir)
	return vmDir, path
}

func verifyRecoveryManifestPaths(service, vmDir string, manifest *instanceManifest) error {
	if manifest.Service != service || manifest.VMDir != vmDir ||
		manifest.SocketPath != filepath.Join(vmDir, "firecracker.sock") ||
		manifest.ConfigPath != filepath.Join(vmDir, "vm-config.json") {
		return fmt.Errorf("ownership manifest paths do not match the named service state directory")
	}
	return nil
}

func verifyRecoveryDirectories(vmDir string) error {
	for _, path := range []string{filepath.Dir(vmDir), vmDir} {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect VM state directory %s: %w", path, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("VM state path %s is not a directory", path)
		}
	}
	return nil
}

func verifyRecoveryStateFiles(vmDir, manifestPath string) error {
	if err := verifyRecoveryDirectories(vmDir); err != nil {
		return err
	}
	info, err := os.Lstat(manifestPath)
	if err != nil {
		return fmt.Errorf("inspect ownership manifest: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("ownership manifest is not a regular file")
	}
	return nil
}

func sameRecoveryIdentity(memory, disk *instanceManifest) bool {
	return memory.Service == disk.Service && memory.InstanceID == disk.InstanceID && memory.PID == disk.PID &&
		memory.HostBootID == disk.HostBootID && memory.ProcessStart == disk.ProcessStart &&
		memory.Executable == disk.Executable && memory.ExecutableDev == disk.ExecutableDev && memory.ExecutableIno == disk.ExecutableIno &&
		memory.ConfigHash == disk.ConfigHash && memory.VMDir == disk.VMDir && memory.SocketPath == disk.SocketPath &&
		memory.ConfigPath == disk.ConfigPath && memory.Launcher == disk.Launcher && memory.LauncherUnit == disk.LauncherUnit &&
		memory.Lifecycle == disk.Lifecycle && memory.Legacy == disk.Legacy
}

func sameRecoveryCandidateMetadata(original, candidate *instanceManifest) bool {
	return original.Service == candidate.Service && original.InstanceID == candidate.InstanceID &&
		original.Lifecycle == candidate.Lifecycle && original.ConfigHash == candidate.ConfigHash &&
		original.VMDir == candidate.VMDir && original.SocketPath == candidate.SocketPath &&
		original.ConfigPath == candidate.ConfigPath && original.Launcher == candidate.Launcher &&
		original.LauncherUnit == candidate.LauncherUnit && original.Legacy == candidate.Legacy
}

func (m *Manager) writeRecoveryManifest(path string, manifest *instanceManifest) error {
	if m.recoveryManifestWriter != nil {
		return m.recoveryManifestWriter(path, manifest)
	}
	return writeManifest(path, manifest)
}

func (m *Manager) noLiveClaimantContext(ctx context.Context, manifest *instanceManifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if scanner, ok := m.inspector.(interface {
		FindByAnyArgumentContext(context.Context, string, string) ([]processIdentity, error)
	}); ok {
		matches, err := scanner.FindByAnyArgumentContext(ctx, manifest.SocketPath, manifest.ConfigPath)
		if err != nil {
			return err
		}
		if len(matches) != 0 {
			return fmt.Errorf("found %d live process(es) using this service's VM socket or config paths", len(matches))
		}
		return nil
	}
	scanner, ok := m.inspector.(interface {
		FindByAnyArgument(string, string) ([]processIdentity, error)
	})
	if !ok {
		return fmt.Errorf("strict process claimant scan is unavailable")
	}
	matches, err := scanner.FindByAnyArgument(manifest.SocketPath, manifest.ConfigPath)
	if err != nil {
		return fmt.Errorf("cannot scan for a live process using this VM state: %w", err)
	}
	if len(matches) != 0 {
		return fmt.Errorf("found %d live process(es) using this service's VM socket or config paths", len(matches))
	}
	return nil
}

func (m *Manager) isCurrentRecoveryInstanceLocked(service string, expected *Instance, instanceID string, manifest *instanceManifest) bool {
	current := m.instances[service]
	return current == expected && current != nil && current.State == StateRecoveryPending && current.instanceID == instanceID &&
		current.PID == manifest.PID && current.manifest != nil && sameRecoveryIdentity(current.manifest, manifest)
}

func (m *Manager) lockRecovery(ctx context.Context) error {
	delay := time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if m.mu.TryLock() {
			if err := ctx.Err(); err != nil {
				m.mu.Unlock()
				return err
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			delay = min(delay*2, 20*time.Millisecond)
			timer.Reset(delay)
		}
	}
}

type recoveryPendingError struct{ error }

func (m *Manager) verifyRecoverySnapshot(path string, initial *instanceManifest) error {
	if err := verifyRecoveryDirectories(initial.VMDir); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("ownership manifest must be a regular file")
	}
	// A previous RemoveAll may have removed the manifest before failing.
	// This check only precedes deletion after a fresh complete absence proof.
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if initial.InstanceID == "" || initial.Legacy {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if initial.InstanceID == "" {
			return fmt.Errorf("manifest appeared during legacy cleanup")
		}
	}
	_, err := readAndVerifyRecoveryManifest(path, initial)
	return err
}
