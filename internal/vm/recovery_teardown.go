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
	Version    int                  `json:"version"`
	Service    string               `json:"service"`
	InstanceID string               `json:"instance_id"`
	ConfigHash string               `json:"config_hash"`
	Config     config.ServiceConfig `json:"config"`
}

func (e recoveryTeardown) id() string {
	data, _ := json.Marshal([]string{e.Service, e.InstanceID, e.ConfigHash})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (e recoveryTeardown) validate(id string) error {
	if e.Version != 1 || !validRecoveryTeardownID(id) || e.id() != id {
		return fmt.Errorf("invalid recovery teardown identity")
	}
	if err := ValidateRecoveryServiceName(e.Service); err != nil {
		return err
	}
	if e.Config.Name != e.Service || e.InstanceID == "" {
		return fmt.Errorf("recovery teardown configuration does not identify its service")
	}
	hash, err := serviceConfigHash(e.Config)
	if err != nil || hash != e.ConfigHash {
		return fmt.Errorf("recovery teardown configuration hash does not match")
	}
	for _, forward := range e.Config.PortForwards {
		if forward.HostPort < 1 || forward.HostPort > 65535 || forward.VMPort < 1 || forward.VMPort > 65535 ||
			e.Config.Network == nil || e.Config.Network.GuestIP == "" {
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
	// Manifestless legacy directories have no recorded host configuration.
	// There is no cleanup instruction to transfer in that case.
	if f.Config.Network == nil {
		return nil
	}
	event := recoveryTeardown{Version: 1, Service: service, InstanceID: f.InstanceID, ConfigHash: f.ConfigHash, Config: f.Config}
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
	if _, err := readRecoveryTeardown(root, name); err == nil {
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
		event, err := readRecoveryTeardown(root, entry.Name())
		if err != nil {
			errs = append(errs, fmt.Errorf("read recovery teardown %s: %w", entry.Name(), err))
			continue
		}
		pending[event.id()] = event.Config
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
	if _, err := readRecoveryTeardown(root, name); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncRecoveryTeardownDirectory(root)
}

func (m *Manager) openRecoveryTeardowns(create bool) (*os.Root, error) {
	info, err := os.Lstat(m.stateDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("recovery state directory must be a directory without a symlink")
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

func readRecoveryTeardown(root *os.Root, name string) (*recoveryTeardown, error) {
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
	return &event, nil
}

func syncRecoveryTeardownDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
