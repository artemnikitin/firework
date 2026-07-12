package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/scheduler"
	"gopkg.in/yaml.v3"
)

// Controller runs scheduling and publishing loops.
type Controller struct {
	cfg    Config
	store  StateStore
	logger *slog.Logger

	id                 string
	epoch              int64
	leader             bool
	lastInputSignature string
}

// NewController creates a controller runtime.
func NewController(cfg Config, store StateStore, logger *slog.Logger) *Controller {
	host, _ := os.Hostname()
	return &Controller{
		cfg:    cfg,
		store:  store,
		logger: logger,
		id:     fmt.Sprintf("%s-%d-%d", host, os.Getpid(), time.Now().UTC().UnixNano()),
	}
}

// Run runs leader election and reconcile loops until context cancellation.
func (c *Controller) Run(ctx context.Context) error {
	renewTicker := time.NewTicker(c.cfg.LeaderRenewInterval)
	defer renewTicker.Stop()
	reconcileTicker := time.NewTicker(c.cfg.ControllerTick)
	defer reconcileTicker.Stop()

	_ = c.renewLeadership(ctx)
	if c.leader {
		c.runReconcile(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-renewTicker.C:
			if err := c.renewLeadership(ctx); err != nil {
				c.logger.Warn("leader renewal failed", "error", err)
			}
		case <-reconcileTicker.C:
			if c.leader {
				c.runReconcile(ctx)
			}
		}
	}
}

func (c *Controller) renewLeadership(ctx context.Context) error {
	key := controllerLockKey(c.cfg.State.Prefix)
	now := time.Now().UTC()

	var current LeaderLock
	token, exists, err := c.store.GetJSON(ctx, key, &current)
	if err != nil {
		return err
	}

	wasLeader := c.leader

	if !exists {
		lock := LeaderLock{
			HolderID:       c.id,
			LeaderEpoch:    1,
			LeaseExpiresAt: now.Add(c.cfg.LeaderLeaseTTL),
			RenewedAt:      now,
		}
		ok, _, err := c.store.PutJSONIfAbsent(ctx, key, lock)
		if err != nil {
			return err
		}
		if ok {
			c.leader = true
			c.epoch = lock.LeaderEpoch
		} else {
			c.leader = false
		}
	} else if current.HolderID == c.id {
		current.LeaseExpiresAt = now.Add(c.cfg.LeaderLeaseTTL)
		current.RenewedAt = now
		ok, _, err := c.store.PutJSONIfMatch(ctx, key, token, current)
		if err != nil {
			return err
		}
		c.leader = ok
		c.epoch = current.LeaderEpoch
	} else if current.LeaseExpiresAt.Before(now) {
		lock := LeaderLock{
			HolderID:       c.id,
			LeaderEpoch:    current.LeaderEpoch + 1,
			LeaseExpiresAt: now.Add(c.cfg.LeaderLeaseTTL),
			RenewedAt:      now,
		}
		ok, _, err := c.store.PutJSONIfMatch(ctx, key, token, lock)
		if err != nil {
			return err
		}
		if ok {
			c.leader = true
			c.epoch = lock.LeaderEpoch
		} else {
			c.leader = false
		}
	} else {
		c.leader = false
	}

	if !wasLeader && c.leader {
		// Fresh leadership should force at least one full reconciliation pass.
		c.lastInputSignature = ""
		c.logger.Info("controller became leader", "id", c.id, "epoch", c.epoch)
	}
	if wasLeader && !c.leader {
		// Drop memoized input signature so re-acquire does not skip verification.
		c.lastInputSignature = ""
		c.logger.Warn("controller lost leadership", "id", c.id)
	}
	return nil
}

