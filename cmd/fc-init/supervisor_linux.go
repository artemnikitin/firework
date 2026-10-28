//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/artemnikitin/firework/internal/guestlifecycle"
)

type lifecycleCall struct {
	request  guestlifecycle.Request
	response chan guestlifecycle.Response
}

type childSupervisor struct {
	pid              int
	exited           bool
	status           syscall.WaitStatus
	pgid             int
	children         bool
	quiesceRequested bool
}

type guestFilesystem interface {
	Sync()
	Unmount(string) error
	Mounted(string) (bool, error)
}

type osGuestFilesystem struct{}

func (osGuestFilesystem) Sync() {
	syscall.Sync()
}

func (osGuestFilesystem) Unmount(path string) error {
	return syscall.Unmount(path, 0)
}

func (osGuestFilesystem) Mounted(path string) (bool, error) {
	return guestPathMounted(path)
}

func prepareChildSupervisor() error {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("enable orphan reaping: %w", err)
	}
	return nil
}

func superviseChild(pid int, calls <-chan lifecycleCall, signals <-chan os.Signal, volumes []guestVolume, filesystem guestFilesystem) int {
	child := childSupervisor{pid: pid, pgid: pid}
	for {
		if child.exited && !child.quiesceRequested {
			child.reapOrphans(signals)
			return applicationExitCode(child.status)
		}
		select {
		case sig := <-signals:
			if sig == syscall.SIGCHLD {
				if _, err := child.reap(); err != nil {
					fmt.Fprintf(os.Stderr, "fc-init: reap child processes: %v\n", err)
				}
				continue
			}
			if err := child.forwardSignal(sig); err != nil && err != syscall.ESRCH {
				fmt.Fprintf(os.Stderr, "fc-init: forward signal %s: %v\n", sig, err)
			}
		case call := <-calls:
			child.quiesceRequested = true
			response := child.quiesce(call.request, signals, volumes, filesystem)
			call.response <- response
		}
	}
}

func (c *childSupervisor) reap() (bool, error) {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if pid > 0 {
			if pid == c.pid {
				c.exited = true
				c.status = status
			}
			continue
		}
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.ECHILD {
			c.children = false
			return false, nil
		}
		if err != nil {
			return false, err
		}
		c.children = true
		return true, nil
	}
}

func (c *childSupervisor) forwardSignal(sig os.Signal) error {
	signal, ok := sig.(syscall.Signal)
	if !ok {
		return fmt.Errorf("unsupported signal type %T", sig)
	}
	if c.exited {
		if !c.children {
			return syscall.ESRCH
		}
		return syscall.Kill(-c.pgid, signal)
	}
	if err := syscall.Kill(-c.pgid, signal); err == nil {
		return nil
	} else if err != syscall.ESRCH {
		return err
	}
	return syscall.Kill(c.pid, signal)
}

func (c *childSupervisor) reapOrphans(signals <-chan os.Signal) {
	if !c.children {
		return
	}
	if err := syscall.Kill(-c.pgid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		fmt.Fprintf(os.Stderr, "fc-init: stop orphaned application children: %v\n", err)
	}
	if c.waitForNoChildren(signals, time.Second) {
		return
	}
	for c.children {
		if err := syscall.Kill(-c.pgid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			fmt.Fprintf(os.Stderr, "fc-init: kill orphaned application children: %v\n", err)
		}
		if c.waitForNoChildren(signals, 2*time.Second) {
			return
		}
		fmt.Fprintln(os.Stderr, "fc-init: orphaned application children did not exit after SIGKILL; continuing to reap")
	}
}

func (c *childSupervisor) waitForNoChildren(signals <-chan os.Signal, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for c.children {
		select {
		case sig := <-signals:
			if sig == syscall.SIGCHLD {
				if _, err := c.reap(); err != nil {
					fmt.Fprintf(os.Stderr, "fc-init: reap orphaned application children: %v\n", err)
					return false
				}
				continue
			}
			if err := c.forwardSignal(sig); err != nil && err != syscall.ESRCH {
				fmt.Fprintf(os.Stderr, "fc-init: forward signal %s to orphaned application children: %v\n", sig, err)
			}
		case <-deadline.C:
			return false
		}
	}
	return true
}

func (c *childSupervisor) quiesce(request guestlifecycle.Request, signals <-chan os.Signal, volumes []guestVolume, filesystem guestFilesystem) guestlifecycle.Response {
	response := guestlifecycle.Response{
		Version: guestlifecycle.Version, Operation: guestlifecycle.QuiesceOperation,
		Mounts: make([]guestlifecycle.MountResult, 0, len(volumes)),
	}
	if _, err := c.reap(); err != nil {
		response.ErrorCode = "reap_failed"
		return response
	}
	if !c.exited || c.children {
		if err := c.forwardSignal(syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			fmt.Fprintf(os.Stderr, "fc-init: send SIGTERM during quiesce: %v\n", err)
		}
	}
	deadline := time.Now().Add(time.Duration(request.TimeoutMS) * time.Millisecond)
	for !c.exited || c.children {
		if _, err := c.reap(); err != nil {
			response.ErrorCode = "reap_failed"
			return response
		}
		if c.exited && !c.children {
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			response.AppExited = c.exited
			if c.exited {
				response.AppExitCode = applicationExitCode(c.status)
			}
			response.ErrorCode = "application_timeout"
			return response
		}
		timer := time.NewTimer(remaining)
		select {
		case sig := <-signals:
			timer.Stop()
			if sig == syscall.SIGCHLD {
				continue
			}
			if err := c.forwardSignal(sig); err != nil && err != syscall.ESRCH {
				fmt.Fprintf(os.Stderr, "fc-init: forward signal %s during quiesce: %v\n", sig, err)
			}
		case <-timer.C:
			response.AppExited = c.exited
			if c.exited {
				response.AppExitCode = applicationExitCode(c.status)
			}
			response.ErrorCode = "application_timeout"
			return response
		}
	}
	response.AppExited = true
	response.AppExitCode = applicationExitCode(c.status)
	filesystem.Sync()
	response.SyncDone = true
	response.Mounts = unmountPersistentVolumes(volumes, filesystem)
	return response
}

func unmountPersistentVolumes(volumes []guestVolume, filesystem guestFilesystem) []guestlifecycle.MountResult {
	results := make([]guestlifecycle.MountResult, 0, len(volumes))
	for i := len(volumes) - 1; i >= 0; i-- {
		volume := volumes[i]
		result := guestlifecycle.MountResult{Name: volume.Name, Path: volume.MountPath}
		mounted, err := filesystem.Mounted(volume.MountPath)
		if err != nil {
			result.Error = boundedLifecycleError(err)
		} else if !mounted {
			result.Unmounted = true
		} else if err := filesystem.Unmount(volume.MountPath); err != nil {
			result.Error = boundedLifecycleError(err)
		} else {
			result.Unmounted = true
		}
		results = append(results, result)
	}
	return results
}

func applicationExitCode(status syscall.WaitStatus) int {
	if status.Exited() {
		return status.ExitStatus()
	}
	if status.Signaled() {
		return 128 + int(status.Signal())
	}
	return 1
}

func boundedLifecycleError(err error) string {
	message := err.Error()
	if len(message) <= guestlifecycle.MaxErrorBytes {
		return message
	}
	var bounded []byte
	for _, r := range message {
		bytes := []byte(string(r))
		if len(bounded)+len(bytes) > guestlifecycle.MaxErrorBytes {
			break
		}
		bounded = append(bounded, bytes...)
	}
	return string(bounded)
}
