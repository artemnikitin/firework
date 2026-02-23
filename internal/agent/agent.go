package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/artemnikitin/firework/internal/api"
	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/healthcheck"
	"github.com/artemnikitin/firework/internal/imagesync"
	"github.com/artemnikitin/firework/internal/network"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/store"
	"github.com/artemnikitin/firework/internal/traefik"
	"github.com/artemnikitin/firework/internal/vm"
)

// Agent is the core component that runs on each node. It periodically pulls
// the desired state from the config store and reconciles it with the actual
// state of running Firecracker microVMs.
type Agent struct {
	cfg            config.AgentConfig
	store          store.Store
	vmManager      *vm.Manager
	reconciler     *reconciler.Reconciler
	healthMon      *healthcheck.Monitor
	networkMgr     *network.Manager
	imageSyncer    *imagesync.Syncer
	apiServer      *api.Server
	logger         *slog.Logger
	metrics        *runtimeMetrics
	capacityReader capacity.Reader
	traefikMgr     *traefik.Manager

	lastRevision string
}

// New creates a new Agent with all its dependencies.
func New(cfg config.AgentConfig, s store.Store, logger *slog.Logger) *Agent {
	vmMgr := vm.NewManager(cfg.FirecrackerBin, cfg.StateDir, logger)
	metrics := newRuntimeMetrics(cfg.NodeName)

	// Set up optional health check monitor.
	var healthMon *healthcheck.Monitor
	if cfg.EnableHealthChecks == nil || *cfg.EnableHealthChecks {
		restartFn := func(ctx context.Context, name string) error {
			inst := vmMgr.List()[name]
			if inst == nil {
				return nil
			}
			metrics.recordServiceRestart(name, tenantForService(inst.Config))
			if err := vmMgr.Stop(name); err != nil {
				logger.Warn("failed to stop service during health restart", "service", name, "error", err)
			}
			return vmMgr.Start(ctx, inst.Config)
		}
		healthMon = healthcheck.NewMonitor(logger, restartFn)
	}

	// Set up optional network manager.
	var networkMgr *network.Manager
	if cfg.EnableNetworkSetup == nil || *cfg.EnableNetworkSetup {
		networkMgr = network.NewManager(logger)
	}

	rec := reconciler.New(vmMgr, logger, healthMon, networkMgr, cfg.UpdateStrategy, cfg.UpdateDelay)

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
	var imgSyncer *imagesync.Syncer
	if cfg.S3ImagesBucket != "" {
		var err error
		imgSyncer, err = imagesync.NewSyncer(
			context.Background(),
			cfg.S3ImagesBucket,
			cfg.ImagesDir,
			cfg.S3Region,
			cfg.S3EndpointURL,
			logger,
		)
		if err != nil {
			logger.Error("failed to create image syncer", "error", err)
		}
	}

	// Set up optional capacity reader.
	var capReader capacity.Reader
	if cfg.EnableCapacityCheck == nil || *cfg.EnableCapacityCheck {
		capReader = capacity.NewOSReader()
	}

	// Set up optional Traefik config manager.
	var traefikMgr *traefik.Manager
	if cfg.TraefikConfigDir != "" {
		traefikMgr = traefik.NewManager(cfg.TraefikConfigDir)
	}

	a := &Agent{
		cfg:            cfg,
		store:          s,
		vmManager:      vmMgr,
		reconciler:     rec,
		healthMon:      healthMon,
		networkMgr:     networkMgr,
		imageSyncer:    imgSyncer,
		logger:         logger,
		metrics:        metrics,
		capacityReader: capReader,
		traefikMgr:     traefikMgr,
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
		"store", a.cfg.StoreURL,
		"poll_interval", a.cfg.PollInterval,
	)

	// Start the API server if configured.
	if a.apiServer != nil {
		if err := a.apiServer.Start(); err != nil {
			return err
		}
	}

	// Run an initial reconciliation immediately.
	a.tick(ctx)

	ticker := time.NewTicker(a.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			a.logger.Info("agent shutting down")
			a.shutdown()
			return ctx.Err()
		case <-ticker.C:
			a.tick(ctx)
		}
	}
}

