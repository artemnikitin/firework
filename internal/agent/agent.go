package agent

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/artemnikitin/firework/internal/agentconfig"
	"github.com/artemnikitin/firework/internal/api"
	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/healthcheck"
	"github.com/artemnikitin/firework/internal/imagesync"
	"github.com/artemnikitin/firework/internal/ingress"
	"github.com/artemnikitin/firework/internal/network"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/statusmodel"
	"github.com/artemnikitin/firework/internal/store"
	"github.com/artemnikitin/firework/internal/traefik"
	"github.com/artemnikitin/firework/internal/vm"
	"github.com/artemnikitin/firework/internal/volume"
)

// routeSyncer applies the desired Traefik route set for the local and remote
// services. It is satisfied by *traefik.Manager and kept as an interface so the
// agent's retry and last-known-good behavior can be tested with a fake.
type routeSyncer interface {
	Sync(services []config.ServiceConfig, remoteNodes []config.NodeConfig) error
	// SyncLocal applies local routes only, leaving existing remote-* route
	// files untouched. Used when the peer node configs cannot be listed.
	SyncLocal(services []config.ServiceConfig) error
}

type imageSyncer interface {
	Sync(context.Context, []config.ServiceConfig) error
	Close() error
}

// Agent is the core component that runs on each node. It periodically pulls
// the desired state from the config store and reconciles it with the actual
// state of running Firecracker microVMs.
type Agent struct {
	cfg             agentconfig.AgentConfig
	store           store.Store
	vmManager       *vm.Manager
	volumeManager   *volume.Manager
	reconciler      *reconciler.Reconciler
	healthMon       *healthcheck.Monitor
	networkMgr      *network.Manager
	imageSyncer     imageSyncer
	apiServer       *api.Server
	logger          *slog.Logger
	metrics         *runtimeMetrics
	capacityReader  capacity.Reader
	traefikMgr      routeSyncer
	registryClient  *registryClient
	recoveryControl *recoveryControlServer
	recoveryWake    chan struct{}
	recoveryMu      sync.Mutex
	recoveryQueue   map[string]bool
	forceReconcile  bool
	// lastPeerNodes is the last complete peer listing, and peersListed says
	// whether this process has one. While listing fails, its remote route
	// files are preserved, and local routes resolve hostname claims against
	// it. Only the tick goroutine uses them.
	lastPeerNodes []config.NodeConfig
	peersListed   bool
	// routeProcess identifies the process serving traefikMgr's routes, which
	// decides when a withdrawn route's address may go to another service.
	routeProcess routeProcess
	// proveServiceAbsent replaces vmManager.ProveServiceAbsent in tests.
	proveServiceAbsent func(context.Context, string) error
	lifecycleMu        sync.Mutex
	// restartNeedsReconcile is set by a health restart that left its service
	// without a running VM; the next tick then skips the unchanged-revision
	// shortcut.
	restartNeedsReconcile atomic.Bool

	lastRevision         string
	lastImageRevision    string
	lastKnownCapacity    capacity.NodeCapacity
	hasLastKnownCapacity bool

	// heartbeatMu guards the resource snapshot read by the independent
	// heartbeat goroutine. Updated by the reconcile loop; read by runHeartbeat.
	heartbeatMu    sync.RWMutex
	heartbeatCap   capacity.NodeCapacity
	heartbeatUsed  capacity.NodeCapacity
	heartbeatReady bool // true once capacity has been read at least once

	statusMu       sync.RWMutex
	currentStatus  statusmodel.AgentStatus
	statusServices []config.ServiceConfig
	restartCounts  map[string]int
	// evaluatedConditions is initialized for each reconciliation tick and is
	// guarded by statusMu. Conditions not reached by the tick are finalized as
	// unknown without rewriting transitions that were actually observed.
	evaluatedConditions map[string]struct{}
}

// New creates a new Agent with all its dependencies.
func New(cfg agentconfig.AgentConfig, s store.Store, logger *slog.Logger) *Agent {
	metrics := newRuntimeMetrics(cfg.NodeName)
	volumeMgr := volume.NewManagerWithObserver(cfg.NodeID, cfg.Storage, metrics)
	vmMgr := vm.NewManagerWithVolumes(cfg.FirecrackerBin, cfg.StateDir, logger, volumeMgr)
	var agentRef *Agent

	// Set up optional health check monitor.
	var healthMon *healthcheck.Monitor
	if cfg.EnableHealthChecks == nil || *cfg.EnableHealthChecks {
		restartFn := func(ctx context.Context, name string) error {
			return agentRef.restartService(ctx, name)
		}
		healthMon = healthcheck.NewMonitor(logger, restartFn)
	}

	// Set up optional network manager.
	var networkMgr *network.Manager
	if cfg.EnableNetworkSetup == nil || *cfg.EnableNetworkSetup {
		networkMgr = network.NewManager(logger)
	}

	var localRoutes func() (bool, error)
	routeEvidence := &agentRouteEvidence{}
	rec := reconciler.New(vmMgr, logger, healthMon, networkMgr, cfg.UpdateStrategy, cfg.UpdateDelay)
	if cfg.TraefikConfigDir != "" {
		localRoutes = func() (bool, error) { return traefik.HasLocalRouteFiles(cfg.TraefikConfigDir) }
		rec.WithRouteEvidence(routeEvidence)
	}
	rec.WithHostHistoryEvidence(cfg.VMSubnet, localRoutes)

	// Initialize shared bridge and masquerade if network setup is enabled.
	if networkMgr != nil && cfg.VMBridge != "" {
		if err := networkMgr.InitBridge(cfg.VMBridge, cfg.VMGateway, cfg.VMSubnet); err != nil {
			logger.Error("failed to initialize shared bridge", "error", err)
		}
		if cfg.OutInterface != "" {
			if err := networkMgr.SetupMasquerade(cfg.VMSubnet, cfg.OutInterface); err != nil {
				logger.Error("failed to setup masquerade", "error", err)
			}
		}
	}

	// Set up optional image syncer.
	var imgSyncer imageSyncer
	switch {
	case cfg.S3ImagesBucket != "":
		var err error
		imgSyncer, err = imagesync.NewS3Syncer(context.Background(), imagesync.S3Config{
			Bucket: cfg.S3ImagesBucket, Region: cfg.S3Region,
			EndpointURL: cfg.S3EndpointURL, ForcePathStyle: cfg.S3EndpointURL != "",
		}, cfg.ImagesDir, logger)
		if err != nil {
			logger.Error("failed to create S3 image syncer", "error", err)
		}
	case cfg.GCSImagesBucket != "":
		var err error
		imgSyncer, err = imagesync.NewGCSSyncer(context.Background(), imagesync.GCSConfig{
			Bucket: cfg.GCSImagesBucket, Project: cfg.GCSProject,
			CredentialsFile: cfg.GCSCredentialsFile,
		}, cfg.ImagesDir, logger)
		if err != nil {
			logger.Error("failed to create GCS image syncer", "error", err)
		}
	}

	// Set up optional capacity reader.
	var capReader capacity.Reader
	if cfg.EnableCapacityCheck == nil || *cfg.EnableCapacityCheck {
		capReader = capacity.NewOSReader()
	}

	// Set up optional Traefik config manager. Keep traefikMgr a nil interface
	// (not a typed-nil) when disabled so the a.traefikMgr == nil checks hold.
	var traefikMgr routeSyncer
	if cfg.TraefikConfigDir != "" {
		traefikMgr = traefik.NewManager(cfg.TraefikConfigDir, cfg.IngressDomain, logger)
	}

	a := &Agent{
		cfg:            cfg,
		store:          s,
		vmManager:      vmMgr,
		volumeManager:  volumeMgr,
		reconciler:     rec,
		healthMon:      healthMon,
		networkMgr:     networkMgr,
		imageSyncer:    imgSyncer,
		logger:         logger,
		metrics:        metrics,
		capacityReader: capReader,
		traefikMgr:     traefikMgr,
		routeProcess:   systemdRouteProcess{unit: cfg.TraefikUnit},
		restartCounts:  make(map[string]int),
		recoveryWake:   make(chan struct{}, 1),
		recoveryQueue:  make(map[string]bool),
		currentStatus: statusmodel.AgentStatus{
			SchemaVersion: statusmodel.SchemaVersion,
			NodeID:        cfg.NodeID,
			ObservedAt:    time.Now().UTC(),
			Phase:         statusmodel.PhaseUnknown,
		},
	}
	agentRef = a
	routeEvidence.agent = a
	// The journal is loaded once the routes it reads are wired up.
	rec.WithStateDir(cfg.StateDir)
	if cfg.RegistryURL != "" {
		a.registryClient = newRegistryClient(cfg, logger, time.Now().UTC().UnixNano())
	}

	// Set up optional API server.
	if cfg.APIListenAddr != "" {
		var healthProvider api.HealthResultsProvider
		if healthMon != nil {
			healthProvider = &healthAdapter{mon: healthMon}
		}
		a.apiServer = api.NewServer(cfg.APIListenAddr, logger, a, healthProvider, a)
	}

	return a
}

