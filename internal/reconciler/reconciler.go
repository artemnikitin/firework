package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/healthcheck"
	"github.com/artemnikitin/firework/internal/network"
	"github.com/artemnikitin/firework/internal/vm"
)

// Action represents a reconciliation action the agent needs to take.
type Action struct {
	Type            ActionType
	Service         config.ServiceConfig
	PreviousService *config.ServiceConfig
}

// ActionType describes the kind of reconciliation action.
type ActionType string

const (
	ActionCreate ActionType = "create"
	ActionUpdate ActionType = "update"
	ActionDelete ActionType = "delete"
)

// VMManager abstracts VM lifecycle operations used by the Reconciler.
type VMManager interface {
	List() map[string]*vm.Instance
	Start(context.Context, config.ServiceConfig) error
	Remove(string) error
}

type volumePreflighter interface {
	Preflight(context.Context, config.ServiceConfig) error
}

type vmRecoverer interface {
	Recover(context.Context, config.NodeConfig) ([]string, error)
}

// NetworkManager abstracts host networking so port-forward convergence can be
// exercised without touching real iptables rules.
type NetworkManager interface {
	Setup(config.ServiceConfig) error
	Teardown(config.ServiceConfig) error
	SetupPortForward(hostPort int, guestIP string, vmPort int) error
	TeardownPortForward(hostPort int, guestIP string, vmPort int) error
}

// Reconciler compares desired state from the config store with the actual
// state of running VMs and produces a plan to converge them.
type Reconciler struct {
	vmManager       VMManager
	healthMon       *healthcheck.Monitor
	networkMgr      NetworkManager
	logger          *slog.Logger
	updateStrategy  string
	updateDelay     time.Duration
	sleepFn         func(context.Context, time.Duration) error
	pendingRecovery map[string]struct{}
	// pendingTeardowns holds services whose VM was already removed but whose
	// host network/port-forward teardown failed, keyed by service name. Once
	// Remove succeeds the service leaves both desired and actual state, so
	// Plan produces no further delete action for it — retryPendingTeardowns
	// is the only thing that will ever finish (or keep retrying) that
	// cleanup, on this tick and every tick after until it succeeds. Persisted
	// to stateDir (when set) so a pure-delete's pending cleanup survives an
	// agent restart; see WithStateDir.
	pendingTeardowns map[string]config.ServiceConfig
	// stateDir, when non-empty, is where pendingTeardowns is persisted. Set
	// via WithStateDir; empty by default, which keeps pendingTeardowns
	// in-memory only (used throughout the test suite).
	stateDir string
}

// New creates a new Reconciler. The healthMon and networkMgr parameters are
// optional and may be nil.
func New(vmManager VMManager, logger *slog.Logger, healthMon *healthcheck.Monitor, networkMgr *network.Manager, updateStrategy string, updateDelay time.Duration) *Reconciler {
	// A nil *network.Manager stored in an interface field is not a nil
	// interface, so every `networkMgr != nil` guard below would pass and then
	// dereference nil. Keep the typed nil out of the interface.
	var netMgr NetworkManager
	if networkMgr != nil {
		netMgr = networkMgr
	}
	return NewWithNetworkManager(vmManager, logger, healthMon, netMgr, updateStrategy, updateDelay)
}

