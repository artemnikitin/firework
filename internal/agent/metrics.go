package agent

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/healthcheck"
	"github.com/artemnikitin/firework/internal/vm"
)

type serviceKey struct {
	service string
	tenant  string
}

type serviceStateKey struct {
	service string
	tenant  string
	state   string
}

// runtimeMetrics keeps in-memory counters/gauges exposed via /metrics.
// It is intentionally lightweight and does not depend on external telemetry libs.
type runtimeMetrics struct {
	mu   sync.RWMutex
	node string

	reconcileRunsTotal         uint64
	reconcileErrorsTotal       uint64
	reconcileDurationSum       float64
	reconcileDurationLast      float64
	imageSyncRunsTotal         uint64
	imageSyncErrorsTotal       uint64
	imageSyncDurationSum       float64
	imageSyncDurationLast      float64
	serviceRestarts            map[serviceKey]uint64
	serviceHealth              map[serviceKey]float64
	serviceState               map[serviceStateKey]float64
	configFetchSuccessByLabel  map[string]float64
	enrichmentTimestampByLabel map[string]float64
	lastAppliedAt              float64
	lastAppliedRevision        string

	nodeCapacityVCPUs    int
	nodeCapacityMemoryMB int
	nodeUsedVCPUs        int
	nodeUsedMemoryMB     int
}

func newRuntimeMetrics(node string) *runtimeMetrics {
	return &runtimeMetrics{
		node:                       node,
		serviceRestarts:            make(map[serviceKey]uint64),
		serviceHealth:              make(map[serviceKey]float64),
		serviceState:               make(map[serviceStateKey]float64),
		configFetchSuccessByLabel:  make(map[string]float64),
		enrichmentTimestampByLabel: make(map[string]float64),
	}
}

func (m *runtimeMetrics) observeReconcile(duration time.Duration, failed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reconcileRunsTotal++
	if failed {
		m.reconcileErrorsTotal++
	}
	sec := duration.Seconds()
	m.reconcileDurationLast = sec
	m.reconcileDurationSum += sec
}

func (m *runtimeMetrics) observeImageSync(duration time.Duration, failed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.imageSyncRunsTotal++
	if failed {
		m.imageSyncErrorsTotal++
	}
	sec := duration.Seconds()
	m.imageSyncDurationLast = sec
	m.imageSyncDurationSum += sec
}

func (m *runtimeMetrics) recordServiceRestart(service, tenant string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.serviceRestarts[serviceKey{
		service: service,
		tenant:  tenant,
	}]++
}

func (m *runtimeMetrics) recordConfigFetchSuccess(label string, t time.Time) {
	if label == "" || t.IsZero() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configFetchSuccessByLabel[label] = float64(t.UTC().Unix())
}

func (m *runtimeMetrics) recordEnrichmentTimestamp(label string, t time.Time) {
	if label == "" || t.IsZero() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enrichmentTimestampByLabel[label] = float64(t.UTC().Unix())
}

func (m *runtimeMetrics) recordConfigApply(revision string, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastAppliedAt = float64(t.UTC().Unix())
	if revision != "" {
		m.lastAppliedRevision = revision
	}
}

func (m *runtimeMetrics) setServiceSnapshot(instances map[string]*vm.Instance, health map[string]healthcheck.Result) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Replace service gauges in one shot to avoid stale series for deleted services.
	m.serviceHealth = make(map[serviceKey]float64, len(instances))
	m.serviceState = make(map[serviceStateKey]float64, len(instances))

	for _, inst := range instances {
		tenant := tenantForService(inst.Config)
		sKey := serviceKey{
			service: inst.Name,
			tenant:  tenant,
		}
		state := string(inst.State)
		m.serviceState[serviceStateKey{
			service: inst.Name,
			tenant:  tenant,
			state:   state,
		}] = 1

		// Health gauge encoding:
		//   1 => healthy, 0 => unhealthy, -1 => unknown/unset.
		healthValue := -1.0
		if result, ok := health[inst.Name]; ok {
			switch result.Status {
			case healthcheck.StatusHealthy:
				healthValue = 1
			case healthcheck.StatusUnhealthy:
				healthValue = 0
			default:
				healthValue = -1
			}
		}
		m.serviceHealth[sKey] = healthValue
	}
}

