package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/objectstorage"
)

type fakeBlob struct {
	objects map[string]fakeBlobObject
	getErr  map[string]error
}

type fakeBlobObject struct {
	data []byte
	meta objectstorage.BlobMeta
}

func (f *fakeBlob) Head(_ context.Context, key string) (objectstorage.BlobMeta, bool, error) {
	obj, ok := f.objects[key]
	return obj.meta, ok, nil
}

func (f *fakeBlob) Get(_ context.Context, key string) (io.ReadCloser, objectstorage.BlobMeta, error) {
	obj, ok := f.objects[key]
	if !ok {
		return nil, objectstorage.BlobMeta{}, objectstorage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(obj.data)), obj.meta, nil
}

func (f *fakeBlob) GetBytes(_ context.Context, key string) ([]byte, objectstorage.BlobMeta, bool, error) {
	if err := f.getErr[key]; err != nil {
		return nil, objectstorage.BlobMeta{}, false, err
	}
	obj, ok := f.objects[key]
	return obj.data, obj.meta, ok, nil
}

func (f *fakeBlob) Put(context.Context, string, io.Reader, objectstorage.PutOptions) (objectstorage.BlobMeta, error) {
	return objectstorage.BlobMeta{}, fmt.Errorf("not implemented")
}

func (f *fakeBlob) PutIfAbsent(context.Context, string, io.Reader, objectstorage.PutOptions) (bool, objectstorage.BlobMeta, error) {
	return false, objectstorage.BlobMeta{}, fmt.Errorf("not implemented")
}

func (f *fakeBlob) PutIfMatch(context.Context, string, objectstorage.WriteToken, io.Reader, objectstorage.PutOptions) (bool, objectstorage.BlobMeta, error) {
	return false, objectstorage.BlobMeta{}, fmt.Errorf("not implemented")
}

func (f *fakeBlob) Delete(context.Context, string) error { return nil }

func (f *fakeBlob) ListKeys(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (f *fakeBlob) Close() error { return nil }

func TestBlobStoreFetchRevisionAndTimestamp(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	backend := &fakeBlob{objects: map[string]fakeBlobObject{
		"cp/v1/nodes/node-1.yaml": {
			data: []byte("node: node-1\nservices: []\n"),
			meta: objectstorage.BlobMeta{WriteToken: "generation-1", LastModified: now},
		},
	}}
	s := newBlobStore(backend, "cp/v1/")

	data, err := s.Fetch(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(data) != "node: node-1\nservices: []\n" {
		t.Fatalf("unexpected data: %q", data)
	}
	revision, err := s.Revision(context.Background())
	if err != nil || revision != "generation-1" {
		t.Fatalf("Revision = %q, %v", revision, err)
	}
	if got, ok := s.LastEnrichmentTimestamp("node-1"); !ok || !got.Equal(now) {
		t.Fatalf("timestamp = %v, %v", got, ok)
	}
}

func TestBlobStoreCheckRevision(t *testing.T) {
	backend := &fakeBlob{objects: map[string]fakeBlobObject{
		"nodes/node-1.yaml": {meta: objectstorage.BlobMeta{WriteToken: "token-2"}},
	}}
	s := newBlobStore(backend, "")
	got, err := s.CheckRevision(context.Background(), "node-1")
	if err != nil || got != "token-2" {
		t.Fatalf("CheckRevision = %q, %v", got, err)
	}
}

func TestBlobStoreListAllNodeConfigs(t *testing.T) {
	backend := &fakeBlob{objects: map[string]fakeBlobObject{
		"nodes/node-1.yaml": {data: []byte("node: node-1\nhost_ip: 10.0.0.1\nservices: []\n")},
		"nodes/node-2.yaml": {data: []byte("node: node-2\nhost_ip: 10.0.0.2\nservices: []\n")},
		"nodes/ignore.json": {data: []byte("{}")},
	}}
	s := newBlobStore(backend, "")
	configs, err := s.ListAllNodeConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListAllNodeConfigs: %v", err)
	}
	if len(configs) != 2 || configs[0].Node != "node-1" || configs[1].Node != "node-2" {
		t.Fatalf("unexpected configs: %#v", configs)
	}
}

func TestBlobStoreListAllNodeConfigsFailsOnIncompletePeerSet(t *testing.T) {
	tests := map[string]*fakeBlob{
		"read failure": {
			objects: map[string]fakeBlobObject{
				"nodes/node-1.yaml": {data: []byte("node: node-1\nservices: []\n")},
				"nodes/node-2.yaml": {data: []byte("node: node-2\nservices: []\n")},
			},
			getErr: map[string]error{"nodes/node-2.yaml": fmt.Errorf("transient object read failure")},
		},
		"invalid yaml": {
			objects: map[string]fakeBlobObject{
				"nodes/node-1.yaml": {data: []byte("node: node-1\nservices: []\n")},
				"nodes/node-2.yaml": {data: []byte("{bad")},
			},
		},
	}

	for name, backend := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := newBlobStore(backend, "").ListAllNodeConfigs(context.Background()); err == nil {
				t.Fatal("expected incomplete peer set to fail")
			}
		})
	}
}

func TestBlobStoreNodeKey(t *testing.T) {
	tests := map[string]string{
		"":          "nodes/node-1.yaml",
		"configs/":  "configs/nodes/node-1.yaml",
		"env/prod/": "env/prod/nodes/node-1.yaml",
	}
	for prefix, want := range tests {
		if got := newBlobStore(&fakeBlob{}, prefix).nodeKey("node-1"); got != want {
			t.Errorf("prefix %q: got %q, want %q", prefix, got, want)
		}
	}
}
