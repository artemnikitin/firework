package imagesync

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/objectstorage"
)

// fakeS3 implements objectstorage.BlobStore for testing.
type fakeS3 struct {
	objects map[string]fakeObject
}

type fakeObject struct {
	body  string
	token string
}

func (f *fakeS3) Head(_ context.Context, key string) (objectstorage.BlobMeta, bool, error) {
	obj, ok := f.objects[key]
	if !ok {
		return objectstorage.BlobMeta{}, false, nil
	}
	return objectstorage.BlobMeta{WriteToken: objectstorage.WriteToken(obj.token)}, true, nil
}

func (f *fakeS3) Get(_ context.Context, key string) (io.ReadCloser, objectstorage.BlobMeta, error) {
	obj, ok := f.objects[key]
	if !ok {
		return nil, objectstorage.BlobMeta{}, objectstorage.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(obj.body)), objectstorage.BlobMeta{WriteToken: objectstorage.WriteToken(obj.token)}, nil
}

func (f *fakeS3) GetBytes(_ context.Context, key string) ([]byte, objectstorage.BlobMeta, bool, error) {
	obj, ok := f.objects[key]
	return []byte(obj.body), objectstorage.BlobMeta{WriteToken: objectstorage.WriteToken(obj.token)}, ok, nil
}

func (f *fakeS3) Put(_ context.Context, key string, r io.Reader, _ objectstorage.PutOptions) (objectstorage.BlobMeta, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return objectstorage.BlobMeta{}, err
	}
	f.objects[key] = fakeObject{body: string(data), token: "new-token"}
	return objectstorage.BlobMeta{WriteToken: "new-token"}, nil
}

func (f *fakeS3) PutIfAbsent(ctx context.Context, key string, r io.Reader, opts objectstorage.PutOptions) (bool, objectstorage.BlobMeta, error) {
	if _, exists := f.objects[key]; exists {
		return false, objectstorage.BlobMeta{}, nil
	}
	meta, err := f.Put(ctx, key, r, opts)
	return err == nil, meta, err
}

func (f *fakeS3) PutIfMatch(ctx context.Context, key string, expected objectstorage.WriteToken, r io.Reader, opts objectstorage.PutOptions) (bool, objectstorage.BlobMeta, error) {
	if objectstorage.WriteToken(f.objects[key].token) != expected {
		return false, objectstorage.BlobMeta{}, nil
	}
	meta, err := f.Put(ctx, key, r, opts)
	return err == nil, meta, err
}

func (f *fakeS3) Delete(_ context.Context, key string) error         { delete(f.objects, key); return nil }
func (f *fakeS3) ListKeys(context.Context, string) ([]string, error) { return nil, nil }
func (f *fakeS3) Close() error                                       { return nil }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSync_DownloadsNewImage(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeS3{
		objects: map[string]fakeObject{
			"web-rootfs.ext4": {body: "rootfs-content", token: `"abc123"`},
		},
	}

	syncer := NewSyncer("images-bucket", dir, fake, testLogger())

	services := []config.ServiceConfig{
		{Name: "web", Image: "/var/lib/images/web-rootfs.ext4"},
	}

	if err := syncer.Sync(context.Background(), services); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Verify file was downloaded.
	data, err := os.ReadFile(filepath.Join(dir, "web-rootfs.ext4"))
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	if string(data) != "rootfs-content" {
		t.Errorf("expected rootfs-content, got %s", string(data))
	}

	// Verify token sidecar was written.
	token, err := os.ReadFile(filepath.Join(dir, "web-rootfs.ext4.token"))
	if err != nil {
		t.Fatalf("reading token sidecar: %v", err)
	}
	if string(token) != `"abc123"` {
		t.Errorf("expected token \"abc123\", got %s", string(token))
	}
}

func TestSync_SkipsUnchangedImage(t *testing.T) {
	dir := t.TempDir()

	// Pre-populate the file and token sidecar.
	os.WriteFile(filepath.Join(dir, "web-rootfs.ext4"), []byte("existing-content"), 0o644)
	os.WriteFile(filepath.Join(dir, "web-rootfs.ext4.token"), []byte(`"abc123"`), 0o644)

	callCount := 0
	fake := &countingFakeS3{
		fakeS3: &fakeS3{
			objects: map[string]fakeObject{
				"web-rootfs.ext4": {body: "new-content", token: `"abc123"`},
			},
		},
		getObjectCalls: &callCount,
	}

	syncer := NewSyncer("images-bucket", dir, fake, testLogger())

	services := []config.ServiceConfig{
		{Name: "web", Image: "/var/lib/images/web-rootfs.ext4"},
	}

	if err := syncer.Sync(context.Background(), services); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Get should NOT have been called (token matched).
	if callCount != 0 {
		t.Errorf("expected 0 Get calls, got %d", callCount)
	}

	// File should still have old content.
	data, err := os.ReadFile(filepath.Join(dir, "web-rootfs.ext4"))
	if err != nil {
		t.Fatalf("reading file: %v", err)
	}
	if string(data) != "existing-content" {
		t.Errorf("expected existing-content, got %s", string(data))
	}
}

