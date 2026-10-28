package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/objectstorage"
)

const (
	maxNodeRetirementsPerPass = 32
	maxNodeRecordsScannedPass = 512
	maxIndexedTombstones      = 10_000
	nodeTombstoneIndexRepair  = time.Hour
)

// runNodeRetirementMaintenance makes a bounded pass over registry records.
// Retirement is leader-owned and remains a metadata operation: the durable
// tombstone fences later control-plane writes but is never proof that a
// disconnected VM stopped.
func (c *Controller) runNodeRetirementMaintenance(ctx context.Context) {
	if !c.leader {
		return
	}
	if !c.cfg.NodeRetirementEnabled || c.cfg.NodeRetirementDryRun {
		return
	}
	if !c.stillLeader(ctx) {
		return
	}
	if c.cfg.State.Backend == "s3" && c.cfg.State.S3.EndpointURL != "" {
		if !c.conditionalDeleteProbeDone {
			c.conditionalDeleteProbeErr = probeConditionalDelete(ctx, c.store, c.cfg.State.Prefix)
			c.conditionalDeleteProbeDone = true
		}
		if c.conditionalDeleteProbeErr != nil {
			c.logger.Error("node retirement disabled: conditional delete capability check failed", "error", c.conditionalDeleteProbeErr)
			return
		}
	}

	if err := c.ensureNodeTombstoneIndex(ctx); err != nil {
		c.logger.Error("node retirement deferred: tombstone index is unavailable", "error", err)
		return
	}
	keys, err := c.store.ListKeys(ctx, registryNodesPrefix(c.cfg.State.Prefix))
	if err != nil {
		c.logger.Warn("node retirement deferred: listing registry records failed", "error", err)
		return
	}
	sort.Strings(keys)
	keys = boundedRegistryKeyWindow(keys, c.retirementCursor, maxNodeRecordsScannedPass)
	processed := 0
	for _, key := range keys {
		if processed >= maxNodeRetirementsPerPass {
			break
		}
		c.retirementCursor = key
		var record NodeRecord
		token, exists, err := c.store.GetJSON(ctx, key, &record)
		if err != nil {
			c.logger.Warn("node retirement skipped unreadable registry record", "key", key, "error", err)
			continue
		}
		if !exists || record.NodeID == "" {
			continue
		}
		expectedKey, keyErr := nodeRecordKey(c.cfg.State.Prefix, record.NodeID)
		if keyErr != nil || expectedKey != key {
			c.logger.Warn("node retirement skipped registry key mismatch", "key", key)
			continue
		}
		reason, eligible := retirementEligibility(record, time.Now().UTC(), c.cfg)
		if !eligible && record.RetirementPendingAt.IsZero() {
			continue
		}
		if !record.RetirementPendingAt.IsZero() {
			reason = retirementReason(record)
		}
		if reason == "" {
			continue
		}
		if !c.readBackExplicitEmptyNodeConfig(ctx, record.NodeID) {
			continue
		}
		c.logger.Info("node retirement candidate", "node", record.NodeID, "generation", record.Generation, "reason", reason)
		processed++
		if !c.stillLeader(ctx) {
			return
		}
		if err := c.retireNodeRecord(ctx, record, token, reason); err != nil {
			c.logger.Warn("node retirement will retry", "node", record.NodeID, "error", err)
		}
	}
}

func (c *Controller) logRetirementDryRun(ctx context.Context) {
	if !c.leader {
		return
	}
	keys, err := c.store.ListKeys(ctx, registryNodesPrefix(c.cfg.State.Prefix))
	if err != nil {
		c.logger.Warn("node retirement dry-run could not list registry records", "error", err)
		return
	}
	sort.Strings(keys)
	keys = boundedRegistryKeyWindow(keys, c.retirementCursor, maxNodeRecordsScannedPass)
	logged := 0
	for _, key := range keys {
		if logged >= maxNodeRetirementsPerPass {
			return
		}
		c.retirementCursor = key
		var record NodeRecord
		_, exists, err := c.store.GetJSON(ctx, key, &record)
		if err != nil || !exists || record.NodeID == "" {
			continue
		}
		expectedKey, keyErr := nodeRecordKey(c.cfg.State.Prefix, record.NodeID)
		if keyErr != nil || expectedKey != key {
			continue
		}
		reason, eligible := retirementEligibility(record, time.Now().UTC(), c.cfg)
		if !eligible || !c.readBackExplicitEmptyNodeConfig(ctx, record.NodeID) {
			continue
		}
		c.logger.Info("node retirement dry-run candidate", "node", record.NodeID, "generation", record.Generation, "reason", reason)
		logged++
	}
}

