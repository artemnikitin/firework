package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/volume"
)

// Recover discovers Firecracker processes that survived an agent restart.
// It returns the names of safely adopted services so the reconciler can
// recreate idempotent host networking and health-monitor state.
func (m *Manager) Recover(_ context.Context, desired config.NodeConfig) ([]string, error) {
	m.mu.Lock()
	if m.recovered {
		m.mu.Unlock()
		return nil, nil
	}
	m.mu.Unlock()

	root := filepath.Join(m.stateDir, "vms")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read VM state directory: %w", err)
	}
	m.mu.Lock()
	m.recovered = true
	m.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	desiredByName := make(map[string]config.ServiceConfig, len(desired.Services))
	for _, service := range desired.Services {
		desiredByName[service.Name] = service
	}
	seenIDs := make(map[string]string)
	var adopted []string
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		vmDir := filepath.Join(root, name)
		path := manifestPath(vmDir)
		manifest, readErr := readManifest(path)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				var handled bool
				manifest, handled, err = m.recoverLegacy(name, vmDir, desiredByName)
				if err != nil {
					errs = append(errs, err)
				}
				if handled {
					continue
				}
			} else {
				m.addRecoveryPending(name, &instanceManifest{
					SchemaVersion: manifestSchemaVersion, Service: name, VMDir: vmDir,
					SocketPath: filepath.Join(vmDir, "firecracker.sock"),
				}, fmt.Sprintf("cannot read ownership manifest: %v", readErr))
				continue
			}
		}
		if manifest.Service != name || manifest.VMDir != vmDir ||
			manifest.SocketPath != filepath.Join(vmDir, "firecracker.sock") ||
			manifest.ConfigPath != filepath.Join(vmDir, "vm-config.json") {
			m.addRecoveryPending(name, manifest, "ownership manifest paths do not match its state directory")
			continue
		}
		hash, hashErr := serviceConfigHash(manifest.Config)
		if hashErr != nil || hash != manifest.ConfigHash {
			m.addRecoveryPending(name, manifest, "ownership manifest service configuration hash does not match")
			continue
		}
		if other, duplicate := seenIDs[manifest.InstanceID]; duplicate {
			m.addRecoveryPending(name, manifest, fmt.Sprintf("instance ID is also owned by service %s", other))
			m.markExistingRecoveryPending(other, fmt.Sprintf("instance ID is also owned by service %s", name))
			continue
		}
		seenIDs[manifest.InstanceID] = name

		if manifest.PID == 0 && (manifest.Lifecycle == lifecycleStopped || manifest.Lifecycle == lifecycleFailed) {
			if removeErr := os.RemoveAll(vmDir); removeErr != nil {
				errs = append(errs, fmt.Errorf("clean stale state for %s: %w", name, removeErr))
			}
			continue
		}
		if manifest.PID <= 0 {
			if manifest.Lifecycle == lifecycleStarting && manifest.Launcher == "systemd" && manifest.LauncherUnit != "" {
				if pid, lookupErr := systemdMainPID("systemctl", manifest.LauncherUnit); lookupErr == nil {
					manifest.PID = pid
					if identity, inspectErr := m.inspector.Inspect(pid); inspectErr == nil {
						manifest.HostBootID = identity.HostBootID
						manifest.ProcessStart = identity.StartTicks
						manifest.Executable = identity.Executable
						manifest.ExecutableDev = identity.ExecutableDev
						manifest.ExecutableIno = identity.ExecutableIno
					}
				}
			}
		}
		if manifest.PID <= 0 {
			m.addRecoveryPending(name, manifest, fmt.Sprintf("manifest is %s but has no process identity", manifest.Lifecycle))
			continue
		}
		if processErr := validateOwnedProcess(m.inspector, manifest); processErr != nil {
			if errors.Is(processErr, errProcessNotFound) {
				if removeErr := os.RemoveAll(vmDir); removeErr != nil {
					errs = append(errs, fmt.Errorf("clean dead VM state for %s: %w", name, removeErr))
				}
				continue
			}
			m.addRecoveryPending(name, manifest, fmt.Sprintf("cannot prove surviving process ownership: %v", processErr))
			continue
		}
		if socketErr := validateOwnedSocket(m.inspector, manifest); socketErr != nil {
			m.addRecoveryPending(name, manifest, fmt.Sprintf("surviving process API socket is not ready: %v", socketErr))
			continue
		}

		manifest.Lifecycle = lifecycleRunning
		manifest.LastError = ""
		if writeErr := writeManifest(path, manifest); writeErr != nil {
			errs = append(errs, fmt.Errorf("persist adoption for %s: %w", name, writeErr))
			continue
		}
		m.mu.Lock()
		m.instances[name] = instanceFromManifest(manifest, StateRunning, "")
		m.mu.Unlock()
		adopted = append(adopted, name)
		go m.monitorAdopted(name, manifest.InstanceID)
		m.logger.Info("adopted surviving microVM", "service", name, "pid", manifest.PID, "launcher", manifest.Launcher)
	}
	if len(errs) > 0 {
		return adopted, fmt.Errorf("VM recovery had %d error(s): %v", len(errs), errs)
	}
	return adopted, nil
}

