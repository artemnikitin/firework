package enricher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/artemnikitin/firework/internal/config"
	"gopkg.in/yaml.v3"
)

// ServiceSpec is the lightweight user-facing service definition.
type ServiceSpec struct {
	Name              string                 `yaml:"name"`
	Image             string                 `yaml:"image"`
	Kernel            string                 `yaml:"kernel,omitempty"`
	VCPUs             int                    `yaml:"vcpus,omitempty"`
	MemoryMB          int                    `yaml:"memory_mb,omitempty"`
	KernelArgs        string                 `yaml:"kernel_args,omitempty"`
	NodeType          string                 `yaml:"node_type"`
	Network           bool                   `yaml:"network,omitempty"`
	PortForwards      []config.PortForward   `yaml:"port_forwards,omitempty"`
	HealthCheck       *HealthCheckSpec       `yaml:"health_check,omitempty"`
	Env               map[string]string      `yaml:"env,omitempty"`
	Links             []config.ServiceLink   `yaml:"links,omitempty"`
	Metadata          map[string]string      `yaml:"metadata,omitempty"`
	AntiAffinityGroup string                 `yaml:"anti_affinity_group,omitempty"`
	CrossNodeLinks    []config.CrossNodeLink `yaml:"cross_node_links,omitempty"`
	// NodeHostIPEnv, when set, causes the enricher to inject this node's own
	// host IP into the named env var (e.g. "transport.publish_host" for ES).
	NodeHostIPEnv string `yaml:"node_host_ip_env,omitempty"`
}

// HealthCheckSpec is the user-facing health check definition.
// It uses port+path so the agent can compose the full target URL
// from the guest IP allocated at runtime.
type HealthCheckSpec struct {
	Type     string `yaml:"type"`
	Port     int    `yaml:"port"`
	Path     string `yaml:"path,omitempty"`
	Interval string `yaml:"interval,omitempty"`
	Timeout  string `yaml:"timeout,omitempty"`
	Retries  int    `yaml:"retries,omitempty"`
}

// Defaults holds global default values applied to every service.
type Defaults struct {
	Kernel      string           `yaml:"kernel,omitempty"`
	VCPUs       int              `yaml:"vcpus,omitempty"`
	MemoryMB    int              `yaml:"memory_mb,omitempty"`
	KernelArgs  string           `yaml:"kernel_args,omitempty"`
	HealthCheck *HealthCheckSpec `yaml:"health_check,omitempty"`
}

// InputConfig is the fully parsed input from the user's Git repo.
type InputConfig struct {
	Services []ServiceSpec
	Defaults Defaults
}

// LoadInput reads all configuration from the given directory.
// It expects: defaults.yaml (optional), services/*.yaml.
func LoadInput(dir string) (*InputConfig, error) {
	defaults, err := parseDefaults(filepath.Join(dir, "defaults.yaml"))
	if err != nil {
		return nil, fmt.Errorf("parsing defaults.yaml: %w", err)
	}

	services, err := parseServices(filepath.Join(dir, "services"))
	if err != nil {
		return nil, fmt.Errorf("parsing services: %w", err)
	}

	return &InputConfig{
		Services: services,
		Defaults: defaults,
	}, nil
}

// parseDefaults reads and parses defaults.yaml. Returns zero-value Defaults
// if the file does not exist (all hardcoded fallbacks apply).
func parseDefaults(path string) (Defaults, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Defaults{}, nil
		}
		return Defaults{}, fmt.Errorf("reading %s: %w", path, err)
	}

	var defs Defaults
	if err := yaml.Unmarshal(data, &defs); err != nil {
		return Defaults{}, fmt.Errorf("unmarshaling %s: %w", path, err)
	}

	return defs, nil
}

// parseServices reads all YAML files in the services/ directory.
// Returns an empty slice without error if the directory does not exist.
func parseServices(dir string) ([]ServiceSpec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading directory %s: %w", dir, err)
	}

	var services []ServiceSpec
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}

		var svc ServiceSpec
		if err := yaml.Unmarshal(data, &svc); err != nil {
			return nil, fmt.Errorf("unmarshaling %s: %w", name, err)
		}

		services = append(services, svc)
	}

	return services, nil
}
