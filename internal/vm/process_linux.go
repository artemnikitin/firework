//go:build linux

package vm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// processInspectionSupported reports whether the host exposes the process
// identity Firework needs to prove VM ownership.
const processInspectionSupported = true

func (osProcessInspector) Inspect(pid int) (processIdentity, error) {
	procDir := filepath.Join("/proc", strconv.Itoa(pid))
	statData, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if errors.Is(err, os.ErrNotExist) {
		return processIdentity{}, errProcessNotFound
	}
	if err != nil {
		return processIdentity{}, fmt.Errorf("read process stat: %w", err)
	}
	if exited, stateErr := procStateIsExited(string(statData)); stateErr != nil {
		return processIdentity{}, stateErr
	} else if exited {
		return processIdentity{}, errProcessNotFound
	}
	startTicks, err := parseProcStartTicks(string(statData))
	if err != nil {
		return processIdentity{}, err
	}
	bootData, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return processIdentity{}, fmt.Errorf("read host boot ID: %w", err)
	}
	executable, err := os.Readlink(filepath.Join(procDir, "exe"))
	if errors.Is(err, os.ErrNotExist) {
		return processIdentity{}, errProcessNotFound
	}
	if err != nil {
		return processIdentity{}, fmt.Errorf("read process executable: %w", err)
	}
	info, err := os.Stat(filepath.Join(procDir, "exe"))
	if err != nil {
		if procExitedWithin(procDir, procExitConfirmTimeout) {
			return processIdentity{}, errProcessNotFound
		}
		return processIdentity{}, fmt.Errorf("stat process executable: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return processIdentity{}, fmt.Errorf("process executable has unsupported stat data")
	}
	cmdline, err := os.ReadFile(filepath.Join(procDir, "cmdline"))
	if err != nil {
		if procExitedWithin(procDir, procExitConfirmTimeout) {
			return processIdentity{}, errProcessNotFound
		}
		return processIdentity{}, fmt.Errorf("read process command line: %w", err)
	}
	// The kernel empties cmdline while a process tears down. Reporting a
	// truncated identity would fail every argument comparison and quarantine a
	// VM that merely exited, so confirm the exit before treating it as one.
	if len(strings.Trim(string(cmdline), "\x00")) == 0 {
		if procExitedWithin(procDir, procExitConfirmTimeout) {
			return processIdentity{}, errProcessNotFound
		}
		return processIdentity{}, fmt.Errorf("process %d has no command line", pid)
	}
	args := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
	return processIdentity{
		PID: pid, HostBootID: strings.TrimSpace(string(bootData)), StartTicks: startTicks,
		Executable: executable, ExecutableDev: uint64(stat.Dev), ExecutableIno: stat.Ino,
		CommandLine: args,
	}, nil
}

func (inspector osProcessInspector) FindByArguments(socketPath, configPath string) ([]processIdentity, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read process table: %w", err)
	}
	return findByVMArgumentsWith(entries, "/proc", socketPath, configPath, false, false,
		readProcessCommandLine, inspector.Inspect)
}

// FindByAnyArgument performs the strict claimant scan used before deleting VM
// state. Either exact path reference is enough to retain the files, and an
// unreadable live process makes the scan incomplete.
func (inspector osProcessInspector) FindByAnyArgument(socketPath, configPath string) ([]processIdentity, error) {
	mounts, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil, fmt.Errorf("cannot verify process table visibility: %w", err)
	}
	if err := verifyProcessTableVisibility(string(mounts)); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read process table: %w", err)
	}
	return findByVMArgumentsWith(entries, "/proc", socketPath, configPath, true, true,
		readProcessCommandLine, inspector.Inspect)
}

