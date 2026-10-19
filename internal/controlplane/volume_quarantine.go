package controlplane

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/objectstorage"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

// Quarantine tiers, ordered by how much of a record the parse could recover.
// The tier is what the caller uses to decide how far a block has to widen; the
// helper itself never decides what to do about a bad record.
const (
	// quarantineTierExact means both sizes and a binding parsed, so the
	// reservation is exact and only the owning service is affected.
	quarantineTierExact = 1
	// quarantineTierPartial means only some of that parsed. Whatever lower
	// bound is attributable is charged, *and* the scope it is attributable to
	// is marked unknown — a lower bound without the flag is the over-commit
	// this tier exists to prevent.
	quarantineTierPartial = 2
	// quarantineTierUnattributable means no binding could be determined, so
	// there is no account to charge. The block widens to a whole storage
	// class, or to both when even the type is unreadable.
	quarantineTierUnattributable = 3
)

// volumeQuarantine describes a retained record that failed validation, in the
// terms scheduling actually consumes.
//
// Class is populated only from a parsed `type` field, never from the key:
// volumeRecordKey encodes service and volume names and nothing else, so an
// unreadable object gives no storage class and its block covers both.
type volumeQuarantine struct {
	Key             string
	LogicalID       string
	Reason          string
	Tier            int
	ReservedBytes   int64
	BoundNode       string
	SharedBackendID string
	Class           config.VolumeType
}

// volumeRecordSet is the outcome of loading every retained record: the ones
// that can be used normally, and the ones that cannot but still hold capacity.
type volumeRecordSet struct {
	Records     map[string]storedVolumeRecord
	Quarantined map[string]volumeQuarantine
}

func (s volumeRecordSet) quarantineFor(logicalID string) (volumeQuarantine, bool) {
	quarantine, ok := s.Quarantined[logicalID]
	return quarantine, ok
}

// classifyVolumeRecord is the single canonical parse of a retained volume
// record. What is canonical here is the *parse*, not the response to it: the
// scheduling loader and the status projection have different jobs and cannot
// share a policy, so this reports how much it recovered and each caller
// decides.
//
// readErr is the object-read failure, if any. Everything else is derived from
// the decoded record.
func classifyVolumeRecord(prefix, key string, record VolumeRecord, readErr error) (VolumeRecord, *volumeQuarantine) {
	logicalID := logicalIDFromRecordKey(prefix, key)
	quarantine := func(reason string) *volumeQuarantine {
		return buildVolumeQuarantine(key, logicalID, reason, record)
	}
	if readErr != nil {
		// Nothing was decoded, so nothing is attributable: no binding, and no
		// class either, because the class lives inside the object.
		return VolumeRecord{}, &volumeQuarantine{
			Key: key, LogicalID: logicalID, Tier: quarantineTierUnattributable,
			Reason: statusmodel.BoundedMessage(fmt.Sprintf("read volume record: %v", readErr)),
		}
	}
	if record.LogicalID == "" {
		return VolumeRecord{}, quarantine("record has no logical_id")
	}
	if logicalID == "" || record.LogicalID != logicalID {
		return VolumeRecord{}, quarantine("logical_id does not match its key")
	}
	if record.Type != config.VolumeTypeLocal && record.Type != config.VolumeTypeShared {
		return VolumeRecord{}, quarantine(fmt.Sprintf("invalid type %q", record.Type))
	}
	if record.Type == config.VolumeTypeLocal && record.BoundNode == "" {
		return VolumeRecord{}, quarantine("missing bound_node")
	}
	if record.Type == config.VolumeTypeShared && record.SharedBackendID == "" {
		return VolumeRecord{}, quarantine("missing shared_backend_id")
	}
	if record.DesiredSizeBytes <= 0 || record.ResizeGeneration <= 0 {
		return VolumeRecord{}, quarantine("invalid size or generation")
	}
	if record.AppliedSizeBytes < 0 {
		return VolumeRecord{}, quarantine("negative applied size")
	}
	if record.ResizeState == VolumeResizeApplied && record.AppliedSizeBytes != record.DesiredSizeBytes {
		return VolumeRecord{}, quarantine("applied state with mismatched size")
	}
	if record.ResizeState == "" {
		// An *absent* state is malformed, not a state owned by a newer
		// protocol. Forward compatibility exists to protect a value a newer
		// controller deliberately wrote; the empty string is what a truncated
		// or hand-edited object produces, and treating it as untouchable would
		// freeze the record forever — every later size request ignored, with
		// nothing to name as the owner.
		return VolumeRecord{}, quarantine("missing resize_state")
	}
	// A non-empty unrecognized resize_state is deliberately *not* a
	// quarantine. A record written by a newer control plane must not brick
	// scheduling on an older one during a rollback, so unknown states are
	// carried through unchanged and never acknowledged or advanced here.
	return record, nil
}

