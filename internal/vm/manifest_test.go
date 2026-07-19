package vm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

func TestWriteManifestIsAtomicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	path := manifestPath(dir)
	manifest := &instanceManifest{
		SchemaVersion: manifestSchemaVersion, Service: "app", InstanceID: "instance",
		VMDir: dir, SocketPath: filepath.Join(dir, "firecracker.sock"),
		ConfigPath: filepath.Join(dir, "vm-config.json"),
	}
	if err := writeManifest(path, manifest); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode = %o, want 600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != manifestFileName {
		t.Fatalf("unexpected manifest directory entries: %v", entries)
	}
}

func TestServiceConfigHashChangesWithResolvedConfiguration(t *testing.T) {
	first := config.ServiceConfig{Name: "app", Image: "/one", VCPUs: 1}
	second := first
	second.Image = "/two"
	firstHash, err := serviceConfigHash(first)
	if err != nil {
		t.Fatal(err)
	}
	secondHash, err := serviceConfigHash(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstHash == secondHash {
		t.Fatal("different resolved configurations produced the same hash")
	}
}
