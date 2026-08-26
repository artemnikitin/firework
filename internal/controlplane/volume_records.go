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

// volumeAdmission is the outcome of reconciling one desired revision against
// the retained records: which services cannot be scheduled from their desired
// configuration, and why.
type volumeAdmission struct {
	// Held names the services whose own volume records could not be used. They
	// are never simply dropped — omission from the rendered node configs is
	// what the agent turns into a delete — so the caller either re-renders the
	// last placement or leaves the service pending.
	Held map[string]string
}

// applyExistingVolumeRecords folds retained records into the desired revision
// and decides which size requests the cluster can accept.
//
// The key property is that a rejection clamps the *rendered* configuration
// rather than merely declining to write a record. Skipping the write alone
// changes nothing the agent sees: the scheduling copy still carries the
// requested SizeBytes, the logical ID is already recorded so the placement
// check computes a zero delta, and BuildNodeConfigs renders the infeasible
// size regardless of what the record says.
//
// Rejections never mark a service pending. Pending drops the service from
// BuildNodeConfigs, the agent turns the absent service into a delete, and a
// refused resize would stop a healthy workload.
func (c *Controller) applyExistingVolumeRecords(ctx context.Context, services []config.ServiceConfig, set volumeRecordSet, nodes []scheduler.Node) (volumeAdmission, error) {
	admission := volumeAdmission{Held: make(map[string]string)}
	nodeByID := make(map[string]scheduler.Node, len(nodes))
	for _, node := range nodes {
		nodeByID[node.InstanceID] = node
	}
	// The evolving reservation total. Checking each raise against a total
	// computed once before the loop is wrong: two individually feasible raises
	// can both be admitted while their combined reservation exceeds the pool.
	localByNode := make(map[string]int64)
	for node, size := range storageReservations(set).LocalByNode {
		localByNode[node] = size
	}

	// Deterministic evaluation order — services then volumes, by name — so the
	// same desired revision admits the same subset on every controller and
	// after every leader change.
	for _, si := range orderedServiceIndexes(services) {
		service := &services[si]
		for _, vi := range orderedVolumeIndexes(service.Volumes) {
			volume := &service.Volumes[vi]
			logicalID := service.Name + "/" + volume.Name
			if quarantine, blocked := set.quarantineFor(logicalID); blocked {
				admission.Held[service.Name] = scheduler.ReasonVolumeRecordInvalid
				c.logger.Warn("holding service with an unreadable volume record",
					"service", service.Name, "volume", volume.Name,
					"tier", quarantine.Tier, "reason", quarantine.Reason)
				continue
			}
			stored, exists := set.Records[logicalID]
			if !exists {
				volume.ResizeGeneration = 1
				continue
			}
			if stored.Record.Type != volume.Type {
				// A desired-configuration conflict, not a record fault. It is
				// held rather than returned as an error, because failing the
				// whole reconcile would stop scheduling for the cluster.
				admission.Held[service.Name] = "volume_type_immutable"
				c.logger.Warn("holding service whose volume type changed",
					"service", service.Name, "volume", volume.Name,
					"stored", stored.Record.Type, "desired", volume.Type)
				continue
			}
			volume.BoundNode = stored.Record.BoundNode
			volume.SharedBackendID = stored.Record.SharedBackendID
			volume.ResizeGeneration = stored.Record.ResizeGeneration

			updated, changed, err := c.admitVolumeSize(ctx, service.Name, volume, stored, nodeByID, localByNode)
			if err != nil {
				return admission, err
			}
			if changed {
				set.Records[logicalID] = updated
			}
		}
	}
	return admission, nil
}

