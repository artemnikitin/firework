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
	// Revision metadata is populated by the control plane so agents can report
	// convergence using stable revision IDs instead of provider write tokens.
	DesiredRevision   string `yaml:"desired_revision,omitempty"`
	PlacementRevision string `yaml:"placement_revision,omitempty"`
	RenderedRevision  string `yaml:"rendered_revision,omitempty"`
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
	// to KernelArgs, using encoded firework.env64.KEY=VALUE entries when
	// values contain whitespace. The guest's fc-init parses both formats
	// from /proc/cmdline.
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
	// Volumes are persistent ext4 images attached after the root filesystem.
	// SizeBytes is always resolved by the enricher/controller; agents never
	// apply application defaults independently.
	Volumes []VolumeConfig `yaml:"volumes,omitempty"`
}

// VolumeType identifies the persistence and placement semantics of a volume.
type VolumeType string

const (
	VolumeTypeLocal  VolumeType = "local"
	VolumeTypeShared VolumeType = "shared"
	// MaxServiceVolumes matches the additional virtio block-device range
	// exposed as /dev/vdb through /dev/vdz. The rootfs occupies /dev/vda.
	MaxServiceVolumes = 25
)

// VolumeConfig is the resolved, provider-neutral persistent-volume contract.
// BoundNode and SharedBackendID are system-owned fields and are not accepted
// in application input.
type VolumeConfig struct {
	Name             string     `yaml:"name" json:"name"`
	Type             VolumeType `yaml:"type" json:"type"`
	MountPath        string     `yaml:"mount_path" json:"mount_path"`
	SizeBytes        int64      `yaml:"size_bytes" json:"size_bytes"`
	BoundNode        string     `yaml:"bound_node,omitempty" json:"bound_node,omitempty"`
	SharedBackendID  string     `yaml:"shared_backend_id,omitempty" json:"shared_backend_id,omitempty"`
	ResizeGeneration int64      `yaml:"resize_generation,omitempty" json:"resize_generation,omitempty"`
}

// CrossNodeLink declares a dependency on a peer service running on a different
// node. The controller resolves the peer node's host IP and injects an env var
// of the form "<ip>:<host_port>". When Protocol is set, it prefixes the value
// as "<protocol>://<ip>:<host_port>".
type CrossNodeLink struct {
	// Service is the fully-qualified peer service name.
	Service string `yaml:"service"`
	// Env is the env var injected into THIS service.
	Env string `yaml:"env"`
	// HostPort is the forwarded port on the peer's host.
	HostPort int `yaml:"host_port"`
	// Protocol is an optional URL scheme. Empty preserves the legacy bare
	// "<ip>:<host_port>" format; for example, "http" produces
	// "http://<ip>:<host_port>".
	Protocol string `yaml:"protocol,omitempty"`
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
	HostPort int `yaml:"host_port" json:"host_port"`
	// VMPort is the port inside the guest VM.
	VMPort int `yaml:"vm_port" json:"vm_port"`
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