// NewWithNetworkManager builds a Reconciler over an arbitrary NetworkManager.
// Callers outside tests should use New; this exists so host networking can be
// faked when exercising the agent's tick paths.
func NewWithNetworkManager(vmManager VMManager, logger *slog.Logger, healthMon *healthcheck.Monitor, networkMgr NetworkManager, updateStrategy string, updateDelay time.Duration) *Reconciler {
	return &Reconciler{
		vmManager:        vmManager,
		healthMon:        healthMon,
		networkMgr:       networkMgr,
		logger:           logger,
		updateStrategy:   updateStrategy,
		updateDelay:      updateDelay,
		pendingRecovery:  make(map[string]struct{}),
		pendingTeardowns: make(map[string]config.ServiceConfig),
		sleepFn: func(ctx context.Context, d time.Duration) error {
			select {
			case <-time.After(d):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
}

// pendingTeardownsFile is the name, under stateDir, of the file that
// persists pendingTeardowns across an agent restart.
const pendingTeardownsFile = "pending_teardowns.json"

// WithStateDir enables persisting pendingTeardowns to
// <dir>/pending_teardowns.json, so a pure delete's host cleanup survives an
// agent restart between a failed teardown and its retry — without this, that
// entry only ever lived in memory and a restart silently forgot it, leaving
// the obsolete host rule with nothing left to notice it. Loads any existing
// file immediately; the first Reconcile call retries whatever it finds.
// Returns r so it can be chained onto New/NewWithNetworkManager; a Reconciler
// that never calls this behaves exactly as before, tracking pending
// teardowns in memory only (the default in tests).
func (r *Reconciler) WithStateDir(dir string) *Reconciler {
	r.stateDir = dir
	if dir == "" {
		return r
	}
	data, err := os.ReadFile(filepath.Join(dir, pendingTeardownsFile))
	if err != nil {
		if !os.IsNotExist(err) {
			r.logger.Warn("failed to load persisted pending teardowns", "error", err)
		}
		return r
	}
	var loaded map[string]config.ServiceConfig
	if err := json.Unmarshal(data, &loaded); err != nil {
		r.logger.Warn("failed to parse persisted pending teardowns", "error", err)
		return r
	}
	for name, svc := range loaded {
		r.pendingTeardowns[name] = svc
	}
	return r
}

// persistPendingTeardowns writes the current pendingTeardowns to stateDir, or
// removes the file once it is empty. A no-op when stateDir is unset.
func (r *Reconciler) persistPendingTeardowns() {
	if r.stateDir == "" {
		return
	}
	path := filepath.Join(r.stateDir, pendingTeardownsFile)
	if len(r.pendingTeardowns) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			r.logger.Warn("failed to remove empty pending teardowns file", "error", err)
		}
		return
	}
	data, err := json.Marshal(r.pendingTeardowns)
	if err != nil {
		r.logger.Warn("failed to marshal pending teardowns", "error", err)
		return
	}
	if err := os.MkdirAll(r.stateDir, 0o755); err != nil {
		r.logger.Warn("failed to create state dir for pending teardowns", "error", err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		r.logger.Warn("failed to persist pending teardowns", "error", err)
	}
}

// Plan computes the list of actions needed to reach the desired state.
func (r *Reconciler) Plan(desired config.NodeConfig) []Action {
	actual := r.vmManager.List()
	var actions []Action

	// Build a set of desired service names for quick lookup.
	desiredSet := make(map[string]config.ServiceConfig, len(desired.Services))
	for _, svc := range desired.Services {
		desiredSet[svc.Name] = svc
	}

	// Check for services that need to be created or updated.
	for _, svc := range desired.Services {
		inst, exists := actual[svc.Name]
		if !exists {
			actions = append(actions, Action{Type: ActionCreate, Service: svc})
			continue
		}
		if needsUpdate(inst, svc) {
			prev := inst.Config
			actions = append(actions, Action{
				Type:            ActionUpdate,
				Service:         svc,
				PreviousService: &prev,
			})
		}
	}

	// Check for services that need to be deleted (running but not in desired).
	// Use inst.Config so the delete handler has full config for teardown
	// (needed for network teardown and port forward cleanup).
	for name, inst := range actual {
		if _, desired := desiredSet[name]; !desired {
			actions = append(actions, Action{Type: ActionDelete, Service: inst.Config})
		}
	}

	return actions
}

// Apply executes the list of reconciliation actions.
// Uses rolling strategy if configured, otherwise applies all at once.
func (r *Reconciler) Apply(ctx context.Context, actions []Action) error {
	if r.updateStrategy == "rolling" {
		return r.applyRolling(ctx, actions)
	}
	return r.applyAllAtOnce(ctx, actions)
}

// applyAllAtOnce applies all actions without rolling-update delays.
func (r *Reconciler) applyAllAtOnce(ctx context.Context, actions []Action) error {
	var errs []error

	for _, action := range actions {
		switch action.Type {
		case ActionCreate:
			r.logger.Info("creating service", "service", action.Service.Name)
			if err := r.createService(ctx, action.Service); err != nil {
				r.logger.Error("failed to create service", "service", action.Service.Name, "error", err)
				errs = append(errs, fmt.Errorf("create %s: %w", action.Service.Name, err))
			}

		case ActionUpdate:
			r.logger.Info("updating service (stop + start)", "service", action.Service.Name)
			if err := r.preflight(ctx, action.Service); err != nil {
				r.logger.Error("volume preflight failed; keeping current VM running", "service", action.Service.Name, "error", err)
				errs = append(errs, stageError(FailureStageVM, fmt.Errorf("preflight update %s: %w", action.Service.Name, err)))
				continue
			}
			prev := action.Service
			if action.PreviousService != nil {
				prev = *action.PreviousService
			}
			if err := r.deleteService(prev); err != nil {
				r.logger.Error("failed to stop existing service during update", "service", action.Service.Name, "error", err)
				errs = append(errs, fmt.Errorf("remove previous %s: %w", action.Service.Name, err))
				continue
			}
			if err := r.createService(ctx, action.Service); err != nil {
				r.logger.Error("failed to start service during update", "service", action.Service.Name, "error", err)
				errs = append(errs, fmt.Errorf("update %s: %w", action.Service.Name, err))
			}

		case ActionDelete:
			r.logger.Info("deleting service", "service", action.Service.Name)
			if err := r.deleteService(action.Service); err != nil {
				errs = append(errs, fmt.Errorf("delete %s: %w", action.Service.Name, err))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("reconciliation had %d error(s): %w", len(errs), errors.Join(errs...))
	}
	return nil
}

// applyRolling applies deletes and creates in batch, then applies updates
// one at a time with an optional delay between each.
func (r *Reconciler) applyRolling(ctx context.Context, actions []Action) error {
	var errs []error

	// Apply all deletes first (no service depends on them).
	for _, action := range actions {
		if action.Type != ActionDelete {
			continue
		}
		r.logger.Info("deleting service", "service", action.Service.Name)
		if err := r.deleteService(action.Service); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", action.Service.Name, err))
		}
	}

	// Apply all creates (new services, no disruption to existing ones).
	for _, action := range actions {
		if action.Type != ActionCreate {
			continue
		}
		r.logger.Info("creating service", "service", action.Service.Name)
		if err := r.createService(ctx, action.Service); err != nil {
			r.logger.Error("failed to create service", "service", action.Service.Name, "error", err)
			errs = append(errs, fmt.Errorf("create %s: %w", action.Service.Name, err))
		}
	}

	// Apply updates one at a time with delay between each.
	var updates []Action
	for _, action := range actions {
		if action.Type == ActionUpdate {
			updates = append(updates, action)
		}
	}

	for i, action := range updates {
		r.logger.Info("updating service (stop + start)", "service", action.Service.Name)
		if err := r.preflight(ctx, action.Service); err != nil {
			r.logger.Error("volume preflight failed; keeping current VM running", "service", action.Service.Name, "error", err)
			errs = append(errs, stageError(FailureStageVM, fmt.Errorf("preflight update %s: %w", action.Service.Name, err)))
			break
		}
		prev := action.Service
		if action.PreviousService != nil {
			prev = *action.PreviousService
		}
		if err := r.deleteService(prev); err != nil {
			r.logger.Error("failed to stop existing service during update", "service", action.Service.Name, "error", err)
			errs = append(errs, fmt.Errorf("remove previous %s: %w", action.Service.Name, err))
			break
		}
		if err := r.createService(ctx, action.Service); err != nil {
			r.logger.Error("failed to start service during update", "service", action.Service.Name, "error", err)
			errs = append(errs, fmt.Errorf("update %s: %w", action.Service.Name, err))
			break
		}

		// Sleep between updates, but not after the last one.
		if i < len(updates)-1 && r.updateDelay > 0 {
			if err := r.sleepFn(ctx, r.updateDelay); err != nil {
				return stageError(FailureStageVM, fmt.Errorf("rolling update interrupted: %w", err))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("reconciliation had %d error(s): %w", len(errs), errors.Join(errs...))
	}
	return nil
}

func (r *Reconciler) preflight(ctx context.Context, svc config.ServiceConfig) error {
	if manager, ok := r.vmManager.(volumePreflighter); ok {
		return manager.Preflight(ctx, svc)
	}
	return nil
}

// Reconcile is a convenience method that plans and applies in one step.
func (r *Reconciler) Reconcile(ctx context.Context, desired config.NodeConfig) error {
	var errs []error
	if recoverer, ok := r.vmManager.(vmRecoverer); ok {
		adopted, err := recoverer.Recover(ctx, desired)
		for _, name := range adopted {
			r.pendingRecovery[name] = struct{}{}
		}
		if err != nil {
			errs = append(errs, stageError(FailureStageVM, fmt.Errorf("recover VMs: %w", err)))
		}
	}
	if err := r.restoreRecovered(ctx, desired); err != nil {
		errs = append(errs, err)
	}
	actions := r.Plan(desired)

	if len(actions) == 0 {
		r.logger.Debug("no changes needed, state is converged")
	} else {
		r.logger.Info("reconciliation plan",
			"creates", countActions(actions, ActionCreate),
			"updates", countActions(actions, ActionUpdate),
			"deletes", countActions(actions, ActionDelete),
		)

		if err := r.Apply(ctx, actions); err != nil {
			errs = append(errs, err)
		}
	}

	// Port forwards converge on every tick, not only when the plan has an
	// action. A failure while creating a service used to be logged and dropped,
	// and because an existing VM produces no further plan actions, nothing ever
	// retried it: the node reported converged while cross-node routing stayed
	// broken. Re-asserting the rules is cheap because the helper checks before
	// it adds, it self-heals a transient failure, and a persistent one keeps
	// surfacing instead of being reported once and lost.
	//
	// Deliberately not done by rolling the service back: a durable failure such
	// as a host-port collision would then repeatedly destroy a running VM.
	if err := r.syncPortForwards(desired); err != nil {
		errs = append(errs, err)
	}
	// Retry host cleanup for every service whose VM removal outran its
	// teardown, on this tick's fresh deletes and any left over from earlier
	// ticks alike. This must run after Plan/Apply above: Plan only diffs
	// desired against actual VMs, and a service that finished being removed
	// on this or an earlier tick is absent from both, so this is the only
	// remaining path that will ever finish tearing down its host rules.
	if err := r.retryPendingTeardowns(); err != nil {
		errs = append(errs, err)
	}
	return combineErrors(errs)
}

// SyncPortForwards re-asserts host DNAT for the desired services without
// planning or applying anything else. Reconcile calls it on ticks that do
// reconcile; the agent's unchanged-revision fast path calls it directly,
// because that path returns before Reconcile and is the common case in steady
// state — which is exactly when host rules drift under the agent.
func (r *Reconciler) SyncPortForwards(desired config.NodeConfig) error {
	return r.syncPortForwards(desired)
}

// syncPortForwards re-asserts the DNAT rules for every desired service that
// currently has a VM. It only adds rules for desired services; a rule left
// behind by a failed teardown of a service that is no longer desired is
// retried separately, by retryPendingTeardowns (see its doc comment for how
// that tracking survives, or does not survive, an agent restart).
func (r *Reconciler) syncPortForwards(desired config.NodeConfig) error {
	if r.networkMgr == nil {
		return nil
	}
	running := r.vmManager.List()
	var errs []error
	for _, svc := range desired.Services {
		if svc.Network == nil || len(svc.PortForwards) == 0 {
			continue
		}
		// A service still waiting to be created has no guest to forward to.
		if running[svc.Name] == nil {
			continue
		}
		for _, pf := range svc.PortForwards {
			if err := r.networkMgr.SetupPortForward(pf.HostPort, svc.Network.GuestIP, pf.VMPort); err != nil {
				errs = append(errs, stageError(FailureStageNetwork,
					fmt.Errorf("port forward %d -> %s:%d for %s: %w",
						pf.HostPort, svc.Network.GuestIP, pf.VMPort, svc.Name, err)))
			}
		}
	}
	return combineErrors(errs)
}

func (r *Reconciler) restoreRecovered(ctx context.Context, desired config.NodeConfig) error {
	if len(r.pendingRecovery) == 0 {
		return nil
	}
	desiredByName := make(map[string]config.ServiceConfig, len(desired.Services))
	for _, service := range desired.Services {
		desiredByName[service.Name] = service
	}
	actual := r.vmManager.List()
	var errs []error
	for name := range r.pendingRecovery {
		service, wanted := desiredByName[name]
		instance := actual[name]
		if !wanted || instance == nil || needsUpdate(instance, service) {
			delete(r.pendingRecovery, name)
			continue
		}
		if r.networkMgr != nil {
			if err := r.networkMgr.Setup(service); err != nil {
				errs = append(errs, stageError(FailureStageNetwork, fmt.Errorf("restore network for adopted %s: %w", name, err)))
				continue
			}
			if service.Network != nil {
				failed := false
				for _, forward := range service.PortForwards {
					if err := r.networkMgr.SetupPortForward(forward.HostPort, service.Network.GuestIP, forward.VMPort); err != nil {
						errs = append(errs, stageError(FailureStageNetwork, fmt.Errorf("restore port forward for adopted %s: %w", name, err)))
						failed = true
					}
				}
				if failed {
					continue
				}
			}
		}
		if r.healthMon != nil && service.HealthCheck != nil {
			r.healthMon.Register(ctx, service)
		}
		delete(r.pendingRecovery, name)
	}
	return combineErrors(errs)
}

func combineErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%d error(s): %w", len(errs), errors.Join(errs...))
}

// createService sets up networking, starts the VM, and registers health checks.
func (r *Reconciler) createService(ctx context.Context, svc config.ServiceConfig) error {
	// Set up network before starting the VM.
	if r.networkMgr != nil {
		if err := r.networkMgr.Setup(svc); err != nil {
			return stageError(FailureStageNetwork, fmt.Errorf("network setup: %w", err))
		}
	}

	// Start the VM.
	if err := r.vmManager.Start(ctx, svc); err != nil {
		// Roll back network on failure.
		if r.networkMgr != nil {
			_ = r.networkMgr.Teardown(svc)
		}
		return stageError(FailureStageVM, fmt.Errorf("starting VM: %w", err))
	}

	// Set up port forwards. A failure here is logged rather than failing the
	// create, because tearing a freshly started VM back down over a host-port
	// collision would crash-loop the service. syncPortForwards re-asserts these
	// rules later in the same tick and on every tick after, so the failure is
	// retried and surfaced there instead of being lost here.
	if r.networkMgr != nil && svc.Network != nil && len(svc.PortForwards) > 0 {
		for _, pf := range svc.PortForwards {
			if err := r.networkMgr.SetupPortForward(pf.HostPort, svc.Network.GuestIP, pf.VMPort); err != nil {
				r.logger.Warn("failed to setup port forward; will be retried by port-forward sync",
					"service", svc.Name, "host_port", pf.HostPort, "error", err)
			}
		}
	}

	// Register health check.
	if r.healthMon != nil && svc.HealthCheck != nil {
		r.healthMon.Register(ctx, svc)
	}

	return nil
}

// deleteService deregisters health checks, stops the VM, and tears down host
// networking (port forwards and the network device).
func (r *Reconciler) deleteService(svc config.ServiceConfig) error {
	// Prove ownership and stop the VM before removing host resources. If the
	// process identity is ambiguous, keep the service quarantined and intact.
	if err := r.vmManager.Remove(svc.Name); err != nil {
		return stageError(FailureStageVM, fmt.Errorf("remove VM: %w", err))
	}

	if r.healthMon != nil {
		r.healthMon.Deregister(svc.Name)
	}

	// A teardown failure here is not returned: the update path skips the
	// recreate when this function errors, and a transient teardown failure
	// must not leave a service deleted. The VM is already gone, though, so
	// this is the last tick that will ever see svc as a Plan delete action —
	// record it so retryPendingTeardowns keeps retrying the host cleanup on
	// every later tick until it actually succeeds, instead of the failure
	// being logged once and never revisited. At this exact point nothing is
	// running under svc.Name yet (Remove just succeeded, and any recreate
	// under an update happens after this call returns), so the full teardown
	// — network device included — is safe unconditionally; only
	// retryPendingTeardowns, which can run after that recreate has happened,
	// needs to be more careful about what it repeats.
	if err := r.teardownHostResources(svc); err != nil {
		r.logger.Warn("failed to tear down host resources; will retry", "service", svc.Name, "error", err)
		r.pendingTeardowns[svc.Name] = svc
		r.persistPendingTeardowns()
		return nil
	}
	delete(r.pendingTeardowns, svc.Name)
	r.persistPendingTeardowns()
	return nil
}

// teardownHostResources removes both the port-forward and network resources
// for a service that is not currently running under that name. Called from
// deleteService and from retryPendingTeardowns' not-running branch; every
// step must be safe to repeat, since either caller may be retrying an
// earlier failure: TeardownPortForward and Teardown both tolerate a rule or
// device that is already gone.
//
// Must not be called for a name that has a live VM again — see
// retryPendingTeardowns for why that case instead calls teardownPortForwards
// directly, with only the specific obsolete entries.
func (r *Reconciler) teardownHostResources(svc config.ServiceConfig) error {
	var errs []error
	if err := r.teardownPortForwards(svc, svc.PortForwards); err != nil {
		errs = append(errs, err)
	}
	if r.networkMgr != nil {
		if err := r.networkMgr.Teardown(svc); err != nil {
			errs = append(errs, stageError(FailureStageNetwork,
				fmt.Errorf("teardown network for %s: %w", svc.Name, err)))
		}
	}
	return combineErrors(errs)
}

// teardownPortForwards removes exactly the given port forwards for svc's
// guest IP. A no-op if svc has no network (the same guard teardownHostResources
// used to apply inline).
func (r *Reconciler) teardownPortForwards(svc config.ServiceConfig, pfs []config.PortForward) error {
	if r.networkMgr == nil || svc.Network == nil || len(pfs) == 0 {
		return nil
	}
	var errs []error
	for _, pf := range pfs {
		if err := r.networkMgr.TeardownPortForward(pf.HostPort, svc.Network.GuestIP, pf.VMPort); err != nil {
			errs = append(errs, stageError(FailureStageNetwork,
				fmt.Errorf("teardown port forward %d for %s: %w", pf.HostPort, svc.Name, err)))
		}
	}
	return combineErrors(errs)
}

// portForwardKey identifies a single DNAT rule the way TeardownPortForward
// and SetupPortForward actually key it: by (hostPort, guestIP, vmPort), not
// by service name. Two different service configs sharing a name across an
// update can still have entirely distinct rules under this key.
type portForwardKey struct {
	hostPort int
	guestIP  string
	vmPort   int
}

func portForwardKeys(svc config.ServiceConfig) map[portForwardKey]struct{} {
	keys := make(map[portForwardKey]struct{}, len(svc.PortForwards))
	if svc.Network == nil {
		return keys
	}
	for _, pf := range svc.PortForwards {
		keys[portForwardKey{pf.HostPort, svc.Network.GuestIP, pf.VMPort}] = struct{}{}
	}
	return keys
}

// obsoletePortForwards returns the entries of old.PortForwards whose
// (hostPort, guestIP, vmPort) key is not also claimed by current — the
// specific DNAT rules that describe a destination current no longer uses. A
// rule current still claims is excluded even though old also lists it: that
// rule now belongs to current, and tearing it down would break the live
// service, not clean up an obsolete one.
func obsoletePortForwards(old, current config.ServiceConfig) []config.PortForward {
	if old.Network == nil || len(old.PortForwards) == 0 {
		return nil
	}
	live := portForwardKeys(current)
	var obsolete []config.PortForward
	for _, pf := range old.PortForwards {
		key := portForwardKey{pf.HostPort, old.Network.GuestIP, pf.VMPort}
		if _, stillLive := live[key]; !stillLive {
			obsolete = append(obsolete, pf)
		}
	}
	return obsolete
}

// retryPendingTeardowns retries host cleanup for every service recorded in
// pendingTeardowns, on this tick's fresh deletes and any left over from
// earlier ticks (or, with WithStateDir, an earlier agent process) alike.
// Anything still pending after this pass is returned as an error, so a tick
// cannot be reported converged while a service's teardown is still failing
// outright.
//
// A name currently running a VM again — a later create or update reclaimed
// it — is handled differently from a name that stayed deleted:
//
//   - The live VM's own network device (tap) was already (re-)established by
//     that create's Setup call and must never be torn down here: Teardown is
//     keyed purely by service name, so calling it now would delete the live
//     VM's own device, not an obsolete one.
//   - Port forwards are keyed by the full (hostPort, guestIP, vmPort) tuple,
//     though, so an old rule the live config no longer claims is a distinct,
//     genuinely obsolete rule — obsoletePortForwards computes exactly that
//     set — and is still torn down and still kept pending until it is.
//
// pendingTeardowns is persisted via persistPendingTeardowns when WithStateDir
// was called; otherwise it is in-memory only and a restart between a failed
// delete's teardown and its retry loses that entry, same as before this
// method existed at all. A persist write failure degrades the same way: the
// entry stays correct in memory and keeps retrying this tick and every tick
// after, it just is not durable against a restart until a later write
// succeeds — never worse than pre-WithStateDir behavior, only sometimes not
// better.
//
// This method only runs from Reconcile, not from SyncPortForwards — so a
// pending entry is only ever retried on a tick that actually reconciles.
// That is safe only because a still-failing entry makes Reconcile return an
// error, which (in internal/agent/agent.go) stops lastRevision from
// advancing, which forces the next tick back onto the full path instead of
// the unchanged-revision fast path that calls SyncPortForwards directly.
// Once an entry clears, nothing forces another reconcile-path tick, but
// nothing needs to either.
func (r *Reconciler) retryPendingTeardowns() error {
	if len(r.pendingTeardowns) == 0 {
		return nil
	}
	running := r.vmManager.List()
	names := make([]string, 0, len(r.pendingTeardowns))
	for name := range r.pendingTeardowns {
		names = append(names, name)
	}
	sort.Strings(names)

	var errs []error
	changed := false
	for _, name := range names {
		old := r.pendingTeardowns[name]
		if inst := running[name]; inst != nil {
			obsolete := obsoletePortForwards(old, inst.Config)
			if len(obsolete) == 0 {
				delete(r.pendingTeardowns, name)
				changed = true
				continue
			}
			if err := r.teardownPortForwards(old, obsolete); err != nil {
				r.logger.Warn("stale port forward from a reclaimed service name still pending, will retry", "service", name, "error", err)
				errs = append(errs, err)
				continue
			}
			delete(r.pendingTeardowns, name)
			changed = true
			continue
		}
		if err := r.teardownHostResources(old); err != nil {
			r.logger.Warn("host teardown still pending, will retry next tick", "service", name, "error", err)
			errs = append(errs, err)
			continue
		}
		delete(r.pendingTeardowns, name)
		changed = true
	}
	if changed {
		r.persistPendingTeardowns()
	}
	return combineErrors(errs)
}

// needsUpdate compares a running instance with its desired config to
// determine if the VM needs to be recreated.
func needsUpdate(inst *vm.Instance, desired config.ServiceConfig) bool {
	cur := inst.Config

	if cur.Image != desired.Image {
		return true
	}
	if cur.Kernel != desired.Kernel {
		return true
	}
	if cur.VCPUs != desired.VCPUs {
		return true
	}
	if cur.MemoryMB != desired.MemoryMB {
		return true
	}
	if cur.KernelArgs != desired.KernelArgs {
		return true
	}
	if !networkEqual(cur.Network, desired.Network) {
		return true
	}
	if !portForwardsEqual(cur.PortForwards, desired.PortForwards) {
		return true
	}
	if !volumesEqual(cur.Volumes, desired.Volumes) {
		return true
	}

	// Check if the VM process is actually still running.
	if inst.State != vm.StateRunning {
		return true
	}

	return false
}

func volumesEqual(a, b []config.VolumeConfig) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]config.VolumeConfig(nil), a...)
	bc := append([]config.VolumeConfig(nil), b...)
	sort.Slice(ac, func(i, j int) bool { return ac[i].Name < ac[j].Name })
	sort.Slice(bc, func(i, j int) bool { return bc[i].Name < bc[j].Name })
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

// networkEqual compares two NetworkConfig pointers for equality.
func networkEqual(a, b *config.NetworkConfig) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Interface == b.Interface &&
		a.GuestIP == b.GuestIP &&
		a.GuestMAC == b.GuestMAC &&
		a.HostDevName == b.HostDevName
}

// portForwardsEqual compares two PortForward slices for equality.
func portForwardsEqual(a, b []config.PortForward) bool {
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

func countActions(actions []Action, t ActionType) int {
	n := 0
	for _, a := range actions {
		if a.Type == t {
			n++
		}
	}
	return n
}
