// Scheduler Lambda — receives a list of enriched services, discovers active
// nodes via CloudWatch, reads existing placement from S3, and returns
// per-instance node configs using best-fit bin-packing.
//
// Environment variables:
//
//	CW_NAMESPACE    CloudWatch namespace containing firework_node_* metrics
//	                (e.g. "Firework/firework-example").
//	S3_BUCKET       S3 configs bucket — read to determine existing placement.
//	S3_PREFIX       Optional key prefix in the bucket (e.g. "nodes/").
//	S3_REGION       AWS region for CloudWatch + S3.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"gopkg.in/yaml.v3"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/scheduler"
)

// Request is what the enricher Lambda sends.
type Request struct {
	Services []config.ServiceConfig `json:"services"`
}

// Response is what the enricher Lambda receives back.
type Response struct {
	NodeConfigs []config.NodeConfig `json:"node_configs"`
}

func handler(ctx context.Context, req Request) (Response, error) {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	namespace := os.Getenv("CW_NAMESPACE")
	if namespace == "" {
		return Response{}, fmt.Errorf("CW_NAMESPACE is required")
	}
	s3Bucket := os.Getenv("S3_BUCKET")
	s3Prefix := os.Getenv("S3_PREFIX")
	region := os.Getenv("S3_REGION")

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return Response{}, fmt.Errorf("loading AWS config: %w", err)
	}

	cwClient := cloudwatch.NewFromConfig(cfg)
	s3Client := s3.NewFromConfig(cfg)

	// Discover active nodes and their capacity from CloudWatch.
	nodes, err := discoverNodes(ctx, cwClient, namespace, logger)
	if err != nil {
		return Response{}, fmt.Errorf("discovering nodes: %w", err)
	}
	logger.Info("discovered nodes", "count", len(nodes))

	// Read existing placement from S3 to preserve stability.
	existing, err := readExistingPlacement(ctx, s3Client, s3Bucket, s3Prefix, logger)
	if err != nil {
		// Non-fatal: fall back to full re-placement.
		logger.Warn("failed to read existing placement, will re-place all services", "error", err)
		existing = nil
	}

	resp, err := scheduleServices(req, nodes, existing)
	if err != nil {
		return Response{}, err
	}

	logger.Info("scheduling complete",
		"services", len(req.Services),
		"nodes", len(nodes),
		"node_configs", len(resp.NodeConfigs),
	)

	return resp, nil
}

// scheduleServices is the pure scheduling step, separated from AWS I/O so it
// can be unit-tested. It returns an error when no nodes are discovered so the
// enricher treats the invocation as a failure and falls back to its existing
// placement — preventing WriteAll from being called with an empty list and
// wiping all node configs from S3.
func scheduleServices(req Request, nodes []scheduler.Node, existing map[string]string) (Response, error) {
	if len(nodes) == 0 {
		return Response{}, fmt.Errorf("no active nodes discovered in CloudWatch; cannot schedule %d service(s)", len(req.Services))
	}

	assignment, err := scheduler.Schedule(req.Services, nodes, existing)
	if err != nil {
		return Response{}, fmt.Errorf("scheduling services: %w", err)
	}

	return Response{NodeConfigs: scheduler.BuildNodeConfigs(assignment)}, nil
}

