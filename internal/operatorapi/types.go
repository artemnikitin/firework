// Package operatorapi defines the response contract shared by the control
// plane's operator API and its clients.
package operatorapi

import "time"

const APIVersion = "v1"

type ListEnvelope[T any] struct {
	APIVersion string    `json:"api_version"`
	ObservedAt time.Time `json:"observed_at"`
	Count      int       `json:"count"`
	Items      []T       `json:"items"`
}

type Resources struct {
	VCPUs    int `json:"vcpus"`
	MemoryMB int `json:"memory_mb"`
}

// StorageCapacitySummary reports reserved storage capacity. It does not
// report filesystem or block-device utilization.
type StorageCapacitySummary struct {
	CapacityBytes  int64 `json:"capacity_bytes"`
	AllocatedBytes int64 `json:"allocated_bytes"`
	AvailableBytes int64 `json:"available_bytes"`
}

type NodeStorageSummary struct {
	Local           StorageCapacitySummary `json:"local"`
	Shared          StorageCapacitySummary `json:"shared"`
	SharedBackendID string                 `json:"shared_backend_id,omitempty"`
}

type NodeSummary struct {
	NodeID           string             `json:"node_id"`
	Labels           []string           `json:"labels,omitempty"`
	State            string             `json:"state"`
	LastSeenAt       time.Time          `json:"last_seen_at,omitempty"`
	StatusAgeSeconds int64              `json:"status_age_seconds,omitempty"`
	AgentVersion     string             `json:"agent_version,omitempty"`
	Capacity         Resources          `json:"capacity"`
	Allocated        Resources          `json:"allocated"`
	Available        Resources          `json:"available"`
	Storage          NodeStorageSummary `json:"storage"`
	DesiredServices  int                `json:"desired_services"`
	RunningServices  int                `json:"running_services"`
	ReasonCode       string             `json:"reason_code,omitempty"`
}

type Condition struct {
	Type             string    `json:"type"`
	Status           string    `json:"status"`
	ReasonCode       string    `json:"reason_code,omitempty"`
	Message          string    `json:"message,omitempty"`
	LastTransitionAt time.Time `json:"last_transition_at,omitempty"`
}

type NodeDetail struct {
	APIVersion string    `json:"api_version"`
	ObservedAt time.Time `json:"observed_at"`
	NodeSummary
	HostIP            string           `json:"host_ip,omitempty"`
	RegisteredAt      time.Time        `json:"registered_at,omitempty"`
	UpdatedAt         time.Time        `json:"updated_at,omitempty"`
	DesiredRevision   string           `json:"desired_revision,omitempty"`
	PlacementRevision string           `json:"placement_revision,omitempty"`
	ObservedRevision  string           `json:"observed_revision,omitempty"`
	AppliedRevision   string           `json:"applied_revision,omitempty"`
	Reconciliation    string           `json:"reconciliation"`
	Message           string           `json:"message,omitempty"`
	StatusMissing     bool             `json:"status_missing"`
	StatusStale       bool             `json:"status_stale"`
	Conditions        []Condition      `json:"conditions,omitempty"`
	Services          []ServiceSummary `json:"services"`
}

// RevisionStatus is derived from current desired, placement, rendered, and
// registry state. The control plane does not persist it.
type RevisionStatus struct {
	APIVersion        string    `json:"api_version"`
	ObservedAt        time.Time `json:"observed_at"`
	Phase             string    `json:"phase"`
	DesiredRevision   string    `json:"desired_revision,omitempty"`
	PlacementRevision string    `json:"placement_revision,omitempty"`
	RenderedRevision  string    `json:"rendered_revision,omitempty"`
	RelevantNodes     int       `json:"relevant_nodes"`
	ConvergedNodes    []string  `json:"converged_nodes"`
	DegradedNodes     []string  `json:"degraded_nodes"`
	ProgressingNodes  []string  `json:"progressing_nodes"`
	FailedNodes       []string  `json:"failed_nodes"`
	StaleNodes        []string  `json:"stale_nodes"`
	DownNodes         []string  `json:"down_nodes"`
	UnknownNodes      []string  `json:"unknown_nodes"`
	ReasonCode        string    `json:"reason_code,omitempty"`
	Message           string    `json:"message,omitempty"`
}

