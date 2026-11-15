//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/artemnikitin/firework/internal/guestlifecycle"
)

type lifecycleCall struct {
	request  guestlifecycle.Request
	response chan guestlifecycle.Response
	deadline time.Time
	canceled <-chan struct{}
}

type childSupervisor struct {
	pid              int
	exited           bool
	status           syscall.WaitStatus
	children         bool
	quiesceRequested bool
	flush            *guestFlush
	processOps       applicationProcesses
}

type applicationProcesses interface {
	Wait(*syscall.WaitStatus) (int, error)
	Signal(int, syscall.Signal) error
	Children() []int
}

type osApplicationProcesses struct{}

func (osApplicationProcesses) Wait(status *syscall.WaitStatus) (int, error) {
	return syscall.Wait4(-1, status, syscall.WNOHANG, nil)
}
func (osApplicationProcesses) Signal(pid int, signal syscall.Signal) error {
	return syscall.Kill(pid, signal)
}
func (osApplicationProcesses) Children() []int { return directChildPIDs(os.Getpid()) }

func (c *childSupervisor) processes() applicationProcesses {
	if c.processOps != nil {
		return c.processOps
	}
	return osApplicationProcesses{}
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
	child := childSupervisor{pid: pid}
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
		case call, ok := <-calls:
			if !ok {
				_, _ = child.reap()
				child.reapOrphans(signals)
				return applicationExitCode(child.status)
			}
			if !call.deadline.IsZero() && !time.Now().Before(call.deadline) {
				call.response <- guestlifecycle.Response{Version: guestlifecycle.Version, Operation: guestlifecycle.QuiesceOperation, ErrorCode: "operation_expired"}
				continue
			}
			select {
			case <-call.canceled:
				call.response <- guestlifecycle.Response{Version: guestlifecycle.Version, Operation: guestlifecycle.QuiesceOperation, ErrorCode: "operation_expired"}
				continue
			default:
			}
			child.quiesceRequested = true
			response := child.quiesce(call.request, signals, volumes, filesystem, call.deadline)
			call.response <- response
		}
	}
}

func (c *childSupervisor) reap() (bool, error) {
	for {
		var status syscall.WaitStatus
		pid, err := c.processes().Wait(&status)
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
		return c.signalApplicationChildren(signal)
	}
	return c.signalApplicationChildren(signal)
}

func (c *childSupervisor) reapOrphans(signals <-chan os.Signal) {
	if !c.children {
		return
	}
	if err := c.signalApplicationChildren(syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		fmt.Fprintf(os.Stderr, "fc-init: stop orphaned application children: %v\n", err)
	}
	if c.waitForNoChildren(signals, time.Second) {
		return
	}
	for c.children {
		if err := c.signalApplicationChildren(syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			fmt.Fprintf(os.Stderr, "fc-init: kill orphaned application children: %v\n", err)
		}
		if c.waitForNoChildren(signals, 2*time.Second) {
			return
		}
		fmt.Fprintln(os.Stderr, "fc-init: orphaned application children did not exit after SIGKILL; continuing to reap")
	}
}

