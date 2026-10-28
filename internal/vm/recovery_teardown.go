package vm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/artemnikitin/firework/internal/config"
)

const recoveryTeardownDir = "recovery-teardowns"

// Recovery deletion and host cleanup are separate transactions. This outbox
// retains the old configuration until the reconciler durably owns its cleanup.
type recoveryTeardown struct {
	Version  int              `json:"version"`
	Manifest instanceManifest `json:"manifest"`
}

func (e recoveryTeardown) id() string {
	data, _ := json.Marshal([]string{e.Manifest.Service, e.Manifest.InstanceID, e.Manifest.ConfigHash})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (e recoveryTeardown) validate(id string) error {
	if e.Version != 1 || !validRecoveryTeardownID(id) || e.id() != id {
		return fmt.Errorf("invalid recovery teardown identity")
	}
	if err := ValidateRecoveryServiceName(e.Manifest.Service); err != nil {
		return err
	}
	if e.Manifest.SchemaVersion != manifestSchemaVersion || e.Manifest.Config.Name != e.Manifest.Service || e.Manifest.InstanceID == "" {
		return fmt.Errorf("recovery teardown configuration does not identify its service")
	}
	hash, err := serviceConfigHash(e.Manifest.Config)
	if err != nil || hash != e.Manifest.ConfigHash {
		return fmt.Errorf("recovery teardown configuration hash does not match")
	}
	for _, forward := range e.Manifest.Config.PortForwards {
		if forward.HostPort < 1 || forward.HostPort > 65535 || forward.VMPort < 1 || forward.VMPort > 65535 ||
			e.Manifest.Config.Network == nil || e.Manifest.Config.Network.GuestIP == "" {
			return fmt.Errorf("invalid recovery teardown port forward")
		}
	}
	return nil
}

func validRecoveryTeardownID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(id) == id
}

func (m *Manager) persistRecoveryTeardown(f *instanceManifest) error {
	service := filepath.Base(f.VMDir)
	if err := ValidateRecoveryServiceName(service); err != nil {
		return err
	}
	if err := verifyRecoveryManifestPaths(service, filepath.Join(m.stateDir, "vms", service), f); err != nil {
		return err
	}
	// Manifestless legacy directories have no recorded identity or host
	// configuration to preserve. Complete manifests remain deletion intents
	// even without networking, so a partially removed directory can retry.
	if f.InstanceID == "" && f.Config.Network == nil {
		return nil
	}
	event := recoveryTeardown{Version: 1, Manifest: *cloneInstanceManifest(f)}
	id := event.id()
	if err := event.validate(id); err != nil {
		return err
	}
	root, err := m.openRecoveryTeardowns(true)
	if err != nil {
		return err
	}
	defer root.Close()
	name := id + ".json"
	if existing, err := readRecoveryTeardown(root, name, m.stateDir); err == nil {
		if !sameRecoveryIdentity(&existing.Manifest, &event.Manifest) {
			return fmt.Errorf("recovery teardown identity changed for the same event")
		}
		return nil // Immutable, stable identities make repeated persistence safe.
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	nonce, err := newInstanceID()
	if err != nil {
		return err
	}
	tempName := ".recovery-teardown-" + nonce + ".tmp"
	file, err := root.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tempName)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := root.Rename(tempName, name); err != nil {
		return err
	}
	return syncRecoveryTeardownDirectory(root)
}

// PendingRecoveryTeardowns returns immutable old configurations, including
// events whose VM directory was already removed before an agent restart.
func (m *Manager) PendingRecoveryTeardowns() (map[string]config.ServiceConfig, error) {
	pending := make(map[string]config.ServiceConfig)
	root, err := m.openRecoveryTeardowns(false)
	if errors.Is(err, os.ErrNotExist) {
		return pending, nil
	}
	if err != nil {
		return pending, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return pending, err
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return pending, err
	}
	var errs []error
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".recovery-teardown-") && strings.HasSuffix(entry.Name(), ".tmp") {
			continue // An interrupted atomic write never became an event.
		}
		event, err := readRecoveryTeardown(root, entry.Name(), m.stateDir)
		if err != nil {
			errs = append(errs, fmt.Errorf("read recovery teardown %s: %w", entry.Name(), err))
			continue
		}
		pending[event.id()] = event.Manifest.Config
	}
	return pending, errors.Join(errs...)
}