func (m *runtimeMetrics) setCapacity(cap, used capacity.NodeCapacity) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nodeCapacityVCPUs = cap.VCPUs
	m.nodeCapacityMemoryMB = cap.MemoryMB
	m.nodeUsedVCPUs = used.VCPUs
	m.nodeUsedMemoryMB = used.MemoryMB
}

func (m *runtimeMetrics) render() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	now := time.Now().UTC().Unix()
	var b strings.Builder

	writeHelpType(&b, "firework_agent_reconcile_runs_total", "Total number of reconcile runs.", "counter")
	fmt.Fprintf(&b, "firework_agent_reconcile_runs_total{node=%q} %d\n", m.node, m.reconcileRunsTotal)

	writeHelpType(&b, "firework_agent_reconcile_errors_total", "Total number of failed reconcile runs.", "counter")
	fmt.Fprintf(&b, "firework_agent_reconcile_errors_total{node=%q} %d\n", m.node, m.reconcileErrorsTotal)

	writeHelpType(&b, "firework_agent_reconcile_duration_seconds_total", "Total cumulative reconcile duration in seconds.", "counter")
	fmt.Fprintf(&b, "firework_agent_reconcile_duration_seconds_total{node=%q} %.6f\n", m.node, m.reconcileDurationSum)

	writeHelpType(&b, "firework_agent_reconcile_duration_seconds_last", "Duration of the latest reconcile run in seconds.", "gauge")
	fmt.Fprintf(&b, "firework_agent_reconcile_duration_seconds_last{node=%q} %.6f\n", m.node, m.reconcileDurationLast)

	writeHelpType(&b, "firework_agent_imagesync_runs_total", "Total number of image sync runs.", "counter")
	fmt.Fprintf(&b, "firework_agent_imagesync_runs_total{node=%q} %d\n", m.node, m.imageSyncRunsTotal)

	writeHelpType(&b, "firework_agent_imagesync_errors_total", "Total number of failed image sync runs.", "counter")
	fmt.Fprintf(&b, "firework_agent_imagesync_errors_total{node=%q} %d\n", m.node, m.imageSyncErrorsTotal)

	writeHelpType(&b, "firework_agent_imagesync_duration_seconds_total", "Total cumulative image sync duration in seconds.", "counter")
	fmt.Fprintf(&b, "firework_agent_imagesync_duration_seconds_total{node=%q} %.6f\n", m.node, m.imageSyncDurationSum)

	writeHelpType(&b, "firework_agent_imagesync_duration_seconds_last", "Duration of the latest image sync run in seconds.", "gauge")
	fmt.Fprintf(&b, "firework_agent_imagesync_duration_seconds_last{node=%q} %.6f\n", m.node, m.imageSyncDurationLast)

	writeHelpType(&b, "firework_agent_service_restarts_total", "Total service restarts triggered by health checks.", "counter")
	restartKeys := sortedServiceKeys(m.serviceRestarts)
	for _, k := range restartKeys {
		fmt.Fprintf(&b,
			"firework_agent_service_restarts_total{node=%q,service=%q,tenant=%q} %d\n",
			m.node, k.service, k.tenant, m.serviceRestarts[k],
		)
	}

	writeHelpType(&b, "firework_agent_service_health", "Service health gauge (1=healthy, 0=unhealthy, -1=unknown).", "gauge")
	healthKeys := sortedServiceKeysFloat(m.serviceHealth)
	for _, k := range healthKeys {
		fmt.Fprintf(&b,
			"firework_agent_service_health{node=%q,service=%q,tenant=%q} %.0f\n",
			m.node, k.service, k.tenant, m.serviceHealth[k],
		)
	}

	writeHelpType(&b, "firework_agent_service_state", "Service state gauge (1 for the current state label).", "gauge")
	stateKeys := sortedServiceStateKeys(m.serviceState)
	for _, k := range stateKeys {
		fmt.Fprintf(&b,
			"firework_agent_service_state{node=%q,service=%q,tenant=%q,state=%q} %.0f\n",
			m.node, k.service, k.tenant, k.state, m.serviceState[k],
		)
	}

	writeHelpType(&b, "firework_agent_config_last_fetch_success_timestamp_seconds", "Unix timestamp of last successful config fetch per node label.", "gauge")
	labels := sortedMapKeys(m.configFetchSuccessByLabel)
	for _, label := range labels {
		fmt.Fprintf(&b,
			"firework_agent_config_last_fetch_success_timestamp_seconds{node=%q,label=%q} %.0f\n",
			m.node, label, m.configFetchSuccessByLabel[label],
		)
	}

	writeHelpType(&b, "firework_agent_config_last_enrichment_timestamp_seconds", "Unix timestamp of the source config object last modification per node label.", "gauge")
	enrichmentLabels := sortedMapKeys(m.enrichmentTimestampByLabel)
	for _, label := range enrichmentLabels {
		fmt.Fprintf(&b,
			"firework_agent_config_last_enrichment_timestamp_seconds{node=%q,label=%q} %.0f\n",
			m.node, label, m.enrichmentTimestampByLabel[label],
		)
	}

	writeHelpType(&b, "firework_agent_config_last_applied_timestamp_seconds", "Unix timestamp of the last successfully applied config.", "gauge")
	fmt.Fprintf(&b, "firework_agent_config_last_applied_timestamp_seconds{node=%q} %.0f\n", m.node, m.lastAppliedAt)

	writeHelpType(&b, "firework_agent_config_last_applied_revision_age_seconds", "Age in seconds since the last successful config apply.", "gauge")
	age := 0.0
	if m.lastAppliedAt > 0 {
		age = float64(now) - m.lastAppliedAt
		if age < 0 {
			age = 0
		}
	}
	fmt.Fprintf(&b, "firework_agent_config_last_applied_revision_age_seconds{node=%q} %.0f\n", m.node, age)

	writeHelpType(&b, "firework_agent_config_last_applied_revision_info", "Info metric for the last successfully applied revision.", "gauge")
	if m.lastAppliedRevision != "" {
		fmt.Fprintf(&b,
			"firework_agent_config_last_applied_revision_info{node=%q,revision=%q} 1\n",
			m.node, m.lastAppliedRevision,
		)
	}

	if m.nodeCapacityVCPUs > 0 {
		writeHelpType(&b, "firework_node_capacity_vcpus", "Total vCPU capacity of the node.", "gauge")
		fmt.Fprintf(&b, "firework_node_capacity_vcpus{node=%q} %d\n", m.node, m.nodeCapacityVCPUs)

		writeHelpType(&b, "firework_node_capacity_memory_mb", "Total memory capacity of the node in MB.", "gauge")
		fmt.Fprintf(&b, "firework_node_capacity_memory_mb{node=%q} %d\n", m.node, m.nodeCapacityMemoryMB)

		writeHelpType(&b, "firework_node_used_vcpus", "Total vCPUs requested by desired services.", "gauge")
		fmt.Fprintf(&b, "firework_node_used_vcpus{node=%q} %d\n", m.node, m.nodeUsedVCPUs)

		writeHelpType(&b, "firework_node_used_memory_mb", "Total memory requested by desired services in MB.", "gauge")
		fmt.Fprintf(&b, "firework_node_used_memory_mb{node=%q} %d\n", m.node, m.nodeUsedMemoryMB)
	}

	return b.String()
}