// shutdown cleans up all resources.
func (a *Agent) shutdown() {
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

	// Always publish node capacity so the scheduler can discover this node
	// in CloudWatch even before a config has been assigned. When services are
	// loaded below, checkCapacity will overwrite the used values with actuals.
	if a.capacityReader != nil {
		if cap, err := a.capacityReader.Read(); err == nil {
			a.metrics.setCapacity(cap, capacity.NodeCapacity{})
		}
	}

	// Fetch and merge configs from all node labels.
	merged := a.fetchAndMerge(ctx)
	if merged == nil {
		// No local config, but still sync Traefik with remote nodes so that
		// this node can proxy requests for services placed on peer nodes.
		a.syncTraefikConfigs(ctx, nil)
		return
	}

	// Check revision only after fetch, so stores that update revision state
	// during Fetch (Git pull, S3 ETag) are evaluated against fresh data.
	// For multi-label nodes we skip this optimization because revision is
	// store-scoped, not label-scoped.
	var rev string
	if len(a.cfg.NodeNames) == 1 {
		var err error
		rev, err = a.store.Revision(ctx)
		if err != nil {
			a.logger.Error("failed to get store revision", "error", err)
		} else if rev != "" && rev == a.lastRevision {
			a.logger.Debug("config unchanged, skipping reconciliation", "revision", rev)
			a.refreshRuntimeMetrics()
			return
		}
	}

	// Assign networking (IPs, MACs, kernel args) to services that need it.
	a.assignNetworking(merged.Services)

	// Resolve service links: look up each linked service's guest IP and
	// inject the composed URL into the dependent service's Env map.
	a.resolveLinks(merged.Services)

	// Inject environment variables into kernel boot args so fc-init can
	// parse them from /proc/cmdline and export them inside the guest.
	a.injectEnvVars(merged.Services)

	// Check node capacity before reconciling; skip if resources are exceeded.
	if !a.checkCapacity(merged.Services) {
		return
	}

	// Sync images from S3 before reconciling (ensures rootfs/kernels are present).
	if a.imageSyncer != nil {
		syncStart := time.Now()
		err := a.imageSyncer.Sync(ctx, merged.Services)
		a.metrics.observeImageSync(time.Since(syncStart), err != nil)
		if err != nil {
			a.logger.Error("image sync failed", "error", err)
			return
		}
	}

	// Reconcile desired vs actual state.
	reconcileStart := time.Now()
	err := a.reconciler.Reconcile(ctx, *merged)
	a.metrics.observeReconcile(time.Since(reconcileStart), err != nil)
	if err != nil {
		a.logger.Error("reconciliation failed", "error", err)
		return
	}

	// Sync Traefik dynamic config files with desired services.
	a.syncTraefikConfigs(ctx, merged.Services)

	// Update the last known revision on success.
	if rev != "" {
		a.lastRevision = rev
	}
	appliedRevision := rev
	if appliedRevision == "" {
		appliedRevision = a.lastRevision
	}
	a.metrics.recordConfigApply(appliedRevision, time.Now())
	a.refreshRuntimeMetrics()

	a.logger.Debug("reconciliation tick completed", "revision", rev)
}

// fetchAndMerge fetches configs for all node labels and merges services.
// Returns nil if all fetches fail.
func (a *Agent) fetchAndMerge(ctx context.Context) *config.NodeConfig {
	seen := make(map[string]config.ServiceConfig)
	var fetchedAny bool

	for _, name := range a.cfg.NodeNames {
		data, err := a.store.Fetch(ctx, name)
		if err != nil {
			a.logger.Error("failed to fetch config from store", "label", name, "error", err)
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
			continue
		}

		fetchedAny = true
		for _, svc := range nc.Services {
			if _, dup := seen[svc.Name]; dup {
				a.logger.Warn("duplicate service across labels, last wins",
					"service", svc.Name, "label", name)
			}
			seen[svc.Name] = svc
		}
	}

	if !fetchedAny {
		a.logger.Error("all config fetches failed")
		return nil
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
		Node:     a.cfg.NodeName,
		Services: services,
	}
}

