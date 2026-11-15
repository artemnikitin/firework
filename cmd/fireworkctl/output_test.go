package main

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/operatorapi"
)

const gib = int64(1) << 30

// runAgainst runs fireworkctl against a TLS test server that answers each
// path with the given response.
func runAgainst(t *testing.T, responses map[string]any, args ...string) string {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value, ok := responses[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(value)
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	global := []string{"--config", filepath.Join(dir, "missing.yaml"), "--endpoint", server.URL, "--ca-file", caFile, "--token-file", tokenFile}
	if err := run(append(global, args...), &out); err != nil {
		t.Fatalf("fireworkctl %v: %v", args, err)
	}
	return out.String()
}

func assertContains(t *testing.T, output string, want ...string) {
	t.Helper()
	for _, fragment := range want {
		if !strings.Contains(output, fragment) {
			t.Errorf("output does not contain %q:\n%s", fragment, output)
		}
	}
}

func TestNodesTableShowsDiskAndSkippedRecords(t *testing.T) {
	output := runAgainst(t, map[string]any{"/v1/nodes": operatorapi.ListEnvelope[operatorapi.NodeSummary]{Items: []operatorapi.NodeSummary{
		{NodeID: "node-1", State: "ready", Storage: operatorapi.NodeStorageSummary{
			Local:  operatorapi.StorageCapacitySummary{Configured: true, CapacityBytes: 100 * gib, AllocatedBytes: 20 * gib},
			Shared: operatorapi.StorageCapacitySummary{Configured: true, Unbounded: true, AllocatedBytes: 5 * gib},
		}},
		{NodeID: "node-2", State: "down", ReasonCode: "node_down"},
	}, Problems: []operatorapi.ProjectionProblem{{Kind: "node", ID: "node-broken", ReasonCode: "record_unreadable"}}}}, "nodes")
	assertContains(t, output, "LOCAL DISK", "SHARED DISK", "20.0 GiB/100.0 GiB", "5.0 GiB (no limit)", "node_down", "node-broken", "record_unreadable")
	// An unconfigured pool is a dash, never 0 B/0 B.
	if strings.Contains(output, "0 B/0 B") {
		t.Fatalf("unconfigured storage rendered as zero capacity:\n%s", output)
	}
}

func TestNodesJSONKeepsExactBytes(t *testing.T) {
	output := runAgainst(t, map[string]any{"/v1/nodes": operatorapi.ListEnvelope[operatorapi.NodeSummary]{Items: []operatorapi.NodeSummary{
		{NodeID: "node-1", Storage: operatorapi.NodeStorageSummary{Local: operatorapi.StorageCapacitySummary{Configured: true, CapacityBytes: 100*gib + 1}}},
	}}}, "nodes", "--output", "json")
	assertContains(t, output, `"capacity_bytes": 107374182401`, `"configured": true`)
}

func TestServiceOutputShowsDiskAndPublicURL(t *testing.T) {
	storage := operatorapi.ServiceStorageSummary{Local: operatorapi.VolumeAllocationSummary{Count: 1, AllocatedBytes: 2 * gib}}
	responses := map[string]any{
		"/v1/services": operatorapi.ListEnvelope[operatorapi.ServiceSummary]{Items: []operatorapi.ServiceSummary{{Name: "db", State: "running", Health: "unhealthy", Storage: storage, ReasonCode: "health_check_failed", PreviousNodeUnconfirmed: true, PreviousNodes: []string{"node-old"}}}},
		"/v1/services/db": operatorapi.ServiceDetail{ServiceSummary: operatorapi.ServiceSummary{Name: "db", Storage: storage},
			PublicURL: "https://db.example.com"},
	}
	assertContains(t, runAgainst(t, responses, "services"), "DISK", "local 2.0 GiB", "health_check_failed", "PREVIOUS NODE UNCONFIRMED", "node-old")
	assertContains(t, runAgainst(t, responses, "service", "db"), "PUBLIC URL", "https://db.example.com", "local 2.0 GiB")
}

func TestNodeDetailShowsLabelsTimesAndRevisions(t *testing.T) {
	output := runAgainst(t, map[string]any{"/v1/nodes/node-1": operatorapi.NodeDetail{
		NodeSummary:     operatorapi.NodeSummary{NodeID: "node-1", Labels: []string{"web", "db"}},
		DesiredRevision: "d-1", PlacementRevision: "p-1", ObservedRevision: "r-2", AppliedRevision: "r-1",
	}}, "node", "node-1")
	assertContains(t, output, "LABELS", "web, db", "REGISTERED", "UPDATED", "DESIRED REVISION", "d-1", "PLACEMENT REVISION", "p-1", "OBSERVED REVISION", "r-2", "APPLIED REVISION", "r-1", "LOCAL DISK")
}

func TestNodeDetailShowsActualOnlyVMsAndRetirementHistory(t *testing.T) {
	output := runAgainst(t, map[string]any{"/v1/nodes/node-old": operatorapi.NodeDetail{
		NodeSummary:   operatorapi.NodeSummary{NodeID: "node-old", State: "retired"},
		MaxGeneration: 42, RetirementReason: "stale_timeout",
		ActualOnlyServices: []operatorapi.ActualOnlyService{{Name: "db", VMState: "failed", ReasonCode: "vm_failed"}},
		RevivalHistory:     []operatorapi.NodeRevivalAudit{{ClearedGeneration: 42, OperatorReason: "host fenced", ExternalFenceReference: "incident-2", RetainedPoolReference: "disk-7", Outcome: "completed"}},
	}}, "node", "node-old")
	assertContains(t, output, "ACTUAL-ONLY SERVICE", "db", "failed", "MAX GENERATION", "42", "stale_timeout", "REVIVAL GENERATION", "incident-2", "disk-7", "completed")
}

func TestStatusShowsServiceCounts(t *testing.T) {
	output := runAgainst(t, map[string]any{"/v1/status": operatorapi.RevisionStatus{
		Phase: "degraded", ReasonCode: "service_unhealthy",
		Services:          operatorapi.ServiceCounts{Desired: 3, Running: 3, Ready: 2, Unhealthy: 1},
		UnhealthyServices: []string{"db"}, PreviousNodeUnconfirmedServices: []string{"api"},
	}}, "status")
	assertContains(t, output, "3 desired, 3 running, 2 ready, 0 pending, 1 unhealthy, 0 unknown", "UNHEALTHY", "db", "PREVIOUS NODE UNCONFIRMED", "api")
}

func TestVolumeCommandsShowLifecycleAndExactBytes(t *testing.T) {
	volume := operatorapi.VolumeSummary{
		LogicalID: "db/data", Service: "db", Name: "data", Type: "local", MountPath: "/data",
		BoundNode: "node-1", Desired: false, State: "orphaned", ReasonCode: "node_retired",
		DesiredSizeBytes: 3*gib + 1, AppliedSizeBytes: 3 * gib, ResizeGeneration: 2,
		ResizeState: "applied", LastError: "previous error",
	}
	list := operatorapi.ListEnvelope[operatorapi.VolumeSummary]{Count: 1, Items: []operatorapi.VolumeSummary{volume}, Limit: 100, Total: 1}
	responses := map[string]any{
		"/v1/volumes":         list,
		"/v1/volumes/db/data": operatorapi.VolumeDetail{VolumeSummary: volume},
	}
	assertContains(t, runAgainst(t, responses, "volumes", "--state", "orphaned"), "db/data", "orphaned", "node_retired", "VOLUMES")
	assertContains(t, runAgainst(t, responses, "volume", "db", "data"), "MOUNT PATH", "/data", "RESIZE STATE", "applied", "orphaned")
	assertContains(t, runAgainst(t, responses, "volume", "db", "data", "--output", "json"), `"desired_size_bytes": 3221225473`, `"state": "orphaned"`)
}

func TestFormatBytes(t *testing.T) {
	for value, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536 * 1024 * 1024: "1.5 GiB", -5: "0 B"} {
		if got := formatBytes(value); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", value, got, want)
		}
	}
}
