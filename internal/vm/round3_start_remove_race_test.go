package vm

import (
	"context"
	"errors"
	"github.com/artemnikitin/firework/internal/config"
	"os"
	"sync"
	"testing"
	"time"
)

func TestRound3RemoveDuringOldStateReclaimPreservesUnprovedRuntime(t *testing.T) {
	m, manifest, inspector := recoveryFixture(t)
	// A failed launch did not record its PID, but a surviving process still
	// claims the old files. Reclamation must finish scanning before deciding.
	manifest.PID = 0
	manifest.Lifecycle = lifecycleFailed
	if err := writeManifest(manifestPath(manifest.VMDir), manifest); err != nil {
		t.Fatal(err)
	}
	oldFiles := map[string]string{
		manifest.ConfigPath: "old VM configuration",
		manifest.SocketPath: "old socket marker",
	}
	for path, content := range oldFiles {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	blocking := &round3BlockingScanner{fakeProcessInspector: inspector, entered: make(chan struct{}), release: make(chan struct{})}
	m.inspector = blocking
	launcher := &countingLauncher{}
	m.launcher = launcher
	var once sync.Once
	unblock := func() { once.Do(func() { close(blocking.release) }) }
	defer unblock()
	started := make(chan error, 1)
	go func() { started <- m.Start(context.Background(), manifest.Config) }()
	select {
	case <-blocking.entered:
	case <-time.After(time.Second):
		t.Fatal("start did not reach old runtime claimant scan")
	}
	removed := make(chan error, 1)
	go func() { removed <- m.Remove(manifest.Service) }()
	select {
	case err := <-removed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Remove blocked behind old runtime inspection")
	}
	assertOldFiles := func() {
		t.Helper()
		for path, content := range oldFiles {
			got, err := os.ReadFile(path)
			if err != nil || string(got) != content {
				t.Fatalf("unproved old runtime file changed: %s: %q, %v", path, got, err)
			}
		}
		if _, err := os.Stat(manifestPath(manifest.VMDir)); err != nil {
			t.Fatalf("old manifest was removed: %v", err)
		}
	}
	assertOldFiles()
	unblock()
	select {
	case err := <-started:
		if err == nil {
			t.Fatal("start succeeded despite the surviving claimant and removal request")
		}
	case <-time.After(time.Second):
		t.Fatal("start did not finish after claimant scan was released")
	}
	assertOldFiles()
	if launcher.count() != 0 {
		t.Fatal("aborted start launched another VM")
	}
}

func TestRound3DiscoveryReservationBlocksConcurrentMutation(t *testing.T) {
	m, f, inspector := recoveryFixture(t)
	blocker := &blockingRecoveryInspector{fakeProcessInspector: inspector, entered: make(chan struct{}), release: make(chan struct{})}
	m.inspector = blocker
	done := make(chan error, 1)
	go func() { _, err := m.Recover(context.Background(), config.NodeConfig{}); done <- err }()
	select {
	case <-blocker.entered:
	case <-time.After(time.Second):
		t.Fatal("discovery did not inspect")
	}
	if err := m.Start(context.Background(), f.Config); !errors.Is(err, ErrStartInProgress) {
		t.Fatalf("start passed discovery reservation: %v", err)
	}
	if err := m.Remove(f.Service); !errors.Is(err, ErrStartInProgress) {
		t.Fatalf("remove passed discovery reservation: %v", err)
	}
	close(blocker.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
