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
	// pendingPortForwards holds every DNAT rule whose teardown has failed,
	// keyed by the rule's own (hostPort, guestIP, vmPort) identity — not by
	// service name. A service can pass through several configs in a row
	// before an earlier teardown ever succeeds (A -> B -> C); keying by name
	// would let generation B's delete (success or failure) overwrite or
	// erase generation A's still-pending entry, silently losing track of
	// A's obsolete rule. Each rule's identity is independent, so each gets
	// its own entry and its own retry history. See retryPendingPortForwards.
	pendingPortForwards map[portForwardKey]pendingPortForward
	// pendingNetworkTeardowns holds every network device (tap, plus its
	// optional bridge) whose teardown has failed, keyed by the device's own
	// identity — not by service name. svc.Network.Interface, when set
	// explicitly, overrides the default tap-<name> path (see
	// internal/network.Manager.Teardown), so two consecutive generations of
	// the same service name can name two entirely distinct tap devices (A ->
	// B -> C, same as pendingPortForwards above): keying by name would let
	// generation B's delete (success or failure) overwrite or erase
	// generation A's still-pending device entry, silently leaking A's tap
	// (and possibly bridge). Each device identity gets its own entry and its
	// own retry history. See retryPendingNetworkTeardowns.
	pendingNetworkTeardowns map[networkTeardownKey]pendingNetworkTeardown
	// Once Remove succeeds a service leaves both desired and actual state,
	// so Plan produces no further delete action for it — retryPendingTeardowns
	// (which retries both maps above) is the only thing that will ever
	// finish, or keep retrying, that cleanup, on this tick and every tick
	// after until it succeeds. Both maps are persisted to stateDir (when
	// set) so pending cleanup survives an agent restart; see WithStateDir.
	//
	// stateDir, when non-empty, is where they are persisted. Set via
	// WithStateDir; empty by default, which keeps everything in-memory only
	// (used throughout the test suite).
	stateDir string
	// journalCorrupt is set by WithStateDir when the persisted journal
	// exists but cannot be read or parsed. It permanently fails every
	// Reconcile call and blocks further journal writes for the life of this
	// process: silently proceeding with an empty pending set would let the
	// node report converged while whatever was actually pending before the
	// corruption — quite possibly a still-live obsolete DNAT rule — goes
	// forever unrecovered. Recovery is manual: stop the agent, inspect and
	// remove <stateDir>/pending_teardowns.json, restart.
	journalCorrupt bool
}

// pendingPortForward is one DNAT rule whose teardown has failed and must
// keep being retried until it succeeds, independent of whatever the named
// service's current (possibly several generations later) config claims.
type pendingPortForward struct {
	ServiceName string             `json:"service_name"`
	GuestIP     string             `json:"guest_ip"`
	PortForward config.PortForward `json:"port_forward"`
}

func (p pendingPortForward) key() portForwardKey {
	return portForwardKey{p.PortForward.HostPort, p.GuestIP, p.PortForward.VMPort}
}

// networkTeardownKey identifies a single network device the way
// internal/network.Manager's Setup/Teardown actually key it: by the tap
// device name (svc.Network.Interface, or tap-<name> when unset) plus the
// bridge name when a bridge was created (br-<name>, only when
// svc.Network.HostDevName is set) — not by service name. Two different
// service configs sharing a name across an update can still name entirely
// distinct tap devices under this key.
type networkTeardownKey struct {
	tapName    string
	bridgeName string
}

// networkKeyForService computes the networkTeardownKey that
// internal/network.Manager.Setup/Teardown would use for svc, mirroring its
// tap/bridge naming exactly. Returns the zero key if svc has no network
// config, matching Teardown's own no-op in that case.
func networkKeyForService(svc config.ServiceConfig) networkTeardownKey {
	if svc.Network == nil {
		return networkTeardownKey{}
	}
	tap := svc.Network.Interface
	if tap == "" {
		tap = fmt.Sprintf("tap-%s", svc.Name)
	}
	var bridge string
	if svc.Network.HostDevName != "" {
		bridge = fmt.Sprintf("br-%s", svc.Name)
	}
	return networkTeardownKey{tapName: tap, bridgeName: bridge}
}

