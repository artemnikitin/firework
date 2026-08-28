package controlplane

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/agent"
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/healthcheck"
	"github.com/artemnikitin/firework/internal/registryapi"
	"github.com/artemnikitin/firework/internal/statusmodel"
	"github.com/artemnikitin/firework/internal/vm"
)

// maximalAgentStatus builds the largest well-behaved agent_status: every
// count at its cap and every field validateAgentStatus actually bounds filled
// to its limit. Message-shaped fields (Message, Condition.Message,
// ServiceStatus.Message, VolumeStatus.LastError) are accept-then-truncate via
// BoundedMessage rather than rejected by the validator, so there is no
// enforced limit to fill to; they are set to statusmodel.MaxMessageLen worth
// of 4-byte runes anyway, matching what a well-behaved agent gains nothing by
// exceeding since BoundedMessage would just discard the rest. Every other
// bounded string field is enforced in bytes, so it is filled with
// single-byte characters instead — 4-byte runes there would just be rejected
// as over length, understating what a byte-capped field can actually hold.
//
// Condition types and service names must additionally be unique or
// validateAgentStatus rejects the status as a duplicate, so each is built
// from a repeated fill plus a distinct numeric suffix, at the exact byte cap.
func maximalAgentStatus(nodeID string) *statusmodel.AgentStatus {
	wide := strings.Repeat("\U0001F680", statusmodel.MaxMessageLen) // 4 bytes per rune
	revision := strings.Repeat("r", statusmodel.MaxRevisionLen)
	reason := strings.Repeat("c", statusmodel.MaxReasonCodeLen)

	status := &statusmodel.AgentStatus{
		SchemaVersion:     statusmodel.SchemaVersion,
		NodeID:            nodeID,
		AgentVersion:      strings.Repeat("v", statusmodel.MaxAgentVersionLen),
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
			Type:             uniqueAtCap(i, statusmodel.MaxConditionTypeLen),
			Status:           statusmodel.ConditionTrue,
			ReasonCode:       reason,
			Message:          wide,
			LastTransitionAt: time.Now().UTC(),
		})
	}
	for i := 0; i < statusmodel.MaxServices; i++ {
		service := statusmodel.ServiceStatus{
			Name:            uniqueAtCap(i, statusmodel.MaxServiceNameLen),
			VMState:         strings.Repeat("s", statusmodel.MaxEnumLen),
			Health:          strings.Repeat("h", statusmodel.MaxEnumLen),
			HealthCheckType: strings.Repeat("k", statusmodel.MaxEnumLen),
			NetworkAddress:  strings.Repeat("n", statusmodel.MaxNetworkAddressLen),
			ReasonCode:      reason,
			Message:         wide,
		}
		for v := 0; v < config.MaxServiceVolumes; v++ {
			service.Volumes = append(service.Volumes, statusmodel.VolumeStatus{
				LogicalID:       strings.Repeat("l", statusmodel.MaxLogicalIDLen),
				Type:            strings.Repeat("t", statusmodel.MaxEnumLen),
				MountPath:       strings.Repeat("m", statusmodel.MaxMountPathLen),
				BoundNode:       strings.Repeat("b", statusmodel.MaxVolumeIDLen),
				SharedBackendID: strings.Repeat("d", statusmodel.MaxVolumeIDLen),
				State:           strings.Repeat("e", statusmodel.MaxEnumLen),
				LastError:       wide,
				// The rejection fields are additive but still per-volume, so
				// they have to be at their bound here or the measured worst
				// case understates what an agent can actually send.
				DesiredSizeBytes:   1<<63 - 1,
				AppliedSizeBytes:   1<<63 - 1,
				ResizeGeneration:   1<<63 - 1,
				RequestedSizeBytes: 1<<63 - 1,
				Rejected:           true,
				RejectedReason:     strings.Repeat("r", statusmodel.MaxReasonCodeLen),
			})
		}
		status.Services = append(status.Services, service)
	}
	return status
}

