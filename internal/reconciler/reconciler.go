package reconciler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/healthcheck"
	"github.com/artemnikitin/firework/internal/network"
	"github.com/artemnikitin/firework/internal/vm"
	"github.com/artemnikitin/firework/internal/volume"
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
	Preflight(context.Context, config.ServiceConfig) ([]volume.Rejection, error)
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
	// SetupPortForward installs the service's tagged DNAT rules.
	SetupPortForward(service string, hostPort int, guestIP string, vmPort int) error
	// ListPortForwards lists the host's DNAT rules. The kernel's list, not
	// any record, is the inventory of what can deliver traffic.
	ListPortForwards() (network.PortForwardListing, error)
	// DeletePortForwardRule deletes exactly one listed rule.
	DeletePortForwardRule(network.PortForwardRule) error
}

// Reconciler compares desired state from the config store with the actual
// state of running VMs and produces a plan to converge them.
type Reconciler struct {
	// cleanupMu serializes cleanup journal mutations with recovery commands.
	// Cleanup retry may call Manager.List while holding it; the manager must
	// release its own mutex before invoking the cleanup recorder.
	cleanupMu       sync.Mutex
	vmManager       VMManager
	healthMon       *healthcheck.Monitor
	networkMgr      NetworkManager
	logger          *slog.Logger
	updateStrategy  string
	updateDelay     time.Duration
	sleepFn         func(context.Context, time.Duration) error
	pendingRecovery map[string]struct{}
	heldMu          sync.RWMutex
	heldServices    map[string]Hold
	capacity        *capacityLimit
	// planned is the revision Reconcile is applying, so a per-action hold
	// check sees held running owners across the whole revision.
	planned *config.NodeConfig
	// allocations are the ownership records of guest addresses and port
	// forwards, keyed by a journal-assigned ID. A service can have several
	// at once: an update keeps its old VM's departing record until that VM's
	// rules are gone while the new VM's record is created. See ownership.go.
	allocations      map[uint64]allocation
	nextAllocationID uint64
	history          journalHistory
	// hostPortHolders maps each host port this node has allocated to the
	// service that first held it (L6).
	hostPortHolders map[int]string
	// routeWithdrawals hold guest addresses whose routes were withdrawn
	// until no process that may still serve them runs (L7), and
	// routeEvidence reads the routes and that process. See route_withdrawal.go.
	routeWithdrawals map[routeKey]routeWithdrawal
	routeEvidence    RouteEvidence
	// unaccountedVMs is the last scan for VM processes and launches no
	// tracked VM accounts for, or the error that prevented it. Their
	// addresses are unknown, so no new address is given out while any runs.
	unaccountedVMs    []string
	unaccountedVMsErr error
	// journalReady is false until the version 2 journal is loaded or created.
	// New networked allocations wait until then.
	journalReady bool
	// portsUntrackedPending means a loaded journal must still record that
	// host ports go untracked while this agent runs without host networking.
	// Until that write succeeds the journal is not ready, so nothing starts.
	portsUntrackedPending bool
	// legacyJournal holds a version 1 journal until the first tick migrates
	// it, once the VM inventory is known.
	legacyJournal *persistedPendingTeardowns
	// journalWrite replaces the journal file. Tests use it to fail or crash
	// at a chosen write.
	journalWrite func(path string, data []byte) error
	// guestSubnet and localRoutes are the other evidence of earlier firework
	// state that initialization checks; see WithHostHistoryEvidence.
	guestSubnet *net.IPNet
	localRoutes func() (bool, error)
	// pendingNetworkDevices holds every individual host network device whose
	// deletion has failed, keyed by the device's own (kind, name) identity —
	// not by service name, and not by the service's composite set of
	// devices. Two separate reasons for this granularity:
	//
	// svc.Network.Interface, when set explicitly, overrides the default
	// tap-<name> path, so two consecutive generations of the same service
	// name can name two entirely distinct tap devices (A -> B -> C): keying
	// by name would let generation B's
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
	// (which retries the records and devices above) is the only thing that will ever
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

// pendingPortForward is one port-forward teardown recorded by a version 1
// journal. The first tick migrates it into a departing allocation record.
type pendingPortForward struct {
	ServiceName string             `json:"service_name"`
	GuestIP     string             `json:"guest_ip"`
	PortForward config.PortForward `json:"port_forward"`
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

// pendingTeardownsVersion is the schema version of the version 1 journal,
// which recorded only failed teardowns. It is migrated on the first tick. A
// file carrying any version other than 1 or journalVersion is treated as
// corrupt rather than best-effort decoded: an older shape can be perfectly
// valid JSON that decodes into zero-valued entries (a device with an empty
// kind and name deletes nothing and then clears itself), which would
// silently discard exactly the cleanup the journal exists to guarantee.
const pendingTeardownsVersion = 1

// persistedPendingTeardowns is the on-disk shape of a version 1 journal.
// Both lists are slices, not maps, because their natural keys are structs
// and encoding/json requires string map keys.
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
	if err := validateNetworkEntries(p.Network); err != nil {
		return err
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

func validateNetworkEntries(entries []pendingNetworkDevice) error {
	for i, entry := range entries {
		switch entry.Kind {
		case networkDeviceTAP, networkDeviceBridge:
		default:
			return fmt.Errorf("network entry %d has unknown device kind %q", i, entry.Kind)
		}
		if entry.Name == "" {
			return fmt.Errorf("network entry %d has an empty device name", i)
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
	r := &Reconciler{
		vmManager:             vmManager,
		healthMon:             healthMon,
		networkMgr:            networkMgr,
		logger:                logger,
		updateStrategy:        updateStrategy,
		updateDelay:           updateDelay,
		pendingRecovery:       make(map[string]struct{}),
		allocations:           make(map[uint64]allocation),
		hostPortHolders:       make(map[int]string),
		routeWithdrawals:      make(map[routeKey]routeWithdrawal),
		pendingNetworkDevices: make(map[networkDeviceKey]pendingNetworkDevice),
		// Without a state directory nothing survives a restart, so this
		// reconciler's records are the whole history.
		journalReady: true,
		history:      journalHistory{Complete: true, InitializedAt: time.Now().UTC()},
		journalWrite: func(path string, data []byte) error { return atomicWriteFile(path, data, 0o644) },
		sleepFn: func(ctx context.Context, d time.Duration) error {
			select {
			case <-time.After(d):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	r.installRecoveryCleanupRecorder()
	return r
}

// pendingTeardownsFile is the name, under stateDir, of the ownership journal:
// allocation records, pending device cleanup and ownership history.
const pendingTeardownsFile = "pending_teardowns.json"

// WithStateDir enables persisting the ownership journal to
// <dir>/pending_teardowns.json, so records and pending cleanup survive an
// agent restart. Loads any existing file immediately; a version 1 file is
// migrated on the first tick, once the VM inventory is known, and a missing
// file is created then. If the file exists but cannot be read or parsed, this
// sets journalCorrupt instead of silently continuing with an empty set — see
// its doc comment. Returns r so it can be chained onto
// New/NewWithNetworkManager; a Reconciler that never calls this keeps its
// records in memory only (the default in tests).
func (r *Reconciler) WithStateDir(dir string) *Reconciler {
	r.cleanupMu.Lock()
	r.loadJournalLocked(dir)
	initialize := dir != "" && !r.journalReady && !r.journalCorrupt
	r.cleanupMu.Unlock()
	if initialize {
		r.initializeJournal()
	}
	return r
}

func (r *Reconciler) loadJournalLocked(dir string) *Reconciler {
	r.stateDir = dir
	if dir == "" {
		return r
	}
	r.journalReady, r.history, r.portsUntrackedPending = false, journalHistory{}, false
	path := filepath.Join(dir, pendingTeardownsFile)
	fail := func(msg string, err error) *Reconciler {
		r.logger.Error(msg+"; this node will not report converged until it is resolved manually", "path", path, "error", err)
		r.journalCorrupt = true
		return r
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			// A read failure other than "file does not exist" (permission
			// denied, I/O error) cannot be distinguished from "a journal
			// existed and is now unreadable", so it gets the same fail-closed
			// treatment as a parse failure below.
			return fail("ownership journal unreadable", err)
		}
		return r
	}
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return fail("ownership journal is corrupt", err)
	}
	// Strict decoding: an unknown field means this file was written by a
	// different schema than the one being decoded into, which is exactly the
	// case where a permissive decode yields plausible-looking zero values.
	// Decode reads exactly one JSON value and stops, so a file like
	// {"version":1}{"network":[...]} would otherwise be accepted as an empty
	// journal and silently drop everything after the first value.
	decode := func(into any) error {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(into); err != nil {
			return err
		}
		if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
			return fmt.Errorf("trailing content after the journal: %v", err)
		}
		return nil
	}
	if header.Version == pendingTeardownsVersion {
		var legacy persistedPendingTeardowns
		if err := decode(&legacy); err != nil {
			return fail("ownership journal is corrupt", err)
		}
		if err := legacy.validate(); err != nil {
			return fail("ownership journal is not valid", err)
		}
		r.legacyJournal = &legacy
		return r
	}
	var loaded persistedJournal
	if err := decode(&loaded); err != nil {
		return fail("ownership journal is corrupt", err)
	}
	if err := loaded.validate(); err != nil {
		return fail("ownership journal is not valid", err)
	}
	for _, a := range loaded.Allocations {
		r.allocations[a.ID] = a
	}
	for _, entry := range loaded.Network {
		r.pendingNetworkDevices[entry.key()] = entry
	}
	for port, service := range loaded.HostPorts {
		n, _ := strconv.Atoi(port)
		r.hostPortHolders[n] = service
	}
	for _, w := range loaded.RouteWithdrawals {
		r.routeWithdrawals[w.key()] = w
	}
	r.nextAllocationID, r.history, r.journalReady = loaded.NextID-1, loaded.History, true
	if r.networkMgr == nil && !r.history.PortsUntracked {
		// Without host networking, VMs use host ports this journal does not
		// record holders for, and peers may keep sending a port's traffic
		// to its service after it is gone. The interval is recorded before
		// anything starts, so a later agent that manages host networking
		// treats a port without a recorded holder as having unknown history.
		r.history.PortsUntracked, r.portsUntrackedPending, r.journalReady = true, true, false
	}
	return r
}

// WithHostHistoryEvidence names the rest of the host state that may show
// earlier firework use when the journal is created: DNAT rules toward the
// guest subnet, tagged or not, and local route files. History is complete
// only if there are none. Call it before WithStateDir. An empty or invalid
// subnet counts every DNAT rule.
func (r *Reconciler) WithHostHistoryEvidence(guestSubnet string, localRoutes func() (bool, error)) *Reconciler {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	r.guestSubnet = nil
	if _, subnet, err := net.ParseCIDR(guestSubnet); err == nil {
		r.guestSubnet = subnet
	}
	r.localRoutes = localRoutes
	return r
}

// WithJournalWriter replaces how the journal file is written. It exists for
// tests, which inject failures and crashes at chosen writes or skip fsync;
// production uses the default atomic, fsynced replacement.
func (r *Reconciler) WithJournalWriter(write func(path string, data []byte) error) *Reconciler {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	r.journalWrite = write
	return r
}

// initializeJournal creates the journal at construction when it is missing
// or from version 1, so recovery can record a cleared VM before the first
// tick. A failure, such as an unreadable rule list, is retried on every tick
// while new networked allocations wait.
func (r *Reconciler) initializeJournal() {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	if err := r.initializeJournalLocked(r.vmManager.List()); err != nil {
		r.logger.Warn("ownership records could not be initialized yet; retrying on the next tick", "error", err)
	}
}

// errJournalCorrupt is returned by persistJournal instead of writing when
// journalCorrupt is set. Overwriting an unreadable journal with a fresh but
// incomplete one would erase the evidence an operator needs to diagnose what
// was actually pending before the corruption — so this is a deliberate
// refusal to write, not a best-effort attempt. Callers that merely clear
// completed entries can ignore it (Reconcile already surfaces the corruption
// on every tick); the write-ahead paths must not, because creating or
// removing a VM whose records cannot be written is precisely the crash
// window that journal exists to close.
var errJournalCorrupt = errors.New("ownership journal is corrupt or unreadable; manual recovery required")

// errJournalNotReady is returned by persistJournal before the first tick has
// initialized the journal.
var errJournalNotReady = errors.New("ownership records are not initialized yet")

// persistJournal writes the ownership journal to stateDir as one atomic
// replacement of the file. A no-op returning nil when stateDir is unset.
// Returns an error — rather than only logging one — so the write-ahead paths
// can refuse to act on what they cannot durably record.
func (r *Reconciler) persistJournal() error {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	return r.persistJournalLocked()
}

func (r *Reconciler) persistJournalLocked() error {
	if r.stateDir == "" {
		return nil
	}
	if r.journalCorrupt {
		return errJournalCorrupt
	}
	if !r.journalReady {
		return errJournalNotReady
	}
	persisted := persistedJournal{Version: journalVersion, NextID: r.nextAllocationID + 1, History: r.history,
		Allocations: r.sortedAllocationsLocked()}
	if len(r.hostPortHolders) > 0 {
		persisted.HostPorts = make(map[string]string, len(r.hostPortHolders))
		for port, service := range r.hostPortHolders {
			persisted.HostPorts[strconv.Itoa(port)] = service
		}
	}
	for _, w := range r.routeWithdrawals {
		persisted.RouteWithdrawals = append(persisted.RouteWithdrawals, w)
	}
	sort.Slice(persisted.RouteWithdrawals, func(i, j int) bool {
		a, b := persisted.RouteWithdrawals[i], persisted.RouteWithdrawals[j]
		return a.Service+"/"+a.GuestIP < b.Service+"/"+b.GuestIP
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
		return fmt.Errorf("marshalling ownership journal: %w", err)
	}
	if err := os.MkdirAll(r.stateDir, 0o755); err != nil {
		return fmt.Errorf("creating state dir for ownership journal: %w", err)
	}
	if err := r.journalWrite(filepath.Join(r.stateDir, pendingTeardownsFile), data); err != nil {
		return fmt.Errorf("writing ownership journal: %w", err)
	}
	return nil
}

// logPersistFailure reports a journal write that failed while merely clearing
// entries whose cleanup already succeeded or advancing a record. That is not
// fatal: every host operation the journal drives (DeleteTAP, DeleteBridge,
// port-forward teardown) is idempotent, and records only move forward, so a
// stale entry surviving to the next tick costs one redundant step and nothing
// else. errJournalCorrupt is filtered out
// because Reconcile already surfaces that condition on every single tick.
func (r *Reconciler) logPersistFailure(err error) {
	if err == nil || errors.Is(err, errJournalCorrupt) {
		return
	}
	r.logger.Warn("failed to persist the ownership journal", "error", err)
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
	held := r.HeldServices(desired)
	var actions []Action

	// Build a set of desired service names for quick lookup.
	desiredSet := make(map[string]config.ServiceConfig, len(desired.Services))
	for _, svc := range desired.Services {
		desiredSet[svc.Name] = svc
	}

	// Check for services that need to be created or updated.
	for _, svc := range desired.Services {
		if _, blocked := held[svc.Name]; blocked {
			continue
		}
		inst, exists := actual[svc.Name]
		if !exists {
			actions = append(actions, Action{Type: ActionCreate, Service: svc})
			continue
		}
		if RetainsRuntime(inst.State) {
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
		if RetainsRuntime(inst.State) {
			continue
		}
		if _, desired := desiredSet[name]; !desired {
			actions = append(actions, Action{Type: ActionDelete, Service: inst.Config})
		}
	}

	return actions
}

// Apply executes the list of reconciliation actions.
// Uses rolling strategy if configured, otherwise applies all at once.
func (r *Reconciler) Apply(ctx context.Context, actions []Action) error {
	// Recovery commands may quarantine an instance between planning and apply.
	actual := r.vmManager.List()
	filtered := make([]Action, 0, len(actions))
	for _, action := range actions {
		if instance := actual[action.Service.Name]; instance != nil && instance.State == vm.StateRecoveryPending {
			continue
		}
		filtered = append(filtered, action)
	}
	actions = filtered
	if r.updateStrategy == "rolling" {
		return r.applyRolling(ctx, actions)
	}
	return r.applyAllAtOnce(ctx, actions)
}

// applyAllAtOnce applies all actions without rolling-update delays. Deletes
// run first, as in a rolling apply, so a create can take the port, address or
// capacity a removed service released in the same tick.
func (r *Reconciler) applyAllAtOnce(ctx context.Context, actions []Action) error {
	var errs []error

	ordered := make([]Action, 0, len(actions))
	for _, action := range actions {
		if action.Type == ActionDelete {
			ordered = append(ordered, action)
		}
	}
	for _, action := range actions {
		if action.Type != ActionDelete {
			ordered = append(ordered, action)
		}
	}
	for _, action := range ordered {
		if hold := r.actionHold(action); hold.Reason != "" {
			errs = append(errs, hold.err(action.Service.Name))
			continue
		}
		switch action.Type {
		case ActionCreate:
			r.logger.Info("creating service", "service", action.Service.Name)
			if err := r.createService(ctx, action.Service, nil); err != nil {
				r.logger.Error("failed to create service", "service", action.Service.Name, "error", err)
				errs = append(errs, fmt.Errorf("create %s: %w", action.Service.Name, err))
			}

		case ActionUpdate:
			r.logger.Info("updating service (stop + start)", "service", action.Service.Name)
			rejections, err := r.preflight(ctx, action.Service)
			if err != nil {
				r.logger.Error("volume preflight failed; keeping current VM running", "service", action.Service.Name, "error", err)
				errs = append(errs, stageError(FailureStageVM, fmt.Errorf("preflight update %s: %w", action.Service.Name, err)))
				continue
			}
			if r.settleRejections(&action, rejections) {
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
			if err := r.createService(ctx, action.Service, &prev); err != nil {
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
		if hold := r.actionHold(action); hold.Reason != "" {
			errs = append(errs, hold.err(action.Service.Name))
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
		if hold := r.actionHold(action); hold.Reason != "" {
			errs = append(errs, hold.err(action.Service.Name))
			continue
		}
		r.logger.Info("creating service", "service", action.Service.Name)
		if err := r.createService(ctx, action.Service, nil); err != nil {
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
		if hold := r.actionHold(action); hold.Reason != "" {
			errs = append(errs, hold.err(action.Service.Name))
			continue
		}
		// A health restart may have run during an earlier pause. Replace only
		// a VM that still needs this update. One the restart left without a
		// VM keeps the revision incomplete, so the next tick re-plans it from
		// real state instead of the revision being applied without it.
		instance := r.vmManager.List()[action.Service.Name]
		if instance == nil {
			errs = append(errs, Hold{Reason: "VM disappeared during the rolling update"}.err(action.Service.Name))
			continue
		}
		if !needsUpdate(instance, action.Service) {
			continue
		}
		r.logger.Info("updating service (stop + start)", "service", action.Service.Name)
		rejections, err := r.preflight(ctx, action.Service)
		if err != nil {
			r.logger.Error("volume preflight failed; keeping current VM running", "service", action.Service.Name, "error", err)
			errs = append(errs, stageError(FailureStageVM, fmt.Errorf("preflight update %s: %w", action.Service.Name, err)))
			break
		}
		if r.settleRejections(&action, rejections) {
			continue
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
		if err := r.createService(ctx, action.Service, &prev); err != nil {
			errs = append(errs, fmt.Errorf("update %s: %w", action.Service.Name, err))
			if errors.Is(err, ErrServiceHeld) {
				// Its start waits for its old rules; the rest of the rollout
				// does not depend on it.
				r.logger.Info("start held during update", "service", action.Service.Name, "error", err)
				continue
			}
			r.logger.Error("failed to start service during update", "service", action.Service.Name, "error", err)
			break
		}

		// Sleep between updates, but not after the last one.
		if i < len(updates)-1 && r.updateDelay > 0 {
			if err := r.pause(ctx); err != nil {
				return stageError(FailureStageVM, fmt.Errorf("rolling update interrupted: %w", err))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("reconciliation had %d error(s): %w", len(errs), errors.Join(errs...))
	}
	return nil
}

func (r *Reconciler) preflight(ctx context.Context, svc config.ServiceConfig) ([]volume.Rejection, error) {
	if manager, ok := r.vmManager.(volumePreflighter); ok {
		return manager.Preflight(ctx, svc)
	}
	return nil, nil
}

// settleRejections applies a preflight refusal to a planned update and reports
// whether the update is now a no-op.
//
// This is what makes an advisory preflight rejection *terminal*. The VM is
// still live at this point, and once the clamp lands the desired configuration
// no longer differs from the running one, so no further update is planned —
// not merely no further failure. Without it the measurement repeats on every
// tick forever.
func (r *Reconciler) settleRejections(action *Action, rejections []volume.Rejection) bool {
	if !clampRejected(&action.Service, rejections) {
		return false
	}
	instance := r.vmManager.List()[action.Service.Name]
	if instance == nil || needsUpdate(instance, action.Service) {
		// Something else about the service still differs — an image change,
		// for instance — so the update proceeds, now carrying the effective
		// volume size and performing no resize.
		return false
	}
	r.logger.Info("volume size request refused; keeping the effective size and skipping the update",
		"service", action.Service.Name)
	return true
}

// clampRejected substitutes the effective size for every volume the preflight
// refused, and reports whether anything changed.
//
// The refusal is applied by clamping rather than by failing the update, so an
// update that changes the image *and* requests a refused shrink still deploys
// the image. Failing the preflight would wedge every unrelated change behind a
// size the node will never accept.
func clampRejected(svc *config.ServiceConfig, rejections []volume.Rejection) bool {
	if len(rejections) == 0 {
		return false
	}
	byID := make(map[string]volume.Rejection, len(rejections))
	for _, rejection := range rejections {
		byID[rejection.LogicalID] = rejection
	}
	clamped := false
	for i := range svc.Volumes {
		rejection, ok := byID[svc.Name+"/"+svc.Volumes[i].Name]
		if !ok {
			continue
		}
		if svc.Volumes[i].SizeBytes == rejection.AppliedSizeBytes &&
			svc.Volumes[i].ResizeGeneration == rejection.AppliedGeneration {
			continue
		}
		// Substitute the whole effective configuration. Clamping only the size
		// leaves the generation differing forever, and needsUpdate compares
		// whole volume configs — so the update would be re-planned on every
		// tick, which is worse than the single failure this replaces.
		svc.Volumes[i].SizeBytes = rejection.AppliedSizeBytes
		svc.Volumes[i].ResizeGeneration = rejection.AppliedGeneration
		clamped = true
	}
	return clamped
}

type pauseReleaseKey struct{}

// WithPauseRelease returns a context whose rolling-update pauses call release
// before sleeping and the function it returns afterwards. A caller that
// serializes reconciliation with health-driven restarts releases its lock this
// way, so a restart waits for at most one update rather than the whole
// rollout. Each update re-checks its hold and target after the pause.
func WithPauseRelease(ctx context.Context, release func() (reacquire func())) context.Context {
	return context.WithValue(ctx, pauseReleaseKey{}, release)
}

func (r *Reconciler) pause(ctx context.Context) error {
	if release, ok := ctx.Value(pauseReleaseKey{}).(func() func()); ok && release != nil {
		reacquire := release()
		defer reacquire()
	}
	return r.sleepFn(ctx, r.updateDelay)
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
			fmt.Errorf("ownership journal at %s is corrupt or unreadable; manual recovery required before this node can report convergence",
				filepath.Join(r.stateDir, pendingTeardownsFile))))
	}
	// Settle the records before recovery and planning, so planning sees
	// finished and interrupted starts. Teardown waits for the pass after the
	// apply, which reports its errors.
	if err := r.settleAllocations(ctx, false); err != nil && !r.ownershipReady() {
		errs = append(errs, err)
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
	r.planned = &desired
	defer func() { r.planned = nil }()
	held := r.holds(desired)
	for _, name := range slices.Sorted(maps.Keys(held)) {
		errs = append(errs, held[name].err(name))
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
	if err := r.commitInPlaceChanges(desired, held); err != nil {
		errs = append(errs, err)
	}
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
	if err := r.retryPendingTeardowns(ctx); err != nil {
		errs = append(errs, err)
	}
	return combineErrors(errs)
}

func (r *Reconciler) ownershipReady() bool {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	return r.journalReady
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
		// A service still waiting to be created has no guest to forward to.
		// A held update keeps its running VM, which still owns these rules.
		if running[svc.Name] == nil || running[svc.Name].State != vm.StateRunning {
			continue
		}
		// A rejected or failed update may leave the old configuration running.
		// Program only the guest and ports that actually own this runtime.
		svc = running[svc.Name].Config
		if svc.Network == nil {
			continue
		}
		if len(svc.PortForwards) > 0 {
			// Record the VM before installing anything for it.
			if err := r.ensureRunningAllocation(svc); err != nil {
				errs = append(errs, stageError(FailureStageNetwork, fmt.Errorf("recording port forwards for %s: %w", svc.Name, err)))
				continue
			}
		}
		for _, pf := range svc.PortForwards {
			if err := r.networkMgr.SetupPortForward(svc.Name, pf.HostPort, svc.Network.GuestIP, pf.VMPort); err != nil {
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
	held := r.HeldServices(desired)
	var errs []error
	for name := range r.pendingRecovery {
		if _, blocked := held[name]; blocked {
			continue
		}
		service, wanted := desiredByName[name]
		instance := actual[name]
		if instance == nil && r.healthMon != nil {
			r.healthMon.Deregister(name)
		}
		if !wanted || instance == nil || instance.State == vm.StateRecoveryPending || needsUpdate(instance, service) {
			delete(r.pendingRecovery, name)
			continue
		}
		if r.networkMgr != nil {
			if err := r.networkMgr.Setup(service); err != nil {
				errs = append(errs, stageError(FailureStageNetwork, fmt.Errorf("restore network for adopted %s: %w", name, err)))
				continue
			}
			if service.Network != nil {
				if err := r.ensureRunningAllocation(service); err != nil {
					errs = append(errs, stageError(FailureStageNetwork, fmt.Errorf("recording port forwards for adopted %s: %w", name, err)))
					continue
				}
				failed := false
				for _, forward := range service.PortForwards {
					if err := r.networkMgr.SetupPortForward(service.Name, forward.HostPort, service.Network.GuestIP, forward.VMPort); err != nil {
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
//
// previous is the configuration an update replaces, or nil for a create.
func (r *Reconciler) createService(ctx context.Context, svc config.ServiceConfig, previous *config.ServiceConfig) error {
	// The definitive start gate, after any old VM of the service is gone: a
	// failed teardown of its rules, or anything another tick left, holds the
	// start until it is resolved.
	keepsAddress := previous != nil && sameGuestIP(guestIPOf(*previous), guestIPOf(svc))
	if hold := r.startHold(svc, false, nil, keepsAddress); hold.Reason != "" {
		return hold.err(svc.Name)
	}
	var allocationID uint64
	if r.networkMgr != nil {
		// Write-ahead, for the same reason deleteService journals before
		// removing a VM: a create that fails partway leaves host devices
		// behind that nothing else will ever notice. A service whose Start
		// never succeeded is absent from vmManager.List(), so if it is then
		// dropped from desired state Plan emits no delete action for it and
		// the devices are orphaned — and a crash between Setup and Start
		// leaves them with no record at all. Recording the devices and the
		// VM's address and port forwards before any of them is created closes
		// both windows: whatever exists on the host afterwards is already
		// accounted for, and a crash at any step recovers without an operator.
		id, err := r.journalCreate(svc)
		if err != nil {
			return stageError(FailureStageNetwork,
				fmt.Errorf("recording the network allocation for %s before creating it: %w", svc.Name, err))
		}
		allocationID = id
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
		// than discarded. A failed start normally leaves no claimant, but an
		// identity failure can retain an unprovable process as recovery_pending.
		// In that case its host resources must stay in place until ownership is
		// resolved; Manager.List is the authoritative claimant set used by the
		// retry paths as well.
		_, retained := r.vmManager.List()[svc.Name]
		if r.networkMgr != nil && !retained {
			if allocationID != 0 {
				r.logPersistFailure(r.setAllocationPhase(allocationID, phaseDeparting))
			}
			if tdErr := r.teardownAndTrackNetwork(svc); tdErr != nil {
				r.logger.Warn("failed to roll back networking after VM start failure; will retry",
					"service", svc.Name, "error", tdErr)
			}
			r.logPersistFailure(r.persistJournal())
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
			if err := r.networkMgr.SetupPortForward(svc.Name, pf.HostPort, svc.Network.GuestIP, pf.VMPort); err != nil {
				r.logger.Warn("failed to setup port forward; will be retried by port-forward sync",
					"service", svc.Name, "host_port", pf.HostPort, "error", err)
			}
		}
	}
	if allocationID != 0 {
		// A lost write is repaired by settleAllocations, which promotes a
		// creating record whose VM is running.
		r.logPersistFailure(r.setAllocationPhase(allocationID, phaseRunning))
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
		// settleAllocations and retryPendingNetworkDevices.
		return stageError(FailureStageVM, fmt.Errorf("remove VM: %w", err))
	}

	if r.healthMon != nil {
		r.healthMon.Deregister(svc.Name)
	}

	// An unknown owner may use any device or rule. Leave svc's cleanup in the
	// journal written above; retryPendingTeardowns finishes it once that
	// ownership resolves.
	if unknown := unknownRuntimeOwner(r.vmManager.List()); unknown != "" {
		r.logger.Info("deferring host cleanup until runtime claims are known",
			"service", svc.Name, "unknown_owner", unknown)
		return nil
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
	if err := r.teardownDeparting(svc.Name); err != nil {
		errs = append(errs, err)
	}
	if err := r.teardownAndTrackNetwork(svc); err != nil {
		errs = append(errs, err)
	}
	r.logPersistFailure(r.persistJournal())
	if len(errs) > 0 {
		r.logger.Warn("failed to tear down host resources; will retry", "service", svc.Name, "error", combineErrors(errs))
	}
	return nil
}

// journalIntendedTeardowns marks svc's records departing and records its
// devices, and durably persists that before the caller removes the VM. The
// records authorize deleting svc's rules; each is removed again once none of
// its rules remains, so the steady-state journal holds only live VMs.
//
// On a write failure the in-memory records are restored to exactly what they
// held before, which is also exactly what the journal file still holds:
// atomicWriteFile leaves the previous file untouched when it fails, so
// rolling back in memory restores agreement between the two.
func (r *Reconciler) journalIntendedTeardowns(svc config.ServiceConfig) error {
	if r.networkMgr == nil {
		return nil
	}
	return r.mutateJournal(func() {
		r.markDepartingLocked(svc)
		r.addPendingNetworkDevices(svc)
	})
}

func (r *Reconciler) addPendingNetworkDevices(svc config.ServiceConfig) {
	for _, dev := range networkDevicesForService(svc) {
		r.pendingNetworkDevices[dev] = pendingNetworkDevice{ServiceName: svc.Name, Kind: dev.kind, Name: dev.name}
	}
}

// clearJournalledNetworkDevices drops svc's device entries once the running VM
// has taken ownership of them, so the journal returns to empty in steady state.
func (r *Reconciler) clearJournalledNetworkDevices(svc config.ServiceConfig) {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	changed := false
	for _, dev := range networkDevicesForService(svc) {
		if _, pending := r.pendingNetworkDevices[dev]; pending {
			delete(r.pendingNetworkDevices, dev)
			changed = true
		}
	}
	if changed {
		r.logPersistFailure(r.persistJournalLocked())
	}
}

// teardownAndTrackNetwork deletes each of svc's host network devices
// individually rather than through NetworkManager.Teardown, so that a
// failure on one component leaves only that component pending: a bridge that
// refuses to go away must not keep a successfully-deleted tap on the retry
// list, and vice versa. Each device is tracked by its own (kind, name)
// identity, so a success clears whichever generation of svc.Name originally
// recorded it. A no-op if there is no network manager or no network config.
func (r *Reconciler) teardownAndTrackNetwork(svc config.ServiceConfig) error {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
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

// retryPendingTeardowns settles the allocation records, which tears down
// departing ones, and retries pending device deletions. Anything still
// pending after this pass keeps being returned as an error, so a tick cannot
// be reported converged while a teardown is still failing outright.
//
// This method runs from Reconcile. HasPendingCleanup also forces that path
// when the revision is unchanged: an uncertain owner can retain an entry
// without a teardown failure, and its cleanup must resume once it resolves.
func (r *Reconciler) retryPendingTeardowns(ctx context.Context) error {
	var errs []error
	if err := r.settleAllocations(ctx, true); err != nil {
		errs = append(errs, err)
	}
	if err := r.retryPendingNetworkDevices(); err != nil {
		errs = append(errs, err)
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
// whenever *any* currently running service's config claims it. Uncertain
// owners retain their cleanup intent without removing their host devices.
// Checking every running service rather than only entry.ServiceName is deliberate: an
// explicit network.interface can be carried across a config change that alters
// the rest of the service's networking, so the device that is obsolete for one
// generation may be the live device of another. Deleting a claimed device
// would break a running VM while reporting a successful reconciliation.
func (r *Reconciler) retryPendingNetworkDevices() error {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	if r.networkMgr == nil || len(r.pendingNetworkDevices) == 0 {
		return nil
	}
	// Links share one host namespace: a pending bridge name may now be a
	// running VM's explicit TAP name, and deleting by name would remove it.
	claimed := make(map[string]string)
	retained := make(map[string]bool)
	actual := r.vmManager.List()
	if unknown := unknownRuntimeOwner(actual); unknown != "" {
		return fmt.Errorf("cleanup held by unknown runtime claims for %s: %w", unknown, ErrRecoveryHeld)
	}
	for name, inst := range actual {
		for _, dev := range networkDevicesForService(inst.Config) {
			claimed[dev.name] = name
			retained[dev.name] = retained[dev.name] || RetainsRuntime(inst.State)
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
		if owner, live := claimed[key.name]; live {
			if retained[key.name] {
				continue // A partial removal must retain its durable cleanup intent.
			}
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
		r.logPersistFailure(r.persistJournalLocked())
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
