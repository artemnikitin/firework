package reconciler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	// The reconciler deliberately has no composite service-level teardown.
	// A tap and its bridge have independent lifetimes — a config can drop
	// host_dev_name while keeping the same explicit interface — so removing
	// them together is only ever correct when nothing else claims either,
	// which the reconciler cannot assume on any path that runs after the
	// fact. Every removal goes through these two, one resource at a time,
	// guarded by the claimant checks in the retry paths.
	DeleteTAP(name string) error
	DeleteBridge(name string) error
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
	// pendingNetworkDevices holds every individual host network device whose
	// deletion has failed, keyed by the device's own (kind, name) identity —
	// not by service name, and not by the service's composite set of
	// devices. Two separate reasons for this granularity:
	//
	// svc.Network.Interface, when set explicitly, overrides the default
	// tap-<name> path, so two consecutive generations of the same service
	// name can name two entirely distinct tap devices (A -> B -> C, same as
	// pendingPortForwards above): keying by name would let generation B's
	// delete overwrite or erase generation A's still-pending entry.
	//
	// And a service's tap and bridge have independent lifetimes: a config
	// can drop host_dev_name while keeping the same explicit interface, so
	// one component can become obsolete while the other stays live. Tracking
	// them as one composite unit meant retrying the obsolete component
	// deleted the live one too, because NetworkManager.Teardown always
	// removes both. Each device is therefore tracked and retried on its own.
	// See retryPendingNetworkDevices.
	pendingNetworkDevices map[networkDeviceKey]pendingNetworkDevice
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

// networkDeviceKind distinguishes the two independently-deletable host
// devices a service's network config can produce.
type networkDeviceKind string

const (
	networkDeviceTAP    networkDeviceKind = "tap"
	networkDeviceBridge networkDeviceKind = "bridge"
)

// networkDeviceKey identifies one host network device the way
// internal/network.Manager's DeleteTAP/DeleteBridge address it: by kind and
// name. A tap and a bridge that happened to be created for the same service
// are two distinct resources under this key, because either can outlive the
// other across a config change.
type networkDeviceKey struct {
	kind networkDeviceKind
	name string
}

// networkDevicesForService lists the host devices internal/network.Manager
// creates and destroys for svc, mirroring Manager.Setup/Teardown's naming and
// conditions exactly: a bridge named br-<name> only when host_dev_name is
// set, and a tap named by network.interface or tap-<name> when that is unset.
// Returned in Teardown's own order (bridge before tap) so retries release
// them the same way a single Teardown call would have. Nil when svc has no
// network config, matching Teardown's own no-op in that case.
func networkDevicesForService(svc config.ServiceConfig) []networkDeviceKey {
	if svc.Network == nil {
		return nil
	}
	var devices []networkDeviceKey
	if svc.Network.HostDevName != "" {
		devices = append(devices, networkDeviceKey{networkDeviceBridge, fmt.Sprintf("br-%s", svc.Name)})
	}
	tap := svc.Network.Interface
	if tap == "" {
		tap = fmt.Sprintf("tap-%s", svc.Name)
	}
	return append(devices, networkDeviceKey{networkDeviceTAP, tap})
}

// pendingNetworkDevice is one host network device whose deletion has failed
// and must keep being retried until it succeeds, independent of whatever the
// named service's current (possibly several generations later) config claims.
// ServiceName is retained for diagnostics only — the retry path decides what
// is safe to delete from the device identity and the set of devices claimed
// by *all* currently running services, never from this name.
type pendingNetworkDevice struct {
	ServiceName string            `json:"service_name"`
	Kind        networkDeviceKind `json:"kind"`
	Name        string            `json:"name"`
}

func (p pendingNetworkDevice) key() networkDeviceKey {
	return networkDeviceKey{kind: p.Kind, name: p.Name}
}

// pendingTeardownsVersion is the schema version of the journal file. Bump it
// whenever the persisted shape changes meaningfully. A file carrying any other
// version is treated as corrupt rather than best-effort decoded: an older
// shape can be perfectly valid JSON that decodes into zero-valued entries
// (a device with an empty kind and name deletes nothing and then clears
// itself), which would silently discard exactly the cleanup the journal
// exists to guarantee.
const pendingTeardownsVersion = 1

// persistedPendingTeardowns is the on-disk shape of pendingPortForwards and
// pendingNetworkDevices. Both are slices, not maps, because their natural
// keys (portForwardKey, networkDeviceKey) are structs and encoding/json
// requires string map keys.
type persistedPendingTeardowns struct {
	Version      int                    `json:"version"`
	PortForwards []pendingPortForward   `json:"port_forwards,omitempty"`
	Network      []pendingNetworkDevice `json:"network,omitempty"`
}

