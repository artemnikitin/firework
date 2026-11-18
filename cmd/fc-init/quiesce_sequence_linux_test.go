//go:build linux

package main

import (
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/guestlifecycle"
)

type fakeApplicationProcesses struct {
	wait     func(*syscall.WaitStatus) (int, error)
	signal   func(int, syscall.Signal) error
	children []int
}

func (f *fakeApplicationProcesses) Wait(status *syscall.WaitStatus) (int, error) {
	return f.wait(status)
}
func (f *fakeApplicationProcesses) Signal(pid int, signal syscall.Signal) error {
	if f.signal != nil {
		return f.signal(pid, signal)
	}
	return nil
}
func (f *fakeApplicationProcesses) Children() []int { return f.children }

func quiesceRequest(app, flush time.Duration) guestlifecycle.Request {
	return guestlifecycle.Request{Version: guestlifecycle.Version, Operation: guestlifecycle.QuiesceOperation,
		ApplicationTimeoutMS: uint32(app / time.Millisecond), FlushTimeoutMS: uint32(flush / time.Millisecond)}
}

func exitedSupervisor() childSupervisor {
	return childSupervisor{pid: 42, exited: true, processOps: &fakeApplicationProcesses{
		wait: func(*syscall.WaitStatus) (int, error) { return 0, syscall.ECHILD },
	}}
}

func TestQuiesceTimeoutThenLateApplicationExitRetries(t *testing.T) {
	exit, reaped := false, false
	var signaled []int
	processes := &fakeApplicationProcesses{
		wait: func(status *syscall.WaitStatus) (int, error) {
			if !exit {
				return 0, nil
			}
			if !reaped {
				reaped = true
				*status = syscall.WaitStatus(9 << 8)
				return 42, nil
			}
			return 0, syscall.ECHILD
		},
		signal: func(pid int, _ syscall.Signal) error { signaled = append(signaled, pid); return nil },
	}
	child := childSupervisor{pid: 42, processOps: processes}
	filesystem := &fakeGuestFilesystem{mounted: map[string]bool{"/data": true}}
	volumes := []guestVolume{{Name: "data", MountPath: "/data"}}
	request := quiesceRequest(10*time.Millisecond, time.Second)
	first := child.quiesce(request, nil, volumes, filesystem, time.Time{})
	if first.ErrorCode != "application_timeout" || first.AppExited || filesystem.synced || len(filesystem.unmountOrder) != 0 {
		t.Fatalf("timeout changed live filesystem: response=%#v filesystem=%#v", first, filesystem)
	}
	if !reflect.DeepEqual(signaled, []int{42}) {
		t.Fatalf("duplicate initial signal: %v", signaled)
	}
	exit = true
	second := child.quiesce(request, nil, volumes, filesystem, time.Time{})
	if !second.Successful() || second.AppExitCode != 9 || !reflect.DeepEqual(filesystem.unmountOrder, []string{"/data"}) {
		t.Fatalf("late exit did not converge: %#v mounts=%v", second, filesystem.unmountOrder)
	}
}

func TestQuiesceSignalsNewlyAdoptedChildren(t *testing.T) {
	stage := 0
	var signaled []int
	processes := &fakeApplicationProcesses{children: []int{101}}
	processes.wait = func(status *syscall.WaitStatus) (int, error) {
		switch stage {
		case 1:
			stage = 2
			*status = 0
			return 42, nil
		case 3:
			stage = 4
			return 101, nil
		case 4:
			return 0, syscall.ECHILD
		default:
			return 0, nil
		}
	}
	processes.signal = func(pid int, signal syscall.Signal) error {
		if signal != syscall.SIGTERM {
			t.Errorf("unexpected signal %v", signal)
		}
		signaled = append(signaled, pid)
		if pid == 42 {
			stage = 1
		} else if pid == 101 {
			stage = 3
		}
		return nil
	}
	child := childSupervisor{pid: 42, processOps: processes}
	response := child.quiesce(quiesceRequest(time.Second, time.Second), nil, nil, &fakeGuestFilesystem{}, time.Time{})
	if !response.Successful() || !reflect.DeepEqual(signaled, []int{42, 101}) {
		t.Fatalf("orphan shutdown response=%#v signals=%v", response, signaled)
	}
}

