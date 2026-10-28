package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/objectstorage"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

const maxRevivalAuditScan = 10_000

// NodeTombstoneRecoveryRequest is an explicit operator attestation. The
// command cannot verify host fencing or local-pool identity itself; it makes
// both claims mandatory and records their references before changing the
// generation floor.
type NodeTombstoneRecoveryRequest struct {
	NodeID                 string
	ExpectedGeneration     int64
	Reason                 string
	ExternalFenceReference string
	RetainedPoolReference  string
	ConfirmedHostFenced    bool
}

// ClearNodeTombstone writes an audit intent before conditionally removing an
// exact tombstone and then conditionally removes its index entry. The empty
// rendered node document must still exist and no active registry record may
// remain. It never touches volume records or host data.
func ClearNodeTombstone(ctx context.Context, store StateStore, prefix string, request NodeTombstoneRecoveryRequest) (NodeRevivalAudit, error) {
	request, err := validateNodeTombstoneRecoveryRequest(request)
	if err != nil {
		return NodeRevivalAudit{}, err
	}
	if _, ok := currentExplicitEmptyNodeConfig(ctx, store, prefix, request.NodeID); !ok {
		return NodeRevivalAudit{}, errors.New("current rendered node config is not a proven explicit empty document")
	}
	if err := requireNoActiveNodeRecord(ctx, store, prefix, request.NodeID); err != nil {
		return NodeRevivalAudit{}, err
	}
	index, exists, err := loadNodeTombstoneIndex(ctx, store, prefix)
	if err != nil || !exists {
		if err != nil {
			return NodeRevivalAudit{}, fmt.Errorf("reading node tombstone index: %w", err)
		}
		return NodeRevivalAudit{}, errors.New("node tombstone index is missing")
	}
	indexPosition := sort.SearchStrings(index.NodeIDs, request.NodeID)
	if indexPosition >= len(index.NodeIDs) || index.NodeIDs[indexPosition] != request.NodeID {
		return NodeRevivalAudit{}, errors.New("node tombstone is missing from the index")
	}

	tombstoneKey, _ := nodeTombstoneKey(prefix, request.NodeID)
	var tombstone NodeTombstone
	tombstoneToken, tombstoneExists, err := store.GetJSON(ctx, tombstoneKey, &tombstone)
	if err != nil {
		return NodeRevivalAudit{}, fmt.Errorf("reading node tombstone: %w", err)
	}
	var audit NodeRevivalAudit
	var auditToken objectstorage.WriteToken
	if tombstoneExists {
		if tombstone.NodeID != request.NodeID || tombstone.MaxGeneration != request.ExpectedGeneration || !validRetirementReason(tombstone.Reason) {
			return NodeRevivalAudit{}, errors.New("node tombstone does not match the expected node and generation")
		}
		if tombstoneToken == "" {
			return NodeRevivalAudit{}, errors.New("node tombstone has no conditional-write token")
		}
		audit = newNodeRevivalAudit(request, tombstoneToken)
		audit, auditToken, err = pendingNodeRevivalAudit(ctx, store, prefix, request, audit.TombstoneTokenSHA256)
		if err != nil {
			return NodeRevivalAudit{}, err
		}
		if auditToken == "" {
			audit = newNodeRevivalAudit(request, tombstoneToken)
			auditKey, keyErr := nodeRevivalAuditKey(prefix, request.NodeID, revivalAuditID(audit))
			if keyErr != nil {
				return NodeRevivalAudit{}, keyErr
			}
			created, token, writeErr := store.PutJSONIfAbsent(ctx, auditKey, audit)
			if writeErr != nil {
				return NodeRevivalAudit{}, fmt.Errorf("writing node revival audit intent: %w", writeErr)
			}
			if !created {
				var current NodeRevivalAudit
				auditToken, exists, err = store.GetJSON(ctx, auditKey, &current)
				if err != nil || !exists || !sameRevivalRequest(current, audit) {
					if err != nil {
						return NodeRevivalAudit{}, fmt.Errorf("reading existing node revival audit: %w", err)
					}
					return NodeRevivalAudit{}, errors.New("existing node revival audit does not match this request")
				}
				audit = current
			} else {
				auditToken = token
			}
		}
	} else {
		// A prior invocation may have deleted the tombstone and crashed before
		// removing its index entry. Resume only from a durable pending intent.
		audit, auditToken, err = pendingNodeRevivalAudit(ctx, store, prefix, request, "")
		if err != nil {
			return NodeRevivalAudit{}, err
		}
		if auditToken == "" {
			return NodeRevivalAudit{}, errors.New("node tombstone is absent and no pending revival audit can be resumed")
		}
	}

	if err := requireNoActiveNodeRecord(ctx, store, prefix, request.NodeID); err != nil {
		return finalizeRevivalAudit(ctx, store, prefix, audit, auditToken, "active_record_present", err)
	}
	if _, ok := currentExplicitEmptyNodeConfig(ctx, store, prefix, request.NodeID); !ok {
		err := errors.New("current rendered node config changed from explicit empty state")
		return finalizeRevivalAudit(ctx, store, prefix, audit, auditToken, "empty_config_changed", err)
	}

	if tombstoneExists {
		deleted, deleteErr := store.DeleteIfMatch(ctx, tombstoneKey, tombstoneToken)
		if deleteErr != nil {
			err := fmt.Errorf("conditionally deleting node tombstone: %w", deleteErr)
			return finalizeRevivalAudit(ctx, store, prefix, audit, auditToken, "delete_failed", err)
		}
		if !deleted {
			var current NodeTombstone
			_, stillExists, readErr := store.GetJSON(ctx, tombstoneKey, &current)
			if readErr != nil || stillExists {
				err := errors.New("node tombstone changed concurrently; no tombstone was cleared")
				if readErr != nil {
					err = fmt.Errorf("checking concurrent tombstone change: %w", readErr)
				}
				return finalizeRevivalAudit(ctx, store, prefix, audit, auditToken, "delete_conflict", err)
			}
		}
	}
	if audit.ClearedAt.IsZero() {
		audit.ClearedAt = time.Now().UTC()
	}

	if err := removeNodeTombstoneIndexEntry(ctx, store, prefix, request.NodeID); err != nil {
		return finalizeRevivalAudit(ctx, store, prefix, audit, auditToken, "index_pending", err)
	}
	var appeared NodeTombstone
	_, tombstoneExists, err = store.GetJSON(ctx, tombstoneKey, &appeared)
	if err != nil || tombstoneExists {
		if err != nil {
			return finalizeRevivalAudit(ctx, store, prefix, audit, auditToken, "index_pending", fmt.Errorf("verifying node tombstone removal: %w", err))
		}
		return finalizeRevivalAudit(ctx, store, prefix, audit, auditToken, "index_pending", errors.New("a node tombstone appeared during recovery"))
	}
	return finalizeRevivalAudit(ctx, store, prefix, audit, auditToken, "completed", nil)
}

