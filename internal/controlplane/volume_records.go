package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/objectstorage"
	"github.com/artemnikitin/firework/internal/scheduler"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

type storedVolumeRecord struct {
	Record VolumeRecord
	Token  objectstorage.WriteToken
}

func (c *Controller) loadVolumeRecords(ctx context.Context) (map[string]storedVolumeRecord, error) {
	keys, err := c.store.ListKeys(ctx, volumeRecordsPrefix(c.cfg.State.Prefix))
	if err != nil {
		return nil, err
	}
	records := make(map[string]storedVolumeRecord, len(keys))
	for _, key := range keys {
		if !strings.HasSuffix(key, ".json") {
			continue
		}
		var record VolumeRecord
		token, exists, err := c.store.GetJSON(ctx, key, &record)
		if err != nil {
			return nil, fmt.Errorf("read volume record %s: %w", key, err)
		}
		if !exists || record.LogicalID == "" {
			continue
		}
		parts := strings.Split(record.LogicalID, "/")
		expectedKey := ""
		if len(parts) == 2 {
			expectedKey, _ = volumeRecordKey(c.cfg.State.Prefix, parts[0], parts[1])
		}
		if expectedKey == "" || key != expectedKey {
			return nil, fmt.Errorf("volume record %s logical_id does not match its key", key)
		}
		if record.DesiredSizeBytes <= 0 || record.ResizeGeneration <= 0 {
			return nil, fmt.Errorf("volume record %s has invalid size or generation", key)
		}
		if record.AppliedSizeBytes < 0 {
			return nil, fmt.Errorf("volume record %s has negative applied size", key)
		}
		if record.ResizeState != VolumeResizePending && record.ResizeState != VolumeResizeApplied && record.ResizeState != VolumeResizeFailed {
			return nil, fmt.Errorf("volume record %s has invalid resize state %q", key, record.ResizeState)
		}
		if record.ResizeState == VolumeResizeApplied && record.AppliedSizeBytes != record.DesiredSizeBytes {
			return nil, fmt.Errorf("volume record %s has applied state with mismatched size", key)
		}
		if record.Type == config.VolumeTypeLocal && record.BoundNode == "" {
			return nil, fmt.Errorf("volume record %s is missing bound_node", key)
		} else if record.Type == config.VolumeTypeShared && record.SharedBackendID == "" {
			return nil, fmt.Errorf("volume record %s is missing shared_backend_id", key)
		} else if record.Type != config.VolumeTypeLocal && record.Type != config.VolumeTypeShared {
			return nil, fmt.Errorf("volume record %s has invalid type %q", key, record.Type)
		}
		records[record.LogicalID] = storedVolumeRecord{Record: record, Token: token}
	}
	return records, nil
}

func (c *Controller) applyExistingVolumeRecords(ctx context.Context, services []config.ServiceConfig, records map[string]storedVolumeRecord) error {
	for si := range services {
		for vi := range services[si].Volumes {
			volume := &services[si].Volumes[vi]
			logicalID := services[si].Name + "/" + volume.Name
			stored, exists := records[logicalID]
			if !exists {
				volume.ResizeGeneration = 1
				continue
			}
			if stored.Record.Type != volume.Type {
				return fmt.Errorf("volume %s: type is immutable (stored %s, desired %s)", logicalID, stored.Record.Type, volume.Type)
			}
			if stored.Record.Type == config.VolumeTypeLocal && stored.Record.BoundNode == "" {
				return fmt.Errorf("volume %s: retained local record is missing bound_node", logicalID)
			}
			if stored.Record.Type == config.VolumeTypeShared && stored.Record.SharedBackendID == "" {
				return fmt.Errorf("volume %s: retained shared record is missing shared_backend_id", logicalID)
			}
			volume.BoundNode = stored.Record.BoundNode
			volume.SharedBackendID = stored.Record.SharedBackendID
			volume.ResizeGeneration = stored.Record.ResizeGeneration
			if stored.Record.DesiredSizeBytes == volume.SizeBytes {
				continue
			}
			updated := stored.Record
			updated.DesiredSizeBytes = volume.SizeBytes
			updated.ResizeGeneration++
			updated.ResizeState = VolumeResizePending
			updated.LastError = ""
			updated.UpdatedAt = time.Now().UTC()
			ok, token, err := c.store.PutJSONIfMatch(ctx, mustVolumeRecordKey(c.cfg.State.Prefix, services[si].Name, volume.Name), stored.Token, updated)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("volume %s changed concurrently; retry reconciliation", logicalID)
			}
			volume.ResizeGeneration = updated.ResizeGeneration
			records[logicalID] = storedVolumeRecord{Record: updated, Token: token}
		}
	}
	return nil
}

