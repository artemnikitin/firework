package vm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/artemnikitin/firework/internal/config"
)

// SetRecoveryCleanupRecorder connects recovery removal to the reconciler's
// existing durable host-resource journal. The callback runs before VM files
// are removed and without the manager mutex held.
func (m *Manager) SetRecoveryCleanupRecorder(record func(config.ServiceConfig) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recoveryCleanupRecorder = record
}

func (m *Manager) persistRecoveryTeardown(manifest *instanceManifest) error {
	if err := verifyRecoveryManifestPaths(manifest.Service,
		filepath.Join(m.stateDir, "vms", manifest.Service), manifest); err != nil {
		return err
	}
	m.mu.Lock()
	record := m.recoveryCleanupRecorder
	m.mu.Unlock()
	if record == nil {
		return nil // Standalone VM managers delegate host networking to callers.
	}
	return record(manifest.Config)
}

// ProveServiceAbsent proves that no live process and no active or queued
// systemd launch uses the service's VM paths. The reconciler calls it for an
// allocation whose start was interrupted before the VM was tracked, so the
// proof needs no ownership manifest: the paths derive from the state
// directory and the service name. VM state with a manifest is left to
// recovery, which tracks it.
func (m *Manager) ProveServiceAbsent(ctx context.Context, service string) error {
	if err := ValidateRecoveryServiceName(service); err != nil {
		return err
	}
	m.mu.Lock()
	_, tracked := m.instances[service]
	m.mu.Unlock()
	if tracked {
		return fmt.Errorf("service %s has a tracked VM", service)
	}
	vmDir := filepath.Join(m.stateDir, "vms", service)
	if _, err := os.Lstat(manifestPath(vmDir)); err == nil || !os.IsNotExist(err) {
		return fmt.Errorf("VM state for %s has an ownership manifest; recovery decides its ownership", service)
	}
	paths := &instanceManifest{Service: service, VMDir: vmDir,
		SocketPath: filepath.Join(vmDir, firecrackerSocketName), ConfigPath: filepath.Join(vmDir, vmConfigName)}
	if err := m.noLiveClaimantContext(ctx, paths); err != nil {
		return err
	}
	return m.noClaimingUnitContext(ctx, paths)
}
