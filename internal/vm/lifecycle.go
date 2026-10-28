package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Stop serializes lifecycle work for this service, then waits without holding
// Manager.mu so status and heartbeats remain responsive during guest quiesce.
func (m *Manager) Stop(name string) error {
	unlock := m.lockService(name)
	defer unlock()
	return m.stopService(name, false)
}

// RemoveForResize requires a successful guest quiesce before removing the old
// VM's state. Reconciliation calls it only after a changed volume size has
// passed volume preflight and rejection settling.
func (m *Manager) RemoveForResize(name string) error {
	unlock := m.lockService(name)
	defer unlock()
	return m.removeService(name, true)
}

// Remove stops (if running) and removes all state for a service.
func (m *Manager) Remove(name string) error {
	unlock := m.lockService(name)
	defer unlock()
	return m.removeService(name, false)
}

func (m *Manager) removeService(name string, requireQuiesce bool) error {
	m.mu.Lock()
	instance, exists := m.instances[name]
	if exists && instance.State == StateRecoveryPending {
		m.mu.Unlock()
		return fmt.Errorf("refusing to remove service %s while recovery is pending: %s", name, instance.LastError)
	}
	if requireQuiesce && !exists {
		m.mu.Unlock()
		return fmt.Errorf("refusing to resize service %s without a tracked VM to confirm guest quiesce", name)
	}
	aborting := exists && (instance.State == StateStarting || instance.State == StateStartAborting)
	if aborting && requireQuiesce {
		m.mu.Unlock()
		return fmt.Errorf("refusing to resize service %s while its VM start is in progress", name)
	}
	if aborting {
		instance.State = StateStartAborting
	}
	m.mu.Unlock()

	// Volume preparation runs without the service operation lock. A regular
	// removal can abort that start and remove only VM runtime files; resize
	// preparation cannot, because it needs a successful guest shutdown proof.
	if aborting {
		m.logger.Info("aborting in-flight start instead of removing", "service", name)
		if err := os.RemoveAll(filepath.Join(m.stateDir, "vms", name)); err != nil {
			return fmt.Errorf("removing vm dir: %w", err)
		}
		return nil
	}

	if exists {
		if err := m.stopService(name, requireQuiesce); err != nil {
			return fmt.Errorf("stopping VM during remove: %w", err)
		}
	}

	m.mu.Lock()
	current := m.instances[name]
	if current != nil && current != instance {
		m.mu.Unlock()
		return fmt.Errorf("service %s instance changed while removing it", name)
	}
	if current != nil && current.State == StateRecoveryPending {
		m.mu.Unlock()
		return fmt.Errorf("refusing to remove service %s while recovery is pending: %s", name, current.LastError)
	}
	delete(m.instances, name)
	m.mu.Unlock()

	vmDir := filepath.Join(m.stateDir, "vms", name)
	if err := os.RemoveAll(vmDir); err != nil {
		return fmt.Errorf("removing vm dir: %w", err)
	}
	return nil
}