// Run starts the agent's reconciliation loop. It blocks until the context
// is cancelled (e.g., on SIGTERM).
func (a *Agent) Run(ctx context.Context) error {
	a.logger.Info("agent starting",
		"node", a.cfg.NodeName,
		"node_names", a.cfg.NodeNames,
		"node_labels", a.cfg.SchedulingLabels(),
		"store", a.cfg.StoreURL,
		"poll_interval", a.cfg.PollInterval,
	)

	// Recover/adopt surviving VMs and run an initial reconciliation before
	// exposing local status. This prevents a restart from briefly publishing an
	// empty node while durable VM ownership is still being reconstructed.
	a.tick(ctx)

	control, err := startRecoveryControlServer(a.cfg.StateDir, a.logger, a.clearRecoveryPending, a.forgetRecoveryPending)
	if err != nil {
		a.logger.Warn("local recovery control is unavailable", "error", err)
	} else {
		a.recoveryControl = control
	}

	if a.apiServer != nil {
		if err := a.apiServer.Start(); err != nil {
			if a.recoveryControl != nil {
				_ = a.recoveryControl.Close()
				a.recoveryControl = nil
			}
			return err
		}
	}

	// Start the independent heartbeat goroutine after the first tick so that
	// the registry client is already enrolled and capacity is known.
	go a.runHeartbeat(ctx)

	ticker := time.NewTicker(a.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			a.logger.Info("agent shutting down")
			a.shutdown()
			return ctx.Err()
		case <-a.recoveryWake:
			a.applyRecoveryRequests(ctx)
		case <-ticker.C:
			a.tick(ctx)
		}
	}
}

// runHeartbeat sends registry heartbeats on a fixed interval that is
// independent of the reconciliation loop. This prevents long-running
// operations (image downloads, reconciliation) from blocking heartbeats
// and causing the controller to mark the node as stale.
func (a *Agent) runHeartbeat(ctx context.Context) {
	if a.registryClient == nil {
		return
	}
	interval := a.cfg.RegistryHeartbeatInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cap, used, ready := a.getHeartbeatResources()
			if !ready {
				continue
			}
			a.syncRegistry(ctx, cap, used)
		}
	}
}

func (a *Agent) setHeartbeatResources(cap, used capacity.NodeCapacity) {
	a.heartbeatMu.Lock()
	defer a.heartbeatMu.Unlock()
	a.heartbeatCap = cap
	a.heartbeatUsed = used
	a.heartbeatReady = true
}

func (a *Agent) getHeartbeatResources() (cap capacity.NodeCapacity, used capacity.NodeCapacity, ready bool) {
	a.heartbeatMu.RLock()
	defer a.heartbeatMu.RUnlock()
	return a.heartbeatCap, a.heartbeatUsed, a.heartbeatReady
}

// shutdown cleans up all resources.
func (a *Agent) shutdown() {
	if a.recoveryControl != nil {
		if err := a.recoveryControl.Close(); err != nil {
			a.logger.Warn("failed to close recovery control socket", "error", err)
		}
		a.recoveryControl = nil
	}
	if a.imageSyncer != nil {
		_ = a.imageSyncer.Close()
	}
	if a.registryClient != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		a.registryClient.markDown(ctx, a.cfg.NodeID)
		cancel()
	}
	if a.healthMon != nil {
		a.healthMon.Stop()
	}
	if a.apiServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.apiServer.Stop(ctx)
	}
}

