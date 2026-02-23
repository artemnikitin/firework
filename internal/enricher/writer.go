package enricher

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/artemnikitin/firework/internal/config"
	"gopkg.in/yaml.v3"
)

type s3WriterAPI interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// S3Writer writes enriched NodeConfig YAML files to S3.
type S3Writer struct {
	client s3WriterAPI
	bucket string
	prefix string
}

// NewS3Writer creates a new S3Writer.
func NewS3Writer(ctx context.Context, bucket, prefix, region, endpointURL string) (*S3Writer, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	var s3Opts []func(*s3.Options)
	if endpointURL != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpointURL)
			o.UsePathStyle = true
		})
	}

	client := s3.NewFromConfig(awsCfg, s3Opts...)
	return newS3WriterWithClient(client, bucket, prefix), nil
}

// WriteNodeConfig marshals the NodeConfig to YAML and puts it to S3.
func (w *S3Writer) WriteNodeConfig(ctx context.Context, nc config.NodeConfig) error {
	data, err := yaml.Marshal(nc)
	if err != nil {
		return fmt.Errorf("marshaling config for node %s: %w", nc.Node, err)
	}

	key := w.nodeKey(nc.Node)

	_, err = w.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(w.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/x-yaml"),
	})
	if err != nil {
		return fmt.Errorf("writing s3://%s/%s: %w", w.bucket, key, err)
	}

	return nil
}

// WriteAll writes all enriched NodeConfigs to S3.
// If configs is empty the function is a no-op: it will not delete existing
// objects, because an empty scheduler response is almost certainly an error
// rather than a genuine "zero services desired" signal.
func (w *S3Writer) WriteAll(ctx context.Context, configs []config.NodeConfig) error {
	if len(configs) == 0 {
		return nil
	}

	desiredKeys := make(map[string]struct{}, len(configs))
	for _, nc := range configs {
		desiredKeys[w.nodeKey(nc.Node)] = struct{}{}
	}

	existingKeys, err := w.listNodeConfigKeys(ctx)
	if err != nil {
		return fmt.Errorf("listing existing node configs: %w", err)
	}

	for _, nc := range configs {
		if err := w.WriteNodeConfig(ctx, nc); err != nil {
			return err
		}
	}

	for _, key := range existingKeys {
		if _, keep := desiredKeys[key]; keep {
			continue
		}
		if err := w.deleteNodeConfigKey(ctx, key); err != nil {
			return fmt.Errorf("deleting stale node config %s: %w", key, err)
		}
	}
	return nil
}

// nodeKey returns the full S3 key for a node's config file.
// Matches the key format used by the agent's S3Store.
func (w *S3Writer) nodeKey(nodeName string) string {
	return w.prefix + "nodes/" + nodeName + ".yaml"
}

func (w *S3Writer) nodesPrefix() string {
	return w.prefix + "nodes/"
}

func (w *S3Writer) listNodeConfigKeys(ctx context.Context) ([]string, error) {
	prefix := w.nodesPrefix()
	var keys []string
	var token *string

	for {
		out, err := w.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(w.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}

		for _, obj := range out.Contents {
			key := objectKey(obj)
			if key == "" || !strings.HasSuffix(key, ".yaml") {
				continue
			}
			keys = append(keys, key)
		}

		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}

	return keys, nil
}

func (w *S3Writer) deleteNodeConfigKey(ctx context.Context, key string) error {
	_, err := w.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(w.bucket),
		Key:    aws.String(key),
	})
	return err
}

func objectKey(obj types.Object) string {
	if obj.Key == nil {
		return ""
	}
	return *obj.Key
}

func newS3WriterWithClient(client s3WriterAPI, bucket, prefix string) *S3Writer {
	return &S3Writer{
		client: client,
		bucket: bucket,
		prefix: prefix,
	}
}
