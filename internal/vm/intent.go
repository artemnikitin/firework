package vm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Intent markers are the manager's durable evidence of its own directory
// operations. One is written before the manager creates or removes a VM
// directory, and removed once the directory is complete (its ownership
// manifest is written) or gone. A directory without a manifest that has a
// marker is an operation this manager was interrupted in, so recovery may
// clear it after its absence proof. Without one, nothing the agent recorded
// explains the state: it is untrusted and only --forget clears it.
//
// Markers live in stateDir, outside the vms service namespace. Older agents
// wrote them inside vms; those regular files remain readable until recovery
// finishes the recorded operation. A VM directory is never a marker.
const intentPrefix = ".firework-intent-"

func (m *Manager) intentPath(service string) string {
	return filepath.Join(m.stateDir, intentPrefix+service)
}

func (m *Manager) legacyIntentPath(service string) string {
	return filepath.Join(m.stateDir, "vms", intentPrefix+service)
}

// markIntent durably records that the manager is about to create or remove
// the service's VM directory.
func (m *Manager) markIntent(service string) error {
	root := filepath.Join(m.stateDir, "vms")
	// Keep the vms root as evidence of earlier host use even if the agent
	// crashes before creating a service directory. Writing the marker syncs
	// stateDir, including the new vms directory entry.
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
func (m *Manager) clearIntent(service string) error {
	var errs []error
	for _, path := range []string{m.intentPath(service), m.legacyIntentPath(service)} {
		// The current path is private bookkeeping for this service. Its
		// contents never authorize recovery, but a completed operation can
		// retire it even if damaged. A legacy path shares the VM namespace,
		// so only a matching regular marker may be unlinked there.
		if path == m.legacyIntentPath(service) && !intentFileMatches(path, service) {
			continue
		}
		// Unlike os.Remove, Unlink never falls back to removing a directory:
		// a legacy path may now be another service's VM directory.
		if err := syscall.Unlink(path); err != nil && !os.IsNotExist(err) {
			m.logger.Warn("failed to remove a VM directory operation marker", "service", service, "path", path, "error", err)
			errs = append(errs, fmt.Errorf("remove VM directory intent %s: %w", path, err))
			continue
		}
		if dir, err := os.Open(filepath.Dir(path)); err == nil {
			if err := dir.Sync(); err != nil {
				errs = append(errs, err)
			}
			dir.Close()
		}
	}
	return errors.Join(errs...)
}

// hasIntent reports whether the manager recorded an operation on the
// service's VM directory that it did not finish.
func (m *Manager) hasIntent(service string) bool {
	return intentFileMatches(m.intentPath(service), service) || intentFileMatches(m.legacyIntentPath(service), service)
}

// An unreadable current marker still fences its service, but never grants
// cleanup authority. The legacy namespace needs a matching record to identify
// its owner because an entry there may belong to another service.
func (m *Manager) hasIntentEntry(service string) bool {
	_, err := os.Lstat(m.intentPath(service))
	return !os.IsNotExist(err) || intentFileMatches(m.legacyIntentPath(service), service)
}

func intentFileMatches(path, service string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || (info.Size() != int64(len(service)) && info.Size() != int64(len(service)+1)) {
		return false
	}
	data, err := os.ReadFile(path)
	return err == nil && (string(data) == service || string(data) == service+"\n")
}

// legacyIntentOwner identifies a marker occupying a possible service name in
// the old namespace. Starts wait for its owner's recovery instead of turning
// the marker into an unrelated, permanently quarantined VM entry.
func (m *Manager) legacyIntentOwner(name string) string {
	service, ok := strings.CutPrefix(name, intentPrefix)
	if !ok || ValidateRecoveryServiceName(service) != nil || !intentFileMatches(m.legacyIntentPath(service), service) {
		return ""
	}
	return service
}

// inventoryIntents fences interrupted operations even if their VM directory
// was never created or is already gone. A completed, durably described VM no
// longer needs its marker; retry retiring it if an earlier unlink failed.
func (m *Manager) inventoryIntents(legacy []os.DirEntry) error {
	current, err := os.ReadDir(m.stateDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read VM directory intents: %w", err)
	}
	owners := make(map[string]bool)
	for _, source := range []struct {
		root    string
		entries []os.DirEntry
	}{{m.stateDir, current}, {filepath.Join(m.stateDir, "vms"), legacy}} {
		for _, entry := range source.entries {
			service, ok := strings.CutPrefix(entry.Name(), intentPrefix)
			if ok && ValidateRecoveryServiceName(service) == nil && (source.root == m.stateDir || intentFileMatches(filepath.Join(source.root, entry.Name()), service)) {
				owners[service] = true
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for service := range owners {
		if m.recoveryReservations[service] {
			continue
		}
		if instance := m.instances[service]; instance != nil {
			if instance.manifest != nil && (instance.State == StateRunning || instance.State == StateStopped || instance.State == StateFailed) {
				if _, err := readAndVerifyRecoveryManifest(manifestPath(instance.manifest.VMDir), instance.manifest); err == nil {
					m.clearIntent(service)
				}
			}
			continue
		}
		vmDir := filepath.Join(m.stateDir, "vms", service)
		m.instances[service] = m.recoveryInstanceForService(service, canonicalManifest(service, vmDir), "interrupted VM directory operation")
	}
	return nil
}
