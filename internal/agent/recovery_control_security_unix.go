//go:build darwin || linux

package agent

import (
	"fmt"
	"os"
	"syscall"
)

func recoveryControlOwned(info os.FileInfo, allowRoot bool) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(stat.Uid) == os.Geteuid() || (allowRoot && os.Geteuid() == 0)
}

func recoveryControlSameOwner(first, second os.FileInfo) bool {
	firstStat, firstOK := first.Sys().(*syscall.Stat_t)
	secondStat, secondOK := second.Sys().(*syscall.Stat_t)
	return firstOK && secondOK && firstStat.Uid == secondStat.Uid
}

func acquireRecoveryControlLock(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() || !recoveryControlOwned(info, false) {
		_ = file.Close()
		return nil, fmt.Errorf("lock file is not an agent-owned regular file")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("another agent holds the recovery control lock: %w", err)
	}
	return file, nil
}