// admitVolumeSize decides what the agent is told to run for one volume, and
// records the decision durably. It returns the record as it now stands and
// whether it was rewritten.
func (c *Controller) admitVolumeSize(
	ctx context.Context,
	serviceName string,
	volume *config.VolumeConfig,
	stored storedVolumeRecord,
	nodeByID map[string]scheduler.Node,
	localByNode map[string]int64,
) (storedVolumeRecord, bool, error) {
	record := stored.Record
	requested := volume.SizeBytes

	// A state this controller does not recognize belongs to a newer resize
	// protocol that is presumably mid-flight. Tolerating it in the parse
	// (so it does not brick scheduling) is only half the rule: advancing the
	// generation and resetting the state to pending here would destroy the
	// very state the tolerance exists to preserve. Render what the record
	// says and write nothing.
	if !knownVolumeResizeState(record.ResizeState) {
		volume.SizeBytes = record.DesiredSizeBytes
		volume.ResizeGeneration = record.ResizeGeneration
		return stored, false, nil
	}

	// A standing rejection for exactly this request. The match key is
	// RequestedSizeBytes, not DesiredSizeBytes: after a rejection
	// DesiredSizeBytes holds the *effective* size, so comparing against it
	// makes the unchanged request look new on every tick and mints a
	// generation forever.
	if record.rejectionStands() && requested == record.RequestedSizeBytes {
		volume.SizeBytes = record.DesiredSizeBytes
		volume.ResizeGeneration = record.ResizeGeneration
		available := c.availableLocalBytes(record, nodeByID, localByNode)
		if record.RejectedAvailableBytes == available {
			// Nothing about the rejection changed, so nothing is written and
			// the records digest stays stable.
			return stored, false, nil
		}
		record.RejectedAvailableBytes = available
		return c.putVolumeRecord(ctx, serviceName, volume.Name, stored.Token, record)
	}

	if record.DesiredSizeBytes == requested {
		// The request matches the effective size. Any standing rejection was
		// for a different size and is cleared — without minting a generation,
		// because there is no resize to perform.
		if !record.clearRejection() {
			return stored, false, nil
		}
		return c.putVolumeRecord(ctx, serviceName, volume.Name, stored.Token, record)
	}

	reason, available := c.admitLocalRaise(record, requested, nodeByID, localByNode)
	if reason != "" {
		// Clamp the rendered configuration to the last accepted size so the
		// service keeps running at a size the cluster is actually able to
		// serve, and record the refusal for status.
		volume.SizeBytes = record.DesiredSizeBytes
		volume.ResizeGeneration = record.ResizeGeneration
		if record.RequestedSizeBytes == requested && record.RejectedReason == reason && record.RejectedAvailableBytes == available {
			return stored, false, nil
		}
		if !record.rejectionStands() || record.RequestedSizeBytes != requested {
			// First refusal of this request: stamp the time once. It is
			// preserved on later ticks so a standing rejection produces no
			// further writes.
			record.RejectedAt = time.Now().UTC()
		}
		record.RequestedSizeBytes = requested
		record.RejectedReason = reason
		record.RejectedAvailableBytes = available
		c.logger.Warn("refused a volume size request",
			"service", serviceName, "volume", volume.Name,
			"requested_bytes", requested, "effective_bytes", record.DesiredSizeBytes, "reason", reason)
		return c.putVolumeRecord(ctx, serviceName, volume.Name, stored.Token, record)
	}

	// Accepted. Fold the new contribution into the evolving total before the
	// next raise is evaluated.
	if record.Type == config.VolumeTypeLocal && record.BoundNode != "" {
		localByNode[record.BoundNode] += recordContribution(requested, record.AppliedSizeBytes) -
			recordContribution(record.DesiredSizeBytes, record.AppliedSizeBytes)
	}
	record.clearRejection()
	record.DesiredSizeBytes = requested
	record.ResizeGeneration++
	record.ResizeState = VolumeResizePending
	record.LastError = ""
	updated, _, err := c.putVolumeRecord(ctx, serviceName, volume.Name, stored.Token, record)
	if err != nil {
		return stored, false, err
	}
	volume.ResizeGeneration = updated.Record.ResizeGeneration
	return updated, true, nil
}

// admitLocalRaise applies the replacement arithmetic that keeps a batch of
// raises inside one pool. It returns an empty reason when the raise is
// accepted.
//
// Shared volumes are admitted here because shared execution is gated off
// upstream (`shared_volume_runtime_unavailable`), so no shared reservation is
// ever handed to a running workload.
func (c *Controller) admitLocalRaise(record VolumeRecord, requested int64, nodeByID map[string]scheduler.Node, localByNode map[string]int64) (string, int64) {
	if record.Type != config.VolumeTypeLocal || record.BoundNode == "" {
		return "", 0
	}
	// A request that does not increase the record's contribution cannot
	// over-commit the pool, so it is not subject to admission at all. This is
	// not just an optimization: because the contribution is
	// max(size, applied), a shrink leaves it unchanged until the shrink
	// actually applies — so checking it against a pool that is *already* over
	// capacity refuses the one operation that would restore the pool. The same
	// reasoning covers a node that is not currently observable: there is
	// nothing to verify when nothing new is being claimed.
	if recordContribution(requested, record.AppliedSizeBytes) <=
		recordContribution(record.DesiredSizeBytes, record.AppliedSizeBytes) {
		return "", 0
	}
	node, active := nodeByID[record.BoundNode]
	if !active {
		// The node's capacity is not observable, and a node being absent is
		// correlated with the node being in trouble — exactly when adopting an
		// unverifiable larger reservation is worst. Only the raise waits; the
		// existing effective size keeps rendering.
		return scheduler.ReasonStorageCapacityUnknown, 0
	}
	total := localByNode[record.BoundNode]
	old := recordContribution(record.DesiredSizeBytes, record.AppliedSizeBytes)
	fresh := recordContribution(requested, record.AppliedSizeBytes)
	// Subtract before adding so a large existing contribution cannot make the
	// running total transiently overflow.
	candidate := total - old
	if fresh > 0 && candidate > (1<<63-1)-fresh {
		return scheduler.ReasonNodeStorageExhausted, node.LocalCapacityBytes
	}
	candidate += fresh
	if candidate > node.LocalCapacityBytes {
		return scheduler.ReasonNodeStorageExhausted, node.LocalCapacityBytes - (total - old)
	}
	return "", 0
}

