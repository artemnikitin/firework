// Package objectstorage provides provider-neutral object storage primitives.
package objectstorage

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

// WriteToken is an opaque handle for conditional writes. S3 implementations
// store ETags and GCS implementations store generation numbers as strings.
// Callers may compare tokens, but must not parse or construct them.
type WriteToken string

// WriteTokenAbsent means that an object must not already exist.
const WriteTokenAbsent WriteToken = ""

// ErrNotFound is returned by Get when an object does not exist.
var ErrNotFound = errors.New("object not found")

// BlobMeta describes an object. WriteToken is both the conditional-write token
// and the cache/change token used by image sync and revision checks.
type BlobMeta struct {
	WriteToken   WriteToken
	LastModified time.Time
	Size         int64
}

// PutOptions controls object writes.
type PutOptions struct {
	ContentType string
}

// BlobStore is the object storage contract shared by agent config, image sync,
// and control-plane state.
type BlobStore interface {
	io.Closer
	Head(ctx context.Context, key string) (BlobMeta, bool, error)
	Get(ctx context.Context, key string) (io.ReadCloser, BlobMeta, error)
	GetBytes(ctx context.Context, key string) ([]byte, BlobMeta, bool, error)
	Put(ctx context.Context, key string, r io.Reader, opts PutOptions) (BlobMeta, error)
	PutIfAbsent(ctx context.Context, key string, r io.Reader, opts PutOptions) (bool, BlobMeta, error)
	PutIfMatch(ctx context.Context, key string, expected WriteToken, r io.Reader, opts PutOptions) (bool, BlobMeta, error)
	Delete(ctx context.Context, key string) error
	ListKeys(ctx context.Context, prefix string) ([]string, error)
}

// JoinKey joins object-key components without introducing duplicate slashes.
// A trailing slash on suffix is preserved.
func JoinKey(prefix, suffix string) string {
	if prefix == "" {
		return strings.TrimPrefix(suffix, "/")
	}
	if suffix == "" {
		return strings.TrimSuffix(prefix, "/")
	}
	return strings.TrimSuffix(prefix, "/") + "/" + strings.TrimPrefix(suffix, "/")
}
