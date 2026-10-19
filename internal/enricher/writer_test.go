package enricher

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestS3Writer_NodeKey(t *testing.T) {
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
			w := &S3Writer{prefix: tt.prefix}
			got := w.nodeKey(tt.nodeName)
			if got != tt.want {
				t.Errorf("nodeKey(%q) = %q, want %q", tt.nodeName, got, tt.want)
			}
		})
	}
}

type fakeWriterS3 struct {
	existingKeys []string
	putKeys      []string
	putBodies    map[string][]byte
	failPutKey   string
}

func (f *fakeWriterS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	key := aws.ToString(in.Key)
	if key == f.failPutKey {
		return nil, fmt.Errorf("put failed for %s", key)
	}
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	if f.putBodies == nil {
		f.putBodies = make(map[string][]byte)
	}
	f.putKeys = append(f.putKeys, key)
	f.putBodies[key] = body
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeWriterS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	prefix := aws.ToString(in.Prefix)
	var contents []types.Object
	for _, key := range f.existingKeys {
		if strings.HasPrefix(key, prefix) {
			contents = append(contents, types.Object{Key: aws.String(key)})
		}
	}
	return &s3.ListObjectsV2Output{
		Contents:    contents,
		IsTruncated: aws.Bool(false),
	}, nil
}

func requireEmptyNodeConfig(t *testing.T, fake *fakeWriterS3, key, node string) {
	t.Helper()
	body, ok := fake.putBodies[key]
	if !ok {
		t.Fatalf("expected %s to be overwritten with an empty config, got puts %v", key, fake.putKeys)
	}
	nc, err := config.ParseNodeConfig(body)
	if err != nil {
		t.Fatalf("%s is not an authoritative empty config: %v\n%s", key, err, body)
	}
	if nc.Node != node || len(nc.Services) != 0 {
		t.Fatalf("%s is not empty for node %s: %#v", key, node, nc)
	}
}

// A node type omitted by a successful enrichment run lost its last service.
// Deleting its config left the agent treating the missing file as a fetch
// failure and running that service forever, so it is emptied instead.
func TestS3Writer_WriteAll_EmptiesOmittedNodeConfigs(t *testing.T) {
	fake := &fakeWriterS3{
		existingKeys: []string{
			"cfg/nodes/web.yaml",
			"cfg/nodes/backend.yaml",
			"cfg/nodes/readme.txt",
			"other/preserve.yaml",
		},
	}
	w := newS3WriterWithClient(fake, "test-bucket", "cfg/")

	configs := []config.NodeConfig{
		{Node: "web", Services: []config.ServiceConfig{{Name: "web"}}},
		{Node: "api", Services: []config.ServiceConfig{}},
	}
	if err := w.WriteAll(context.Background(), configs); err != nil {
		t.Fatalf("WriteAll() error = %v", err)
	}

	for _, k := range []string{"cfg/nodes/web.yaml", "cfg/nodes/api.yaml"} {
		if !slices.Contains(fake.putKeys, k) {
			t.Fatalf("expected PutObject for key %q, got %v", k, fake.putKeys)
		}
	}
	requireEmptyNodeConfig(t, fake, "cfg/nodes/backend.yaml", "backend")
	for _, k := range []string{"cfg/nodes/readme.txt", "other/preserve.yaml"} {
		if slices.Contains(fake.putKeys, k) {
			t.Fatalf("non-node-config key %q was written", k)
		}
	}
}

// A successful run with no node types at all is a genuine "run nothing", and
// it empties every existing node config rather than being skipped.
func TestS3Writer_WriteAll_EmptyResultEmptiesEveryNodeConfig(t *testing.T) {
	fake := &fakeWriterS3{existingKeys: []string{"nodes/web.yaml", "nodes/api.yaml"}}
	w := newS3WriterWithClient(fake, "test-bucket", "")

	if err := w.WriteAll(context.Background(), nil); err != nil {
		t.Fatalf("WriteAll() error = %v", err)
	}
	requireEmptyNodeConfig(t, fake, "nodes/web.yaml", "web")
	requireEmptyNodeConfig(t, fake, "nodes/api.yaml", "api")
}

func TestS3Writer_WriteAll_EmptyingError(t *testing.T) {
	fake := &fakeWriterS3{
		existingKeys: []string{"nodes/stale.yaml"},
		failPutKey:   "nodes/stale.yaml",
	}
	w := newS3WriterWithClient(fake, "test-bucket", "")

	err := w.WriteAll(context.Background(), []config.NodeConfig{
		{Node: "web", Services: []config.ServiceConfig{}},
	})
	if err == nil {
		t.Fatal("expected an error emptying the omitted node config, got nil")
	}
}
