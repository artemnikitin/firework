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
)

func (osProcessInspector) Inspect(pid int) (processIdentity, error) {
	procDir := filepath.Join("/proc", strconv.Itoa(pid))
	statData, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if errors.Is(err, os.ErrNotExist) {
		return processIdentity{}, errProcessNotFound
	}
	if err != nil {
		return processIdentity{}, fmt.Errorf("read process stat: %w", err)
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
		return processIdentity{}, fmt.Errorf("stat process executable: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return processIdentity{}, fmt.Errorf("process executable has unsupported stat data")
	}
	cmdline, err := os.ReadFile(filepath.Join(procDir, "cmdline"))
	if err != nil {
		return processIdentity{}, fmt.Errorf("read process command line: %w", err)
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
	var matches []processIdentity
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
		if !hasExactArgument(args, "--api-sock", socketPath) || !hasExactArgument(args, "--config-file", configPath) {
			continue
		}
		identity, err := inspector.Inspect(pid)
		if err != nil {
			return nil, fmt.Errorf("inspect matching process %d: %w", pid, err)
		}
		matches = append(matches, identity)
	}
	return matches, nil
}

func parseProcStartTicks(stat string) (uint64, error) {
	closing := strings.LastIndex(stat, ")")
	if closing < 0 || closing+2 >= len(stat) {
		return 0, fmt.Errorf("malformed process stat")
	}
	fields := strings.Fields(stat[closing+2:])
	// The suffix starts at field 3 (state); starttime is field 22.
	if len(fields) <= 19 {
		return 0, fmt.Errorf("process stat has %d fields after comm", len(fields))
	}
	startTicks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse process start time: %w", err)
	}
	return startTicks, nil
}
