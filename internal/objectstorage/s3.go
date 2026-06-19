package objectstorage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// S3Config configures an S3 BlobStore.
type S3Config struct {
	Bucket         string
	Region         string
	EndpointURL    string
	ForcePathStyle bool
}

type s3API interface {
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

type s3BlobStore struct {
	client s3API
	bucket string
}

// NewS3BlobStore creates an S3-backed BlobStore.
func NewS3BlobStore(ctx context.Context, cfg S3Config) (BlobStore, error) {
	var loadOpts []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	var clientOpts []func(*s3.Options)
	if cfg.EndpointURL != "" {
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.EndpointURL)
			o.UsePathStyle = cfg.ForcePathStyle
		})
	}
	return NewS3BlobStoreFromClient(s3.NewFromConfig(awsCfg, clientOpts...), cfg.Bucket), nil
}

// NewS3BlobStoreFromClient wraps a preconfigured S3 client.
func NewS3BlobStoreFromClient(client *s3.Client, bucket string) BlobStore {
	return &s3BlobStore{client: client, bucket: bucket}
}

func (s *s3BlobStore) Head(ctx context.Context, key string) (BlobMeta, bool, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		if s3NotFound(err) {
			return BlobMeta{}, false, nil
		}
		return BlobMeta{}, false, fmt.Errorf("head s3://%s/%s: %w", s.bucket, key, err)
	}
	return BlobMeta{
		WriteToken:   WriteToken(aws.ToString(out.ETag)),
		LastModified: timeOrZero(out.LastModified),
		Size:         aws.ToInt64(out.ContentLength),
	}, true, nil
}

func (s *s3BlobStore) Get(ctx context.Context, key string) (io.ReadCloser, BlobMeta, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		if s3NotFound(err) {
			return nil, BlobMeta{}, fmt.Errorf("%w: s3://%s/%s", ErrNotFound, s.bucket, key)
		}
		return nil, BlobMeta{}, fmt.Errorf("get s3://%s/%s: %w", s.bucket, key, err)
	}
	return out.Body, BlobMeta{
		WriteToken:   WriteToken(aws.ToString(out.ETag)),
		LastModified: timeOrZero(out.LastModified),
		Size:         aws.ToInt64(out.ContentLength),
	}, nil
}

func (s *s3BlobStore) GetBytes(ctx context.Context, key string) ([]byte, BlobMeta, bool, error) {
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
		return nil, BlobMeta{}, false, fmt.Errorf("read s3://%s/%s: %w", s.bucket, key, err)
	}
	return data, meta, true, nil
}

func (s *s3BlobStore) Put(ctx context.Context, key string, r io.Reader, opts PutOptions) (BlobMeta, error) {
	out, err := s.client.PutObject(ctx, putInput(s.bucket, key, r, opts))
	if err != nil {
		return BlobMeta{}, fmt.Errorf("write s3://%s/%s: %w", s.bucket, key, err)
	}
	return BlobMeta{WriteToken: WriteToken(aws.ToString(out.ETag))}, nil
}

func (s *s3BlobStore) PutIfAbsent(ctx context.Context, key string, r io.Reader, opts PutOptions) (bool, BlobMeta, error) {
	in := putInput(s.bucket, key, r, opts)
	in.IfNoneMatch = aws.String("*")
	out, err := s.client.PutObject(ctx, in)
	if err != nil {
		if s3PreconditionFailure(err) {
			return false, BlobMeta{}, nil
		}
		return false, BlobMeta{}, fmt.Errorf("conditional write s3://%s/%s: %w", s.bucket, key, err)
	}
	return true, BlobMeta{WriteToken: WriteToken(aws.ToString(out.ETag))}, nil
}

func (s *s3BlobStore) PutIfMatch(ctx context.Context, key string, expected WriteToken, r io.Reader, opts PutOptions) (bool, BlobMeta, error) {
	in := putInput(s.bucket, key, r, opts)
	in.IfMatch = aws.String(string(expected))
	out, err := s.client.PutObject(ctx, in)
	if err != nil {
		if s3PreconditionFailure(err) {
			return false, BlobMeta{}, nil
		}
		return false, BlobMeta{}, fmt.Errorf("conditional write s3://%s/%s: %w", s.bucket, key, err)
	}
	return true, BlobMeta{WriteToken: WriteToken(aws.ToString(out.ETag))}, nil
}

func (s *s3BlobStore) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil && !s3NotFound(err) {
		return fmt.Errorf("delete s3://%s/%s: %w", s.bucket, key, err)
	}
	return nil
}

func (s *s3BlobStore) ListKeys(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(s.bucket), Prefix: aws.String(prefix), ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("list s3://%s/%s: %w", s.bucket, prefix, err)
		}
		for _, obj := range out.Contents {
			if obj.Key != nil {
				keys = append(keys, *obj.Key)
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		token = out.NextContinuationToken
	}
	return keys, nil
}

func (s *s3BlobStore) Close() error { return nil }

func putInput(bucket, key string, r io.Reader, opts PutOptions) *s3.PutObjectInput {
	in := &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: r}
	if opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	return in
}

func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

func s3NotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}

func s3PreconditionFailure(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "PreconditionFailed", "ConditionalRequestConflict", "412", "409":
			return true
		}
	}
	return false
}
