package vm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

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

// persistRecoveryTeardowns records cleanup for state cleared through an
// absence probe. An untrusted probe carries only the canonical paths and the
// service name, so the in-memory snapshots the manager still trusts, never
// disk contents, contribute what the VM was given. Without any, the probe
// itself is recorded, which departs the service's recorded allocations.
func (m *Manager) persistRecoveryTeardowns(probe *instanceManifest, trusted []*instanceManifest) error {
	if len(trusted) == 0 {
		return m.persistRecoveryTeardown(probe)
	}
	for i, snapshot := range trusted {
		if slices.ContainsFunc(trusted[:i], func(earlier *instanceManifest) bool {
			return reflect.DeepEqual(earlier.Config, snapshot.Config)
		}) {
			continue
		}
		if err := m.persistRecoveryTeardown(snapshot); err != nil {
			return err
		}
	}
	return nil
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

// vmServiceUnder returns the service whose VM directory under root a
// Firecracker socket or config argument names, or "".
func vmServiceUnder(args []string, root string) string {
	prefix := filepath.Clean(root) + string(filepath.Separator)
	for i, arg := range args {
		value := ""
		switch {
		case (arg == "--api-sock" || arg == "--config-file") && i+1 < len(args):
			value = args[i+1]
		case strings.HasPrefix(arg, "--api-sock="), strings.HasPrefix(arg, "--config-file="):
			_, value, _ = strings.Cut(arg, "=")
		}
		if rest, ok := strings.CutPrefix(filepath.Clean(value), prefix); ok && value != "" {
			if service, _, found := strings.Cut(rest, string(filepath.Separator)); found && service != "" {
				return service
			}
		}
	}
	return ""
}

// UnaccountedVMs lists VM processes and systemd launches under this state
// directory that no tracked VM accounts for, such as a VM whose directory was
// removed by hand while it ran. Their guest addresses are unknown, so the
// reconciler gives no new address to anyone while any exists.
func (m *Manager) UnaccountedVMs(ctx context.Context) ([]string, error) {
	scanner, ok := m.inspector.(interface {
		FindVMProcessesContext(context.Context, string) ([]processIdentity, error)
	})
	if !ok {
		return nil, fmt.Errorf("this host cannot scan for VM processes")
	}
	root := filepath.Join(m.stateDir, "vms")
	processes, err := scanner.FindVMProcessesContext(ctx, root)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	tracked := make(map[string]bool, len(m.instances))
	units := make(map[string]bool, len(m.instances))
	for name, instance := range m.instances {
		tracked[name] = true
		if instance.instanceID != "" {
			units["firework-vm-"+instance.instanceID+".service"] = true
		}
		if instance.manifest != nil && instance.manifest.LauncherUnit != "" {
			units[instance.manifest.LauncherUnit] = true
		}
	}
	_, systemd := m.launcher.(*systemdLauncher)
	m.mu.Unlock()
	var found []string
	for _, process := range processes {
		if service := vmServiceUnder(process.CommandLine, root); !tracked[service] {
			found = append(found, fmt.Sprintf("process %d (VM directory of %s)", process.PID, service))
		}
	}
	if !systemd {
		return found, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	run := m.recoveryCommandOutput
	if run == nil {
		run = commandOutput
	}
	out, err := run(ctx, m.recoverySystemctl(), "show", "firework-vm-*.service", "--property=Id,ActiveState,MainPID,Job")
	if err != nil {
		return nil, fmt.Errorf("inspect systemd launches: %w", err)
	}
	for _, block := range strings.Split(string(out), "\n\n") {
		fields := parseUnitProperties(block)
		id := fields["Id"]
		inactive := fields["ActiveState"] == "inactive" || fields["ActiveState"] == "failed"
		if id == "" || units[id] || inactive && (fields["MainPID"] == "" || fields["MainPID"] == "0") && fields["Job"] == "" {
			continue
		}
		found = append(found, "systemd unit "+id)
	}
	return found, nil
}