// tick performs a single reconciliation cycle.
func (a *Agent) tick(ctx context.Context) {
	a.logger.Debug("reconciliation tick starting")
	a.beginTickStatus()
	defer a.finishTickStatus()

	var (
		nodeCap capacity.NodeCapacity
		hasCap  bool
	)
	// Capture node capacity early so registry heartbeats include capacity even
	// when no desired services are assigned yet.
	if a.capacityReader != nil {
		nodeCap, hasCap = a.readNodeCapacity()
		if hasCap {
			a.metrics.setCapacity(nodeCap, capacity.NodeCapacity{})
			// Publish capacity to the heartbeat goroutine immediately so it can
			// keep the node active in the registry even while images are downloading.
			a.setHeartbeatResources(nodeCap, capacity.NodeCapacity{})
		}
	}

	// Fetch and merge configs from all node labels.
	merged, loadErr := a.fetchAndMerge(ctx)
	if loadErr != nil {
		if loadErr.fetchErr != nil {
			a.failAgentStatus("ConfigFetched", "config_fetch_failed", loadErr.fetchErr.Error())
		}
		if loadErr.parseErr != nil {
			if loadErr.fetchErr == nil {
				a.setStatusCondition("ConfigFetched", statusmodel.ConditionTrue, "", "")
			}
			a.failAgentStatus("ConfigParsed", "config_parse_failed", loadErr.parseErr.Error())
		}
		// The merged local config is incomplete. A nil service slice means
		// "unknown", not an empty desired route set, so preserve all
		// last-known-good local and peer routes until every label is available.
		a.logger.Warn("skipping traefik route sync because local config is incomplete")
		a.refreshRuntimeMetrics()
		a.syncRegistryAfterTick(ctx, nodeCap, capacity.NodeCapacity{})
		return
	}
	a.setStatusCondition("ConfigFetched", statusmodel.ConditionTrue, "", "")
	a.setStatusCondition("ConfigParsed", statusmodel.ConditionTrue, "", "")

	// Fence persisted survivors before assigning resources or planning new VMs.
	if err := a.vmManager.DiscoverRecoveryState(); err != nil {
		a.failAgentStatus("VMsReconciled", "recovery_discovery_failed", err.Error())
		a.refreshRuntimeMetrics()
		a.syncRegistryAfterTick(ctx, nodeCap, capacity.NodeCapacity{})
		return
	}

	// Clamp any volume size this node has already refused, before anything
	// else in the tick reads the merged configuration. Normalizing later — say,
	// just before Plan — would leave the status snapshot describing the raw
	// requested size while Plan, the instance, and the Firecracker config all
	// use the effective one, and the two surfaces would disagree permanently.
	a.vmManager.NormalizeVolumes(merged.Services)
	a.reportVolumeSizeConvergence()

	// Prepare desired state once per tick: assigned addresses (fetched node
	// configs do not persist them), resolved links and injected environment.
	// Status, holds, routes, port forwards and Reconcile all read this same
	// configuration, so they cannot disagree about whether a running VM
	// matches it.
	addressHolds := a.prepareDesired(merged.Services)

	// Check revision only after fetch, so stores that update revision state
	// during Fetch (Git pull, object write token) are evaluated against fresh data.
	// For multi-label nodes we skip this optimization because revision is
	// store-scoped, not label-scoped.
	var rev string
	if len(a.cfg.NodeNames) == 1 {
		var err error
		rev, err = a.store.Revision(ctx)
		if err != nil {
			a.logger.Error("failed to get store revision", "error", err)
		}
		a.setStatusServices(*merged, rev)
		// A health restart that could not restore its VM leaves the revision
		// unchanged; only a full tick recreates the service.
		if a.reconciler.HasPendingRecovery() || a.reconciler.HasPendingCleanup() || a.restartNeedsReconcile.Swap(false) {
			a.forceReconcile = true
		}
		if rev != "" && rev == a.lastRevision && !a.forceReconcile {
			// A local revision does not describe the peer node configs used for
			// remote Traefik routes. Refresh routes on every poll even when the
			// local VM configuration is unchanged, but avoid the expensive image
			// sync and reconcile work.

			// Host DNAT can be flushed by anything else on the box — a
			// firewalld reload, a container runtime restart — without the
			// config revision changing. Reconcile re-asserts port forwards,
			// but this path returns before it and is the common case in
			// steady state, so re-assert here too. Without this the rules
			// converge only on ticks that change the revision, while
			// markUnchangedRevisionReady below still reports NetworkReady.
			portForwardErr := a.reconciler.SyncPortForwards(*merged)
			if portForwardErr != nil {
				a.logger.Error("port forward refresh failed", "error", portForwardErr)
			}
			// Run the route refresh regardless, so one failing subsystem does
			// not stop the other from converging.
			traefikErr := a.syncTraefikConfigs(ctx, merged.Services)
			if traefikErr != nil {
				a.logger.Error("traefik route refresh failed", "error", traefikErr)
			}
			switch {
			case portForwardErr != nil:
				a.failAgentStatus("NetworkReady", "port_forward_sync_failed", portForwardErr.Error())
			case traefikErr != nil:
				a.failAgentStatus("LocalRoutesReady", "local_route_sync_failed", traefikErr.Error())
			default:
				// Routes did sync, so record that first — it holds whether or
				// not the VMs are healthy, and it makes the snapshot taken
				// inside a failed markUnchangedRevisionReady consistent.
				a.setStatusCondition("LocalRoutesReady", statusmodel.ConditionTrue, "", "")
				if a.markUnchangedRevisionReady() {
					a.refreshAgentStatus(statusmodel.PhaseReady, "", "")
				}
			}
			a.logger.Debug("config unchanged, skipped reconciliation after route refresh", "revision", rev)
			a.refreshRuntimeMetrics()
			return
		}
	} else {
		a.setStatusServices(*merged, "")
	}

	// Reject a node config whose services conflict on a host port before any
	// networking or service state changes. The control plane keeps colocated
	// claims unique, so this is defense in depth for a stale, hand-written, or
	// older-controller config: installing its DNAT rules would silently deliver
	// host-port traffic to whichever guest matched first.
	if err := config.ValidateNodePortClaims(*merged); err != nil {
		a.logger.Error("host-port claim preflight failed; not advancing revision", "error", err)
		a.failAgentStatus("NetworkReady", "host_port_conflict", err.Error())
		a.syncRegistry(ctx, nodeCap, capacity.NodeCapacity{})
		return
	}

	// Preflight routing metadata before any reconciliation so an invalid
	// direct node config or a missing ingress_domain fails the revision early
	// instead of being recorded as applied.
	if err := a.preflightRouting(merged.Services); err != nil {
		a.logger.Error("routing preflight failed; not advancing revision", "error", err)
		a.failAgentStatus("LocalRoutesReady", "invalid_routing_config", err.Error())
		a.syncRegistryAfterTick(ctx, nodeCap, capacity.NodeCapacity{})
		return
	}

	// NetworkReady is deliberately not set here. Routing preflight only
	// validates metadata; host networking is attempted later, inside Reconcile.
	// Claiming it now reports NetworkReady=true on ticks that fail at capacity
	// or image sync and never touch the network at all. Leaving it unevaluated
	// lets finishTickStatus finalize it as unknown, which is what the per-tick
	// condition contract says an unreached stage should be.

	// Admit desired growth against node capacity service by service. A
	// service that does not fit is held while independent work, including
	// recovery, still runs; genuine over-capacity fails the revision below.
	used, capacityHolds, allowance := a.admit(merged.Services, nodeCap, hasCap, addressHolds)
	a.reconciler.SetHeldServices(mergeHolds(addressHolds, capacityHolds))
	a.reconciler.SetCapacity(nodeCap, hasCap, allowance)
	// Update the heartbeat goroutine with the latest used resources now, before
	// the potentially long-running image sync, so heartbeats reflect current
	// scheduling intent while downloads are in progress.
	if hasCap {
		a.setHeartbeatResources(nodeCap, used)
		a.metrics.setCapacity(nodeCap, used)
	}
	admissionCode, admissionMessage := admissionFailure(addressHolds, capacityHolds)
	if total := sumResources(merged.Services); admissionCode == "" && hasCap && (total.VCPUs > nodeCap.VCPUs || total.MemoryMB > nodeCap.MemoryMB) {
		// Desired state that cannot fit fails even when every service already
		// runs, over capacity, and so none waits to grow.
		admissionCode, admissionMessage = "capacity_exceeded", capacityExceededReason
	}
	switch {
	case admissionCode != "":
		a.logger.Warn("desired services do not fit this node; holding those that do not fit", "reason", admissionCode, "detail", admissionMessage)
		a.setStatusCondition("CapacityReady", statusmodel.ConditionFalse, admissionCode, admissionMessage)
	case !hasCap:
		a.setStatusCondition("CapacityReady", statusmodel.ConditionTrue, "not_configured", "")
	default:
		a.setStatusCondition("CapacityReady", statusmodel.ConditionTrue, "", "")
	}

	// Sync images from S3 before reconciling (ensures rootfs/kernels are present).
	if a.imageSyncer != nil {
		syncStart := time.Now()
		err := a.syncImages(ctx, merged.Services, rev)
		a.metrics.observeImageSync(time.Since(syncStart), err != nil)
		if err != nil {
			a.logger.Error("image sync failed", "error", err)
			a.failAgentStatus("ImagesReady", "image_sync_failed", err.Error())
			a.syncRegistryAfterTick(ctx, nodeCap, used)
			return
		}
		a.setStatusCondition("ImagesReady", statusmodel.ConditionTrue, "", "")
	} else {
		a.setStatusCondition("ImagesReady", statusmodel.ConditionTrue, "not_configured", "")
	}

	// Reconcile desired vs actual state.
	reconcileStart := time.Now()
	// Serialize only Recover/Plan/Apply with health-driven Stop/Start, so a
	// restart never waits behind config fetch, image sync, route refresh or
	// the pauses of a rolling update. assignNetworking already reserved
	// addresses of every tracked VM, including one between a restart's Stop
	// and Start.
	a.lifecycleMu.Lock()
	err := a.reconciler.Reconcile(reconciler.WithPauseRelease(ctx, func() func() {
		a.lifecycleMu.Unlock()
		return a.lifecycleMu.Lock
	}), *merged)
	a.lifecycleMu.Unlock()
	a.metrics.observeReconcile(time.Since(reconcileStart), err != nil)
	if err == nil {
		a.forceReconcile = false
	}
	// Publish routes from the VMs that exist after this apply, whatever it
	// returned. Deletes and replacements run even while other work is held
	// or failing, and a route must never outlive its guest or point at an
	// address another service now owns. An error here means the local routes
	// were not applied, so the revision must not advance. (Peer-side failures
	// degrade inside syncTraefikConfigs without surfacing here.)
	traefikErr := a.syncTraefikConfigs(ctx, merged.Services)
	if traefikErr != nil {
		a.logger.Error("traefik route sync failed; not advancing revision", "error", traefikErr)
		a.setStatusCondition("LocalRoutesReady", statusmodel.ConditionFalse, "local_route_sync_failed", traefikErr.Error())
	} else {
		a.setStatusCondition("LocalRoutesReady", statusmodel.ConditionTrue, "", "")
	}
	if err != nil && admissionCode == "" && reconciler.OnlyRecoveryHeld(err) && a.reconciler.HasPendingRecovery() &&
		(!hasCap || recoveryOnly(merged.Services, nodeCap, a.reconciler.Instances())) {
		// Every hold was caused by unresolved VM ownership, which
		// VMRecoveryReady reports as degradation and held services report as
		// resource_held. Freezing the revision would hide both behind
		// "progressing". Pending recovery keeps forcing reconciliation, so the
		// holds still retry. Any other hold, such as genuine over-capacity or
		// an exhausted subnet, leaves the revision incomplete, and so does any
		// capacity the remaining work still needs on the runtime this apply
		// left; see recoveryOnly.
		a.logger.Info("applying revision with services held for VM recovery", "error", err)
		a.forceReconcile = true
		err = nil
	}
	var action *reconciler.ActionRequiredError
	if err != nil && reconciler.IsIncomplete(err) && errors.As(err, &action) {
		// Only an operator can resolve this hold, such as a host port another
		// service used on this node (L6). Report it as a failure naming the
		// action, not as a wait another tick may end. Independent work still
		// converged.
		a.logger.Warn("reconciliation needs an operator action", "reason", action.Code, "error", err)
		a.setStatusCondition("NetworkReady", statusmodel.ConditionTrue, "", "")
		a.setStatusCondition("VMsReconciled", statusmodel.ConditionFalse, action.Code, err.Error())
		a.failAgentStatus("Reconciled", action.Code, action.Reason)
		a.refreshRuntimeMetrics()
		a.syncRegistryAfterTick(ctx, nodeCap, used)
		return
	}
	if err != nil && reconciler.IsIncomplete(err) {
		// Every collected error was a hold or a benign start race: a stop or
		// remove arrived while a start was preparing volumes, or two starts
		// collided. Nothing failed, but not everything converged either, so
		// the revision must not advance and the node must not be reported as
		// having reconciled. Returning here leaves lastRevision unchanged, so
		// the next tick re-plans from real state instead of taking the
		// unchanged-revision shortcut.
		a.logger.Info("reconciliation incomplete; retrying on the next tick", "error", err)
		reason := "start_aborted"
		if errors.Is(err, reconciler.ErrServiceHeld) || errors.Is(err, reconciler.ErrRecoveryHeld) {
			reason = "resource_held"
		}
		a.setStatusCondition("NetworkReady", statusmodel.ConditionTrue, "", "")
		a.setStatusCondition("VMsReconciled", statusmodel.ConditionUnknown, reason, err.Error())
		if admissionCode != "" {
			// Genuine over-capacity or an exhausted subnet. What fits still
			// converged, but the revision cannot be applied as requested,
			// whether or not any other VM happens to be retained.
			a.setStatusCondition("Reconciled", statusmodel.ConditionUnknown, "reconcile_incomplete", err.Error())
			a.failAgentStatus("CapacityReady", admissionCode, admissionMessage)
		} else {
			a.incompleteAgentStatus("Reconciled", "reconcile_incomplete", err.Error())
		}
		a.refreshRuntimeMetrics()
		a.syncRegistryAfterTick(ctx, nodeCap, used)
		return
	}
	if err != nil {
		a.logger.Error("reconciliation failed", "error", err)
		networkFailed, vmFailed, code := classifyReconcileFailure(err)
		if networkFailed {
			a.setStatusCondition("NetworkReady", statusmodel.ConditionFalse, "network_setup_failed", err.Error())
		} else {
			a.setStatusCondition("NetworkReady", statusmodel.ConditionTrue, "", "")
		}
		if vmFailed || !networkFailed {
			a.setStatusCondition("VMsReconciled", statusmodel.ConditionFalse, "vm_reconcile_failed", err.Error())
		} else {
			a.setStatusCondition("VMsReconciled", statusmodel.ConditionUnknown, "not_reached", "")
		}
		a.failAgentStatus("Reconciled", code, err.Error())
		a.refreshRuntimeMetrics()
		a.syncRegistryAfterTick(ctx, nodeCap, used)
		return
	}
	a.setStatusCondition("NetworkReady", statusmodel.ConditionTrue, "", "")
	a.setStatusCondition("VMsReconciled", statusmodel.ConditionTrue, "", "")
	a.setStatusCondition("Reconciled", statusmodel.ConditionTrue, "", "")
	if traefikErr != nil {
		a.failAgentStatus("LocalRoutesReady", "local_route_sync_failed", traefikErr.Error())
		a.syncRegistryAfterTick(ctx, nodeCap, used)
		return
	}
	if admissionCode != "" {
		a.failAgentStatus("CapacityReady", admissionCode, admissionMessage)
		a.syncRegistryAfterTick(ctx, nodeCap, used)
		return
	}

	// Update the last known revision on success.
	if rev != "" {
		a.lastRevision = rev
	}
	appliedRevision := rev
	if appliedRevision == "" {
		appliedRevision = a.lastRevision
	}
	a.metrics.recordConfigApply(appliedRevision, time.Now())
	a.markAgentStatusApplied(appliedRevision)
	a.refreshRuntimeMetrics()
	a.syncRegistryAfterTick(ctx, nodeCap, used)

	a.logger.Debug("reconciliation tick completed", "revision", rev)
}