func writeHelpType(b *strings.Builder, metric, help, typ string) {
	fmt.Fprintf(b, "# HELP %s %s\n", metric, help)
	fmt.Fprintf(b, "# TYPE %s %s\n", metric, typ)
}

func tenantForService(svc config.ServiceConfig) string {
	if svc.Metadata != nil {
		if tenant := strings.TrimSpace(svc.Metadata["tenant"]); tenant != "" {
			return tenant
		}
	}
	return "shared"
}

func sortedServiceKeys(m map[serviceKey]uint64) []serviceKey {
	keys := make([]serviceKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].service == keys[j].service {
			return keys[i].tenant < keys[j].tenant
		}
		return keys[i].service < keys[j].service
	})
	return keys
}

func sortedServiceKeysFloat(m map[serviceKey]float64) []serviceKey {
	keys := make([]serviceKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].service == keys[j].service {
			return keys[i].tenant < keys[j].tenant
		}
		return keys[i].service < keys[j].service
	})
	return keys
}

func sortedServiceStateKeys(m map[serviceStateKey]float64) []serviceStateKey {
	keys := make([]serviceStateKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].service == keys[j].service {
			if keys[i].tenant == keys[j].tenant {
				return keys[i].state < keys[j].state
			}
			return keys[i].tenant < keys[j].tenant
		}
		return keys[i].service < keys[j].service
	})
	return keys
}

func sortedMapKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
