package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/statusmodel"
	"github.com/artemnikitin/firework/internal/vm"
	"github.com/artemnikitin/firework/internal/volume"
)

// TestBuildVolumeStatusesBoundsMountPath exercises the real send-side status
// path (BuildVolumeStatuses), not a hand-built VolumeStatus. A config store
// not run through the enricher's mount_path length check (internal/enricher's
// new bound, or an older stored config predating it) could still hand the
// agent an oversized mount_path; without truncation here, the resulting
// heartbeat fails controlplane's validateVolumeStatus outright and the node
// loses its entire agent_status, not just this one field.
func TestBuildVolumeStatusesBoundsMountPath(t *testing.T) {
	longPath := "/data/" + strings.Repeat("x", statusmodel.MaxMountPathLen*2)
	service := config.ServiceConfig{
		Name: "web",
		Volumes: []config.VolumeConfig{
			{Name: "data", Type: config.VolumeTypeLocal, MountPath: longPath},
		},
	}

	statuses := BuildVolumeStatuses(service, map[string]volume.PreparedVolume{})
	if len(statuses) != 1 {
		t.Fatalf("got %d volume statuses, want 1", len(statuses))
	}
	got := statuses[0].MountPath
	if len(got) > statusmodel.MaxMountPathLen {
		t.Fatalf("mount_path is %d bytes, exceeding MaxMountPathLen (%d): sent unbounded to the registry",
			len(got), statusmodel.MaxMountPathLen)
	}
	if want := statusmodel.BoundedPath(longPath); got != want {
		t.Fatalf("mount_path = %q, want the BoundedPath truncation %q", got, want)
	}
}