func classifyReconcileFailure(err error) (networkFailed, vmFailed bool, reasonCode string) {
	networkFailed = reconciler.HasFailureStage(err, reconciler.FailureStageNetwork)
	vmFailed = reconciler.HasFailureStage(err, reconciler.FailureStageVM)
	reasonCode = "vm_reconcile_failed"
	if networkFailed && !vmFailed {
		reasonCode = "network_setup_failed"
	}
	return networkFailed, vmFailed, reasonCode
}

// fetchAndMerge fetches and parses every configured node label. A partial
// result fails closed because it cannot describe one applied merged revision.
type configLoadError struct {
	fetchErr error
	parseErr error
}

func (e *configLoadError) Error() string {
	if e == nil {
		return ""
	}
	joined := errors.Join(e.fetchErr, e.parseErr)
	if joined == nil {
		return "configuration load failed"
	}
	return joined.Error()
}

func (a *Agent) fetchAndMerge(ctx context.Context) (*config.NodeConfig, *configLoadError) {
	seen := make(map[string]config.ServiceConfig)
	var loadErr configLoadError
	var desiredRevision, placementRevision, renderedRevision string

	for _, name := range a.cfg.NodeNames {
		data, err := a.store.Fetch(ctx, name)
		if err != nil {
			a.logger.Error("failed to fetch config from store", "label", name, "error", err)
			if loadErr.fetchErr == nil {
				loadErr.fetchErr = fmt.Errorf("fetch label %s: %w", name, err)
			}
			continue
		}
		a.metrics.recordConfigFetchSuccess(name, time.Now())
		if p, ok := a.store.(store.EnrichmentTimestampProvider); ok {
			if ts, ok := p.LastEnrichmentTimestamp(name); ok {
				a.metrics.recordEnrichmentTimestamp(name, ts)
			}
		}

		nc, err := config.ParseNodeConfig(data)
		if err != nil {
			a.logger.Error("failed to parse node config", "label", name, "error", err)
			if loadErr.parseErr == nil {
				loadErr.parseErr = fmt.Errorf("parse label %s: %w", name, err)
			}
			continue
		}

		desiredRevision = mergeRevisionMetadata(desiredRevision, nc.DesiredRevision)
		placementRevision = mergeRevisionMetadata(placementRevision, nc.PlacementRevision)
		renderedRevision = mergeRevisionMetadata(renderedRevision, nc.RenderedRevision)
		for _, svc := range nc.Services {
			if _, dup := seen[svc.Name]; dup {
				a.logger.Warn("duplicate service across labels, last wins",
					"service", svc.Name, "label", name)
			}
			seen[svc.Name] = svc
		}
	}

	if loadErr.fetchErr != nil || loadErr.parseErr != nil {
		return nil, &loadErr
	}

	// Sort by name for deterministic ordering (important for IP allocation).
	services := make([]config.ServiceConfig, 0, len(seen))
	for _, svc := range seen {
		services = append(services, svc)
	}
	sort.Slice(services, func(i, j int) bool {
		return services[i].Name < services[j].Name
	})

	return &config.NodeConfig{
		Node:              a.cfg.NodeName,
		Services:          services,
		DesiredRevision:   usableRevisionMetadata(desiredRevision),
		PlacementRevision: usableRevisionMetadata(placementRevision),
		RenderedRevision:  usableRevisionMetadata(renderedRevision),
	}, nil
}

const revisionMetadataConflict = "\x00"

func mergeRevisionMetadata(current, next string) string {
	if current == revisionMetadataConflict {
		return current
	}
	if current == "" {
		return next
	}
	if next == "" || current == next {
		return current
	}
	// Multiple label configs with different control-plane revisions do not
	// describe one converged snapshot. Clear the value instead of guessing.
	return revisionMetadataConflict
}

func usableRevisionMetadata(revision string) string {
	if revision == revisionMetadataConflict {
		return ""
	}
	return revision
}

const (
	addressExhaustedReason = "no free guest address in vm_subnet "
	addressRecoveryReason  = "guest address retained by unresolved VM ownership"
	addressTransientReason = "guest addresses are still reserved by VMs being stopped or removed"
	// addressWithdrawalReason takes the Traefik unit (L7).
	addressWithdrawalReason = "the free guest addresses were withdrawn from other services' routes, and %s may still serve those routes; restart it to release them"
)

