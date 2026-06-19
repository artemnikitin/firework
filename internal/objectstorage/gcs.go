package objectstorage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// GCSConfig configures a native GCS BlobStore.
type GCSConfig struct {
	Bucket          string
	Project         string
	CredentialsFile string
}

type gcsBlobStore struct {
	client *storage.Client
	bucket *storage.BucketHandle
}

// NewGCSBlobStore creates a native GCS-backed BlobStore using Application
// Default Credentials unless CredentialsFile is set.
func NewGCSBlobStore(ctx context.Context, cfg GCSConfig) (BlobStore, error) {
	var opts []option.ClientOption
	if cfg.CredentialsFile != "" {
		opts = append(opts, option.WithCredentialsFile(cfg.CredentialsFile))
	}
	client, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating GCS client: %w", err)
	}
	return NewGCSBlobStoreFromClient(client, cfg.Bucket), nil
}

// NewGCSBlobStoreFromClient wraps a preconfigured GCS client.
func NewGCSBlobStoreFromClient(client *storage.Client, bucket string) BlobStore {
	return &gcsBlobStore{client: client, bucket: client.Bucket(bucket)}
}

func (s *gcsBlobStore) Head(ctx context.Context, key string) (BlobMeta, bool, error) {
	attrs, err := s.bucket.Object(key).Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return BlobMeta{}, false, nil
	}
	if err != nil {
		return BlobMeta{}, false, fmt.Errorf("head gs://%s/%s: %w", s.bucket.BucketName(), key, err)
	}
	return gcsMeta(attrs), true, nil
}

func (s *gcsBlobStore) Get(ctx context.Context, key string) (io.ReadCloser, BlobMeta, error) {
	r, err := s.bucket.Object(key).NewReader(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil, BlobMeta{}, fmt.Errorf("%w: gs://%s/%s", ErrNotFound, s.bucket.BucketName(), key)
	}
	if err != nil {
		return nil, BlobMeta{}, fmt.Errorf("get gs://%s/%s: %w", s.bucket.BucketName(), key, err)
	}
	return r, BlobMeta{
		WriteToken:   WriteToken(strconv.FormatInt(r.Attrs.Generation, 10)),
		LastModified: r.Attrs.LastModified.UTC(),
		Size:         r.Attrs.Size,
	}, nil
}

func (s *gcsBlobStore) GetBytes(ctx context.Context, key string) ([]byte, BlobMeta, bool, error) {
	r, meta, err := s.Get(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return nil, BlobMeta{}, false, nil
	}
	if err != nil {
		return nil, BlobMeta{}, false, err
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, BlobMeta{}, false, fmt.Errorf("read gs://%s/%s: %w", s.bucket.BucketName(), key, err)
	}
	return data, meta, true, nil
}

func (s *gcsBlobStore) Put(ctx context.Context, key string, r io.Reader, opts PutOptions) (BlobMeta, error) {
	meta, err := s.write(ctx, s.bucket.Object(key), r, opts, false)
	if err != nil {
		return BlobMeta{}, fmt.Errorf("write gs://%s/%s: %w", s.bucket.BucketName(), key, err)
	}
	return meta, nil
}

func (s *gcsBlobStore) PutIfAbsent(ctx context.Context, key string, r io.Reader, opts PutOptions) (bool, BlobMeta, error) {
	// The Go client represents the JSON API's ifGenerationMatch=0 condition
	// with DoesNotExist; GenerationMatch's zero value means "unset".
	obj := s.bucket.Object(key).If(storage.Conditions{DoesNotExist: true})
	meta, err := s.write(ctx, obj, r, opts, true)
	if gcsPreconditionFailure(err) {
		return false, BlobMeta{}, nil
	}
	if err != nil {
		return false, BlobMeta{}, fmt.Errorf("conditional write gs://%s/%s: %w", s.bucket.BucketName(), key, err)
	}
	return true, meta, nil
}

func (s *gcsBlobStore) PutIfMatch(ctx context.Context, key string, expected WriteToken, r io.Reader, opts PutOptions) (bool, BlobMeta, error) {
	generation, err := strconv.ParseInt(string(expected), 10, 64)
	if err != nil {
		return false, BlobMeta{}, fmt.Errorf("invalid GCS write token: %w", err)
	}
	obj := s.bucket.Object(key).If(storage.Conditions{GenerationMatch: generation})
	meta, err := s.write(ctx, obj, r, opts, true)
	if gcsPreconditionFailure(err) {
		return false, BlobMeta{}, nil
	}
	if err != nil {
		return false, BlobMeta{}, fmt.Errorf("conditional write gs://%s/%s: %w", s.bucket.BucketName(), key, err)
	}
	return true, meta, nil
}

func (s *gcsBlobStore) write(ctx context.Context, obj *storage.ObjectHandle, r io.Reader, opts PutOptions, conditional bool) (BlobMeta, error) {
	w := obj.NewWriter(ctx)
	if conditional {
		// CAS objects are small. Non-resumable uploads preserve generation
		// preconditions in fake-gcs-server and avoid misleading test failures.
		w.ChunkSize = 0
	}
	if opts.ContentType != "" {
		w.ContentType = opts.ContentType
	}
	if _, err := io.Copy(w, r); err != nil {
		_ = w.CloseWithError(err)
		return BlobMeta{}, err
	}
	if err := w.Close(); err != nil {
		return BlobMeta{}, err
	}
	return gcsMeta(w.Attrs()), nil
}

func (s *gcsBlobStore) Delete(ctx context.Context, key string) error {
	err := s.bucket.Object(key).Delete(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete gs://%s/%s: %w", s.bucket.BucketName(), key, err)
	}
	return nil
}

func (s *gcsBlobStore) ListKeys(ctx context.Context, prefix string) ([]string, error) {
	it := s.bucket.Objects(ctx, &storage.Query{Prefix: prefix})
	var keys []string
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("list gs://%s/%s: %w", s.bucket.BucketName(), prefix, err)
		}
		keys = append(keys, attrs.Name)
	}
	return keys, nil
}

func (s *gcsBlobStore) Close() error { return s.client.Close() }

func gcsMeta(attrs *storage.ObjectAttrs) BlobMeta {
	if attrs == nil {
		return BlobMeta{}
	}
	return BlobMeta{
		WriteToken:   WriteToken(strconv.FormatInt(attrs.Generation, 10)),
		LastModified: attrs.Updated.UTC(),
		Size:         attrs.Size,
	}
}

func gcsPreconditionFailure(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && (apiErr.Code == 409 || apiErr.Code == 412)
}
