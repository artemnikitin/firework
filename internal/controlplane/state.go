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
}

// NodeRegisterRequest is the request payload for node registration.
type NodeRegisterRequest struct {
	NodeID     string    `json:"node_id"`
	Generation int64     `json:"generation"`
	Labels     []string  `json:"labels,omitempty"`
	Capacity   Resources `json:"capacity"`
	State      NodeState `json:"state,omitempty"`
	HostIP     string    `json:"host_ip,omitempty"`
}

// NodeHeartbeatRequest is the request payload for node heartbeat.
type NodeHeartbeatRequest struct {
	NodeID      string                   `json:"node_id"`
	Generation  int64                    `json:"generation"`
	Capacity    Resources                `json:"capacity,omitempty"`
	Used        Resources                `json:"used,omitempty"`
	HostIP      string                   `json:"host_ip,omitempty"`
	AgentStatus *statusmodel.AgentStatus `json:"agent_status,omitempty"`
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
