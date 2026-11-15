package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

func TestActualOnlyCompletenessDoesNotOverrideWorkloadHealth(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		for _, tc := range []struct {
			name, phase, reason, serviceReason string
			change                             func(*statusmodel.AgentStatus)
		}{
			{name: "healthy", phase: "converged"},
			{name: "recovery pending", phase: "degraded", reason: "vm_recovery_pending", change: func(s *statusmodel.AgentStatus) {
				s.Conditions = append(s.Conditions, statusmodel.Condition{Type: "VMRecoveryReady", Status: statusmodel.ConditionFalse, ReasonCode: "vm_recovery_pending"})
			}},
			{name: "blocking failure", phase: "failed", reason: "node_apply_failed", change: func(s *statusmodel.AgentStatus) {
				s.Conditions[0].Status = statusmodel.ConditionFalse
			}},
			{name: "missing desired service", phase: "unknown", reason: "node_status_unavailable", change: func(s *statusmodel.AgentStatus) {
				s.Services[0].Name = "other"
			}},
			{name: "unaccounted VM hold", phase: "progressing", serviceReason: "unaccounted_vm", change: func(s *statusmodel.AgentStatus) {
				s.Phase = statusmodel.PhaseReconciling
				s.ReadyServices = 0
				s.Services[0] = statusmodel.ServiceStatus{Name: "service", VMState: "unknown", Health: "not_configured", ReasonCode: "unaccounted_vm", Message: "process 4242 uses this node's VM state but no VM tracks it"}
			}},
			{name: "failed scan hold", phase: "progressing", serviceReason: "resource_held", change: func(s *statusmodel.AgentStatus) {
				s.Phase = statusmodel.PhaseReconciling
				s.ReadyServices = 0
				s.Services[0] = statusmodel.ServiceStatus{Name: "service", VMState: "unknown", Health: "not_configured", ReasonCode: "resource_held", Message: "VM processes cannot be scanned, so an untracked VM may hold any address"}
			}},
		} {
			name := tc.name + "/complete"
			if incomplete {
				name = tc.name + "/incomplete"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				store := newBlobStateStore(newMemBlob())
				cfg := validConfigForRole(RoleAPI)
				desired := DesiredRevision{Revision: "desired-1", Services: []config.ServiceConfig{{Name: "service"}}}
				placement := PlacementRevision{Revision: "placement-1", DesiredRevision: desired.Revision, NodeConfigs: []config.NodeConfig{{Node: "node-1", Services: desired.Services}}}
				putCurrentState(t, ctx, store, desired, placement, "rendered-1")
				status := currentAgentStatus(statusmodel.PhaseReady, "rendered-1")
				status.SchemaVersion = statusmodel.SchemaVersion
				status.NodeID = "node-1"
				status.DesiredServices, status.ReadyServices = 1, 1
				status.ActualOnlyServices = []statusmodel.ServiceStatus{}
				status.ActualOnlyServicesIncomplete = incomplete
				if tc.change != nil {
					tc.change(status)
				}
				if err := validateAgentStatus(*status); err != nil {
					t.Fatalf("invalid v2 telemetry fixture: %v", err)
				}
				putNode(t, ctx, store, cfg, NodeRecord{NodeID: "node-1", State: NodeStateReady, LastSeenAt: time.Now().UTC(), AgentStatus: status})
				visibility := NewVisibilityService(cfg, store)
				got, err := visibility.Revision(ctx)
				if err != nil || got.Phase != tc.phase || got.ReasonCode != tc.reason {
					t.Fatalf("workload health changed with incomplete=%v: %+v err=%v", incomplete, got, err)
				}
				node, found, err := visibility.Node(ctx, "node-1")
				if err != nil || !found || node.ActualOnlyServicesIncomplete != incomplete {
					t.Fatalf("node lost inventory evidence: %+v found=%v err=%v", node, found, err)
				}
				if tc.serviceReason == "" {
					return
				}
				services, err := visibility.Services(ctx, "", "", "")
				if err != nil || len(services.Items) != 1 || services.Items[0].ReasonCode != tc.serviceReason || services.Items[0].Message != status.Services[0].Message {
					t.Fatalf("service list lost hold: %+v err=%v", services, err)
				}
				service, found, err := visibility.Service(ctx, "service")
				if err != nil || !found || service.ReasonCode != tc.serviceReason || service.Message != status.Services[0].Message {
					t.Fatalf("service detail lost hold: %+v found=%v err=%v", service, found, err)
				}
				if len(node.Services) != 1 || node.Services[0].ReasonCode != tc.serviceReason || node.Services[0].Message != status.Services[0].Message {
					t.Fatalf("node detail lost service hold: %+v", node)
				}
			})
		}
	}
}

func TestIncompleteInventoryCannotClearPreviousOwnerThroughPublishingPasses(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	c := retirementTestController(t, store, false, true)
	putDesired(t, store, "desired-1", testService("db"), testService("web"))
	putEmptyStateNode(t, store, "node-a", NodeStateReady, time.Now().UTC())
	c.runReconcile(ctx)
	putDesired(t, store, "desired-2", testService("db"))
	c.runReconcile(ctx)
	acknowledgePlacement(t, store, c)
	record, _ := readNode(t, store, c.cfg, "node-a")
	record.AgentStatus.ActualOnlyServicesIncomplete = true
	putNode(t, ctx, store, c.cfg, record)
	c.runReconcile(ctx)
	owners := currentPlacement(t, store).UnconfirmedPrevious
	if len(owners) != 1 || owners[0] != (PreviousPlacementOwner{Service: "web", Node: "node-a"}) {
		t.Fatalf("incomplete inventory proved absence: %+v", owners)
	}
	before, err := NewVisibilityService(c.cfg, store).Revision(ctx)
	if err != nil || before.Phase != "degraded" || before.ReasonCode != "previous_node_unconfirmed" {
		t.Fatalf("expected an ownership wait, not unavailable workload status: %+v err=%v", before, err)
	}
	// Only a later complete observation of the same published config proves
	// web absent; db can keep running on the node throughout.
	record.AgentStatus.ActualOnlyServicesIncomplete = false
	putNode(t, ctx, store, c.cfg, record)
	c.runReconcile(ctx)
	if owners := currentPlacement(t, store).UnconfirmedPrevious; len(owners) != 0 {
		t.Fatalf("complete per-service absence did not clear owner: %+v", owners)
	}
	acknowledgePlacement(t, store, c)
	after, err := NewVisibilityService(c.cfg, store).Revision(ctx)
	if err != nil || after.Phase != "converged" || after.ReasonCode != "" || after.Services.Desired != 1 || after.Services.Ready != 1 {
		t.Fatalf("complete absence did not converge with db still running: %+v err=%v", after, err)
	}
}
