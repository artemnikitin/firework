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
	// Node discovery is hoisted above the record work because admission needs
	// node capacity in scope: a size request is only feasible relative to the
	// pool it would land in. discoverActiveNodes does not read volume records,
	// so the move is safe, and schedulingInputSignature is computed after both
	// either way.
	activeNodes, hostIPByNode, err := c.discoverActiveNodes(ctx)
	if err != nil {
		c.logger.Error("discovering active nodes failed", "error", err)
		return
	}
	if err := c.acknowledgeVolumeRecords(ctx); err != nil {
		c.logger.Warn("acknowledging volume records failed", "error", err)
	}
	volumeRecords, err := c.loadVolumeRecords(ctx)
	if err != nil {
		c.logger.Error("loading retained volume records failed", "error", err)
		return
	}
	services := append([]config.ServiceConfig(nil), desired.Services...)
	for i := range services {
		services[i].Volumes = append([]config.VolumeConfig(nil), desired.Services[i].Volumes...)
	}
	admission, err := c.applyExistingVolumeRecords(ctx, services, volumeRecords, activeNodes)
	if err != nil {
		c.logger.Error("reconciling volume records failed", "error", err)
		return
	}

	inputSig, err := schedulingInputSignature(desired.Revision, activeNodes, hostIPByNode, volumeRecordsDigest(volumeRecords))
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

	existingPlacement, placementFound, err := c.readExistingPlacement(ctx)
	if err != nil {
		c.logger.Warn("reading existing placement failed; will re-place all", "error", err)
		existingPlacement = nil
	}
	// A held service is one already running that must keep running, and the
	// prior placement is the only source for where. Without it the service
	// would be classified as never-placed and left pending, which drops it
	// from the rendered node configs — and the agent turns an absent service
	// into a delete. Publishing anything here would evict a healthy workload
	// over a transient read failure, so nothing is published and the next tick
	// retries. Omission is eviction; there is no partial answer to give.
	if heldPlacementUnrecoverable(placementFound, admission.Held) {
		c.logger.Error("holding services but the previous placement is unreadable; not publishing",
			"held", len(admission.Held))
		return
	}
	existingAssignment := make(map[string]string, len(existingPlacement))
	for name, placed := range existingPlacement {
		existingAssignment[name] = placed.Node
	}

	// A service whose own volume record cannot be used is held, not dropped.
	// Omitting it from the rendered node configs is what the agent turns into
	// a delete, so a malformed *record* would stop a healthy *workload*.
	schedulable, held, heldPending := splitHeldServices(services, admission, existingPlacement)
	schedulingNodes := reserveHeldCapacity(activeNodes, held)

	assignments, pending := scheduler.ScheduleWithStorage(
		schedulable, schedulingNodes, existingAssignment, storageReservations(volumeRecords), heldPortClaims(held))
	for node, services := range held {
		assignments[node] = append(assignments[node], services...)
	}
	pending = append(pending, heldPending...)
	sort.Slice(pending, func(i, j int) bool { return pending[i].Service < pending[j].Service })

	nodeConfigs := scheduler.BuildNodeConfigs(assignments)
	nodeConfigs = appendRetiredNodeConfigs(nodeConfigs, activeNodes)
	if err := c.createAssignedVolumeRecords(ctx, nodeConfigs, volumeRecords); err != nil {
		c.logger.Warn("creating volume records conflicted; retrying on next tick", "error", err)
		return
	}
	applyHostIPAndCrossNodeLinks(nodeConfigs, hostIPByNode)

	renderRev := newRevision("rendered")
	placementID := newRevision("placement")
	for i := range nodeConfigs {
		nodeConfigs[i].DesiredRevision = desired.Revision
		nodeConfigs[i].PlacementRevision = placementID
		nodeConfigs[i].RenderedRevision = renderRev
	}
	placementRev := PlacementRevision{
		Revision:        placementID,
		DesiredRevision: desired.Revision,
		LeaderEpoch:     c.epoch,
		CreatedAt:       time.Now().UTC(),
		NodeConfigs:     nodeConfigs,
	}
	for _, item := range pending {
		placementRev.PendingServices = append(placementRev.PendingServices, PendingPlacement{
			Service: item.Service, ReasonCode: item.ReasonCode, Message: item.Message,
		})
	}
	for _, services := range held {
		for _, service := range services {
			placementRev.HeldServices = append(placementRev.HeldServices, PendingPlacement{
				Service: service.Name, ReasonCode: admission.Held[service.Name],
				Message: "running the last applied configuration; the desired one could not be resolved",
			})
		}
	}
	sort.Slice(placementRev.HeldServices, func(i, j int) bool {
		return placementRev.HeldServices[i].Service < placementRev.HeldServices[j].Service
	})
	if err := c.publishPlacement(ctx, placementRev); err != nil {
		c.logger.Error("publishing placement failed", "error", err)
		return
	}

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
		"pending_services", len(pending),
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
			InstanceID:          rec.NodeID,
			CapacityVCPUs:       rec.Capacity.VCPUs,
			CapacityMemMB:       rec.Capacity.MemoryMB,
			LocalCapacityBytes:  rec.Storage.LocalCapacityBytes,
			SharedBackendID:     rec.Storage.SharedBackendID,
			SharedCapacityBytes: rec.Storage.SharedCapacityBytes,
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

