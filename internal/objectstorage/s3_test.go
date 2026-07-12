package objectstorage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// fakeS3API is an in-memory s3API implementation that models ETag generation
// and the conditional-write semantics the blob store relies on.
type fakeS3API struct {
	objects map[string]fakeS3Object
	counter int
}

type fakeS3Object struct {
	data         []byte
	etag         string
	lastModified time.Time
}

func newFakeS3API() *fakeS3API {
	return &fakeS3API{objects: map[string]fakeS3Object{}}
}

func (f *fakeS3API) nextETag() string {
	f.counter++
	return fmt.Sprintf("\"etag-%d\"", f.counter)
}

func apiErr(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: code}
}

func (f *fakeS3API) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	obj, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, apiErr("NotFound")
	}
	return &s3.HeadObjectOutput{
		ETag:          aws.String(obj.etag),
		LastModified:  aws.Time(obj.lastModified),
		ContentLength: aws.Int64(int64(len(obj.data))),
	}, nil
}

func (f *fakeS3API) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	obj, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, apiErr("NoSuchKey")
	}
	return &s3.GetObjectOutput{
		Body:          io.NopCloser(bytes.NewReader(obj.data)),
		ETag:          aws.String(obj.etag),
		LastModified:  aws.Time(obj.lastModified),
		ContentLength: aws.Int64(int64(len(obj.data))),
	}, nil
}

func (f *fakeS3API) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	key := aws.ToString(in.Key)
	existing, exists := f.objects[key]
	if aws.ToString(in.IfNoneMatch) == "*" && exists {
		return nil, apiErr("PreconditionFailed")
	}
	if in.IfMatch != nil && (!exists || existing.etag != aws.ToString(in.IfMatch)) {
		return nil, apiErr("PreconditionFailed")
	}
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	etag := f.nextETag()
	f.objects[key] = fakeS3Object{data: data, etag: etag, lastModified: time.Now().UTC()}
	return &s3.PutObjectOutput{ETag: aws.String(etag)}, nil
}

func (f *fakeS3API) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	delete(f.objects, aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeS3API) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	prefix := aws.ToString(in.Prefix)
	var contents []types.Object
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) {
			contents = append(contents, types.Object{Key: aws.String(key)})
		}
	}
	return &s3.ListObjectsV2Output{Contents: contents, IsTruncated: aws.Bool(false)}, nil
}

func newTestS3Store() BlobStore {
	return &s3BlobStore{client: newFakeS3API(), bucket: "test-bucket"}
}

func TestS3BlobStoreCRUD(t *testing.T) {
	ctx := context.Background()
	store := newTestS3Store()

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

func TestS3BlobStoreConditionalWrites(t *testing.T) {
	ctx := context.Background()
	store := newTestS3Store()

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
