package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/artemnikitin/firework/internal/objectstorage"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

const (
	nodeQuarantineReasonInvalid = "record_invalid"
	nodeQuarantineMessage       = "The stored node registry record is malformed or inconsistent."
	maxNodeQuarantineTokenBytes = 1024
	maxNodeQuarantineCASRetries = 6
)

// nodeIDFromRegistryKey accepts only a direct node-record child with an ID
// representable by the quarantine and API contracts.
func nodeIDFromRegistryKey(prefix, key string) (string, bool) {
	root := registryNodesPrefix(prefix)
	if !strings.HasPrefix(key, root) {
		return "", false
	}
	relative := strings.TrimPrefix(key, root)
	if strings.Contains(relative, "/") || !strings.HasSuffix(relative, ".json") {
		return "", false
	}
	nodeID := strings.TrimSuffix(relative, ".json")
	if strings.TrimSpace(nodeID) == "" || len(nodeID) > statusmodel.MaxVolumeIDLen || strings.Contains(nodeID, "/") {
		return "", false
	}
	expected, err := nodeRecordKey(prefix, nodeID)
	return nodeID, err == nil && expected == key
}

// decodeNodeRegistryRecord validates the durable identity and lifecycle fields
// that must be trustworthy before a record can affect scheduling or retirement.
func decodeNodeRegistryRecord(prefix, key string, raw []byte) (NodeRecord, string, bool) {
	nodeID, ok := nodeIDFromRegistryKey(prefix, key)
	if !ok {
		return NodeRecord{}, "", false
	}
	var record NodeRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return NodeRecord{}, nodeID, false
	}
	if !validNodeRegistryRecord(prefix, key, record) {
		return NodeRecord{}, nodeID, false
	}
	return record, nodeID, true
}

func validNodeRegistryRecord(prefix, key string, record NodeRecord) bool {
	nodeID, ok := nodeIDFromRegistryKey(prefix, key)
	if !ok {
		return false
	}
	if record.NodeID != nodeID || record.Generation < 0 {
		return false
	}
	switch record.State {
	case NodeStateReady, NodeStateDraining, NodeStateDown:
	default:
		return false
	}
	return true
}

func validNodeQuarantine(nodeID string, quarantine NodeQuarantine) bool {
	return quarantine.NodeID == nodeID && quarantine.ObjectToken != "" &&
		len(quarantine.ObjectToken) <= maxNodeQuarantineTokenBytes &&
		quarantine.ReasonCode == nodeQuarantineReasonInvalid
}

func (c *Controller) writeNodeQuarantine(ctx context.Context, nodeID string, sourceToken objectstorage.WriteToken) error {
	if nodeID == "" || sourceToken == "" || len(sourceToken) > maxNodeQuarantineTokenBytes {
		return errors.New("invalid node quarantine source identity")
	}
	key, err := nodeQuarantineKey(c.cfg.State.Prefix, nodeID)
	if err != nil {
		return err
	}
	sourceKey := mustNodeRecordKey(c.cfg.State.Prefix, nodeID)
	for attempt := 0; attempt < maxNodeQuarantineCASRetries; attempt++ {
		if !c.stillLeader(ctx) {
			return errors.New("lost leadership while quarantining node record")
		}
		_, currentSourceToken, sourceExists, err := c.store.GetRaw(ctx, sourceKey)
		if err != nil {
			return fmt.Errorf("verify node quarantine source: %w", err)
		}
		if !sourceExists || currentSourceToken != sourceToken {
			return nil
		}
		raw, quarantineToken, exists, err := c.store.GetRaw(ctx, key)
		if err != nil {
			return fmt.Errorf("read node quarantine: %w", err)
		}
		if exists {
			var current NodeQuarantine
			if json.Unmarshal(raw, &current) == nil && validNodeQuarantine(nodeID, current) && current.ObjectToken == string(sourceToken) {
				return nil
			}
			if quarantineToken == "" {
				return errors.New("node quarantine object has no compare token")
			}
			next := NodeQuarantine{
				NodeID: nodeID, ObjectToken: string(sourceToken), DetectedAt: time.Now().UTC(),
				ReasonCode: nodeQuarantineReasonInvalid, Message: nodeQuarantineMessage,
			}
			ok, _, err := c.store.PutJSONIfMatch(ctx, key, quarantineToken, next)
			if err != nil {
				return fmt.Errorf("update node quarantine: %w", err)
			}
			if ok {
				return nil
			}
			continue
		}
		next := NodeQuarantine{
			NodeID: nodeID, ObjectToken: string(sourceToken), DetectedAt: time.Now().UTC(),
			ReasonCode: nodeQuarantineReasonInvalid, Message: nodeQuarantineMessage,
		}
		ok, _, err := c.store.PutJSONIfAbsent(ctx, key, next)
		if err != nil {
			return fmt.Errorf("create node quarantine: %w", err)
		}
		if ok {
			return nil
		}
	}
	return errors.New("node quarantine changed too often")
}

