package enricher

import (
	"fmt"
	"sort"

	"github.com/artemnikitin/firework/internal/config"
)

// Result holds the outcome of an enrichment run.
type Result struct {
	NodeConfigs []config.NodeConfig
	Warnings    []Warn
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