func boundedRegistryKeyWindow(keys []string, after string, limit int) []string {
	if len(keys) == 0 || limit <= 0 {
		return nil
	}
	start := sort.Search(len(keys), func(i int) bool { return keys[i] > after })
	if start == len(keys) {
		start = 0
	}
	count := min(len(keys), limit)
	window := make([]string, 0, count)
	for i := 0; i < count; i++ {
		window = append(window, keys[(start+i)%len(keys)])
	}
	return window
}

func retirementEligibility(record NodeRecord, now time.Time, cfg Config) (string, bool) {
	switch record.State {
	case NodeStateDown:
		at := record.DownAt
		if at.IsZero() {
			at = record.StateChangedAt
		}
		if at.IsZero() {
			at = record.UpdatedAt
		}
		if at.IsZero() {
			at = record.LastSeenAt
		}
		return "explicit_down", !at.IsZero() && now.Sub(at) >= cfg.NodeDownRetention
	case NodeStateReady, NodeStateDraining:
		return "stale_timeout", !record.LastSeenAt.IsZero() && now.Sub(record.LastSeenAt) > cfg.NodeStaleTTL && now.Sub(record.LastSeenAt) >= cfg.NodeStaleRetention
	default:
		return "", false
	}
}

func retirementReason(record NodeRecord) string {
	if record.State == NodeStateDown {
		return "explicit_down"
	}
	if record.State == NodeStateReady || record.State == NodeStateDraining {
		return "stale_timeout"
	}
	return ""
}

// readBackExplicitEmptyNodeConfig proves the latest published node document is
// an explicit empty list with revision metadata and an identical immutable
// rendered copy. Missing, malformed, partial, or superseded documents fail
// closed and will be reconsidered on a later pass.
func (c *Controller) readBackExplicitEmptyNodeConfig(ctx context.Context, nodeID string) bool {
	var rendered RevisionPointer
	_, exists, err := c.store.GetJSON(ctx, renderedCurrentKey(c.cfg.State.Prefix), &rendered)
	if err != nil || !exists || rendered.Revision == "" {
		return false
	}
	key := legacyNodeConfigKey(c.cfg.State.Prefix, nodeID)
	data, _, exists, err := c.store.GetRaw(ctx, key)
	if err != nil || !exists {
		return false
	}
	nc, err := config.ParseNodeConfig(data)
	if err != nil || nc.Node != nodeID || len(nc.Services) != 0 || nc.Services == nil ||
		nc.DesiredRevision == "" || nc.PlacementRevision == "" || nc.RenderedRevision == "" || nc.RenderedRevision != rendered.Revision {
		return false
	}
	immutable, _, exists, err := c.store.GetRaw(ctx, renderedNodeKey(c.cfg.State.Prefix, nc.RenderedRevision, nodeID))
	if err != nil || !exists || !bytes.Equal(data, immutable) {
		return false
	}
	var after RevisionPointer
	_, exists, err = c.store.GetJSON(ctx, renderedCurrentKey(c.cfg.State.Prefix), &after)
	return err == nil && exists && after.Revision == rendered.Revision
}

func (c *Controller) retireNodeRecord(ctx context.Context, observed NodeRecord, observedToken objectstorage.WriteToken, reason string) error {
	if observed.RetirementPendingAt.IsZero() {
		if !c.stillLeader(ctx) {
			return errors.New("lost leadership before marking node retirement pending")
		}
		current := observed
		current.RetirementPendingAt = time.Now().UTC()
		current.UpdatedAt = current.RetirementPendingAt
		ok, _, err := c.store.PutJSONIfMatch(ctx, mustNodeRecordKey(c.cfg.State.Prefix, observed.NodeID), observedToken, current)
		if err != nil {
			return fmt.Errorf("mark retirement pending: %w", err)
		}
		if !ok {
			return nil
		}
		observed = current
	} else {
		reason = retirementReason(observed)
	}

	if !c.stillLeader(ctx) {
		return errors.New("lost leadership while retiring node")
	}
	if err := c.advanceNodeTombstone(ctx, observed, reason); err != nil {
		return fmt.Errorf("write node tombstone: %w", err)
	}
	if err := c.confirmNodeTombstoneIndexed(ctx, observed.NodeID); err != nil {
		return fmt.Errorf("confirm node tombstone index: %w", err)
	}
	if !c.stillLeader(ctx) {
		return errors.New("lost leadership before deleting node record")
	}
	key := mustNodeRecordKey(c.cfg.State.Prefix, observed.NodeID)
	var current NodeRecord
	token, exists, err := c.store.GetJSON(ctx, key, &current)
	if err != nil {
		return fmt.Errorf("read pending node record: %w", err)
	}
	if !exists {
		return nil
	}
	if current.NodeID != observed.NodeID || current.Generation != observed.Generation || current.RetirementPendingAt.IsZero() {
		// A strictly newer registration is allowed to win while pending. Its
		// generation exceeds the tombstone floor, so readers keep it active.
		return nil
	}
	deleted, err := c.store.DeleteIfMatch(ctx, key, token)
	if err != nil {
		return fmt.Errorf("conditionally delete retired node record: %w", err)
	}
	if !deleted {
		return nil
	}
	c.logger.Info("retired node registry record", "node", observed.NodeID, "generation", observed.Generation, "reason", reason)
	return nil
}