// buildVolumeQuarantine recovers as much accounting as the decoded record
// allows. It is deliberately conservative: one parsed size is not an upper
// bound on the other (a pending shrink has applied > desired, a pending grow
// the reverse), so a record with only one readable size yields a lower bound,
// and a size with no readable binding is a number with no account.
func buildVolumeQuarantine(key, logicalID, reason string, record VolumeRecord) *volumeQuarantine {
	quarantine := &volumeQuarantine{
		Key: key, LogicalID: logicalID, Reason: statusmodel.BoundedMessage(reason),
	}
	bothSizes := record.DesiredSizeBytes > 0 && record.AppliedSizeBytes > 0
	if record.DesiredSizeBytes > 0 {
		quarantine.ReservedBytes = record.DesiredSizeBytes
	}
	if record.AppliedSizeBytes > quarantine.ReservedBytes {
		quarantine.ReservedBytes = record.AppliedSizeBytes
	}

	bound := false
	switch record.Type {
	case config.VolumeTypeLocal:
		quarantine.Class = config.VolumeTypeLocal
		if record.BoundNode != "" {
			quarantine.BoundNode = record.BoundNode
			bound = true
		}
	case config.VolumeTypeShared:
		quarantine.Class = config.VolumeTypeShared
		if record.SharedBackendID != "" {
			quarantine.SharedBackendID = record.SharedBackendID
			bound = true
		}
	}

	switch {
	case bound && bothSizes:
		quarantine.Tier = quarantineTierExact
	case bound:
		quarantine.Tier = quarantineTierPartial
	default:
		// No binding: the reservation cannot be charged to any node or
		// backend. If the class parsed, the block covers that class; if it did
		// not, it covers both.
		quarantine.Tier = quarantineTierUnattributable
		quarantine.ReservedBytes = 0
	}
	return quarantine
}

// logicalIDFromRecordKey recovers "service/volume" from a record key. The key
// is the only identity available for an object that cannot be decoded, and it
// is what lets a tier-3 quarantine still name the repair target.
func logicalIDFromRecordKey(prefix, key string) string {
	root := volumeRecordsPrefix(prefix)
	if !strings.HasPrefix(key, root) || !strings.HasSuffix(key, ".json") {
		return ""
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(key, root), ".json")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return path.Join(parts[0], parts[1])
}

// loadVolumeRecords partitions retained records instead of failing on the first
// bad one. Returning an error here stopped cluster-wide scheduling for a single
// malformed key.
//
// Partitioning alone would be unsafe: dropping a bad record silently releases
// its reservation and converts a hard failure into over-commit. So every
// quarantine is retained with whatever accounting can be proved, and the scope
// it cannot prove is flagged rather than handed out again.
func (c *Controller) loadVolumeRecords(ctx context.Context) (volumeRecordSet, error) {
	keys, err := c.store.ListKeys(ctx, volumeRecordsPrefix(c.cfg.State.Prefix))
	if err != nil {
		return volumeRecordSet{}, err
	}
	set := volumeRecordSet{
		Records:     make(map[string]storedVolumeRecord, len(keys)),
		Quarantined: make(map[string]volumeQuarantine),
	}
	for _, key := range keys {
		if !strings.HasSuffix(key, ".json") {
			continue
		}
		var record VolumeRecord
		var token objectstorage.WriteToken
		token, exists, readErr := c.store.GetJSON(ctx, key, &record)
		if readErr == nil && !exists {
			continue
		}
		valid, quarantine := classifyVolumeRecord(c.cfg.State.Prefix, key, record, readErr)
		if quarantine != nil {
			id := quarantine.LogicalID
			if id == "" {
				id = key
				quarantine.LogicalID = key
			}
			c.logger.Warn("quarantined volume record",
				"key", key, "tier", quarantine.Tier, "reason", quarantine.Reason)
			set.Quarantined[id] = *quarantine
			continue
		}
		set.Records[valid.LogicalID] = storedVolumeRecord{Record: valid, Token: token}
	}
	return set, nil
}
