package controlplane

import (
	"time"

	"github.com/artemnikitin/firework/internal/statusmodel"
)

// These helpers are shared by the controller and the read-only projection so
// their lifecycle and freshness decisions cannot drift.

// nodeLifecycleState derives the operator-facing lifecycle state. An explicit
// shutdown is a lifecycle fact, not a transport observation, so a down node
// stays down after its last heartbeat ages out; only ready and draining
// records derive stale from last_seen_at.
func nodeLifecycleState(record NodeRecord, now time.Time, staleTTL time.Duration) string {
	switch record.State {
	case NodeStateDown:
		return "down"
	case NodeStateReady, NodeStateDraining:
		if record.LastSeenAt.IsZero() || now.Sub(record.LastSeenAt) > staleTTL {
			return "stale"
		}
		return string(record.State)
	default:
		return "unknown"
	}
}

// freshAgentStatus returns a node's agent status only when it is usable as
// current evidence: supported schema, not from an explicitly down node, and
// received within ttl on the registry's own clock. The agent's observed_at is
// an ordering hint from another machine's clock and does not gate freshness.
func freshAgentStatus(record NodeRecord, now time.Time, ttl time.Duration) (statusmodel.AgentStatus, bool) {
	if record.AgentStatus == nil || record.AgentStatus.SchemaVersion != statusmodel.SchemaVersion {
		return statusmodel.AgentStatus{}, false
	}
	if record.State == NodeStateDown {
		return statusmodel.AgentStatus{}, false
	}
	if record.LastSeenAt.IsZero() || now.Sub(record.LastSeenAt) > ttl {
		return statusmodel.AgentStatus{}, false
	}
	received := record.AgentStatusReceivedAt
	if received.IsZero() {
		// Records written before receipt tracking replaced their status on
		// every accepted heartbeat, so last_seen_at is its receipt time.
		received = record.LastSeenAt
	}
	if now.Sub(received) > ttl {
		return statusmodel.AgentStatus{}, false
	}
	return *record.AgentStatus, true
}

// statusUnavailableReason names why freshAgentStatus refused a node's status,
// so a down node, a stale heartbeat, a refused payload, a missing payload, an
// unsupported schema, and an old observation each read differently.
func statusUnavailableReason(record NodeRecord, now time.Time, ttl time.Duration) string {
	switch {
	case record.State == NodeStateDown:
		return "node_down"
	case record.LastSeenAt.IsZero() || now.Sub(record.LastSeenAt) > ttl:
		return "node_stale"
	case record.AgentStatus == nil && record.AgentStatusRejected != "":
		return record.AgentStatusRejected
	case record.AgentStatus == nil:
		return "agent_status_missing"
	case record.AgentStatus.SchemaVersion != statusmodel.SchemaVersion:
		return agentStatusRejectedUnsupportedSchema
	default:
		return "agent_status_stale"
	}
}
