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
	for _, block := range strings.Split(string(out), "\n\n") {
		fields := parseUnitProperties(block)
		if fields["Id"] == "" || !unitArgvClaims(fields["ExecStart"], f.SocketPath, f.ConfigPath) {
			continue
		}
		inactive := fields["ActiveState"] == "inactive" || fields["ActiveState"] == "failed"
		if inactive && (fields["MainPID"] == "" || fields["MainPID"] == "0") && fields["Job"] == "" {
			continue
		}
		return fmt.Errorf("systemd unit %s may still run this service's VM", fields["Id"])
	}
	return nil
}

// unitArgvClaims reports whether a systemctl ExecStart value launches with
// either VM path, e.g. "{ path=... ; argv[]=fc --api-sock /p ... ; ... }".
func unitArgvClaims(execStart, socketPath, configPath string) bool {
	_, argv, found := strings.Cut(execStart, "argv[]=")
	if !found {
		return false
	}
	argv, _, _ = strings.Cut(argv, " ;")
	args := strings.Fields(argv)
	return hasExactArgument(args, "--api-sock", socketPath) || hasExactArgument(args, "--config-file", configPath)
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
	return os.RemoveAll(f.VMDir)
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
