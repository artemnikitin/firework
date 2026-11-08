package vm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

type round3StopCompletionInspector struct {
	*round3DirectInspector
	gateMu   sync.Mutex
	oldPID   int
	calls    int
	entered  chan struct{}
	release  chan struct{}
	finalErr error
}

func (i *round3StopCompletionInspector) Inspect(pid int) (processIdentity, error) {
	i.gateMu.Lock()
	block := false
	if pid == i.oldPID {
		i.calls++
		// First inspection validates ownership before SIGTERM. Delay only the
		// next inspection, after Stop has signalled the old direct child.
		block = i.calls == 2
	}
	i.gateMu.Unlock()
	if block {
		close(i.entered)
		<-i.release
		return processIdentity{}, i.finalErr
	}
	return i.round3DirectInspector.Inspect(pid)
}

func (*round3StopCompletionInspector) FindByAnyArgument(string, string) ([]processIdentity, error) {
	// Replacement is attempted only after the real Wait monitor has reaped
	// the sole previous child. There are no additional process claimants.
	return nil, nil
}

func TestRound3StaleStopCannotChangeReplacement(t *testing.T) {
	inspectionFailure := errors.New("delayed old-process inspection failure")
	for _, finalErr := range []error{errProcessNotFound, inspectionFailure} {
		t.Run(finalErr.Error(), func(t *testing.T) {
			// Keep generated process paths short on development hosts.
			stateDir, err := os.MkdirTemp("", "fw-stop-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(stateDir)
			binary := filepath.Join(stateDir, "firecracker")
			if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			m := NewManager(binary, stateDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
			launcher := &specCapturingLauncher{inner: &directLauncher{binary: binary}}
			m.launcher = launcher
			inspector := &round3StopCompletionInspector{
				round3DirectInspector: &round3DirectInspector{launchedSpecInspector: &launchedSpecInspector{launcher: launcher, executable: binary}},
				entered:               make(chan struct{}), release: make(chan struct{}), finalErr: finalErr,
			}
			m.inspector = inspector
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(inspector.release) }) }
			defer release()
			defer func() {
				// Reap the current real child before removing its state directory.
				release()
				for _, instance := range m.List() {
					if instance.PID > 0 {
						_ = syscall.Kill(instance.PID, syscall.SIGKILL)
					}
				}
				deadline := time.Now().Add(time.Second)
				for time.Now().Before(deadline) {
					current := m.List()["app"]
					if current == nil || current.PID == 0 {
						return
					}
					time.Sleep(time.Millisecond)
				}
				t.Error("direct child was not reaped during cleanup")
			}()
			service := config.ServiceConfig{Name: "app", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}
			if err := m.Start(context.Background(), service); err != nil {
				t.Fatal(err)
			}
			old := m.List()[service.Name]
			inspector.gateMu.Lock()
			inspector.oldPID = old.PID
			inspector.gateMu.Unlock()
			stopped := make(chan error, 1)
			go func() { stopped <- m.Stop(service.Name) }()
			select {
			case <-inspector.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("old Stop did not reach the delayed post-signal inspection")
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				current := m.List()[service.Name]
				if current.PID == 0 && current.State == StateStopped {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("real Wait monitor did not publish the old child's exit: %#v", current)
				}
				time.Sleep(time.Millisecond)
			}
			if err := m.Start(context.Background(), service); err != nil {
				t.Fatalf("replacement could not start while stale Stop inspection was delayed: %v", err)
			}
			replacement := m.List()[service.Name]
			if replacement.PID == old.PID || replacement.instanceID == old.instanceID {
				t.Fatalf("Start did not create an independent replacement: %#v", replacement)
			}
			// Stop unlinks this path without inspecting the file type. A marker
			// exercises that exact side effect without requiring socket binding
			// permissions in the test environment.
			if err := os.WriteFile(replacement.SocketPath, []byte("replacement socket"), 0o600); err != nil {
				t.Fatal(err)
			}
			socketInfo, err := os.Stat(replacement.SocketPath)
			if err != nil {
				t.Fatal(err)
			}
			path := manifestPath(replacement.manifest.VMDir)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			release()
			select {
			case err := <-stopped:
				if finalErr == errProcessNotFound && !errors.Is(err, ErrStartInProgress) {
					t.Fatalf("stale completion did not report changed ownership: %v", err)
				}
				if finalErr == inspectionFailure && !errors.Is(err, inspectionFailure) {
					t.Fatalf("delayed inspection failure was not returned: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("stale Stop did not complete after releasing inspection")
			}
			current := m.List()[service.Name]
			if current.PID != replacement.PID || current.instanceID != replacement.instanceID || current.State != StateRunning || current.LastError != "" {
				t.Fatalf("stale Stop changed or quarantined the replacement: %#v", current)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("stale Stop overwrote the replacement manifest: before=%s after=%s err=%v", before, after, err)
			}
			afterSocket, err := os.Stat(replacement.SocketPath)
			if err != nil || !os.SameFile(socketInfo, afterSocket) {
				t.Fatalf("stale Stop unlinked or replaced the replacement socket: %v", err)
			}
		})
	}
}
