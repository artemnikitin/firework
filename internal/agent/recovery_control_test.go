package agent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/vm"
)

func TestRecoveryControlRoundTripUsesPrivateSocketAndCleansUp(t *testing.T) {
	stateDir := recoveryControlTestStateDir(t)
	server := startTestRecoveryControlServer(t, stateDir, func(service string) (vm.RecoveryResult, error) {
		return vm.RecoveryResult{Service: service, Outcome: vm.RecoveryOutcomeNoop}, nil
	})
	statePath, controlDir, socketPath, lockPath, err := recoveryControlPaths(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareRecoveryControlDirectory(statePath, controlDir, false, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("recovery socket has unsafe mode or type: %v", info.Mode())
	}
	if lockInfo, err := os.Lstat(lockPath); err != nil || lockInfo.Mode().Perm() != 0o600 {
		t.Fatalf("recovery lock is not private: info=%v err=%v", lockInfo, err)
	}

	result, err := ClearRecovery(context.Background(), stateDir, "missing")
	if err != nil || result.Service != "missing" || result.Outcome != vm.RecoveryOutcomeNoop {
		t.Fatalf("unexpected local recovery response: result=%#v err=%v", result, err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shutdown left the recovery socket behind: %v", err)
	}
	if _, err := ClearRecovery(context.Background(), stateDir, "missing"); err == nil {
		t.Fatal("command connected after the agent stopped")
	}
	if _, err := os.Stat(filepath.Join(statePath, "control", ".recovery.lock")); err != nil {
		t.Fatalf("persistent lock anchor is missing: %v", err)
	}
}

func TestRecoveryControlReplacesOnlyAStaleOwnedSocket(t *testing.T) {
	stateDir := recoveryControlTestStateDir(t)
	statePath, controlDir, socketPath, _, err := recoveryControlPaths(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareRecoveryControlDirectory(statePath, controlDir, true, false); err != nil {
		t.Fatal(err)
	}
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := os.Chmod(socketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	server := startTestRecoveryControlServer(t, stateDir, func(service string) (vm.RecoveryResult, error) {
		return vm.RecoveryResult{Service: service, Outcome: vm.RecoveryOutcomeNoop}, nil
	})
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryControlDoesNotReplaceAnActiveSocket(t *testing.T) {
	stateDir := recoveryControlTestStateDir(t)
	server := startTestRecoveryControlServer(t, stateDir, func(service string) (vm.RecoveryResult, error) {
		return vm.RecoveryResult{Service: service, Outcome: vm.RecoveryOutcomeNoop}, nil
	})
	before, err := os.Lstat(server.path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := startRecoveryControlServer(stateDir, testLogger(), func(string) (vm.RecoveryResult, error) {
		return vm.RecoveryResult{}, nil
	}); err == nil {
		_ = second.Close()
		t.Fatal("a second agent acquired the active recovery socket")
	}
	after, err := os.Lstat(server.path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("active recovery socket changed: before=%v after=%v err=%v", before, after, err)
	}
}

func TestRecoveryControlRejectsMalformedAndUnsafeRequestsBeforeResolution(t *testing.T) {
	stateDir := recoveryControlTestStateDir(t)
	var calls atomic.Int32
	server := startTestRecoveryControlServer(t, stateDir, func(service string) (vm.RecoveryResult, error) {
		calls.Add(1)
		return vm.RecoveryResult{Service: service, Outcome: vm.RecoveryOutcomeNoop}, nil
	})
	connection, err := net.Dial("unix", server.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRecoveryControlFrame(connection, []byte("{")); err != nil {
		t.Fatal(err)
	}
	malformedBody, err := readRecoveryControlFrame(connection)
	_ = connection.Close()
	if err != nil {
		t.Fatal(err)
	}
	malformedResponse, err := decodeRecoveryControlResponse(malformedBody)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(malformedResponse.Error, "decode recovery request") {
		t.Fatalf("malformed JSON was not rejected: %#v", malformedResponse)
	}

	connection, err = net.Dial("unix", server.path)
	if err != nil {
		t.Fatal(err)
	}
	var oversizedHeader [4]byte
	binary.BigEndian.PutUint32(oversizedHeader[:], recoveryControlMaxFrameBytes+1)
	if _, err := connection.Write(oversizedHeader[:]); err != nil {
		t.Fatal(err)
	}
	oversizedBody, err := readRecoveryControlFrame(connection)
	_ = connection.Close()
	if err != nil {
		t.Fatal(err)
	}
	oversizedResponse, err := decodeRecoveryControlResponse(oversizedBody)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(oversizedResponse.Error, "outside the allowed range") {
		t.Fatalf("oversized request was not rejected: %#v", oversizedResponse)
	}

	connection, err = net.Dial("unix", server.path)
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(recoveryControlRequest{Version: recoveryControlProtocolVersion, Service: "../other"})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRecoveryControlFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	responseBody, err := readRecoveryControlFrame(connection)
	_ = connection.Close()
	if err != nil {
		t.Fatal(err)
	}
	response, err := decodeRecoveryControlResponse(responseBody)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Error, "invalid service name") {
		t.Fatalf("unsafe service request was not rejected: %#v", response)
	}
	if calls.Load() != 0 {
		t.Fatalf("resolver ran for an invalid request %d times", calls.Load())
	}

	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	missingStateDir := filepath.Join(stateDir, "uncreated")
	if _, err := ClearRecovery(context.Background(), missingStateDir, "../other"); err == nil {
		t.Fatal("client accepted a path-traversal service name")
	}
	if _, err := os.Stat(missingStateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid client request created a state directory: %v", err)
	}
}

func TestRecoveryControlRejectsAWorldAccessibleSocket(t *testing.T) {
	stateDir := recoveryControlTestStateDir(t)
	server := startTestRecoveryControlServer(t, stateDir, func(service string) (vm.RecoveryResult, error) {
		return vm.RecoveryResult{Service: service, Outcome: vm.RecoveryOutcomeNoop}, nil
	})
	if err := os.Chmod(server.path, 0o660); err != nil {
		t.Fatal(err)
	}
	if _, err := ClearRecovery(context.Background(), stateDir, "app"); err == nil || !strings.Contains(err.Error(), "owner and mode") {
		t.Fatalf("client accepted a broadly accessible socket: %v", err)
	}
}

func TestRecoveryControlClientHonorsTimeout(t *testing.T) {
	stateDir := recoveryControlTestStateDir(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	server := startTestRecoveryControlServer(t, stateDir, func(service string) (vm.RecoveryResult, error) {
		close(entered)
		<-release
		return vm.RecoveryResult{Service: service, Outcome: vm.RecoveryOutcomeNoop}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := ClearRecovery(ctx, stateDir, "app")
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not reach the local server")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("timed-out recovery request succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("client did not honor its request deadline")
	}
	close(release)
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryControlIdleClientDoesNotBlockRequestsOrShutdown(t *testing.T) {
	stateDir := recoveryControlTestStateDir(t)
	server := startTestRecoveryControlServer(t, stateDir, func(service string) (vm.RecoveryResult, error) {
		return vm.RecoveryResult{Service: service, Outcome: vm.RecoveryOutcomeNoop}, nil
	})
	idle, err := net.Dial("unix", server.path)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	deadline := time.Now().Add(time.Second)
	for {
		server.mu.Lock()
		active := len(server.active)
		server.mu.Unlock()
		if active != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle client was not accepted")
		}
		time.Sleep(time.Millisecond)
	}

	result, err := ClearRecovery(context.Background(), stateDir, "app")
	if err != nil || result.Service != "app" {
		t.Fatalf("idle client blocked a second request: result=%#v err=%v", result, err)
	}

	closed := make(chan error, 1)
	go func() { closed <- server.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown waited for an idle client")
	}
	_ = idle.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := idle.Read(make([]byte, 1)); err == nil {
		t.Fatal("shutdown left the idle client connection open")
	}
}

func TestRecoveryControlBoundsConcurrentRequests(t *testing.T) {
	stateDir := recoveryControlTestStateDir(t)
	server := startTestRecoveryControlServer(t, stateDir, func(service string) (vm.RecoveryResult, error) {
		return vm.RecoveryResult{Service: service, Outcome: vm.RecoveryOutcomeNoop}, nil
	})
	connections := make([]net.Conn, 0, recoveryControlMaxHandlers)
	defer func() {
		for _, connection := range connections {
			_ = connection.Close()
		}
	}()
	for range recoveryControlMaxHandlers {
		connection, err := net.Dial("unix", server.path)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, connection)
	}
	deadline := time.Now().Add(time.Second)
	for {
		server.mu.Lock()
		active := len(server.active)
		server.mu.Unlock()
		if active == recoveryControlMaxHandlers {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("accepted %d concurrent requests; want %d", active, recoveryControlMaxHandlers)
		}
		time.Sleep(time.Millisecond)
	}

	_, err := ClearRecovery(context.Background(), stateDir, "overflow")
	if err == nil || !strings.Contains(err.Error(), "maximum number of requests") {
		t.Fatalf("server did not reject an excess request: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryControlRetriesTransientAcceptFailuresWithCancellableBackoff(t *testing.T) {
	server := &recoveryControlServer{
		logger: testLogger(), closing: make(chan struct{}), done: make(chan struct{}),
	}
	accepted := make(chan time.Time, 4)
	go func() {
		defer close(server.done)
		server.serveAccepting(func() (*net.UnixConn, error) {
			select {
			case accepted <- time.Now():
			default:
			}
			return nil, syscall.EMFILE
		})
	}()
	var first, second time.Time
	select {
	case first = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("accept loop did not make its initial attempt")
	}
	select {
	case second = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("accept loop did not retry a transient resource failure")
	}
	if second.Sub(first) < recoveryAcceptRetryInitial {
		t.Fatalf("accept retry did not back off: first=%v second=%v", first, second)
	}
	close(server.closing)
	select {
	case <-server.done:
	case <-time.After(time.Second):
		t.Fatal("accept retry backoff did not stop after shutdown")
	}
}

func TestAgentContinuesWhenRecoveryControlSetupIsUnavailable(t *testing.T) {
	target := t.TempDir()
	linkParent := t.TempDir()
	stateDir := filepath.Join(linkParent, "state")
	if err := os.Symlink(target, stateDir); err != nil {
		t.Fatal(err)
	}
	cfg := testAgentConfig(t)
	cfg.StateDir = stateDir
	store := &fakeStore{
		data:     map[string][]byte{"web": []byte("node: web\nservices: []\n")},
		revision: "rev-1",
	}
	a := New(cfg, store, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	select {
	case err := <-done:
		cancel()
		t.Fatalf("agent stopped when optional recovery control setup failed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("agent did not complete normal shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not shut down after context cancellation")
	}
	if store.fetchCount == 0 {
		t.Fatal("agent did not run normal reconciliation after control setup failed")
	}
}

func TestRecoveryReconcileQueueCoalescesServicesAndKeepsAdoption(t *testing.T) {
	a := &Agent{recoveryWake: make(chan struct{}, 1)}
	a.queueRecoveryReconcile("app", false)
	a.queueRecoveryReconcile("app", true)
	a.queueRecoveryReconcile("app", false)
	a.queueRecoveryReconcile("worker", false)
	if got := len(a.recoveryWake); got != 1 {
		t.Fatalf("recovery wake signals were not coalesced: %d", got)
	}
	requests := a.takeRecoveryRequests()
	if len(requests) != 2 || !requests["app"] || requests["worker"] {
		t.Fatalf("queued recovery requests lost service or adoption state: %#v", requests)
	}
	if len(a.takeRecoveryRequests()) != 0 {
		t.Fatal("taking recovery requests did not clear the shared queue")
	}
}

func TestRecoveryControlRejectsInsecureDirectoryAndSymlink(t *testing.T) {
	t.Run("writable by other users", func(t *testing.T) {
		stateDir := recoveryControlTestStateDir(t)
		if err := os.Chmod(stateDir, 0o777); err != nil {
			t.Fatal(err)
		}
		if _, err := startRecoveryControlServer(stateDir, testLogger(), func(string) (vm.RecoveryResult, error) {
			return vm.RecoveryResult{}, nil
		}); err == nil {
			t.Fatal("server started in a state directory writable by other users")
		}
	})

	t.Run("symlink socket path", func(t *testing.T) {
		stateDir := recoveryControlTestStateDir(t)
		statePath, controlDir, socketPath, _, err := recoveryControlPaths(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := prepareRecoveryControlDirectory(statePath, controlDir, true, false); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, socketPath); err != nil {
			t.Fatal(err)
		}
		if _, err := startRecoveryControlServer(stateDir, testLogger(), func(string) (vm.RecoveryResult, error) {
			return vm.RecoveryResult{}, nil
		}); err == nil {
			t.Fatal("server replaced a symlink at the recovery socket path")
		}
		if data, err := os.ReadFile(target); err != nil || string(data) != "keep" {
			t.Fatalf("symlink target changed: %q err=%v", data, err)
		}
	})
}

func startTestRecoveryControlServer(t *testing.T, stateDir string, resolve recoveryControlResolver) *recoveryControlServer {
	t.Helper()
	server, err := startRecoveryControlServer(stateDir, testLogger(), resolve)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server
}

func recoveryControlTestStateDir(t *testing.T) string {
	t.Helper()
	stateDir, err := os.MkdirTemp("/tmp", "firework48-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	return stateDir
}
