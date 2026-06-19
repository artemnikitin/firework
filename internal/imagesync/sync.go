package imagesync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/objectstorage"
)

// S3Config configures an S3 image source.
type S3Config = objectstorage.S3Config

// GCSConfig configures a GCS image source.
type GCSConfig = objectstorage.GCSConfig

// Syncer downloads VM images from object storage to the local image directory.
// Opaque write-token sidecars prevent unchanged objects from being downloaded.
type Syncer struct {
	store     objectstorage.BlobStore
	bucket    string
	imagesDir string
	logger    *slog.Logger
}

// NewSyncer creates a syncer over an existing BlobStore.
func NewSyncer(bucket, imagesDir string, store objectstorage.BlobStore, logger *slog.Logger) *Syncer {
	return &Syncer{store: store, bucket: bucket, imagesDir: imagesDir, logger: logger}
}

// NewS3Syncer creates an S3-backed image syncer.
func NewS3Syncer(ctx context.Context, cfg S3Config, imagesDir string, logger *slog.Logger) (*Syncer, error) {
	store, err := objectstorage.NewS3BlobStore(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return NewSyncer(cfg.Bucket, imagesDir, store, logger), nil
}

// NewGCSSyncer creates a native GCS-backed image syncer.
func NewGCSSyncer(ctx context.Context, cfg GCSConfig, imagesDir string, logger *slog.Logger) (*Syncer, error) {
	store, err := objectstorage.NewGCSBlobStore(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return NewSyncer(cfg.Bucket, imagesDir, store, logger), nil
}

// Close releases the underlying object storage client.
func (s *Syncer) Close() error { return s.store.Close() }

// Sync ensures all referenced images are present locally and current.
func (s *Syncer) Sync(ctx context.Context, services []config.ServiceConfig) error {
	paths := collectImagePaths(services)
	for _, path := range paths {
		key := filepath.Base(path)
		if err := s.syncOne(ctx, key, filepath.Join(s.imagesDir, key)); err != nil {
			return fmt.Errorf("syncing %s: %w", key, err)
		}
	}
	return nil
}

func (s *Syncer) syncOne(ctx context.Context, key, localPath string) error {
	tokenPath := localPath + ".token"
	meta, exists, err := s.store.Head(ctx, key)
	if err != nil {
		return fmt.Errorf("head object %s: %w", key, err)
	}
	if !exists {
		if _, statErr := os.Stat(localPath); statErr == nil {
			s.logger.Debug("not in object storage, using local copy", "key", key)
			return nil
		}
		if aliasTarget, aliasErr := ensureLocalKernelAlias(localPath, key); aliasErr == nil && aliasTarget != "" {
			s.logger.Debug("not in object storage, using local kernel alias", "key", key, "target", aliasTarget)
			return nil
		}
		return fmt.Errorf("image %s not found in object storage and no local copy exists", key)
	}

	remoteToken := string(meta.WriteToken)
	if localToken, err := os.ReadFile(tokenPath); err == nil && string(localToken) == remoteToken {
		s.logger.Debug("image up to date, skipping", "key", key)
		return nil
	}

	s.logger.Info("downloading image", "bucket", s.bucket, "key", key, "write_token", remoteToken)
	r, getMeta, err := s.store.Get(ctx, key)
	if err != nil {
		if errors.Is(err, objectstorage.ErrNotFound) {
			return fmt.Errorf("image %s disappeared during download: %w", key, err)
		}
		return fmt.Errorf("get object %s: %w", key, err)
	}
	defer r.Close()
	if getMeta.WriteToken != "" {
		remoteToken = string(getMeta.WriteToken)
	}

	tmpPath := localPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing %s: %w", tmpPath, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, localPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming to %s: %w", localPath, err)
	}
	if err := os.WriteFile(tokenPath, []byte(remoteToken), 0o644); err != nil {
		return fmt.Errorf("writing token sidecar: %w", err)
	}
	return nil
}

func collectImagePaths(services []config.ServiceConfig) []string {
	seen := make(map[string]bool)
	var paths []string
	for _, svc := range services {
		for _, p := range []string{svc.Image, svc.Kernel} {
			if p != "" && !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	return paths
}

func ensureLocalKernelAlias(localPath, key string) (string, error) {
	prefix := strings.TrimPrefix(key, "vmlinux-")
	if prefix == key || len(strings.Split(prefix, ".")) != 2 {
		return "", nil
	}
	matches, err := filepath.Glob(localPath + ".*")
	if err != nil || len(matches) == 0 {
		return "", err
	}
	sort.Slice(matches, func(i, j int) bool {
		infoI, errI := os.Stat(matches[i])
		infoJ, errJ := os.Stat(matches[j])
		if errI != nil || errJ != nil {
			return matches[i] < matches[j]
		}
		return infoI.ModTime().After(infoJ.ModTime())
	})
	target := matches[0]
	_ = os.Remove(localPath)
	if err := os.Symlink(filepath.Base(target), localPath); err == nil {
		return target, nil
	}
	if err := copyFile(target, localPath); err != nil {
		return "", err
	}
	return target, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