// availableLocalBytes recomputes the capacity figure a standing rejection was
// measured against, so a changed pool is reflected without restamping the time.
func (c *Controller) availableLocalBytes(record VolumeRecord, nodeByID map[string]scheduler.Node, localByNode map[string]int64) int64 {
	if record.Type != config.VolumeTypeLocal || record.BoundNode == "" {
		return 0
	}
	node, active := nodeByID[record.BoundNode]
	if !active {
		return 0
	}
	return node.LocalCapacityBytes - (localByNode[record.BoundNode] - recordContribution(record.DesiredSizeBytes, record.AppliedSizeBytes))
}

// recordContribution is what storageReservations charges for one record.
func recordContribution(desired, applied int64) int64 {
	return max64(max64(desired, 0), max64(applied, 0))
}

func (c *Controller) putVolumeRecord(ctx context.Context, service, volume string, token objectstorage.WriteToken, record VolumeRecord) (storedVolumeRecord, bool, error) {
	record.UpdatedAt = time.Now().UTC()
	ok, newToken, err := c.store.PutJSONIfMatch(ctx, mustVolumeRecordKey(c.cfg.State.Prefix, service, volume), token, record)
	if err != nil {
		return storedVolumeRecord{}, false, err
	}
	if !ok {
		return storedVolumeRecord{}, false, fmt.Errorf("volume %s/%s changed concurrently; retry reconciliation", service, volume)
	}
	return storedVolumeRecord{Record: record, Token: newToken}, true, nil
}

func orderedServiceIndexes(services []config.ServiceConfig) []int {
	indexes := make([]int, len(services))
	for i := range services {
		indexes[i] = i
	}
	sort.Slice(indexes, func(i, j int) bool { return services[indexes[i]].Name < services[indexes[j]].Name })
	return indexes
}

func orderedVolumeIndexes(volumes []config.VolumeConfig) []int {
	indexes := make([]int, len(volumes))
	for i := range volumes {
		indexes[i] = i
	}
	sort.Slice(indexes, func(i, j int) bool { return volumes[indexes[i]].Name < volumes[indexes[j]].Name })
	return indexes
}

