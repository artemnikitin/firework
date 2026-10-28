package vm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	result := RecoveryResult{Service: service, Outcome: RecoveryOutcomeNoop}
	if err := ValidateRecoveryServiceName(service); err != nil {
		return result, err
	}

	m.mu.Lock()
	instance := m.instances[service]
	if instance == nil {
		m.mu.Unlock()
		result.Message = "service is not tracked by the running agent"
		return result, nil
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
	m.mu.Unlock()
	if instanceID == "" || instanceID != initialManifest.InstanceID || instancePID != initialManifest.PID {
		return recoveryAmbiguous(result, "in-memory ownership identities do not match")
	}
	vmDir, manifestPath := m.recoveryManifestPath(service)
	if err := verifyRecoveryManifestPaths(service, vmDir, &manifest); err != nil {
		return recoveryAmbiguous(result, err.Error())
	}
	if err := verifyRecoveryStateFiles(vmDir, manifestPath); err != nil {
		return recoveryAmbiguous(result, err.Error())
	}
	_, err := readAndVerifyRecoveryManifest(manifestPath, &initialManifest)
	if err != nil {
		return recoveryAmbiguous(result, err.Error())
	}
	if initialManifest.Lifecycle == lifecycleStopping {
		return recoveryAmbiguous(result, "durable VM shutdown state is unresolved; refusing to clear or re-adopt its runtime state")
	}
	if recoveryCandidate != nil {
		if !sameRecoveryCandidateMetadata(&initialManifest, recoveryCandidate) {
			return recoveryAmbiguous(result, "reconstructed process identity no longer matches its durable ownership record")
		}
		manifest = *recoveryCandidate
	}
	if manifest.PID <= 0 {
		return recoveryAmbiguous(result, "ownership manifest has no recorded process identity")
	}
	if _, ok := m.inspector.(osProcessInspector); ok && !processInspectionSupported {
		return recoveryAmbiguous(result, "this host cannot inspect process ownership")
	}

	processErr := validateOwnedProcess(m.inspector, &manifest)
	switch {
	case errors.Is(processErr, errProcessNotFound), errors.Is(processErr, errProcessIdentityReused):
		if err := m.noLiveClaimant(&manifest); err != nil {
			return recoveryAmbiguous(result, err.Error())
		}
		m.mu.Lock()
		if !m.isCurrentRecoveryInstanceLocked(service, instance, instanceID, &initialManifest) {
			m.mu.Unlock()
			return recoveryAmbiguous(result, "service ownership changed while recovery was being checked")
		}
		if err := verifyRecoveryStateFiles(vmDir, manifestPath); err != nil {
			m.mu.Unlock()
			return recoveryAmbiguous(result, err.Error())
		}
		if _, err := readAndVerifyRecoveryManifest(manifestPath, &initialManifest); err != nil {
			m.mu.Unlock()
			return recoveryAmbiguous(result, err.Error())
		}
		if err := os.RemoveAll(vmDir); err != nil {
			m.mu.Unlock()
			return result, fmt.Errorf("remove stale VM state for %s: %w", service, err)
		}
		delete(m.instances, service)
		m.mu.Unlock()
		result.Outcome = RecoveryOutcomeCleared
		result.State = StateStopped
		result.Message = "proved the old process is gone and removed its VM runtime state"
		m.logger.Info("cleared stale VM recovery state", "service", service)
		return result, nil
	case processErr != nil:
		if recoveryCandidate == nil && errors.Is(processErr, errIdentityNotRecorded) {
			return recoveryAmbiguous(result, fmt.Sprintf("refusing to reconstruct process ownership identity that was never recorded: %v", processErr))
		}
		if !m.repairOwnership(service, &manifest, processErr) {
			return recoveryAmbiguous(result, fmt.Sprintf("cannot prove surviving process ownership: %v", processErr))
		}
		if err := validateOwnedProcess(m.inspector, &manifest); err != nil {
			return recoveryAmbiguous(result, fmt.Sprintf("repaired process identity does not validate: %v", err))
		}
	}
	if err := validateOwnedSocket(m.inspector, &manifest); err != nil {
		return recoveryAmbiguous(result, fmt.Sprintf("surviving process API socket is not ready: %v", err))
	}
	m.mu.Lock()
	if !m.isCurrentRecoveryInstanceLocked(service, instance, instanceID, &initialManifest) {
		m.mu.Unlock()
		return recoveryAmbiguous(result, "service ownership changed while recovery was being checked")
	}
	if err := verifyRecoveryStateFiles(vmDir, manifestPath); err != nil {
		m.mu.Unlock()
		return recoveryAmbiguous(result, err.Error())
	}
	if _, err := readAndVerifyRecoveryManifest(manifestPath, &initialManifest); err != nil {
		m.mu.Unlock()
		return recoveryAmbiguous(result, err.Error())
	}
	manifest.Lifecycle = lifecycleRunning
	manifest.LastError = ""
	if err := m.writeRecoveryManifest(manifestPath, &manifest); err != nil {
		m.mu.Unlock()
		return result, fmt.Errorf("persist recovery adoption for %s: %w", service, err)
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
		hash, err := serviceConfigHash(manifest.Config)
		if err != nil || hash != manifest.ConfigHash {
			return nil, fmt.Errorf("ownership manifest service configuration hash does not match")
		}
	}
	return durable, nil
}

func recoveryAmbiguous(result RecoveryResult, message string) (RecoveryResult, error) {
	result.Outcome = RecoveryOutcomeAmbiguous
	result.State = StateRecoveryPending
	result.Message = message
	return result, fmt.Errorf("service %s remains recovery_pending: %s", result.Service, message)
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

func (m *Manager) recoveryManifestPath(service string) (vmDir, manifestPath string) {
	vmDir = filepath.Join(m.stateDir, "vms", service)
	manifestPath = filepath.Join(vmDir, manifestFileName)
	return vmDir, manifestPath
}

func verifyRecoveryManifestPaths(service, vmDir string, manifest *instanceManifest) error {
	if manifest.Service != service || manifest.VMDir != vmDir ||
		manifest.SocketPath != filepath.Join(vmDir, "firecracker.sock") ||
		manifest.ConfigPath != filepath.Join(vmDir, "vm-config.json") {
		return fmt.Errorf("ownership manifest paths do not match the named service state directory")
	}
	return nil
}

func verifyRecoveryStateFiles(vmDir, manifestPath string) error {
	for _, path := range []string{filepath.Dir(vmDir), vmDir} {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect VM state directory %s: %w", path, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("VM state path %s is not a directory", path)
		}
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

func (m *Manager) noLiveClaimant(manifest *instanceManifest) error {
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
