package controlplane

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/operatorapi"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

const (
	defaultVolumeListLimit = 100
	maxVolumeListLimit     = 500
	maxNodeRevivalHistory  = 100
	maxVolumeNameBytes     = 63
)

type desiredVolumeView struct {
	service config.ServiceConfig
	volume  config.VolumeConfig
	status  statusmodel.VolumeStatus
}

// Volumes returns retained and desired volume records from the same immutable
// snapshot used by service visibility. State filters are applied before the
// bounded page is selected.
func (s *VisibilityService) Volumes(ctx context.Context, stateFilter string, offset, limit int) (operatorapi.ListEnvelope[operatorapi.VolumeSummary], error) {
	if stateFilter != "" && !validVolumeLifecycleState(stateFilter) {
		return operatorapi.ListEnvelope[operatorapi.VolumeSummary]{}, fmt.Errorf("invalid volume state %q", stateFilter)
	}
	if offset < 0 {
		return operatorapi.ListEnvelope[operatorapi.VolumeSummary]{}, fmt.Errorf("volume offset must not be negative")
	}
	if limit == 0 {
		limit = defaultVolumeListLimit
	}
	if limit < 0 || limit > maxVolumeListLimit {
		return operatorapi.ListEnvelope[operatorapi.VolumeSummary]{}, fmt.Errorf("volume limit must be between 1 and %d", maxVolumeListLimit)
	}
	snapshot, err := s.load(ctx)
	if err != nil {
		return operatorapi.ListEnvelope[operatorapi.VolumeSummary]{}, err
	}
	all := snapshot.volumeSummaries()
	filtered := make([]operatorapi.VolumeSummary, 0, len(all))
	for _, volume := range all {
		if stateFilter == "" || volume.State == stateFilter {
			filtered = append(filtered, volume)
		}
	}
	total := len(filtered)
	start := min(offset, total)
	end := min(start+limit, total)
	items := append([]operatorapi.VolumeSummary{}, filtered[start:end]...)
	return operatorapi.ListEnvelope[operatorapi.VolumeSummary]{
		APIVersion: operatorapi.APIVersion, ObservedAt: snapshot.now, Count: len(items), Items: items,
		Problems: snapshot.problems, Limit: limit, Offset: offset, Total: total, HasMore: end < total,
	}, nil
}

// Volume returns one volume by its stable service/name identity.
func (s *VisibilityService) Volume(ctx context.Context, service, name string) (operatorapi.VolumeDetail, bool, error) {
	want := service + "/" + name
	if !projectableVolumeID(want) {
		return operatorapi.VolumeDetail{}, false, nil
	}
	snapshot, err := s.load(ctx)
	if err != nil {
		return operatorapi.VolumeDetail{}, false, err
	}
	for _, summary := range snapshot.volumeSummaries() {
		if summary.LogicalID == want {
			return operatorapi.VolumeDetail{APIVersion: operatorapi.APIVersion, ObservedAt: snapshot.now, VolumeSummary: summary}, true, nil
		}
	}
	return operatorapi.VolumeDetail{}, false, nil
}

func (s visibilitySnapshot) volumeSummaries() []operatorapi.VolumeSummary {
	desired := make(map[string]desiredVolumeView)
	for _, service := range s.desired.Services {
		statusByID := make(map[string]statusmodel.VolumeStatus)
		for _, status := range s.serviceVolumeStatuses(service) {
			statusByID[status.LogicalID] = status
		}
		for _, volume := range service.Volumes {
			id := service.Name + "/" + volume.Name
			status := statusByID[id]
			if status.LogicalID == "" {
				status = statusmodel.VolumeStatus{
					LogicalID: id, Type: string(volume.Type), MountPath: volume.MountPath,
					BoundNode: volume.BoundNode, SharedBackendID: volume.SharedBackendID,
					DesiredSizeBytes: volume.SizeBytes, ResizeGeneration: volume.ResizeGeneration, State: "pending",
				}
			}
			desired[id] = desiredVolumeView{service: service, volume: volume, status: status}
		}
	}

	byID := make(map[string]operatorapi.VolumeSummary, len(s.volumeByID)+len(desired)+len(s.invalidVolumes))
	for id, view := range desired {
		var record *VolumeRecord
		if current, ok := s.volumeByID[id]; ok {
			record = &current
		}
		summary := s.desiredVolumeSummary(view, record)
		byID[id] = summary
	}
	for id, record := range s.volumeByID {
		if _, wanted := desired[id]; wanted {
			continue
		}
		summary := operatorapi.VolumeSummary{
			LogicalID: id, Desired: false, Type: string(record.Type), BoundNode: record.BoundNode,
			SharedBackendID: record.SharedBackendID, DesiredSizeBytes: record.DesiredSizeBytes,
			AppliedSizeBytes: record.AppliedSizeBytes, RequestedSizeBytes: record.RequestedSizeBytes,
			ResizeGeneration: record.ResizeGeneration, ResizeState: string(record.ResizeState),
			Rejected: record.rejectionStands(), RejectedReason: statusmodel.BoundedMessage(record.RejectedReason),
			LastError: statusmodel.BoundedMessage(record.LastError),
		}
		summary.Service, summary.Name = splitLogicalVolumeID(id)
		summary.State, summary.ReasonCode, summary.Message = s.retainedVolumeState(summary)
		byID[id] = summary
	}
	for id, reason := range s.invalidVolumes {
		summary := operatorapi.VolumeSummary{LogicalID: id, State: "invalid", ReasonCode: reason, Message: "retained volume record needs repair"}
		if view, wanted := desired[id]; wanted {
			summary.Service = view.service.Name
			summary.Name = view.volume.Name
			summary.Type = string(view.volume.Type)
			summary.MountPath = view.volume.MountPath
			summary.BoundNode = view.status.BoundNode
			summary.SharedBackendID = view.status.SharedBackendID
			summary.Desired = true
			summary.DesiredSizeBytes = view.status.DesiredSizeBytes
		}
		byID[id] = summary
	}

	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]operatorapi.VolumeSummary, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out
}