func mustNodeRecordKey(prefix, nodeID string) string {
	key, _ := nodeRecordKey(prefix, nodeID)
	return key
}

func mustNodeTombstoneKey(prefix, nodeID string) string {
	key, _ := nodeTombstoneKey(prefix, nodeID)
	return key
}

func (c *Controller) advanceNodeTombstone(ctx context.Context, record NodeRecord, reason string) error {
	key, err := nodeTombstoneKey(c.cfg.State.Prefix, record.NodeID)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 6; attempt++ {
		var current NodeTombstone
		token, exists, err := c.store.GetJSON(ctx, key, &current)
		if err != nil {
			return err
		}
		if exists {
			if current.NodeID != record.NodeID || current.MaxGeneration < 0 || !validRetirementReason(current.Reason) {
				return fmt.Errorf("existing tombstone for %s is invalid", record.NodeID)
			}
			if current.MaxGeneration >= record.Generation {
				break
			}
			current.MaxGeneration = record.Generation
			current.RetiredAt = time.Now().UTC()
			current.Reason = reason
			current.LastRegisteredAt = record.RegisteredAt
			ok, _, err := c.store.PutJSONIfMatch(ctx, key, token, current)
			if err != nil {
				return err
			}
			if ok {
				break
			}
			continue
		}
		current = NodeTombstone{
			NodeID: record.NodeID, MaxGeneration: record.Generation,
			RetiredAt: time.Now().UTC(), Reason: reason, LastRegisteredAt: record.RegisteredAt,
		}
		ok, _, err := c.store.PutJSONIfAbsent(ctx, key, current)
		if err != nil {
			return err
		}
		if ok {
			break
		}
	}
	// Read-back confirms the durable floor before index publication or active
	// record deletion, including after a crash or a concurrent monotonic write.
	var verified NodeTombstone
	_, exists, err := c.store.GetJSON(ctx, key, &verified)
	if err != nil || !exists || verified.NodeID != record.NodeID || verified.MaxGeneration < record.Generation || !validRetirementReason(verified.Reason) {
		if err != nil {
			return err
		}
		return fmt.Errorf("tombstone for %s did not advance to generation %d", record.NodeID, record.Generation)
	}
	return c.addNodeTombstoneIndex(ctx, record.NodeID)
}

func validRetirementReason(reason string) bool {
	return reason == "explicit_down" || reason == "stale_timeout"
}

