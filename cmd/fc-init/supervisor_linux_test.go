//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/guestlifecycle"
)

type fakeGuestFilesystem struct {
	mounted       map[string]bool
	unmountErrors map[string]error
	unmountOrder  []string
	synced        bool
}

func (f *fakeGuestFilesystem) Sync() {
	f.synced = true
}

func (f *fakeGuestFilesystem) Unmount(path string) error {
	f.unmountOrder = append(f.unmountOrder, path)
	if err := f.unmountErrors[path]; err != nil {
		return err
	}
	f.mounted[path] = false
	return nil
}

func (f *fakeGuestFilesystem) Mounted(path string) (bool, error) {
	return f.mounted[path], nil
}

func TestSupervisorChildHelper(t *testing.T) {
	if os.Getenv("FC_INIT_SUPERVISOR_TEST_HELPER") != "orphan" {
		return
	}
	_, err := syscall.ForkExec("/bin/sleep", []string{"sleep", "10"}, &syscall.ProcAttr{
		Env: os.Environ(), Files: []uintptr{os.Stdin.Fd(), os.Stdout.Fd(), os.Stderr.Fd()},
		Sys: &syscall.SysProcAttr{Setsid: true},
	})
	if err != nil {
		os.Exit(98)
	}
	os.Exit(23)
}

func TestSupervisorForwardsSignalAndPreservesApplicationExitCode(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGCHLD)
	defer signal.Stop(signals)

	// This channel is the supervisor's signal input. Sending SIGTERM here tests
	// the same forwarding path used by Linux signals delivered to PID 1.
	started := startTestChild(t, "/bin/sh", []string{"sh", "-c", fmt.Sprintf("trap 'exit 42' TERM; touch %q; while :; do sleep 0.05; done", ready)}, os.Environ(), nil, nil)
	result := make(chan int, 1)
	go func() { result <- superviseChild(started.pid, nil, signals, nil, &fakeGuestFilesystem{}) }()
	waitForPath(t, ready)
	signals <- syscall.SIGTERM
	select {
	case code := <-result:
		if code != 42 {
			t.Fatalf("application exit code = %d, want 42", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not reap the signaled application")
	}
}

func TestSupervisorReapsOrphanedApplicationChildren(t *testing.T) {
	if err := prepareChildSupervisor(); err != nil {
		t.Fatal(err)
	}
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGCHLD)
	defer signal.Stop(signals)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	environment := append(os.Environ(), "FC_INIT_SUPERVISOR_TEST_HELPER=orphan")
	started := startTestChild(t, bin, []string{bin, "-test.run=^TestSupervisorChildHelper$"}, environment, nil, nil)
	result := make(chan int, 1)
	go func() { result <- superviseChild(started.pid, nil, signals, nil, &fakeGuestFilesystem{}) }()
	select {
	case code := <-result:
		if code != 23 {
			t.Fatalf("application exit code = %d, want 23", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not terminate and reap an escaped orphan")
	}
	if _, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
		t.Fatalf("orphan children remain unreaped: wait4 error = %v", err)
	}
}

func TestChildCredentialsAndEnvironmentApplyOnlyToChild(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("setting child groups requires root")
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := resolveUserSpec(current.Username)
	if err != nil {
		t.Fatal(err)
	}
	credential := &syscall.Credential{Uid: identity.uid, Gid: identity.gid, Groups: identity.groups}
	output := filepath.Join(t.TempDir(), "child.txt")
	file, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	environment := append(os.Environ(), "FC_INIT_CHILD_MARKER=preserved")
	command := fmt.Sprintf("printf '%%s\\n' \"$FC_INIT_CHILD_MARKER\" \"$(id -u)\" \"$(id -g)\" \"$(id -G)\" > %q", output)
	started := startTestChild(t, "/bin/sh", []string{"sh", "-c", command}, environment, credential, []uintptr{os.Stdin.Fd(), file.Fd(), file.Fd()})
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGCHLD)
	defer signal.Stop(signals)
	if code := superviseChild(started.pid, nil, signals, nil, &fakeGuestFilesystem{}); code != 0 {
		t.Fatalf("child exit code = %d, want 0", code)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 || lines[0] != "preserved" {
		t.Fatalf("child output does not preserve environment: %q", data)
	}
	if lines[1] != strconv.FormatUint(uint64(identity.uid), 10) || lines[2] != strconv.FormatUint(uint64(identity.gid), 10) {
		t.Fatalf("child uid/gid = %s/%s, want %d/%d", lines[1], lines[2], identity.uid, identity.gid)
	}
	groups := make(map[string]struct{}, len(identity.groups))
	for _, group := range identity.groups {
		groups[strconv.FormatUint(uint64(group), 10)] = struct{}{}
	}
	for _, group := range strings.Fields(lines[3]) {
		delete(groups, group)
	}
	if len(groups) != 0 {
		t.Fatalf("child did not receive supplementary groups %v; output %q", groups, lines[3])
	}
	if os.Geteuid() != 0 {
		t.Fatal("fc-init lost its root credentials")
	}
}

func TestQuiesceIsRepeatableAndUnmountsInReverseOrder(t *testing.T) {
	filesystem := &fakeGuestFilesystem{mounted: map[string]bool{"/data": true, "/cache": true}}
	ready := filepath.Join(t.TempDir(), "ready")
	calls := make(chan lifecycleCall)
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGCHLD)
	defer signal.Stop(signals)
	started := startTestChild(t, "/bin/sh", []string{"sh", "-c", fmt.Sprintf("trap 'exit 9' TERM; touch %q; while :; do sleep 0.05; done", ready)}, os.Environ(), nil, nil)
	go superviseChild(started.pid, calls, signals, []guestVolume{
		{Name: "data", MountPath: "/data"}, {Name: "cache", MountPath: "/cache"},
	}, filesystem)
	waitForPath(t, ready)

	first := sendQuiesce(t, calls, 2*time.Second)
	if !first.Successful() || !filesystem.synced {
		t.Fatalf("first quiesce failed: %#v, synced=%v", first, filesystem.synced)
	}
	if len(filesystem.unmountOrder) != 2 || filesystem.unmountOrder[0] != "/cache" || filesystem.unmountOrder[1] != "/data" {
		t.Fatalf("unmount order = %v, want reverse mount order", filesystem.unmountOrder)
	}
	second := sendQuiesce(t, calls, 2*time.Second)
	if !second.Successful() {
		t.Fatalf("repeated quiesce did not converge: %#v", second)
	}
	if len(filesystem.unmountOrder) != 2 {
		t.Fatalf("repeated quiesce attempted to unmount absent mounts again: %v", filesystem.unmountOrder)
	}
}

