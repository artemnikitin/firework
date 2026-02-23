package enricher

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/artemnikitin/firework/internal/config"
)

// Config holds runtime configuration for the enrichment pipeline.
type Config struct {
	S3Bucket      string
	S3Prefix      string
	S3Region      string
	S3EndpointURL string
	// SchedulerLambdaARN is the ARN of the scheduler Lambda to invoke for
	// multi-node placement. When empty, the enricher uses node_type grouping
	// (single-node / static placement mode).
	SchedulerLambdaARN string
	// SchedulerRegion is the AWS region for the scheduler Lambda invocation.
	// Defaults to S3Region when empty.
	SchedulerRegion string
	// EC2Region is the AWS region for ec2:DescribeInstances calls used to
	// resolve node private IPs for cross-node links. Defaults to S3Region.
	EC2Region string
}

// Result holds the outcome of an enrichment run.
type Result struct {
	NodeConfigs []config.NodeConfig
	Warnings    []Warn
}

// Run executes the full enrichment pipeline and writes results to S3.
func Run(ctx context.Context, inputDir string, cfg Config, logger *slog.Logger) (*Result, error) {
	result, err := Enrich(inputDir)
	if err != nil {
		return nil, err
	}

	for _, w := range result.Warnings {
		logger.Warn("enrichment warning", "message", w.Message)
	}

	// Delegate placement to the scheduler Lambda when configured.
	nodeConfigs := result.NodeConfigs
	if cfg.SchedulerLambdaARN != "" {
		schedulerCfg := cfg
		if schedulerCfg.SchedulerRegion == "" {
			schedulerCfg.SchedulerRegion = cfg.S3Region
		}
		scheduled, err := invokeScheduler(ctx, schedulerCfg, result.NodeConfigs)
		if err != nil {
			return nil, fmt.Errorf("scheduler invocation failed: %w", err)
		}
		nodeConfigs = scheduled
		logger.Info("scheduler returned placement",
			"node_configs", len(nodeConfigs))

		// Resolve EC2 private IPs and inject cross-node link env vars.
		ec2Region := cfg.EC2Region
		if ec2Region == "" {
			ec2Region = cfg.S3Region
		}
		if resolved, err := resolveNodeIPs(ctx, ec2Region, nodeConfigs); err != nil {
			logger.Warn("could not resolve node IPs, cross-node links skipped", "error", err)
		} else {
			nodeConfigs = resolveCrossNodeLinks(resolved)
		}
	}

	writer, err := NewS3Writer(ctx, cfg.S3Bucket, cfg.S3Prefix, cfg.S3Region, cfg.S3EndpointURL)
	if err != nil {
		return nil, fmt.Errorf("creating S3 writer: %w", err)
	}

	if err := writer.WriteAll(ctx, nodeConfigs); err != nil {
		return nil, fmt.Errorf("writing to S3: %w", err)
	}

	return result, nil
}

// Enrich performs the enrichment pipeline without writing to S3.
// Useful for testing and dry-run mode.
//
// Pipeline:
//  1. Load input configs from the given directory
//  2. Load and expand tenants (optional — no-op if tenants/ doesn't exist)
//  3. Validate input
//  4. Group services by node type
//  5. Enrich each service with defaults
//  6. Validate output
func Enrich(inputDir string) (*Result, error) {
	// 1. Load input.
	input, err := LoadInput(inputDir)
	if err != nil {
		return nil, fmt.Errorf("loading input: %w", err)
	}

	// 2. Load and expand tenants (optional — no-op if tenants/ doesn't exist).
	tenants, err := LoadTenants(inputDir)
	if err != nil {
		return nil, fmt.Errorf("loading tenants: %w", err)
	}
	if len(tenants) > 0 {
		expanded := ExpandTenants(input.Services, tenants)
		input.Services = append(input.Services, expanded...)
	}

	// 3. Validate input.
	if err := ValidateInput(input); err != nil {
		return nil, fmt.Errorf("input validation: %w", err)
	}

	// Collect warnings.
	warnings := CheckWarnings(input)

	// 4. Group services by node type.
	groups := groupByNodeType(input.Services)

	// Process node types in sorted order for deterministic output.
	nodeTypes := make([]string, 0, len(groups))
	for nt := range groups {
		nodeTypes = append(nodeTypes, nt)
	}
	sort.Strings(nodeTypes)

	// 5-6. For each node type, enrich and validate.
	var nodeConfigs []config.NodeConfig

	for _, nodeType := range nodeTypes {
		services := groups[nodeType]

		var enrichedServices []config.ServiceConfig
		for _, spec := range services {
			svc := EnrichService(spec, input.Defaults)
			enrichedServices = append(enrichedServices, svc)
		}

		nc := config.NodeConfig{
			Node:     nodeType,
			Services: enrichedServices,
		}

		// 6. Validate output.
		if err := ValidateOutput(nc); err != nil {
			return nil, fmt.Errorf("output validation for type %s: %w", nodeType, err)
		}

		nodeConfigs = append(nodeConfigs, nc)
	}

	return &Result{
		NodeConfigs: nodeConfigs,
		Warnings:    warnings,
	}, nil
}

// groupByNodeType groups services by their NodeType field.
func groupByNodeType(services []ServiceSpec) map[string][]ServiceSpec {
	groups := make(map[string][]ServiceSpec)
	for _, svc := range services {
		groups[svc.NodeType] = append(groups[svc.NodeType], svc)
	}
	return groups
}