func (c *Controller) ensureNodeTombstoneIndex(ctx context.Context) error {
	key := nodeTombstoneIndexKey(c.cfg.State.Prefix)
	var index NodeTombstoneIndex
	raw, token, exists, err := c.store.GetRaw(ctx, key)
	if err != nil {
		return fmt.Errorf("read tombstone index: %w", err)
	}
	indexValid := false
	if exists && json.Unmarshal(raw, &index) == nil && validNodeTombstoneIndex(index) {
		indexValid = true
	}
	if indexValid && time.Since(c.lastTombstoneIndexRepairAt) < nodeTombstoneIndexRepair {
		return nil
	}
	keys, err := c.store.ListKeys(ctx, nodeTombstonesPrefix(c.cfg.State.Prefix))
	if err != nil {
		return fmt.Errorf("list tombstones for index repair: %w", err)
	}
	ids := make([]string, 0, len(keys))
	for _, tombstoneKey := range keys {
		if tombstoneKey == key || !strings.HasSuffix(tombstoneKey, ".json") {
			continue
		}
		id := strings.TrimSuffix(path.Base(tombstoneKey), ".json")
		var record NodeTombstone
		_, present, err := c.store.GetJSON(ctx, tombstoneKey, &record)
		if err != nil || !present || record.NodeID != id {
			if err != nil {
				return fmt.Errorf("read tombstone %s during index repair: %w", id, err)
			}
			return fmt.Errorf("invalid tombstone %s during index repair", id)
		}
		ids = append(ids, id)
		if len(ids) > maxIndexedTombstones {
			return fmt.Errorf("tombstone count exceeds index limit %d", maxIndexedTombstones)
		}
	}
	sort.Strings(ids)
	if indexValid && equalStrings(index.NodeIDs, ids) {
		c.lastTombstoneIndexRepairAt = time.Now().UTC()
		return nil
	}
	revision := index.Revision + 1
	if revision < 1 {
		revision = 1
	}
	rebuilt := NodeTombstoneIndex{NodeIDs: ids, Count: len(ids), Revision: revision, UpdatedAt: time.Now().UTC()}
	var written bool
	if exists {
		ok, _, err := c.store.PutJSONIfMatch(ctx, key, token, rebuilt)
		if err != nil {
			return fmt.Errorf("repair tombstone index: %w", err)
		}
		if !ok {
			return errors.New("tombstone index changed during repair")
		}
		written = true
	} else {
		ok, _, err := c.store.PutJSONIfAbsent(ctx, key, rebuilt)
		if err != nil {
			return fmt.Errorf("create tombstone index: %w", err)
		}
		if !ok {
			return errors.New("tombstone index appeared during repair")
		}
		written = true
	}
	if written {
		c.lastTombstoneIndexRepairAt = time.Now().UTC()
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validNodeTombstoneIndex(index NodeTombstoneIndex) bool {
	if index.Count != len(index.NodeIDs) || index.Count < 0 || index.Count > maxIndexedTombstones || index.Revision < 1 {
		return false
	}
	for i, id := range index.NodeIDs {
		if strings.TrimSpace(id) == "" || strings.Contains(id, "/") || (i > 0 && index.NodeIDs[i-1] >= id) {
			return false
		}
	}
	return true
}

func (c *Controller) addNodeTombstoneIndex(ctx context.Context, nodeID string) error {
	key := nodeTombstoneIndexKey(c.cfg.State.Prefix)
	for attempt := 0; attempt < 6; attempt++ {
		var index NodeTombstoneIndex
		token, exists, err := c.store.GetJSON(ctx, key, &index)
		if err != nil {
			return err
		}
		if !exists || !validNodeTombstoneIndex(index) {
			return errors.New("tombstone index is missing or invalid")
		}
		position := sort.SearchStrings(index.NodeIDs, nodeID)
		if position < len(index.NodeIDs) && index.NodeIDs[position] == nodeID {
			return nil
		}
		if len(index.NodeIDs) >= maxIndexedTombstones {
			return fmt.Errorf("tombstone index reached limit %d", maxIndexedTombstones)
		}
		index.NodeIDs = append(index.NodeIDs, "")
		copy(index.NodeIDs[position+1:], index.NodeIDs[position:])
		index.NodeIDs[position] = nodeID
		index.Count = len(index.NodeIDs)
		index.Revision++
		index.UpdatedAt = time.Now().UTC()
		ok, _, err := c.store.PutJSONIfMatch(ctx, key, token, index)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return errors.New("too many concurrent tombstone index updates")
}

func (c *Controller) confirmNodeTombstoneIndexed(ctx context.Context, nodeID string) error {
	index, exists, err := loadNodeTombstoneIndex(ctx, c.store, c.cfg.State.Prefix)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("node tombstone index is missing")
	}
	position := sort.SearchStrings(index.NodeIDs, nodeID)
	if position >= len(index.NodeIDs) || index.NodeIDs[position] != nodeID {
		return fmt.Errorf("node %s is missing from tombstone index", nodeID)
	}
	return nil
}

func loadNodeTombstoneIndex(ctx context.Context, store StateStore, prefix string) (NodeTombstoneIndex, bool, error) {
	var index NodeTombstoneIndex
	_, exists, err := store.GetJSON(ctx, nodeTombstoneIndexKey(prefix), &index)
	if err != nil {
		return NodeTombstoneIndex{}, false, err
	}
	if !exists {
		return NodeTombstoneIndex{}, false, nil
	}
	if !validNodeTombstoneIndex(index) {
		return NodeTombstoneIndex{}, true, errors.New("node tombstone index is invalid")
	}
	return index, true, nil
}