func (s visibilitySnapshot) desiredVolumeSummary(view desiredVolumeView, record *VolumeRecord) operatorapi.VolumeSummary {
	status := view.status
	summary := operatorapi.VolumeSummary{
		LogicalID: status.LogicalID, Service: view.service.Name, Name: view.volume.Name,
		Type: string(view.volume.Type), MountPath: view.volume.MountPath,
		BoundNode: status.BoundNode, SharedBackendID: status.SharedBackendID, Desired: true,
		DesiredSizeBytes: status.DesiredSizeBytes, AppliedSizeBytes: status.AppliedSizeBytes,
		RequestedSizeBytes: status.RequestedSizeBytes, ResizeGeneration: status.ResizeGeneration,
		ObservedState: status.State, Rejected: status.Rejected, RejectedReason: status.RejectedReason,
		LastError: statusmodel.BoundedMessage(status.LastError),
	}
	if record != nil {
		summary.ResizeState = string(record.ResizeState)
		if summary.LastError == "" {
			summary.LastError = statusmodel.BoundedMessage(record.LastError)
		}
	}
	if !s.volumeBindingAvailable(summary) {
		if summary.Type == string(config.VolumeTypeLocal) {
			summary.State = "node_unavailable"
			if _, retired := s.tombstoneNodeIDs[summary.BoundNode]; retired {
				summary.ReasonCode = "node_retired"
				summary.Message = "the volume's bound node is retired"
			} else {
				summary.ReasonCode = "node_unavailable"
				summary.Message = "the volume's bound node is not currently available"
			}
		} else {
			summary.State = "backend_unavailable"
			summary.ReasonCode = "backend_unavailable"
			summary.Message = "the volume's shared backend is not currently available"
		}
		return summary
	}
	if s.volumePrepared(status, record) {
		summary.State = "active"
		return summary
	}
	summary.State = "pending"
	summary.ReasonCode = "volume_pending"
	summary.Message = "volume preparation or resize is incomplete"
	if status.Rejected {
		summary.ReasonCode = status.RejectedReason
		summary.Message = status.LastError
	}
	return summary
}