func (m *Manager) stopService(name string, requireQuiesce bool) error {
	m.mu.Lock()
	instance, exists := m.instances[name]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("service %s not found", name)
	}
	if instance.State == StateRecoveryPending {
		err := fmt.Errorf("refusing to stop service %s: process ownership is ambiguous: %s", name, instance.LastError)
		m.mu.Unlock()
		return err
	}
	if instance.State == StateStarting || instance.State == StateStartAborting {
		if requireQuiesce {
			m.mu.Unlock()
			return fmt.Errorf("refusing to resize service %s while its VM start is in progress", name)
		}
		instance.State = StateStartAborting
		m.mu.Unlock()
		m.logger.Info("aborting in-flight start instead of stopping", "service", name)
		return nil
	}
	manifest := instance.manifest
	if manifest == nil {
		m.mu.Unlock()
		return fmt.Errorf("service %s has no ownership manifest", name)
	}
	if instance.State == StateStopped {
		if requireQuiesce && !manifest.GuestQuiesced {
			m.mu.Unlock()
			return fmt.Errorf("refusing to resize service %s: its stopped VM has no successful guest quiesce record", name)
		}
		m.mu.Unlock()
		return nil
	}
	if requireQuiesce && instance.State == StateFailed && manifest.PID <= 0 && !manifest.GuestQuiesced {
		m.mu.Unlock()
		return fmt.Errorf("refusing to resize service %s: its failed VM has no successful guest quiesce record", name)
	}

	previousState := instance.State
	previousLifecycle := manifest.Lifecycle
	instanceID := instance.instanceID
	if instanceID == "" {
		instanceID = manifest.InstanceID
	}
	pid := manifest.PID
	instance.State = StateStopping
	manifest.Lifecycle = lifecycleStopping
	if err := writeManifest(manifestPath(manifest.VMDir), manifest); err != nil {
		instance.State = previousState
		manifest.Lifecycle = previousLifecycle
		m.mu.Unlock()
		return err
	}
	ownedManifest := *manifest
	m.mu.Unlock()

	m.logger.Info("stopping microVM", "service", name, "pid", pid)
	ownerErr := m.validateStoppingOwner(name, instance, instanceID, &ownedManifest)
	if ownerErr != nil {
		if errors.Is(ownerErr, errProcessNotFound) {
			if requireQuiesce && !ownedManifest.GuestQuiesced {
				m.markProcessGone(name, instance, instanceID, &ownedManifest, "Firecracker exited before guest quiesce completed")
				return fmt.Errorf("refusing to resize service %s: Firecracker exited before guest quiesce completed", name)
			}
			return m.completeStop(name, instance, instanceID, &ownedManifest)
		}
		m.quarantineStopping(name, instance, instanceID, &ownedManifest, fmt.Errorf("ownership validation failed before stop: %w", ownerErr))
		return fmt.Errorf("refusing to stop service %s: ownership validation failed; recover it with firework-agent clear-recovery %s: %w", name, name, ownerErr)
	}

	quiesceErr := m.requestGuestQuiesce(&ownedManifest)
	if quiesceErr != nil {
		if len(ownedManifest.Volumes) > 0 {
			m.setVolumeError(name, quiesceErr)
		}
		if requireQuiesce {
			if ownerErr := m.validateStoppingOwner(name, instance, instanceID, &ownedManifest); ownerErr == nil {
				if restoreErr := m.restoreRunningAfterQuiesceFailure(name, instance, instanceID); restoreErr != nil {
					return fmt.Errorf("guest quiesce failed: %v; could not restore running state: %w", quiesceErr, restoreErr)
				}
				return fmt.Errorf("guest quiesce required before resizing service %s: %w", name, quiesceErr)
			} else if errors.Is(ownerErr, errProcessNotFound) {
				m.markProcessGone(name, instance, instanceID, &ownedManifest, "Firecracker exited before guest quiesce completed")
				return fmt.Errorf("refusing to resize service %s: Firecracker exited before guest quiesce completed: %w", name, quiesceErr)
			} else {
				m.quarantineStopping(name, instance, instanceID, &ownedManifest, fmt.Errorf("ownership became ambiguous after failed guest quiesce: %w", ownerErr))
				return fmt.Errorf("refusing to resize service %s after guest quiesce failure and ownership change: %w", name, ownerErr)
			}
		}
		m.logger.Warn("guest quiesce failed; falling back to ownership-validated VMM termination",
			"service", name, "error", quiesceErr)
	} else {
		m.setVolumeError(name, nil)
		if err := m.persistGuestQuiesceConfirmation(name, instance, instanceID); err != nil {
			if requireQuiesce {
				ownerErr := m.validateStoppingOwner(name, instance, instanceID, &ownedManifest)
				if ownerErr == nil {
					if restoreErr := m.restoreRunningAfterQuiesceFailure(name, instance, instanceID); restoreErr != nil {
						return fmt.Errorf("guest quiesce succeeded but its confirmation could not be persisted: %v; could not restore running state: %w", err, restoreErr)
					}
				} else if errors.Is(ownerErr, errProcessNotFound) {
					m.markProcessGone(name, instance, instanceID, &ownedManifest, "Firecracker exited before guest quiesce confirmation could be persisted")
					return fmt.Errorf("guest quiesce succeeded but its confirmation could not be persisted and Firecracker exited: %w", err)
				} else {
					m.quarantineStopping(name, instance, instanceID, &ownedManifest, fmt.Errorf("ownership became ambiguous after guest quiesce: %w", ownerErr))
					return fmt.Errorf("guest quiesce succeeded but its confirmation could not be persisted; ownership changed: %w", ownerErr)
				}
				return fmt.Errorf("guest quiesce succeeded but its confirmation could not be persisted: %w", err)
			}
			m.logger.Warn("could not persist guest quiesce confirmation", "service", name, "error", err)
		} else {
			ownedManifest.GuestQuiesced = true
		}
	}

	launcher := m.stopLauncherForManifest(&ownedManifest)
	if err := m.validateStoppingOwner(name, instance, instanceID, &ownedManifest); err != nil {
		if errors.Is(err, errProcessNotFound) {
			return m.completeStop(name, instance, instanceID, &ownedManifest)
		}
		m.quarantineStopping(name, instance, instanceID, &ownedManifest, fmt.Errorf("ownership changed after guest quiesce: %w", err))
		return fmt.Errorf("refusing to signal pid %d after guest quiesce because ownership changed: %w", pid, err)
	}
	if err := launcher.Stop(&ownedManifest, syscall.SIGTERM); err != nil {
		m.logger.Warn("SIGTERM failed; verifying ownership before SIGKILL", "service", name, "error", err)
		ownerErr := m.validateStoppingOwner(name, instance, instanceID, &ownedManifest)
		if errors.Is(ownerErr, errProcessNotFound) {
			return m.completeStop(name, instance, instanceID, &ownedManifest)
		}
		if ownerErr != nil {
			m.quarantineStopping(name, instance, instanceID, &ownedManifest, fmt.Errorf("ownership changed before SIGKILL: %w", ownerErr))
			return fmt.Errorf("refusing to signal pid %d with SIGKILL because ownership changed: %w", pid, ownerErr)
		}
		if killErr := launcher.Stop(&ownedManifest, syscall.SIGKILL); killErr != nil {
			m.logger.Warn("SIGKILL failed after verified ownership", "service", name, "error", killErr)
		}
	}

	exited, waitErr := waitForOwnedProcessExit(m.inspector, &ownedManifest, 5*time.Second)
	if waitErr != nil {
		m.quarantineStopping(name, instance, instanceID, &ownedManifest, fmt.Errorf("process identity changed while stopping: %w", waitErr))
		return fmt.Errorf("refusing to signal pid %d again: %w", pid, waitErr)
	}
	if !exited {
		m.logger.Warn("microVM did not exit after SIGTERM, verifying ownership before SIGKILL", "service", name, "pid", pid)
		ownerErr := m.validateStoppingOwner(name, instance, instanceID, &ownedManifest)
		if errors.Is(ownerErr, errProcessNotFound) {
			return m.completeStop(name, instance, instanceID, &ownedManifest)
		}
		if ownerErr != nil {
			m.quarantineStopping(name, instance, instanceID, &ownedManifest, fmt.Errorf("ownership changed before SIGKILL: %w", ownerErr))
			return fmt.Errorf("refusing to send SIGKILL to pid %d because ownership changed: %w", pid, ownerErr)
		}
		if err := launcher.Stop(&ownedManifest, syscall.SIGKILL); err != nil {
			m.logger.Warn("SIGKILL failed after verified ownership", "service", name, "error", err)
		}
		exited, waitErr = waitForOwnedProcessExit(m.inspector, &ownedManifest, 2*time.Second)
		if waitErr != nil {
			m.quarantineStopping(name, instance, instanceID, &ownedManifest, fmt.Errorf("process identity changed after SIGKILL: %w", waitErr))
			return fmt.Errorf("process identity changed after SIGKILL: %w", waitErr)
		}
		if !exited {
			return fmt.Errorf("process %d did not exit after SIGKILL", pid)
		}
	}
	return m.completeStop(name, instance, instanceID, &ownedManifest)
}