func validateNodeTombstoneRecoveryRequest(request NodeTombstoneRecoveryRequest) (NodeTombstoneRecoveryRequest, error) {
	if strings.TrimSpace(request.NodeID) == "" || strings.Contains(request.NodeID, "/") {
		return request, errors.New("node ID must be non-empty and must not contain '/'")
	}
	if request.ExpectedGeneration <= 0 {
		return request, errors.New("expected generation must be positive")
	}
	if !request.ConfirmedHostFenced {
		return request, errors.New("external host fencing must be explicitly confirmed")
	}
	request.Reason = statusmodel.BoundedMessage(strings.TrimSpace(request.Reason))
	request.ExternalFenceReference = statusmodel.BoundedMessage(strings.TrimSpace(request.ExternalFenceReference))
	request.RetainedPoolReference = statusmodel.BoundedMessage(strings.TrimSpace(request.RetainedPoolReference))
	if request.Reason == "" {
		return request, errors.New("operator reason is required")
	}
	if request.ExternalFenceReference == "" {
		return request, errors.New("external fence reference is required")
	}
	if request.RetainedPoolReference == "" {
		return request, errors.New("retained local-pool reference is required (use 'none' after verifying no local volume is bound)")
	}
	return request, nil
}

func currentExplicitEmptyNodeConfig(ctx context.Context, store StateStore, prefix, nodeID string) (config.NodeConfig, bool) {
	var first RevisionPointer
	_, exists, err := store.GetJSON(ctx, renderedCurrentKey(prefix), &first)
	if err != nil || !exists || first.Revision == "" {
		return config.NodeConfig{}, false
	}
	nodeConfig, ok := readExplicitEmptyNodeConfig(ctx, store, prefix, nodeID)
	if !ok || nodeConfig.RenderedRevision != first.Revision {
		return config.NodeConfig{}, false
	}
	var second RevisionPointer
	_, exists, err = store.GetJSON(ctx, renderedCurrentKey(prefix), &second)
	if err != nil || !exists || second.Revision != first.Revision {
		return config.NodeConfig{}, false
	}
	return nodeConfig, true
}

func requireNoActiveNodeRecord(ctx context.Context, store StateStore, prefix, nodeID string) error {
	key, err := nodeRecordKey(prefix, nodeID)
	if err != nil {
		return err
	}
	_, _, exists, err := store.GetRaw(ctx, key)
	if err != nil {
		return fmt.Errorf("checking active node record: %w", err)
	}
	if exists {
		return errors.New("active node record still exists; wait for retirement to finish")
	}
	return nil
}

func newNodeRevivalAudit(request NodeTombstoneRecoveryRequest, tombstoneToken objectstorage.WriteToken) NodeRevivalAudit {
	hash := sha256.Sum256([]byte(tombstoneToken))
	return NodeRevivalAudit{
		NodeID: request.NodeID, ClearedGeneration: request.ExpectedGeneration, IntentAt: time.Now().UTC(),
		OperatorReason: request.Reason, ExternalFenceReference: request.ExternalFenceReference,
		RetainedPoolReference: request.RetainedPoolReference, TombstoneTokenSHA256: hex.EncodeToString(hash[:]),
		Outcome: "pending",
	}
}

