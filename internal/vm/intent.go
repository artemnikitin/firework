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
		// contents never authorize adoption or deletion, but once something
		// else explains the VM state (a validated trusted manifest or an
		// operation this manager finished) it is redundant and retired even
		// if damaged. Deleting VM state while a directory sits at this path
		// needs --forget (removeProvenRecoveryState). A legacy path shares
		// the VM namespace, so only a matching regular marker may be unlinked
		// there.
		if path == m.legacyIntentPath(service) && !intentFileMatches(path, service) {
			continue
		}
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			errs = append(errs, fmt.Errorf("inspect VM directory intent %s: %w", path, err))
			continue
		}
		var removeErr error
		if path == m.legacyIntentPath(service) {
			// Unlink cannot remove another service's runtime directory if a
			// legacy marker is replaced after it was inspected.
			removeErr = syscall.Unlink(path)
		} else {
			// This is private bookkeeping outside the VM namespace. A
			// damaged directory there is retired too, without following links.
			removeErr = os.RemoveAll(path)
		}
		if err := removeErr; err != nil && !os.IsNotExist(err) {
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
	matched, _ := readIntent(path, service)
	return matched
}

func readIntent(path, service string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || (info.Size() != int64(len(service)) && info.Size() != int64(len(service)+1)) {
		return false, nil
	}
	data, err := os.ReadFile(path)
	return err == nil && (string(data) == service || string(data) == service+"\n"), err
}

// legacyIntentOwner identifies a marker occupying a possible service name in
// the old namespace. Starts wait for its owner's recovery instead of turning
// the marker into an unrelated, permanently quarantined VM entry.
func (m *Manager) legacyIntentOwner(name string) string {
	service, ok := strings.CutPrefix(name, intentPrefix)
	if !ok || ValidateRecoveryServiceName(service) != nil {
		return ""
	}
	matched, err := readIntent(m.legacyIntentPath(service), service)
	if matched || err != nil && !os.IsNotExist(err) {
		return service // Unavailable evidence is a wait, never another VM's state.
	}
	return ""
}

// inventoryIntents fences interrupted operations even if their VM directory
// was never created or is already gone. A completed, durably described VM no
// longer needs its marker; retry retiring it if an earlier unlink failed.
func (m *Manager) inventoryIntents(legacy []os.DirEntry) error {
	current, err := os.ReadDir(m.stateDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read VM directory intents: %w", err)
	}
	m.inventoryIntentEntries(current, legacy)
	return nil
}

// A listing is only a candidate snapshot: another resolver may clear an
// entry before inventory acquires the manager lock.
func (m *Manager) inventoryIntentEntries(current, legacy []os.DirEntry) {
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
		if !m.hasIntentEntry(service) {
			continue // The operation completed since its directory was listed.
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
}
