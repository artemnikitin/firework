package objectstorage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/fsouza/fake-gcs-server/fakestorage"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
)

func newTestGCSStore(t *testing.T) BlobStore {
	t.Helper()
	server := fakestorage.NewServer(nil)
	t.Cleanup(server.Stop)
	server.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: "test-bucket"})
	store := NewGCSBlobStoreFromClient(server.Client(), "test-bucket")
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestGCSBlobStoreCRUD(t *testing.T) {
	ctx := context.Background()
	store := newTestGCSStore(t)

	if _, exists, err := store.Head(ctx, "missing"); err != nil || exists {
		t.Fatalf("Head missing = exists %v, err %v", exists, err)
	}
	if _, _, err := store.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing error = %v", err)
	}

	meta, err := store.Put(ctx, "configs/node.yaml", strings.NewReader("node: test"), PutOptions{ContentType: "text/yaml"})
	if err != nil || meta.WriteToken == "" {
		t.Fatalf("Put meta = %#v, err %v", meta, err)
	}

	r, getMeta, err := store.Get(ctx, "configs/node.yaml")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	data, readErr := io.ReadAll(r)
	closeErr := r.Close()
	if readErr != nil || closeErr != nil || string(data) != "node: test" {
		t.Fatalf("Get data = %q, read err %v, close err %v", data, readErr, closeErr)
	}
	if getMeta.WriteToken != meta.WriteToken {
		t.Fatalf("Get token = %q, want %q", getMeta.WriteToken, meta.WriteToken)
	}

	keys, err := store.ListKeys(ctx, "configs/")
	if err != nil || len(keys) != 1 || keys[0] != "configs/node.yaml" {
		t.Fatalf("ListKeys = %v, err %v", keys, err)
	}
	if err := store.Delete(ctx, "configs/node.yaml"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.Delete(ctx, "configs/node.yaml"); err != nil {
		t.Fatalf("Delete absent: %v", err)
	}
}

func TestGCSBlobStoreConditionalWrites(t *testing.T) {
	ctx := context.Background()
	store := newTestGCSStore(t)

	ok, initial, err := store.PutIfAbsent(ctx, "lock.json", strings.NewReader(`{"holder":"one"}`), PutOptions{ContentType: "application/json"})
	if err != nil || !ok || initial.WriteToken == "" {
		t.Fatalf("first PutIfAbsent = ok %v, meta %#v, err %v", ok, initial, err)
	}
	ok, _, err = store.PutIfAbsent(ctx, "lock.json", strings.NewReader(`{"holder":"two"}`), PutOptions{})
	if err != nil || ok {
		t.Fatalf("second PutIfAbsent = ok %v, err %v", ok, err)
	}

	ok, updated, err := store.PutIfMatch(ctx, "lock.json", initial.WriteToken, strings.NewReader(`{"holder":"two"}`), PutOptions{ContentType: "application/json"})
	if err != nil || !ok || updated.WriteToken == "" || updated.WriteToken == initial.WriteToken {
		t.Fatalf("matching PutIfMatch = ok %v, meta %#v, err %v", ok, updated, err)
	}
	ok, _, err = store.PutIfMatch(ctx, "lock.json", initial.WriteToken, strings.NewReader(`{"holder":"stale"}`), PutOptions{})
	if err != nil || ok {
		t.Fatalf("stale PutIfMatch = ok %v, err %v", ok, err)
	}

	data, meta, exists, err := store.GetBytes(ctx, "lock.json")
	if err != nil || !exists || string(data) != `{"holder":"two"}` || meta.WriteToken != updated.WriteToken {
		t.Fatalf("GetBytes = data %q, meta %#v, exists %v, err %v", data, meta, exists, err)
	}
}

func TestGCSBlobStoreConditionalDelete(t *testing.T) {
	ctx := context.Background()
	server := fakestorage.NewServer(nil)
	t.Cleanup(server.Stop)
	server.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: "test-bucket"})
	transport := &generationMatchTransport{next: server.HTTPClient().Transport}
	httpClient := server.HTTPClient()
	httpClient.Transport = transport
	client, err := storage.NewClient(ctx, option.WithHTTPClient(httpClient), option.WithCredentials(&google.Credentials{}))
	if err != nil {
		t.Fatal(err)
	}
	store := NewGCSBlobStoreFromClient(client, "test-bucket")
	t.Cleanup(func() { _ = store.Close() })
	meta, err := store.Put(ctx, "node.json", strings.NewReader(`{"generation":7}`), PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	transport.expectedGeneration = string(meta.WriteToken)
	wrong := WriteToken("999999999")
	deleted, err := store.DeleteIfMatch(ctx, "node.json", wrong)
	if err != nil || deleted {
		t.Fatalf("wrong-token delete = %v, %v; want conflict", deleted, err)
	}
	if _, exists, err := store.Head(ctx, "node.json"); err != nil || !exists {
		t.Fatalf("wrong-token delete removed object: exists %v, err %v", exists, err)
	}
	deleted, err = store.DeleteIfMatch(ctx, "node.json", meta.WriteToken)
	if err != nil || !deleted {
		t.Fatalf("matching delete = %v, %v", deleted, err)
	}
	deleted, err = store.DeleteIfMatch(ctx, "node.json", meta.WriteToken)
	if err != nil || deleted {
		t.Fatalf("missing-object delete = %v, %v; want conflict", deleted, err)
	}
}

// fake-gcs-server currently ignores delete preconditions. This transport
// checks the JSON API condition emitted by the Cloud Storage client and
// supplies the precondition response a conforming endpoint returns.
type generationMatchTransport struct {
	next               http.RoundTripper
	expectedGeneration string
}

func (t *generationMatchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodDelete && req.URL.Query().Get("ifGenerationMatch") != t.expectedGeneration {
		return &http.Response{
			StatusCode: http.StatusPreconditionFailed,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":412}}`)),
			Request:    req,
		}, nil
	}
	return t.next.RoundTrip(req)
}

func TestGCSBlobStoreRejectsInvalidWriteToken(t *testing.T) {
	store := newTestGCSStore(t)
	if _, _, err := store.PutIfMatch(context.Background(), "key", "not-a-generation", strings.NewReader("x"), PutOptions{}); err == nil {
		t.Fatal("expected invalid generation token error")
	}
}
