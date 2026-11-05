package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// inspectRecoveryUnit treats activating/deactivating units as potential owners.
// A missing unit is established by LoadState, never by a failed systemctl call.
// systemd-run --no-block can leave a queued start job on an inactive unit with
// no MainPID, so a pending job also keeps the unit a potential owner.
func (m *Manager) inspectRecoveryUnitContext(ctx context.Context, f *instanceManifest) (int, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if m.recoveryUnitInspectorContext != nil {
		return m.recoveryUnitInspectorContext(ctx, f)
	}
	if f.LauncherUnit != "firework-vm-"+f.InstanceID+".service" {
		return 0, false, fmt.Errorf("invalid systemd ownership unit")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	run := m.recoveryCommandOutput
	if run == nil {
		run = commandOutput
	}
	out, err := run(ctx, m.recoverySystemctl(), "show", f.LauncherUnit, "--property=LoadState,ActiveState,MainPID,Job")
	if err != nil {
		return 0, false, fmt.Errorf("inspect systemd ownership: %w", err)
	}
	fields := parseUnitProperties(string(out))
	pid, err := strconv.Atoi(fields["MainPID"])
	if err != nil {
		return 0, false, fmt.Errorf("invalid systemd MainPID")
	}
	absent := fields["LoadState"] == "not-found" || fields["ActiveState"] == "inactive" || fields["ActiveState"] == "failed"
	return pid, absent && pid == 0 && fields["Job"] == "", nil
}

func parseUnitProperties(block string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			fields[k] = v
		}
	}
	return fields
}

func (m *Manager) recoverySystemctl() string {
	if launcher, ok := m.launcher.(*systemdLauncher); ok && launcher.systemctl != "" {
		return launcher.systemctl
	}
	return "systemctl"
}

// noClaimingUnitContext proves that no Firework transient unit may still run
// Firecracker for these paths. A queued --no-block unit has no process for the
// claimant scan to find, and untrusted metadata may not name its unit. Hosts
// whose agent cannot create system units and whose record names no unit skip it.
//
// One systemctl call reads every unit's ExecStart. systemctl renders argv
// joined by spaces without quoting, so the canonical paths are matched as
// substrings of a potential owner's whole property block: a path containing
// whitespace or separators still matches, and an unrelated argument that
// happens to contain a path only keeps the entry quarantined. systemctl reaches
// systemd over its private socket as root, so no D-Bus daemon is required.
func (m *Manager) noClaimingUnitContext(ctx context.Context, f *instanceManifest) error {
	if _, ok := m.launcher.(*systemdLauncher); !ok && f.Launcher != "systemd" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	run := m.recoveryCommandOutput
	if run == nil {
		run = commandOutput
	}
	out, err := run(ctx, m.recoverySystemctl(), "show", "firework-vm-*.service", "--property=Id,ActiveState,MainPID,Job,ExecStart")
	if err != nil {
		return fmt.Errorf("inspect systemd launches: %w", err)
	}
	// A blank line separates units, so a path containing one could split a
	// unit's ExecStart from its state. Such paths fail closed.
	ambiguousPaths := strings.ContainsAny(f.SocketPath+f.ConfigPath, "\r\n")
	for _, block := range strings.Split(string(out), "\n\n") {
		fields := parseUnitProperties(block)
		if fields["Id"] == "" {
			continue
		}
		inactive := fields["ActiveState"] == "inactive" || fields["ActiveState"] == "failed"
		if inactive && (fields["MainPID"] == "" || fields["MainPID"] == "0") && fields["Job"] == "" {
			continue
		}
		if ambiguousPaths {
			return fmt.Errorf("systemd unit %s may still run this service's VM: its state path contains a line break", fields["Id"])
		}
		if !strings.Contains(block, "ExecStart=") {
			return fmt.Errorf("systemd unit %s reported no ExecStart", fields["Id"])
		}
		if (f.SocketPath != "" && strings.Contains(block, f.SocketPath)) || (f.ConfigPath != "" && strings.Contains(block, f.ConfigPath)) {
			return fmt.Errorf("systemd unit %s may still run this service's VM", fields["Id"])
		}
	}
	return nil
}

func commandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

func (m *Manager) proveRecoveryAbsentContext(ctx context.Context, f *instanceManifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.PID > 0 {
		identity, err := inspectProcessContext(ctx, m.inspector, f.PID)
		if err == nil && (f.ProcessStart == 0 || (identity.StartTicks == f.ProcessStart && identity.HostBootID == f.HostBootID)) {
			return fmt.Errorf("recorded process is still live")
		}
		if err != nil && !errors.Is(err, errProcessNotFound) {
			return err
		}
	}
	if f.Launcher == "systemd" {
		_, inactive, err := m.inspectRecoveryUnitContext(ctx, f)
		if err != nil {
			return err
		}
		if !inactive {
			return fmt.Errorf("systemd unit may still own the VM")
		}
	}
	if err := m.noClaimingUnitContext(ctx, f); err != nil {
		return err
	}
	return m.noLiveClaimantContext(ctx, f)
}

// All recovery deletion paths use the same unit and process-claimant proof.
func (m *Manager) removeRecoveryStateContext(ctx context.Context, f *instanceManifest) error {
	if err := verifyRecoveryDirectories(f.VMDir); err != nil {
		return err
	}
	if err := m.proveRecoveryAbsentContext(ctx, f); err != nil {
		return err
	}
	return m.removeProvenRecoveryState(ctx, f)
}

func (m *Manager) removeProvenRecoveryState(ctx context.Context, f *instanceManifest) error {
	if err := verifyRecoveryManifestPaths(f.Service, filepath.Join(m.stateDir, "vms", f.Service), f); err != nil {
		return err
	}
	if err := verifyRecoveryDirectories(f.VMDir); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.persistRecoveryTeardown(f); err != nil {
		return fmt.Errorf("persist recovery cleanup intent: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.markRemovedDir(f.Service)
	if err := m.markIntent(f.Service); err != nil {
		return err
	}
	if err := os.RemoveAll(f.VMDir); err != nil {
		return err
	}
	m.clearIntent(f.Service)
	return nil
}

func (m *Manager) HasPendingRecovery() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, i := range m.instances {
		if i.State == StateRecoveryPending {
			return true
		}
	}
	return false
}
