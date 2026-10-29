package vm

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestConcurrentRecoveryReservesServiceAndRevalidatesSnapshot(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "external replacement"}[replace], func(t *testing.T) {
			m, f, inspector := recoveryFixture(t)
			delete(inspector.identities, f.PID)
			blocker := &blockingRecoveryInspector{fakeProcessInspector: inspector, entered: make(chan struct{}), release: make(chan struct{})}
			m.inspector = blocker
			first := make(chan error, 1)
			go func() { _, err := m.ClearRecoveryPending(f.Service); first <- err }()
			select {
			case <-blocker.entered:
			case <-time.After(time.Second):
				t.Fatal("first resolver did not inspect")
			}
			if _, err := m.ClearRecoveryPending(f.Service); err == nil {
				t.Fatal("second resolver bypassed reservation")
			}
			if m.reserveRecoveryDiscovery(f.Service) {
				t.Fatal("discovery bypassed reservation")
			}
			if replace {
				replacement := *f
				replacement.InstanceID = "replacement-instance"
				if err := writeManifest(manifestPath(f.VMDir), &replacement); err != nil {
					t.Fatal(err)
				}
			}
			close(blocker.release)
			select {
			case err := <-first:
				if replace && err == nil {
					t.Fatal("stale manifest was accepted")
				}
				if !replace && err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("resolver stuck")
			}
			if replace {
				durable, err := readManifest(manifestPath(f.VMDir))
				if err != nil || durable.InstanceID != "replacement-instance" {
					t.Fatalf("replacement was changed: %#v %v", durable, err)
				}
			} else if _, err := os.Stat(f.VMDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("absent runtime not removed: %v", err)
			}
		})
	}
}
