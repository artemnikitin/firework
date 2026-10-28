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
	desired []config.ServiceConfig,
	current []config.NodeConfig,
) ([]PreviousPlacementOwner, error) {
	desiredNames := make(map[string]struct{}, len(desired))
	for _, service := range desired {
		desiredNames[service.Name] = struct{}{}
	}
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
		if _, wanted := desiredNames[owner.Service]; !wanted || currentNodeByService[owner.Service] == owner.Node {
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
		if confirmedNodeExecutionAbsent(ctx, c.store, c.cfg.State.Prefix, c.cfg.NodeStaleTTL, owner.Node) {
			continue
		}
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
