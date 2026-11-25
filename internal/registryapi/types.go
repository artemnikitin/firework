// Package registryapi defines the wire contract shared by agents and the
// control-plane registry.
package registryapi

import (
	"time"

	"github.com/artemnikitin/firework/internal/statusmodel"
)

type NodeState string

const (
	NodeStateReady    NodeState = "ready"
	NodeStateDraining NodeState = "draining"
	NodeStateDown     NodeState = "down"
)

type Resources struct {
	VCPUs    int `json:"vcpus"`
	MemoryMB int `json:"memory_mb"`
}

type StorageResources struct {
	LocalCapacityBytes  int64  `json:"local_capacity_bytes,omitempty"`
	SharedBackendID     string `json:"shared_backend_id,omitempty"`
	SharedCapacityBytes int64  `json:"shared_capacity_bytes,omitempty"`
}

type RegisterRequest struct {
	NodeID     string           `json:"node_id"`
	Generation int64            `json:"generation"`
	Labels     []string         `json:"labels,omitempty"`
	Capacity   Resources        `json:"capacity"`
	State      NodeState        `json:"state,omitempty"`
	HostIP     string           `json:"host_ip,omitempty"`
	Storage    StorageResources `json:"storage,omitempty"`
}

type HeartbeatRequest struct {
	NodeID      string                   `json:"node_id"`
	Generation  int64                    `json:"generation"`
	Capacity    Resources                `json:"capacity,omitempty"`
	Used        Resources                `json:"used,omitempty"`
	HostIP      string                   `json:"host_ip,omitempty"`
	AgentStatus *statusmodel.AgentStatus `json:"agent_status,omitempty"`
	Storage     StorageResources         `json:"storage,omitempty"`
}

// StateRequest changes the lifecycle state of the caller's current
// incarnation. Generation must equal the registered generation: a process
// that was replaced by a newer registration cannot change the newer one.
type StateRequest struct {
	State      NodeState `json:"state"`
	Generation int64     `json:"generation"`
}

type EnrollRequest struct {
	NodeID         string `json:"node_id"`
	BootstrapToken string `json:"bootstrap_token"`
	CSRPEM         string `json:"csr_pem"`
}

type RenewRequest struct {
	CSRPEM string `json:"csr_pem"`
}

type CertResponse struct {
	CertPEM   string    `json:"cert_pem"`
	ExpiresAt time.Time `json:"expires_at"`
}

type NodeResponse struct {
	NodeID     string    `json:"node_id"`
	Generation int64     `json:"generation"`
	State      NodeState `json:"state"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

// Stable registry error codes. Clients branch on these codes and the HTTP
// status, never on the message text.
const (
	ErrorCodeInvalidRequest     = "invalid_request"
	ErrorCodeUnauthorized       = "unauthorized"
	ErrorCodeForbidden          = "forbidden"
	ErrorCodeNodeNotRegistered  = "node_not_registered"
	ErrorCodeStaleGeneration    = "stale_generation"
	ErrorCodeNodeRetiring       = "node_retiring"
	ErrorCodeNodeRetired        = "node_retired"
	ErrorCodeRequestTooLarge    = "request_too_large"
	ErrorCodeInvalidAgentStatus = "invalid_agent_status"
	ErrorCodeInternal           = "internal_error"
)

// ErrorResponse is the bounded error envelope returned by every registry
// endpoint.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
