package controlplane

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

const visibilityAPIVersion = "v1"

type ListEnvelope[T any] struct {
	APIVersion string    `json:"api_version"`
	ObservedAt time.Time `json:"observed_at"`
	Count      int       `json:"count"`
	Items      []T       `json:"items"`
}

type NodeSummary struct {
	NodeID           string    `json:"node_id"`
	Labels           []string  `json:"labels,omitempty"`
	State            string    `json:"state"`
	LastSeenAt       time.Time `json:"last_seen_at,omitempty"`
	StatusAgeSeconds int64     `json:"status_age_seconds,omitempty"`
	AgentVersion     string    `json:"agent_version,omitempty"`
	Capacity         Resources `json:"capacity"`
	Allocated        Resources `json:"allocated"`
	Available        Resources `json:"available"`
	DesiredServices  int       `json:"desired_services"`
	RunningServices  int       `json:"running_services"`
	ReasonCode       string    `json:"reason_code,omitempty"`
}

type NodeDetail struct {
	APIVersion string    `json:"api_version"`
	ObservedAt time.Time `json:"observed_at"`
	NodeSummary
	HostIP            string                  `json:"host_ip,omitempty"`
	RegisteredAt      time.Time               `json:"registered_at,omitempty"`
	UpdatedAt         time.Time               `json:"updated_at,omitempty"`
	DesiredRevision   string                  `json:"desired_revision,omitempty"`
	PlacementRevision string                  `json:"placement_revision,omitempty"`
	ObservedRevision  string                  `json:"observed_revision,omitempty"`
	AppliedRevision   string                  `json:"applied_revision,omitempty"`
	Reconciliation    string                  `json:"reconciliation"`
	Message           string                  `json:"message,omitempty"`
	StatusMissing     bool                    `json:"status_missing"`
	StatusStale       bool                    `json:"status_stale"`
	Conditions        []statusmodel.Condition `json:"conditions,omitempty"`
	Services          []ServiceSummary        `json:"services"`
}

type ServiceSummary struct {
	Name             string    `json:"name"`
	Node             string    `json:"node,omitempty"`
	State            string    `json:"state"`
	Health           string    `json:"health"`
	VCPUs            int       `json:"vcpus"`
	MemoryMB         int       `json:"memory_mb"`
	ObservedAt       time.Time `json:"observed_at,omitempty"`
	LastTransitionAt time.Time `json:"last_transition_at,omitempty"`
	ReasonCode       string    `json:"reason_code,omitempty"`
	Message          string    `json:"message,omitempty"`
}

type ServiceHealthDetail struct {
	Type          string    `json:"type,omitempty"`
	State         string    `json:"state"`
	LastCheckedAt time.Time `json:"last_checked_at,omitempty"`
	Failures      int       `json:"failures"`
	LastError     string    `json:"last_error,omitempty"`
}

type ServiceDetail struct {
	APIVersion string    `json:"api_version"`
	ObservedAt time.Time `json:"observed_at"`
	ServiceSummary
	ServiceObservedAt time.Time                  `json:"service_observed_at,omitempty"`
	DesiredImage      string                     `json:"desired_image,omitempty"`
	DesiredKernel     string                     `json:"desired_kernel,omitempty"`
	DesiredNode       string                     `json:"desired_node,omitempty"`
	ActualNode        string                     `json:"actual_node,omitempty"`
	PID               int                        `json:"pid,omitempty"`
	HealthCheck       ServiceHealthDetail        `json:"health_check"`
	NetworkAddress    string                     `json:"network_address,omitempty"`
	PortForwards      []config.PortForward       `json:"port_forwards,omitempty"`
	RoutingHostname   string                     `json:"routing_hostname,omitempty"`
	RestartCount      int                        `json:"restart_count"`
	DesiredRevision   string                     `json:"desired_revision,omitempty"`
	PlacementRevision string                     `json:"placement_revision,omitempty"`
	RenderedRevision  string                     `json:"rendered_revision,omitempty"`
	AppliedRevision   string                     `json:"applied_revision,omitempty"`
	Volumes           []statusmodel.VolumeStatus `json:"volumes,omitempty"`
}

type visibilitySnapshot struct {
	now                time.Time
	staleTTL           time.Duration
	nodes              []NodeRecord
	desired            DesiredRevision
	placement          PlacementRevision
	renderedRevision   string
	placementCurrent   bool
	placementByService map[string]placedService
	nodeByID           map[string]NodeRecord
	pendingByService   map[string]PendingPlacement
	volumeByID         map[string]VolumeRecord
}