func (c *Controller) runReconcile(ctx context.Context) {
	if !c.leader {
		return
	}

	var desiredPtr RevisionPointer
	_, exists, err := c.store.GetJSON(ctx, desiredCurrentKey(c.cfg.State.Prefix), &desiredPtr)
	if err != nil {
		c.logger.Error("reading desired pointer failed", "error", err)
		return
	}
	if !exists || desiredPtr.Revision == "" {
		c.logger.Debug("no desired revision published yet")
		return
	}

	var desired DesiredRevision
	_, exists, err = c.store.GetJSON(ctx, desiredRevisionKey(c.cfg.State.Prefix, desiredPtr.Revision), &desired)
	if err != nil {
		c.logger.Error("reading desired revision failed", "revision", desiredPtr.Revision, "error", err)
		return
	}
	if !exists {
		c.logger.Warn("desired revision pointer targets missing object", "revision", desiredPtr.Revision)
		return
	}

	activeNodes, hostIPByNode, err := c.discoverActiveNodes(ctx)
	if err != nil {
		c.logger.Error("discovering active nodes failed", "error", err)
		return
	}
	if len(activeNodes) == 0 && len(desired.Services) > 0 {
		c.logger.Warn("no active nodes available for scheduling", "services", len(desired.Services))
		return
	}
	inputSig, err := schedulingInputSignature(desired.Revision, activeNodes, hostIPByNode)
	if err != nil {
		c.logger.Error("failed to compute scheduling input signature; skipping signature cache optimization", "error", err)
	}
	if inputSig != "" && c.lastInputSignature == inputSig {
		c.logger.Debug("reconcile skipped: desired revision and active nodes unchanged",
			"desired_revision", desired.Revision,
			"nodes", len(activeNodes),
		)
		return
	}

	existingAssignment, err := c.readExistingAssignment(ctx)
	if err != nil {
		c.logger.Warn("reading existing placement failed; will re-place all", "error", err)
		existingAssignment = nil
	}

	assignments, err := scheduler.Schedule(desired.Services, activeNodes, existingAssignment)
	if err != nil {
		c.logger.Error("scheduling failed", "error", err, "services", len(desired.Services), "nodes", len(activeNodes))
		return
	}

	nodeConfigs := scheduler.BuildNodeConfigs(assignments)
	applyHostIPAndCrossNodeLinks(nodeConfigs, hostIPByNode)

	placementRev := PlacementRevision{
		Revision:        newRevision("placement"),
		DesiredRevision: desired.Revision,
		LeaderEpoch:     c.epoch,
		CreatedAt:       time.Now().UTC(),
		NodeConfigs:     nodeConfigs,
	}
	if err := c.publishPlacement(ctx, placementRev); err != nil {
		c.logger.Error("publishing placement failed", "error", err)
		return
	}

	renderRev := newRevision("rendered")
	if err := c.publishRendered(ctx, renderRev, nodeConfigs); err != nil {
		c.logger.Error("publishing rendered configs failed", "error", err)
		return
	}
	c.lastInputSignature = inputSig

	c.logger.Info("reconcile complete",
		"desired_revision", desired.Revision,
		"placement_revision", placementRev.Revision,
		"rendered_revision", renderRev,
		"services", len(desired.Services),
		"nodes", len(nodeConfigs),
	)
}

func (c *Controller) discoverActiveNodes(ctx context.Context) ([]scheduler.Node, map[string]string, error) {
	keys, err := c.store.ListKeys(ctx, registryNodesPrefix(c.cfg.State.Prefix))
	if err != nil {
		return nil, nil, err
	}

	now := time.Now().UTC()
	var nodes []scheduler.Node
	hostIPByNode := make(map[string]string)
	for _, key := range keys {
		var rec NodeRecord
		_, exists, err := c.store.GetJSON(ctx, key, &rec)
		if err != nil {
			c.logger.Warn("failed reading node record", "key", key, "error", err)
			continue
		}
		if !exists {
			continue
		}
		if rec.State != NodeStateReady {
			continue
		}
		if rec.LastSeenAt.IsZero() || now.Sub(rec.LastSeenAt) > c.cfg.NodeStaleTTL {
			continue
		}
		if rec.Capacity.VCPUs <= 0 || rec.Capacity.MemoryMB <= 0 {
			continue
		}
		nodes = append(nodes, scheduler.Node{
			InstanceID:    rec.NodeID,
			CapacityVCPUs: rec.Capacity.VCPUs,
			CapacityMemMB: rec.Capacity.MemoryMB,
		})
		if rec.HostIP != "" {
			hostIPByNode[rec.NodeID] = rec.HostIP
		}
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].InstanceID < nodes[j].InstanceID
	})
	return nodes, hostIPByNode, nil
}

func (c *Controller) readExistingAssignment(ctx context.Context) (map[string]string, error) {
	var ptr RevisionPointer
	_, exists, err := c.store.GetJSON(ctx, placementCurrentKey(c.cfg.State.Prefix), &ptr)
	if err != nil || !exists || ptr.Revision == "" {
		return nil, err
	}

	var rev PlacementRevision
	_, exists, err = c.store.GetJSON(ctx, placementRevisionKey(c.cfg.State.Prefix, ptr.Revision), &rev)
	if err != nil || !exists {
		return nil, err
	}

	assignment := make(map[string]string)
	for _, nc := range rev.NodeConfigs {
		for _, svc := range nc.Services {
			assignment[svc.Name] = nc.Node
		}
	}
	return assignment, nil
}

func (c *Controller) stillLeader(ctx context.Context) bool {
	var lock LeaderLock
	_, exists, err := c.store.GetJSON(ctx, controllerLockKey(c.cfg.State.Prefix), &lock)
	if err != nil || !exists {
		return false
	}
	return lock.HolderID == c.id && lock.LeaderEpoch == c.epoch && lock.LeaseExpiresAt.After(time.Now().UTC())
}

