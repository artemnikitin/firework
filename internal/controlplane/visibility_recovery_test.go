package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

func TestRevisionStatusDegradesForQuarantineWithoutHidingIndependentServices(t *testing.T) {
	for _, tc := range []struct {
		name    string
		desired []config.ServiceConfig
	}{
		{name: "desired quarantine", desired: []config.ServiceConfig{{Name: "quarantined"}, {Name: "healthy"}}},
		{name: "removed quarantine", desired: []config.ServiceConfig{{Name: "healthy"}}},
		{name: "empty desired with quarantine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := newBlobStateStore(newMemBlob())
			cfg := validConfigForRole(RoleAPI)
			cfg.NodeStaleTTL = time.Minute
			desired := DesiredRevision{Revision: "desired-1", Services: tc.desired}
			placement := PlacementRevision{Revision: "placement-1", DesiredRevision: desired.Revision, NodeConfigs: []config.NodeConfig{{Node: "node-1", Services: tc.desired}}}
			putCurrentState(t, ctx, store, desired, placement, "rendered-1")
			status := currentAgentStatus(statusmodel.PhaseReady, "rendered-1")
			status.Services = []statusmodel.ServiceStatus{
				{Name: "quarantined", VMState: "recovery_pending", Health: "unknown", ReasonCode: "vm_recovery_pending"},
			}
			if len(tc.desired) > 0 {
				status.Services = append(status.Services, statusmodel.ServiceStatus{Name: "healthy", VMState: "running", Health: "healthy"})
			}
			putNode(t, ctx, store, cfg, NodeRecord{NodeID: "node-1", State: NodeStateReady, LastSeenAt: time.Now().UTC(), AgentStatus: status})
			visibility := NewVisibilityService(cfg, store)
			got, err := visibility.Revision(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got.Phase != "degraded" || got.ReasonCode != "vm_recovery_pending" || len(got.DegradedNodes) != 1 || got.DegradedNodes[0] != "node-1" || len(got.ConvergedNodes) != 0 {
				t.Fatalf("quarantined runtime was classified as converged: %#v", got)
			}
			if len(tc.desired) > 0 {
				services, err := visibility.Services(ctx, "", "", "")
				if err != nil {
					t.Fatal(err)
				}
				foundHealthy := false
				for _, service := range services.Items {
					if service.Name == "healthy" {
						foundHealthy = true
						if service.State != "running" || service.Health != "healthy" {
							t.Fatalf("quarantine hid independent healthy service: %#v", service)
						}
					}
				}
				if !foundHealthy || got.Services.Ready != 1 {
					t.Fatalf("healthy service disappeared: services=%#v revision=%#v", services, got)
				}
			}

			// A later successful recovery clears degradation without requiring a
			// new desired revision or changing independent service observations.
			status.Services = nil
			for _, service := range tc.desired {
				status.Services = append(status.Services, statusmodel.ServiceStatus{Name: service.Name, VMState: "running", Health: "healthy"})
			}
			putNode(t, ctx, store, cfg, NodeRecord{NodeID: "node-1", State: NodeStateReady, LastSeenAt: time.Now().UTC(), AgentStatus: status})
			got, err = NewVisibilityService(cfg, store).Revision(ctx)
			if err != nil || got.Phase != "converged" || len(got.DegradedNodes) != 0 {
				t.Fatalf("resolved quarantine did not converge: status=%#v err=%v", got, err)
			}
		})
	}
}