func (s visibilitySnapshot) volumeBindingAvailable(volume operatorapi.VolumeSummary) bool {
	switch volume.Type {
	case string(config.VolumeTypeLocal):
		if volume.BoundNode == "" {
			return false
		}
		record, exists := s.nodeByID[volume.BoundNode]
		if !exists {
			return false
		}
		state := nodeLifecycleState(record, s.now, s.staleTTL)
		return state == "ready" || state == "draining"
	case string(config.VolumeTypeShared):
		if volume.SharedBackendID == "" {
			return false
		}
		for _, node := range s.nodes {
			state := nodeLifecycleState(node, s.now, s.staleTTL)
			if (state == "ready" || state == "draining") && node.Storage.SharedBackendID == volume.SharedBackendID {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func (s visibilitySnapshot) volumePrepared(status statusmodel.VolumeStatus, record *VolumeRecord) bool {
	if status.State == "prepared" {
		return true
	}
	if record == nil || record.rejectionStands() || record.ResizeState != VolumeResizeApplied {
		return false
	}
	return record.DesiredSizeBytes > 0 && record.AppliedSizeBytes >= record.DesiredSizeBytes
}

func (s visibilitySnapshot) retainedVolumeState(volume operatorapi.VolumeSummary) (string, string, string) {
	if volume.Type == string(config.VolumeTypeLocal) {
		if _, retired := s.tombstoneNodeIDs[volume.BoundNode]; retired {
			return "orphaned", "node_retired", "retained volume is bound to a retired node"
		}
		return "retained", "volume_retained", "volume is retained after its service was removed"
	}
	if volume.Type == string(config.VolumeTypeShared) && volume.SharedBackendID != "" {
		knownBackend := false
		for _, node := range s.nodes {
			if node.Storage.SharedBackendID != "" {
				knownBackend = true
				if node.Storage.SharedBackendID == volume.SharedBackendID {
					return "retained", "volume_retained", "volume is retained after its service was removed"
				}
			}
		}
		if knownBackend {
			return "orphaned", "backend_removed", "retained volume refers to a backend no longer advertised by any node"
		}
	}
	return "retained", "volume_retained", "volume is retained after its service was removed"
}

func splitLogicalVolumeID(id string) (string, string) {
	parts := strings.SplitN(id, "/", 2)
	if len(parts) != 2 {
		return id, ""
	}
	return parts[0], parts[1]
}

func projectableVolumeID(id string) bool {
	if id == "" || len(id) > statusmodel.MaxLogicalIDLen {
		return false
	}
	parts := strings.Split(id, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != "" &&
		len(parts[0]) <= statusmodel.MaxServiceNameLen && len(parts[1]) <= maxVolumeNameBytes
}

func validVolumeLifecycleState(state string) bool {
	switch state {
	case "active", "pending", "node_unavailable", "backend_unavailable", "retained", "orphaned", "invalid":
		return true
	default:
		return false
	}
}

func (s *VisibilityService) retiredNodeDetail(ctx context.Context, snapshot visibilitySnapshot, nodeID string) (operatorapi.NodeDetail, bool, error) {
	key, err := nodeTombstoneKey(s.cfg.State.Prefix, nodeID)
	if err != nil {
		return operatorapi.NodeDetail{}, false, nil
	}
	var tombstone NodeTombstone
	_, exists, err := s.store.GetJSON(ctx, key, &tombstone)
	if err != nil {
		if scanInterrupted(ctx, err) {
			return operatorapi.NodeDetail{}, false, err
		}
		return operatorapi.NodeDetail{}, false, err
	}
	if exists && (tombstone.NodeID != nodeID || tombstone.MaxGeneration < 0 || !validRetirementReason(tombstone.Reason)) {
		return operatorapi.NodeDetail{}, false, fmt.Errorf("node tombstone is invalid")
	}
	history, truncated, err := s.nodeRevivalHistory(ctx, nodeID)
	if err != nil {
		return operatorapi.NodeDetail{}, false, err
	}
	if !exists && len(history) == 0 {
		return operatorapi.NodeDetail{}, false, nil
	}
	detail := operatorapi.NodeDetail{
		APIVersion: operatorapi.APIVersion, ObservedAt: snapshot.now,
		NodeSummary:    operatorapi.NodeSummary{NodeID: nodeID, State: "unknown", ReasonCode: "node_tombstone_cleared"},
		Reconciliation: "unknown", StatusMissing: true,
		Services: []operatorapi.ServiceSummary{}, RevivalHistory: history, RevivalHistoryTruncated: truncated,
	}
	if exists {
		detail.State = "retired"
		detail.MaxGeneration = tombstone.MaxGeneration
		detail.RetiredAt = tombstone.RetiredAt
		detail.RetirementReason = tombstone.Reason
	}
	return detail, true, nil
}

func (s *VisibilityService) nodeRevivalHistory(ctx context.Context, nodeID string) ([]operatorapi.NodeRevivalAudit, bool, error) {
	keys, err := s.store.ListKeys(ctx, nodeRevivalAuditsPrefix(s.cfg.State.Prefix, nodeID))
	if err != nil {
		return nil, false, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	truncated := len(keys) > maxNodeRevivalHistory
	if truncated {
		keys = keys[:maxNodeRevivalHistory]
	}
	history := make([]operatorapi.NodeRevivalAudit, 0, len(keys))
	for _, key := range keys {
		var audit NodeRevivalAudit
		_, exists, err := s.store.GetJSON(ctx, key, &audit)
		if err != nil {
			if scanInterrupted(ctx, err) {
				return nil, false, err
			}
			continue
		}
		if !exists || audit.NodeID != nodeID || audit.ClearedGeneration < 0 {
			continue
		}
		history = append(history, operatorapi.NodeRevivalAudit{
			ClearedGeneration: audit.ClearedGeneration, ClearedAt: audit.ClearedAt,
			OperatorReason:         statusmodel.BoundedMessage(audit.OperatorReason),
			ExternalFenceReference: statusmodel.BoundedMessage(audit.ExternalFenceReference),
			RetainedPoolReference:  statusmodel.BoundedMessage(audit.RetainedPoolReference),
			Outcome:                statusmodel.BoundedMessage(audit.Outcome),
		})
	}
	return history, truncated, nil
}
