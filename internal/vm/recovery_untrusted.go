package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// recoveryEntrySnapshot identifies the on-disk state an untrusted recovery
// decision was based on without trusting any of its contents. Deletion
// proceeds only if the entry is unchanged after the absence proof.
type recoveryEntrySnapshot struct {
	kind     string // "missing", "dir" or "symlink"
	target   string
	manifest string // content digest, "missing", or the irregular file mode
}

func snapshotRecoveryEntry(vmDir string) (recoveryEntrySnapshot, error) {
	var snapshot recoveryEntrySnapshot
	parent, err := os.Lstat(filepath.Dir(vmDir))
	if err != nil {
		return snapshot, fmt.Errorf("inspect VM state directory %s: %w", filepath.Dir(vmDir), err)
	}
	if !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 {
		return snapshot, fmt.Errorf("VM state path %s is not a directory", filepath.Dir(vmDir))
	}
	info, err := os.Lstat(vmDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		snapshot.kind = "missing"
		return snapshot, nil
	case err != nil:
		return snapshot, fmt.Errorf("inspect VM state directory %s: %w", vmDir, err)
	case info.Mode()&os.ModeSymlink != 0:
		snapshot.kind = "symlink"
		snapshot.target, err = os.Readlink(vmDir)
		return snapshot, err
	case !info.IsDir():
		return snapshot, fmt.Errorf("VM state path %s is not a directory", vmDir)
	}
	snapshot.kind = "dir"
	path := manifestPath(vmDir)
	manifestInfo, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		snapshot.manifest = "missing"
	case err != nil:
		return snapshot, fmt.Errorf("inspect ownership manifest: %w", err)
	case !manifestInfo.Mode().IsRegular():
		snapshot.manifest = "irregular " + manifestInfo.Mode().String()
	default:
		data, err := os.ReadFile(path)
		if err != nil {
			return snapshot, fmt.Errorf("read ownership manifest: %w", err)
		}
		sum := sha256.Sum256(data)
		snapshot.manifest = hex.EncodeToString(sum[:])
	}
	return snapshot, nil
}

// recoveryAbsenceProbe builds the identity used to prove that no process owns
// a service's runtime state. Untrusted metadata contributes only fields that
// add refusals; the canonical paths always come from the service name.
func recoveryAbsenceProbe(service, vmDir string, recorded *instanceManifest, untrusted bool) *instanceManifest {
	if !untrusted {
		return cloneInstanceManifest(recorded)
	}
	probe := &instanceManifest{
		SchemaVersion: manifestSchemaVersion, Service: service, VMDir: vmDir,
		SocketPath: filepath.Join(vmDir, "firecracker.sock"), ConfigPath: filepath.Join(vmDir, "vm-config.json"),
	}
	probe.Config.Name = service
	// A PID without its start time could name any later process.
	if recorded.PID > 0 && recorded.ProcessStart != 0 {
		probe.PID, probe.ProcessStart, probe.HostBootID = recorded.PID, recorded.ProcessStart, recorded.HostBootID
	}
	if recorded.Launcher == "systemd" && recorded.InstanceID != "" &&
		recorded.LauncherUnit == "firework-vm-"+recorded.InstanceID+".service" {
		probe.Launcher, probe.LauncherUnit, probe.InstanceID = recorded.Launcher, recorded.LauncherUnit, recorded.InstanceID
	}
	return probe
}

