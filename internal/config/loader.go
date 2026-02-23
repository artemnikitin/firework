package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultAgentConfig returns sensible defaults for the agent configuration.
func DefaultAgentConfig() AgentConfig {
	return AgentConfig{
		StoreType:      "git",
		StoreBranch:    "main",
		PollInterval:   30 * time.Second,
		FirecrackerBin: "/usr/bin/firecracker",
		StateDir:       "/var/lib/firework",
		LogLevel:       "info",
		ImagesDir:      "/var/lib/images",
		VMSubnet:       "172.16.0.0/24",
		VMGateway:      "172.16.0.1",
		VMBridge:       "br-firework",
	}
}

// LoadAgentConfig reads the agent configuration from a YAML file and applies
// defaults for any unset fields.
func LoadAgentConfig(path string) (AgentConfig, error) {
	cfg := DefaultAgentConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("reading agent config %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing agent config %s: %w", path, err)
	}

	if cfg.NodeName == "" && len(cfg.NodeNames) == 0 {
		hostname, err := os.Hostname()
		if err != nil {
			return cfg, fmt.Errorf("node_name not set and hostname unavailable: %w", err)
		}
		cfg.NodeName = hostname
	}

	// Backcompat: populate NodeNames from NodeName if not set.
	if len(cfg.NodeNames) == 0 && cfg.NodeName != "" {
		cfg.NodeNames = []string{cfg.NodeName}
	}
	// Set NodeName from first label for display/logging.
	if cfg.NodeName == "" && len(cfg.NodeNames) > 0 {
		cfg.NodeName = cfg.NodeNames[0]
	}

	switch cfg.StoreType {
	case "git":
		if cfg.StoreURL == "" {
			return cfg, fmt.Errorf("store_url is required for git store")
		}
	case "s3":
		if cfg.S3Bucket == "" {
			return cfg, fmt.Errorf("s3_bucket is required for s3 store")
		}
	default:
		return cfg, fmt.Errorf("unsupported store_type: %q (expected \"git\" or \"s3\")", cfg.StoreType)
	}

	return cfg, nil
}

// ParseNodeConfig parses a node configuration from raw YAML bytes.
func ParseNodeConfig(data []byte) (NodeConfig, error) {
	var nc NodeConfig
	if err := yaml.Unmarshal(data, &nc); err != nil {
		return nc, fmt.Errorf("parsing node config: %w", err)
	}
	return nc, nil
}
