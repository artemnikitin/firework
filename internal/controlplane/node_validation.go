package controlplane

import (
	"encoding/json"
	"strings"

	"github.com/artemnikitin/firework/internal/statusmodel"
)

// nodeIDFromRegistryKey accepts only a direct node-record child with an ID
// representable by the node API contract.
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
