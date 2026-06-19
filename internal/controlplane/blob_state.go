package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/artemnikitin/firework/internal/objectstorage"
)

// StateStore is the provider-neutral control-plane state contract.
type StateStore interface {
	GetJSON(ctx context.Context, key string, out any) (objectstorage.WriteToken, bool, error)
	GetRaw(ctx context.Context, key string) ([]byte, objectstorage.WriteToken, bool, error)
	PutJSON(ctx context.Context, key string, value any) (objectstorage.WriteToken, error)
	PutJSONIfAbsent(ctx context.Context, key string, value any) (bool, objectstorage.WriteToken, error)
	PutJSONIfMatch(ctx context.Context, key string, expected objectstorage.WriteToken, value any) (bool, objectstorage.WriteToken, error)
	PutRaw(ctx context.Context, key string, data []byte, contentType string) (objectstorage.WriteToken, error)
	PutRawIfAbsent(ctx context.Context, key string, data []byte, contentType string) (bool, objectstorage.WriteToken, error)
	PutRawIfMatch(ctx context.Context, key string, expected objectstorage.WriteToken, data []byte, contentType string) (bool, objectstorage.WriteToken, error)
	Delete(ctx context.Context, key string) error
	ListKeys(ctx context.Context, prefix string) ([]string, error)
	Close() error
}

type blobStateStore struct {
	store objectstorage.BlobStore
}

// NewS3StateStore creates an S3-backed state store.
func NewS3StateStore(ctx context.Context, cfg S3StateConfig) (StateStore, error) {
	store, err := objectstorage.NewS3BlobStore(ctx, objectstorage.S3Config{
		Bucket: cfg.Bucket, Region: cfg.Region, EndpointURL: cfg.EndpointURL,
		ForcePathStyle: cfg.ForcePathStyle,
	})
	if err != nil {
		return nil, err
	}
	return &blobStateStore{store: store}, nil
}

// NewGCSStateStore creates a native GCS-backed state store.
func NewGCSStateStore(ctx context.Context, cfg GCSStateConfig) (StateStore, error) {
	store, err := objectstorage.NewGCSBlobStore(ctx, objectstorage.GCSConfig{
		Bucket: cfg.Bucket, Project: cfg.Project, CredentialsFile: cfg.CredentialsFile,
	})
	if err != nil {
		return nil, err
	}
	return &blobStateStore{store: store}, nil
}

func newBlobStateStore(store objectstorage.BlobStore) StateStore {
	return &blobStateStore{store: store}
}

func (s *blobStateStore) GetJSON(ctx context.Context, key string, out any) (objectstorage.WriteToken, bool, error) {
	data, token, exists, err := s.GetRaw(ctx, key)
	if err != nil || !exists {
		return token, exists, err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return "", false, fmt.Errorf("decode JSON object %s: %w", key, err)
	}
	return token, true, nil
}

func (s *blobStateStore) GetRaw(ctx context.Context, key string) ([]byte, objectstorage.WriteToken, bool, error) {
	data, meta, exists, err := s.store.GetBytes(ctx, key)
	if err != nil {
		return nil, "", false, err
	}
	return data, meta.WriteToken, exists, nil
}

func (s *blobStateStore) PutJSON(ctx context.Context, key string, value any) (objectstorage.WriteToken, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal JSON: %w", err)
	}
	return s.PutRaw(ctx, key, data, "application/json")
}

func (s *blobStateStore) PutJSONIfAbsent(ctx context.Context, key string, value any) (bool, objectstorage.WriteToken, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return false, "", fmt.Errorf("marshal JSON: %w", err)
	}
	return s.PutRawIfAbsent(ctx, key, data, "application/json")
}

func (s *blobStateStore) PutJSONIfMatch(ctx context.Context, key string, expected objectstorage.WriteToken, value any) (bool, objectstorage.WriteToken, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return false, "", fmt.Errorf("marshal JSON: %w", err)
	}
	return s.PutRawIfMatch(ctx, key, expected, data, "application/json")
}

func (s *blobStateStore) PutRaw(ctx context.Context, key string, data []byte, contentType string) (objectstorage.WriteToken, error) {
	meta, err := s.store.Put(ctx, key, bytes.NewReader(data), objectstorage.PutOptions{ContentType: contentType})
	return meta.WriteToken, err
}

func (s *blobStateStore) PutRawIfAbsent(ctx context.Context, key string, data []byte, contentType string) (bool, objectstorage.WriteToken, error) {
	ok, meta, err := s.store.PutIfAbsent(ctx, key, bytes.NewReader(data), objectstorage.PutOptions{ContentType: contentType})
	return ok, meta.WriteToken, err
}

func (s *blobStateStore) PutRawIfMatch(ctx context.Context, key string, expected objectstorage.WriteToken, data []byte, contentType string) (bool, objectstorage.WriteToken, error) {
	ok, meta, err := s.store.PutIfMatch(ctx, key, expected, bytes.NewReader(data), objectstorage.PutOptions{ContentType: contentType})
	return ok, meta.WriteToken, err
}

func (s *blobStateStore) Delete(ctx context.Context, key string) error {
	return s.store.Delete(ctx, key)
}

func (s *blobStateStore) ListKeys(ctx context.Context, prefix string) ([]string, error) {
	return s.store.ListKeys(ctx, prefix)
}

func (s *blobStateStore) Close() error { return s.store.Close() }