// validate rejects a journal that decoded cleanly but does not describe
// actionable cleanup. Every field here drives a real host operation, so a
// zero value is never a benign default: it is either a shape mismatch or a
// truncated write, and both must fail closed rather than resolve to a no-op
// that clears the entry.
func (p persistedPendingTeardowns) validate() error {
	if p.Version != pendingTeardownsVersion {
		return fmt.Errorf("unsupported journal version %d (expected %d)", p.Version, pendingTeardownsVersion)
	}
	for i, entry := range p.Network {
		switch entry.Kind {
		case networkDeviceTAP, networkDeviceBridge:
		default:
			return fmt.Errorf("network entry %d has unknown device kind %q", i, entry.Kind)
		}
		if entry.Name == "" {
			return fmt.Errorf("network entry %d has an empty device name", i)
		}
	}
	for i, entry := range p.PortForwards {
		if entry.PortForward.HostPort < 1 || entry.PortForward.HostPort > 65535 {
			return fmt.Errorf("port forward entry %d has out-of-range host port %d", i, entry.PortForward.HostPort)
		}
		if entry.PortForward.VMPort < 1 || entry.PortForward.VMPort > 65535 {
			return fmt.Errorf("port forward entry %d has out-of-range VM port %d", i, entry.PortForward.VMPort)
		}
		if entry.GuestIP == "" {
			return fmt.Errorf("port forward entry %d has an empty guest IP", i)
		}
	}
	return nil
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
		vmManager:             vmManager,
		healthMon:             healthMon,
		networkMgr:            networkMgr,
		logger:                logger,
		updateStrategy:        updateStrategy,
		updateDelay:           updateDelay,
		pendingRecovery:       make(map[string]struct{}),
		pendingPortForwards:   make(map[portForwardKey]pendingPortForward),
		pendingNetworkDevices: make(map[networkDeviceKey]pendingNetworkDevice),
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
// persists pendingPortForwards and pendingNetworkDevices across an agent
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
	// Strict decoding: an unknown field means this file was written by a
	// different schema than the one being decoded into, which is exactly the
	// case where a permissive decode yields plausible-looking zero values.
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&loaded); err != nil {
		r.logger.Error("pending teardowns journal is corrupt; this node will not report converged until it is resolved manually",
			"path", path, "error", err)
		r.journalCorrupt = true
		return r
	}
	if err := loaded.validate(); err != nil {
		r.logger.Error("pending teardowns journal is not valid; this node will not report converged until it is resolved manually",
			"path", path, "error", err)
		r.journalCorrupt = true
		return r
	}
	for _, entry := range loaded.PortForwards {
		r.pendingPortForwards[entry.key()] = entry
	}
	for _, entry := range loaded.Network {
		r.pendingNetworkDevices[entry.key()] = entry
	}
	return r
}

// errJournalCorrupt is returned by persistPendingTeardowns instead of writing
// when journalCorrupt is set. Overwriting an unreadable journal with a fresh
// but incomplete one would erase the evidence an operator needs to diagnose
// what was actually pending before the corruption — so this is a deliberate
// refusal to write, not a best-effort attempt. Callers that merely clear
// completed entries can ignore it (Reconcile already surfaces the corruption
// on every tick); the write-ahead path in deleteService must not, because
// removing a VM whose cleanup cannot be journalled is precisely the crash
// window that journal exists to close.
var errJournalCorrupt = errors.New("pending teardowns journal is corrupt or unreadable; manual recovery required")

// persistPendingTeardowns writes pendingPortForwards and
// pendingNetworkDevices to stateDir as one atomic replacement of the journal
// file, or removes the file once both are empty. A no-op returning nil when
// stateDir is unset. Returns an error — rather than only logging one — so the
// write-ahead path can refuse to remove a VM it cannot durably record the
// cleanup for.
func (r *Reconciler) persistPendingTeardowns() error {
	if r.stateDir == "" {
		return nil
	}
	if r.journalCorrupt {
		return errJournalCorrupt
	}
	path := filepath.Join(r.stateDir, pendingTeardownsFile)
	if len(r.pendingPortForwards) == 0 && len(r.pendingNetworkDevices) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing empty pending teardowns file: %w", err)
		}
		return nil
	}
	persisted := persistedPendingTeardowns{Version: pendingTeardownsVersion}
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
	for _, entry := range r.pendingNetworkDevices {
		persisted.Network = append(persisted.Network, entry)
	}
	sort.Slice(persisted.Network, func(i, j int) bool {
		a, b := persisted.Network[i].key(), persisted.Network[j].key()
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		return a.name < b.name
	})

	data, err := json.Marshal(persisted)
	if err != nil {
		return fmt.Errorf("marshalling pending teardowns: %w", err)
	}
	if err := os.MkdirAll(r.stateDir, 0o755); err != nil {
		return fmt.Errorf("creating state dir for pending teardowns: %w", err)
	}
	if err := atomicWriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing pending teardowns: %w", err)
	}
	return nil
}

