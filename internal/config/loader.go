package config

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// ErrNoServicesList reports a node config that does not state its services.
var ErrNoServicesList = errors.New("node config has no services list; write `services: []` to declare a node that runs nothing")

// ParseNodeConfig parses a node configuration from raw YAML bytes.
//
// A config is authoritative only when it carries an explicit services list.
// The agent deletes every running service absent from that list, so an empty
// body, an empty document, an omitted key, or `services: null` must not decode
// to zero services: each is what a truncated write, a half-synced checkout, or
// a hand-edit mistake looks like, and treating it as "run nothing" would stop
// every workload on the node. Only `services: []` declares an empty node.
func ParseNodeConfig(data []byte) (NodeConfig, error) {
	var nc NodeConfig
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nc, fmt.Errorf("parsing node config: %w", err)
	}
	if !hasServicesList(&doc) {
		return nc, fmt.Errorf("parsing node config: %w", ErrNoServicesList)
	}
	if err := doc.Decode(&nc); err != nil {
		return nc, fmt.Errorf("parsing node config: %w", err)
	}
	if nc.Services == nil {
		nc.Services = []ServiceConfig{}
	}
	return nc, nil
}

// hasServicesList reports whether a parsed document is a mapping whose
// services key holds a sequence, however short. Presence is checked on the
// node tree because the decoded slice is nil for `[]`, null, and absence alike.
func hasServicesList(doc *yaml.Node) bool {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return false
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "services" {
			return root.Content[i+1].Kind == yaml.SequenceNode
		}
	}
	return false
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