func (c *Controller) createAssignedVolumeRecords(ctx context.Context, nodeConfigs []config.NodeConfig, records map[string]storedVolumeRecord) error {
	now := time.Now().UTC()
	for _, node := range nodeConfigs {
		for _, service := range node.Services {
			for _, volume := range service.Volumes {
				logicalID := service.Name + "/" + volume.Name
				if _, exists := records[logicalID]; exists {
					continue
				}
				record := VolumeRecord{
					LogicalID: logicalID, Type: volume.Type, BoundNode: volume.BoundNode,
					SharedBackendID: volume.SharedBackendID, DesiredSizeBytes: volume.SizeBytes,
					ResizeGeneration: max64(1, volume.ResizeGeneration), ResizeState: VolumeResizePending,
					CreatedAt: now, UpdatedAt: now,
				}
				key := mustVolumeRecordKey(c.cfg.State.Prefix, service.Name, volume.Name)
				ok, token, err := c.store.PutJSONIfAbsent(ctx, key, record)
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("volume %s was created concurrently; retry reconciliation", logicalID)
				}
				records[logicalID] = storedVolumeRecord{Record: record, Token: token}
			}
		}
	}
	return nil
}

func storageReservations(records map[string]storedVolumeRecord) scheduler.StorageReservations {
	reservations := scheduler.StorageReservations{
		LocalByNode: make(map[string]int64), SharedByBackend: make(map[string]int64),
		RecordedLogicalIDs: make(map[string]bool, len(records)), SharedEnabled: false,
	}
	for id, stored := range records {
		record := stored.Record
		size := record.DesiredSizeBytes
		if record.AppliedSizeBytes > size {
			size = record.AppliedSizeBytes
		}
		reservations.RecordedLogicalIDs[id] = true
		if record.Type == config.VolumeTypeLocal {
			reservations.LocalByNode[record.BoundNode] += size
		} else {
			reservations.SharedByBackend[record.SharedBackendID] += size
		}
	}
	return reservations
}

func volumeRecordsDigest(records map[string]storedVolumeRecord) string {
	ordered := make([]VolumeRecord, 0, len(records))
	for _, stored := range records {
		ordered = append(ordered, stored.Record)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].LogicalID < ordered[j].LogicalID })
	data, _ := json.Marshal(ordered)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (c *Controller) acknowledgeVolumeRecords(ctx context.Context) error {
	keys, err := c.store.ListKeys(ctx, registryNodesPrefix(c.cfg.State.Prefix))
	if err != nil {
		return err
	}
	for _, key := range keys {
		var node NodeRecord
		_, exists, err := c.store.GetJSON(ctx, key, &node)
		if err != nil || !exists || node.AgentStatus == nil {
			continue
		}
		for _, service := range node.AgentStatus.Services {
			for _, observed := range service.Volumes {
				if observed.State != "prepared" && observed.State != "error" {
					continue
				}
				parts := strings.Split(observed.LogicalID, "/")
				if len(parts) != 2 {
					continue
				}
				key := mustVolumeRecordKey(c.cfg.State.Prefix, parts[0], parts[1])
				var record VolumeRecord
				token, exists, err := c.store.GetJSON(ctx, key, &record)
				if err != nil || !exists || observed.ResizeGeneration != record.ResizeGeneration {
					continue
				}
				if observed.Type != string(record.Type) {
					continue
				}
				switch record.Type {
				case config.VolumeTypeLocal:
					if node.NodeID != record.BoundNode || observed.BoundNode != record.BoundNode {
						continue
					}
				case config.VolumeTypeShared:
					if observed.SharedBackendID != record.SharedBackendID {
						continue
					}
				default:
					continue
				}
				switch observed.State {
				case "prepared":
					if observed.AppliedSizeBytes <= 0 || observed.AppliedSizeBytes != record.DesiredSizeBytes {
						continue
					}
					if record.AppliedSizeBytes == observed.AppliedSizeBytes && record.ResizeState == VolumeResizeApplied && record.LastError == "" {
						continue
					}
					record.AppliedSizeBytes = observed.AppliedSizeBytes
					record.ResizeState = VolumeResizeApplied
					record.LastError = ""
				case "error":
					if record.ResizeState == VolumeResizeFailed && record.LastError == statusmodel.BoundedMessage(observed.LastError) {
						continue
					}
					record.ResizeState = VolumeResizeFailed
					record.LastError = statusmodel.BoundedMessage(observed.LastError)
				}
				record.UpdatedAt = time.Now().UTC()
				_, _, _ = c.store.PutJSONIfMatch(ctx, key, token, record)
			}
		}
	}
	return nil
}

func mustVolumeRecordKey(prefix, service, volume string) string {
	key, err := volumeRecordKey(prefix, service, volume)
	if err != nil {
		panic(err)
	}
	return key
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
