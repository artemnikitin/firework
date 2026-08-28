package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// ParseNodeConfig parses a node configuration from raw YAML bytes.
func ParseNodeConfig(data []byte) (NodeConfig, error) {
	var nc NodeConfig
	if err := yaml.Unmarshal(data, &nc); err != nil {
		return nc, fmt.Errorf("parsing node config: %w", err)
	}
	return nc, nil
}

// NodeConfigWarnings reports non-fatal problems in a hand-authored node config.
//
// It exists for direct-Git mode. A control-plane-managed cluster mints a new
// resize_generation whenever a volume's requested size changes; a hand-authored
// file carries the generation itself, and Firework cannot mint one for it. A
// declared size with an absent or zero generation is the shape such a file most
// often takes, and it means no resize is ever recognized — so it is worth
// saying out loud even though nothing can verify the generation without history.
func NodeConfigWarnings(nc NodeConfig) []string {
	var warnings []string
	for _, service := range nc.Services {
		for _, volume := range service.Volumes {
			if volume.SizeBytes > 0 && volume.ResizeGeneration <= 0 {
				warnings = append(warnings, fmt.Sprintf(
					"service %s volume %s declares a size with no resize_generation; bump resize_generation whenever you change size_bytes",
					service.Name, volume.Name))
			}
			// The agent matches bound_node against its stable node_id, which
			// need not equal the config's node key — so a mismatch is only
			// probably wrong, and warns rather than failing.
			if volume.Type == VolumeTypeLocal && volume.BoundNode != "" && volume.BoundNode != nc.Node {
				warnings = append(warnings, fmt.Sprintf(
					"service %s volume %s is bound to %q but this config is for node %q; bound_node must match the agent's node_id",
					service.Name, volume.Name, volume.BoundNode, nc.Node))
			}
		}
	}
	return warnings
}
