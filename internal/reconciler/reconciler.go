package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// tickTeardownErrs collects non-blocking host-teardown failures for the
	// current Reconcile call. Reset per tick; see deleteService.
	tickTeardownErrs []error
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
		vmManager:       vmManager,
		healthMon:       healthMon,
		networkMgr:      networkMgr,
		logger:          logger,
		updateStrategy:  updateStrategy,
		updateDelay:     updateDelay,
		pendingRecovery: make(map[string]struct{}),
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
	r.tickTeardownErrs = nil
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
	errs = append(errs, r.tickTeardownErrs...)
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
// currently has a VM. It only adds rules; obsolete rules left behind by a
// failed teardown are not pruned, which would require enumerating existing
// rules rather than asserting desired ones.
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

// deleteService deregisters health checks, tears down port forwards,
// stops the VM, and tears down networking.
func (r *Reconciler) deleteService(svc config.ServiceConfig) error {
	// Prove ownership and stop the VM before removing host resources. If the
	// process identity is ambiguous, keep the service quarantined and intact.
	if err := r.vmManager.Remove(svc.Name); err != nil {
		return stageError(FailureStageVM, fmt.Errorf("remove VM: %w", err))
	}

	if r.healthMon != nil {
		r.healthMon.Deregister(svc.Name)
	}

	// Teardown failures are recorded for the tick rather than returned. A DNAT
	// rule left pointing at a dead guest silently misroutes traffic on that host
	// port, so it must not stay invisible — but the update path skips the
	// recreate when this function errors, and a transient teardown failure must
	// not leave a service deleted. Reconcile folds these into the tick result.
	if r.networkMgr != nil && svc.Network != nil && len(svc.PortForwards) > 0 {
		for _, pf := range svc.PortForwards {
			if err := r.networkMgr.TeardownPortForward(pf.HostPort, svc.Network.GuestIP, pf.VMPort); err != nil {
				r.logger.Warn("failed to teardown port forward",
					"service", svc.Name, "host_port", pf.HostPort, "error", err)
				r.tickTeardownErrs = append(r.tickTeardownErrs, stageError(FailureStageNetwork,
					fmt.Errorf("teardown port forward %d for %s: %w", pf.HostPort, svc.Name, err)))
			}
		}
	}

	// Tear down network.
	if r.networkMgr != nil {
		if err := r.networkMgr.Teardown(svc); err != nil {
			r.logger.Warn("failed to tear down network", "service", svc.Name, "error", err)
			r.tickTeardownErrs = append(r.tickTeardownErrs, stageError(FailureStageNetwork,
				fmt.Errorf("teardown network for %s: %w", svc.Name, err)))
		}
	}
	return nil
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
