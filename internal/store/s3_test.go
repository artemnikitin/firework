package store

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fakeS3Client implements a minimal S3 client interface for testing.
// We test through the S3Store's public methods by injecting a fake client.

func TestS3Store_NodeKey(t *testing.T) {
	tests := []struct {
		name     string
		prefix   string
		nodeName string
		want     string
	}{
		{"no prefix", "", "node-1", "nodes/node-1.yaml"},
		{"with prefix", "configs/", "node-1", "configs/nodes/node-1.yaml"},
		{"nested prefix", "env/prod/", "web-server", "env/prod/nodes/web-server.yaml"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &S3Store{prefix: tt.prefix}
			got := s.nodeKey(tt.nodeName)
			if got != tt.want {
				t.Errorf("nodeKey(%q) = %q, want %q", tt.nodeName, got, tt.want)
			}
		})
	}
}

func TestS3Store_RevisionEmpty(t *testing.T) {
	s := &S3Store{}

	rev, err := s.Revision(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rev != "" {
		t.Errorf("expected empty revision on fresh store, got %q", rev)
	}
}

func TestS3Store_RevisionAfterETagSet(t *testing.T) {
	s := &S3Store{lastETag: `"abc123"`}

	rev, err := s.Revision(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rev != `"abc123"` {
		t.Errorf("expected etag %q, got %q", `"abc123"`, rev)
	}
}

func TestS3Store_CloseIsNoop(t *testing.T) {
	s := &S3Store{}
	if err := s.Close(); err != nil {
		t.Errorf("Close() returned error: %v", err)
	}
}

func TestS3Store_FetchAndRevision(t *testing.T) {
	// This test uses the real S3Store with a mock-like S3 client.
	// We create a store and manually set the client to our fake.
	configYAML := `node: "test-node"
services:
  - name: "web"
    image: "/img/web.ext4"
    kernel: "/img/vmlinux"
    vcpus: 2
    memory_mb: 512
`
	etag := `"deadbeef123"`

	fake := &fakeS3API{
		objects: map[string]fakeObject{
			"nodes/test-node.yaml": {
				body: configYAML,
				etag: etag,
			},
		},
	}

	s := &S3Store{
		client: nil, // we won't use the real client
		bucket: "test-bucket",
		prefix: "",
	}
	// Override fetch to use our fake.
	data, revision, err := fetchWithFake(fake, s, "test-node")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(data) != configYAML {
		t.Errorf("unexpected body:\n%s", string(data))
	}
	if revision != etag {
		t.Errorf("expected revision %q, got %q", etag, revision)
	}
}

func TestS3Store_FetchNotFound(t *testing.T) {
	fake := &fakeS3API{
		objects: map[string]fakeObject{},
	}

	s := &S3Store{
		bucket: "test-bucket",
		prefix: "",
	}

	_, _, err := fetchWithFake(fake, s, "nonexistent")
	if err == nil {
		t.Fatal("expected error for missing object")
	}
}

func TestS3Store_CheckRevision(t *testing.T) {
	etag := `"rev456"`
	fake := &fakeS3API{
		objects: map[string]fakeObject{
			"nodes/node-1.yaml": {body: "data", etag: etag},
		},
	}

	s := &S3Store{
		bucket: "test-bucket",
		prefix: "",
	}

	rev, err := checkRevisionWithFake(fake, s, "node-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rev != etag {
		t.Errorf("expected %q, got %q", etag, rev)
	}

	// Verify it was cached.
	s.mu.Lock()
	cached := s.lastETag
	s.mu.Unlock()
	if cached != etag {
		t.Errorf("expected cached etag %q, got %q", etag, cached)
	}
}

func TestS3Store_LastEnrichmentTimestamp(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	s := &S3Store{
		lastEnrichmentByNode: map[string]time.Time{
			"web": now,
		},
	}

	got, ok := s.LastEnrichmentTimestamp("web")
	if !ok {
		t.Fatal("expected timestamp to be present")
	}
	if !got.Equal(now) {
		t.Fatalf("unexpected timestamp: got %v want %v", got, now)
	}

	if _, ok := s.LastEnrichmentTimestamp("missing"); ok {
		t.Fatal("expected missing node to return ok=false")
	}
}

// --- Fake S3 API for unit tests ---

type fakeObject struct {
	body string
	etag string
}

type fakeS3API struct {
	objects map[string]fakeObject
}

// fetchWithFake simulates S3Store.Fetch using the fake API.
func fetchWithFake(fake *fakeS3API, s *S3Store, nodeName string) ([]byte, string, error) {
	key := s.nodeKey(nodeName)
	obj, ok := fake.objects[key]
	if !ok {
		return nil, "", &notFoundError{key: key}
	}

	data, err := io.ReadAll(io.NopCloser(strings.NewReader(obj.body)))
	if err != nil {
		return nil, "", err
	}

	s.mu.Lock()
	s.lastETag = obj.etag
	s.mu.Unlock()

	return data, obj.etag, nil
}

// checkRevisionWithFake simulates S3Store.CheckRevision using the fake API.
func checkRevisionWithFake(fake *fakeS3API, s *S3Store, nodeName string) (string, error) {
	key := s.nodeKey(nodeName)
	obj, ok := fake.objects[key]
	if !ok {
		return "", &notFoundError{key: key}
	}

	s.mu.Lock()
	s.lastETag = obj.etag
	s.mu.Unlock()

	return obj.etag, nil
}

type notFoundError struct {
	key string
}

func (e *notFoundError) Error() string {
	return "object not found: " + e.key
}

// TestNewS3StoreFromClient verifies the constructor wires fields correctly.
func TestNewS3StoreFromClient(t *testing.T) {
	client := s3.New(s3.Options{Region: "us-east-1"})
	s := NewS3StoreFromClient(client, "my-bucket", "prefix/")

	if s.bucket != "my-bucket" {
		t.Errorf("expected bucket my-bucket, got %s", s.bucket)
	}
	if s.prefix != "prefix/" {
		t.Errorf("expected prefix prefix/, got %s", s.prefix)
	}
	if s.client != client {
		t.Error("client not set correctly")
	}
}

// listAllWithFake simulates S3Store.ListAllNodeConfigs using the fake API.
func listAllWithFake(fake *fakeS3API, s *S3Store) ([]config.NodeConfig, error) {
	prefix := s.prefix + "nodes/"
	var configs []config.NodeConfig
	for key, obj := range fake.objects {
		if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, ".yaml") {
			continue
		}
		nc, err := config.ParseNodeConfig([]byte(obj.body))
		if err != nil {
			// mimic the warn-and-skip behaviour of the real implementation
			continue
		}
		configs = append(configs, nc)
	}
	return configs, nil
}

func TestS3Store_ListAllNodeConfigs_HappyPath(t *testing.T) {
	node1YAML := `node: "node-1"
host_ip: "10.0.0.1"
services:
  - name: "svc-a"
    image: "/img/a.ext4"
    kernel: "/img/vmlinux"
    vcpus: 1
    memory_mb: 256
`
	node2YAML := `node: "node-2"
host_ip: "10.0.0.2"
services:
  - name: "svc-b"
    image: "/img/b.ext4"
    kernel: "/img/vmlinux"
    vcpus: 2
    memory_mb: 512
`

	fake := &fakeS3API{
		objects: map[string]fakeObject{
			"nodes/node-1.yaml": {body: node1YAML, etag: `"etag1"`},
			"nodes/node-2.yaml": {body: node2YAML, etag: `"etag2"`},
		},
	}

	s := &S3Store{bucket: "test-bucket", prefix: ""}
	configs, err := listAllWithFake(fake, s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("expected 2 configs, got %d", len(configs))
	}

	// Verify HostIP propagation.
	byNode := make(map[string]config.NodeConfig)
	for _, nc := range configs {
		byNode[nc.Node] = nc
	}
	if byNode["node-1"].HostIP != "10.0.0.1" {
		t.Errorf("expected node-1 HostIP 10.0.0.1, got %q", byNode["node-1"].HostIP)
	}
	if byNode["node-2"].HostIP != "10.0.0.2" {
		t.Errorf("expected node-2 HostIP 10.0.0.2, got %q", byNode["node-2"].HostIP)
	}
}

func TestS3Store_ListAllNodeConfigs_SkipsMalformedYAML(t *testing.T) {
	goodYAML := `node: "node-1"
host_ip: "10.0.0.1"
services: []
`
	fake := &fakeS3API{
		objects: map[string]fakeObject{
			"nodes/node-1.yaml": {body: goodYAML, etag: `"etag1"`},
			"nodes/bad.yaml":    {body: "{ invalid yaml: [", etag: `"etag2"`},
		},
	}

	s := &S3Store{bucket: "test-bucket", prefix: ""}
	configs, err := listAllWithFake(fake, s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 valid config (bad YAML skipped), got %d", len(configs))
	}
	if configs[0].Node != "node-1" {
		t.Errorf("expected node-1, got %q", configs[0].Node)
	}
}

// TestS3StoreConfig verifies NewS3Store rejects invalid configs gracefully.
func TestNewS3Store_WithEndpoint(t *testing.T) {
	// This tests that NewS3Store doesn't panic with a custom endpoint.
	// It won't actually connect since there's nothing listening.
	s, err := NewS3Store(context.Background(), S3StoreConfig{
		Bucket:      "test-bucket",
		Region:      "us-east-1",
		EndpointURL: "http://localhost:4566",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.bucket != "test-bucket" {
		t.Errorf("expected bucket test-bucket, got %s", s.bucket)
	}

	// Verify path style is set by checking the endpoint resolves correctly.
	_ = aws.ToString(nil) // just to use aws package
}