// renderedPlacement is one service as the previous cycle actually rendered it:
// its node together with its complete configuration at the effective volume
// sizes the cluster accepted.
//
// The placement map alone (service to node) cannot supply that configuration,
// and the malformed record is by definition not a source either. Without this
// third source, "re-render its last effective configuration" is unimplementable
// and the obvious shortcut — re-rendering the desired revision's volume config
// — silently reintroduces the very size the record was supposed to gate.
type renderedPlacement struct {
	Node    string
	Service config.ServiceConfig
}

// readExistingPlacement returns the previous placement and whether it could be
// established at all.
//
// found is false both for a read error and for missing state, and the
// difference matters to exactly one caller. A pointer that names a revision
// whose object is gone is *not* the same as "nothing has been placed yet":
// services may well be running under it. Treating a missing object as an empty
// placement is what lets a held service be classified as never-placed and
// dropped from the rendered configs, which the agent turns into a delete.
//
// An absent pointer is genuinely a cluster that has never placed anything, but
// it is reported the same way because the only caller that consults found also
// requires a held service — which requires a retained volume record, which a
// cluster that has never placed anything does not have.
func (c *Controller) readExistingPlacement(ctx context.Context) (placement map[string]renderedPlacement, found bool, err error) {
	var ptr RevisionPointer
	_, exists, err := c.store.GetJSON(ctx, placementCurrentKey(c.cfg.State.Prefix), &ptr)
	if err != nil || !exists || ptr.Revision == "" {
		return nil, false, err
	}

	var rev PlacementRevision
	_, exists, err = c.store.GetJSON(ctx, placementRevisionKey(c.cfg.State.Prefix, ptr.Revision), &rev)
	if err != nil || !exists {
		return nil, false, err
	}

	placement = make(map[string]renderedPlacement)
	for _, nc := range rev.NodeConfigs {
		for _, svc := range nc.Services {
			placement[svc.Name] = renderedPlacement{Node: nc.Node, Service: svc}
		}
	}
	return placement, true, nil
}