// TestBuildVolumeStatusesPreservesNormalMountPath guards against
// over-truncation: an ordinary mount_path well under the bound must round
// through unchanged.
func TestBuildVolumeStatusesPreservesNormalMountPath(t *testing.T) {
	service := config.ServiceConfig{
		Name: "web",
		Volumes: []config.VolumeConfig{
			{Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data/es"},
		},
	}
	statuses := BuildVolumeStatuses(service, map[string]volume.PreparedVolume{})
	if len(statuses) != 1 || statuses[0].MountPath != "/data/es" {
		t.Fatalf("got %#v, want mount_path /data/es unchanged", statuses)
	}
}

// The regression: the unchanged-revision fast path skips reconciliation and
// asserted VMsReconciled and Reconciled true unconditionally. A VM that fails
// *after* the revision was applied therefore kept reporting converged -- the
// service summary said failed, but the fleet view only requires the service to
// be present with true conditions, so the whole fleet went green over a
// crashed workload.
func TestMarkUnchangedRevisionReadyRefusesReadyOverFailedVM(t *testing.T) {
	s := &fakeStore{data: map[string][]byte{"web": []byte("node: web\nservices: []\n")}, revision: "rev-1"}
	a := New(testAgentConfig(t), s, testLogger())

	if a.markUnchangedRevisionReadyWith([]string{"web"}) {
		t.Fatal("readiness was claimed despite a failed VM")
	}

	status := a.agentStatusSnapshot()
	for _, kind := range []string{"VMsReconciled", "Reconciled"} {
		condition, ok := agentCondition(status, kind)
		if !ok {
			t.Fatalf("%s was not reported at all: %#v", kind, status)
		}
		if condition.Status != statusmodel.ConditionFalse {
			t.Fatalf("%s should be false over a failed VM, got %q", kind, condition.Status)
		}
		if condition.ReasonCode != "vm_reconcile_failed" {
			t.Fatalf("%s should reuse the reconciling path's reason code, got %q", kind, condition.ReasonCode)
		}
	}
	if status.Phase != statusmodel.PhaseFailed {
		t.Fatalf("phase should be failed over a failed VM, got %q", status.Phase)
	}
	// The stages that genuinely still hold must not be dragged down with it.
	for _, kind := range []string{"NetworkReady", "CapacityReady", "ImagesReady"} {
		condition, ok := agentCondition(status, kind)
		if !ok || condition.Status != statusmodel.ConditionTrue {
			t.Fatalf("%s should still be true: %#v", kind, status)
		}
	}
}

// With no failed VM the fast path still reports ready, as before.
func TestMarkUnchangedRevisionReadyClaimsReadyWhenNoVMFailed(t *testing.T) {
	s := &fakeStore{data: map[string][]byte{"web": []byte("node: web\nservices: []\n")}, revision: "rev-1"}
	a := New(testAgentConfig(t), s, testLogger())
	if !a.markUnchangedRevisionReadyWith(nil) {
		t.Fatal("readiness should be claimed when no VM has failed")
	}
	status := a.agentStatusSnapshot()
	for _, kind := range []string{"NetworkReady", "CapacityReady", "ImagesReady", "VMsReconciled", "Reconciled"} {
		condition, ok := agentCondition(status, kind)
		if !ok || condition.Status != statusmodel.ConditionTrue {
			t.Fatalf("%s should be true: %#v", kind, status)
		}
	}
}

func TestFailedVMNamesFrom(t *testing.T) {
	got := failedVMNamesFrom(map[string]*vm.Instance{
		"web":     {Name: "web", State: vm.StateFailed},
		"api":     {Name: "api", State: vm.StateRunning},
		"batch":   {Name: "batch", State: vm.StateFailed},
		"missing": nil,
	})
	if len(got) != 2 || got[0] != "batch" || got[1] != "web" {
		t.Fatalf("expected the failed VMs in sorted order, got %#v", got)
	}
}

func TestBuildActualOnlyServiceStatusesIncludesEveryTrackedNonDesiredVM(t *testing.T) {
	instances := map[string]*vm.Instance{
		"api":   {Name: "api", State: vm.StateRunning, PID: 1234},
		"batch": {Name: "batch", State: vm.StateFailed, LastError: "child exited"},
		"db":    {Name: "db", State: vm.StateRunning, PID: 5678},
		"nil":   nil,
	}
	got, truncated := buildActualOnlyServiceStatuses(instances, map[string]struct{}{"db": {}}, map[string]string{
		"api": strings.Repeat("volume failed ", 40),
	})
	if truncated || len(got) != 2 || got[0].Name != "api" || got[1].Name != "batch" {
		t.Fatalf("actual-only inventory = %#v, truncated=%v", got, truncated)
	}
	if got[0].VMState != string(vm.StateRunning) || got[0].PID != 1234 || got[0].ReasonCode != "volume_failed" || len(got[0].Message) > statusmodel.MaxMessageLen {
		t.Fatalf("running actual-only entry lost bounded volume error or identity: %+v", got[0])
	}
	if got[1].VMState != string(vm.StateFailed) || got[1].ReasonCode != "vm_failed" || got[1].Message != "child exited" {
		t.Fatalf("failed actual-only entry lost execution state: %+v", got[1])
	}
}

func TestBuildActualOnlyServiceStatusesMarksTruncationIncomplete(t *testing.T) {
	instances := make(map[string]*vm.Instance, statusmodel.MaxActualOnlyServices+1)
	for i := 0; i < statusmodel.MaxActualOnlyServices+1; i++ {
		name := fmt.Sprintf("service-%04d", i)
		instances[name] = &vm.Instance{Name: name, State: vm.StateStopped}
	}
	got, truncated := buildActualOnlyServiceStatuses(instances, nil, nil)
	if !truncated || len(got) != statusmodel.MaxActualOnlyServices {
		t.Fatalf("actual-only inventory count=%d truncated=%v", len(got), truncated)
	}
	if statusmodel.HasCompleteActualOnlyInventory(statusmodel.AgentStatus{
		SchemaVersion: statusmodel.SchemaVersion, ActualOnlyServices: got, ActualOnlyServicesTruncated: truncated,
	}) {
		t.Fatal("truncated actual-only inventory was treated as proof of absence")
	}
}

// A refused size is reported as its own state carrying the requested
// generation. Reporting it as "prepared" would be dropped by the control plane
// twice over — the generation guard rejects it, and the prepared arm requires
// the applied size to equal the record's desired size.
func TestBuildVolumeStatusesReportsRejectionsWithTheRequestedGeneration(t *testing.T) {
	// The desired config reaching status has already been normalized to the
	// effective generation, which is what the running instance carries.
	service := config.ServiceConfig{Name: "db", Volumes: []config.VolumeConfig{
		{Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data", SizeBytes: 10 * config.GiB, ResizeGeneration: 1},
		{Name: "cache", Type: config.VolumeTypeLocal, MountPath: "/cache", SizeBytes: 4 * config.GiB, ResizeGeneration: 1},
	}}
	prepared := map[string]volume.PreparedVolume{
		"db/data":  {LogicalID: "db/data", SizeBytes: 10 * config.GiB, ResizeGeneration: 1},
		"db/cache": {LogicalID: "db/cache", SizeBytes: 4 * config.GiB, ResizeGeneration: 1},
	}
	rejections := map[string]volume.Rejection{"db/data": {
		LogicalID: "db/data", ResizeGeneration: 2, AppliedGeneration: 1,
		RequestedSizeBytes: 2 * config.GiB, AppliedSizeBytes: 10 * config.GiB,
		MinimumSizeBytes: 5 * config.GiB,
	}}

	statuses := BuildVolumeStatusesWithRejections(service, prepared, rejections)
	byID := make(map[string]statusmodel.VolumeStatus, len(statuses))
	for _, status := range statuses {
		byID[status.LogicalID] = status
	}

	rejected := byID["db/data"]
	if rejected.State != "rejected" || !rejected.Rejected {
		t.Fatalf("expected a rejected state, got %#v", rejected)
	}
	if rejected.ResizeGeneration != 2 {
		t.Fatalf("expected the requested generation to be reported, got %d", rejected.ResizeGeneration)
	}
	if rejected.AppliedSizeBytes != 10*config.GiB || rejected.RequestedSizeBytes != 2*config.GiB {
		t.Fatalf("unexpected sizes on the rejected volume: %#v", rejected)
	}
	if rejected.LastError == "" {
		t.Fatal("expected the measured minimum to be reported")
	}

	// A volume with no rejection is untouched.
	if other := byID["db/cache"]; other.State != "prepared" || other.Rejected {
		t.Fatalf("an unrejected volume was relabelled: %#v", other)
	}
}

func TestServiceReadyRequiresAPassingOrAbsentHealthCheck(t *testing.T) {
	tests := []struct {
		vmState, health string
		want            bool
	}{
		{"running", "healthy", true},
		{"running", "not_configured", true},
		{"running", "unknown", false}, // configured but not yet checked
		{"running", "unhealthy", false},
		{"starting", "not_configured", false},
		{"stopped", "healthy", false},
	}
	for _, tt := range tests {
		if got := serviceReady(statusmodel.ServiceStatus{VMState: tt.vmState, Health: tt.health}); got != tt.want {
			t.Errorf("serviceReady(%s, %s) = %v, want %v", tt.vmState, tt.health, got, tt.want)
		}
	}
}