func revivalAuditID(audit NodeRevivalAudit) string {
	return fmt.Sprintf("%020d-%s", audit.IntentAt.UTC().UnixNano(), audit.TombstoneTokenSHA256[:16])
}

func sameRevivalRequest(left, right NodeRevivalAudit) bool {
	return left.NodeID == right.NodeID && left.ClearedGeneration == right.ClearedGeneration &&
		left.TombstoneTokenSHA256 == right.TombstoneTokenSHA256 && left.OperatorReason == right.OperatorReason &&
		left.ExternalFenceReference == right.ExternalFenceReference && left.RetainedPoolReference == right.RetainedPoolReference
}

func pendingNodeRevivalAudit(ctx context.Context, store StateStore, prefix string, request NodeTombstoneRecoveryRequest, tokenHash string) (NodeRevivalAudit, objectstorage.WriteToken, error) {
	keys, err := store.ListKeys(ctx, nodeRevivalAuditsPrefix(prefix, request.NodeID))
	if err != nil {
		return NodeRevivalAudit{}, "", fmt.Errorf("listing node revival audits: %w", err)
	}
	if len(keys) > maxRevivalAuditScan {
		return NodeRevivalAudit{}, "", fmt.Errorf("node revival audit history exceeds the bounded resume scan of %d", maxRevivalAuditScan)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	for _, key := range keys {
		var audit NodeRevivalAudit
		token, exists, err := store.GetJSON(ctx, key, &audit)
		if err != nil {
			if scanInterrupted(ctx, err) {
				return NodeRevivalAudit{}, "", err
			}
			continue
		}
		if !exists || audit.NodeID != request.NodeID || audit.ClearedGeneration != request.ExpectedGeneration ||
			(audit.Outcome != "pending" && audit.Outcome != "index_pending") || (tokenHash != "" && audit.TombstoneTokenSHA256 != tokenHash) {
			continue
		}
		if audit.OperatorReason != request.Reason || audit.ExternalFenceReference != request.ExternalFenceReference || audit.RetainedPoolReference != request.RetainedPoolReference {
			return NodeRevivalAudit{}, "", errors.New("pending revival audit must be resumed with the same operator attestations")
		}
		return audit, token, nil
	}
	return NodeRevivalAudit{}, "", nil
}

func removeNodeTombstoneIndexEntry(ctx context.Context, store StateStore, prefix, nodeID string) error {
	key := nodeTombstoneIndexKey(prefix)
	for attempt := 0; attempt < 6; attempt++ {
		var index NodeTombstoneIndex
		token, exists, err := store.GetJSON(ctx, key, &index)
		if err != nil {
			return fmt.Errorf("reading tombstone index before recovery: %w", err)
		}
		if !exists || !validNodeTombstoneIndex(index) {
			return errors.New("node tombstone index is missing or invalid during recovery")
		}
		position := sort.SearchStrings(index.NodeIDs, nodeID)
		if position >= len(index.NodeIDs) || index.NodeIDs[position] != nodeID {
			return nil
		}
		var tombstone NodeTombstone
		_, tombstoneExists, err := store.GetJSON(ctx, mustNodeTombstoneKey(prefix, nodeID), &tombstone)
		if err != nil {
			return fmt.Errorf("checking tombstone before index update: %w", err)
		}
		if tombstoneExists {
			return errors.New("node tombstone reappeared before index update")
		}
		index.NodeIDs = append(index.NodeIDs[:position], index.NodeIDs[position+1:]...)
		index.Count = len(index.NodeIDs)
		index.Revision++
		index.UpdatedAt = time.Now().UTC()
		ok, _, err := store.PutJSONIfMatch(ctx, key, token, index)
		if err != nil {
			return fmt.Errorf("conditionally removing tombstone index entry: %w", err)
		}
		if ok {
			return nil
		}
	}
	return errors.New("too many concurrent tombstone index changes")
}

func finalizeRevivalAudit(ctx context.Context, store StateStore, prefix string, audit NodeRevivalAudit, token objectstorage.WriteToken, outcome string, operationErr error) (NodeRevivalAudit, error) {
	if outcome == "completed" && audit.ClearedAt.IsZero() {
		audit.ClearedAt = time.Now().UTC()
	}
	audit.Outcome = outcome
	auditKey, err := nodeRevivalAuditKey(prefix, audit.NodeID, revivalAuditID(audit))
	if err != nil {
		if operationErr != nil {
			return audit, fmt.Errorf("%v; writing audit outcome: %w", operationErr, err)
		}
		return audit, err
	}
	ok, _, writeErr := store.PutJSONIfMatch(ctx, auditKey, token, audit)
	if writeErr != nil || !ok {
		if writeErr == nil {
			writeErr = errors.New("audit record changed concurrently")
		}
		if operationErr != nil {
			return audit, fmt.Errorf("%v; finalizing audit outcome: %w", operationErr, writeErr)
		}
		return audit, fmt.Errorf("finalizing audit outcome: %w", writeErr)
	}
	if operationErr != nil {
		return audit, operationErr
	}
	return audit, nil
}