func (c *Controller) publishPlacement(ctx context.Context, placement PlacementRevision) error {
	if !c.stillLeader(ctx) {
		return fmt.Errorf("lost leadership before placement publish")
	}
	revKey := placementRevisionKey(c.cfg.State.Prefix, placement.Revision)
	if _, err := c.store.PutJSON(ctx, revKey, placement); err != nil {
		return err
	}
	return upsertPointer(ctx, c.store, placementCurrentKey(c.cfg.State.Prefix), placement.Revision)
}

func (c *Controller) publishRendered(ctx context.Context, renderRev string, nodeConfigs []config.NodeConfig) error {
	if !c.stillLeader(ctx) {
		return fmt.Errorf("lost leadership before rendered publish")
	}

	desiredLegacyKeys := make(map[string]struct{}, len(nodeConfigs))

	for _, nc := range nodeConfigs {
		data, err := yaml.Marshal(nc)
		if err != nil {
			return fmt.Errorf("marshal node config %s: %w", nc.Node, err)
		}
		renderKey := renderedNodeKey(c.cfg.State.Prefix, renderRev, nc.Node)
		if _, err := c.store.PutRaw(ctx, renderKey, data, "application/x-yaml"); err != nil {
			return err
		}

		legacyKey := legacyNodeConfigKey(c.cfg.State.Prefix, nc.Node)
		desiredLegacyKeys[legacyKey] = struct{}{}
		if _, err := c.store.PutRaw(ctx, legacyKey, data, "application/x-yaml"); err != nil {
			return err
		}
	}

	// Clean stale legacy node configs so agents that still read nodes/<node>.yaml
	// don't keep obsolete placements forever.
	keys, err := c.store.ListKeys(ctx, legacyNodesPrefix(c.cfg.State.Prefix))
	if err != nil {
		return err
	}
	for _, key := range keys {
		if !strings.HasSuffix(key, ".yaml") {
			continue
		}
		if _, keep := desiredLegacyKeys[key]; keep {
			continue
		}
		if err := c.store.Delete(ctx, key); err != nil {
			return err
		}
	}

	return upsertPointer(ctx, c.store, renderedCurrentKey(c.cfg.State.Prefix), renderRev)
}

func applyHostIPAndCrossNodeLinks(nodeConfigs []config.NodeConfig, hostIPByNode map[string]string) {
	for i := range nodeConfigs {
		if ip := hostIPByNode[nodeConfigs[i].Node]; ip != "" {
			nodeConfigs[i].HostIP = ip
		}
	}

	serviceNode := make(map[string]config.NodeConfig)
	for _, nc := range nodeConfigs {
		for _, svc := range nc.Services {
			serviceNode[svc.Name] = nc
		}
	}

	for i := range nodeConfigs {
		for j := range nodeConfigs[i].Services {
			svc := &nodeConfigs[i].Services[j]
			needsEnv := len(svc.CrossNodeLinks) > 0 || svc.NodeHostIPEnv != ""
			if !needsEnv {
				continue
			}
			if svc.Env == nil {
				svc.Env = make(map[string]string)
			}
			for _, link := range svc.CrossNodeLinks {
				peerNC, ok := serviceNode[link.Service]
				if !ok || peerNC.HostIP == "" {
					continue
				}
				svc.Env[link.Env] = fmt.Sprintf("%s:%d", peerNC.HostIP, link.HostPort)
			}
			if svc.NodeHostIPEnv != "" && nodeConfigs[i].HostIP != "" {
				svc.Env[svc.NodeHostIPEnv] = nodeConfigs[i].HostIP
			}
		}
	}
}

func schedulingInputSignature(desiredRevision string, nodes []scheduler.Node, hostIPByNode map[string]string) (string, error) {
	// Intentionally excludes runtime "used" resources from node heartbeats.
	// Current scheduler decisions are based on node total capacity plus desired
	// assignment bookkeeping, not host-reported instantaneous utilization.
	type nodeInput struct {
		ID         string `json:"id"`
		CapacityV  int    `json:"capacity_v"`
		CapacityMB int    `json:"capacity_mb"`
		HostIP     string `json:"host_ip,omitempty"`
	}
	payload := struct {
		DesiredRevision string      `json:"desired_revision"`
		Nodes           []nodeInput `json:"nodes"`
	}{
		DesiredRevision: desiredRevision,
		Nodes:           make([]nodeInput, 0, len(nodes)),
	}
	for _, n := range nodes {
		payload.Nodes = append(payload.Nodes, nodeInput{
			ID:         n.InstanceID,
			CapacityV:  n.CapacityVCPUs,
			CapacityMB: n.CapacityMemMB,
			HostIP:     hostIPByNode[n.InstanceID],
		})
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