type placedService struct {
	node   config.NodeConfig
	config config.ServiceConfig
}

type VisibilityService struct {
	cfg   Config
	store StateStore
}

func NewVisibilityService(cfg Config, store StateStore) *VisibilityService {
	return &VisibilityService{cfg: cfg, store: store}
}

func (s *VisibilityService) Nodes(ctx context.Context, stateFilter string) (ListEnvelope[NodeSummary], error) {
	snapshot, err := s.load(ctx)
	if err != nil {
		return ListEnvelope[NodeSummary]{}, err
	}
	items := make([]NodeSummary, 0, len(snapshot.nodes))
	for _, record := range snapshot.nodes {
		summary := snapshot.nodeSummary(record)
		if stateFilter != "" && summary.State != stateFilter {
			continue
		}
		items = append(items, summary)
	}
	return ListEnvelope[NodeSummary]{APIVersion: visibilityAPIVersion, ObservedAt: snapshot.now, Count: len(items), Items: items}, nil
}

func (s *VisibilityService) Node(ctx context.Context, nodeID string) (NodeDetail, bool, error) {
	snapshot, err := s.load(ctx)
	if err != nil {
		return NodeDetail{}, false, err
	}
	record, ok := snapshot.nodeByID[nodeID]
	if !ok {
		return NodeDetail{}, false, nil
	}
	summary := snapshot.nodeSummary(record)
	status, statusFresh := snapshot.freshStatus(record)
	detail := NodeDetail{
		APIVersion: visibilityAPIVersion, ObservedAt: snapshot.now, NodeSummary: summary,
		HostIP: record.HostIP, RegisteredAt: record.RegisteredAt, UpdatedAt: record.UpdatedAt,
		Reconciliation: "unknown", StatusMissing: record.AgentStatus == nil, StatusStale: record.AgentStatus != nil && !statusFresh,
		Services: make([]ServiceSummary, 0),
	}
	if statusFresh {
		detail.DesiredRevision = status.DesiredRevision
		detail.PlacementRevision = status.PlacementRevision
		detail.ObservedRevision = status.ObservedRevision
		detail.AppliedRevision = status.AppliedRevision
		detail.Reconciliation = string(status.Phase)
		detail.Message = status.Message
		detail.Conditions = append([]statusmodel.Condition(nil), status.Conditions...)
	}
	for _, desired := range snapshot.desired.Services {
		placed, exists := snapshot.placementByService[desired.Name]
		if !exists || placed.node.Node != nodeID {
			continue
		}
		detail.Services = append(detail.Services, snapshot.serviceSummary(desired))
	}
	sort.Slice(detail.Services, func(i, j int) bool { return detail.Services[i].Name < detail.Services[j].Name })
	return detail, true, nil
}

func (s *VisibilityService) Services(ctx context.Context, stateFilter, healthFilter, nodeFilter string) (ListEnvelope[ServiceSummary], error) {
	snapshot, err := s.load(ctx)
	if err != nil {
		return ListEnvelope[ServiceSummary]{}, err
	}
	items := make([]ServiceSummary, 0, len(snapshot.desired.Services))
	for _, desired := range snapshot.desired.Services {
		summary := snapshot.serviceSummary(desired)
		if stateFilter != "" && summary.State != stateFilter {
			continue
		}
		if healthFilter != "" && summary.Health != healthFilter {
			continue
		}
		if nodeFilter != "" && summary.Node != nodeFilter {
			continue
		}
		items = append(items, summary)
	}
	return ListEnvelope[ServiceSummary]{APIVersion: visibilityAPIVersion, ObservedAt: snapshot.now, Count: len(items), Items: items}, nil
}