func TestQuiescePartialUnmountThenRetry(t *testing.T) {
	child := exitedSupervisor()
	filesystem := &fakeGuestFilesystem{mounted: map[string]bool{"/data": true, "/cache": true}, unmountErrors: map[string]error{"/data": syscall.EBUSY}}
	volumes := []guestVolume{{Name: "data", MountPath: "/data"}, {Name: "cache", MountPath: "/cache"}}
	request := quiesceRequest(time.Second, time.Second)
	first := child.quiesce(request, nil, volumes, filesystem, time.Time{})
	if first.Successful() || !first.AppExited || !first.SyncDone || !first.Mounts[0].Unmounted {
		t.Fatalf("partial outcome=%#v", first)
	}
	delete(filesystem.unmountErrors, "/data")
	second := child.quiesce(request, nil, volumes, filesystem, time.Time{})
	if !second.Successful() || !reflect.DeepEqual(filesystem.unmountOrder, []string{"/cache", "/data", "/data"}) {
		t.Fatalf("partial retry response=%#v order=%v", second, filesystem.unmountOrder)
	}
}

type blockedFlushFilesystem struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (f *blockedFlushFilesystem) Sync() {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	select {
	case f.entered <- struct{}{}:
	default:
	}
	<-f.release
}
func (*blockedFlushFilesystem) Mounted(string) (bool, error) { return false, nil }
func (*blockedFlushFilesystem) Unmount(string) error         { return nil }
func (f *blockedFlushFilesystem) syncCalls() int             { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func TestQuiesceFlushTimeoutSerializesRetryWork(t *testing.T) {
	child := exitedSupervisor()
	filesystem := &blockedFlushFilesystem{entered: make(chan struct{}, 3), release: make(chan struct{})}
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(filesystem.release) }) })
	first := child.quiesce(quiesceRequest(time.Second, 10*time.Millisecond), nil, nil, filesystem, time.Time{})
	if first.ErrorCode != "flush_timeout" || !first.AppExited {
		t.Fatalf("timeout=%#v", first)
	}
	second := child.quiesce(quiesceRequest(time.Second, 10*time.Millisecond), nil, nil, filesystem, time.Time{})
	if second.ErrorCode != "flush_timeout" || filesystem.syncCalls() != 1 {
		t.Fatalf("parallel flush: %#v calls=%d", second, filesystem.syncCalls())
	}
	release.Do(func() { close(filesystem.release) })
	select {
	case <-child.flush.done:
	case <-time.After(time.Second):
		t.Fatal("flush did not finish")
	}
	third := child.quiesce(quiesceRequest(time.Second, time.Second), nil, nil, filesystem, time.Time{})
	if !third.Successful() || filesystem.syncCalls() != 2 {
		t.Fatalf("completed timeout poisoned retry: %#v calls=%d", third, filesystem.syncCalls())
	}
}

func TestExpiredQuiesceDoesNotSignalApplication(t *testing.T) {
	child := childSupervisor{pid: 42, processOps: &fakeApplicationProcesses{
		wait:   func(*syscall.WaitStatus) (int, error) { t.Fatal("expired request inspected app"); return 0, nil },
		signal: func(int, syscall.Signal) error { t.Fatal("expired request signaled app"); return nil },
	}}
	response := child.quiesce(quiesceRequest(time.Second, time.Second), nil, nil, &fakeGuestFilesystem{}, time.Now().Add(-time.Second))
	if response.ErrorCode != "operation_expired" {
		t.Fatalf("expired request=%#v", response)
	}
}
