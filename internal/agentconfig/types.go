package agentconfig

import "time"

// StorageConfig describes host storage pools supplied and mounted by the
// deployment operator. Firework never provisions cloud storage resources.
type StorageConfig struct {
	Local  *LocalStorageConfig  `yaml:"local,omitempty"`
	Shared *SharedStorageConfig `yaml:"shared,omitempty"`
}

// LocalStorageConfig configures a node-affine storage pool.
type LocalStorageConfig struct {
	Path          string `yaml:"path"`
	Capacity      string `yaml:"capacity"`
	CapacityBytes int64  `yaml:"-"`
}

// SharedStorageConfig configures one deployment-wide shared storage backend.
// CapacityBytes may be zero when the operator intentionally omits aggregate
// admission control (for example, elastic EFS).
type SharedStorageConfig struct {
	BackendID     string `yaml:"backend_id"`
	Path          string `yaml:"path"`
	Capacity      string `yaml:"capacity,omitempty"`
	CapacityBytes int64  `yaml:"-"`
}

// AgentConfig holds the agent's own operational configuration.
type AgentConfig struct {
	// NodeID is the stable identity used for control-plane registry and mTLS
	// certificate subject matching. Defaults to NodeName when empty.
	NodeID string `yaml:"node_id,omitempty"`
	// NodeName is this node's unique identifier.
	NodeName string `yaml:"node_name"`
	// StoreType is the config store backend: "git", "s3", or "gcs".
	StoreType string `yaml:"store_type"`
	// StoreURL is the URL/path to the config store.
	// For git: the repo URL. For S3: not used (use S3Bucket instead).
	StoreURL string `yaml:"store_url,omitempty"`
	// StoreBranch is the git branch to track (for git store).
	StoreBranch string `yaml:"store_branch,omitempty"`
	// S3Bucket is the S3 bucket name (for s3 store).
	S3Bucket string `yaml:"s3_bucket,omitempty"`
	// S3Prefix is an optional key prefix in the bucket (e.g. "configs/").
	// Include trailing slash.
	S3Prefix string `yaml:"s3_prefix,omitempty"`
	// S3Region is the AWS region for the S3 bucket. If empty, resolved from
	// the environment (instance metadata, AWS_REGION env var, etc.).
	S3Region string `yaml:"s3_region,omitempty"`
	// S3EndpointURL overrides the S3 endpoint (useful for LocalStack/MinIO).
	S3EndpointURL string `yaml:"s3_endpoint_url,omitempty"`
	// GCSBucket is the GCS bucket name (for gcs store).
	GCSBucket string `yaml:"gcs_bucket,omitempty"`
	// GCSPrefix is an optional key prefix. Include a trailing slash.
	GCSPrefix string `yaml:"gcs_prefix,omitempty"`
	// GCSCredentialsFile overrides Application Default Credentials when set.
	GCSCredentialsFile string `yaml:"gcs_credentials_file,omitempty"`
	// GCSProject is the GCP project containing the bucket.
	GCSProject string `yaml:"gcs_project,omitempty"`
	// PollInterval is how often the agent polls the config store.
	PollInterval time.Duration `yaml:"poll_interval"`
	// FirecrackerBin is the path to the firecracker binary.
	FirecrackerBin string `yaml:"firecracker_bin"`
	// StateDir is where the agent stores runtime state.
	StateDir string `yaml:"state_dir"`
	// LogLevel controls verbosity: "debug", "info", "warn", "error".
	LogLevel string `yaml:"log_level"`
	// APIListenAddr is the address for the status/health HTTP API (e.g. ":8080").
	// If empty, the API server is not started.
	APIListenAddr string `yaml:"api_listen_addr,omitempty"`
	// EnableHealthChecks enables the health check monitor. Default: true.
	EnableHealthChecks *bool `yaml:"enable_health_checks,omitempty"`
	// EnableNetworkSetup enables automatic TAP/bridge creation. Default: true.
	EnableNetworkSetup *bool `yaml:"enable_network_setup,omitempty"`
	// NodeNames lists all labels for this node. The agent fetches and merges
	// configs for each name. Overrides NodeName when non-empty.
	NodeNames []string `yaml:"node_names,omitempty"`
	// S3ImagesBucket is the S3 bucket containing VM images (rootfs, kernels).
	// If empty, image sync is disabled (images must be pre-placed on disk).
	S3ImagesBucket string `yaml:"s3_images_bucket,omitempty"`
	// GCSImagesBucket is the GCS bucket containing VM images.
	GCSImagesBucket string `yaml:"gcs_images_bucket,omitempty"`
	// ImagesDir is the local directory where VM images are stored.
	ImagesDir string `yaml:"images_dir"`
	// VMSubnet is the CIDR subnet for VM guest IPs.
	VMSubnet string `yaml:"vm_subnet,omitempty"`
	// VMGateway is the gateway IP assigned to the shared bridge.
	VMGateway string `yaml:"vm_gateway,omitempty"`
	// VMBridge is the name of the shared bridge device.
	VMBridge string `yaml:"vm_bridge,omitempty"`
	// OutInterface is the host's external network interface for masquerade.
	OutInterface string `yaml:"out_interface,omitempty"`
	// EnableCapacityCheck enables node resource capacity checking. Default: true.
	// When enabled, the agent reads vCPU and memory from the OS and skips
	// reconciliation if desired services exceed available resources.
	EnableCapacityCheck *bool `yaml:"enable_capacity_check,omitempty"`
	// UpdateStrategy controls how service updates are applied.
	// "" or "all-at-once" (default): all updates applied simultaneously.
	// "rolling": updates applied one at a time with UpdateDelay between each.
	UpdateStrategy string `yaml:"update_strategy,omitempty"`
	// UpdateDelay is the pause between individual service updates in rolling mode.
	UpdateDelay time.Duration `yaml:"update_delay,omitempty"`
	// TraefikConfigDir is the directory where the agent writes per-service
	// Traefik dynamic config files. Traefik's file provider watches this
	// directory and picks up changes without a reload.
	// If empty, Traefik config management is disabled.
	TraefikConfigDir string `yaml:"traefik_config_dir,omitempty"`
	// IngressDomain is the deployment-owned DNS suffix used to form the public
	// hostname for a service that sets metadata.subdomain: the final hostname is
	// "<subdomain>.<ingress_domain>". It is a bare DNS domain — no "*." prefix,
	// URL scheme, port, or path. If empty, services must use an exact
	// metadata.host instead. The loader normalizes and validates this value.
	IngressDomain string `yaml:"ingress_domain,omitempty"`
	// Storage contains optional operator-provided persistent storage pools.
	Storage StorageConfig `yaml:"storage,omitempty"`

	// RegistryURL enables control-plane registry integration when set.
	// The agent will enroll/renew mTLS certificates and send register/heartbeat
	// updates to this endpoint.
	RegistryURL string `yaml:"registry_url,omitempty"`
	// RegistryServerName overrides TLS server name for the registry endpoint.
	RegistryServerName string `yaml:"registry_server_name,omitempty"`
	// RegistryCertFile is the local path to the node mTLS client certificate.
	RegistryCertFile string `yaml:"registry_cert_file,omitempty"`
	// RegistryKeyFile is the local path to the node mTLS private key.
	RegistryKeyFile string `yaml:"registry_key_file,omitempty"`
	// RegistryCAFile is the CA bundle used to verify the registry TLS cert.
	RegistryCAFile string `yaml:"registry_ca_file,omitempty"`
	// RegistryBootstrapToken is used once to enroll a node certificate.
	// Supports env placeholders (for example ${REGISTRY_BOOTSTRAP_TOKEN}).
	RegistryBootstrapToken string `yaml:"registry_bootstrap_token,omitempty"`
	// RegistryBootstrapTokenFile points to a file containing the bootstrap
	// token. Useful for secret mounts; file contents are trimmed.
	RegistryBootstrapTokenFile string `yaml:"registry_bootstrap_token_file,omitempty"`
	// RegistryCertRenewBefore triggers proactive certificate renewal when the
	// current certificate expires within this window.
	RegistryCertRenewBefore time.Duration `yaml:"registry_cert_renew_before,omitempty"`
	// RegistryHeartbeatInterval is how often the agent sends heartbeats to the
	// registry on its own goroutine, independent of the reconciliation loop.
	// Must be shorter than the server-side node_stale_ttl (default 45s).
	// Default: 15s.
	RegistryHeartbeatInterval time.Duration `yaml:"registry_heartbeat_interval,omitempty"`
}
