package controlplane

import (
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/statusmodel"
)

func TestApplyHeartbeatAgentStatusClearsCachedStatusForOldAgent(t *testing.T) {
	record := NodeRecord{AgentStatus: &statusmodel.AgentStatus{SchemaVersion: statusmodel.SchemaVersion}}
	if err := applyHeartbeatAgentStatus(&record, "node-1", nil); err != nil {
		t.Fatal(err)
	}
	if record.AgentStatus != nil {
		t.Fatalf("cached agent status was retained: %#v", record.AgentStatus)
	}
}

func TestApplyHeartbeatAgentStatusValidatesIdentityAndBoundsMessages(t *testing.T) {
	record := NodeRecord{}
	wrongNode := &statusmodel.AgentStatus{SchemaVersion: statusmodel.SchemaVersion, NodeID: "node-2"}
	if err := applyHeartbeatAgentStatus(&record, "node-1", wrongNode); err == nil {
		t.Fatal("mismatched agent status node was accepted")
	}

	incoming := &statusmodel.AgentStatus{
		SchemaVersion: statusmodel.SchemaVersion,
		NodeID:        "node-1",
		Message:       strings.Repeat("x", statusmodel.MaxMessageLen+50),
	}
	if err := applyHeartbeatAgentStatus(&record, "node-1", incoming); err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(record.AgentStatus.Message)); got != statusmodel.MaxMessageLen {
		t.Fatalf("bounded message length = %d, want %d", got, statusmodel.MaxMessageLen)
	}
}

func TestApplyHeartbeatAgentStatusRejectsUnboundedAndAmbiguousPayloads(t *testing.T) {
	record := NodeRecord{}
	tooManyServices := &statusmodel.AgentStatus{
		SchemaVersion: statusmodel.SchemaVersion, NodeID: "node-1", Phase: statusmodel.PhaseReady,
		Services: make([]statusmodel.ServiceStatus, statusmodel.MaxServices+1),
	}
	if err := applyHeartbeatAgentStatus(&record, "node-1", tooManyServices); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("unbounded service payload was accepted: %v", err)
	}

	duplicateConditions := &statusmodel.AgentStatus{
		SchemaVersion: statusmodel.SchemaVersion, NodeID: "node-1", Phase: statusmodel.PhaseReady,
		Conditions: []statusmodel.Condition{
			{Type: "ImagesReady", Status: statusmodel.ConditionTrue},
			{Type: "ImagesReady", Status: statusmodel.ConditionFalse},
		},
	}
	if err := applyHeartbeatAgentStatus(&record, "node-1", duplicateConditions); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("duplicate conditions were accepted: %v", err)
	}
}