func (s *VisibilityService) Service(ctx context.Context, name string) (ServiceDetail, bool, error) {
	snapshot, err := s.load(ctx)
	if err != nil {
		return ServiceDetail{}, false, err
	}
	var desired *config.ServiceConfig
	for i := range snapshot.desired.Services {
		if snapshot.desired.Services[i].Name == name {
			desired = &snapshot.desired.Services[i]
			break
		}
	}
	if desired == nil {
		return ServiceDetail{}, false, nil
	}
	summary := snapshot.serviceSummary(*desired)
	detail := ServiceDetail{
		APIVersion: visibilityAPIVersion, ObservedAt: snapshot.now, ServiceSummary: summary,
		ServiceObservedAt: summary.ObservedAt,
		DesiredImage:      safeIdentifier(desired.Image), DesiredKernel: safeIdentifier(desired.Kernel),
		DesiredNode: summary.Node, PortForwards: append([]config.PortForward(nil), desired.PortForwards...),
		HealthCheck: ServiceHealthDetail{State: summary.Health}, DesiredRevision: snapshot.desired.Revision,
		RenderedRevision: snapshot.renderedRevision, Volumes: desiredVolumeStatuses(*desired, snapshot.volumeByID),
	}
	if desired.Network != nil {
		detail.NetworkAddress = desired.Network.GuestIP
	}
	if desired.Metadata != nil {
		detail.RoutingHostname = strings.TrimSpace(desired.Metadata["host"])
		if detail.RoutingHostname == "" {
			detail.RoutingHostname = strings.TrimSpace(desired.Metadata["subdomain"])
		}
	}
	placed, placedOK := snapshot.placementByService[name]
	if placedOK {
		detail.Volumes = desiredVolumeStatuses(placed.config, snapshot.volumeByID)
		detail.PlacementRevision = snapshot.placement.Revision
		record, nodeExists := snapshot.nodeByID[placed.node.Node]
		if nodeExists {
			status, current := snapshot.currentStatus(record)
			if current {
				detail.AppliedRevision = status.AppliedRevision
				if actual, ok := findAgentService(status, name); ok {
					detail.ActualNode = record.NodeID
					detail.PID = actual.PID
					detail.RestartCount = actual.RestartCount
					detail.Volumes = append([]statusmodel.VolumeStatus(nil), actual.Volumes...)
					if actual.NetworkAddress != "" {
						detail.NetworkAddress = actual.NetworkAddress
					}
					lastHealthError := ""
					if actual.ReasonCode == "health_check_failed" {
						lastHealthError = actual.Message
					}
					detail.HealthCheck = ServiceHealthDetail{Type: actual.HealthCheckType, State: actual.Health, LastCheckedAt: actual.HealthLastCheckedAt, Failures: actual.HealthFailures, LastError: lastHealthError}
				}
			}
		}
	}
	if desired.HealthCheck != nil && detail.HealthCheck.Type == "" {
		detail.HealthCheck.Type = desired.HealthCheck.Type
	}
	return detail, true, nil
}

func desiredVolumeStatuses(service config.ServiceConfig, records map[string]VolumeRecord) []statusmodel.VolumeStatus {
	volumes := make([]statusmodel.VolumeStatus, 0, len(service.Volumes))
	for _, volume := range service.Volumes {
		status := statusmodel.VolumeStatus{
			LogicalID: service.Name + "/" + volume.Name, Type: string(volume.Type), MountPath: volume.MountPath,
			BoundNode: volume.BoundNode, SharedBackendID: volume.SharedBackendID,
			DesiredSizeBytes: volume.SizeBytes, ResizeGeneration: volume.ResizeGeneration, State: "pending",
		}
		if record, ok := records[status.LogicalID]; ok {
			status.BoundNode = record.BoundNode
			status.SharedBackendID = record.SharedBackendID
			status.DesiredSizeBytes = record.DesiredSizeBytes
			status.AppliedSizeBytes = record.AppliedSizeBytes
			status.ResizeGeneration = record.ResizeGeneration
			status.State = string(record.ResizeState)
			status.LastError = record.LastError
		}
		volumes = append(volumes, status)
	}
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].LogicalID < volumes[j].LogicalID })
	return volumes
}