func findByVMArgumentsWith(
	entries []os.DirEntry,
	procRoot, socketPath, configPath string,
	matchEither, failOnUnreadable bool,
	readCommandLine func(string) ([]string, error),
	inspect func(int) (processIdentity, error),
) ([]processIdentity, error) {
	var matches []processIdentity
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		procDir := filepath.Join(procRoot, entry.Name())
		args, err := readCommandLine(procDir)
		if err != nil {
			if errors.Is(err, errProcessNotFound) {
				continue
			}
			if failOnUnreadable {
				return nil, fmt.Errorf("inspect process %d command line: %w", pid, err)
			}
			continue
		}
		if !matchesVMArgumentPaths(args, socketPath, configPath, matchEither) {
			continue
		}
		identity, err := inspect(pid)
		if err != nil {
			if failOnUnreadable && errors.Is(err, errProcessNotFound) {
				continue
			}
			return nil, fmt.Errorf("inspect matching process %d: %w", pid, err)
		}
		matches = append(matches, identity)
	}
	return matches, nil
}

func matchesVMArgumentPaths(args []string, socketPath, configPath string, matchEither bool) bool {
	socketMatch := hasExactArgument(args, "--api-sock", socketPath)
	configMatch := hasExactArgument(args, "--config-file", configPath)
	if matchEither {
		return socketMatch || configMatch
	}
	return socketMatch && configMatch
}

func readProcessCommandLine(procDir string) ([]string, error) {
	return readProcessCommandLineWith(procDir, os.ReadFile, os.Stat)
}

func readProcessCommandLineWith(procDir string, readFile func(string) ([]byte, error), stat func(string) (os.FileInfo, error)) ([]string, error) {
	cmdline, err := readFile(filepath.Join(procDir, "cmdline"))
	if err != nil {
		if _, statErr := stat(procDir); errors.Is(statErr, os.ErrNotExist) {
			return nil, errProcessNotFound
		} else if statErr != nil {
			return nil, fmt.Errorf("inspect process directory: %w", statErr)
		}
		return nil, fmt.Errorf("read process command line: %w", err)
	}
	return strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00"), nil
}

// procExitConfirmTimeout bounds how long a partially readable /proc entry is
// given to finish exiting. Teardown is kernel-side and cannot be delayed by the
// process itself, so a live process never stays unreadable for this long.
const procExitConfirmTimeout = time.Second

// procExitedWithin reports whether a process whose /proc entry became partially
// unreadable has gone away. The kernel releases the address space, and with it
// `exe` and `cmdline`, before the task reaches the zombie state, so a single
// state read can still show a running task. Only a missing /proc entry or a
// dead state counts as an exit, which keeps a live process — including one
// whose PID was recycled during the wait — from ever being reported as gone.
func procExitedWithin(procDir string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		statData, err := os.ReadFile(filepath.Join(procDir, "stat"))
		if errors.Is(err, os.ErrNotExist) {
			return true
		}
		if err == nil {
			if exited, parseErr := procStateIsExited(string(statData)); parseErr == nil && exited {
				return true
			}
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func procStateIsExited(stat string) (bool, error) {
	state, err := parseProcState(stat)
	if err != nil {
		return false, err
	}
	return state == 'Z' || state == 'X' || state == 'x', nil
}

// procStatSuffix returns the part of /proc/<pid>/stat that follows the comm
// field, which may itself contain spaces and parentheses. It starts at the
// state character, which is field 3.
func procStatSuffix(stat string) (string, error) {
	closing := strings.LastIndex(stat, ")")
	if closing < 0 || closing+2 >= len(stat) {
		return "", fmt.Errorf("malformed process stat")
	}
	return stat[closing+2:], nil
}

func parseProcState(stat string) (byte, error) {
	suffix, err := procStatSuffix(stat)
	if err != nil {
		return 0, err
	}
	return suffix[0], nil
}

func parseProcStartTicks(stat string) (uint64, error) {
	suffix, err := procStatSuffix(stat)
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(suffix)
	// starttime is field 22, so index 19 within the suffix.
	if len(fields) <= 19 {
		return 0, fmt.Errorf("process stat has %d fields after comm", len(fields))
	}
	startTicks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse process start time: %w", err)
	}
	return startTicks, nil
}

func verifyProcessTableVisibility(mounts string) error {
	for _, line := range strings.Split(mounts, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] != "/proc" {
			continue
		}
		for _, option := range strings.Split(fields[3], ",") {
			if strings.HasPrefix(option, "hidepid=") && option != "hidepid=0" && option != "hidepid=off" {
				return fmt.Errorf("restricted process table cannot prove absence: %s", option)
			}
		}
	}
	return nil
}
