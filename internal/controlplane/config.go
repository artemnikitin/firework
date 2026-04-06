package controlplane

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	RoleAll        = "all"
	RoleRegistry   = "registry"
	RoleEvents     = "events"
	RoleController = "controller"
)

// Config configures the control plane runtime.
type Config struct {
	Role string `yaml:"role"`

	RegistryListenAddr string `yaml:"registry_listen_addr"`
	EventsListenAddr   string `yaml:"events_listen_addr"`

	State StateConfig `yaml:"state"`

	LeaderLeaseTTL      time.Duration `yaml:"leader_lease_ttl"`
	LeaderRenewInterval time.Duration `yaml:"leader_renew_interval"`
	NodeStaleTTL        time.Duration `yaml:"node_stale_ttl"`
	ControllerTick      time.Duration `yaml:"controller_tick"`

	TargetBranch string `yaml:"target_branch"`
	ConfigDir    string `yaml:"config_dir"`
	GitRepoURL   string `yaml:"git_repo_url"`

	ReconcileOnStart    bool   `yaml:"reconcile_on_start"`
	GitHubWebhookSecret string `yaml:"github_webhook_secret"`

	TLS        TLSConfig        `yaml:"tls"`
	Enrollment EnrollmentConfig `yaml:"enrollment"`
}

// StateConfig configures durable control-plane state storage.
type StateConfig struct {
	Backend string `yaml:"backend"` // currently: s3
	Prefix  string `yaml:"prefix"`

	S3 S3StateConfig `yaml:"s3"`
}

// S3StateConfig configures S3 state storage.
type S3StateConfig struct {
	Bucket         string `yaml:"bucket"`
	Region         string `yaml:"region"`
	EndpointURL    string `yaml:"endpoint_url"`
	ForcePathStyle bool   `yaml:"force_path_style"`
}

// TLSConfig configures server-side TLS and client CA verification.
type TLSConfig struct {
	CertFile     string `yaml:"cert_file"`
	KeyFile      string `yaml:"key_file"`
	ClientCAFile string `yaml:"client_ca_file"`
}

// EnrollmentConfig configures node certificate enrollment/signing.
type EnrollmentConfig struct {
	CAFile          string           `yaml:"ca_file"`
	CAKeyFile       string           `yaml:"ca_key_file"`
	NodeCertTTL     time.Duration    `yaml:"node_cert_ttl"`
	BootstrapTokens []BootstrapToken `yaml:"bootstrap_tokens"`
}

// BootstrapToken authorizes a node to enroll.
type BootstrapToken struct {
	Token  string `yaml:"token"`
	NodeID string `yaml:"node_id,omitempty"`
}

// DefaultConfig returns defaults for control-plane configuration.
func DefaultConfig() Config {
	return Config{
		Role:               RoleAll,
		RegistryListenAddr: ":9443",
		EventsListenAddr:   ":9444",
		State: StateConfig{
			Backend: "s3",
			Prefix:  "cp/v1",
		},
		LeaderLeaseTTL:      30 * time.Second,
		LeaderRenewInterval: 10 * time.Second,
		NodeStaleTTL:        45 * time.Second,
		ControllerTick:      10 * time.Second,
		TargetBranch:        "main",
		Enrollment: EnrollmentConfig{
			NodeCertTTL: 24 * time.Hour,
		},
	}
}

// LoadConfig loads and validates control-plane config from YAML.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("reading config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate validates runtime configuration.
func (c Config) Validate() error {
	switch c.Role {
	case RoleAll, RoleRegistry, RoleEvents, RoleController:
	default:
		return fmt.Errorf("unsupported role %q", c.Role)
	}

	if c.State.Backend != "s3" {
		return fmt.Errorf("unsupported state backend %q (only s3 is supported)", c.State.Backend)
	}
	if c.State.S3.Bucket == "" {
		return fmt.Errorf("state.s3.bucket is required")
	}
	if c.State.Prefix == "" {
		return fmt.Errorf("state.prefix is required")
	}
	if c.LeaderLeaseTTL <= 0 {
		return fmt.Errorf("leader_lease_ttl must be > 0")
	}
	if c.LeaderRenewInterval <= 0 {
		return fmt.Errorf("leader_renew_interval must be > 0")
	}
	if c.LeaderRenewInterval >= c.LeaderLeaseTTL {
		return fmt.Errorf("leader_renew_interval must be smaller than leader_lease_ttl")
	}
	if c.ControllerTick <= 0 {
		return fmt.Errorf("controller_tick must be > 0")
	}
	if c.NodeStaleTTL <= 0 {
		return fmt.Errorf("node_stale_ttl must be > 0")
	}

	// Controller-only role does not expose HTTPS endpoints, so server TLS
	// cert/key are only required when registry and/or events APIs are enabled.
	needsTLS := c.Role == RoleAll || c.Role == RoleRegistry || c.Role == RoleEvents
	if needsTLS {
		if c.TLS.CertFile == "" || c.TLS.KeyFile == "" {
			return fmt.Errorf("tls.cert_file and tls.key_file are required for role %q", c.Role)
		}
	}

	needsRegistry := c.Role == RoleAll || c.Role == RoleRegistry
	if needsRegistry {
		if c.TLS.ClientCAFile == "" {
			return fmt.Errorf("tls.client_ca_file is required for role %q", c.Role)
		}
		if c.Enrollment.CAFile == "" || c.Enrollment.CAKeyFile == "" {
			return fmt.Errorf("enrollment.ca_file and enrollment.ca_key_file are required for role %q", c.Role)
		}
	}

	needsEvents := c.Role == RoleAll || c.Role == RoleEvents
	if needsEvents && c.GitHubWebhookSecret == "" {
		return fmt.Errorf("github_webhook_secret is required for role %q", c.Role)
	}
	if c.ReconcileOnStart && c.GitRepoURL == "" {
		return fmt.Errorf("git_repo_url is required when reconcile_on_start is enabled")
	}

	return nil
}