// AcknowledgeRecoveryTeardown is called only after a durable cleanup journal
// has accepted this event. Repeated acknowledgements are harmless.
func (m *Manager) AcknowledgeRecoveryTeardown(id string) error {
	if !validRecoveryTeardownID(id) {
		return fmt.Errorf("invalid recovery teardown ID")
	}
	root, err := m.openRecoveryTeardowns(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	name := id + ".json"
	event, err := readRecoveryTeardown(root, name, m.stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.instances[event.Manifest.Service]; m.recoveryReservations[event.Manifest.Service] || (current != nil && (current.State == StateRecoveryPending || current.State == StateStarting || current.State == StateStartAborting)) {
		return fmt.Errorf("recovery cleanup is still in progress for %s", event.Manifest.Service)
	}
	if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncRecoveryTeardownDirectory(root)
}

func (m *Manager) openRecoveryTeardowns(create bool) (*os.Root, error) {
	// The configured state root may itself be a trusted data-disk symlink.
	// Child directories and event files must still be real filesystem nodes.
	info, err := os.Stat(m.stateDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("recovery state directory must be a directory")
	}
	state, err := os.OpenRoot(m.stateDir)
	if err != nil {
		return nil, err
	}
	defer state.Close()
	opened, err := state.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return nil, fmt.Errorf("recovery state directory changed during access")
	}
	if create {
		if err := state.Mkdir(recoveryTeardownDir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if err := syncRecoveryTeardownDirectory(state); err != nil {
			return nil, err
		}
	}
	info, err = state.Lstat(recoveryTeardownDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("recovery teardown directory must be a private directory without a symlink")
	}
	root, err := state.OpenRoot(recoveryTeardownDir)
	if err != nil {
		return nil, err
	}
	opened, err = root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		root.Close()
		return nil, fmt.Errorf("recovery teardown directory changed during access")
	}
	return root, nil
}

func readRecoveryTeardown(root *os.Root, name, stateDir string) (*recoveryTeardown, error) {
	if !strings.HasSuffix(name, ".json") || !validRecoveryTeardownID(strings.TrimSuffix(name, ".json")) {
		return nil, fmt.Errorf("invalid recovery teardown filename")
	}
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("recovery teardown must be a private regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, fmt.Errorf("recovery teardown changed during access")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 4<<20))
	decoder.DisallowUnknownFields()
	var event recoveryTeardown
	if err := decoder.Decode(&event); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("recovery teardown has trailing data")
	}
	if err := event.validate(strings.TrimSuffix(name, ".json")); err != nil {
		return nil, err
	}
	if err := verifyRecoveryManifestPaths(event.Manifest.Service,
		filepath.Join(stateDir, "vms", event.Manifest.Service), &event.Manifest); err != nil {
		return nil, err
	}
	return &event, nil
}

// recoveryTeardownManifest recovers the exact deletion intent after a partial
// RemoveAll removed instance.json. Distinct old incarnations are ambiguous.
func (m *Manager) recoveryTeardownManifest(service string) (*instanceManifest, error) {
	if err := ValidateRecoveryServiceName(service); err != nil {
		return nil, err
	}
	root, err := m.openRecoveryTeardowns(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return nil, err
	}
	var manifest *instanceManifest
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".recovery-teardown-") && strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		event, err := readRecoveryTeardown(root, entry.Name(), m.stateDir)
		if err != nil {
			return nil, err
		}
		if event.Manifest.Service != service {
			continue
		}
		if manifest != nil && !sameRecoveryIdentity(manifest, &event.Manifest) {
			return nil, fmt.Errorf("multiple incompatible recovery teardown identities for %s", service)
		}
		manifest = cloneInstanceManifest(&event.Manifest)
	}
	return manifest, nil
}

func (m *Manager) hasRecoveryTeardown(manifest *instanceManifest) (bool, error) {
	root, err := m.openRecoveryTeardowns(false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer root.Close()
	event, err := readRecoveryTeardown(root, (recoveryTeardown{Manifest: *manifest}).id()+".json", m.stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return sameRecoveryIdentity(manifest, &event.Manifest), nil
}

func syncRecoveryTeardownDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
