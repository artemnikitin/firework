package controlplane

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

const maxPreviousPlacementOwners = 1024

func (c *Controller) buildUnconfirmedPreviousOwners(
	ctx context.Context,
	previous PlacementRevision,
	_ []config.ServiceConfig,
	current []config.NodeConfig,
) ([]PreviousPlacementOwner, error) {
	currentNodeByService := make(map[string]string)
	for _, node := range current {
		for _, service := range node.Services {
			currentNodeByService[service.Name] = node.Node
		}
	}
	owners := make(map[PreviousPlacementOwner]struct{})
	add := func(owner PreviousPlacementOwner) error {
		if owner.Service == "" || owner.Node == "" {
			return fmt.Errorf("previous placement has an incomplete service owner")
		}
		if currentNodeByService[owner.Service] == owner.Node || c.previousOwnerResolved(ctx, owner) {
			return nil
		}
		owners[owner] = struct{}{}
		if len(owners) > maxPreviousPlacementOwners {
			return fmt.Errorf("unconfirmed previous owner count exceeds limit %d", maxPreviousPlacementOwners)
		}
		return nil
	}
	for _, owner := range previous.UnconfirmedPrevious {
		if err := add(owner); err != nil {
			return nil, err
		}
	}
	for _, node := range previous.NodeConfigs {
		for _, service := range node.Services {
			if err := add(PreviousPlacementOwner{Service: service.Name, Node: node.Node}); err != nil {
				return nil, err
			}
		}
	}
	if len(owners) > maxPreviousPlacementOwners {
		return nil, fmt.Errorf("unconfirmed previous owner count %d exceeds limit %d", len(owners), maxPreviousPlacementOwners)
	}

	out := make([]PreviousPlacementOwner, 0, len(owners))
	for owner := range owners {
		out = append(out, owner)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service == out[j].Service {
			return out[i].Node < out[j].Node
		}
		return out[i].Service < out[j].Service
	})
	return out, nil
}

// Resolution only ends the control plane's ownership wait. Retention expiry
// and metadata retirement are not evidence that a process stopped or that
// local storage can be reused.
func (c *Controller) previousOwnerResolved(ctx context.Context, owner PreviousPlacementOwner) bool {
	var record NodeRecord
	_, active, err := c.store.GetJSON(ctx, mustNodeRecordKey(c.cfg.State.Prefix, owner.Node), &record)
	if err != nil {
		return false
	}
	var tombstone NodeTombstone
	_, retired, err := c.store.GetJSON(ctx, mustNodeTombstoneKey(c.cfg.State.Prefix, owner.Node), &tombstone)
	if err != nil {
		return false
	}
	if retired && tombstone.NodeID == owner.Node && validRetirementReason(tombstone.Reason) &&
		(!active || record.Generation <= tombstone.MaxGeneration) {
		return true
	}
	if !active || record.NodeID != owner.Node {
		return false
	}
	return previousOwnerWaitExpired(record, time.Now().UTC(), c.cfg) ||
		confirmedServiceExecutionAbsent(ctx, c.store, c.cfg.State.Prefix, c.cfg.NodeStaleTTL, owner.Node, owner.Service)
}

func previousOwnerWaitExpired(record NodeRecord, now time.Time, cfg Config) bool {
	defaults := DefaultConfig()
	if cfg.NodeDownRetention <= 0 {
		cfg.NodeDownRetention = defaults.NodeDownRetention
	}
	if cfg.NodeStaleRetention <= 0 {
		cfg.NodeStaleRetention = defaults.NodeStaleRetention
	}
	_, expired := retirementEligibility(record, now, cfg)
	return expired
}

func upsertPlacementPointer(ctx context.Context, store StateStore, key, revision string) error {
	for attempt := 0; attempt < 6; attempt++ {
		var current RevisionPointer
		token, exists, err := store.GetJSON(ctx, key, &current)
		if err != nil {
			return err
		}
		if exists && current.Revision == revision {
			return nil
		}
		next := RevisionPointer{Revision: revision, UpdatedAt: time.Now().UTC()}
		if exists {
			next.PreviousRevision = current.Revision
			ok, _, err := store.PutJSONIfMatch(ctx, key, token, next)
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
			continue
		}
		ok, _, err := store.PutJSONIfAbsent(ctx, key, next)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return fmt.Errorf("placement pointer update conflict for %s", key)
}