// pendingNetworkTeardown is one network device (tap, plus optional bridge)
// whose teardown has failed and must keep being retried until it succeeds,
// independent of whatever the named service's current (possibly several
// generations later) config claims.
type pendingNetworkTeardown struct {
	ServiceName string               `json:"service_name"`
	Config      config.ServiceConfig `json:"config"`
}

func (p pendingNetworkTeardown) key() networkTeardownKey {
	return networkKeyForService(p.Config)
}

// persistedPendingTeardowns is the on-disk shape of pendingPortForwards and
// pendingNetworkTeardowns. Both are slices, not maps, because their natural
// keys (portForwardKey, networkTeardownKey) are structs and encoding/json
// requires string map keys.
type persistedPendingTeardowns struct {
	PortForwards []pendingPortForward     `json:"port_forwards,omitempty"`
	Network      []pendingNetworkTeardown `json:"network,omitempty"`
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
		vmManager:               vmManager,
		healthMon:               healthMon,
		networkMgr:              networkMgr,
		logger:                  logger,
		updateStrategy:          updateStrategy,
		updateDelay:             updateDelay,
		pendingRecovery:         make(map[string]struct{}),
		pendingPortForwards:     make(map[portForwardKey]pendingPortForward),
		pendingNetworkTeardowns: make(map[networkTeardownKey]pendingNetworkTeardown),
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
// persists pendingPortForwards and pendingNetworkTeardowns across an agent
// restart.
const pendingTeardownsFile = "pending_teardowns.json"

// WithStateDir enables persisting pending teardown state to
// <dir>/pending_teardowns.json, so cleanup survives an agent restart between
// a failed teardown and its retry — without this, that entry only ever lived
// in memory and a restart silently forgot it, leaving the obsolete host rule
// with nothing left to notice it. Loads any existing file immediately; the
// first Reconcile call retries whatever it finds. If the file exists but
// cannot be read or parsed, this sets journalCorrupt instead of silently
// continuing with an empty set — see its doc comment. Returns r so it can be
// chained onto New/NewWithNetworkManager; a Reconciler that never calls this
// behaves exactly as before, tracking pending teardowns in memory only (the
// default in tests).
func (r *Reconciler) WithStateDir(dir string) *Reconciler {
	r.stateDir = dir
	if dir == "" {
		return r
	}
	path := filepath.Join(dir, pendingTeardownsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			// A read failure other than "file does not exist" (permission
			// denied, I/O error) cannot be distinguished from "a journal
			// existed and is now unreadable", so it gets the same fail-closed
			// treatment as a parse failure below.
			r.logger.Error("pending teardowns journal unreadable; this node will not report converged until it is resolved manually",
				"path", path, "error", err)
			r.journalCorrupt = true
		}
		return r
	}
	var loaded persistedPendingTeardowns
	if err := json.Unmarshal(data, &loaded); err != nil {
		r.logger.Error("pending teardowns journal is corrupt; this node will not report converged until it is resolved manually",
			"path", path, "error", err)
		r.journalCorrupt = true
		return r
	}
	for _, entry := range loaded.PortForwards {
		r.pendingPortForwards[entry.key()] = entry
	}
	for _, entry := range loaded.Network {
		r.pendingNetworkTeardowns[entry.key()] = entry
	}
	return r
}

