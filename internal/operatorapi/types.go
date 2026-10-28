// Package operatorapi defines the response contract shared by the control
// plane's operator API and its clients.
package operatorapi

import "time"

const APIVersion = "v1"

type ListEnvelope[T any] struct {
	APIVersion string              `json:"api_version"`
	ObservedAt time.Time           `json:"observed_at"`
	Count      int                 `json:"count"`
	Items      []T                 `json:"items"`
	Problems   []ProjectionProblem `json:"problems,omitempty"`
	Limit      int                 `json:"limit,omitempty"`
	Offset     int                 `json:"offset,omitempty"`
	Total      int                 `json:"total,omitempty"`
	HasMore    bool                `json:"has_more,omitempty"`
}

// ProjectionProblem is a stored record the projection could not read or
// interpret and therefore left out of the response. ID is the record's
// identifier taken from its storage key, never its contents.
type ProjectionProblem struct {
	Kind       string `json:"kind"` // node or volume
	ID         string `json:"id"`
	ReasonCode string `json:"reason_code"` // record_unreadable or record_invalid
}

type Resources struct {
	VCPUs    int `json:"vcpus"`
	MemoryMB int `json:"memory_mb"`
}

// StorageCapacitySummary reports reserved storage capacity. It does not
// report filesystem or block-device utilization. Configured is false when the
// node has no such pool, so zero capacity is never ambiguous; Unbounded marks
// a configured pool without aggregate capacity (for example, elastic shared
// storage), whose capacity and available bytes are then not meaningful.
type StorageCapacitySummary struct {
	Configured     bool  `json:"configured"`
	Unbounded      bool  `json:"unbounded,omitempty"`
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
	HostIP                      string              `json:"host_ip,omitempty"`
	RegisteredAt                time.Time           `json:"registered_at,omitempty"`
	UpdatedAt                   time.Time           `json:"updated_at,omitempty"`
	DesiredRevision             string              `json:"desired_revision,omitempty"`
	PlacementRevision           string              `json:"placement_revision,omitempty"`
	ObservedRevision            string              `json:"observed_revision,omitempty"`
	AppliedRevision             string              `json:"applied_revision,omitempty"`
	Reconciliation              string              `json:"reconciliation"`
	Message                     string              `json:"message,omitempty"`
	StatusMissing               bool                `json:"status_missing"`
	StatusStale                 bool                `json:"status_stale"`
	Conditions                  []Condition         `json:"conditions,omitempty"`
	Services                    []ServiceSummary    `json:"services"`
	ActualOnlyServices          []ActualOnlyService `json:"actual_only_services,omitempty"`
	ActualOnlyServicesTruncated bool                `json:"actual_only_services_truncated,omitempty"`
	MaxGeneration               int64               `json:"max_generation,omitempty"`
	RetiredAt                   time.Time           `json:"retired_at,omitempty"`
	RetirementReason            string              `json:"retirement_reason,omitempty"`
	RevivalHistory              []NodeRevivalAudit  `json:"revival_history,omitempty"`
	RevivalHistoryTruncated     bool                `json:"revival_history_truncated,omitempty"`
}

type ActualOnlyService struct {
	Name       string `json:"name"`
	VMState    string `json:"vm_state"`
	PID        int    `json:"pid,omitempty"`
	ReasonCode string `json:"reason_code,omitempty"`
	Message    string `json:"message,omitempty"`
}

type NodeRevivalAudit struct {
	ClearedGeneration      int64     `json:"cleared_generation"`
	ClearedAt              time.Time `json:"cleared_at"`
	OperatorReason         string    `json:"operator_reason"`
	ExternalFenceReference string    `json:"external_fence_reference,omitempty"`
	RetainedPoolReference  string    `json:"retained_pool_reference,omitempty"`
	Outcome                string    `json:"outcome"`
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
	// Services counts desired services by their projected /v1/services
	// state, so the overview needs no second request to show unhealthy or
	// pending work. UnhealthyServices names the unhealthy ones.
	Services                        ServiceCounts       `json:"services"`
	UnhealthyServices               []string            `json:"unhealthy_services"`
	PreviousNodeUnconfirmedServices []string            `json:"previous_node_unconfirmed_services,omitempty"`
	Problems                        []ProjectionProblem `json:"problems,omitempty"`
	ReasonCode                      string              `json:"reason_code,omitempty"`
	Message                         string              `json:"message,omitempty"`
}

// ServiceCounts summarizes desired services. Ready counts running services
// whose health check passed or that have none.
type ServiceCounts struct {
	Desired   int `json:"desired"`
	Running   int `json:"running"`
	Ready     int `json:"ready"`
	Pending   int `json:"pending"`
	Unhealthy int `json:"unhealthy"`
	Unknown   int `json:"unknown"`
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

// VolumeSummary combines the durable resize record with the desired config
// and the current node/backend availability. State is a derived lifecycle
// value; ResizeState preserves the underlying durable resize protocol state.
type VolumeSummary struct {
	LogicalID          string `json:"logical_id"`
	Service            string `json:"service"`
	Name               string `json:"name"`
	Type               string `json:"type,omitempty"`
	MountPath          string `json:"mount_path,omitempty"`
	BoundNode          string `json:"bound_node,omitempty"`
	SharedBackendID    string `json:"shared_backend_id,omitempty"`
	Desired            bool   `json:"desired"`
	State              string `json:"state"`
	ReasonCode         string `json:"reason_code,omitempty"`
	Message            string `json:"message,omitempty"`
	DesiredSizeBytes   int64  `json:"desired_size_bytes,omitempty"`
	AppliedSizeBytes   int64  `json:"applied_size_bytes,omitempty"`
	RequestedSizeBytes int64  `json:"requested_size_bytes,omitempty"`
	ResizeGeneration   int64  `json:"resize_generation,omitempty"`
	ResizeState        string `json:"resize_state,omitempty"`
	Rejected           bool   `json:"rejected,omitempty"`
	RejectedReason     string `json:"rejected_reason,omitempty"`
	LastError          string `json:"last_error,omitempty"`
	ObservedState      string `json:"observed_state,omitempty"`
}

type VolumeDetail struct {
	APIVersion string    `json:"api_version"`
	ObservedAt time.Time `json:"observed_at"`
	VolumeSummary
}

type ServiceSummary struct {
	Name                    string                `json:"name"`
	Node                    string                `json:"node,omitempty"`
	State                   string                `json:"state"`
	Health                  string                `json:"health"`
	VCPUs                   int                   `json:"vcpus"`
	MemoryMB                int                   `json:"memory_mb"`
	Storage                 ServiceStorageSummary `json:"storage"`
	ObservedAt              time.Time             `json:"observed_at,omitempty"`
	LastTransitionAt        time.Time             `json:"last_transition_at,omitempty"`
	ReasonCode              string                `json:"reason_code,omitempty"`
	Message                 string                `json:"message,omitempty"`
	PreviousNodeUnconfirmed bool                  `json:"previous_node_unconfirmed,omitempty"`
	PreviousNodes           []string              `json:"previous_nodes,omitempty"`
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
