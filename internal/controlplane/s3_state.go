package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// S3StateStore persists control-plane state in S3.
type S3StateStore struct {
	client *s3.Client
	bucket string
}

// NewS3StateStore creates an S3-backed state store.
func NewS3StateStore(ctx context.Context, cfg S3StateConfig) (*S3StateStore, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading aws config: %w", err)
	}

	var s3Opts []func(*s3.Options)
	if cfg.EndpointURL != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.EndpointURL)
			o.UsePathStyle = cfg.ForcePathStyle
		})
	}
	client := s3.NewFromConfig(awsCfg, s3Opts...)

	return &S3StateStore{
		client: client,
		bucket: cfg.Bucket,
	}, nil
}

// GetJSON fetches and unmarshals a JSON object.
func (s *S3StateStore) GetJSON(ctx context.Context, key string, out any) (etag string, exists bool, err error) {
	data, etag, exists, err := s.GetRaw(ctx, key)
	if err != nil || !exists {
		return etag, exists, err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return "", false, fmt.Errorf("decoding json s3://%s/%s: %w", s.bucket, key, err)
	}
	return etag, true, nil
}

// GetRaw fetches a raw object.
func (s *S3StateStore) GetRaw(ctx context.Context, key string) (data []byte, etag string, exists bool, err error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, "", false, nil
		}
		return nil, "", false, fmt.Errorf("getting s3://%s/%s: %w", s.bucket, key, err)
	}
	defer out.Body.Close()

	data, err = io.ReadAll(out.Body)
	if err != nil {
		return nil, "", false, fmt.Errorf("reading s3://%s/%s: %w", s.bucket, key, err)
	}
	return data, aws.ToString(out.ETag), true, nil
}

// PutJSON writes JSON object unconditionally.
func (s *S3StateStore) PutJSON(ctx context.Context, key string, value any) (etag string, err error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal json: %w", err)
	}
	return s.PutRaw(ctx, key, data, "application/json")
}

// PutJSONIfAbsent writes JSON object if key doesn't exist.
func (s *S3StateStore) PutJSONIfAbsent(ctx context.Context, key string, value any) (ok bool, etag string, err error) {
	data, err := json.Marshal(value)
	if err != nil {
		return false, "", fmt.Errorf("marshal json: %w", err)
	}
	return s.PutRawIfAbsent(ctx, key, data, "application/json")
}

// PutJSONIfMatch writes JSON object if current ETag matches expectedETag.
func (s *S3StateStore) PutJSONIfMatch(ctx context.Context, key string, expectedETag string, value any) (ok bool, etag string, err error) {
	data, err := json.Marshal(value)
	if err != nil {
		return false, "", fmt.Errorf("marshal json: %w", err)
	}
	return s.PutRawIfMatch(ctx, key, expectedETag, data, "application/json")
}

// PutRaw writes object unconditionally.
func (s *S3StateStore) PutRaw(ctx context.Context, key string, data []byte, contentType string) (etag string, err error) {
	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("writing s3://%s/%s: %w", s.bucket, key, err)
	}
	return aws.ToString(out.ETag), nil
}

// PutRawIfAbsent writes object only if key doesn't exist.
func (s *S3StateStore) PutRawIfAbsent(ctx context.Context, key string, data []byte, contentType string) (ok bool, etag string, err error) {
	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(contentType),
		IfNoneMatch: aws.String("*"),
	})
	if err != nil {
		if isPreconditionFailure(err) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("conditional write s3://%s/%s: %w", s.bucket, key, err)
	}
	return true, aws.ToString(out.ETag), nil
}

// PutRawIfMatch writes object only when ETag matches.
func (s *S3StateStore) PutRawIfMatch(ctx context.Context, key string, expectedETag string, data []byte, contentType string) (ok bool, etag string, err error) {
	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(contentType),
		IfMatch:     aws.String(expectedETag),
	})
	if err != nil {
		if isPreconditionFailure(err) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("conditional write s3://%s/%s: %w", s.bucket, key, err)
	}
	return true, aws.ToString(out.ETag), nil
}

// Delete deletes an object.
func (s *S3StateStore) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("deleting s3://%s/%s: %w", s.bucket, key, err)
	}
	return nil
}

// ListKeys lists keys under a prefix.
func (s *S3StateStore) ListKeys(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("listing s3://%s/%s: %w", s.bucket, prefix, err)
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

func isNotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		return code == "NoSuchKey" || code == "NotFound" || code == "404"
	}
	return false
}

func isPreconditionFailure(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		return code == "PreconditionFailed" || code == "ConditionalRequestConflict" || code == "412"
	}
	return false
}