// assignNetworking fills in IP, MAC, and kernel IP boot args for services
// that have networking enabled. Services must be sorted by name for
// deterministic IP allocation. It returns the services that could not get an
// address, as holds classified like capacity: the desired services alone
// exceed the subnet, an undesired quarantined VM keeps the address, or an
// undesired VM the plan removes keeps it for one more tick.
func (a *Agent) assignNetworking(services []config.ServiceConfig) map[string]reconciler.Hold {
	if a.cfg.VMSubnet == "" {
		return nil
	}

	gateway := stripCIDR(a.cfg.VMGateway)
	netmask := network.SubnetMaskBits(a.cfg.VMSubnet)

	// Parse subnet to compute sequential IPs starting at network + 2.
	ip, ipNet, err := net.ParseCIDR(a.cfg.VMSubnet)
	if err != nil {
		a.logger.Error("invalid vm_subnet", "subnet", a.cfg.VMSubnet, "error", err)
		return nil
	}
	ip = ip.To4()
	if ip == nil {
		a.logger.Error("vm_subnet is not IPv4", "subnet", a.cfg.VMSubnet)
		return nil
	}
	ipNet.IP = ip

	reservedIP, reservedMAC := map[string]string{}, map[string]string{}
	retained := map[string]config.ServiceConfig{}
	desired := make(map[string]bool, len(services))
	networked := 0
	for _, svc := range services {
		desired[svc.Name] = true
		if svc.Network != nil {
			networked++
		}
	}
	// Why an undesired VM keeps an address or MAC: quarantine or a removal
	// that recovery holds, or a removal this tick's plan performs.
	var quarantinedReserves, departingReserves bool
	if a.reconciler != nil {
		deleteHolds := a.reconciler.DeleteHolds(config.NodeConfig{Services: services})
		// Every tracked VM keeps its address: a route-only tick must not shift
		// a running guest to a freed IP, and a crashed or restarting service
		// must not lose its IP to first-fit reallocation. Retained VMs keep
		// theirs even outside the current subnet.
		retainedNames := make(map[string]bool)
		for _, svc := range a.reconciler.RetainedServices() {
			retainedNames[svc.Name] = true
		}
		for name, inst := range a.reconciler.Instances() {
			svc := inst.Config
			inSubnet := svc.Network != nil && ipNet.Contains(net.ParseIP(svc.Network.GuestIP))
			if !retainedNames[name] && !inSubnet {
				continue
			}
			retained[name] = svc
			if svc.Network == nil {
				continue
			}
			reservedIP[svc.Network.GuestIP] = name
			reservedMAC[strings.ToLower(svc.Network.GuestMAC)] = name
			// A VM outside the subnet still takes a MAC index.
			if !desired[name] {
				if inst.State == vm.StateRecoveryPending || deleteHolds[name].Recovery {
					quarantinedReserves = true
				} else {
					departingReserves = true
				}
			}
		}
	}
	// A guest IP that a removed VM's host rules, its record, or a route may
	// still reach is not given to another service until they are gone, or
	// that service would receive the removed one's traffic. A withdrawn
	// route is gone only once the process serving routes has restarted.
	departed := map[string]string{}
	var unconfirmed map[string]bool
	everyAddress, unconfirmedReserves := false, false
	if a.reconciler != nil {
		var routeHeld map[string]string
		routeHeld, unconfirmed, everyAddress = a.reconciler.RouteHeldGuestIPs()
		maps.Copy(departed, routeHeld)
		maps.Copy(departed, a.reconciler.PendingTeardownGuestIPs())
	}
	for ip := range departed {
		switch _, tracked := reservedIP[ip]; {
		case tracked || !ipNet.Contains(net.ParseIP(ip)):
			delete(departed, ip)
		case unconfirmed[ip]:
			unconfirmedReserves = true
		default:
			// Released once its rules and routes are gone, whether or not its
			// owner is still desired elsewhere: a wait, not invalid demand.
			departingReserves = true
		}
	}
	unconfirmedReserves = unconfirmedReserves || everyAddress
	exhausted := reconciler.Hold{Reason: addressExhaustedReason + a.cfg.VMSubnet, Admission: "guest_addresses_exhausted"}
	switch {
	case networked > guestAddressCount(ipNet):
	case quarantinedReserves && !departingReserves && !unconfirmedReserves:
		exhausted = reconciler.Hold{Reason: addressRecoveryReason, Recovery: true}
	case quarantinedReserves || departingReserves:
		exhausted = reconciler.Hold{Reason: addressTransientReason}
	case unconfirmedReserves:
		exhausted = reconciler.Hold{Reason: fmt.Sprintf(addressWithdrawalReason, a.cfg.TraefikUnit),
			Admission: reconciler.ActionGuestAddressWithdrawalUnconfirmed}
	}
	// Restore every existing assignment before allocating any new one, so a
	// new service that exhausts the subnet can never leave an existing VM's
	// desired config without its address and trigger a restart.
	for i := range services {
		svc := &services[i]
		if old, ok := retained[svc.Name]; ok && svc.Network != nil && old.Network != nil {
			svc.Network.GuestIP, svc.Network.GuestMAC = old.Network.GuestIP, old.Network.GuestMAC
			if !hasKernelArgPrefix(svc.KernelArgs, "ip=") {
				svc.KernelArgs = insertKernelArg(svc.KernelArgs, fmt.Sprintf("ip=%s::%s:%s::eth0:off", old.Network.GuestIP, gateway, netmask))
			}
		}
	}
	var held map[string]reconciler.Hold
	for i := range services {
		svc := &services[i]
		if svc.Network == nil {
			continue
		}
		if old, ok := retained[svc.Name]; ok && old.Network != nil {
			continue
		}
		// First fit from the start for each service: an address held back
		// for one service is skipped by the others, and must not be skipped
		// by its own owner because an earlier service passed it.
		idx := 0
		for {
			candidate, ok := guestIPv4ForIndex(ipNet, idx)
			if !ok {
				break
			}
			_, ipUsed := reservedIP[candidate]
			_, macUsed := reservedMAC[strings.ToLower(guestMACForIndex(idx))]
			owner, departing := departed[candidate]
			if departing && owner != svc.Name || everyAddress && !departing {
				ipUsed = true
			}
			if !ipUsed && !macUsed {
				break
			}
			idx++
		}
		guestIP, ok := guestIPv4ForIndex(ipNet, idx)
		if !ok {
			// Hold the service instead of reconciling it without an address.
			if held == nil {
				held = make(map[string]reconciler.Hold)
			}
			held[svc.Name] = exhausted
			a.logger.Warn("vm_subnet has no available guest IP for service", "subnet", a.cfg.VMSubnet, "service", svc.Name, "reason", exhausted.Reason)
			continue
		}
		mac := guestMACForIndex(idx)

		svc.Network.GuestIP = guestIP
		svc.Network.GuestMAC = mac
		reservedIP[guestIP], reservedMAC[strings.ToLower(mac)] = svc.Name, svc.Name

		// Add kernel IP autoconfig so the guest configures networking
		// before init runs (no fc-init changes needed).
		ipArg := fmt.Sprintf("ip=%s::%s:%s::eth0:off", guestIP, gateway, netmask)
		if !hasKernelArgPrefix(svc.KernelArgs, "ip=") {
			svc.KernelArgs = insertKernelArg(svc.KernelArgs, ipArg)
		}
	}
	return held
}

// guestAddressCount is the number of guest addresses guestIPv4ForIndex can
// produce: every host address except the network, the gateway (.1) and the
// broadcast address.
func guestAddressCount(ipNet *net.IPNet) int {
	if ipNet == nil || ipNet.IP.To4() == nil {
		return 0
	}
	ones, bits := ipNet.Mask.Size()
	if bits != 32 || ones > 30 {
		return 0
	}
	return 1<<uint(32-ones) - 3
}

func guestIPv4ForIndex(ipNet *net.IPNet, idx int) (string, bool) {
	if idx < 0 || ipNet == nil {
		return "", false
	}
	base := ipNet.IP.To4()
	if base == nil {
		return "", false
	}
	ones, bits := ipNet.Mask.Size()
	if bits != 32 || ones > 30 {
		return "", false
	}

	hostCount := uint64(1) << uint(32-ones)
	offset := uint64(idx) + 2 // reserve network + .1 gateway
	if offset >= hostCount-1 {
		return "", false
	}

	candidate := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(candidate, binary.BigEndian.Uint32(base)+uint32(offset))
	if !ipNet.Contains(candidate) {
		return "", false
	}
	return candidate.String(), true
}