func (c *Controller) createAssignedVolumeRecords(ctx context.Context, nodeConfigs []config.NodeConfig, set volumeRecordSet) error {
	now := time.Now().UTC()
	for _, node := range nodeConfigs {
		for _, service := range node.Services {
			for _, volume := range service.Volumes {
				logicalID := service.Name + "/" + volume.Name
				if _, exists := set.Records[logicalID]; exists {
					continue
				}
				// A quarantined key already has an object. Creating a fresh
				// record over it would overwrite state that has not been read.
				if _, quarantined := set.Quarantined[logicalID]; quarantined {
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
				set.Records[logicalID] = storedVolumeRecord{Record: record, Token: token}
			}
		}
	}
	return nil
}

func storageReservations(set volumeRecordSet) scheduler.StorageReservations {
	reservations := scheduler.StorageReservations{
		LocalByNode: make(map[string]int64), SharedByBackend: make(map[string]int64),
		RecordedLogicalIDs:     make(map[string]bool, len(set.Records)+len(set.Quarantined)),
		LocalUnknownByNode:     make(map[string]bool),
		SharedUnknownByBackend: make(map[string]bool),
		SharedEnabled:          false,
	}
	for id, stored := range set.Records {
		record := stored.Record
		size := recordContribution(record.DesiredSizeBytes, record.AppliedSizeBytes)
		reservations.RecordedLogicalIDs[id] = true
		if record.Type == config.VolumeTypeLocal {
			reservations.LocalByNode[record.BoundNode] += size
		} else {
			reservations.SharedByBackend[record.SharedBackendID] += size
		}
	}
	// A quarantined record still holds capacity. Dropping it would silently
	// release the reservation and turn a hard failure into over-commit, which
	// is the failure mode the admission check exists to prevent. So each tier
	// charges what it can prove and flags the scope it cannot.
	for id, quarantine := range set.Quarantined {
		// Marked recorded so the owner's volume never contributes a *second*
		// delta on top of the reservation charged here.
		reservations.RecordedLogicalIDs[id] = true
		switch quarantine.Tier {
		case quarantineTierExact, quarantineTierPartial:
			switch quarantine.Class {
			case config.VolumeTypeLocal:
				reservations.LocalByNode[quarantine.BoundNode] += quarantine.ReservedBytes
				if quarantine.Tier == quarantineTierPartial {
					reservations.LocalUnknownByNode[quarantine.BoundNode] = true
				}
			case config.VolumeTypeShared:
				reservations.SharedByBackend[quarantine.SharedBackendID] += quarantine.ReservedBytes
				if quarantine.Tier == quarantineTierPartial {
					reservations.SharedUnknownByBackend[quarantine.SharedBackendID] = true
				}
			}
		default:
			// No binding, so there is no account to charge. The block widens to
			// the class if the type parsed, and to both classes if it did not —
			// the key encodes only service and volume names, never the class.
			switch quarantine.Class {
			case config.VolumeTypeLocal:
				reservations.LocalClassUnknown = true
			case config.VolumeTypeShared:
				reservations.SharedClassUnknown = true
			default:
				reservations.LocalClassUnknown = true
				reservations.SharedClassUnknown = true
			}
		}
	}
	return reservations
}

// digestEntry is the normalized scheduling-visible outcome for one record key.
//
// Quarantined objects have no valid VolumeRecord, so hashing only the valid
// ones would leave the scheduling signature unchanged across transitions that
// must trigger re-placement — a partial repair that narrows a block, a changed
// binding that moves which node is blocked, or the full repair an operator is
// actively waiting on.
//
// Reason is deliberately excluded: it is display text, and hashing it would
// make a reworded error message invalidate the signature cache.
type digestEntry struct {
	Key             string            `json:"key"`
	Record          *VolumeRecord     `json:"record,omitempty"`
	Tier            int               `json:"tier,omitempty"`
	BoundNode       string            `json:"bound_node,omitempty"`
	SharedBackendID string            `json:"shared_backend_id,omitempty"`
	Class           config.VolumeType `json:"class,omitempty"`
	ReservedBytes   int64             `json:"reserved_bytes,omitempty"`
}

func volumeRecordsDigest(set volumeRecordSet) string {
	ordered := make([]digestEntry, 0, len(set.Records)+len(set.Quarantined))
	for id, stored := range set.Records {
		record := stored.Record
		ordered = append(ordered, digestEntry{Key: id, Record: &record})
	}
	for id, quarantine := range set.Quarantined {
		ordered = append(ordered, digestEntry{
			Key: id, Tier: quarantine.Tier, BoundNode: quarantine.BoundNode,
			SharedBackendID: quarantine.SharedBackendID, Class: quarantine.Class,
			ReservedBytes: quarantine.ReservedBytes,
		})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Key < ordered[j].Key })
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
				if observed.State != "prepared" && observed.State != "error" && observed.State != "rejected" {
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
				// A state written by a newer control plane is carried through
				// untouched: an older controller must not advance a state
				// machine it does not understand.
				if !knownVolumeResizeState(record.ResizeState) {
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
				case "rejected":
					// The generation alone does not say the refusal is still
					// outstanding. A record whose refusal was withdrawn sits at
					// the same generation, so a stale heartbeat would reopen it
					// — and the next desired-state pass clears it again, two
					// durable writes every tick, with a crash between them
					// leaving the degraded state behind. Accept the
					// observation only while the record is still refusing, or
					// while it already records this same refusal.
					if !record.rejectionStands() && record.ResizeState != VolumeResizeRejected {
						if record.DesiredSizeBytes != observed.RequestedSizeBytes {
							continue
						}
					}
					// The agent refused this size. Converge the record on the
					// effective size in one write: RequestedSizeBytes keeps the
					// refused size for display, DesiredSizeBytes becomes what
					// is actually running, and the generation is left alone so
					// the rejection stays keyed to this one request.
					//
					// After this write DesiredSizeBytes means "effective" for
					// both rejection kinds, which is what lets one clamp serve
					// both. The invariant is that the record never renders a
					// size the cluster is not running.
					if observed.AppliedSizeBytes <= 0 || observed.RequestedSizeBytes <= 0 {
						continue
					}
					if record.ResizeState == VolumeResizeRejected &&
						record.DesiredSizeBytes == observed.AppliedSizeBytes &&
						record.AppliedSizeBytes == observed.AppliedSizeBytes &&
						record.RequestedSizeBytes == observed.RequestedSizeBytes {
						continue
					}
					if record.RejectedAt.IsZero() || record.RequestedSizeBytes != observed.RequestedSizeBytes {
						record.RejectedAt = time.Now().UTC()
					}
					record.RequestedSizeBytes = observed.RequestedSizeBytes
					record.DesiredSizeBytes = observed.AppliedSizeBytes
					record.AppliedSizeBytes = observed.AppliedSizeBytes
					record.ResizeState = VolumeResizeRejected
					record.RejectedReason = observed.RejectedReason
					record.LastError = statusmodel.BoundedMessage(observed.LastError)
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