// persistPendingTeardowns writes pendingPortForwards and
// pendingNetworkTeardowns to stateDir as one atomic replacement of the
// journal file, or removes the file once both are empty. A no-op when
// stateDir is unset, and a no-op (deliberately, not a best-effort write)
// once journalCorrupt is set: overwriting an unreadable journal with a fresh
// but incomplete one would erase the evidence an operator needs to diagnose
// what was actually pending before the corruption.
func (r *Reconciler) persistPendingTeardowns() {
	if r.stateDir == "" || r.journalCorrupt {
		return
	}
	path := filepath.Join(r.stateDir, pendingTeardownsFile)
	if len(r.pendingPortForwards) == 0 && len(r.pendingNetworkTeardowns) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			r.logger.Warn("failed to remove empty pending teardowns file", "error", err)
		}
		return
	}
	var persisted persistedPendingTeardowns
	for _, entry := range r.pendingPortForwards {
		persisted.PortForwards = append(persisted.PortForwards, entry)
	}
	sort.Slice(persisted.PortForwards, func(i, j int) bool {
		a, b := persisted.PortForwards[i], persisted.PortForwards[j]
		if a.PortForward.HostPort != b.PortForward.HostPort {
			return a.PortForward.HostPort < b.PortForward.HostPort
		}
		if a.GuestIP != b.GuestIP {
			return a.GuestIP < b.GuestIP
		}
		return a.PortForward.VMPort < b.PortForward.VMPort
	})
	for _, entry := range r.pendingNetworkTeardowns {
		persisted.Network = append(persisted.Network, entry)
	}
	sort.Slice(persisted.Network, func(i, j int) bool {
		a, b := persisted.Network[i].key(), persisted.Network[j].key()
		if a.tapName != b.tapName {
			return a.tapName < b.tapName
		}
		return a.bridgeName < b.bridgeName
	})

	data, err := json.Marshal(persisted)
	if err != nil {
		r.logger.Warn("failed to marshal pending teardowns", "error", err)
		return
	}
	if err := os.MkdirAll(r.stateDir, 0o755); err != nil {
		r.logger.Warn("failed to create state dir for pending teardowns", "error", err)
		return
	}
	if err := atomicWriteFile(path, data, 0o644); err != nil {
		r.logger.Warn("failed to persist pending teardowns", "error", err)
	}
}

// atomicWriteFile replaces path with data as a single atomic operation: the
// new content is written to a temporary file in the same directory (so the
// rename below is within one filesystem and therefore atomic on POSIX),
// fsynced, renamed over path, and the directory entry for that rename is
// itself fsynced. A crash or power loss before the rename leaves the
// previous file, if any, completely untouched — never the
// truncated-then-partially-written file os.WriteFile's truncate-then-write
// can leave when interrupted mid-write. Without the final directory fsync,
// the rename itself is only guaranteed durable once the OS decides to flush
// the directory's metadata on its own; a crash in that window can still roll
// path back to its pre-rename content even though Rename returned success.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".pending-teardowns-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	dirHandle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer dirHandle.Close()
	return dirHandle.Sync()
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
	if r.journalCorrupt {
		// Deliberately does not skip the rest of Reconcile: VMs and port
		// forwards still converge normally, only the convergence *signal*
		// is permanently poisoned until an operator resolves the journal by
		// hand. See journalCorrupt's doc comment on the Reconciler struct.
		errs = append(errs, stageError(FailureStageNetwork,
			fmt.Errorf("pending teardowns journal at %s is corrupt or unreadable; manual recovery required before this node can report convergence",
				filepath.Join(r.stateDir, pendingTeardownsFile))))
	}
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
	// Retry host cleanup for every port forward and network device whose
	// teardown outran a VM removal, on this tick's fresh deletes and any
	// left over from earlier ticks alike. This must run after Plan/Apply
	// above: Plan only diffs desired against actual VMs, and a service that
	// finished being removed on this or an earlier tick is absent from
	// both, so this is the only remaining path that will ever finish
	// tearing down its host rules.
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

	// Neither teardown failure here is returned: the update path skips the
	// recreate when this function errors, and a transient teardown failure
	// must not leave a service deleted. The VM is already gone, though, so
	// this is the last tick that will ever see svc as a Plan delete action —
	// a failure is recorded so retryPendingTeardowns keeps retrying it on
	// every later tick until it actually succeeds, instead of being logged
	// once and never revisited. At this exact point nothing is running
	// under svc.Name yet (Remove just succeeded, and any recreate under an
	// update happens after this call returns), so tearing down everything
	// unconditionally is safe here; only the retry path, which can run
	// after that recreate has happened, needs to be more careful about what
	// it repeats.
	var errs []error
	if err := r.teardownAndTrackPortForwards(svc, svc.PortForwards); err != nil {
		errs = append(errs, err)
	}
	if err := r.teardownAndTrackNetwork(svc); err != nil {
		errs = append(errs, err)
	}
	r.persistPendingTeardowns()
	if len(errs) > 0 {
		r.logger.Warn("failed to tear down host resources; will retry", "service", svc.Name, "error", combineErrors(errs))
	}
	return nil
}