func guestMACForIndex(idx int) string {
	n := uint32(idx + 1)
	return fmt.Sprintf("AA:FC:%02X:%02X:%02X:%02X",
		byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}

// resolveLinks iterates over each service's declared links and resolves them
// to concrete URLs using the target service's assigned guest IP. The resolved
// URL is injected into the service's Env map, which injectEnvVars later
// converts to kernel boot arguments for the guest.
//
// Must be called after assignNetworking (so guest IPs are set).
func (a *Agent) resolveLinks(services []config.ServiceConfig) {
	// Build a lookup map: service name → guest IP.
	ipByName := make(map[string]string, len(services))
	for _, svc := range services {
		if svc.Network != nil && svc.Network.GuestIP != "" {
			ipByName[svc.Name] = svc.Network.GuestIP
		}
	}

	for i := range services {
		svc := &services[i]
		if len(svc.Links) == 0 {
			continue
		}

		for _, link := range svc.Links {
			targetIP, ok := ipByName[link.Service]
			if !ok {
				a.logger.Warn("linked service not found or has no network",
					"service", svc.Name, "link_target", link.Service)
				continue
			}

			proto := link.Protocol
			if proto == "" {
				proto = "http"
			}
			url := fmt.Sprintf("%s://%s:%d", proto, targetIP, link.Port)

			if svc.Env == nil {
				svc.Env = make(map[string]string)
			}
			svc.Env[link.EnvVar] = url

			a.logger.Debug("resolved service link",
				"service", svc.Name, "target", link.Service,
				"env", link.EnvVar, "url", url)
		}
	}
}

// injectEnvVars appends environment variables to each service's KernelArgs.
// Values with whitespace are encoded as firework.env64.KEY=VALUE so they
// remain a single kernel-argument token; fc-init also supports legacy raw
// firework.env.KEY=VALUE entries for values that need no encoding.
func (a *Agent) injectEnvVars(services []config.ServiceConfig) {
	for i := range services {
		svc := &services[i]
		if len(svc.Env) == 0 {
			continue
		}

		// Sort keys for deterministic boot args.
		keys := make([]string, 0, len(svc.Env))
		for k := range svc.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, k := range keys {
			v := svc.Env[k]
			arg := envKernelArg(k, v)
			svc.KernelArgs = insertKernelArg(svc.KernelArgs, arg)
		}

		a.logger.Debug("injected env vars into kernel args",
			"service", svc.Name, "env_count", len(svc.Env))
	}
}

func envKernelArg(key, value string) string {
	if strings.ContainsAny(value, " \t\n\r\v\f") {
		encoded := base64.RawURLEncoding.EncodeToString([]byte(value))
		return fmt.Sprintf("firework.env64.%s=%s", key, encoded)
	}
	return fmt.Sprintf("firework.env.%s=%s", key, value)
}

// syncTraefikConfigs writes or removes Traefik dynamic config files for the
// desired local services, each from the configuration of the VM that owns its
// backend (see selectLocalRoutes); a service without a VM publishes no route.
// When the store implements NodeConfigLister,
// peer node configs are also fetched so that remote services can be proxied.
// No-op when Traefik management is disabled.
//
// Error semantics are split by fault domain. A returned error means the local
// routes could not be applied — the caller must not treat the revision as
// applied, so the next tick retries. Peer-side failures (the peer list cannot
// be fetched in full) never block local progress: local routes are still
// synced via SyncLocal, existing remote-* files are preserved as last-known-
// good (synchronizing against an incomplete peer set would delete valid
// routes), a warning is logged, and the per-tick route refresh retries the
// peer list automatically. The degraded state is exposed via the
// firework_agent_remote_route_sync_degraded gauge.
func (a *Agent) syncTraefikConfigs(ctx context.Context, services []config.ServiceConfig) error {
	if a.traefikMgr == nil {
		a.setStatusCondition("PeerRoutesReady", statusmodel.ConditionTrue, "not_configured", "")
		return nil
	}

	var remoteNodes []config.NodeConfig
	if lister, ok := a.store.(store.NodeConfigLister); ok {
		all, err := lister.ListAllNodeConfigs(ctx)
		if err != nil {
			a.logger.Warn("failed to list peer node configs; keeping last-known-good remote routes",
				"error", err)
			a.metrics.setRemoteRouteSyncDegraded(true)
			a.setStatusCondition("PeerRoutesReady", statusmodel.ConditionFalse, "peer_route_sync_degraded", err.Error())
			if err := a.syncRuntimeRoutes(services, a.lastPeerNodes, true); err != nil {
				return fmt.Errorf("syncing local traefik configs: %w", err)
			}
			return nil
		}
		a.metrics.setRemoteRouteSyncDegraded(false)
		a.setStatusCondition("PeerRoutesReady", statusmodel.ConditionTrue, "", "")
		ownNames := make(map[string]bool, len(a.cfg.NodeNames))
		for _, n := range a.cfg.NodeNames {
			ownNames[n] = true
		}
		for _, nc := range all {
			if !ownNames[nc.Node] {
				remoteNodes = append(remoteNodes, nc)
			}
		}
		a.lastPeerNodes, a.peersListed = remoteNodes, true
	} else {
		a.setStatusCondition("PeerRoutesReady", statusmodel.ConditionTrue, "not_configured", "")
	}

	if err := a.syncRuntimeRoutes(services, remoteNodes, false); err != nil {
		return fmt.Errorf("syncing traefik configs: %w", err)
	}
	return nil
}

// preflightRouting validates user-authored routing metadata for the local
// services before any reconciliation, so a missing ingress_domain or an invalid
// direct node config fails fast without advancing the revision. Exact
// metadata.host on an installation with Traefik management disabled is left as a
// historical no-op (no route is generated).
func (a *Agent) preflightRouting(services []config.ServiceConfig) error {
	for _, svc := range services {
		if _, hasSub := svc.Metadata[ingress.MetadataSubdomain]; hasSub && a.traefikMgr == nil {
			return fmt.Errorf("service %s sets metadata.subdomain but Traefik management is disabled (traefik_config_dir is empty)", svc.Name)
		}
		if a.traefikMgr == nil {
			continue
		}
		if _, err := ingress.Resolve(svc.Name, svc.Metadata, a.cfg.IngressDomain); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) readNodeCapacity() (capacity.NodeCapacity, bool) {
	cap, err := a.capacityReader.Read()
	if err == nil {
		a.lastKnownCapacity = cap
		a.hasLastKnownCapacity = true
		return cap, true
	}
	if a.hasLastKnownCapacity {
		a.logger.Debug("capacity read failed, using last-known-good value", "error", err)
		return a.lastKnownCapacity, true
	}
	a.logger.Debug("capacity check skipped", "error", err)
	return capacity.NodeCapacity{}, false
}

// sumResources returns the total vCPUs and memory requested by all services.
func sumResources(services []config.ServiceConfig) capacity.NodeCapacity {
	var total capacity.NodeCapacity
	for _, svc := range services {
		total.VCPUs += svc.VCPUs
		total.MemoryMB += svc.MemoryMB
	}
	return total
}

// stripCIDR removes the CIDR prefix length from an IP string.
func stripCIDR(s string) string {
	if idx := strings.LastIndex(s, "/"); idx != -1 {
		return s[:idx]
	}
	return s
}

// hasKernelArgPrefix checks if kernelArgs already has a token with the given
// prefix in the kernel-argument section (before the "--" app separator).
func hasKernelArgPrefix(kernelArgs, prefix string) bool {
	for _, tok := range strings.Fields(kernelArgs) {
		if tok == "--" {
			break
		}
		if strings.HasPrefix(tok, prefix) {
			return true
		}
	}
	return false
}

// insertKernelArg inserts a kernel argument before the optional "--" separator.
// If no separator exists, it appends to the end.
func insertKernelArg(kernelArgs, arg string) string {
	if kernelArgs == "" {
		return arg
	}

	parts := strings.Fields(kernelArgs)
	for i, part := range parts {
		if part != "--" {
			continue
		}

		updated := make([]string, 0, len(parts)+1)
		updated = append(updated, parts[:i]...)
		updated = append(updated, arg)
		updated = append(updated, parts[i:]...)
		return strings.Join(updated, " ")
	}

	return kernelArgs + " " + arg
}

// Status returns a summary of the agent's current state.
// Implements api.StatusProvider.
func (a *Agent) Status() map[string]any {
	snapshot := a.agentStatusSnapshot()
	data, _ := json.Marshal(snapshot)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	// Preserve the legacy field during the status schema transition.
	out["last_revision"] = snapshot.AppliedRevision
	return out
}

// MetricsText returns the Prometheus text exposition for agent runtime metrics.
func (a *Agent) MetricsText() string {
	return a.metrics.render()
}

func (a *Agent) refreshRuntimeMetrics() {
	a.finishTickStatus()
	// Pool gauges are published from retained state on every tick, not from
	// the admission path, so a node holding retained-but-unplaced volumes
	// keeps reporting them.
	a.volumeManager.ObservePool()
	results := make(map[string]healthcheck.Result)
	if a.healthMon != nil {
		results = a.healthMon.Results()
	}
	a.metrics.setServiceSnapshot(a.vmManager.List(), results)
	a.metrics.setAgentStatusSnapshot(a.agentStatusSnapshot())
}

func (a *Agent) syncRegistry(ctx context.Context, cap, used capacity.NodeCapacity) {
	if a.registryClient == nil {
		return
	}
	status := a.agentStatusSnapshot()
	a.registryClient.sync(ctx, a.cfg.NodeID, a.cfg.SchedulingLabels(), cap, used, &status)
	a.metrics.setRegistryFenced(a.registryClient.isFenced())
}

func (a *Agent) syncRegistryAfterTick(ctx context.Context, cap, used capacity.NodeCapacity) {
	a.finishTickStatus()
	a.syncRegistry(ctx, cap, used)
}

// healthAdapter wraps healthcheck.Monitor to satisfy api.HealthResultsProvider.
type healthAdapter struct {
	mon *healthcheck.Monitor
}

func (h *healthAdapter) Results() map[string]any {
	results := h.mon.Results()
	out := make(map[string]any, len(results))
	for k, v := range results {
		out[k] = map[string]any{
			"status":       v.Status,
			"last_checked": v.LastChecked,
			"failures":     v.Failures,
			"last_error":   statusmodel.BoundedMessage(v.LastError),
		}
	}
	return out
}

// syncImages commits its revision only after a complete successful sync. Recovery
// retries for an unchanged desired revision do not repeat object-store downloads.
// Multi-label stores have no combined revision, so they continue to sync.
func (a *Agent) syncImages(ctx context.Context, services []config.ServiceConfig, revision string) error {
	if revision != "" && revision == a.lastImageRevision {
		return nil
	}
	// A failed sync may already have replaced some artifacts. Invalidate the
	// previous revision too, so rolling back cannot reuse a partial download.
	a.lastImageRevision = ""
	if err := a.imageSyncer.Sync(ctx, services); err != nil {
		return err
	}
	a.lastImageRevision = revision
	return nil
}

const (
	capacityExceededReason  = "desired services exceed node capacity"
	capacityTransientReason = "capacity is still used by VMs that are starting, stopping, shrinking or being removed"
	capacityBlockedReason   = "capacity is used by VMs no pending change frees; add headroom or stage the change"
	capacityOwnershipReason = "capacity is kept by VMs whose ownership is unresolved"
)

// admit admits the desired services against node capacity and returns what
// the node uses, counting the growth it admitted, with the capacity holds and
// the size each admitted service may occupy. Address holds from
// prepareDesired are given as well, because they can hold other services
// through the claims of held VMs. It installs nothing; the caller installs
// the holds and the allowances, against which the reconciler checks every
// start again when it happens.
//
// A capacity hold is never a recovery hold: whether a capacity wait outlasts
// recovery depends on admission order, claims and how each quarantine
// resolves, so recovery cannot vouch for it. A recovery hold on a service
// that needs capacity is joined by a capacity hold, so its wait is reported
// as one.
func (a *Agent) admit(services []config.ServiceConfig, cap capacity.NodeCapacity, known bool, addressHolds map[string]reconciler.Hold) (capacity.NodeCapacity, map[string]reconciler.Hold, map[string]capacity.NodeCapacity) {
	instances := a.reconciler.Instances()
	return a.admitIn(services, cap, known, instances, addressHolds, hasRecoveryPending(instances))
}

func hasRecoveryPending(instances map[string]*vm.Instance) bool {
	for _, inst := range instances {
		if inst.State == vm.StateRecoveryPending {
			return true
		}
	}
	return false
}

// recoveryOnly reports whether resolving recovery is all the remaining work
// needs as far as capacity goes: no desired service needs capacity its VM
// does not already use. A desired quarantined VM whose configuration is
// unknown may need any amount, so at most one is allowed, and only if its
// whole desired size fits beside what the node uses with undesired
// quarantined VMs gone; it then fits whether it is adopted at whatever size
// it has or recreated. No capacity wait can outlast recovery then.
func recoveryOnly(services []config.ServiceConfig, cap capacity.NodeCapacity, instances map[string]*vm.Instance) bool {
	desired := make(map[string]config.ServiceConfig, len(services))
	for _, svc := range services {
		desired[svc.Name] = svc
	}
	var used, unknown capacity.NodeCapacity
	unknowns := 0
	for name, inst := range instances {
		svc, wanted := desired[name]
		switch {
		case !reconciler.UsesCapacity(inst.State):
			continue
		case inst.State == vm.StateRecoveryPending && !wanted:
			continue
		case inst.State == vm.StateRecoveryPending && inst.RecoveryConfigUnknown:
			unknowns++
			unknown = capacity.NodeCapacity{VCPUs: unknown.VCPUs + svc.VCPUs, MemoryMB: unknown.MemoryMB + svc.MemoryMB}
			continue
		}
		used = capacity.NodeCapacity{VCPUs: used.VCPUs + inst.Config.VCPUs, MemoryMB: used.MemoryMB + inst.Config.MemoryMB}
	}
	for _, svc := range services {
		inst := instances[svc.Name]
		switch {
		case inst != nil && inst.State == vm.StateRecoveryPending && inst.RecoveryConfigUnknown:
		case inst == nil || !reconciler.UsesCapacity(inst.State):
			if svc.VCPUs > 0 || svc.MemoryMB > 0 {
				return false
			}
		case svc.VCPUs > inst.Config.VCPUs || svc.MemoryMB > inst.Config.MemoryMB:
			return false
		}
	}
	if unknowns > 1 {
		return false
	}
	return (unknown.VCPUs == 0 || used.VCPUs+unknown.VCPUs <= cap.VCPUs) && (unknown.MemoryMB == 0 || used.MemoryMB+unknown.MemoryMB <= cap.MemoryMB)
}

// admitIn admits against one inventory snapshot, so no pass sees a VM change
// state that another did not. Admission and holds depend on each other: a
// capacity-held VM owns its claims, which can hold other services, which then
// are not admitted. They are computed together, starting every tick from no
// capacity holds. Within a tick a capacity hold is never released: a later
// pass keeps it and admits the rest around it. The held names therefore only
// grow, admission ends by construction, and its last pass counted the holds
// the tick installs.
func (a *Agent) admitIn(services []config.ServiceConfig, cap capacity.NodeCapacity, known bool, instances map[string]*vm.Instance, addressHolds map[string]reconciler.Hold, pending bool) (capacity.NodeCapacity, map[string]reconciler.Hold, map[string]capacity.NodeCapacity) {
	desired := config.NodeConfig{Services: services}
	deleteHeld := reconciler.DeleteHoldsIn(instances, desired)
	capacityHolds := map[string]reconciler.Hold{}
	pass := func() (capacity.NodeCapacity, map[string]reconciler.Hold, map[string]capacity.NodeCapacity) {
		held := reconciler.HoldsIn(instances, desired, mergeHolds(addressHolds, capacityHolds))
		return admitPass(services, cap, known, instances, held, deleteHeld, capacityHolds, pending)
	}
	// A pass keeps every hold it is given, so a longer result means new names.
	used, next, allowance := pass()
	for len(next) > len(capacityHolds) {
		capacityHolds = next
		used, next, allowance = pass()
	}
	return used, next, allowance
}

// mergeHolds combines address and capacity holds. A capacity hold does not
// replace an address hold that recovery does not explain.
func mergeHolds(addressHolds, capacityHolds map[string]reconciler.Hold) map[string]reconciler.Hold {
	holds := make(map[string]reconciler.Hold, len(addressHolds)+len(capacityHolds))
	maps.Copy(holds, addressHolds)
	for name, hold := range capacityHolds {
		if existing, ok := holds[name]; ok && !existing.Recovery {
			continue
		}
		holds[name] = hold
	}
	return holds
}

// admitPass decides which desired services may change this tick without
// taking capacity that is not free now. Every VM that still uses its
// resources counts at its current size until a change to it succeeds, whatever
// the plan does to it: a VM the plan removes or shrinks, a starting, stopping
// or quarantined VM, and a VM held for any reason. A failed or stopped VM uses
// nothing. In service-name order, each create or update is admitted by what it
// adds to a resource; one that adds nothing, such as a shrink, always
// proceeds. A shrink or removal therefore frees capacity for the next tick,
// and a service held for another reason reserves nothing. The reconciler
// checks every start again against the runtime as it is then, so a hold that
// clears during the apply, or a VM that changes state after admission, takes
// only capacity that is free at that moment.
//
// A service that does not fit is held, and so is every service in
// capacityHeld. The reason names the cause: desired state that does not fit
// at all; capacity that this tick's removals and shrinks, or a VM starting or
// stopping, frees for a later tick; capacity that quarantined VMs keep, when
// everything would fit once they and the work recovery holds resolved; or
// capacity nothing pending frees, such as two VMs trading sizes. The first is
// the same failure whether or not anything happens to be retained. None is a
// recovery hold. A service held for another reason is held for capacity too
// when desired state cannot fit, and a service held for recovery while
// pending when it needs capacity, so that its other hold cannot hide the
// wait.
func admitPass(services []config.ServiceConfig, cap capacity.NodeCapacity, known bool, instances map[string]*vm.Instance, held, deleteHeld, capacityHeld map[string]reconciler.Hold, pending bool) (capacity.NodeCapacity, map[string]reconciler.Hold, map[string]capacity.NodeCapacity) {
	type candidate struct {
		svc    config.ServiceConfig
		growth capacity.NodeCapacity
		// recovery marks a service held for unresolved VM ownership.
		recovery bool
	}
	plus := func(a, b capacity.NodeCapacity) capacity.NodeCapacity {
		return capacity.NodeCapacity{VCPUs: a.VCPUs + b.VCPUs, MemoryMB: a.MemoryMB + b.MemoryMB}
	}
	size := func(svc config.ServiceConfig) capacity.NodeCapacity {
		return capacity.NodeCapacity{VCPUs: svc.VCPUs, MemoryMB: svc.MemoryMB}
	}
	uses := func(inst *vm.Instance) capacity.NodeCapacity {
		if inst == nil || !reconciler.UsesCapacity(inst.State) {
			return capacity.NodeCapacity{}
		}
		return size(inst.Config)
	}
	exceeds := func(c capacity.NodeCapacity) bool { return c.VCPUs > cap.VCPUs || c.MemoryMB > cap.MemoryMB }
	// fits checks only the resources a change adds to, so a node already over
	// capacity in one resource still admits growth of the other.
	fits := func(used, growth capacity.NodeCapacity) bool {
		return (growth.VCPUs == 0 || used.VCPUs+growth.VCPUs <= cap.VCPUs) && (growth.MemoryMB == 0 || used.MemoryMB+growth.MemoryMB <= cap.MemoryMB)
	}
	heldOtherwise := func(name string) (reconciler.Hold, bool) {
		hold, ok := held[name]
		return hold, ok && hold.Reason != "" && !hold.Capacity
	}
	byName := make(map[string]bool, len(services))
	for _, svc := range services {
		byName[svc.Name] = true
	}
	// used is what the node uses now plus the growth admitted so far. freeing
	// records a removal or shrink this tick, or a VM starting or stopping,
	// after which more can fit without any recovery resolving. settled, which
	// only names a cause, is the node once quarantined VMs and the work
	// recovery holds resolved, and every other service applied.
	var used, settled capacity.NodeCapacity
	freeing := false
	for _, name := range slices.Sorted(maps.Keys(instances)) {
		inst := instances[name]
		used = plus(used, uses(inst))
		if byName[name] || inst.State == vm.StateRecoveryPending {
			continue
		}
		if hold, removalHeld := deleteHeld[name]; !(removalHeld && hold.Recovery) {
			settled = plus(settled, uses(inst))
		}
		if hold, removalHeld := deleteHeld[name]; reconciler.UsesCapacity(inst.State) && !(removalHeld && hold.Recovery) {
			freeing = true // It is removed this tick, or once its lifecycle or its owner's settles.
		}
	}
	var candidates, keepHeld, heldElsewhere []candidate
	allowance := make(map[string]capacity.NodeCapacity, len(services))
	for _, svc := range services {
		inst := instances[svc.Name]
		current := uses(inst)
		growth := capacity.NodeCapacity{VCPUs: max(svc.VCPUs-current.VCPUs, 0), MemoryMB: max(svc.MemoryMB-current.MemoryMB, 0)}
		hold, other := heldOtherwise(svc.Name)
		c := candidate{svc: svc, growth: growth, recovery: other && hold.Recovery}
		if c.recovery || inst != nil && inst.State == vm.StateRecoveryPending {
			settled = plus(settled, size(svc))
		} else {
			settled = plus(settled, plus(current, growth))
		}
		if inst != nil && reconciler.RetainsRuntime(inst.State) && inst.State != vm.StateRecoveryPending {
			freeing = true
		}
		_, heldForCapacity := capacityHeld[svc.Name]
		switch {
		case heldForCapacity:
			keepHeld = append(keepHeld, c) // The hold keeps it as it is until the tick ends.
		case other:
			// Its hold keeps it as it is. Should the hold clear during the
			// apply, the reconciler checks its start against what is free then.
			heldElsewhere = append(heldElsewhere, c)
		case growth == capacity.NodeCapacity{}:
			allowance[svc.Name] = size(svc)
			if inst != nil && inst.State == vm.StateRunning && (svc.VCPUs < current.VCPUs || svc.MemoryMB < current.MemoryMB) {
				freeing = true
			}
		default:
			candidates = append(candidates, c)
		}
	}
	if !known {
		return used, map[string]reconciler.Hold{}, nil
	}
	waiting := keepHeld
	for _, c := range candidates {
		if !fits(used, c.growth) {
			waiting = append(waiting, c)
			continue
		}
		used = plus(used, c.growth)
		allowance[c.svc.Name] = size(c.svc)
		if inst := instances[c.svc.Name]; inst != nil && inst.State == vm.StateRunning && (c.svc.VCPUs < inst.Config.VCPUs || c.svc.MemoryMB < inst.Config.MemoryMB) {
			freeing = true // It frees one resource while it takes another.
		}
	}
	exceeded := exceeds(sumResources(services))
	for _, c := range heldElsewhere {
		// Desired state that cannot fit fails whatever else holds a service,
		// and a capacity wait is never left to a recovery hold to report.
		if c.growth != (capacity.NodeCapacity{}) && !fits(used, c.growth) && (exceeded || pending && c.recovery) {
			waiting = append(waiting, c)
		}
	}
	var cause reconciler.Hold
	switch {
	case exceeded:
		cause = reconciler.Hold{Reason: capacityExceededReason, Admission: "capacity_exceeded", Capacity: true}
	case freeing:
		cause = reconciler.Hold{Reason: capacityTransientReason, Capacity: true}
	case pending && !exceeds(settled):
		cause = reconciler.Hold{Reason: capacityOwnershipReason, Capacity: true}
	default:
		cause = reconciler.Hold{Reason: capacityBlockedReason, Capacity: true}
	}
	holds := make(map[string]reconciler.Hold, len(waiting))
	for _, c := range waiting {
		holds[c.svc.Name] = cause
	}
	return used, holds, allowance
}

// admissionFailure reports holds that no amount of waiting resolves: desired
// services that exceed node capacity, or a subnet without a free address.
// They fail the revision instead of leaving it reconciling.
func admissionFailure(addressHolds, capacityHolds map[string]reconciler.Hold) (string, string) {
	for _, holds := range []map[string]reconciler.Hold{capacityHolds, addressHolds} {
		var names []string
		var code, reason string
		for name, hold := range holds {
			if hold.Admission != "" {
				names = append(names, name)
				code, reason = hold.Admission, hold.Reason
			}
		}
		if len(names) > 0 {
			sort.Strings(names)
			return code, reason + "; held: " + strings.Join(names, ", ")
		}
	}
	return "", ""
}

// prepareDesired completes desired state for this node: guest addresses,
// same-node link URLs, then environment kernel arguments, which carry the
// link URLs. It returns the services held because no guest address is free.
func (a *Agent) prepareDesired(services []config.ServiceConfig) map[string]reconciler.Hold {
	held := a.assignNetworking(services)
	a.resolveLinks(services)
	a.injectEnvVars(services)
	return held
}

func (a *Agent) restartService(ctx context.Context, name string) error {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	inst := a.reconciler.Instances()[name]
	if inst == nil {
		// An earlier restart's failed Start may have dropped the VM.
		a.restartNeedsReconcile.Store(true)
		return nil
	}
	// A failed or stopped VM takes its resources again, so it waits like a
	// create for capacity other VMs do not use or were not admitted to.
	if hold := a.reconciler.CapacityHold(inst.Config); hold.Reason != "" {
		a.restartNeedsReconcile.Store(true)
		return fmt.Errorf("restart %s waits for capacity: %s", name, hold.Reason)
	}
	// A rule that could deliver another service's traffic to this address
	// holds the restart, so a refused restart leaves the VM as it is.
	if hold := a.reconciler.StartHold(inst.Config); hold.Reason != "" {
		a.restartNeedsReconcile.Store(true)
		return fmt.Errorf("restart %s waits: %s", name, hold.Reason)
	}
	a.metrics.recordServiceRestart(name, tenantForService(inst.Config))
	a.recordRestart(name)
	if err := a.reconciler.Restart(ctx, inst.Config); err != nil {
		a.restartNeedsReconcile.Store(true)
		return err
	}
	return nil
}
