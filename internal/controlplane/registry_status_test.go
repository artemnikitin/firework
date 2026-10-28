package controlplane

import (
	"fmt"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/statusmodel"
)

func TestApplyHeartbeatAgentStatusClearsCachedStatusForOldAgent(t *testing.T) {
	record := NodeRecord{AgentStatus: &statusmodel.AgentStatus{SchemaVersion: statusmodel.SchemaVersion, ActualOnlyServices: []statusmodel.ServiceStatus{}}}
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
		SchemaVersion:      statusmodel.SchemaVersion,
		NodeID:             "node-1",
		Message:            strings.Repeat("x", statusmodel.MaxMessageLen+50),
		ActualOnlyServices: []statusmodel.ServiceStatus{},
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
		ActualOnlyServices: []statusmodel.ServiceStatus{},
		Services:           make([]statusmodel.ServiceStatus, statusmodel.MaxServices+1),
	}
	if err := applyHeartbeatAgentStatus(&record, "node-1", tooManyServices); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("unbounded service payload was accepted: %v", err)
	}

	duplicateConditions := &statusmodel.AgentStatus{
		SchemaVersion: statusmodel.SchemaVersion, NodeID: "node-1", Phase: statusmodel.PhaseReady,
		ActualOnlyServices: []statusmodel.ServiceStatus{},
		Conditions: []statusmodel.Condition{
			{Type: "ImagesReady", Status: statusmodel.ConditionTrue},
			{Type: "ImagesReady", Status: statusmodel.ConditionFalse},
		},
	}
	if err := applyHeartbeatAgentStatus(&record, "node-1", duplicateConditions); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("duplicate conditions were accepted: %v", err)
	}
}

func TestInvalidHeartbeatStatusCanBeDroppedWithoutRejectingLiveness(t *testing.T) {
	status, err := validatedHeartbeatAgentStatus("node-1", &statusmodel.AgentStatus{
		SchemaVersion:      statusmodel.SchemaVersion,
		NodeID:             "node-2",
		ActualOnlyServices: []statusmodel.ServiceStatus{},
	})
	if err == nil || status != nil {
		t.Fatalf("invalid status normalization = %#v, %v; want discarded status and validation error", status, err)
	}
}

func TestLegacyStatusDoesNotClaimAnEmptyActualOnlyInventory(t *testing.T) {
	status, err := validatedHeartbeatAgentStatus("node-1", &statusmodel.AgentStatus{
		SchemaVersion: statusmodel.LegacySchemaVersion, NodeID: "node-1", Phase: statusmodel.PhaseReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.ActualOnlyServices != nil || statusmodel.HasCompleteActualOnlyInventory(*status) {
		t.Fatalf("legacy status was interpreted as complete actual-only inventory: %+v", status)
	}
}

func TestV2StatusRequiresCompleteShapeAndDisjointActualOnlyNames(t *testing.T) {
	base := &statusmodel.AgentStatus{
		SchemaVersion: statusmodel.SchemaVersion, NodeID: "node-1", Phase: statusmodel.PhaseReady,
		DesiredServices: 1, ReadyServices: 1,
		Services:           []statusmodel.ServiceStatus{{Name: "web", VMState: "running", Health: "healthy"}},
		ActualOnlyServices: []statusmodel.ServiceStatus{},
	}
	if err := validateAgentStatus(*base); err != nil {
		t.Fatalf("valid complete v2 status: %v", err)
	}
	missing := *base
	missing.ActualOnlyServices = nil
	if err := validateAgentStatus(missing); err == nil {
		t.Fatal("v2 status without an explicit actual-only list was accepted")
	}
	overlap := *base
	overlap.ActualOnlyServices = []statusmodel.ServiceStatus{{Name: "web", VMState: "failed", Health: "unknown"}}
	if err := validateAgentStatus(overlap); err == nil || !strings.Contains(err.Error(), "also appears") {
		t.Fatalf("desired/actual-only overlap was accepted: %v", err)
	}
	truncated := *base
	truncated.ActualOnlyServices = make([]statusmodel.ServiceStatus, statusmodel.MaxActualOnlyServices)
	for i := range truncated.ActualOnlyServices {
		truncated.ActualOnlyServices[i] = statusmodel.ServiceStatus{Name: strings.Repeat("x", statusmodel.MaxServiceNameLen-8) + fmt.Sprintf("%08d", i), VMState: "failed", Health: "unknown"}
	}
	truncated.ActualOnlyServicesTruncated = true
	if err := validateAgentStatus(truncated); err != nil {
		t.Fatalf("well-formed truncated inventory: %v", err)
	}
	if statusmodel.HasCompleteActualOnlyInventory(truncated) {
		t.Fatal("truncated v2 inventory was treated as complete")
	}
}
