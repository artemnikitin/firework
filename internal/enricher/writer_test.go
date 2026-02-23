package enricher

import (
	"context"
	"fmt"
	"slices"
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
	deleteKeys   []string
}

func (f *fakeWriterS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.putKeys = append(f.putKeys, aws.ToString(in.Key))
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeWriterS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	prefix := aws.ToString(in.Prefix)
	var contents []types.Object
	for _, key := range f.existingKeys {
		if len(prefix) == 0 || len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			contents = append(contents, types.Object{Key: aws.String(key)})
		}
	}
	return &s3.ListObjectsV2Output{
		Contents:    contents,
		IsTruncated: aws.Bool(false),
	}, nil
}

func (f *fakeWriterS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	key := aws.ToString(in.Key)
	f.deleteKeys = append(f.deleteKeys, key)
	return &s3.DeleteObjectOutput{}, nil
}

func TestS3Writer_WriteAll_DeletesStaleNodeConfigs(t *testing.T) {
	fake := &fakeWriterS3{
		existingKeys: []string{
			"nodes/web.yaml",
			"nodes/backend.yaml",
			"nodes/readme.txt",
			"other/preserve.yaml",
		},
	}
	w := newS3WriterWithClient(fake, "test-bucket", "")

	configs := []config.NodeConfig{
		{Node: "web", Services: []config.ServiceConfig{}},
		{Node: "api", Services: []config.ServiceConfig{}},
	}

	if err := w.WriteAll(context.Background(), configs); err != nil {
		t.Fatalf("WriteAll() error = %v", err)
	}

	wantPut := []string{"nodes/web.yaml", "nodes/api.yaml"}
	for _, k := range wantPut {
		if !slices.Contains(fake.putKeys, k) {
			t.Fatalf("expected PutObject for key %q, got %v", k, fake.putKeys)
		}
	}

	if len(fake.deleteKeys) != 1 || fake.deleteKeys[0] != "nodes/backend.yaml" {
		t.Fatalf("expected one stale delete for nodes/backend.yaml, got %v", fake.deleteKeys)
	}
}

type failingDeleteWriterS3 struct {
	fakeWriterS3
	failKey string
}

func (f *failingDeleteWriterS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	key := aws.ToString(in.Key)
	f.deleteKeys = append(f.deleteKeys, key)
	if key == f.failKey {
		return nil, fmt.Errorf("delete failed for %s", key)
	}
	return &s3.DeleteObjectOutput{}, nil
}

func TestS3Writer_WriteAll_DeleteError(t *testing.T) {
	fake := &failingDeleteWriterS3{
		fakeWriterS3: fakeWriterS3{
			existingKeys: []string{"nodes/stale.yaml"},
		},
		failKey: "nodes/stale.yaml",
	}
	w := newS3WriterWithClient(fake, "test-bucket", "")

	err := w.WriteAll(context.Background(), []config.NodeConfig{
		{Node: "web", Services: []config.ServiceConfig{}},
	})
	if err == nil {
		t.Fatal("expected delete error, got nil")
	}
}