func TestQuiesceTimeoutDoesNotSyncOrUnmountLiveApplication(t *testing.T) {
	filesystem := &fakeGuestFilesystem{mounted: map[string]bool{"/data": true}}
	ready := filepath.Join(t.TempDir(), "ready")
	calls := make(chan lifecycleCall)
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGCHLD)
	defer signal.Stop(signals)
	started := startTestChild(t, "/bin/sh", []string{"sh", "-c", fmt.Sprintf("trap '' TERM; touch %q; while :; do sleep 0.05; done", ready)}, os.Environ(), nil, nil)
	go superviseChild(started.pid, calls, signals, []guestVolume{{Name: "data", MountPath: "/data"}}, filesystem)
	waitForPath(t, ready)
	response := sendQuiesce(t, calls, 100*time.Millisecond)
	if response.AppExited || response.SyncDone || len(response.Mounts) != 0 || response.ErrorCode != "application_timeout" {
		t.Fatalf("unexpected timeout response: %#v", response)
	}
	if filesystem.synced || len(filesystem.unmountOrder) != 0 || !filesystem.mounted["/data"] {
		t.Fatalf("timeout changed live volume state: %#v", filesystem)
	}
	_ = syscall.Kill(-started.pid, syscall.SIGKILL)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(started.pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed-out test application did not exit")
}

func TestQuiesceReportsEveryUnmountFailure(t *testing.T) {
	filesystem := &fakeGuestFilesystem{
		mounted:       map[string]bool{"/data": true, "/cache": true},
		unmountErrors: map[string]error{"/cache": syscall.EBUSY},
	}
	ready := filepath.Join(t.TempDir(), "ready")
	calls := make(chan lifecycleCall)
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGCHLD)
	defer signal.Stop(signals)
	started := startTestChild(t, "/bin/sh", []string{"sh", "-c", fmt.Sprintf("trap 'exit 0' TERM; touch %q; while :; do sleep 0.05; done", ready)}, os.Environ(), nil, nil)
	go superviseChild(started.pid, calls, signals, []guestVolume{
		{Name: "data", MountPath: "/data"}, {Name: "cache", MountPath: "/cache"},
	}, filesystem)
	waitForPath(t, ready)
	response := sendQuiesce(t, calls, 2*time.Second)
	if response.Successful() || len(response.Mounts) != 2 {
		t.Fatalf("unexpected unmount response: %#v", response)
	}
	if response.Mounts[0].Path != "/cache" || response.Mounts[0].Unmounted || response.Mounts[0].Error == "" {
		t.Fatalf("failed mount result missing: %#v", response.Mounts[0])
	}
	if !response.Mounts[1].Unmounted {
		t.Fatalf("successful mount result missing: %#v", response.Mounts[1])
	}
}

func startTestChild(t *testing.T, bin string, argv, environment []string, credential *syscall.Credential, files []uintptr) testChild {
	t.Helper()
	if len(files) == 0 {
		files = []uintptr{os.Stdin.Fd(), os.Stdout.Fd(), os.Stderr.Fd()}
	}
	if err := prepareChildSupervisor(); err != nil {
		t.Fatal(err)
	}
	pid, err := startApplicationChild(bin, argv, environment, files, credential)
	if err != nil {
		t.Fatal(err)
	}
	return testChild{pid: pid}
}

func waitForPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("child did not create readiness file %s", path)
}

type testChild struct {
	pid int
}

func sendQuiesce(t *testing.T, calls chan<- lifecycleCall, timeout time.Duration) guestlifecycle.Response {
	t.Helper()
	response := make(chan guestlifecycle.Response, 1)
	calls <- lifecycleCall{
		request: guestlifecycle.Request{
			Version: guestlifecycle.Version, Operation: guestlifecycle.QuiesceOperation,
			TimeoutMS: uint32(timeout / time.Millisecond),
		},
		response: response,
	}
	select {
	case result := <-response:
		return result
	case <-time.After(timeout + time.Second):
		t.Fatal("guest did not answer quiesce request")
		return guestlifecycle.Response{}
	}
}
