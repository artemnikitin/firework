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
	if errors.Is(err, os.ErrNotExist) {
		// Without the vms root there is no entry either; absence is still
		// proved from the canonical paths before anything is cleared.
		snapshot.kind = "missing"
		return snapshot, nil
	}
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
	probe := canonicalManifest(service, vmDir)
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
// an unreadable or mismatched manifest, a missing manifest or directory, or a
// service path that is not a real directory. State on disk stays fenced until
// --forget deletes it after an absence proof, or a restored trusted record
// replaces it: nothing in it identifies the host networking a dead owner
// left, so the operator checks that networking first (L4). A directory that
// is gone is no exception, unless this manager removed it itself, as a failed
// start, a removal or a clear racing an inventory does: only then does an
// operation the agent recorded explain its disappearance.
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
	if absenceErr != nil {
		return recoveryAmbiguous(result, fmt.Sprintf("%s and absence remains unproved: %v", reason, absenceErr))
	}
	preservesMetadata := before.kind == "symlink" || (before.kind == "dir" && before.manifest != "missing")
	// The manager's own interrupted operation explains a vanished directory or
	// one without a manifest; nothing else does.
	ownOperation := before.kind == "missing" && (m.removedOwnDir(service) || m.hasIntent(service)) ||
		before.kind == "dir" && before.manifest == "missing" && m.hasIntent(service)
	if !forget && !ownOperation {
		// What a dead owner left on the host is unknown, so its claims stay
		// fenced until an operator decides; recovery never releases them.
		if preservesMetadata {
			return recoveryAmbiguous(result, fmt.Sprintf("%s; no process owns it, so it is preserved for diagnosis and its claims stay fenced: restore a trusted ownership record or run firework-agent clear-recovery --forget %s", reason, service))
		}
		return recoveryAmbiguous(result, fmt.Sprintf("%s; no process owns it, but nothing identifies the host networking it may have left, so its claims stay fenced: check that networking, then run firework-agent clear-recovery --forget %s", reason, service))
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
		return recoveryIncomplete(result, fmt.Errorf("remove stale VM state for %s: %w", service, removeErr))
	}
	result.Outcome = RecoveryOutcomeCleared
	result.State = StateStopped
	result.Message = fmt.Sprintf("%s; proved no process owns it and removed its VM runtime state", reason)
	m.logger.Info("cleared stale VM recovery state", "service", service, "reason", reason)
	return result, nil
}
