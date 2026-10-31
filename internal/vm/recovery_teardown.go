package vm

import (
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
