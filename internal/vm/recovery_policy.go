package vm

import (
	"context"
	"encoding/json"
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
	out, err := run(ctx, m.recoverySystemctl(), "show", "firework-vm-*.service", "--property=Id,ActiveState,MainPID,Job")
	if err != nil {
		return fmt.Errorf("inspect systemd launches: %w", err)
	}
	for _, block := range strings.Split(string(out), "\n\n") {
		fields := parseUnitProperties(block)
		if fields["Id"] == "" {
			continue
		}
		inactive := fields["ActiveState"] == "inactive" || fields["ActiveState"] == "failed"
		if inactive && (fields["MainPID"] == "" || fields["MainPID"] == "0") && fields["Job"] == "" {
			continue
		}
		commands, err := recoveryUnitArguments(ctx, run, fields["Id"])
		if err != nil {
			return fmt.Errorf("inspect systemd unit %s arguments: %w", fields["Id"], err)
		}
		for _, args := range commands {
			if hasExactArgument(args, "--api-sock", f.SocketPath) || hasExactArgument(args, "--config-file", f.ConfigPath) {
				return fmt.Errorf("systemd unit %s may still run this service's VM", fields["Id"])
			}
		}
	}
	return nil
}

// recoveryUnitArguments reads the typed D-Bus ExecStart property. systemctl's
// human rendering joins arguments with spaces and cannot distinguish a path
// containing whitespace from several arguments. busctl JSON retains the
// original argument array, including whitespace, quotes and backslashes.
func recoveryUnitArguments(ctx context.Context, run func(context.Context, string, ...string) ([]byte, error), unit string) ([][]string, error) {
	out, err := run(ctx, "busctl", "--json=short", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "GetUnit", "s", unit)
	if err != nil {
		return nil, fmt.Errorf("get unit object: %w", err)
	}
	var property struct {
		Type string            `json:"type"`
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(out, &property); err != nil || property.Type != "o" || len(property.Data) != 1 {
		return nil, fmt.Errorf("invalid systemd unit object response")
	}
	var path string
	if err := json.Unmarshal(property.Data[0], &path); err != nil || !strings.HasPrefix(path, "/org/freedesktop/systemd1/unit/") {
		return nil, fmt.Errorf("invalid systemd unit object path")
	}
	out, err = run(ctx, "busctl", "--json=short", "get-property", "org.freedesktop.systemd1", path, "org.freedesktop.systemd1.Service", "ExecStart")
	if err != nil {
		return nil, fmt.Errorf("get ExecStart property: %w", err)
	}
	property.Data = nil
	if err := json.Unmarshal(out, &property); err != nil || property.Type != "a(sasbttttuii)" || len(property.Data) == 0 {
		return nil, fmt.Errorf("invalid systemd ExecStart response")
	}
	commands := make([][]string, 0, len(property.Data))
	for _, raw := range property.Data {
		var command []json.RawMessage
		if err := json.Unmarshal(raw, &command); err != nil || len(command) != 10 {
			return nil, fmt.Errorf("invalid systemd ExecStart command")
		}
		var args []string
		if err := json.Unmarshal(command[1], &args); err != nil || len(args) == 0 {
			return nil, fmt.Errorf("invalid systemd ExecStart argument array")
		}
		commands = append(commands, args)
	}
	return commands, nil
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
