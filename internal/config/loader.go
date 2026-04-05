package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultAgentConfig returns sensible defaults for the agent configuration.
func DefaultAgentConfig() AgentConfig {
	return AgentConfig{
		StoreType:               "git",
		StoreBranch:             "main",
		PollInterval:            30 * time.Second,
		FirecrackerBin:          "/usr/bin/firecracker",
		StateDir:                "/var/lib/firework",
		LogLevel:                "info",
		ImagesDir:               "/var/lib/images",
		VMSubnet:                "172.16.0.0/24",
		VMGateway:               "172.16.0.1",
		VMBridge:                "br-firework",
		RegistryCertRenewBefore:   6 * time.Hour,
		RegistryHeartbeatInterval: 15 * time.Second,
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
	if cfg.NodeID == "" {
		cfg.NodeID = cfg.NodeName
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

	if cfg.RegistryURL != "" {
		if cfg.RegistryCertFile == "" || cfg.RegistryKeyFile == "" || cfg.RegistryCAFile == "" {
			return cfg, fmt.Errorf("registry_cert_file, registry_key_file and registry_ca_file are required when registry_url is set")
		}
		token, err := resolveRegistryBootstrapToken(cfg.RegistryBootstrapToken, cfg.RegistryBootstrapTokenFile)
		if err != nil {
			return cfg, err
		}
		cfg.RegistryBootstrapToken = token
		cfg.NodeID = strings.TrimSpace(cfg.NodeID)
		if cfg.NodeID == "" {
			return cfg, fmt.Errorf("node_id is required when registry_url is set")
		}
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

func resolveRegistryBootstrapToken(token, tokenFile string) (string, error) {
	t := strings.TrimSpace(os.ExpandEnv(token))
	f := strings.TrimSpace(os.ExpandEnv(tokenFile))
	if t != "" && f != "" {
		return "", fmt.Errorf("registry_bootstrap_token and registry_bootstrap_token_file are mutually exclusive")
	}
	if f == "" {
		return t, nil
	}
	data, err := os.ReadFile(f)
	if err != nil {
		return "", fmt.Errorf("reading registry_bootstrap_token_file %s: %w", f, err)
	}
	t = strings.TrimSpace(string(data))
	if t == "" {
		return "", fmt.Errorf("registry_bootstrap_token_file %s is empty", f)
	}
	return t, nil
}
