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
func (m *Manager) inspectRecoveryUnit(f *instanceManifest) (int, bool, error) {
	if m.recoveryUnitInspector != nil {
		return m.recoveryUnitInspector(f)
	}
	if f.LauncherUnit != "firework-vm-"+f.InstanceID+".service" {
		return 0, false, fmt.Errorf("invalid systemd ownership unit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "show", f.LauncherUnit, "--property=LoadState,ActiveState,MainPID").Output()
	if err != nil {
		return 0, false, fmt.Errorf("inspect systemd ownership: %w", err)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			fields[k] = v
		}
	}
	pid, err := strconv.Atoi(fields["MainPID"])
	if err != nil {
		return 0, false, fmt.Errorf("invalid systemd MainPID")
	}
	absent := fields["LoadState"] == "not-found" || fields["ActiveState"] == "inactive" || fields["ActiveState"] == "failed"
	return pid, absent && pid == 0, nil
}

func (m *Manager) proveRecoveryAbsent(f *instanceManifest) error {
	if f.PID > 0 {
		identity, err := m.inspector.Inspect(f.PID)
		if err == nil && (f.ProcessStart == 0 || (identity.StartTicks == f.ProcessStart && identity.HostBootID == f.HostBootID)) {
			return fmt.Errorf("recorded process is still live")
		}
		if err != nil && !errors.Is(err, errProcessNotFound) {
			return err
		}
	}
	if f.Launcher == "systemd" {
		_, inactive, err := m.inspectRecoveryUnit(f)
		if err != nil {
			return err
		}
		if !inactive {
			return fmt.Errorf("systemd unit may still own the VM")
		}
	}
	return m.noLiveClaimant(f)
}

// All recovery deletion paths use the same unit and process-claimant proof.
func (m *Manager) removeRecoveryState(f *instanceManifest) error {
	if err := verifyRecoveryManifestPaths(f.Service, filepath.Join(m.stateDir, "vms", f.Service), f); err != nil {
		return err
	}
	return m.removeRecoveryStateContext(context.Background(), f)
}

func (m *Manager) removeRecoveryStateContext(ctx context.Context, f *instanceManifest) error {
	if err := verifyRecoveryDirectories(f.VMDir); err != nil {
		return err
	}
	if err := m.proveRecoveryAbsent(f); err != nil {
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
