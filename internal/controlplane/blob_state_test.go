package controlplane

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/objectstorage"
)

// memBlob is an in-memory objectstorage.BlobStore that models generation-style
// write tokens and conditional writes, enough to exercise blobStateStore.
type memBlob struct {
	objects map[string][]byte
	tokens  map[string]objectstorage.WriteToken
	counter int
}

func newMemBlob() *memBlob {
	return &memBlob{objects: map[string][]byte{}, tokens: map[string]objectstorage.WriteToken{}}
}

func (m *memBlob) nextToken() objectstorage.WriteToken {
	m.counter++
	return objectstorage.WriteToken(strconv.Itoa(m.counter))
}

func (m *memBlob) Head(_ context.Context, key string) (objectstorage.BlobMeta, bool, error) {
	data, ok := m.objects[key]
	if !ok {
		return objectstorage.BlobMeta{}, false, nil
	}
	return objectstorage.BlobMeta{WriteToken: m.tokens[key], Size: int64(len(data))}, true, nil
}

func (m *memBlob) Get(_ context.Context, key string) (io.ReadCloser, objectstorage.BlobMeta, error) {
	data, ok := m.objects[key]
	if !ok {
		return nil, objectstorage.BlobMeta{}, objectstorage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), objectstorage.BlobMeta{WriteToken: m.tokens[key], Size: int64(len(data))}, nil
}

func (m *memBlob) GetBytes(_ context.Context, key string) ([]byte, objectstorage.BlobMeta, bool, error) {
	data, ok := m.objects[key]
	if !ok {
		return nil, objectstorage.BlobMeta{}, false, nil
	}
	return data, objectstorage.BlobMeta{WriteToken: m.tokens[key], Size: int64(len(data))}, true, nil
}

func (m *memBlob) put(key string, r io.Reader) (objectstorage.BlobMeta, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return objectstorage.BlobMeta{}, err
	}
	token := m.nextToken()
	m.objects[key] = data
	m.tokens[key] = token
	return objectstorage.BlobMeta{WriteToken: token, Size: int64(len(data))}, nil
}

func (m *memBlob) Put(_ context.Context, key string, r io.Reader, _ objectstorage.PutOptions) (objectstorage.BlobMeta, error) {
	return m.put(key, r)
}

func (m *memBlob) PutIfAbsent(_ context.Context, key string, r io.Reader, _ objectstorage.PutOptions) (bool, objectstorage.BlobMeta, error) {
	if _, exists := m.objects[key]; exists {
		return false, objectstorage.BlobMeta{}, nil
	}
	meta, err := m.put(key, r)
	return err == nil, meta, err
}

func (m *memBlob) PutIfMatch(_ context.Context, key string, expected objectstorage.WriteToken, r io.Reader, _ objectstorage.PutOptions) (bool, objectstorage.BlobMeta, error) {
	if m.tokens[key] != expected {
		return false, objectstorage.BlobMeta{}, nil
	}
	meta, err := m.put(key, r)
	return err == nil, meta, err
}

func (m *memBlob) Delete(_ context.Context, key string) error {
	delete(m.objects, key)
	delete(m.tokens, key)
	return nil
}

func (m *memBlob) ListKeys(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	for key := range m.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (m *memBlob) Close() error { return nil }

type lease struct {
	Holder string `json:"holder"`
}

func TestBlobStateStoreJSONRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newBlobStateStore(newMemBlob())

	if _, exists, err := s.GetJSON(ctx, "missing.json", &lease{}); err != nil || exists {
		t.Fatalf("GetJSON missing = exists %v, err %v", exists, err)
	}

	putToken, err := s.PutJSON(ctx, "cp/v1/lease.json", lease{Holder: "one"})
	if err != nil || putToken == "" {
		t.Fatalf("PutJSON = token %q, err %v", putToken, err)
	}

	var got lease
	getToken, exists, err := s.GetJSON(ctx, "cp/v1/lease.json", &got)
	if err != nil || !exists || got.Holder != "one" || getToken != putToken {
		t.Fatalf("GetJSON = %#v, token %q, exists %v, err %v", got, getToken, exists, err)
	}
}

func TestBlobStateStoreConditionalJSON(t *testing.T) {
	ctx := context.Background()
	s := newBlobStateStore(newMemBlob())

	ok, initial, err := s.PutJSONIfAbsent(ctx, "lease.json", lease{Holder: "one"})
	if err != nil || !ok || initial == "" {
		t.Fatalf("first PutJSONIfAbsent = ok %v, token %q, err %v", ok, initial, err)
	}
	ok, _, err = s.PutJSONIfAbsent(ctx, "lease.json", lease{Holder: "two"})
	if err != nil || ok {
		t.Fatalf("second PutJSONIfAbsent = ok %v, err %v", ok, err)
	}

	ok, updated, err := s.PutJSONIfMatch(ctx, "lease.json", initial, lease{Holder: "two"})
	if err != nil || !ok || updated == initial {
		t.Fatalf("matching PutJSONIfMatch = ok %v, token %q, err %v", ok, updated, err)
	}
	ok, _, err = s.PutJSONIfMatch(ctx, "lease.json", initial, lease{Holder: "stale"})
	if err != nil || ok {
		t.Fatalf("stale PutJSONIfMatch = ok %v, err %v", ok, err)
	}
}

func TestBlobStateStoreGetJSONDecodeError(t *testing.T) {
	ctx := context.Background()
	s := newBlobStateStore(newMemBlob())
	if _, err := s.PutRaw(ctx, "bad.json", []byte("{not json"), "application/json"); err != nil {
		t.Fatalf("PutRaw: %v", err)
	}
	if _, _, err := s.GetJSON(ctx, "bad.json", &lease{}); err == nil {
		t.Fatal("expected decode error for invalid JSON")
	}
}

func TestBlobStateStoreRawAndList(t *testing.T) {
	ctx := context.Background()
	s := newBlobStateStore(newMemBlob())
	if _, err := s.PutRaw(ctx, "cp/v1/nodes/a.yaml", []byte("node: a"), "text/yaml"); err != nil {
		t.Fatalf("PutRaw a: %v", err)
	}
	if _, err := s.PutRaw(ctx, "cp/v1/nodes/b.yaml", []byte("node: b"), "text/yaml"); err != nil {
		t.Fatalf("PutRaw b: %v", err)
	}

	data, _, exists, err := s.GetRaw(ctx, "cp/v1/nodes/a.yaml")
	if err != nil || !exists || string(data) != "node: a" {
		t.Fatalf("GetRaw = data %q, exists %v, err %v", data, exists, err)
	}

	keys, err := s.ListKeys(ctx, "cp/v1/nodes/")
	if err != nil || len(keys) != 2 {
		t.Fatalf("ListKeys = %v, err %v", keys, err)
	}

	if err := s.Delete(ctx, "cp/v1/nodes/a.yaml"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, exists, _ := s.GetRaw(ctx, "cp/v1/nodes/a.yaml"); exists {
		t.Fatal("expected a.yaml to be deleted")
	}
}