// logPersistFailure reports a journal write that failed while merely clearing
// entries whose cleanup already succeeded. That is not fatal: every host
// operation the journal drives (DeleteTAP, DeleteBridge, TeardownPortForward)
// is idempotent, so a stale entry surviving to the next tick costs one
// redundant delete and nothing else. errJournalCorrupt is filtered out
// because Reconcile already surfaces that condition on every single tick.
func (r *Reconciler) logPersistFailure(err error) {
	if err == nil || errors.Is(err, errJournalCorrupt) {
		return
	}
	r.logger.Warn("failed to persist pending teardowns", "error", err)
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
	if r.networkMgr != nil {
		// Write-ahead, for the same reason deleteService journals before
		// removing a VM: a create that fails partway leaves host devices
		// behind that nothing else will ever notice. A service whose Start
		// never succeeded is absent from vmManager.List(), so if it is then
		// dropped from desired state Plan emits no delete action for it and
		// the devices are orphaned — and a crash between Setup and Start
		// leaves them with no record at all. Journalling the devices before
		// they are created closes both windows: whatever exists on the host
		// afterwards is already accounted for.
		if err := r.journalNetworkDevices(svc); err != nil {
			return stageError(FailureStageNetwork,
				fmt.Errorf("journalling network devices for %s before creating them: %w", svc.Name, err))
		}
		if err := r.networkMgr.Setup(svc); err != nil {
			// The journalled entries stay: Setup can fail after creating one
			// of the two devices (it rolls the tap back on a bridge failure,
			// but that rollback can itself fail), and the retry path is now
			// the thing that finishes the job.
			return stageError(FailureStageNetwork, fmt.Errorf("network setup: %w", err))
		}
	}

	// Start the VM.
	if err := r.vmManager.Start(ctx, svc); err != nil {
		// Roll back networking through the component-level teardown so a
		// failure to remove either device is recorded and retried rather
		// than discarded. Nothing claims these devices — the VM did not
		// start — so the retry path will keep at them until they are gone.
		if r.networkMgr != nil {
			if tdErr := r.teardownAndTrackNetwork(svc); tdErr != nil {
				r.logger.Warn("failed to roll back networking after VM start failure; will retry",
					"service", svc.Name, "error", tdErr)
			}
			r.logPersistFailure(r.persistPendingTeardowns())
		}
		return stageError(FailureStageVM, fmt.Errorf("starting VM: %w", err))
	}

	// The VM is running and now claims these devices, so the write-ahead
	// entries have served their purpose. Dropping them here keeps the journal
	// empty in steady state; the claimant guard in retryPendingNetworkDevices
	// would otherwise do it on the next tick anyway.
	if r.networkMgr != nil {
		r.clearJournalledNetworkDevices(svc)
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
	// Write-ahead: durably record every host resource this delete will have
	// to clean up *before* the VM goes away. Once Remove succeeds, svc is
	// absent from both desired and actual state, so Plan will never emit a
	// delete action for it again and this journal is the only thing that can
	// still finish the cleanup. Journalling afterwards left a crash window in
	// which the VM was already gone, no delete action would ever be planned
	// again, and no durable record existed — the node restarted and reported
	// converged with stale DNAT rules and network devices still on the host.
	//
	// A journal write failure aborts the delete instead of proceeding: a VM
	// that outlives one tick is corrected on the next one, whereas a leaked
	// host resource with no record of it is unrecoverable without an
	// operator. This does mean a full disk or a corrupt journal blocks
	// deletes outright, which is the intended fail-closed trade.
	if err := r.journalIntendedTeardowns(svc); err != nil {
		return stageError(FailureStageNetwork,
			fmt.Errorf("journalling pending teardowns for %s before removing its VM: %w", svc.Name, err))
	}

	// Prove ownership and stop the VM before removing host resources. If the
	// process identity is ambiguous, keep the service quarantined and intact.
	if err := r.vmManager.Remove(svc.Name); err != nil {
		// The entries just journalled are deliberately left in place. They
		// are not leaked: the VM is still running, so the retry paths see
		// svc.Name in vmManager.List() and find every one of those resources
		// claimed by a *running* config, which drops them silently on the
		// next tick. That self-healing depends on the retry guards comparing
		// against the running config rather than the desired one — see
		// retryPendingPortForwards and retryPendingNetworkDevices.
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
	r.logPersistFailure(r.persistPendingTeardowns())
	if len(errs) > 0 {
		r.logger.Warn("failed to tear down host resources; will retry", "service", svc.Name, "error", combineErrors(errs))
	}
	return nil
}

// journalIntendedTeardowns records every host resource deleteService is about
// to become responsible for cleaning up, and durably persists that record
// before the caller removes the VM. Entries are cleared again as each
// resource is actually released, so the steady-state journal stays empty.
//
// On a write failure the in-memory maps are restored to exactly what they
// held before, which is also exactly what the journal file still holds:
// atomicWriteFile leaves the previous file untouched when it fails, so
// rolling back in memory restores agreement between the two.
func (r *Reconciler) journalIntendedTeardowns(svc config.ServiceConfig) error {
	if r.networkMgr == nil {
		return nil
	}
	return r.journalPendingCleanup(func() {
		if svc.Network != nil {
			for _, pf := range svc.PortForwards {
				key := portForwardKey{pf.HostPort, svc.Network.GuestIP, pf.VMPort}
				r.pendingPortForwards[key] = pendingPortForward{ServiceName: svc.Name, GuestIP: svc.Network.GuestIP, PortForward: pf}
			}
		}
		r.addPendingNetworkDevices(svc)
	})
}

// journalNetworkDevices durably records the host network devices createService
// is about to create, before it creates them. Only the devices: a create sets
// its port forwards up after the VM is already running, and syncPortForwards
// owns retrying those.
func (r *Reconciler) journalNetworkDevices(svc config.ServiceConfig) error {
	if r.networkMgr == nil {
		return nil
	}
	return r.journalPendingCleanup(func() { r.addPendingNetworkDevices(svc) })
}

func (r *Reconciler) addPendingNetworkDevices(svc config.ServiceConfig) {
	for _, dev := range networkDevicesForService(svc) {
		r.pendingNetworkDevices[dev] = pendingNetworkDevice{ServiceName: svc.Name, Kind: dev.kind, Name: dev.name}
	}
}

// clearJournalledNetworkDevices drops svc's device entries once the running VM
// has taken ownership of them, so the journal returns to empty in steady state.
func (r *Reconciler) clearJournalledNetworkDevices(svc config.ServiceConfig) {
	changed := false
	for _, dev := range networkDevicesForService(svc) {
		if _, pending := r.pendingNetworkDevices[dev]; pending {
			delete(r.pendingNetworkDevices, dev)
			changed = true
		}
	}
	if changed {
		r.logPersistFailure(r.persistPendingTeardowns())
	}
}

// journalPendingCleanup applies mutate to the pending maps and durably records
// the result. On a write failure both maps are restored to exactly what they
// held before, which is also exactly what the journal file still holds:
// atomicWriteFile leaves the previous file untouched when it fails, so rolling
// back in memory restores agreement between the two.
func (r *Reconciler) journalPendingCleanup(mutate func()) error {
	prevPortForwards := maps.Clone(r.pendingPortForwards)
	prevNetworkDevices := maps.Clone(r.pendingNetworkDevices)
	mutate()
	if err := r.persistPendingTeardowns(); err != nil {
		r.pendingPortForwards = prevPortForwards
		r.pendingNetworkDevices = prevNetworkDevices
		return err
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

// teardownAndTrackNetwork deletes each of svc's host network devices
// individually rather than through NetworkManager.Teardown, so that a
// failure on one component leaves only that component pending: a bridge that
// refuses to go away must not keep a successfully-deleted tap on the retry
// list, and vice versa. Each device is tracked by its own (kind, name)
// identity, so a success clears whichever generation of svc.Name originally
// recorded it. A no-op if there is no network manager or no network config.
func (r *Reconciler) teardownAndTrackNetwork(svc config.ServiceConfig) error {
	if r.networkMgr == nil {
		return nil
	}
	var errs []error
	for _, dev := range networkDevicesForService(svc) {
		if err := r.deleteNetworkDevice(dev); err != nil {
			r.pendingNetworkDevices[dev] = pendingNetworkDevice{ServiceName: svc.Name, Kind: dev.kind, Name: dev.name}
			errs = append(errs, stageError(FailureStageNetwork,
				fmt.Errorf("delete %s %s for %s: %w", dev.kind, dev.name, svc.Name, err)))
			continue
		}
		delete(r.pendingNetworkDevices, dev)
	}
	return combineErrors(errs)
}

// deleteNetworkDevice removes one host network device by its identity.
func (r *Reconciler) deleteNetworkDevice(dev networkDeviceKey) error {
	if dev.kind == networkDeviceBridge {
		return r.networkMgr.DeleteBridge(dev.name)
	}
	return r.networkMgr.DeleteTAP(dev.name)
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
// pendingNetworkDevices. See their respective retry methods for how each
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
	if err := r.retryPendingNetworkDevices(); err != nil {
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
// A rule is skipped — dropped from the pending set without being torn down —
// whenever *any* currently running service's config claims it. Checking every
// running service rather than only entry.ServiceName matters because a DNAT
// rule is identified by its tuple alone: after a rename or a replacement,
// service B can legitimately claim the exact (hostPort, guestIP, vmPort) that
// service A left pending, and retrying A's entry would delete B's live rule
// while reporting a successful reconciliation. This mirrors the identical
// guard in retryPendingNetworkDevices.
func (r *Reconciler) retryPendingPortForwards() error {
	if r.networkMgr == nil || len(r.pendingPortForwards) == 0 {
		return nil
	}
	claimed := make(map[portForwardKey]string)
	for name, inst := range r.vmManager.List() {
		for key := range portForwardKeys(inst.Config) {
			claimed[key] = name
		}
	}
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
		if owner, live := claimed[key]; live {
			r.logger.Debug("pending port forward is claimed by a running service; dropping without tearing down",
				"host_port", key.hostPort, "guest_ip", key.guestIP, "vm_port", key.vmPort, "claimed_by", owner)
			delete(r.pendingPortForwards, key)
			changed = true
			continue
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
		r.logPersistFailure(r.persistPendingTeardowns())
	}
	return combineErrors(errs)
}

// retryPendingNetworkDevices retries every entry in pendingNetworkDevices,
// keyed by the device's own (kind, name) identity. That granularity is what
// makes the retry safe on two axes at once: a service passing through several
// configs before an earlier deletion succeeds (A -> B -> C, each naming a
// distinct tap) keeps one entry per device rather than one per name, and a
// tap and bridge that were created together are released separately rather
// than through NetworkManager.Teardown, which always removes both.
//
// A device is skipped — dropped from the pending set without being deleted —
// whenever *any* currently running service's config claims it. Checking every
// running service rather than only entry.ServiceName is deliberate: an
// explicit network.interface can be carried across a config change that alters
// the rest of the service's networking, so the device that is obsolete for one
// generation may be the live device of another. Deleting a claimed device
// would break a running VM while reporting a successful reconciliation.
func (r *Reconciler) retryPendingNetworkDevices() error {
	if r.networkMgr == nil || len(r.pendingNetworkDevices) == 0 {
		return nil
	}
	claimed := make(map[networkDeviceKey]string)
	for name, inst := range r.vmManager.List() {
		for _, dev := range networkDevicesForService(inst.Config) {
			claimed[dev] = name
		}
	}

	keys := make([]networkDeviceKey, 0, len(r.pendingNetworkDevices))
	for key := range r.pendingNetworkDevices {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		return a.name < b.name
	})

	var errs []error
	changed := false
	for _, key := range keys {
		entry := r.pendingNetworkDevices[key]
		if owner, live := claimed[key]; live {
			r.logger.Debug("pending network device is claimed by a running service; dropping without deleting",
				"kind", key.kind, "device", key.name, "claimed_by", owner)
			delete(r.pendingNetworkDevices, key)
			changed = true
			continue
		}
		if err := r.deleteNetworkDevice(key); err != nil {
			r.logger.Warn("network device deletion still pending, will retry next tick",
				"service", entry.ServiceName, "kind", key.kind, "device", key.name, "error", err)
			errs = append(errs, stageError(FailureStageNetwork,
				fmt.Errorf("delete %s %s for %s: %w", key.kind, key.name, entry.ServiceName, err)))
			continue
		}
		delete(r.pendingNetworkDevices, key)
		changed = true
	}
	if changed {
		r.logPersistFailure(r.persistPendingTeardowns())
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
