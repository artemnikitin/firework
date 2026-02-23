package store

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Store implements Store by reading node configs from an S3 bucket.
//
// Expected bucket layout:
//
//	nodes/<node-name>.yaml   — per-node service assignments (enriched)
//
// Change detection uses S3 ETags via HeadObject, which is very cheap
// compared to downloading the full object every poll cycle.
type S3Store struct {
	client *s3.Client
	bucket string
	prefix string

	mu       sync.Mutex
	lastETag string
	// lastEnrichmentByNode stores LastModified for each fetched node object.
	lastEnrichmentByNode map[string]time.Time
}

// S3StoreConfig holds options for creating an S3Store.
type S3StoreConfig struct {
	// Bucket is the S3 bucket name.
	Bucket string
	// Prefix is an optional key prefix (e.g. "configs/"). Include trailing slash.
	Prefix string
	// Region is the AWS region. If empty, it's resolved from the environment.
	Region string
	// EndpointURL overrides the S3 endpoint (useful for LocalStack/MinIO testing).
	EndpointURL string
}

// NewS3Store creates a new S3Store. AWS credentials are resolved from the
// standard chain (env vars, instance profile, shared config, etc.).
func NewS3Store(ctx context.Context, cfg S3StoreConfig) (*S3Store, error) {
	var opts []func(*awsconfig.LoadOptions) error

	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	var s3Opts []func(*s3.Options)
	if cfg.EndpointURL != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.EndpointURL)
			o.UsePathStyle = true
		})
	}

	client := s3.NewFromConfig(awsCfg, s3Opts...)

	return &S3Store{
		client:               client,
		bucket:               cfg.Bucket,
		prefix:               cfg.Prefix,
		lastEnrichmentByNode: make(map[string]time.Time),
	}, nil
}

// NewS3StoreFromClient creates an S3Store with a pre-configured S3 client.
// This is useful for testing.
func NewS3StoreFromClient(client *s3.Client, bucket, prefix string) *S3Store {
	return &S3Store{
		client:               client,
		bucket:               bucket,
		prefix:               prefix,
		lastEnrichmentByNode: make(map[string]time.Time),
	}
}

// Fetch downloads the node config from s3://<bucket>/<prefix>nodes/<nodeName>.yaml.
func (s *S3Store) Fetch(ctx context.Context, nodeName string) ([]byte, error) {
	key := s.nodeKey(nodeName)

	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("fetching s3://%s/%s: %w", s.bucket, key, err)
	}
	defer out.Body.Close()

	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("reading s3://%s/%s body: %w", s.bucket, key, err)
	}

	// Update the cached ETag from the GetObject response.
	s.mu.Lock()
	defer s.mu.Unlock()
	if out.ETag != nil {
		s.lastETag = *out.ETag
	}
	if out.LastModified != nil {
		if s.lastEnrichmentByNode == nil {
			s.lastEnrichmentByNode = make(map[string]time.Time)
		}
		s.lastEnrichmentByNode[nodeName] = out.LastModified.UTC()
	}

	return data, nil
}

// Revision returns the S3 ETag of the node config object. The agent calls
// this before Fetch to check if the config has changed. A HeadObject call
// is much cheaper than a full GetObject.
//
// Note: Because the ETag is per-object and we need a node name to check,
// we return the last known ETag cached from the most recent Fetch or
// Revision call. On the first call (before any Fetch), we return "" to
// force an initial Fetch.
func (s *S3Store) Revision(ctx context.Context) (string, error) {
	s.mu.Lock()
	lastETag := s.lastETag
	s.mu.Unlock()

	// On the first call we don't have a node name context, so we return
	// empty to force a Fetch. After the first Fetch, lastETag is populated.
	if lastETag == "" {
		return "", nil
	}

	return lastETag, nil
}

// CheckRevision does a HeadObject for a specific node to get the current
// ETag without downloading the full object. This is the preferred way to
// detect changes cheaply.
func (s *S3Store) CheckRevision(ctx context.Context, nodeName string) (string, error) {
	key := s.nodeKey(nodeName)

	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return "", fmt.Errorf("head s3://%s/%s: %w", s.bucket, key, err)
	}

	etag := ""
	if out.ETag != nil {
		etag = *out.ETag
	}

	s.mu.Lock()
	s.lastETag = etag
	s.mu.Unlock()

	return etag, nil
}

// LastEnrichmentTimestamp returns the last object LastModified observed during Fetch.
func (s *S3Store) LastEnrichmentTimestamp(nodeName string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.lastEnrichmentByNode[nodeName]
	return t, ok
}

// Close is a no-op for S3Store (no persistent resources to release).
func (s *S3Store) Close() error {
	return nil
}

// ListAllNodeConfigs fetches and parses all node config objects under the
// nodes/ prefix. Objects that fail to fetch or parse are skipped with a
// warning log. Implements store.NodeConfigLister.
func (s *S3Store) ListAllNodeConfigs(ctx context.Context) ([]config.NodeConfig, error) {
	prefix := s.prefix + "nodes/"
	var configs []config.NodeConfig
	var continuationToken *string

	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return nil, fmt.Errorf("listing s3://%s/%s: %w", s.bucket, prefix, err)
		}

		for _, obj := range out.Contents {
			if obj.Key == nil || !strings.HasSuffix(*obj.Key, ".yaml") {
				continue
			}
			getOut, err := s.client.GetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String(s.bucket),
				Key:    obj.Key,
			})
			if err != nil {
				slog.Warn("failed to fetch node config from S3", "key", *obj.Key, "error", err)
				continue
			}
			data, readErr := io.ReadAll(getOut.Body)
			getOut.Body.Close()
			if readErr != nil {
				slog.Warn("failed to read node config body", "key", *obj.Key, "error", readErr)
				continue
			}
			nc, parseErr := config.ParseNodeConfig(data)
			if parseErr != nil {
				slog.Warn("failed to parse node config", "key", *obj.Key, "error", parseErr)
				continue
			}
			configs = append(configs, nc)
		}

		if !aws.ToBool(out.IsTruncated) {
			break
		}
		continuationToken = out.NextContinuationToken
	}

	return configs, nil
}

// nodeKey returns the full S3 key for a node's config file.
func (s *S3Store) nodeKey(nodeName string) string {
	return s.prefix + "nodes/" + nodeName + ".yaml"
}
