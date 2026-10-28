package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

func TestPlacementPointerRetainsOnlyImmediatePreviousRevision(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	key := placementCurrentKey("cp/v1")
	for _, revision := range []string{"placement-1", "placement-2", "placement-3"} {
		if err := upsertPlacementPointer(ctx, store, key, revision); err != nil {
			t.Fatal(err)
		}
	}
	var pointer RevisionPointer
	if _, exists, err := store.GetJSON(ctx, key, &pointer); err != nil || !exists {
		t.Fatalf("placement pointer exists=%v err=%v", exists, err)
	}
	if pointer.Revision != "placement-3" || pointer.PreviousRevision != "placement-2" {
		t.Fatalf("placement history pointer = %+v", pointer)
	}
}

func TestUnconfirmedPreviousOwnersCarryAcrossPlacementChanges(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	controller := retirementTestController(t, store, false, true)
	service := config.ServiceConfig{Name: "web"}
	first, err := controller.buildUnconfirmedPreviousOwners(ctx, PlacementRevision{
		NodeConfigs: []config.NodeConfig{{Node: "node-a", Services: []config.ServiceConfig{service}}},
	}, []config.ServiceConfig{service}, []config.NodeConfig{{Node: "node-b", Services: []config.ServiceConfig{service}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0] != (PreviousPlacementOwner{Service: "web", Node: "node-a"}) {
		t.Fatalf("unconfirmed owners after first move = %#v", first)
	}
	second, err := controller.buildUnconfirmedPreviousOwners(ctx, PlacementRevision{
		NodeConfigs:         []config.NodeConfig{{Node: "node-b", Services: []config.ServiceConfig{service}}},
		UnconfirmedPrevious: first,
	}, []config.ServiceConfig{service}, []config.NodeConfig{{Node: "node-c", Services: []config.ServiceConfig{service}}})
	if err != nil {
		t.Fatal(err)
	}
	wantSecond := []PreviousPlacementOwner{{Service: "web", Node: "node-a"}, {Service: "web", Node: "node-b"}}
	if len(second) != len(wantSecond) || second[0] != wantSecond[0] || second[1] != wantSecond[1] {
		t.Fatalf("unconfirmed owners after second move = %#v, want %#v", second, wantSecond)
	}
	third, err := controller.buildUnconfirmedPreviousOwners(ctx, PlacementRevision{
		NodeConfigs:         []config.NodeConfig{{Node: "node-c", Services: []config.ServiceConfig{service}}},
		UnconfirmedPrevious: second,
	}, []config.ServiceConfig{service}, []config.NodeConfig{{Node: "node-d", Services: []config.ServiceConfig{service}}})
	if err != nil {
		t.Fatal(err)
	}
	wantThird := []PreviousPlacementOwner{{Service: "web", Node: "node-a"}, {Service: "web", Node: "node-b"}, {Service: "web", Node: "node-c"}}
	if len(third) != len(wantThird) || third[0] != wantThird[0] || third[1] != wantThird[1] || third[2] != wantThird[2] {
		t.Fatalf("unconfirmed owners after third move = %#v, want %#v", third, wantThird)
	}
}

func TestUnconfirmedPreviousOwnerLimitFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	controller := retirementTestController(t, store, false, true)
	previous := PlacementRevision{}
	desired := make([]config.ServiceConfig, 0, maxPreviousPlacementOwners+1)
	for i := 0; i <= maxPreviousPlacementOwners; i++ {
		name := fmt.Sprintf("service-%04d", i)
		desired = append(desired, config.ServiceConfig{Name: name})
		previous.UnconfirmedPrevious = append(previous.UnconfirmedPrevious, PreviousPlacementOwner{Service: name, Node: fmt.Sprintf("node-%04d", i)})
	}
	if _, err := controller.buildUnconfirmedPreviousOwners(ctx, previous, desired, nil); err == nil {
		t.Fatal("placement that exceeds the previous-owner bound was allowed to publish")
	}
}

func TestConfirmedV2EmptyObservationRequiresActualOnlyInventoryAndEmptyConfig(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	cfg := validConfigForRole(RoleController)
	now := time.Now().UTC()
	putRetirementEmptyConfig(t, store, cfg.State.Prefix, "node-a", "rendered-a")
	putStatusNode(t, store, cfg, "node-a", statusmodel.AgentStatus{
		SchemaVersion: statusmodel.SchemaVersion, NodeID: "node-a", ObservedAt: now, Phase: statusmodel.PhaseReady,
		DesiredRevision: "desired-1", PlacementRevision: "placement-1", ObservedRevision: "rendered-a", AppliedRevision: "rendered-a",
		ActualOnlyServices: []statusmodel.ServiceStatus{}, Services: []statusmodel.ServiceStatus{},
	})
	if !confirmedNodeExecutionAbsent(ctx, store, cfg.State.Prefix, cfg.NodeStaleTTL, "node-a") {
		t.Fatal("complete empty v2 observation was not accepted")
	}

	for _, test := range []struct {
		name   string
		status statusmodel.AgentStatus
	}{
		{name: "failed actual-only record", status: statusmodel.AgentStatus{
			SchemaVersion: statusmodel.SchemaVersion, NodeID: "node-a", ObservedAt: now, Phase: statusmodel.PhaseReady,
			DesiredRevision: "desired-1", PlacementRevision: "placement-1", ObservedRevision: "rendered-a", AppliedRevision: "rendered-a",
			ActualOnlyServices: []statusmodel.ServiceStatus{{Name: "web", VMState: "failed", Health: "unknown"}}, Services: []statusmodel.ServiceStatus{},
		}},
		{name: "truncated actual-only inventory", status: statusmodel.AgentStatus{
			SchemaVersion: statusmodel.SchemaVersion, NodeID: "node-a", ObservedAt: now, Phase: statusmodel.PhaseReady,
			DesiredRevision: "desired-1", PlacementRevision: "placement-1", ObservedRevision: "rendered-a", AppliedRevision: "rendered-a",
			ActualOnlyServices: []statusmodel.ServiceStatus{}, ActualOnlyServicesTruncated: true, Services: []statusmodel.ServiceStatus{},
		}},
		{name: "legacy v1 omission", status: statusmodel.AgentStatus{
			SchemaVersion: statusmodel.LegacySchemaVersion, NodeID: "node-a", ObservedAt: now, Phase: statusmodel.PhaseReady,
			DesiredRevision: "desired-1", PlacementRevision: "placement-1", ObservedRevision: "rendered-a", AppliedRevision: "rendered-a",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			putStatusNode(t, store, cfg, "node-a", test.status)
			if confirmedNodeExecutionAbsent(ctx, store, cfg.State.Prefix, cfg.NodeStaleTTL, "node-a") {
				t.Fatal("incomplete or non-empty execution inventory proved absence")
			}
		})
	}
}

func putStatusNode(t *testing.T, store StateStore, cfg Config, nodeID string, status statusmodel.AgentStatus) {
	t.Helper()
	now := time.Now().UTC()
	if status.ObservedAt.IsZero() {
		status.ObservedAt = now
	}
	_, err := store.PutJSON(context.Background(), mustNodeRecordKey(cfg.State.Prefix, nodeID), NodeRecord{
		NodeID: nodeID, Generation: 1, State: NodeStateReady,
		LastSeenAt: now, AgentStatusReceivedAt: now, AgentStatus: &status,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestVisibilityReportsPreviousNodeUnconfirmedUntilV2EmptyProof(t *testing.T) {
	now := time.Now().UTC()
	desired := config.ServiceConfig{Name: "web", VCPUs: 1, MemoryMB: 128}
	s := visibilitySnapshot{
		now: now, staleTTL: time.Minute, desired: DesiredRevision{Revision: "desired-2", Services: []config.ServiceConfig{desired}},
		placement: PlacementRevision{
			Revision: "placement-2", DesiredRevision: "desired-2", NodeConfigs: []config.NodeConfig{{Node: "node-b", RenderedRevision: "rendered-2", Services: []config.ServiceConfig{desired}}},
			UnconfirmedPrevious: []PreviousPlacementOwner{{Service: "web", Node: "node-a"}},
		},
		previousPlacement: PlacementRevision{Revision: "placement-1", NodeConfigs: []config.NodeConfig{{Node: "node-a", Services: []config.ServiceConfig{desired}}}},
		placementCurrent:  true, renderedRevision: "rendered-2",
		placementByService: map[string]placedService{"web": {node: config.NodeConfig{Node: "node-b"}, config: desired}},
		pendingByService:   make(map[string]PendingPlacement),
		nodeByID: map[string]NodeRecord{
			"node-a": {NodeID: "node-a", State: NodeStateReady, LastSeenAt: now, AgentStatusReceivedAt: now, AgentStatus: &statusmodel.AgentStatus{
				SchemaVersion: statusmodel.SchemaVersion, NodeID: "node-a", ObservedAt: now, Phase: statusmodel.PhaseReady,
				DesiredRevision: "desired-1", PlacementRevision: "placement-1", ObservedRevision: "rendered-a", AppliedRevision: "rendered-a",
				Services: []statusmodel.ServiceStatus{}, ActualOnlyServices: []statusmodel.ServiceStatus{{Name: "web", VMState: "failed", Health: "unknown"}},
			}},
			"node-b": {NodeID: "node-b", State: NodeStateReady, LastSeenAt: now, AgentStatusReceivedAt: now, AgentStatus: &statusmodel.AgentStatus{
				SchemaVersion: statusmodel.SchemaVersion, NodeID: "node-b", ObservedAt: now, Phase: statusmodel.PhaseReady,
				DesiredRevision: "desired-2", PlacementRevision: "placement-2", ObservedRevision: "rendered-2", AppliedRevision: "rendered-2",
				DesiredServices: 1, ReadyServices: 1,
				Services:           []statusmodel.ServiceStatus{{Name: "web", VMState: "running", Health: "not_configured"}},
				ActualOnlyServices: []statusmodel.ServiceStatus{},
				Conditions:         trueBlockingConditions(now),
			}},
		},
		emptyNodeConfigs: map[string]config.NodeConfig{"node-a": {
			Node: "node-a", DesiredRevision: "desired-1", PlacementRevision: "placement-1", RenderedRevision: "rendered-a", Services: []config.ServiceConfig{},
		}},
	}
	summary := s.serviceSummary(desired)
	if !summary.PreviousNodeUnconfirmed || summary.ReasonCode != "previous_node_unconfirmed" || len(summary.PreviousNodes) != 1 || summary.PreviousNodes[0] != "node-a" {
		t.Fatalf("service summary failed to identify the possible old execution: %+v", summary)
	}
	fleet := s.revisionStatus()
	if fleet.Phase != "degraded" || fleet.ReasonCode != "previous_node_unconfirmed" || len(fleet.PreviousNodeUnconfirmedServices) != 1 || fleet.PreviousNodeUnconfirmedServices[0] != "web" {
		t.Fatalf("fleet summary did not report the unconfirmed prior owner: %+v", fleet)
	}

	// A fresh complete v2 empty observation for the exact retained config is
	// the only telemetry proof that clears the old owner.
	old := s.nodeByID["node-a"]
	old.AgentStatus.ActualOnlyServices = []statusmodel.ServiceStatus{}
	old.AgentStatus.DesiredServices = 0
	old.AgentStatus.ReadyServices = 0
	old.AgentStatus.DesiredRevision = "desired-1"
	old.AgentStatus.PlacementRevision = "placement-1"
	old.AgentStatus.ObservedRevision = "rendered-a"
	old.AgentStatus.AppliedRevision = "rendered-a"
	s.nodeByID["node-a"] = old
	summary = s.serviceSummary(desired)
	if summary.PreviousNodeUnconfirmed {
		t.Fatalf("confirmed empty v2 observation did not clear the previous owner: %+v", summary)
	}
}

func TestReturningOldNodeRemainsVisibleAfterAnotherPlacementRevision(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	cfg := validConfigForRole(RoleAPI)
	now := time.Now().UTC()
	service := config.ServiceConfig{Name: "web", VCPUs: 1, MemoryMB: 128}
	putRetirementEmptyConfig(t, store, cfg.State.Prefix, "node-a", "rendered-a")
	putRetirementEmptyConfig(t, store, cfg.State.Prefix, "node-b", "rendered-b")
	p2 := PlacementRevision{
		Revision: "placement-2", DesiredRevision: "desired-3",
		NodeConfigs:         []config.NodeConfig{{Node: "node-b", DesiredRevision: "desired-2", PlacementRevision: "placement-2", RenderedRevision: "rendered-b", Services: []config.ServiceConfig{service}}},
		UnconfirmedPrevious: []PreviousPlacementOwner{{Service: "web", Node: "node-a"}},
	}
	p3 := PlacementRevision{
		Revision: "placement-3", DesiredRevision: "desired-3",
		NodeConfigs:         []config.NodeConfig{{Node: "node-c", DesiredRevision: "desired-3", PlacementRevision: "placement-3", RenderedRevision: "rendered-3", Services: []config.ServiceConfig{service}}},
		UnconfirmedPrevious: []PreviousPlacementOwner{{Service: "web", Node: "node-a"}, {Service: "web", Node: "node-b"}},
	}
	if _, err := store.PutJSON(ctx, placementRevisionKey(cfg.State.Prefix, p2.Revision), p2); err != nil {
		t.Fatal(err)
	}
	desired := DesiredRevision{Revision: "desired-3", Services: []config.ServiceConfig{service}}
	putCurrentState(t, ctx, store, desired, p3, "rendered-3")
	if _, err := store.PutJSON(ctx, placementCurrentKey(cfg.State.Prefix), RevisionPointer{
		Revision: p3.Revision, PreviousRevision: p2.Revision, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	putNode(t, ctx, store, cfg, NodeRecord{NodeID: "node-a", Generation: 1, State: NodeStateReady, LastSeenAt: now, AgentStatusReceivedAt: now,
		AgentStatus: &statusmodel.AgentStatus{
			SchemaVersion: statusmodel.SchemaVersion, NodeID: "node-a", ObservedAt: now, Phase: statusmodel.PhaseReady,
			DesiredRevision: "desired-1", PlacementRevision: "placement-1", ObservedRevision: "rendered-a", AppliedRevision: "rendered-a",
			ActualOnlyServices: []statusmodel.ServiceStatus{{Name: "web", VMState: "running", PID: 901, Health: "unknown"}}, Services: []statusmodel.ServiceStatus{},
		},
	})
	putNode(t, ctx, store, cfg, NodeRecord{NodeID: "node-b", Generation: 1, State: NodeStateReady, LastSeenAt: now, AgentStatusReceivedAt: now,
		AgentStatus: &statusmodel.AgentStatus{
			SchemaVersion: statusmodel.SchemaVersion, NodeID: "node-b", ObservedAt: now, Phase: statusmodel.PhaseReady,
			DesiredRevision: "desired-1", PlacementRevision: "placement-1", ObservedRevision: "rendered-b", AppliedRevision: "rendered-b",
			ActualOnlyServices: []statusmodel.ServiceStatus{}, Services: []statusmodel.ServiceStatus{},
		},
	})
	putNode(t, ctx, store, cfg, NodeRecord{NodeID: "node-c", Generation: 1, State: NodeStateReady, LastSeenAt: now, AgentStatusReceivedAt: now,
		AgentStatus: &statusmodel.AgentStatus{
			SchemaVersion: statusmodel.SchemaVersion, NodeID: "node-c", ObservedAt: now, Phase: statusmodel.PhaseReady,
			DesiredRevision: "desired-3", PlacementRevision: "placement-3", ObservedRevision: "rendered-3", AppliedRevision: "rendered-3",
			DesiredServices: 1, ReadyServices: 1,
			Services:           []statusmodel.ServiceStatus{{Name: "web", VMState: "running", Health: "not_configured"}},
			ActualOnlyServices: []statusmodel.ServiceStatus{}, Conditions: trueBlockingConditions(now),
		},
	})

	visibility := NewVisibilityService(cfg, store)
	detail, exists, err := visibility.Service(ctx, "web")
	if err != nil || !exists {
		t.Fatalf("service detail exists=%v err=%v", exists, err)
	}
	if !detail.PreviousNodeUnconfirmed || len(detail.PreviousNodes) != 1 || detail.PreviousNodes[0] != "node-a" {
		t.Fatalf("returned old node was lost after placement pointer advanced: %+v", detail.ServiceSummary)
	}
	nodeDetail, exists, err := visibility.Node(ctx, "node-a")
	if err != nil || !exists {
		t.Fatalf("old node detail exists=%v err=%v", exists, err)
	}
	if len(nodeDetail.ActualOnlyServices) != 1 || nodeDetail.ActualOnlyServices[0].Name != "web" || nodeDetail.ActualOnlyServices[0].VMState != "running" {
		t.Fatalf("actual-only runtime was not projected on the old node: %+v", nodeDetail.ActualOnlyServices)
	}
}

func trueBlockingConditions(now time.Time) []statusmodel.Condition {
	conditions := make([]statusmodel.Condition, 0, len(statusmodel.BlockingConditionTypes()))
	for _, kind := range statusmodel.BlockingConditionTypes() {
		conditions = append(conditions, statusmodel.Condition{Type: kind, Status: statusmodel.ConditionTrue, LastTransitionAt: now})
	}
	return conditions
}