// uniqueAtCap returns a string of exactly maxLen bytes: a repeated fill with
// a zero-padded numeric suffix distinct per i, so a caller building many
// values that must all be unique (condition types, service names) can do so
// without any of them going under the byte cap being tested.
func uniqueAtCap(i, maxLen int) string {
	suffix := fmt.Sprintf("%08d", i)
	return strings.Repeat("x", maxLen-len(suffix)) + suffix
}

// The request cap must exceed the largest status the validator accepts.
// Otherwise MaxBytesReader truncates the body, readJSON fails, and the handler
// returns 400 before reaching validatedHeartbeatAgentStatus — rejecting the
// node's liveness heartbeat, not just its telemetry. LastSeenAt then stops
// advancing and the node goes stale and down, which is the opposite of what
// treating status as optional telemetry is meant to achieve.
func TestMaxRegistryRequestBytesExceedsLargestValidHeartbeat(t *testing.T) {
	nodeID := strings.Repeat("a", statusmodel.MaxServiceNameLen)
	status := maximalAgentStatus(nodeID)

	// Prove this is actually accepted, not merely serializable. The original
	// version of this test built a status with duplicate condition types and
	// duplicate service names, which validateAgentStatus rejects, and never
	// called it — so the "largest accepted" claim was unverified.
	if _, err := validatedHeartbeatAgentStatus(nodeID, status); err != nil {
		t.Fatalf("maximalAgentStatus was rejected by the real validator, so it does not describe the largest accepted heartbeat: %v", err)
	}

	body, err := json.Marshal(registryapi.HeartbeatRequest{
		NodeID:      nodeID,
		Generation:  1,
		HostIP:      "255.255.255.255",
		AgentStatus: status,
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

// maxLengthVolumeName returns a distinct 63-character DNS-label-like volume
// name for index j — the length internal/enricher's volumeNamePattern caps
// volume names to.
func maxLengthVolumeName(j int) string {
	suffix := fmt.Sprintf("%02d", j)
	return strings.Repeat("v", 63-len(suffix)-1) + "-" + suffix
}

// TestMaxRegistryRequestBytesThroughAgentStatusPath closes a gap the test
// above leaves open: maximalAgentStatus hand-types every VolumeStatus field,
// including LogicalID and MountPath, which a real agent instead *computes*
// from config (agent.BuildVolumeStatuses: service.Name+"/"+volume.Name, and
// desired.MountPath bounded through statusmodel.BoundedPath). Hand-typed
// values prove the validator's bounds are internally consistent; they do not
// prove a real agent, fed a realistic MaxServices x config.MaxServiceVolumes
// config, actually produces bytes under maxRegistryRequestBytes. This test
// builds real config.ServiceConfig/VolumeConfig at maximal realistic sizes,
// runs them through agent.BuildVolumeStatuses, and only fills in the fields
// that function does not set (State, LastError, and the ServiceStatus
// wrapper around it) to reach the same worst case.
func TestMaxRegistryRequestBytesThroughAgentStatusPath(t *testing.T) {
	nodeID := strings.Repeat("a", statusmodel.MaxServiceNameLen)
	reason := strings.Repeat("c", statusmodel.MaxReasonCodeLen)
	wide := strings.Repeat("\U0001F680", statusmodel.MaxMessageLen)
	maxMountPath := "/" + strings.Repeat("m", statusmodel.MaxMountPathLen-1)

	status := &statusmodel.AgentStatus{
		SchemaVersion: statusmodel.SchemaVersion,
		NodeID:        nodeID,
		AgentVersion:  strings.Repeat("v", statusmodel.MaxAgentVersionLen),
		ObservedAt:    time.Now().UTC(),
		Phase:         statusmodel.PhaseReady,
		ReasonCode:    reason,
		Message:       wide,
	}
	for i := 0; i < statusmodel.MaxConditions; i++ {
		status.Conditions = append(status.Conditions, statusmodel.Condition{
			Type: uniqueAtCap(i, statusmodel.MaxConditionTypeLen), Status: statusmodel.ConditionTrue,
			ReasonCode: reason, Message: wide, LastTransitionAt: time.Now().UTC(),
		})
	}
	for i := 0; i < statusmodel.MaxServices; i++ {
		svcName := uniqueAtCap(i, statusmodel.MaxServiceNameLen)

		volumes := make([]config.VolumeConfig, config.MaxServiceVolumes)
		for v := range volumes {
			volumes[v] = config.VolumeConfig{
				Name: maxLengthVolumeName(v), Type: config.VolumeTypeLocal, MountPath: maxMountPath,
				BoundNode:       strings.Repeat("b", statusmodel.MaxVolumeIDLen),
				SharedBackendID: strings.Repeat("d", statusmodel.MaxVolumeIDLen),
			}
		}
		// The real send path (internal/agent/status.go's refreshAgentStatus)
		// sets State/LastError from live VM/volume-manager state after
		// calling BuildVolumeStatuses, not inside it; fill them in the same
		// way here to reach the same worst case.
		volumeStatuses := agent.BuildVolumeStatuses(config.ServiceConfig{Name: svcName, Volumes: volumes}, nil)
		for v := range volumeStatuses {
			volumeStatuses[v].State = strings.Repeat("e", statusmodel.MaxEnumLen)
			volumeStatuses[v].LastError = statusmodel.BoundedMessage(wide)
		}

		status.Services = append(status.Services, statusmodel.ServiceStatus{
			Name: svcName, VMState: strings.Repeat("s", statusmodel.MaxEnumLen),
			Health: strings.Repeat("h", statusmodel.MaxEnumLen), HealthCheckType: strings.Repeat("k", statusmodel.MaxEnumLen),
			NetworkAddress: strings.Repeat("n", statusmodel.MaxNetworkAddressLen),
			ReasonCode:     reason, Message: wide, Volumes: volumeStatuses,
		})
	}

	if _, err := validatedHeartbeatAgentStatus(nodeID, status); err != nil {
		t.Fatalf("agent-produced status was rejected by the real validator: %v", err)
	}
	body, err := json.Marshal(registryapi.HeartbeatRequest{
		NodeID: nodeID, Generation: 1, HostIP: "255.255.255.255", AgentStatus: status,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	t.Logf("agent-path heartbeat body: %d bytes (%.2f MiB); cap is %d bytes (%.2f MiB)",
		len(body), float64(len(body))/(1<<20),
		maxRegistryRequestBytes, float64(maxRegistryRequestBytes)/(1<<20))
	if len(body) >= maxRegistryRequestBytes {
		t.Fatalf("an agent-produced heartbeat of %d bytes exceeds the %d byte request cap — "+
			"this is the reviewer's actual scenario: a realistic config pushed the real send path over the limit",
			len(body), maxRegistryRequestBytes)
	}
}

// TestValidateAgentStatusRejectsOversizedFields locks in that every string
// field scaling with MaxServices or config.MaxServiceVolumes and lacking a
// truncation fallback is actually bounded, one field at a time. Without
// these, a single oversized field (AgentVersion and VolumeStatus.MountPath
// were previously unbounded) would not be caught by any test, and the 16 MiB
// proof above would be checking a status that a real agent could exceed.
// Message-shaped fields are deliberately not covered here — see
// TestApplyHeartbeatAgentStatusValidatesIdentityAndBoundsMessages for their
// accept-then-truncate contract.
func TestValidateAgentStatusRejectsOversizedFields(t *testing.T) {
	nodeID := strings.Repeat("a", statusmodel.MaxServiceNameLen)

	tests := []struct {
		name   string
		mutate func(*statusmodel.AgentStatus)
	}{
		{"agent_version", func(s *statusmodel.AgentStatus) {
			s.AgentVersion = strings.Repeat("v", statusmodel.MaxAgentVersionLen+1)
		}},
		{"service vm_state", func(s *statusmodel.AgentStatus) {
			s.Services[0].VMState = strings.Repeat("s", statusmodel.MaxEnumLen+1)
		}},
		{"service health", func(s *statusmodel.AgentStatus) {
			s.Services[0].Health = strings.Repeat("h", statusmodel.MaxEnumLen+1)
		}},
		{"service health_check_type", func(s *statusmodel.AgentStatus) {
			s.Services[0].HealthCheckType = strings.Repeat("k", statusmodel.MaxEnumLen+1)
		}},
		{"service network_address", func(s *statusmodel.AgentStatus) {
			s.Services[0].NetworkAddress = strings.Repeat("n", statusmodel.MaxNetworkAddressLen+1)
		}},
		{"volume logical_id", func(s *statusmodel.AgentStatus) {
			s.Services[0].Volumes[0].LogicalID = strings.Repeat("l", statusmodel.MaxLogicalIDLen+1)
		}},
		{"volume type", func(s *statusmodel.AgentStatus) {
			s.Services[0].Volumes[0].Type = strings.Repeat("t", statusmodel.MaxEnumLen+1)
		}},
		{"volume mount_path", func(s *statusmodel.AgentStatus) {
			s.Services[0].Volumes[0].MountPath = strings.Repeat("m", statusmodel.MaxMountPathLen+1)
		}},
		{"volume bound_node", func(s *statusmodel.AgentStatus) {
			s.Services[0].Volumes[0].BoundNode = strings.Repeat("b", statusmodel.MaxVolumeIDLen+1)
		}},
		{"volume shared_backend_id", func(s *statusmodel.AgentStatus) {
			s.Services[0].Volumes[0].SharedBackendID = strings.Repeat("d", statusmodel.MaxVolumeIDLen+1)
		}},
		{"volume state", func(s *statusmodel.AgentStatus) {
			s.Services[0].Volumes[0].State = strings.Repeat("e", statusmodel.MaxEnumLen+1)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := maximalAgentStatus(nodeID)
			test.mutate(status)
			if _, err := validatedHeartbeatAgentStatus(nodeID, status); err == nil {
				t.Fatalf("%s over the byte/rune cap was accepted", test.name)
			}
		})
	}
}

// TestKnownEnumValuesFitMaxEnumLen locks in the assumption behind MaxEnumLen:
// every real VMState/Health/HealthCheckType/VolumeStatus.Type/
// VolumeStatus.State value this codebase actually emits is well inside the
// bound, so a real agent never loses its telemetry to it. These are string
// literals rather than a closed exported set (volume states in particular are
// assigned as bare strings in internal/agent/status.go), so this list must be
// kept in sync by hand rather than by the compiler — but MaxEnumLen has a 2x+
// margin over the longest of these today, so drift would have to be
// significant before it matters.
func TestKnownEnumValuesFitMaxEnumLen(t *testing.T) {
	values := []string{
		string(vm.StateRunning), string(vm.StateStopping), string(vm.StateStopped),
		string(vm.StateFailed), string(vm.StateRecoveryPending),
		string(healthcheck.StatusHealthy), string(healthcheck.StatusUnhealthy),
		"unknown", "not_configured", // internal/agent/status.go ServiceStatus.Health fallbacks
		"http", "tcp", "exec", // internal/config HealthCheck.Type
		"pending", "prepared", "error", // internal/agent/status.go VolumeStatus.State
		string(config.VolumeTypeLocal), string(config.VolumeTypeShared),
	}
	for _, v := range values {
		if len(v) > statusmodel.MaxEnumLen {
			t.Fatalf("enum value %q is %d bytes, exceeding MaxEnumLen (%d)", v, len(v), statusmodel.MaxEnumLen)
		}
	}
}

// TestValidateVolumeStatusAcceptsRealisticComposedLogicalID guards the
// composition visibility.go actually performs:
// `service.Name + "/" + volume.Name`. Service names are capped at
// statusmodel.MaxServiceNameLen (128) by the enricher, and volume names at 63
// characters by volumeNamePattern; MaxLogicalIDLen must cover the combined
// worst case or a legitimately-named, legitimately-configured service would
// have its entire agent_status dropped by validateAgentStatus.
func TestValidateVolumeStatusAcceptsRealisticComposedLogicalID(t *testing.T) {
	serviceName := strings.Repeat("s", statusmodel.MaxServiceNameLen)
	volumeName := strings.Repeat("v", 63)
	logicalID := serviceName + "/" + volumeName

	volume := statusmodel.VolumeStatus{LogicalID: logicalID, Type: "local", MountPath: "/data", State: "prepared"}
	if err := validateVolumeStatus(serviceName, volume); err != nil {
		t.Fatalf("a realistic max-length service/volume name pair was rejected: %v", err)
	}
}
