package vm

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/artemnikitin/firework/internal/config"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestConcurrentClearRevalidatesUntrackedSnapshot(t *testing.T) {
	for _, change := range []string{"cleared", "replacement", "discovery"} {
		t.Run(change, func(t *testing.T) {
			m, f, i := recoveryFixture(t)
			m.inspector = i
			delete(i.identities, f.PID)
			body, err := json.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			path := manifestPath(f.VMDir)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			first := make(chan error, 1)
			go func() { _, err := m.ClearRecoveryPending(f.Service); first <- err }()
			// A FIFO pauses only the first disk read. The second command sees a
			// normal manifest, so the interleaving models two ordinary file reads.
			var writer *os.File
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				writer, err = os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0600)
				if err == nil {
					break
				}
				if !errors.Is(err, syscall.ENXIO) {
					t.Fatal(err)
				}
				time.Sleep(time.Millisecond)
			}
			if writer == nil {
				t.Fatal("first resolver did not begin its read")
			}
			defer writer.Close()
			if err := os.Rename(path, filepath.Join(f.VMDir, "old-read.fifo")); err != nil {
				t.Fatal(err)
			}
			if err := writeManifest(path, f); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "cleared":
				if r, err := m.ClearRecoveryPending(f.Service); err != nil || r.Outcome != RecoveryOutcomeCleared {
					t.Fatalf("second clear %#v %v", r, err)
				}
			case "replacement":
				replacement := *f
				replacement.InstanceID = "replacement-instance"
				if err := writeManifest(path, &replacement); err != nil {
					t.Fatal(err)
				}
			case "discovery":
				if !m.reserveRecoveryDiscovery(f.Service) {
					t.Fatal("could not reserve discovery")
				}
				defer m.releaseRecoveryDiscovery(f.Service)
			}
			if _, err := writer.Write(body); err != nil {
				t.Fatal(err)
			}
			writer.Close()
			select {
			case err := <-first:
				if change == "cleared" && err != nil {
					t.Fatal("stale clear did not converge", err)
				}
				if change != "cleared" && err == nil {
					t.Fatal("stale snapshot was accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("stale command stuck")
			}
			if m.HasPendingRecovery() {
				t.Fatal("stale read resurrected pending state")
			}
			if change == "cleared" {
				if _, err := os.Stat(f.VMDir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("runtime not absent %v", err)
				}
				if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
					t.Fatal(err)
				}
			} else {
				got, err := readManifest(path)
				if err != nil {
					t.Fatal(err)
				}
				if change == "replacement" && got.InstanceID != "replacement-instance" {
					t.Fatal("replacement manifest was changed")
				}
			}
			if got := m.List()[f.Service]; got != nil {
				t.Fatalf("phantom instance %#v", got)
			}
		})
	}
}