func TestSync_RedownloadsOnTokenChange(t *testing.T) {
	dir := t.TempDir()

	// Pre-populate with old token.
	os.WriteFile(filepath.Join(dir, "web-rootfs.ext4"), []byte("old-content"), 0o644)
	os.WriteFile(filepath.Join(dir, "web-rootfs.ext4.token"), []byte(`"old-token"`), 0o644)

	fake := &fakeS3{
		objects: map[string]fakeObject{
			"web-rootfs.ext4": {body: "new-content", token: `"new-token"`},
		},
	}

	syncer := NewSyncer("images-bucket", dir, fake, testLogger())

	services := []config.ServiceConfig{
		{Name: "web", Image: "/var/lib/images/web-rootfs.ext4"},
	}

	if err := syncer.Sync(context.Background(), services); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// File should have new content.
	data, err := os.ReadFile(filepath.Join(dir, "web-rootfs.ext4"))
	if err != nil {
		t.Fatalf("reading file: %v", err)
	}
	if string(data) != "new-content" {
		t.Errorf("expected new-content, got %s", string(data))
	}

	// Token sidecar should be updated.
	token, err := os.ReadFile(filepath.Join(dir, "web-rootfs.ext4.token"))
	if err != nil {
		t.Fatalf("reading token: %v", err)
	}
	if string(token) != `"new-token"` {
		t.Errorf("expected new-token, got %s", string(token))
	}
}

func TestSync_MultipleImages(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeS3{
		objects: map[string]fakeObject{
			"web-rootfs.ext4": {body: "web-data", token: `"e1"`},
			"vmlinux-5.10":    {body: "kernel-data", token: `"e2"`},
		},
	}

	syncer := NewSyncer("images-bucket", dir, fake, testLogger())

	services := []config.ServiceConfig{
		{
			Name:   "web",
			Image:  "/var/lib/images/web-rootfs.ext4",
			Kernel: "/var/lib/images/vmlinux-5.10",
		},
	}

	if err := syncer.Sync(context.Background(), services); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Both files should be present.
	for _, name := range []string{"web-rootfs.ext4", "vmlinux-5.10"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s to exist: %v", name, err)
		}
	}
}

func TestSync_DeduplicatesSharedKernel(t *testing.T) {
	dir := t.TempDir()
	callCount := 0
	fake := &countingFakeS3{
		fakeS3: &fakeS3{
			objects: map[string]fakeObject{
				"web-rootfs.ext4":    {body: "web", token: `"e1"`},
				"worker-rootfs.ext4": {body: "worker", token: `"e2"`},
				"vmlinux-5.10":       {body: "kernel", token: `"e3"`},
			},
		},
		getObjectCalls: &callCount,
	}

	syncer := NewSyncer("images-bucket", dir, fake, testLogger())

	services := []config.ServiceConfig{
		{Name: "web", Image: "/var/lib/images/web-rootfs.ext4", Kernel: "/var/lib/images/vmlinux-5.10"},
		{Name: "worker", Image: "/var/lib/images/worker-rootfs.ext4", Kernel: "/var/lib/images/vmlinux-5.10"},
	}

	if err := syncer.Sync(context.Background(), services); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// 3 Get calls (web rootfs, vmlinux, worker rootfs) — kernel is not downloaded twice.
	if callCount != 3 {
		t.Errorf("expected 3 Get calls (dedup kernel), got %d", callCount)
	}
}

