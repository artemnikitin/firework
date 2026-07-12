package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/objectstorage"
)

// S3StoreConfig configures the S3-backed agent config store.
type S3StoreConfig struct {
	Bucket      string
	Prefix      string
	Region      string
	EndpointURL string
}

// GCSStoreConfig configures the GCS-backed agent config store.
type GCSStoreConfig struct {
	Bucket          string
	Prefix          string
	CredentialsFile string
	Project         string
}

// blobStore implements Store, EnrichmentTimestampProvider, and
// NodeConfigLister over a provider-neutral object store.
type blobStore struct {
	store  objectstorage.BlobStore
	prefix string

	mu                   sync.Mutex
	lastToken            objectstorage.WriteToken
	lastEnrichmentByNode map[string]time.Time
}

// NewS3Store creates an S3-backed config store.
func NewS3Store(ctx context.Context, cfg S3StoreConfig) (*blobStore, error) {
	bs, err := objectstorage.NewS3BlobStore(ctx, objectstorage.S3Config{
		Bucket: cfg.Bucket, Region: cfg.Region, EndpointURL: cfg.EndpointURL,
		ForcePathStyle: cfg.EndpointURL != "",
	})
	if err != nil {
		return nil, err
	}
	return newBlobStore(bs, cfg.Prefix), nil
}

// NewGCSStore creates a native GCS-backed config store.
func NewGCSStore(ctx context.Context, cfg GCSStoreConfig) (*blobStore, error) {
	bs, err := objectstorage.NewGCSBlobStore(ctx, objectstorage.GCSConfig{
		Bucket: cfg.Bucket, Project: cfg.Project, CredentialsFile: cfg.CredentialsFile,
	})
	if err != nil {
		return nil, err
	}
	return newBlobStore(bs, cfg.Prefix), nil
}

func newBlobStore(store objectstorage.BlobStore, prefix string) *blobStore {
	return &blobStore{
		store: store, prefix: prefix,
		lastEnrichmentByNode: make(map[string]time.Time),
	}
}

// Fetch downloads a node config from <prefix>/nodes/<nodeName>.yaml.
func (s *blobStore) Fetch(ctx context.Context, nodeName string) ([]byte, error) {
	key := s.nodeKey(nodeName)
	data, meta, exists, err := s.store.GetBytes(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("fetching object %s: %w", key, err)
	}
	if !exists {
		return nil, fmt.Errorf("fetching object %s: %w", key, objectstorage.ErrNotFound)
	}

	s.mu.Lock()
	s.lastToken = meta.WriteToken
	if !meta.LastModified.IsZero() {
		s.lastEnrichmentByNode[nodeName] = meta.LastModified.UTC()
	}
	s.mu.Unlock()
	return data, nil
}

// Revision returns the last cache/change token observed for a fetched config.
func (s *blobStore) Revision(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.lastToken), nil
}

// CheckRevision retrieves the current cache/change token without downloading.
func (s *blobStore) CheckRevision(ctx context.Context, nodeName string) (string, error) {
	key := s.nodeKey(nodeName)
	meta, exists, err := s.store.Head(ctx, key)
	if err != nil {
		return "", fmt.Errorf("head object %s: %w", key, err)
	}
	if !exists {
		return "", fmt.Errorf("head object %s: %w", key, objectstorage.ErrNotFound)
	}
	s.mu.Lock()
	s.lastToken = meta.WriteToken
	s.mu.Unlock()
	return string(meta.WriteToken), nil
}

func (s *blobStore) LastEnrichmentTimestamp(nodeName string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.lastEnrichmentByNode[nodeName]
	return t, ok
}

func (s *blobStore) Close() error { return s.store.Close() }

// ListAllNodeConfigs returns every YAML node config under nodes/. It fails as a
// whole if any listed config cannot be read or parsed: omitting one peer would
// make the route sync treat that peer as removed and delete its live route.
func (s *blobStore) ListAllNodeConfigs(ctx context.Context) ([]config.NodeConfig, error) {
	prefix := objectstorage.JoinKey(s.prefix, "nodes/")
	keys, err := s.store.ListKeys(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("listing objects under %s: %w", prefix, err)
	}

	configs := make([]config.NodeConfig, 0, len(keys))
	for _, key := range keys {
		if !strings.HasSuffix(key, ".yaml") {
			continue
		}
		data, _, exists, err := s.store.GetBytes(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("fetching node config %s: %w", key, err)
		}
		if !exists {
			return nil, fmt.Errorf("fetching node config %s: %w", key, objectstorage.ErrNotFound)
		}
		nc, err := config.ParseNodeConfig(data)
		if err != nil {
			return nil, fmt.Errorf("parsing node config %s: %w", key, err)
		}
		configs = append(configs, nc)
	}
	return configs, nil
}

func (s *blobStore) nodeKey(nodeName string) string {
	return objectstorage.JoinKey(s.prefix, "nodes/"+nodeName+".yaml")
}