// splitHeldServices separates the services that can be scheduled from their
// desired configuration from those whose volume records cannot be used.
//
// Blocking is scoped by whether the owner is already running:
//
//   - already placed: hold the last placement and re-render it at its last
//     known effective volume configuration. The service keeps running, and no
//     resize, rebinding, or capacity change is applied while the record is
//     unreadable.
//   - not yet placed: withhold placement. There is nothing running to disturb,
//     and admitting it would allocate against capacity that cannot be verified.
//
// When the prior rendered snapshot is unavailable — a first reconcile after a
// prefix change, or an unreadable placement revision — the service is *still*
// never omitted, because omission is eviction. It is re-rendered from the
// desired revision instead, and charged no reservation; the scope block from
// storageReservations is what keeps anything new from being admitted against
// capacity that cannot be proved.
func splitHeldServices(services []config.ServiceConfig, admission volumeAdmission, placement map[string]renderedPlacement) ([]config.ServiceConfig, map[string][]config.ServiceConfig, []scheduler.Pending) {
	if len(admission.Held) == 0 {
		return services, nil, nil
	}
	schedulable := make([]config.ServiceConfig, 0, len(services))
	held := make(map[string][]config.ServiceConfig)
	var pending []scheduler.Pending
	for _, service := range services {
		reason, blocked := admission.Held[service.Name]
		if !blocked {
			schedulable = append(schedulable, service)
			continue
		}
		placed, wasPlaced := placement[service.Name]
		if !wasPlaced {
			pending = append(pending, scheduler.Pending{
				Service: service.Name, ReasonCode: reason,
				Message: "volume record cannot be read; placement withheld until it is repaired",
			})
			continue
		}
		rendered := placed.Service
		if len(rendered.Volumes) == 0 {
			// No prior snapshot of the volume configuration; render the
			// desired one unchanged rather than dropping the service.
			rendered = service
		}
		held[placed.Node] = append(held[placed.Node], rendered)
	}
	return schedulable, held, pending
}

// heldPlacementUnrecoverable reports whether publishing this cycle could evict
// a running service.
//
// A held service is one already running that must keep running, and the prior
// placement is the only source for where it runs. When that read fails, a held
// service would be classified as never-placed and left pending, which drops it
// from the rendered node configs — and the agent turns an absent service into a
// delete. Omission is eviction, and there is no partial answer to give, so the
// cycle publishes nothing and the next tick retries.
//
// With nothing held, a failed placement read is harmless: the scheduler simply
// re-places everything from the desired revision, which is the pre-existing
// behavior.
func heldPlacementUnrecoverable(placementFound bool, held map[string]string) bool {
	return !placementFound && len(held) > 0
}

// heldPortClaims collects the node-exclusive host-port claims a held service is
// still holding.
//
// A held service is re-rendered outside the scheduler, so the scheduler cannot
// see its claims and would otherwise place a new service claiming the same
// (tcp, host_port) on the same node. The agent rejects a node config with a
// duplicate claim outright, so that would take down every service on the node —
// a strictly worse outcome than the unreadable record that caused the hold.
func heldPortClaims(held map[string][]config.ServiceConfig) map[string]map[config.PortClaim]string {
	if len(held) == 0 {
		return nil
	}
	claims := make(map[string]map[config.PortClaim]string, len(held))
	for node, services := range held {
		for _, service := range services {
			for _, claim := range service.PortClaims() {
				if claims[node] == nil {
					claims[node] = make(map[config.PortClaim]string)
				}
				claims[node][claim] = service.Name
			}
		}
	}
	return claims
}