func (s *VisibilityService) load(ctx context.Context) (visibilitySnapshot, error) {
	now := time.Now().UTC()
	snapshot := visibilitySnapshot{now: now, staleTTL: s.cfg.NodeStaleTTL, placementByService: make(map[string]placedService), nodeByID: make(map[string]NodeRecord), pendingByService: make(map[string]PendingPlacement), volumeByID: make(map[string]VolumeRecord)}
	keys, err := s.store.ListKeys(ctx, registryNodesPrefix(s.cfg.State.Prefix))
	if err != nil {
		return snapshot, fmt.Errorf("listing nodes: %w", err)
	}
	for _, key := range keys {
		var record NodeRecord
		_, exists, err := s.store.GetJSON(ctx, key, &record)
		if err != nil {
			return snapshot, fmt.Errorf("reading node record: %w", err)
		}
		if exists {
			snapshot.nodes = append(snapshot.nodes, record)
			snapshot.nodeByID[record.NodeID] = record
		}
	}
	sort.Slice(snapshot.nodes, func(i, j int) bool { return snapshot.nodes[i].NodeID < snapshot.nodes[j].NodeID })
	if err := loadCurrentRevision(ctx, s.store, desiredCurrentKey(s.cfg.State.Prefix), func(revision string) (string, any) {
		return desiredRevisionKey(s.cfg.State.Prefix, revision), &snapshot.desired
	}); err != nil {
		return snapshot, fmt.Errorf("reading desired state: %w", err)
	}
	if err := loadCurrentRevision(ctx, s.store, placementCurrentKey(s.cfg.State.Prefix), func(revision string) (string, any) {
		return placementRevisionKey(s.cfg.State.Prefix, revision), &snapshot.placement
	}); err != nil {
		return snapshot, fmt.Errorf("reading placement state: %w", err)
	}
	var rendered RevisionPointer
	_, exists, err := s.store.GetJSON(ctx, renderedCurrentKey(s.cfg.State.Prefix), &rendered)
	if err != nil {
		return snapshot, fmt.Errorf("reading rendered state: %w", err)
	}
	if exists {
		snapshot.renderedRevision = rendered.Revision
	}
	volumeKeys, err := s.store.ListKeys(ctx, volumeRecordsPrefix(s.cfg.State.Prefix))
	if err != nil {
		return snapshot, fmt.Errorf("listing volume records: %w", err)
	}
	for _, key := range volumeKeys {
		if !strings.HasSuffix(key, ".json") {
			continue
		}
		var record VolumeRecord
		_, exists, err := s.store.GetJSON(ctx, key, &record)
		if err != nil {
			return snapshot, fmt.Errorf("reading volume record %s: %w", key, err)
		}
		if exists && record.LogicalID != "" {
			snapshot.volumeByID[record.LogicalID] = record
		}
	}
	snapshot.placementCurrent = snapshot.desired.Revision != "" && snapshot.placement.DesiredRevision == snapshot.desired.Revision
	if snapshot.placementCurrent {
		for _, pending := range snapshot.placement.PendingServices {
			snapshot.pendingByService[pending.Service] = pending
		}
		for _, node := range snapshot.placement.NodeConfigs {
			for _, service := range node.Services {
				snapshot.placementByService[service.Name] = placedService{node: node, config: service}
			}
		}
	}
	sort.Slice(snapshot.desired.Services, func(i, j int) bool { return snapshot.desired.Services[i].Name < snapshot.desired.Services[j].Name })
	return snapshot, nil
}

func loadCurrentRevision(ctx context.Context, store StateStore, pointerKey string, target func(string) (string, any)) error {
	var pointer RevisionPointer
	_, exists, err := store.GetJSON(ctx, pointerKey, &pointer)
	if err != nil || !exists || pointer.Revision == "" {
		return err
	}
	key, out := target(pointer.Revision)
	_, exists, err = store.GetJSON(ctx, key, out)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("revision %q not found", pointer.Revision)
	}
	return nil
}

func (s visibilitySnapshot) nodeSummary(record NodeRecord) NodeSummary {
	state := string(record.State)
	if record.LastSeenAt.IsZero() || s.now.Sub(record.LastSeenAt) > s.staleTTL {
		state = "stale"
	} else if state != "ready" && state != "draining" && state != "down" {
		state = "unknown"
	}
	allocated := Resources{}
	desiredCount := 0
	for _, placed := range s.placementByService {
		if placed.node.Node == record.NodeID {
			allocated.VCPUs += placed.config.VCPUs
			allocated.MemoryMB += placed.config.MemoryMB
			desiredCount++
		}
	}
	available := Resources{VCPUs: max(record.Capacity.VCPUs-allocated.VCPUs, 0), MemoryMB: max(record.Capacity.MemoryMB-allocated.MemoryMB, 0)}
	summary := NodeSummary{NodeID: record.NodeID, Labels: append([]string(nil), record.Labels...), State: state, LastSeenAt: record.LastSeenAt, Capacity: record.Capacity, Allocated: allocated, Available: available, DesiredServices: desiredCount}
	if !record.LastSeenAt.IsZero() {
		summary.StatusAgeSeconds = max(int64(s.now.Sub(record.LastSeenAt).Seconds()), 0)
	}
	if state == "down" {
		summary.ReasonCode = "node_down"
	} else if status, fresh := s.freshStatus(record); fresh {
		summary.AgentVersion = status.AgentVersion
		if s.statusMatchesCurrent(status) {
			summary.ReasonCode = status.ReasonCode
			for _, service := range status.Services {
				if service.VMState == "running" {
					summary.RunningServices++
				}
			}
		} else {
			summary.ReasonCode = "agent_status_revision_mismatch"
		}
	} else if record.AgentStatus == nil {
		summary.ReasonCode = "agent_status_missing"
	} else {
		summary.ReasonCode = "agent_status_stale"
	}
	return summary
}

