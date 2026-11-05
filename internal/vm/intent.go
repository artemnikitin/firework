package vm

import (
	"fmt"
	"os"
	"path/filepath"
)

// Intent markers are the manager's durable evidence of its own directory
// operations. One is written before the manager creates or removes a VM
// directory, and removed once the directory is complete (its ownership
// manifest is written) or gone. A directory without a manifest that has a
// marker is an operation this manager was interrupted in, so recovery may
// clear it after its absence proof. Without one, nothing the agent recorded
// explains the state: it is untrusted and only --forget clears it.
//
// Markers live in the vms root beside the VM directories. Inventory skips
// regular files, so a marker is never taken for a VM.
const intentPrefix = ".firework-intent-"

func (m *Manager) intentPath(service string) string {
	return filepath.Join(m.stateDir, "vms", intentPrefix+service)
}

// markIntent durably records that the manager is about to create or remove
// the service's VM directory.
func (m *Manager) markIntent(service string) error {
	root := filepath.Join(m.stateDir, "vms")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("recording the VM directory operation for %s: %w", service, err)
	}
	if err := writeManifestFile(m.intentPath(service), []byte(service+"\n")); err != nil {
		return fmt.Errorf("recording the VM directory operation for %s: %w", service, err)
	}
	return nil
}

// clearIntent removes the marker once the operation finished. A marker left
// by a failed removal only keeps evidence of an operation the manager did
// start.
func (m *Manager) clearIntent(service string) {
	if err := os.Remove(m.intentPath(service)); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("failed to remove a VM directory operation marker", "service", service, "error", err)
		return
	}
	if dir, err := os.Open(filepath.Join(m.stateDir, "vms")); err == nil {
		_ = dir.Sync()
		dir.Close()
	}
}

// hasIntent reports whether the manager recorded an operation on the
// service's VM directory that it did not finish.
func (m *Manager) hasIntent(service string) bool {
	info, err := os.Lstat(m.intentPath(service))
	return err == nil && info.Mode().IsRegular()
}