// resolveUntrustedRecovery handles state that can never authorize adoption:
// an unreadable or mismatched manifest, or a service path that is not a real
// directory. Proven absence releases its unknown resource claims. Deleting it
// requires --forget when metadata would be lost, so corrupt records stay
// available for diagnosis by default; a missing directory or manifest has
// nothing to preserve and is cleared automatically.
func (m *Manager) resolveUntrustedRecovery(ctx context.Context, result RecoveryResult, instance *Instance, recorded *instanceManifest, untrusted, forget bool) (RecoveryResult, error) {
	service := result.Service
	vmDir, path := m.recoveryManifestPath(service)
	before, err := snapshotRecoveryEntry(vmDir)
	if err != nil {
		return recoveryAmbiguous(result, err.Error())
	}
	if untrusted && before.kind == "dir" {
		// Restoring a trusted record is the documented remedy for corruption.
		if durable, err := readManifest(path); err == nil && verifyRecoveryManifestPaths(service, vmDir, durable) == nil &&
			verifyRecoveryManifestIntegrity(durable) == nil && durable.Config.Name == service {
			if err := m.lockRecovery(ctx); err != nil {
				return recoveryAmbiguous(result, err.Error())
			}
			if m.instances[service] != instance {
				m.mu.Unlock()
				return recoveryAmbiguous(result, "ownership changed during manifest repair")
			}
			m.instances[service] = m.recoveryInstanceForService(service, durable, "retrying restored ownership manifest")
			m.mu.Unlock()
			return result, errRecoveryRetry
		}
	}
	reason := "ownership metadata is untrusted"
	switch before.kind {
	case "missing":
		reason = "VM state directory is missing"
	case "symlink":
		reason = "VM state path is a symlink"
	}
	probe := recoveryAbsenceProbe(service, vmDir, recorded, untrusted)
	absenceErr := m.proveRecoveryAbsentContext(ctx, probe)
	var journalErr error
	if absenceErr == nil && untrusted {
		journalErr = m.journalReleasedRecoveryClaims(service, vmDir, instance, recorded)
	}
	m.mu.Lock()
	if m.instances[service] == instance {
		instance.RecoveryOwnerAbsent = absenceErr == nil && journalErr == nil
	}
	m.mu.Unlock()
	if absenceErr != nil {
		return recoveryAmbiguous(result, fmt.Sprintf("%s and absence remains unproved: %v", reason, absenceErr))
	}
	if journalErr != nil {
		return recoveryAmbiguous(result, fmt.Sprintf("%s; no process owns it, but its claims stay reserved: %v", reason, journalErr))
	}
	preservesMetadata := before.kind == "symlink" || (before.kind == "dir" && before.manifest != "missing")
	if preservesMetadata && !forget {
		return recoveryAmbiguous(result, fmt.Sprintf("%s; no process owns it, so it is preserved for diagnosis: restore a trusted ownership record or run firework-agent clear-recovery --forget %s", reason, service))
	}

	if err := m.lockRecovery(ctx); err != nil {
		return recoveryAmbiguous(result, err.Error())
	}
	if m.instances[service] != instance || instance.State != StateRecoveryPending {
		m.mu.Unlock()
		return recoveryAmbiguous(result, "service ownership changed while recovery was being checked")
	}
	if after, err := snapshotRecoveryEntry(vmDir); err != nil || after != before {
		m.mu.Unlock()
		return recoveryAmbiguous(result, "VM state changed while recovery was being checked")
	}
	// The reservation prevents a replacement start, and removing the entry
	// keeps a Wait monitor from recreating files during deletion.
	delete(m.instances, service)
	m.mu.Unlock()
	var removeErr error
	switch before.kind {
	case "missing":
		removeErr = m.persistRecoveryTeardown(probe)
	case "symlink":
		// Remove the link itself; never follow it into another directory.
		if removeErr = ctx.Err(); removeErr == nil {
			removeErr = os.Remove(vmDir)
		}
	default:
		removeErr = m.removeProvenRecoveryState(ctx, probe)
	}
	if removeErr != nil {
		m.mu.Lock()
		m.instances[service] = instance
		m.mu.Unlock()
		return result, fmt.Errorf("remove stale VM state for %s: %w", service, removeErr)
	}
	result.Outcome = RecoveryOutcomeCleared
	result.State = StateStopped
	result.Message = fmt.Sprintf("%s; proved no process owns it and removed its VM runtime state", reason)
	m.logger.Info("cleared stale VM recovery state", "service", service, "reason", reason)
	return result, nil
}

// journalReleasedRecoveryClaims records the host cleanup that a readable but
// untrusted record describes, before its claims are released for reuse.
// Otherwise its stale port forwards could shadow a replacement's. The
// journal's claimant guards keep any device or rule a live service uses.
func (m *Manager) journalReleasedRecoveryClaims(service, vmDir string, instance *Instance, recorded *instanceManifest) error {
	m.mu.Lock()
	released := m.instances[service] == instance && instance.RecoveryOwnerAbsent
	m.mu.Unlock()
	if released || recorded.Service != service || recorded.Config.Name != service ||
		verifyRecoveryManifestPaths(service, vmDir, recorded) != nil {
		return nil // Already journaled, or the record names no readable claims.
	}
	if err := m.persistRecoveryTeardown(recorded); err != nil {
		return fmt.Errorf("persist cleanup intent: %w", err)
	}
	return nil
}
