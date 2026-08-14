package controlplane

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

// maximalAgentStatus builds the largest agent_status that validateAgentStatus
// accepts: every count at its cap and every bounded string at its limit, using
// 4-byte runes because BoundedMessage bounds runes rather than bytes.
func maximalAgentStatus() *statusmodel.AgentStatus {
	wide := strings.Repeat("\U0001F680", statusmodel.MaxMessageLen) // 4 bytes per rune
	name := strings.Repeat("a", statusmodel.MaxServiceNameLen)
	revision := strings.Repeat("r", statusmodel.MaxRevisionLen)
	reason := strings.Repeat("c", statusmodel.MaxReasonCodeLen)

	status := &statusmodel.AgentStatus{
		SchemaVersion:     statusmodel.SchemaVersion,
		NodeID:            name,
		AgentVersion:      revision,
		ObservedAt:        time.Now().UTC(),
		Phase:             statusmodel.PhaseReady,
		DesiredRevision:   revision,
		PlacementRevision: revision,
		ObservedRevision:  revision,
		AppliedRevision:   revision,
		ReasonCode:        reason,
		Message:           wide,
	}
	for i := 0; i < statusmodel.MaxConditions; i++ {
		status.Conditions = append(status.Conditions, statusmodel.Condition{
			Type:             strings.Repeat("t", statusmodel.MaxConditionTypeLen),
			Status:           statusmodel.ConditionTrue,
			ReasonCode:       reason,
			Message:          wide,
			LastTransitionAt: time.Now().UTC(),
		})
	}
	for i := 0; i < statusmodel.MaxServices; i++ {
		service := statusmodel.ServiceStatus{
			Name: name, VMState: "running", Health: "healthy",
			ReasonCode: reason, Message: wide,
			NetworkAddress: revision, HealthCheckType: reason,
		}
		for v := 0; v < config.MaxServiceVolumes; v++ {
			service.Volumes = append(service.Volumes, statusmodel.VolumeStatus{
				LogicalID: name, Type: reason, MountPath: revision,
				BoundNode: name, SharedBackendID: name, LastError: wide,
			})
		}
		status.Services = append(status.Services, service)
	}
	return status
}

// The request cap must exceed the largest status the validator accepts.
// Otherwise MaxBytesReader truncates the body, readJSON fails, and the handler
// returns 400 before reaching validatedHeartbeatAgentStatus — rejecting the
// node's liveness heartbeat, not just its telemetry. LastSeenAt then stops
// advancing and the node goes stale and down, which is the opposite of what
// treating status as optional telemetry is meant to achieve.
func TestMaxRegistryRequestBytesExceedsLargestValidHeartbeat(t *testing.T) {
	body, err := json.Marshal(NodeHeartbeatRequest{
		NodeID:      strings.Repeat("a", statusmodel.MaxServiceNameLen),
		Generation:  1,
		HostIP:      "255.255.255.255",
		AgentStatus: maximalAgentStatus(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	t.Logf("largest valid heartbeat body: %d bytes (%.2f MiB); cap is %d bytes (%.2f MiB)",
		len(body), float64(len(body))/(1<<20),
		maxRegistryRequestBytes, float64(maxRegistryRequestBytes)/(1<<20))
	if len(body) >= maxRegistryRequestBytes {
		t.Fatalf("a valid heartbeat of %d bytes exceeds the %d byte request cap; "+
			"it would be rejected as invalid json and drop the node's liveness",
			len(body), maxRegistryRequestBytes)
	}
}
