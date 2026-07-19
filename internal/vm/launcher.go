package vm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type launchSpec struct {
	InstanceID string
	SocketPath string
	ConfigPath string
	LogPath    string
}

type launchedProcess struct {
	PID      int
	Launcher string
	Unit     string
	Cmd      *exec.Cmd
	LogFile  *os.File
}

type processLauncher interface {
	Launch(context.Context, launchSpec) (*launchedProcess, error)
	Stop(*instanceManifest, syscall.Signal) error
}

type directLauncher struct {
	binary string
}

func (l *directLauncher) Launch(ctx context.Context, spec launchSpec) (*launchedProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open firecracker log: %w", err)
	}
	cmd := exec.Command(l.binary,
		"--id", spec.InstanceID,
		"--api-sock", spec.SocketPath,
		"--config-file", spec.ConfigPath,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("start firecracker: %w", err)
	}
	return &launchedProcess{PID: cmd.Process.Pid, Launcher: "direct", Cmd: cmd, LogFile: logFile}, nil
}

func (l *directLauncher) Stop(manifest *instanceManifest, signal syscall.Signal) error {
	process, err := os.FindProcess(manifest.PID)
	if err != nil {
		return err
	}
	return process.Signal(signal)
}

type systemdLauncher struct {
	binary       string
	systemdRun   string
	systemctl    string
	pollInterval time.Duration
}

func (l *systemdLauncher) Launch(ctx context.Context, spec launchSpec) (*launchedProcess, error) {
	unit := "firework-vm-" + spec.InstanceID + ".service"
	args := []string{
		"--unit=" + unit,
		"--slice=firework-vms.slice",
		"--service-type=exec",
		"--collect",
		"--no-block",
		"--property=Restart=no",
		"--property=KillMode=mixed",
		"--property=StandardOutput=append:" + spec.LogPath,
		"--property=StandardError=append:" + spec.LogPath,
		"--",
		l.binary,
		"--id", spec.InstanceID,
		"--api-sock", spec.SocketPath,
		"--config-file", spec.ConfigPath,
	}
	if output, err := exec.CommandContext(ctx, l.systemdRun, args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("start transient unit: %w: %s", err, strings.TrimSpace(string(output)))
	}
	interval := l.pollInterval
	if interval <= 0 {
		interval = 50 * time.Millisecond
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		output, err := exec.CommandContext(ctx, l.systemctl, "show", "--property=MainPID", "--value", unit).Output()
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(output)))
			if parseErr == nil && pid > 0 {
				return &launchedProcess{PID: pid, Launcher: "systemd", Unit: unit}, nil
			}
		}
		time.Sleep(interval)
	}
	return nil, fmt.Errorf("transient unit %s did not report a main PID", unit)
}

func (l *systemdLauncher) Stop(manifest *instanceManifest, signal syscall.Signal) error {
	if manifest.LauncherUnit == "" {
		return fmt.Errorf("manifest has no systemd unit")
	}
	signalName := "TERM"
	if signal == syscall.SIGKILL {
		signalName = "KILL"
	}
	output, err := exec.Command(l.systemctl, "kill", "--kill-who=main", "--signal="+signalName, manifest.LauncherUnit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl kill: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func chooseLauncher(binary string) processLauncher {
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/run/systemd/system"); err == nil {
			systemdRun, runErr := exec.LookPath("systemd-run")
			systemctl, ctlErr := exec.LookPath("systemctl")
			if runErr == nil && ctlErr == nil {
				return &systemdLauncher{binary: binary, systemdRun: systemdRun, systemctl: systemctl}
			}
		}
	}
	return &directLauncher{binary: binary}
}

func launcherForManifest(binary string, manifest *instanceManifest) processLauncher {
	if manifest.Launcher == "systemd" {
		return &systemdLauncher{binary: binary, systemdRun: "systemd-run", systemctl: "systemctl"}
	}
	return &directLauncher{binary: binary}
}

func startingLauncherMetadata(launcher processLauncher, instanceID string) (string, string) {
	if _, ok := launcher.(*systemdLauncher); ok {
		return "systemd", "firework-vm-" + instanceID + ".service"
	}
	return "direct", ""
}

func systemdMainPID(systemctl, unit string) (int, error) {
	output, err := exec.Command(systemctl, "show", "--property=MainPID", "--value", unit).Output()
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("unit %s has no main PID", unit)
	}
	return pid, nil
}
