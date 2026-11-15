package vm

import (
	"context"
	"fmt"
	"time"
)

// GuestQuiesceRequiredError leaves a live VM intact for another resize attempt.
type GuestQuiesceRequiredError struct {
	Cause             error
	ApplicationExited bool
}

func (e *GuestQuiesceRequiredError) Error() string {
	return fmt.Sprintf("guest quiesce required before resizing: %v", e.Cause)
}
func (e *GuestQuiesceRequiredError) Unwrap() error { return e.Cause }

func (m *Manager) requestGuestQuiesce(manifest *instanceManifest) (bool, error) {
	path, err := guestVsockPath(manifest)
	if err != nil {
		return false, err
	}
	app, flush := m.guestApplicationShutdownTimeout, m.guestFlushTimeout
	if app <= 0 {
		app = 15 * time.Second
	}
	if flush <= 0 {
		flush = 15 * time.Second
	}
	budget, err := guestQuiesceBudget(app, flush)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	quiescer := m.quiescer
	if quiescer == nil {
		quiescer = firecrackerGuestQuiescer{}
	}
	response, err := quiescer.Quiesce(ctx, path, app, flush)
	if err != nil {
		return false, err
	}
	exited := response.Validate() == nil && response.AppExited
	if err := ctx.Err(); err != nil {
		return exited, err
	}
	return exited, validateGuestQuiesceResponse(response, manifest)
}

func (m *Manager) validateStopOwner(name string, expected *Instance, manifest *instanceManifest) error {
	m.mu.Lock()
	current := m.instances[name]
	valid := current == expected && current.instanceID == manifest.InstanceID &&
		(current.State == StateStopping || current.State == StateStopped)
	m.mu.Unlock()
	if !valid {
		return fmt.Errorf("service %s stopping owner changed: %w", name, ErrStartInProgress)
	}
	return validateOwnedProcess(m.inspector, manifest)
}

func (m *Manager) restoreAfterQuiesceFailure(name string, expected *Instance, manifest *instanceManifest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.instances[name]
	if current != expected || current.instanceID != manifest.InstanceID || current.State != StateStopping {
		return fmt.Errorf("service %s owner changed after quiesce refusal: %w", name, ErrStartInProgress)
	}
	current.manifest.Lifecycle = lifecycleRunning
	if err := m.writeRecoveryManifest(manifestPath(current.manifest.VMDir), current.manifest); err != nil {
		current.State = StateRecoveryPending
		current.LastError = fmt.Sprintf("persist quiesce refusal: %v", err)
		return fmt.Errorf("persist quiesce refusal: %w", err)
	}
	current.State = StateRunning
	return nil
}

func (m *Manager) stopLauncher(manifest *instanceManifest) processLauncher {
	if m.launcher != nil {
		switch m.launcher.(type) {
		case *directLauncher, *systemdLauncher:
		default:
			return m.launcher
		}
	}
	return launcherForManifest(m.firecrackerBin, manifest)
}