// VolumeAllocationSummary reserves the larger desired or applied size for
// each volume in AllocatedBytes.
type VolumeAllocationSummary struct {
	Count          int   `json:"count"`
	DesiredBytes   int64 `json:"desired_bytes"`
	AppliedBytes   int64 `json:"applied_bytes"`
	AllocatedBytes int64 `json:"allocated_bytes"`
}

type ServiceStorageSummary struct {
	Local  VolumeAllocationSummary `json:"local"`
	Shared VolumeAllocationSummary `json:"shared"`
}

type ServiceSummary struct {
	Name             string                `json:"name"`
	Node             string                `json:"node,omitempty"`
	State            string                `json:"state"`
	Health           string                `json:"health"`
	VCPUs            int                   `json:"vcpus"`
	MemoryMB         int                   `json:"memory_mb"`
	Storage          ServiceStorageSummary `json:"storage"`
	ObservedAt       time.Time             `json:"observed_at,omitempty"`
	LastTransitionAt time.Time             `json:"last_transition_at,omitempty"`
	ReasonCode       string                `json:"reason_code,omitempty"`
	Message          string                `json:"message,omitempty"`
}

type ServiceHealthDetail struct {
	Type          string    `json:"type,omitempty"`
	State         string    `json:"state"`
	LastCheckedAt time.Time `json:"last_checked_at,omitempty"`
	Failures      int       `json:"failures"`
	LastError     string    `json:"last_error,omitempty"`
}

type PortForward struct {
	HostPort int `json:"host_port"`
	VMPort   int `json:"vm_port"`
}

type VolumeStatus struct {
	LogicalID          string `json:"logical_id"`
	Type               string `json:"type"`
	MountPath          string `json:"mount_path"`
	BoundNode          string `json:"bound_node,omitempty"`
	SharedBackendID    string `json:"shared_backend_id,omitempty"`
	DesiredSizeBytes   int64  `json:"desired_size_bytes"`
	AppliedSizeBytes   int64  `json:"applied_size_bytes,omitempty"`
	ResizeGeneration   int64  `json:"resize_generation,omitempty"`
	State              string `json:"state"`
	LastError          string `json:"last_error,omitempty"`
	RequestedSizeBytes int64  `json:"requested_size_bytes,omitempty"`
	Rejected           bool   `json:"rejected,omitempty"`
	RejectedReason     string `json:"rejected_reason,omitempty"`
}

type ServiceDetail struct {
	APIVersion string    `json:"api_version"`
	ObservedAt time.Time `json:"observed_at"`
	ServiceSummary
	ServiceObservedAt time.Time           `json:"service_observed_at,omitempty"`
	DesiredImage      string              `json:"desired_image,omitempty"`
	DesiredKernel     string              `json:"desired_kernel,omitempty"`
	DesiredNode       string              `json:"desired_node,omitempty"`
	ActualNode        string              `json:"actual_node,omitempty"`
	PID               int                 `json:"pid,omitempty"`
	HealthCheck       ServiceHealthDetail `json:"health_check"`
	NetworkAddress    string              `json:"network_address,omitempty"`
	PortForwards      []PortForward       `json:"port_forwards,omitempty"`
	RoutingHostname   string              `json:"routing_hostname,omitempty"`
	PublicURL         string              `json:"public_url,omitempty"`
	RestartCount      int                 `json:"restart_count"`
	DesiredRevision   string              `json:"desired_revision,omitempty"`
	PlacementRevision string              `json:"placement_revision,omitempty"`
	RenderedRevision  string              `json:"rendered_revision,omitempty"`
	AppliedRevision   string              `json:"applied_revision,omitempty"`
	Volumes           []VolumeStatus      `json:"volumes,omitempty"`
}
