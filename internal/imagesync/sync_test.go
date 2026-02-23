package imagesync

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fakeS3 implements s3API for testing.
type fakeS3 struct {
	objects map[string]fakeObject
}

type fakeObject struct {
	body string
	etag string
}

func (f *fakeS3) HeadObject(_ context.Context, input *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	obj, ok := f.objects[*input.Key]
	if !ok {
		return nil, fmt.Errorf("NoSuchKey: %s", *input.Key)
	}
	return &s3.HeadObjectOutput{
		ETag: aws.String(obj.etag),
	}, nil
}

func (f *fakeS3) GetObject(_ context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	obj, ok := f.objects[*input.Key]
	if !ok {
		return nil, fmt.Errorf("NoSuchKey: %s", *input.Key)
	}
	return &s3.GetObjectOutput{
		Body: io.NopCloser(strings.NewReader(obj.body)),
		ETag: aws.String(obj.etag),
	}, nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSync_DownloadsNewImage(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeS3{
		objects: map[string]fakeObject{
			"web-rootfs.ext4": {body: "rootfs-content", etag: `"abc123"`},
		},
	}

	syncer := newSyncerWithAPI(fake, "images-bucket", dir, testLogger())

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

	// Verify ETag sidecar was written.
	etag, err := os.ReadFile(filepath.Join(dir, "web-rootfs.ext4.etag"))
	if err != nil {
		t.Fatalf("reading etag sidecar: %v", err)
	}
	if string(etag) != `"abc123"` {
		t.Errorf("expected etag \"abc123\", got %s", string(etag))
	}
}

func TestSync_SkipsUnchangedImage(t *testing.T) {
	dir := t.TempDir()

	// Pre-populate the file and ETag sidecar.
	os.WriteFile(filepath.Join(dir, "web-rootfs.ext4"), []byte("existing-content"), 0o644)
	os.WriteFile(filepath.Join(dir, "web-rootfs.ext4.etag"), []byte(`"abc123"`), 0o644)

	callCount := 0
	fake := &countingFakeS3{
		fakeS3: &fakeS3{
			objects: map[string]fakeObject{
				"web-rootfs.ext4": {body: "new-content", etag: `"abc123"`},
			},
		},
		getObjectCalls: &callCount,
	}

	syncer := newSyncerWithAPI(fake, "images-bucket", dir, testLogger())

	services := []config.ServiceConfig{
		{Name: "web", Image: "/var/lib/images/web-rootfs.ext4"},
	}

	if err := syncer.Sync(context.Background(), services); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// GetObject should NOT have been called (ETag matched).
	if callCount != 0 {
		t.Errorf("expected 0 GetObject calls, got %d", callCount)
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

func TestSync_RedownloadsOnETagChange(t *testing.T) {
	dir := t.TempDir()

	// Pre-populate with old ETag.
	os.WriteFile(filepath.Join(dir, "web-rootfs.ext4"), []byte("old-content"), 0o644)
	os.WriteFile(filepath.Join(dir, "web-rootfs.ext4.etag"), []byte(`"old-etag"`), 0o644)

	fake := &fakeS3{
		objects: map[string]fakeObject{
			"web-rootfs.ext4": {body: "new-content", etag: `"new-etag"`},
		},
	}

	syncer := newSyncerWithAPI(fake, "images-bucket", dir, testLogger())

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

	// ETag sidecar should be updated.
	etag, err := os.ReadFile(filepath.Join(dir, "web-rootfs.ext4.etag"))
	if err != nil {
		t.Fatalf("reading etag: %v", err)
	}
	if string(etag) != `"new-etag"` {
		t.Errorf("expected new-etag, got %s", string(etag))
	}
}

func TestSync_MultipleImages(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeS3{
		objects: map[string]fakeObject{
			"web-rootfs.ext4": {body: "web-data", etag: `"e1"`},
			"vmlinux-5.10":    {body: "kernel-data", etag: `"e2"`},
		},
	}

	syncer := newSyncerWithAPI(fake, "images-bucket", dir, testLogger())

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
				"web-rootfs.ext4":    {body: "web", etag: `"e1"`},
				"worker-rootfs.ext4": {body: "worker", etag: `"e2"`},
				"vmlinux-5.10":       {body: "kernel", etag: `"e3"`},
			},
		},
		getObjectCalls: &callCount,
	}

	syncer := newSyncerWithAPI(fake, "images-bucket", dir, testLogger())

	services := []config.ServiceConfig{
		{Name: "web", Image: "/var/lib/images/web-rootfs.ext4", Kernel: "/var/lib/images/vmlinux-5.10"},
		{Name: "worker", Image: "/var/lib/images/worker-rootfs.ext4", Kernel: "/var/lib/images/vmlinux-5.10"},
	}

	if err := syncer.Sync(context.Background(), services); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// 3 GetObject calls (web rootfs, vmlinux, worker rootfs) — kernel NOT downloaded twice.
	if callCount != 3 {
		t.Errorf("expected 3 GetObject calls (dedup kernel), got %d", callCount)
	}
}

func TestSync_NotFoundInS3_LocalExists(t *testing.T) {
	dir := t.TempDir()

	// Kernel exists locally (baked into AMI) but not in S3.
	os.WriteFile(filepath.Join(dir, "vmlinux-5.10"), []byte("kernel-data"), 0o644)

	fake := &fakeS3{
		objects: map[string]fakeObject{
			"web-rootfs.ext4": {body: "rootfs-content", etag: `"abc123"`},
			// vmlinux-5.10 intentionally absent from S3
		},
	}

	syncer := newSyncerWithAPI(fake, "images-bucket", dir, testLogger())

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

	syncer := newSyncerWithAPI(fake, "images-bucket", dir, testLogger())

	services := []config.ServiceConfig{
		{Name: "web", Image: "/var/lib/images/missing.ext4"},
	}

	err := syncer.Sync(context.Background(), services)
	if err == nil {
		t.Fatal("expected error when image missing from S3 and locally, got nil")
	}
	if !strings.Contains(err.Error(), "not found in S3") {
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
			"web-rootfs.ext4": {body: "rootfs-content", etag: `"abc123"`},
			// vmlinux-5.10 intentionally absent from S3
		},
	}

	syncer := newSyncerWithAPI(fake, "images-bucket", dir, testLogger())
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
	syncer := newSyncerWithAPI(fake, "images-bucket", dir, testLogger())

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

// countingFakeS3 wraps fakeS3 to count GetObject calls.
type countingFakeS3 struct {
	*fakeS3
	getObjectCalls *int
}

func (c *countingFakeS3) GetObject(ctx context.Context, input *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	*c.getObjectCalls++
	return c.fakeS3.GetObject(ctx, input, opts...)
}