// assignNetworking fills in IP, MAC, and kernel IP boot args for services
// that have networking enabled. Services must be sorted by name for
// deterministic IP allocation.
func (a *Agent) assignNetworking(services []config.ServiceConfig) {
	if a.cfg.VMSubnet == "" {
		return
	}

	gateway := stripCIDR(a.cfg.VMGateway)
	netmask := network.SubnetMaskBits(a.cfg.VMSubnet)

	// Parse subnet to compute sequential IPs starting at .2.
	ip, _, err := net.ParseCIDR(a.cfg.VMSubnet)
	if err != nil {
		a.logger.Error("invalid vm_subnet", "subnet", a.cfg.VMSubnet, "error", err)
		return
	}
	ip = ip.To4()
	if ip == nil {
		a.logger.Error("vm_subnet is not IPv4", "subnet", a.cfg.VMSubnet)
		return
	}

	// Start at .2 (gateway is .1).
	nextIP := make(net.IP, 4)
	copy(nextIP, ip)
	nextIP[3] = 2

	idx := 0
	for i := range services {
		svc := &services[i]
		if svc.Network == nil {
			continue
		}

		guestIP := nextIP.String()
		mac := fmt.Sprintf("AA:FC:00:00:00:%02X", idx+1)

		svc.Network.GuestIP = guestIP
		svc.Network.GuestMAC = mac

		// Add kernel IP autoconfig so the guest configures networking
		// before init runs (no fc-init changes needed).
		ipArg := fmt.Sprintf("ip=%s::%s:%s::eth0:off", guestIP, gateway, netmask)
		if !hasKernelArgPrefix(svc.KernelArgs, "ip=") {
			svc.KernelArgs = insertKernelArg(svc.KernelArgs, ipArg)
		}

		idx++
		nextIP[3]++
	}
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

// injectEnvVars appends environment variables to each service's KernelArgs
// as firework.env.KEY=VALUE entries. The guest's /sbin/fc-init parses
// /proc/cmdline for these entries and exports them before launching the app.
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
			arg := fmt.Sprintf("firework.env.%s=%s", k, v)
			svc.KernelArgs = insertKernelArg(svc.KernelArgs, arg)
		}

		a.logger.Debug("injected env vars into kernel args",
			"service", svc.Name, "env_count", len(svc.Env))
	}
}

// syncTraefikConfigs writes or removes Traefik dynamic config files to match
// the desired set of services. When the store implements NodeConfigLister,
// peer node configs are also fetched so that remote services can be proxied.
// No-op when Traefik management is disabled.
func (a *Agent) syncTraefikConfigs(ctx context.Context, services []config.ServiceConfig) {
	if a.traefikMgr == nil {
		return
	}

	var remoteNodes []config.NodeConfig
	if lister, ok := a.store.(store.NodeConfigLister); ok {
		all, err := lister.ListAllNodeConfigs(ctx)
		if err != nil {
			a.logger.Warn("failed to list peer node configs, remote routing skipped", "error", err)
		} else {
			ownNames := make(map[string]bool, len(a.cfg.NodeNames))
			for _, n := range a.cfg.NodeNames {
				ownNames[n] = true
			}
			for _, nc := range all {
				if !ownNames[nc.Node] {
					remoteNodes = append(remoteNodes, nc)
				}
			}
		}
	}

	if err := a.traefikMgr.Sync(services, remoteNodes); err != nil {
		a.logger.Warn("failed to sync traefik configs", "error", err)
	}
}

// checkCapacity reads node capacity and compares it against the desired
// services. Returns false (skip reconcile) if resources would be exceeded.
func (a *Agent) checkCapacity(services []config.ServiceConfig) bool {
	if a.capacityReader == nil {
		return true
	}

	cap, err := a.capacityReader.Read()
	if err != nil {
		a.logger.Debug("capacity check skipped", "error", err)
		return true
	}

	used := sumResources(services)
	a.metrics.setCapacity(cap, used)

	if used.VCPUs > cap.VCPUs || used.MemoryMB > cap.MemoryMB {
		a.logger.Warn("desired services exceed node capacity, skipping reconciliation",
			"cap_vcpus", cap.VCPUs, "used_vcpus", used.VCPUs,
			"cap_memory_mb", cap.MemoryMB, "used_memory_mb", used.MemoryMB,
		)
		return false
	}

	return true
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
	instances := a.vmManager.List()
	services := make([]map[string]any, 0, len(instances))

	for _, inst := range instances {
		svcInfo := map[string]any{
			"name":  inst.Name,
			"state": inst.State,
			"pid":   inst.PID,
		}

		// Attach health status if available.
		if a.healthMon != nil {
			if result, ok := a.healthMon.GetResult(inst.Name); ok {
				svcInfo["health"] = map[string]any{
					"status":       result.Status,
					"last_checked": result.LastChecked,
					"failures":     result.Failures,
					"last_error":   result.LastError,
				}
			}
		}

		services = append(services, svcInfo)
	}

	return map[string]any{
		"node":          a.cfg.NodeName,
		"last_revision": a.lastRevision,
		"services":      services,
	}
}

// MetricsText returns the Prometheus text exposition for agent runtime metrics.
func (a *Agent) MetricsText() string {
	return a.metrics.render()
}

func (a *Agent) refreshRuntimeMetrics() {
	results := make(map[string]healthcheck.Result)
	if a.healthMon != nil {
		results = a.healthMon.Results()
	}
	a.metrics.setServiceSnapshot(a.vmManager.List(), results)
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
			"last_error":   v.LastError,
		}
	}
	return out
}