// discoverNodes queries CloudWatch for all nodes reporting
// firework_node_capacity_vcpus and fetches their latest capacity values.
func discoverNodes(ctx context.Context, cw *cloudwatch.Client, namespace string, logger *slog.Logger) ([]scheduler.Node, error) {
	// List all metrics to find node names.
	var nodeNames []string
	paginator := cloudwatch.NewListMetricsPaginator(cw, &cloudwatch.ListMetricsInput{
		Namespace:  aws.String(namespace),
		MetricName: aws.String("firework_node_capacity_vcpus"),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing metrics: %w", err)
		}
		for _, m := range page.Metrics {
			for _, d := range m.Dimensions {
				if aws.ToString(d.Name) == "node" {
					nodeNames = append(nodeNames, aws.ToString(d.Value))
				}
			}
		}
	}

	if len(nodeNames) == 0 {
		return nil, nil
	}

	// Fetch latest capacity values for all nodes in one GetMetricData call.
	endTime := time.Now()
	startTime := endTime.Add(-5 * time.Minute) // look back 5 min for freshness

	var queries []cwtypes.MetricDataQuery
	for i, name := range nodeNames {
		dim := cwtypes.Dimension{Name: aws.String("node"), Value: aws.String(name)}
		queries = append(queries,
			cwtypes.MetricDataQuery{
				Id:    aws.String(fmt.Sprintf("cap_vcpu_%d", i)),
				Label: aws.String(name + "/cap_vcpu"),
				MetricStat: &cwtypes.MetricStat{
					Metric: &cwtypes.Metric{
						Namespace:  aws.String(namespace),
						MetricName: aws.String("firework_node_capacity_vcpus"),
						Dimensions: []cwtypes.Dimension{dim},
					},
					Period: aws.Int32(60),
					Stat:   aws.String("Maximum"),
				},
			},
			cwtypes.MetricDataQuery{
				Id:    aws.String(fmt.Sprintf("cap_mem_%d", i)),
				Label: aws.String(name + "/cap_mem"),
				MetricStat: &cwtypes.MetricStat{
					Metric: &cwtypes.Metric{
						Namespace:  aws.String(namespace),
						MetricName: aws.String("firework_node_capacity_memory_mb"),
						Dimensions: []cwtypes.Dimension{dim},
					},
					Period: aws.Int32(60),
					Stat:   aws.String("Maximum"),
				},
			},
			cwtypes.MetricDataQuery{
				Id:    aws.String(fmt.Sprintf("used_vcpu_%d", i)),
				Label: aws.String(name + "/used_vcpu"),
				MetricStat: &cwtypes.MetricStat{
					Metric: &cwtypes.Metric{
						Namespace:  aws.String(namespace),
						MetricName: aws.String("firework_node_used_vcpus"),
						Dimensions: []cwtypes.Dimension{dim},
					},
					Period: aws.Int32(60),
					Stat:   aws.String("Maximum"),
				},
			},
			cwtypes.MetricDataQuery{
				Id:    aws.String(fmt.Sprintf("used_mem_%d", i)),
				Label: aws.String(name + "/used_mem"),
				MetricStat: &cwtypes.MetricStat{
					Metric: &cwtypes.Metric{
						Namespace:  aws.String(namespace),
						MetricName: aws.String("firework_node_used_memory_mb"),
						Dimensions: []cwtypes.Dimension{dim},
					},
					Period: aws.Int32(60),
					Stat:   aws.String("Maximum"),
				},
			},
		)
	}

	out, err := cw.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
		MetricDataQueries: queries,
		StartTime:         aws.Time(startTime),
		EndTime:           aws.Time(endTime),
	})
	if err != nil {
		return nil, fmt.Errorf("getting metric data: %w", err)
	}

	// Parse results into a map[id]value.
	vals := make(map[string]float64, len(out.MetricDataResults))
	for _, r := range out.MetricDataResults {
		if len(r.Values) > 0 {
			vals[aws.ToString(r.Id)] = r.Values[0]
		}
	}

	var nodes []scheduler.Node
	for i, name := range nodeNames {
		capVCPU := int(vals[fmt.Sprintf("cap_vcpu_%d", i)])
		capMem := int(vals[fmt.Sprintf("cap_mem_%d", i)])
		usedVCPU := int(vals[fmt.Sprintf("used_vcpu_%d", i)])
		usedMem := int(vals[fmt.Sprintf("used_mem_%d", i)])

		if capVCPU == 0 {
			// Node hasn't reported capacity — likely stale; skip.
			logger.Warn("skipping node with zero capacity (stale?)", "node", name)
			continue
		}

		// Available = total - already running. Scheduler then adds new services on top.
		// We report total capacity; scheduler tracks used separately.
		_ = usedVCPU
		_ = usedMem

		nodes = append(nodes, scheduler.Node{
			InstanceID:    name,
			CapacityVCPUs: capVCPU,
			CapacityMemMB: capMem,
		})
	}

	return nodes, nil
}

// readExistingPlacement fetches current per-node configs from S3 and builds
// a serviceName → instanceID mapping.
func readExistingPlacement(ctx context.Context, s3c *s3.Client, bucket, prefix string, logger *slog.Logger) (map[string]string, error) {
	if bucket == "" {
		return nil, nil
	}

	listOut, err := s3c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix + "nodes/"),
	})
	if err != nil {
		return nil, fmt.Errorf("listing S3 objects: %w", err)
	}

	assignment := make(map[string]string)

	for _, obj := range listOut.Contents {
		key := aws.ToString(obj.Key)

		// Extract instance ID from key: "{prefix}nodes/{instanceID}.yaml"
		base := strings.TrimPrefix(key, prefix+"nodes/")
		base = strings.TrimSuffix(base, ".yaml")
		if base == "" || strings.Contains(base, "/") {
			continue
		}
		instanceID := base

		getOut, err := s3c.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			logger.Warn("failed to fetch existing placement file", "key", key, "error", err)
			continue
		}

		body, readErr := io.ReadAll(getOut.Body)
		_ = getOut.Body.Close()
		if readErr != nil {
			logger.Warn("failed to read existing placement file", "key", key, "error", readErr)
			continue
		}

		var nc config.NodeConfig
		if err := yaml.Unmarshal(body, &nc); err != nil {
			logger.Warn("failed to decode existing placement", "key", key, "error", err)
			continue
		}

		for _, svc := range nc.Services {
			assignment[svc.Name] = instanceID
		}
	}

	return assignment, nil
}

func main() {
	lambda.Start(handler)
}