// teardownAndTrackPortForwards tears down each of pfs for svc's guest IP.
// Each one is tracked independently, by its own (hostPort, guestIP, vmPort)
// identity: a failure adds (or refreshes) its pendingPortForwards entry, a
// success clears any entry under that same key, whichever generation of
// svc.Name originally recorded it. A no-op if svc has no network.
func (r *Reconciler) teardownAndTrackPortForwards(svc config.ServiceConfig, pfs []config.PortForward) error {
	if r.networkMgr == nil || svc.Network == nil || len(pfs) == 0 {
		return nil
	}
	var errs []error
	for _, pf := range pfs {
		key := portForwardKey{pf.HostPort, svc.Network.GuestIP, pf.VMPort}
		if err := r.networkMgr.TeardownPortForward(pf.HostPort, svc.Network.GuestIP, pf.VMPort); err != nil {
			r.pendingPortForwards[key] = pendingPortForward{ServiceName: svc.Name, GuestIP: svc.Network.GuestIP, PortForward: pf}
			errs = append(errs, stageError(FailureStageNetwork,
				fmt.Errorf("teardown port forward %d for %s: %w", pf.HostPort, svc.Name, err)))
			continue
		}
		delete(r.pendingPortForwards, key)
	}
	return combineErrors(errs)
}

// teardownAndTrackNetwork tears down svc's network device (tap, plus its
// optional bridge), tracking the outcome in pendingNetworkTeardowns keyed by
// the device's own identity (see networkKeyForService), not by svc.Name. A
// no-op if there is no network manager.
func (r *Reconciler) teardownAndTrackNetwork(svc config.ServiceConfig) error {
	if r.networkMgr == nil {
		return nil
	}
	key := networkKeyForService(svc)
	if err := r.networkMgr.Teardown(svc); err != nil {
		r.pendingNetworkTeardowns[key] = pendingNetworkTeardown{ServiceName: svc.Name, Config: svc}
		return stageError(FailureStageNetwork, fmt.Errorf("teardown network device %s for %s: %w", key.tapName, svc.Name, err))
	}
	delete(r.pendingNetworkTeardowns, key)
	return nil
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

// retryPendingTeardowns retries both pendingPortForwards and
// pendingNetworkTeardowns. See their respective retry methods for how each
// is tracked and why they need different reclaimed-name handling. Anything
// still pending after this pass keeps being returned as an error, so a tick
// cannot be reported converged while a teardown is still failing outright.
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
	var errs []error
	if err := r.retryPendingPortForwards(); err != nil {
		errs = append(errs, err)
	}
	if err := r.retryPendingNetworkTeardowns(); err != nil {
		errs = append(errs, err)
	}
	return combineErrors(errs)
}

