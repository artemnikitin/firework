package config

import "time"

// NodeConfig represents the desired state for a specific node.
// Each node pulls its own config from the central store.
type NodeConfig struct {
	// Node is the unique identifier for this node.
	Node string `yaml:"node"`
	// Services is the list of services assigned to this node.
	Services []ServiceConfig `yaml:"services"`
	// HostIP is the EC2 instance private IP, resolved by the enricher.
	HostIP string `yaml:"host_ip,omitempty"`
}

// ServiceConfig defines a single service (Firecracker microVM) to run.
type ServiceConfig struct {
	// Name is a unique identifier for this service on the node.
	Name string `yaml:"name"`
	// Image is the path or URL to the root filesystem image.
	Image string `yaml:"image"`
	// Kernel is the path or URL to the kernel binary.
	Kernel string `yaml:"kernel"`
	// VCPUs is the number of virtual CPUs to allocate.
	VCPUs int `yaml:"vcpus"`
	// MemoryMB is the amount of memory in megabytes.
	MemoryMB int `yaml:"memory_mb"`
	// KernelArgs are additional kernel boot arguments.
	KernelArgs string `yaml:"kernel_args,omitempty"`
	// Network holds optional network configuration.
	Network *NetworkConfig `yaml:"network,omitempty"`
	// HealthCheck holds optional health check configuration.
	HealthCheck *HealthCheckConfig `yaml:"health_check,omitempty"`
	// PortForwards defines host-to-VM port mappings for external access.
	PortForwards []PortForward `yaml:"port_forwards,omitempty"`
	// Env holds environment variables injected into the guest via kernel
	// boot arguments. The agent appends firework.env.KEY=VALUE entries
	// to KernelArgs and the guest's fc-init parses them from /proc/cmdline.
	Env map[string]string `yaml:"env,omitempty"`
	// Links declares dependencies on other services. The agent resolves each
	// link to the target service's guest IP and injects the composed URL into
	// the Env map (which then gets passed to the guest via kernel boot args).
	Links []ServiceLink `yaml:"links,omitempty"`
	// Metadata is arbitrary key-value pairs passed to the VM.
	Metadata map[string]string `yaml:"metadata,omitempty"`
	// AntiAffinityGroup is an optional group label. The scheduler prefers
	// placing services with the same group on different nodes.
	AntiAffinityGroup string `yaml:"anti_affinity_group,omitempty"`
	// CrossNodeLinks declares env vars to inject from peer services on other nodes.
	CrossNodeLinks []CrossNodeLink `yaml:"cross_node_links,omitempty"`
	// NodeHostIPEnv, when non-empty, causes the enricher to inject this node's
	// own host IP (EC2 private IP) into the named env var. Useful when a
	// service needs to advertise its transport address as the host IP rather
	// than the VM guest IP (e.g. Elasticsearch transport.publish_host).
	NodeHostIPEnv string `yaml:"node_host_ip_env,omitempty"`
}

// CrossNodeLink declares a dependency on a peer service running on a different
// EC2 instance. The enricher resolves the peer node's private IP and injects
// an env var of the form "<ip>:<host_port>".
type CrossNodeLink struct {
	// Service is the fully-qualified peer service name.
	Service string `yaml:"service"`
	// Env is the env var injected into THIS service.
	Env string `yaml:"env"`
	// HostPort is the forwarded port on the peer's host.
	HostPort int `yaml:"host_port"`
}

// ServiceLink declares that this service needs connectivity to another
// service. The agent resolves it at runtime by looking up the target's
// guest IP and injecting an environment variable with the composed URL.
type ServiceLink struct {
	// Service is the name of the target service (must exist on the same node).
	Service string `yaml:"service"`
	// EnvVar is the environment variable name to inject (e.g. "ELASTICSEARCH_HOSTS").
	EnvVar string `yaml:"env"`
	// Port is the target service's port.
	Port int `yaml:"port"`
	// Protocol is the URL scheme. Defaults to "http" if empty.
	Protocol string `yaml:"protocol,omitempty"`
}

// NetworkConfig defines network settings for a microVM.
type NetworkConfig struct {
	// Interface is the name of the tap device on the host.
	Interface string `yaml:"interface"`
	// HostDevName is the host network device to bridge to.
	HostDevName string `yaml:"host_dev_name,omitempty"`
	// GuestMAC is the MAC address for the guest network interface.
	GuestMAC string `yaml:"guest_mac,omitempty"`
	// GuestIP is the static IP to assign inside the guest (CIDR notation).
	GuestIP string `yaml:"guest_ip,omitempty"`
}

// PortForward maps a host port to a VM port via iptables DNAT.
type PortForward struct {
	// HostPort is the port on the host machine.
	HostPort int `yaml:"host_port"`
	// VMPort is the port inside the guest VM.
	VMPort int `yaml:"vm_port"`
}

// HealthCheckConfig defines how to check if a service is healthy.
type HealthCheckConfig struct {
	// Type is the health check type: "http", "tcp", or "exec".
	Type string `yaml:"type"`
	// Target is the address or command depending on the type.
	// For HTTP: "http://guest-ip:port/path"
	// For TCP:  "guest-ip:port"
	// For Exec: not yet implemented
	// When empty, the agent composes it from Port, Path, and the allocated guest IP.
	Target string `yaml:"target,omitempty"`
	// Port is the service port for health checks. The agent uses this
	// together with the allocated guest IP to compose the Target when
	// Target is not set directly.
	Port int `yaml:"port,omitempty"`
	// Path is the HTTP path for health checks (e.g. "/health").
	// Only used when Type is "http".
	Path string `yaml:"path,omitempty"`
	// Interval is how often to run the check.
	Interval time.Duration `yaml:"interval"`
	// Timeout is the maximum time to wait for a check.
	Timeout time.Duration `yaml:"timeout"`
	// Retries is how many consecutive failures before marking unhealthy.
	Retries int `yaml:"retries"`
}

// AgentConfig holds the agent's own operational configuration.
type AgentConfig struct {
	// NodeName is this node's unique identifier.
	NodeName string `yaml:"node_name"`
	// StoreType is the config store backend: "git" or "s3".
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
}