// reserveHeldCapacity removes the compute a held service is still using from
// the capacity offered to the scheduler, so re-rendering it outside the
// scheduler cannot over-commit the node it runs on.
func reserveHeldCapacity(nodes []scheduler.Node, held map[string][]config.ServiceConfig) []scheduler.Node {
	if len(held) == 0 {
		return nodes
	}
	adjusted := append([]scheduler.Node(nil), nodes...)
	for i := range adjusted {
		for _, service := range held[adjusted[i].InstanceID] {
			adjusted[i].CapacityVCPUs -= service.VCPUs
			adjusted[i].CapacityMemMB -= service.MemoryMB
		}
		if adjusted[i].CapacityVCPUs < 0 {
			adjusted[i].CapacityVCPUs = 0
		}
		if adjusted[i].CapacityMemMB < 0 {
			adjusted[i].CapacityMemMB = 0
		}
	}
	return adjusted
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

// appendRetiredNodeConfigs adds an explicit empty config for every active node
// the placement assigned no services to. scheduler.BuildNodeConfigs omits such
// a node entirely, and publishRendered then deletes its nodes/<node>.yaml —
// but an agent treats a missing config as a fetch failure, not as "run
// nothing", so it keeps every VM it already had. Meanwhile the fleet view
// derives its relevant node set from this same placement, so the retired node
// stops being counted at all and the revision can report converged on the
// strength of the nodes that did receive work. During an A -> B migration
// that means B alone converges the revision while A still runs the old
// workload.
//
// Publishing an explicit empty config instead turns retirement into an
// ordinary convergence step: the node fetches it, reconciles down to zero
// services, and reports the new revision like every other node — and stays in
// the relevant set until it does. This must run before the revision metadata
// is stamped onto nodeConfigs below, because a config published without those
// revisions can never satisfy the fleet view's observed check.
//
// Nodes missing from activeNodes entirely (down, stale, or decommissioned)
// are deliberately not included: they are not part of this placement at all,
// and publishRendered's stale-key cleanup still applies to them.
func appendRetiredNodeConfigs(nodeConfigs []config.NodeConfig, activeNodes []scheduler.Node) []config.NodeConfig {
	placed := make(map[string]struct{}, len(nodeConfigs))
	for _, nc := range nodeConfigs {
		placed[nc.Node] = struct{}{}
	}
	for _, node := range activeNodes {
		if _, ok := placed[node.InstanceID]; ok {
			continue
		}
		nodeConfigs = append(nodeConfigs, config.NodeConfig{Node: node.InstanceID})
	}
	sort.Slice(nodeConfigs, func(i, j int) bool { return nodeConfigs[i].Node < nodeConfigs[j].Node })
	return nodeConfigs
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
			// Links sharing an env key join into a comma-separated list (in
			// spec order), so a service can receive a multi-address value such
			// as an Elasticsearch discovery.seed_hosts set. The first resolved
			// link still replaces any same-named static env value.
			linkSet := make(map[string]bool)
			for _, link := range svc.CrossNodeLinks {
				peerNC, ok := serviceNode[link.Service]
				if !ok || peerNC.HostIP == "" {
					continue
				}
				address := fmt.Sprintf("%s:%d", peerNC.HostIP, link.HostPort)
				if link.Protocol != "" {
					address = fmt.Sprintf("%s://%s", link.Protocol, address)
				}
				if linkSet[link.Env] {
					svc.Env[link.Env] += "," + address
				} else {
					svc.Env[link.Env] = address
					linkSet[link.Env] = true
				}
			}
			if svc.NodeHostIPEnv != "" && nodeConfigs[i].HostIP != "" {
				svc.Env[svc.NodeHostIPEnv] = nodeConfigs[i].HostIP
			}
		}
	}
}

func schedulingInputSignature(desiredRevision string, nodes []scheduler.Node, hostIPByNode map[string]string, volumeDigest string) (string, error) {
	// Intentionally excludes runtime "used" resources from node heartbeats.
	// Current scheduler decisions are based on node total capacity plus desired
	// assignment bookkeeping, not host-reported instantaneous utilization.
	type nodeInput struct {
		ID                  string `json:"id"`
		CapacityV           int    `json:"capacity_v"`
		CapacityMB          int    `json:"capacity_mb"`
		HostIP              string `json:"host_ip,omitempty"`
		LocalCapacityBytes  int64  `json:"local_capacity_bytes,omitempty"`
		SharedBackendID     string `json:"shared_backend_id,omitempty"`
		SharedCapacityBytes int64  `json:"shared_capacity_bytes,omitempty"`
	}
	payload := struct {
		DesiredRevision     string      `json:"desired_revision"`
		Nodes               []nodeInput `json:"nodes"`
		VolumeRecordsDigest string      `json:"volume_records_digest,omitempty"`
	}{
		DesiredRevision:     desiredRevision,
		Nodes:               make([]nodeInput, 0, len(nodes)),
		VolumeRecordsDigest: volumeDigest,
	}
	for _, n := range nodes {
		payload.Nodes = append(payload.Nodes, nodeInput{
			ID:                  n.InstanceID,
			CapacityV:           n.CapacityVCPUs,
			CapacityMB:          n.CapacityMemMB,
			HostIP:              hostIPByNode[n.InstanceID],
			LocalCapacityBytes:  n.LocalCapacityBytes,
			SharedBackendID:     n.SharedBackendID,
			SharedCapacityBytes: n.SharedCapacityBytes,
		})
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
