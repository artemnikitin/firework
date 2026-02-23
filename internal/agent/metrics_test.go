package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/healthcheck"
	"github.com/artemnikitin/firework/internal/vm"
)

func TestRuntimeMetrics_RenderIncludesServiceAndFreshnessMetrics(t *testing.T) {
	m := newRuntimeMetrics("web")

	m.observeReconcile(1500*time.Millisecond, false)
	m.observeImageSync(2*time.Second, true)
	m.recordServiceRestart("tenant-1-kibana", "tenant-1")
	m.recordConfigFetchSuccess("web", time.Unix(1700000000, 0))
	m.recordEnrichmentTimestamp("web", time.Unix(1700000010, 0))
	m.recordConfigApply("rev-123", time.Now().Add(-10*time.Second))

	m.setServiceSnapshot(
		map[string]*vm.Instance{
			"tenant-1-kibana": {
				Name:  "tenant-1-kibana",
				State: vm.StateRunning,
				Config: config.ServiceConfig{
					Name: "tenant-1-kibana",
					Metadata: map[string]string{
						"tenant": "tenant-1",
					},
				},
			},
			"kibana": {
				Name:  "kibana",
				State: vm.StateRunning,
				Config: config.ServiceConfig{
					Name: "kibana",
				},
			},
		},
		map[string]healthcheck.Result{
			"tenant-1-kibana": {Status: healthcheck.StatusHealthy},
			"kibana":          {Status: healthcheck.StatusUnhealthy},
		},
	)

	out := m.render()
	assertContains(t, out, `firework_agent_reconcile_runs_total{node="web"} 1`)
	assertContains(t, out, `firework_agent_imagesync_errors_total{node="web"} 1`)
	assertContains(t, out, `firework_agent_service_restarts_total{node="web",service="tenant-1-kibana",tenant="tenant-1"} 1`)
	assertContains(t, out, `firework_agent_service_health{node="web",service="tenant-1-kibana",tenant="tenant-1"} 1`)
	assertContains(t, out, `firework_agent_service_health{node="web",service="kibana",tenant="shared"} 0`)
	assertContains(t, out, `firework_agent_config_last_fetch_success_timestamp_seconds{node="web",label="web"} 1700000000`)
	assertContains(t, out, `firework_agent_config_last_enrichment_timestamp_seconds{node="web",label="web"} 1700000010`)
	assertContains(t, out, `firework_agent_config_last_applied_revision_info{node="web",revision="rev-123"} 1`)
}

func TestTenantForService_DefaultsToShared(t *testing.T) {
	if got := tenantForService(config.ServiceConfig{Name: "kibana"}); got != "shared" {
		t.Fatalf("expected shared tenant, got %q", got)
	}
}

func TestRender_CapacityGauges(t *testing.T) {
	m := newRuntimeMetrics("web")

	// Capacity gauges should not appear until setCapacity is called with non-zero vcpus.
	out := m.render()
	if strings.Contains(out, "firework_node_capacity_vcpus") {
		t.Error("expected no capacity gauges before setCapacity is called")
	}

	m.setCapacity(
		capacity.NodeCapacity{VCPUs: 8, MemoryMB: 16384},
		capacity.NodeCapacity{VCPUs: 3, MemoryMB: 768},
	)

	out = m.render()
	assertContains(t, out, `firework_node_capacity_vcpus{node="web"} 8`)
	assertContains(t, out, `firework_node_capacity_memory_mb{node="web"} 16384`)
	assertContains(t, out, `firework_node_used_vcpus{node="web"} 3`)
	assertContains(t, out, `firework_node_used_memory_mb{node="web"} 768`)
}

func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected output to contain %q, got:\n%s", needle, haystack)
	}
}
