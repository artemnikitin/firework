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
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// s3API is the subset of S3 operations needed by the syncer.
type s3API interface {
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// Syncer downloads VM images (rootfs, kernels) from S3 to the local images
// directory. It uses ETag sidecars to skip re-downloading unchanged files.
type Syncer struct {
	client    s3API
	bucket    string
	imagesDir string
	logger    *slog.Logger
}

// NewSyncer creates a Syncer that downloads from the given S3 bucket.
func NewSyncer(ctx context.Context, bucket, imagesDir, region, endpointURL string, logger *slog.Logger) (*Syncer, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	var s3Opts []func(*s3.Options)
	if endpointURL != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpointURL)
			o.UsePathStyle = true
		})
	}

	client := s3.NewFromConfig(awsCfg, s3Opts...)

	return &Syncer{
		client:    client,
		bucket:    bucket,
		imagesDir: imagesDir,
		logger:    logger,
	}, nil
}

// newSyncerWithAPI creates a Syncer with a custom S3 API implementation.
// Used for testing.
func newSyncerWithAPI(api s3API, bucket, imagesDir string, logger *slog.Logger) *Syncer {
	return &Syncer{
		client:    api,
		bucket:    bucket,
		imagesDir: imagesDir,
		logger:    logger,
	}
}

// Sync ensures all images referenced by the given services are present
// locally and up to date. It compares S3 ETags against local sidecar files
// to skip unchanged objects.
func (s *Syncer) Sync(ctx context.Context, services []config.ServiceConfig) error {
	paths := collectImagePaths(services)
	if len(paths) == 0 {
		return nil
	}

	for _, path := range paths {
		key := filepath.Base(path)
		localPath := filepath.Join(s.imagesDir, key)

		if err := s.syncOne(ctx, key, localPath); err != nil {
			return fmt.Errorf("syncing %s: %w", key, err)
		}
	}
	return nil
}

// syncOne downloads a single object if the local copy is missing or stale.
func (s *Syncer) syncOne(ctx context.Context, key, localPath string) error {
	etagPath := localPath + ".etag"

	// Get the current ETag from S3.
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			// Object doesn't exist in S3. If we have a local copy (e.g. the
			// kernel baked into the AMI by Packer), skip silently.
			if _, statErr := os.Stat(localPath); statErr == nil {
				s.logger.Debug("not in S3, using local copy", "key", key)
				return nil
			}
			if aliasTarget, aliasErr := ensureLocalKernelAlias(localPath, key); aliasErr == nil && aliasTarget != "" {
				s.logger.Debug("not in S3, using local kernel alias", "key", key, "target", aliasTarget)
				return nil
			}
			return fmt.Errorf("image %s not found in S3 and no local copy exists", key)
		}
		return fmt.Errorf("HeadObject %s: %w", key, err)
	}

	remoteETag := ""
	if head.ETag != nil {
		remoteETag = *head.ETag
	}

	// Check local ETag sidecar — skip if it matches.
	if localETag, err := os.ReadFile(etagPath); err == nil {
		if string(localETag) == remoteETag {
			s.logger.Debug("image up to date, skipping", "key", key)
			return nil
		}
	}

	s.logger.Info("downloading image", "key", key, "etag", remoteETag)

	// Download to a temp file, then atomic rename.
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("GetObject %s: %w", key, err)
	}
	defer out.Body.Close()

	tmpPath := localPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}

	if _, err := io.Copy(f, out.Body); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing %s: %w", tmpPath, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing %s: %w", tmpPath, err)
	}

	// Atomic rename.
	if err := os.Rename(tmpPath, localPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming to %s: %w", localPath, err)
	}

	// Write ETag sidecar.
	if err := os.WriteFile(etagPath, []byte(remoteETag), 0o644); err != nil {
		return fmt.Errorf("writing etag sidecar: %w", err)
	}

	return nil
}

// isNotFound returns true if the error indicates the S3 object does not exist.
func isNotFound(err error) bool {
	// AWS SDK v2 structured error check.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		return code == "NotFound" || code == "NoSuchKey"
	}
	// Fallback for test fakes and non-AWS errors.
	msg := err.Error()
	return strings.Contains(msg, "NoSuchKey") || strings.Contains(msg, "NotFound")
}

// collectImagePaths returns deduplicated image paths from all services.
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
	if prefix == key {
		return "", nil
	}
	// Treat vmlinux-<major>.<minor> as an alias and resolve to the newest
	// local vmlinux-<major>.<minor>.* file when present.
	if len(strings.Split(prefix, ".")) != 2 {
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

	// Prefer symlink to avoid duplicating potentially large kernel files.
	if err := os.Symlink(filepath.Base(target), localPath); err == nil {
		return target, nil
	}

	// Fall back to copying when symlink creation is not possible.
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
