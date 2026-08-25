package controlplane

import (
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

// NodeState is the lifecycle state of a node.
type NodeState string

const (
	NodeStateReady    NodeState = "ready"
	NodeStateDraining NodeState = "draining"
	NodeStateDown     NodeState = "down"
)

// Resources represents node resource quantities.
type Resources struct {
	VCPUs    int `json:"vcpus"`
	MemoryMB int `json:"memory_mb"`
}

// StorageResources is the provider-neutral storage admission information
// reported by an agent. Paths are intentionally not sent to the control plane.
type StorageResources struct {
	LocalCapacityBytes  int64  `json:"local_capacity_bytes,omitempty"`
	SharedBackendID     string `json:"shared_backend_id,omitempty"`
	SharedCapacityBytes int64  `json:"shared_capacity_bytes,omitempty"`
}

// NodeRecord is the registry source-of-truth for a node.
type NodeRecord struct {
	NodeID       string                   `json:"node_id"`
	Generation   int64                    `json:"generation"`
	State        NodeState                `json:"state"`
	Labels       []string                 `json:"labels,omitempty"`
	Capacity     Resources                `json:"capacity"`
	Used         Resources                `json:"used"`
	HostIP       string                   `json:"host_ip,omitempty"`
	RegisteredAt time.Time                `json:"registered_at,omitempty"`
	LastSeenAt   time.Time                `json:"last_seen_at"`
	UpdatedAt    time.Time                `json:"updated_at"`
	AgentStatus  *statusmodel.AgentStatus `json:"agent_status,omitempty"`
	Storage      StorageResources         `json:"storage,omitempty"`
}

// NodeRegisterRequest is the request payload for node registration.
type NodeRegisterRequest struct {
	NodeID     string           `json:"node_id"`
	Generation int64            `json:"generation"`
	Labels     []string         `json:"labels,omitempty"`
	Capacity   Resources        `json:"capacity"`
	State      NodeState        `json:"state,omitempty"`
	HostIP     string           `json:"host_ip,omitempty"`
	Storage    StorageResources `json:"storage,omitempty"`
}

// NodeHeartbeatRequest is the request payload for node heartbeat.
type NodeHeartbeatRequest struct {
	NodeID      string                   `json:"node_id"`
	Generation  int64                    `json:"generation"`
	Capacity    Resources                `json:"capacity,omitempty"`
	Used        Resources                `json:"used,omitempty"`
	HostIP      string                   `json:"host_ip,omitempty"`
	AgentStatus *statusmodel.AgentStatus `json:"agent_status,omitempty"`
	Storage     StorageResources         `json:"storage,omitempty"`
}

// NodeStateRequest updates node state.
type NodeStateRequest struct {
	State NodeState `json:"state"`
}

// DesiredRevision stores normalized services from events.
type DesiredRevision struct {
	Revision  string                 `json:"revision"`
	Source    string                 `json:"source,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
	Services  []config.ServiceConfig `json:"services"`
}

// PlacementRevision stores scheduler output.
type PlacementRevision struct {
	Revision        string              `json:"revision"`
	DesiredRevision string              `json:"desired_revision"`
	LeaderEpoch     int64               `json:"leader_epoch"`
	CreatedAt       time.Time           `json:"created_at"`
	NodeConfigs     []config.NodeConfig `json:"node_configs"`
	PendingServices []PendingPlacement  `json:"pending_services,omitempty"`
}

type PendingPlacement struct {
	Service    string `json:"service"`
	ReasonCode string `json:"reason_code"`
	Message    string `json:"message,omitempty"`
}

type VolumeResizeState string

const (
	VolumeResizePending VolumeResizeState = "pending"
	VolumeResizeApplied VolumeResizeState = "applied"
	VolumeResizeFailed  VolumeResizeState = "failed"
	// VolumeResizeRejected is a terminal sibling of VolumeResizeFailed. Failed
	// means an operation that failed and may succeed on retry; rejected means
	// the requested size is impossible for the current contents and will not
	// be retried without a new generation.
	VolumeResizeRejected VolumeResizeState = "rejected"
)

// knownVolumeResizeState reports whether a state is one this controller
// understands. An unrecognized value is carried through untouched rather than
// treated as invalid, so a record written by a newer control plane does not
// brick scheduling on an older one during a rollback.
func knownVolumeResizeState(state VolumeResizeState) bool {
	switch state {
	case VolumeResizePending, VolumeResizeApplied, VolumeResizeFailed, VolumeResizeRejected:
		return true
	default:
		return false
	}
}

// VolumeRecord retains volume identity and placement after workload removal.
type VolumeRecord struct {
	LogicalID        string            `json:"logical_id"`
	Type             config.VolumeType `json:"type"`
	BoundNode        string            `json:"bound_node,omitempty"`
	SharedBackendID  string            `json:"shared_backend_id,omitempty"`
	DesiredSizeBytes int64             `json:"desired_size_bytes"`
	AppliedSizeBytes int64             `json:"applied_size_bytes"`
	ResizeGeneration int64             `json:"resize_generation"`
	ResizeState      VolumeResizeState `json:"resize_state"`
	LastError        string            `json:"last_error,omitempty"`
	// RequestedSizeBytes preserves a size the cluster refused, so status can
	// show the operator's request next to the effective size actually running.
	//
	// The invariant that makes one field enough is that DesiredSizeBytes never
	// renders a size the cluster is not running: a capacity rejection is
	// refused before it is written, and a shrink rejection is acknowledged by
	// resetting DesiredSizeBytes to AppliedSizeBytes in the same write that
	// records RequestedSizeBytes.
	RequestedSizeBytes int64 `json:"requested_size_bytes,omitempty"`
	// RejectedReason is non-empty exactly while a rejection stands. It is the
	// signal the idempotence guard keys on, alongside RequestedSizeBytes.
	RejectedReason string `json:"rejected_reason,omitempty"`
	// RejectedAvailableBytes is the capacity figure the refusal was measured
	// against, for the operator who has to decide what to free.
	RejectedAvailableBytes int64 `json:"rejected_available_bytes,omitempty"`
	// RejectedAt records when the request was *first* refused. It is preserved
	// across ticks rather than restamped: restamping makes every comparison
	// unequal and turns a standing rejection into a write loop.
	RejectedAt time.Time `json:"rejected_at,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// rejectionStands reports whether the record currently carries a refused
// request. ResizeState deliberately does not carry a capacity rejection: it
// describes the last resize the agent actually attempted, and a request
// refused before it became a resize never entered that state machine.
func (r VolumeRecord) rejectionStands() bool { return r.RejectedReason != "" }

// clearRejection drops the rejection fields and reports whether anything
// changed, so a caller can avoid a no-op write.
func (r *VolumeRecord) clearRejection() bool {
	if r.RequestedSizeBytes == 0 && r.RejectedReason == "" && r.RejectedAvailableBytes == 0 && r.RejectedAt.IsZero() {
		return false
	}
	r.RequestedSizeBytes = 0
	r.RejectedReason = ""
	r.RejectedAvailableBytes = 0
	r.RejectedAt = time.Time{}
	return true
}

// RevisionPointer points to the current immutable revision.
type RevisionPointer struct {
	Revision  string    `json:"revision"`
	UpdatedAt time.Time `json:"updated_at"`
}

// LeaderLock stores controller leadership lease.
type LeaderLock struct {
	HolderID       string    `json:"holder_id"`
	LeaderEpoch    int64     `json:"leader_epoch"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	RenewedAt      time.Time `json:"renewed_at"`
}

// EventDedupeMarker marks a processed event.
type EventDedupeMarker struct {
	EventID    string    `json:"event_id"`
	ReceivedAt time.Time `json:"received_at"`
}

// EnrollRequest requests a node certificate.
type EnrollRequest struct {
	NodeID         string `json:"node_id"`
	BootstrapToken string `json:"bootstrap_token"`
	CSRPEM         string `json:"csr_pem"`
}

// RenewRequest requests node certificate rotation.
type RenewRequest struct {
	CSRPEM string `json:"csr_pem"`
}

// CertResponse is returned by enrollment endpoints.
type CertResponse struct {
	CertPEM   string    `json:"cert_pem"`
	ExpiresAt time.Time `json:"expires_at"`
}

// NodeResponse is a generic node response payload.
type NodeResponse struct {
	NodeID     string    `json:"node_id"`
	Generation int64     `json:"generation"`
	State      NodeState `json:"state"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

func stateRoot(prefix string) string { return strings.TrimSuffix(prefix, "/") + "/" }

func nodeRecordKey(prefix, nodeID string) (string, error) {
	if strings.TrimSpace(nodeID) == "" {
		return "", fmt.Errorf("node_id is empty")
	}
	if strings.Contains(nodeID, "/") {
		return "", fmt.Errorf("node_id must not contain '/'")
	}
	return path.Join(stateRoot(prefix), "registry", "nodes", nodeID+".json"), nil
}

func registryNodesPrefix(prefix string) string {
	return path.Join(stateRoot(prefix), "registry", "nodes") + "/"
}

func desiredRevisionKey(prefix, rev string) string {
	return path.Join(stateRoot(prefix), "desired", "revisions", rev+".json")
}

func desiredCurrentKey(prefix string) string {
	return path.Join(stateRoot(prefix), "desired", "current.json")
}

func placementRevisionKey(prefix, rev string) string {
	return path.Join(stateRoot(prefix), "placements", "revisions", rev+".json")
}

func placementCurrentKey(prefix string) string {
	return path.Join(stateRoot(prefix), "placements", "current.json")
}

func volumeRecordKey(prefix, service, volume string) (string, error) {
	if strings.TrimSpace(service) == "" || strings.Contains(service, "/") || strings.TrimSpace(volume) == "" || strings.Contains(volume, "/") {
		return "", fmt.Errorf("invalid volume logical id %q/%q", service, volume)
	}
	return path.Join(stateRoot(prefix), "volumes", service, volume+".json"), nil
}

func volumeRecordsPrefix(prefix string) string {
	return path.Join(stateRoot(prefix), "volumes") + "/"
}

func renderedNodeKey(prefix, rev, nodeID string) string {
	return path.Join(stateRoot(prefix), "rendered", "revisions", rev, "nodes", nodeID+".yaml")
}

func renderedCurrentKey(prefix string) string {
	return path.Join(stateRoot(prefix), "rendered", "current.json")
}

func legacyNodeConfigKey(prefix, nodeID string) string {
	return path.Join(stateRoot(prefix), "nodes", nodeID+".yaml")
}

func legacyNodesPrefix(prefix string) string {
	return path.Join(stateRoot(prefix), "nodes") + "/"
}

func dedupeKey(prefix, eventID string) string {
	return path.Join(stateRoot(prefix), "events", "dedupe", eventID+".json")
}

func controllerLockKey(prefix string) string {
	return path.Join(stateRoot(prefix), "locks", "controller.json")
}