func TestSync_NotFoundInS3_LocalExists(t *testing.T) {
	dir := t.TempDir()

	// Kernel exists locally (baked into AMI) but not in S3.
	os.WriteFile(filepath.Join(dir, "vmlinux-5.10"), []byte("kernel-data"), 0o644)

	fake := &fakeS3{
		objects: map[string]fakeObject{
			"web-rootfs.ext4": {body: "rootfs-content", token: `"abc123"`},
			// vmlinux-5.10 intentionally absent from S3
		},
	}

	syncer := NewSyncer("images-bucket", dir, fake, testLogger())

	services := []config.ServiceConfig{
		{
			Name:   "web",
			Image:  "/var/lib/images/web-rootfs.ext4",
			Kernel: "/var/lib/images/vmlinux-5.10",
		},
	}

	// Should succeed — kernel skipped, rootfs downloaded.
	if err := syncer.Sync(context.Background(), services); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Kernel file should still have original content (not overwritten).
	data, _ := os.ReadFile(filepath.Join(dir, "vmlinux-5.10"))
	if string(data) != "kernel-data" {
		t.Errorf("kernel content changed unexpectedly: %s", string(data))
	}
}

func TestSync_NotFoundInS3_NoLocalCopy(t *testing.T) {
	dir := t.TempDir()

	// Neither in S3 nor locally — should fail.
	fake := &fakeS3{
		objects: map[string]fakeObject{},
	}

	syncer := NewSyncer("images-bucket", dir, fake, testLogger())

	services := []config.ServiceConfig{
		{Name: "web", Image: "/var/lib/images/missing.ext4"},
	}

	err := syncer.Sync(context.Background(), services)
	if err == nil {
		t.Fatal("expected error when image missing from S3 and locally, got nil")
	}
	if !strings.Contains(err.Error(), "not found in object storage") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSync_NotFoundInS3_ResolvesUnversionedKernelAlias(t *testing.T) {
	dir := t.TempDir()
	oldKernel := filepath.Join(dir, "vmlinux-5.10.100")
	newKernel := filepath.Join(dir, "vmlinux-5.10.200")

	if err := os.WriteFile(oldKernel, []byte("old-kernel"), 0o644); err != nil {
		t.Fatalf("write old kernel: %v", err)
	}
	if err := os.WriteFile(newKernel, []byte("new-kernel"), 0o644); err != nil {
		t.Fatalf("write new kernel: %v", err)
	}

	now := time.Now()
	if err := os.Chtimes(oldKernel, now.Add(-1*time.Hour), now.Add(-1*time.Hour)); err != nil {
		t.Fatalf("set old kernel mtime: %v", err)
	}
	if err := os.Chtimes(newKernel, now, now); err != nil {
		t.Fatalf("set new kernel mtime: %v", err)
	}

	fake := &fakeS3{
		objects: map[string]fakeObject{
			"web-rootfs.ext4": {body: "rootfs-content", token: `"abc123"`},
			// vmlinux-5.10 intentionally absent from S3
		},
	}

	syncer := NewSyncer("images-bucket", dir, fake, testLogger())
	services := []config.ServiceConfig{
		{
			Name:   "web",
			Image:  "/var/lib/images/web-rootfs.ext4",
			Kernel: "/var/lib/images/vmlinux-5.10",
		},
	}

	if err := syncer.Sync(context.Background(), services); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	aliasPath := filepath.Join(dir, "vmlinux-5.10")
	data, err := os.ReadFile(aliasPath)
	if err != nil {
		t.Fatalf("reading kernel alias %s: %v", aliasPath, err)
	}
	if string(data) != "new-kernel" {
		t.Fatalf("expected alias to resolve newest kernel content, got %q", string(data))
	}
}

func TestSync_NoServices(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeS3{objects: map[string]fakeObject{}}
	syncer := NewSyncer("images-bucket", dir, fake, testLogger())

	if err := syncer.Sync(context.Background(), nil); err != nil {
		t.Fatalf("Sync with nil services: %v", err)
	}
}

func TestCollectImagePaths(t *testing.T) {
	services := []config.ServiceConfig{
		{Image: "/img/web.ext4", Kernel: "/img/vmlinux"},
		{Image: "/img/worker.ext4", Kernel: "/img/vmlinux"}, // shared kernel
		{Image: "/img/web.ext4"},                            // duplicate image
	}

	paths := collectImagePaths(services)

	// Should be 3 unique paths: web.ext4, vmlinux, worker.ext4.
	if len(paths) != 3 {
		t.Fatalf("expected 3 unique paths, got %d: %v", len(paths), paths)
	}
}

// countingFakeS3 wraps fakeS3 to count streaming Get calls.
type countingFakeS3 struct {
	*fakeS3
	getObjectCalls *int
}

func (c *countingFakeS3) Get(ctx context.Context, key string) (io.ReadCloser, objectstorage.BlobMeta, error) {
	*c.getObjectCalls++
	return c.fakeS3.Get(ctx, key)
}