func (m *Manager) stopLauncherForManifest(manifest *instanceManifest) processLauncher {
	switch launcher := m.launcher.(type) {
	case *directLauncher:
		if manifest.Launcher == "direct" {
			return launcher
		}
	case *systemdLauncher:
		if manifest.Launcher == "systemd" {
			return launcher
		}
	default:
		if launcher != nil {
			// Tests and embedders can inject a launcher that owns the live
			// instance and also supplies its matching stop operation.
			return launcher
		}
	}
	return launcherForManifest(m.firecrackerBin, manifest)
}

func (m *Manager) requestGuestQuiesce(manifest *instanceManifest) error {
	path, err := guestVsockPath(manifest)
	if err != nil {
		return err
	}
	timeout := m.guestQuiesceTimeout
	if timeout <= 0 {
		timeout = defaultGuestQuiesceTimeout
	}
	appTimeout := timeout - 2*time.Second
	if appTimeout <= 0 {
		appTimeout = timeout / 2
	}
	if appTimeout > time.Duration(maxGuestQuiesceProtocolTimeout) {
		appTimeout = time.Duration(maxGuestQuiesceProtocolTimeout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	quiescer := m.quiescer
	if quiescer == nil {
		quiescer = firecrackerGuestQuiescer{}
	}
	response, err := quiescer.Quiesce(ctx, path, appTimeout)
	if err != nil {
		return err
	}
	return validateGuestQuiesceResponse(response, manifest)
}

const maxGuestQuiesceProtocolTimeout = 5 * time.Minute

func (m *Manager) validateStoppingOwner(name string, expected *Instance, instanceID string, manifest *instanceManifest) error {
	m.mu.Lock()
	current := m.instances[name]
	valid := current == expected && current.instanceID == instanceID && current.State == StateStopping &&
		current.manifest != nil && current.manifest.InstanceID == instanceID
	m.mu.Unlock()
	if !valid {
		return fmt.Errorf("service %s no longer has the stopping instance %s", name, instanceID)
	}
	return validateOwnedProcess(m.inspector, manifest)
}

func (m *Manager) persistGuestQuiesceConfirmation(name string, expected *Instance, instanceID string) error {
	m.mu.Lock()
	current := m.instances[name]
	if current != expected || current.instanceID != instanceID || current.State != StateStopping || current.manifest == nil || current.manifest.InstanceID != instanceID {
		m.mu.Unlock()
		return fmt.Errorf("service %s instance changed before guest quiesce confirmation", name)
	}
	previous := current.manifest.GuestQuiesced
	current.manifest.GuestQuiesced = true
	path := manifestPath(current.manifest.VMDir)
	snapshot := *current.manifest
	m.mu.Unlock()
	if err := writeManifest(path, &snapshot); err != nil {
		m.mu.Lock()
		current := m.instances[name]
		if current == expected && current.instanceID == instanceID && current.State == StateStopping && current.manifest != nil && current.manifest.InstanceID == instanceID {
			current.manifest.GuestQuiesced = previous
		}
		m.mu.Unlock()
		return err
	}
	return nil
}

func (m *Manager) restoreRunningAfterQuiesceFailure(name string, expected *Instance, instanceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.instances[name]
	if current != expected || current.instanceID != instanceID || current.State != StateStopping || current.manifest == nil || current.manifest.InstanceID != instanceID {
		return fmt.Errorf("service %s instance changed while restoring after guest quiesce failure", name)
	}
	current.State = StateRunning
	current.manifest.Lifecycle = lifecycleRunning
	return writeManifest(manifestPath(current.manifest.VMDir), current.manifest)
}

func (m *Manager) quarantineStopping(name string, expected *Instance, instanceID string, manifest *instanceManifest, reason error) bool {
	m.mu.Lock()
	current := m.instances[name]
	if current != expected || current.instanceID != instanceID || current.State != StateStopping || current.manifest == nil || current.manifest.InstanceID != instanceID {
		m.mu.Unlock()
		return false
	}
	current.State = StateRecoveryPending
	current.LastError = reason.Error()
	current.manifest.LastError = reason.Error()
	_ = writeManifest(manifestPath(current.manifest.VMDir), current.manifest)
	m.mu.Unlock()
	manifest.LastError = reason.Error()
	m.logger.Error("VM stop requires operator intervention", "service", name, "error", reason,
		"command", "firework-agent clear-recovery "+name)
	return true
}

func (m *Manager) markProcessGone(name string, expected *Instance, instanceID string, manifest *instanceManifest, reason string) {
	m.mu.Lock()
	current := m.instances[name]
	if current != expected || current.instanceID != instanceID || current.State != StateStopping || current.manifest == nil || current.manifest.InstanceID != instanceID {
		m.mu.Unlock()
		return
	}
	current.State = StateFailed
	current.PID = 0
	current.LastError = reason
	current.manifest.PID = 0
	current.manifest.Lifecycle = lifecycleFailed
	current.manifest.LastError = current.LastError
	_ = writeManifest(manifestPath(current.manifest.VMDir), current.manifest)
	m.mu.Unlock()
	manifest.PID = 0
	manifest.Lifecycle = lifecycleFailed
	manifest.LastError = reason
}

func (m *Manager) completeStop(name string, expected *Instance, instanceID string, manifest *instanceManifest) error {
	m.mu.Lock()
	current := m.instances[name]
	if current != expected || current.instanceID != instanceID || current.State != StateStopping || current.manifest == nil || current.manifest.InstanceID != instanceID {
		m.mu.Unlock()
		return fmt.Errorf("service %s instance changed before stop completion", name)
	}
	current.State = StateStopped
	current.PID = 0
	current.LastError = ""
	current.manifest.PID = 0
	current.manifest.Lifecycle = lifecycleStopped
	current.manifest.LastError = ""
	apiSocket := current.manifest.SocketPath
	guestSocket := current.manifest.GuestVsockPath
	manifest.PID = 0
	manifest.Lifecycle = lifecycleStopped
	_ = writeManifest(manifestPath(current.manifest.VMDir), current.manifest)
	m.mu.Unlock()
	_ = os.Remove(apiSocket)
	if guestSocket != "" {
		_ = os.Remove(guestSocket)
	}
	m.logger.Info("microVM stopped", "service", name)
	return nil
}