// clearStaleNodeQuarantine removes quarantine only after the source object has
// changed from the exact malformed version that was recorded.
func (c *Controller) clearStaleNodeQuarantine(ctx context.Context, nodeID string, sourceToken objectstorage.WriteToken, force bool) error {
	key, err := nodeQuarantineKey(c.cfg.State.Prefix, nodeID)
	if err != nil {
		return err
	}
	sourceKey := mustNodeRecordKey(c.cfg.State.Prefix, nodeID)
	for attempt := 0; attempt < maxNodeQuarantineCASRetries; attempt++ {
		if !c.stillLeader(ctx) {
			return errors.New("lost leadership while clearing node quarantine")
		}
		_, currentSourceToken, sourceExists, err := c.store.GetRaw(ctx, sourceKey)
		if err != nil {
			return fmt.Errorf("verify node quarantine source: %w", err)
		}
		if !sourceExists || currentSourceToken != sourceToken {
			return nil
		}
		raw, quarantineToken, exists, err := c.store.GetRaw(ctx, key)
		if err != nil {
			return fmt.Errorf("read node quarantine: %w", err)
		}
		if !exists {
			return nil
		}
		if !force {
			var current NodeQuarantine
			if json.Unmarshal(raw, &current) == nil && validNodeQuarantine(nodeID, current) && current.ObjectToken == string(sourceToken) {
				return nil
			}
		}
		if quarantineToken == "" {
			return errors.New("node quarantine object has no compare token")
		}
		deleted, err := c.store.DeleteIfMatch(ctx, key, quarantineToken)
		if err != nil {
			return fmt.Errorf("conditionally delete node quarantine: %w", err)
		}
		if deleted {
			return nil
		}
	}
	return errors.New("node quarantine changed too often")
}

// runNodeQuarantineMaintenance keeps quarantine metadata aligned with the
// exact registry object version. It runs even when automatic retirement is
// disabled, so malformed records remain visible to API-only operators.
func (c *Controller) runNodeQuarantineMaintenance(ctx context.Context) {
	if !c.leader || !c.stillLeader(ctx) {
		return
	}
	keys, err := c.store.ListKeys(ctx, registryNodesPrefix(c.cfg.State.Prefix))
	if err != nil {
		c.logger.Warn("node quarantine pass could not list registry records", "error", err)
		return
	}
	sort.Strings(keys)
	keys = boundedRegistryKeyWindow(keys, c.quarantineCursor, maxNodeRecordsScannedPass)
	for _, key := range keys {
		c.quarantineCursor = key
		raw, token, exists, err := c.store.GetRaw(ctx, key)
		if err != nil {
			c.logger.Warn("node quarantine pass could not read registry record", "key", key, "error", err)
			continue
		}
		if !exists {
			continue
		}
		record, nodeID, valid := decodeNodeRegistryRecord(c.cfg.State.Prefix, key, raw)
		if !valid {
			if nodeID == "" {
				continue
			}
			if err := c.writeNodeQuarantine(ctx, nodeID, token); err != nil {
				c.logger.Warn("node quarantine write will retry", "node", nodeID, "error", err)
			}
			continue
		}
		if err := c.clearStaleNodeQuarantine(ctx, record.NodeID, token, false); err != nil {
			c.logger.Warn("stale node quarantine cleanup will retry", "node", record.NodeID, "error", err)
		}
	}
}

func nodeQuarantineKeyMatches(prefix, key, nodeID string) bool {
	expected, err := nodeQuarantineKey(prefix, nodeID)
	return err == nil && expected == key
}