func (m *Manager) recoverLegacy(name, vmDir string, desired map[string]config.ServiceConfig) (*instanceManifest, bool, error) {
	service, wanted := desired[name]
	partial := &instanceManifest{
		SchemaVersion: manifestSchemaVersion, Service: name, VMDir: vmDir,
		SocketPath: filepath.Join(vmDir, "firecracker.sock"), ConfigPath: filepath.Join(vmDir, "vm-config.json"),
	}
	if !wanted {
		m.addRecoveryPending(name, partial, "legacy VM state has no ownership manifest and no desired service configuration")
		return nil, true, nil
	}
	matches, err := m.inspector.FindByArguments(partial.SocketPath, partial.ConfigPath)
	if err != nil {
		m.addRecoveryPending(name, partial, fmt.Sprintf("cannot inspect legacy VM processes: %v", err))
		return nil, true, nil
	}
	expectedExecutable, err := filepath.EvalSymlinks(m.firecrackerBin)
	if err != nil {
		m.addRecoveryPending(name, partial, fmt.Sprintf("cannot resolve configured Firecracker executable: %v", err))
		return nil, true, nil
	}
	expectedExecutable, err = filepath.Abs(expectedExecutable)
	if err != nil {
		return nil, true, fmt.Errorf("resolve Firecracker executable path: %w", err)
	}
	var owned []processIdentity
	for _, identity := range matches {
		if identity.Executable == expectedExecutable {
			owned = append(owned, identity)
		}
	}
	if len(owned) == 0 {
		if len(matches) > 0 {
			m.addRecoveryPending(name, partial, "legacy process arguments match but its executable is not the configured Firecracker binary")
			return nil, true, nil
		}
		if err := os.RemoveAll(vmDir); err != nil {
			return nil, true, fmt.Errorf("clean stale legacy state for %s: %w", name, err)
		}
		return nil, true, nil
	}
	if len(owned) != 1 {
		m.addRecoveryPending(name, partial, fmt.Sprintf("found %d matching legacy Firecracker processes", len(owned)))
		return nil, true, nil
	}
	identity := owned[0]
	instanceID, err := newInstanceID()
	if err != nil {
		return nil, true, err
	}
	hash, err := serviceConfigHash(service)
	if err != nil {
		return nil, true, err
	}
	return &instanceManifest{
		SchemaVersion: manifestSchemaVersion, Service: name, InstanceID: instanceID,
		Lifecycle: lifecycleRunning, Config: service, ConfigHash: hash, PID: identity.PID,
		HostBootID: identity.HostBootID, ProcessStart: identity.StartTicks,
		Executable: identity.Executable, ExecutableDev: identity.ExecutableDev, ExecutableIno: identity.ExecutableIno,
		SocketPath: partial.SocketPath, ConfigPath: partial.ConfigPath, VMDir: vmDir,
		Launcher: "direct", Legacy: true, StartedAt: time.Now().UTC(),
	}, false, nil
}

func instanceFromManifest(manifest *instanceManifest, state State, lastError string) *Instance {
	serviceConfig := manifest.Config
	if serviceConfig.Name == "" {
		serviceConfig.Name = manifest.Service
	}
	return &Instance{
		Name: manifest.Service, Config: serviceConfig, State: state, PID: manifest.PID,
		LastError: lastError, SocketPath: manifest.SocketPath,
		Volumes:    append([]volume.PreparedVolume(nil), manifest.Volumes...),
		instanceID: manifest.InstanceID, manifest: manifest,
	}
}

func (m *Manager) addRecoveryPending(name string, manifest *instanceManifest, reason string) {
	manifest.LastError = reason
	m.mu.Lock()
	m.instances[name] = instanceFromManifest(manifest, StateRecoveryPending, reason)
	m.mu.Unlock()
	m.logger.Error("VM recovery requires operator intervention", "service", name, "error", reason)
}

func (m *Manager) markExistingRecoveryPending(name, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if instance := m.instances[name]; instance != nil {
		instance.State = StateRecoveryPending
		instance.LastError = reason
		if instance.manifest != nil {
			instance.manifest.LastError = reason
			_ = writeManifest(manifestPath(instance.manifest.VMDir), instance.manifest)
		}
	}
}

// monitorAdopted observes a process without requiring it to remain a child of
// the current agent. Identity mismatch is quarantined instead of interpreted
// as exit, which prevents PID reuse from mutating or killing another process.
func (m *Manager) monitorAdopted(name, instanceID string) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		m.mu.Lock()
		instance := m.instances[name]
		if instance == nil || instance.instanceID != instanceID || instance.State != StateRunning {
			m.mu.Unlock()
			return
		}
		manifest := *instance.manifest
		m.mu.Unlock()

		err := validateOwnedProcess(m.inspector, &manifest)
		if err == nil {
			continue
		}
		m.mu.Lock()
		current := m.instances[name]
		active := current != nil && current.instanceID == instanceID && current.State == StateRunning
		var currentManifest *instanceManifest
		if active {
			currentManifest = current.manifest
		}
		m.mu.Unlock()
		if !active {
			return
		}
		if !errors.Is(err, errProcessNotFound) {
			m.quarantine(name, currentManifest, fmt.Errorf("process identity changed while monitoring: %w", err))
			return
		}

		m.mu.Lock()
		instance = m.instances[name]
		if instance == nil || instance.instanceID != instanceID || instance.State != StateRunning {
			m.mu.Unlock()
			return
		}
		instance.State = StateFailed
		instance.PID = 0
		instance.LastError = "adopted Firecracker process exited"
		instance.manifest.PID = 0
		instance.manifest.Lifecycle = lifecycleFailed
		instance.manifest.LastError = instance.LastError
		_ = writeManifest(manifestPath(instance.manifest.VMDir), instance.manifest)
		m.mu.Unlock()
		return
	}
}