func (s visibilitySnapshot) freshStatus(record NodeRecord) (statusmodel.AgentStatus, bool) {
	if record.AgentStatus == nil || record.AgentStatus.SchemaVersion != statusmodel.SchemaVersion {
		return statusmodel.AgentStatus{}, false
	}
	if record.State == NodeStateDown {
		return statusmodel.AgentStatus{}, false
	}
	if record.LastSeenAt.IsZero() || s.now.Sub(record.LastSeenAt) > s.staleTTL {
		return statusmodel.AgentStatus{}, false
	}
	if record.AgentStatus.ObservedAt.IsZero() || s.now.Sub(record.AgentStatus.ObservedAt) > s.staleTTL {
		return statusmodel.AgentStatus{}, false
	}
	return *record.AgentStatus, true
}

func (s visibilitySnapshot) currentStatus(record NodeRecord) (statusmodel.AgentStatus, bool) {
	status, fresh := s.freshStatus(record)
	if !fresh || !s.statusMatchesCurrent(status) {
		return statusmodel.AgentStatus{}, false
	}
	return status, true
}

func (s visibilitySnapshot) statusMatchesCurrent(status statusmodel.AgentStatus) bool {
	if !s.placementCurrent || s.renderedRevision == "" {
		return false
	}
	return status.DesiredRevision == s.desired.Revision &&
		status.PlacementRevision == s.placement.Revision &&
		status.ObservedRevision == s.renderedRevision &&
		status.AppliedRevision == s.renderedRevision
}

func (s visibilitySnapshot) serviceSummary(desired config.ServiceConfig) ServiceSummary {
	reason := "unplaced"
	if !s.placementCurrent {
		reason = "placement_pending"
	}
	summary := ServiceSummary{Name: desired.Name, State: "pending", Health: "unknown", VCPUs: desired.VCPUs, MemoryMB: desired.MemoryMB, ReasonCode: reason}
	if pending, ok := s.pendingByService[desired.Name]; ok && s.placementCurrent {
		summary.ReasonCode = pending.ReasonCode
		summary.Message = pending.Message
	}
	placed, ok := s.placementByService[desired.Name]
	if !ok {
		return summary
	}
	summary.Node = placed.node.Node
	summary.State = "unknown"
	summary.ReasonCode = "agent_status_missing"
	record, exists := s.nodeByID[placed.node.Node]
	if !exists {
		summary.ReasonCode = "node_missing"
		return summary
	}
	if record.State == NodeStateDown {
		summary.ReasonCode = "node_down"
		return summary
	}
	status, current := s.currentStatus(record)
	if !current {
		if record.LastSeenAt.IsZero() || s.now.Sub(record.LastSeenAt) > s.staleTTL {
			summary.ReasonCode = "node_stale"
		} else if _, fresh := s.freshStatus(record); fresh {
			summary.ReasonCode = "agent_status_revision_mismatch"
		}
		return summary
	}
	summary.ObservedAt = status.ObservedAt
	actual, exists := findAgentService(status, desired.Name)
	if !exists {
		summary.ReasonCode = "service_status_missing"
		return summary
	}
	switch actual.VMState {
	case "running", "stopped", "failed":
		summary.State = actual.VMState
	default:
		summary.State = "unknown"
	}
	switch actual.Health {
	case "healthy", "unhealthy", "unknown", "not_configured":
		summary.Health = actual.Health
	default:
		summary.Health = "unknown"
	}
	summary.LastTransitionAt = actual.LastTransitionAt
	summary.ReasonCode = actual.ReasonCode
	summary.Message = actual.Message
	return summary
}

func findAgentService(status statusmodel.AgentStatus, name string) (statusmodel.ServiceStatus, bool) {
	for _, service := range status.Services {
		if service.Name == name {
			return service, true
		}
	}
	return statusmodel.ServiceStatus{}, false
}

func safeIdentifier(value string) string {
	value = strings.SplitN(value, "?", 2)[0]
	value = strings.SplitN(value, "#", 2)[0]
	return filepath.Base(value)
}
