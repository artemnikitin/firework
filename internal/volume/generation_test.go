package volume

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

func TestGenerationOnlyPreparationPersistsWithoutFilesystemWork(t *testing.T) {
	for _, failWrite := range []bool{false, true} {
		name := "durable generation"
		if failWrite {
			name = "failed write then retry"
		}
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{}
			manager, root := hardeningManager(t, runner)
			prepared, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1))
			if err != nil {
				t.Fatal(err)
			}
			image := prepared[0].PathOnHost
			data := []byte("retained application data")
			file, err := os.OpenFile(image, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, err = file.WriteAt(data, 0)
			_ = file.Close()
			if err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(root, "app", "data", manifestFilename)
			runner.destructive, runner.calls = nil, nil
			if failWrite {
				// A directory blocks the atomic writer without relying on uid permissions.
				if err := os.Mkdir(manifestPath+".tmp", 0o700); err != nil {
					t.Fatal(err)
				}
				if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 3)); err == nil {
					t.Fatal("failed generation write was reported applied")
				}
				if got := readManifest(t, root, "app", "data"); got.ResizeGeneration != 1 {
					t.Fatalf("failed write advanced durable generation: %#v", got)
				}
				if len(runner.destructive) != 0 {
					t.Fatalf("failed metadata write ran filesystem tools: %v", runner.destructive)
				}
				if err := os.Remove(manifestPath + ".tmp"); err != nil {
					t.Fatal(err)
				}
			}
			got, err := manager.Prepare(context.Background(), localService(16*config.MiB, 3))
			if err != nil {
				t.Fatal(err)
			}
			if got[0].ResizeGeneration != 3 || got[0].SizeBytes != 16*config.MiB || len(runner.calls) != 0 {
				t.Fatalf("generation-only request changed filesystem: prepared=%#v tools=%v", got, runner.calls)
			}
			if _, err := os.Stat(filepath.Join(root, "app", "data", transactionFilename)); !os.IsNotExist(err) {
				t.Fatalf("metadata update created a resize transaction: %v", err)
			}
			freshRunner := &fakeRunner{}
			restarted := NewManagerWithDependencies("node-1", manager.storage, freshRunner, acceptingMounts{})
			again, err := restarted.Prepare(context.Background(), localService(16*config.MiB, 3))
			if err != nil || again[0].ResizeGeneration != 3 || len(freshRunner.calls) != 0 {
				t.Fatalf("generation was not durable/idempotent: prepared=%#v tools=%v err=%v", again, freshRunner.calls, err)
			}
			file, err = os.Open(image)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			retained := make([]byte, len(data))
			if _, err := file.ReadAt(retained, 0); err != nil || string(retained) != string(data) {
				t.Fatalf("metadata update changed retained data: %q %v", retained, err)
			}
			if info, err := file.Stat(); err != nil || info.Size() != 16*config.MiB {
				t.Fatalf("image size changed: %v %v", info, err)
			}
		})
	}
}

func TestGenerationOnlyPreparationHonorsResizeTransactions(t *testing.T) {
	for _, kind := range []string{"matching pending", "conflicting pending", "older committed", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			runner := &fakeRunner{}
			manager, root := hardeningManager(t, runner)
			if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "app", "data")
			transactionPath := filepath.Join(dir, transactionFilename)
			tx := resizeTransaction{OldSizeBytes: 16 * config.MiB, DesiredSizeBytes: 16 * config.MiB, Generation: 3, Direction: "grow", Phase: "checking"}
			switch kind {
			case "conflicting pending":
				tx.DesiredSizeBytes, tx.Generation = 24*config.MiB, 2
			case "older committed":
				tx.OldSizeBytes, tx.Generation, tx.Phase = 8*config.MiB, 1, "filesystem_resized"
			}
			if kind == "unreadable" {
				if err := os.WriteFile(transactionPath, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := writeJSONAtomic(transactionPath, tx); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(dir, manifestFilename))
			if err != nil {
				t.Fatal(err)
			}
			runner.calls, runner.destructive = nil, nil
			got, err := manager.Prepare(context.Background(), localService(16*config.MiB, 3))
			if kind == "matching pending" {
				if err != nil || got[0].ResizeGeneration != 3 || len(runner.destructive) != 3 {
					t.Fatalf("pending transaction was bypassed: prepared=%#v tools=%v err=%v", got, runner.destructive, err)
				}
				if _, err := os.Stat(transactionPath); !os.IsNotExist(err) {
					t.Fatalf("completed transaction retained: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "quarantined") || len(runner.destructive) != 0 {
					t.Fatalf("ambiguous transaction was bypassed: tools=%v err=%v", runner.destructive, err)
				}
				after, readErr := os.ReadFile(filepath.Join(dir, manifestFilename))
				if readErr != nil || string(after) != string(before) {
					t.Fatalf("quarantine advanced manifest: %v", readErr)
				}
				if _, err := os.Stat(transactionPath); err != nil {
					t.Fatalf("quarantine removed transaction: %v", err)
				}
			}
		})
	}
}

func TestGenerationOnlyPreparationClearsPreviousRejection(t *testing.T) {
	runner := &fakeRunner{}
	manager, root := hardeningManager(t, runner)
	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prepare(context.Background(), localService(2*config.MiB, 2)); err != nil {
		t.Fatal(err)
	}
	if len(manager.Rejections()) != 1 {
		t.Fatal("expected a standing rejected shrink")
	}
	runner.calls, runner.destructive = nil, nil
	got, err := manager.Prepare(context.Background(), localService(16*config.MiB, 3))
	if err != nil || got[0].Rejected || got[0].ResizeGeneration != 3 || len(runner.destructive) != 0 || len(manager.Rejections()) != 0 {
		t.Fatalf("new generation did not clear rejection without filesystem work: prepared=%#v tools=%v rejections=%v err=%v", got, runner.destructive, manager.Rejections(), err)
	}
	durable := readManifest(t, root, "app", "data")
	if durable.ResizeGeneration != 3 || durable.RejectedGeneration != 0 || durable.RejectedSizeBytes != 0 || durable.RejectedMinimumBytes != 0 || !durable.RejectedAt.IsZero() {
		t.Fatalf("stale rejection retained in manifest: %#v", durable)
	}
}