func (c *childSupervisor) signalApplicationChildren(sig syscall.Signal) error {
	if !c.exited {
		return c.processes().Signal(c.pid, sig)
	}
	var firstErr error
	for _, pid := range c.processes().Children() {
		if err := c.processes().Signal(pid, sig); err != nil && err != syscall.ESRCH && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func directChildPIDs(parentPID int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	children := make([]int, 0)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || pid == os.Getpid() || !entry.IsDir() {
			continue
		}
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		closeParen := strings.LastIndexByte(string(data), ')')
		if closeParen < 0 {
			continue
		}
		fields := strings.Fields(string(data[closeParen+1:]))
		if len(fields) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err == nil && ppid == parentPID {
			children = append(children, pid)
		}
	}
	return children
}

func (c *childSupervisor) waitForNoChildren(signals <-chan os.Signal, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for c.children {
		select {
		case <-poll.C:
			if _, err := c.reap(); err != nil {
				return false
			}
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

type guestFlush struct {
	done     chan struct{}
	response guestlifecycle.Response
}

func (c *childSupervisor) quiesce(request guestlifecycle.Request, signals <-chan os.Signal, volumes []guestVolume, filesystem guestFilesystem, operationDeadline time.Time) guestlifecycle.Response {
	response := guestlifecycle.Response{Version: guestlifecycle.Version, Operation: guestlifecycle.QuiesceOperation, Mounts: make([]guestlifecycle.MountResult, 0, len(volumes))}
	if !operationDeadline.IsZero() && !time.Now().Before(operationDeadline) {
		response.ErrorCode = "operation_expired"
		return response
	}
	deadline := time.Now().Add(time.Duration(request.ApplicationTimeoutMS) * time.Millisecond)
	if !operationDeadline.IsZero() && operationDeadline.Before(deadline) {
		deadline = operationDeadline
	}
	signalled := make(map[int]bool)
	for {
		if _, err := c.reap(); err != nil {
			response.ErrorCode = "reap_failed"
			return response
		}
		if c.exited && !c.children {
			break
		}
		pids := []int{c.pid}
		if c.exited {
			pids = c.processes().Children()
		}
		for _, pid := range pids {
			if !signalled[pid] {
				if err := c.processes().Signal(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
					fmt.Fprintf(os.Stderr, "fc-init: signal application: %v\n", err)
				}
				signalled[pid] = true
			}
		}
		if !time.Now().Before(deadline) {
			response.AppExited = c.exited
			if c.exited {
				response.AppExitCode = applicationExitCode(c.status)
			}
			response.ErrorCode = "application_timeout"
			return response
		}
		timer := time.NewTimer(min(time.Until(deadline), 10*time.Millisecond))
		select {
		case sig := <-signals:
			timer.Stop()
			if sig != syscall.SIGCHLD {
				_ = c.forwardSignal(sig)
			}
		case <-timer.C:
		}
	}
	response.AppExited = true
	response.AppExitCode = applicationExitCode(c.status)
	flushDeadline := time.Now().Add(time.Duration(request.FlushTimeoutMS) * time.Millisecond)
	if !operationDeadline.IsZero() && operationDeadline.Before(flushDeadline) {
		flushDeadline = operationDeadline
	}
	// A timed-out syscall may still be running. Reuse its job until it finishes.
	if c.flush != nil {
		select {
		case <-c.flush.done:
			c.flush = nil
		default:
		}
	}
	if c.flush == nil {
		job := &guestFlush{done: make(chan struct{}), response: response}
		c.flush = job
		go func() {
			defer close(job.done)
			filesystem.Sync()
			job.response.SyncDone = true
			if !time.Now().Before(flushDeadline) {
				job.response.ErrorCode = "flush_timeout"
				return
			}
			job.response.Mounts = unmountPersistentVolumesBefore(volumes, filesystem, flushDeadline)
			if !time.Now().Before(flushDeadline) {
				job.response.ErrorCode = "flush_timeout"
			}
		}()
	}
	ctx, cancel := context.WithDeadline(context.Background(), flushDeadline)
	defer cancel()
	select {
	case <-c.flush.done:
		result := c.flush.response
		c.flush = nil
		if ctx.Err() != nil {
			result.ErrorCode = "flush_timeout"
		}
		return result
	case <-ctx.Done():
		response.ErrorCode = "flush_timeout"
		return response
	}
}

func unmountPersistentVolumesBefore(volumes []guestVolume, filesystem guestFilesystem, deadline time.Time) []guestlifecycle.MountResult {
	results := make([]guestlifecycle.MountResult, 0, len(volumes))
	for i := len(volumes) - 1; i >= 0; i-- {
		volume := volumes[i]
		result := guestlifecycle.MountResult{Name: volume.Name, Path: volume.MountPath}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			result.Error = "flush timeout"
			results = append(results, result)
			continue
		}
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