// retryPendingPortForwards retries every entry in pendingPortForwards, keyed
// by the rule's own (hostPort, guestIP, vmPort) identity so that a service
// passing through several configs before an earlier teardown ever succeeds
// (A -> B -> C) cannot lose track of an older generation's still-obsolete
// rule: each generation's failure gets its own entry, independent of
// whatever entry.ServiceName's current config looks like.
//
// If entry.ServiceName is running again — a later create or update reclaimed
// it — this rule is retried only if the live config no longer claims the
// exact same (hostPort, guestIP, vmPort): if it does, this "pending" rule is
// in fact the live service's current rule (most often because an update left
// the port forward unchanged), and tearing it down would break it rather
// than clean up anything obsolete.
func (r *Reconciler) retryPendingPortForwards() error {
	if r.networkMgr == nil || len(r.pendingPortForwards) == 0 {
		return nil
	}
	running := r.vmManager.List()
	keys := make([]portForwardKey, 0, len(r.pendingPortForwards))
	for key := range r.pendingPortForwards {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.hostPort != b.hostPort {
			return a.hostPort < b.hostPort
		}
		if a.guestIP != b.guestIP {
			return a.guestIP < b.guestIP
		}
		return a.vmPort < b.vmPort
	})

	var errs []error
	changed := false
	for _, key := range keys {
		entry := r.pendingPortForwards[key]
		if inst := running[entry.ServiceName]; inst != nil {
			if _, stillLive := portForwardKeys(inst.Config)[key]; stillLive {
				delete(r.pendingPortForwards, key)
				changed = true
				continue
			}
		}
		if err := r.networkMgr.TeardownPortForward(key.hostPort, key.guestIP, key.vmPort); err != nil {
			r.logger.Warn("port forward teardown still pending, will retry next tick",
				"service", entry.ServiceName, "host_port", key.hostPort, "error", err)
			errs = append(errs, stageError(FailureStageNetwork,
				fmt.Errorf("teardown port forward %d for %s: %w", key.hostPort, entry.ServiceName, err)))
			continue
		}
		delete(r.pendingPortForwards, key)
		changed = true
	}
	if changed {
		r.persistPendingTeardowns()
	}
	return combineErrors(errs)
}

// retryPendingNetworkTeardowns retries every entry in
// pendingNetworkTeardowns, keyed by the device's own identity so that a
// service passing through several configs before an earlier teardown ever
// succeeds (A -> B -> C, each naming a distinct tap via svc.Network.Interface)
// cannot lose track of an older generation's still-obsolete device: each
// generation's failure gets its own entry, independent of whatever
// entry.ServiceName's current config looks like.
//
// If entry.ServiceName is running again — a later create or update reclaimed
// it — this device is retried only if the live config's own device identity
// differs from this entry's key: if it matches, this "pending" device is in
// fact the live service's current tap (most often because an update left the
// network config unchanged), and tearing it down would break the running VM
// rather than clean up anything obsolete.
func (r *Reconciler) retryPendingNetworkTeardowns() error {
	if r.networkMgr == nil || len(r.pendingNetworkTeardowns) == 0 {
		return nil
	}
	running := r.vmManager.List()
	keys := make([]networkTeardownKey, 0, len(r.pendingNetworkTeardowns))
	for key := range r.pendingNetworkTeardowns {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.tapName != b.tapName {
			return a.tapName < b.tapName
		}
		return a.bridgeName < b.bridgeName
	})

	var errs []error
	changed := false
	for _, key := range keys {
		entry := r.pendingNetworkTeardowns[key]
		if inst := running[entry.ServiceName]; inst != nil {
			if networkKeyForService(inst.Config) == key {
				delete(r.pendingNetworkTeardowns, key)
				changed = true
				continue
			}
		}
		if err := r.networkMgr.Teardown(entry.Config); err != nil {
			r.logger.Warn("network teardown still pending, will retry next tick",
				"service", entry.ServiceName, "tap", key.tapName, "error", err)
			errs = append(errs, stageError(FailureStageNetwork, fmt.Errorf("teardown network device %s for %s: %w", key.tapName, entry.ServiceName, err)))
			continue
		}
		delete(r.pendingNetworkTeardowns, key)
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
