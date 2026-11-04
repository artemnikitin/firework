package reconciler

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/network"
	"github.com/artemnikitin/firework/internal/vm"
)

// allocationPhase is where an allocation record is in its VM's lifecycle.
type allocationPhase string

const (
	// phaseCreating: the agent may be creating devices, starting or
	// restarting the VM, or installing its rules. After a crash the agent
	// recovers the VM or proves it absent, and never needs an operator.
	phaseCreating allocationPhase = "creating"
	// phaseRunning: the VM was started and is tracked.
	phaseRunning allocationPhase = "running"
	// phaseDeparting: the agent is removing the VM. The record authorizes
	// deleting the rules it covers once nothing claims them.
	phaseDeparting allocationPhase = "departing"
)

// allocation records one VM's guest address and port forwards, written before
// anything is created for it and removed only once none of its rules remain.
// While it exists, its address and host ports are held for every other
// service, so the kernel's rules and this record together say whose traffic
// an address or port may still receive.
type allocation struct {
	ID           uint64               `json:"id"`
	Service      string               `json:"service"`
	Phase        allocationPhase      `json:"phase"`
	GuestIP      string               `json:"guest_ip"`
	PortForwards []config.PortForward `json:"port_forwards,omitempty"`
	// Untagged also authorizes deleting untagged rules for exactly these
	// port forwards: rules an agent installed before rules were tagged.
	Untagged bool `json:"untagged,omitempty"`
}

func (a allocation) live() bool { return a.Phase != phaseDeparting }

// covers reports whether this record authorizes deleting rule.
func (a allocation) covers(rule network.PortForwardRule) bool {
	if !sameGuestIP(rule.GuestIP, a.GuestIP) {
		return false
	}
	if rule.TaggedFor(a.Service) {
		return true
	}
	if !a.Untagged || rule.Tagged() {
		return false
	}
	return slices.ContainsFunc(a.PortForwards, func(pf config.PortForward) bool {
		return rule.Targets(pf.HostPort, a.GuestIP, pf.VMPort)
	})
}

func sameGuestIP(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	pa, pb := net.ParseIP(a), net.ParseIP(b)
	return pa != nil && pb != nil && pa.Equal(pb)
}

func guestIPOf(svc config.ServiceConfig) string {
	if svc.Network == nil {
		return ""
	}
	return svc.Network.GuestIP
}

// journalVersion is the schema version of the ownership journal.
const journalVersion = 2

// persistedJournal is the on-disk ownership journal.
type persistedJournal struct {
	Version     int                    `json:"version"`
	NextID      uint64                 `json:"next_id"`
	History     journalHistory         `json:"history"`
	Allocations []allocation           `json:"allocations,omitempty"`
	Network     []pendingNetworkDevice `json:"network,omitempty"`
	// HostPorts maps each host port this node has allocated to the service
	// that first held it. Peers may still send that service's traffic to
	// it, so it is never given to another service (L6).
	HostPorts map[string]string `json:"host_ports,omitempty"`
	// RouteWithdrawals hold addresses whose routes may still be served (L7).
	RouteWithdrawals []routeWithdrawal `json:"route_withdrawals,omitempty"`
}

// journalHistory says how much of this host's ownership history the journal
// holds.
type journalHistory struct {
	// Complete means the journal was created on a host without earlier
	// firework state, so its records are the whole history. A migrated or
	// repaired journal is never complete.
	Complete      bool      `json:"complete"`
	InitializedAt time.Time `json:"initialized_at"`
}

func (p persistedJournal) validate() error {
	if p.Version != journalVersion {
		return fmt.Errorf("unsupported journal version %d (expected %d)", p.Version, journalVersion)
	}
	if p.NextID == 0 {
		return fmt.Errorf("journal has no next allocation id")
	}
	if err := validateNetworkEntries(p.Network); err != nil {
		return err
	}
	for port, service := range p.HostPorts {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port || service == "" {
			return fmt.Errorf("host port holder %q -> %q is invalid", port, service)
		}
	}
	for i, w := range p.RouteWithdrawals {
		everyAddress := w.Service == routeEveryAddress && w.GuestIP == routeEveryAddress
		switch {
		case w.State != routeWithdrawing && w.State != routeWithdrawn:
			return fmt.Errorf("route withdrawal %d has unknown state %q", i, w.State)
		case w.Service == "" || !everyAddress && net.ParseIP(w.GuestIP) == nil:
			return fmt.Errorf("route withdrawal %d names no service or guest IP", i)
		case w.State == routeWithdrawn && (w.BootID == "" || w.ProcessStart == ""):
			return fmt.Errorf("route withdrawal %d is complete without the process that served it", i)
		}
	}
	seen := make(map[uint64]bool, len(p.Allocations))
	for i, a := range p.Allocations {
		switch {
		case a.ID == 0 || a.ID >= p.NextID || seen[a.ID]:
			return fmt.Errorf("allocation %d has an invalid or duplicate id %d", i, a.ID)
		case a.Service == "":
			return fmt.Errorf("allocation %d has no service", i)
		case a.Phase != phaseCreating && a.Phase != phaseRunning && a.Phase != phaseDeparting:
			return fmt.Errorf("allocation %d has unknown phase %q", i, a.Phase)
		case net.ParseIP(a.GuestIP) == nil:
			return fmt.Errorf("allocation %d has invalid guest IP %q", i, a.GuestIP)
		}
		if err := validatePortForwards(a.PortForwards); err != nil {
			return fmt.Errorf("allocation %d: %w", i, err)
		}
		seen[a.ID] = true
	}
	return nil
}

func validatePortForwards(pfs []config.PortForward) error {
	for i, pf := range pfs {
		if pf.HostPort < 1 || pf.HostPort > 65535 {
			return fmt.Errorf("port forward %d has out-of-range host port %d", i, pf.HostPort)
		}
		if pf.VMPort < 1 || pf.VMPort > 65535 {
			return fmt.Errorf("port forward %d has out-of-range VM port %d", i, pf.VMPort)
		}
	}
	return nil
}

// absenceProver proves that no process or queued launch uses a service's VM
// paths. *vm.Manager implements it; a manager without real processes need not.
type absenceProver interface {
	ProveServiceAbsent(ctx context.Context, service string) error
}

var _ absenceProver = (*vm.Manager)(nil)

// newAllocationLocked adds a record and returns it.
func (r *Reconciler) newAllocationLocked(service string, phase allocationPhase, guestIP string, pfs []config.PortForward, untagged bool) allocation {
	r.nextAllocationID++
	a := allocation{ID: r.nextAllocationID, Service: service, Phase: phase, GuestIP: guestIP,
		PortForwards: slices.Clone(pfs), Untagged: untagged}
	r.allocations[a.ID] = a
	for _, pf := range pfs {
		if _, held := r.hostPortHolders[pf.HostPort]; !held {
			r.hostPortHolders[pf.HostPort] = service
		}
	}
	return a
}

// departLocked marks a record departing. A departing record of the same
// service and address absorbs it, so one record covers every generation's
// leftover rules toward that address.
func (r *Reconciler) departLocked(a allocation) {
	for _, other := range r.sortedAllocationsLocked() {
		if other.ID == a.ID || other.Service != a.Service || other.Phase != phaseDeparting || !sameGuestIP(other.GuestIP, a.GuestIP) {
			continue
		}
		for _, pf := range a.PortForwards {
			if !slices.Contains(other.PortForwards, pf) {
				other.PortForwards = append(other.PortForwards, pf)
			}
		}
		other.Untagged = other.Untagged || a.Untagged
		r.allocations[other.ID] = other
		delete(r.allocations, a.ID)
		return
	}
	a.Phase = phaseDeparting
	r.allocations[a.ID] = a
}

// sortedAllocationsLocked returns the records in creation order.
func (r *Reconciler) sortedAllocationsLocked() []allocation {
	return slices.SortedFunc(maps.Values(r.allocations), func(a, b allocation) int {
		return cmp.Compare(a.ID, b.ID)
	})
}

// mutateJournal applies mutate and durably records the result. On a write
// failure every record is restored to what the journal file still holds.
func (r *Reconciler) mutateJournal(mutate func()) error {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	return r.mutateJournalLocked(mutate)
}

func (r *Reconciler) mutateJournalLocked(mutate func()) error {
	prevAllocations := maps.Clone(r.allocations)
	prevNext := r.nextAllocationID
	prevDevices := maps.Clone(r.pendingNetworkDevices)
	prevHolders := maps.Clone(r.hostPortHolders)
	prevRoutes := maps.Clone(r.routeWithdrawals)
	mutate()
	if r.nextAllocationID == prevNext && maps.EqualFunc(prevAllocations, r.allocations, allocationsEqual) &&
		maps.Equal(prevDevices, r.pendingNetworkDevices) && maps.Equal(prevRoutes, r.routeWithdrawals) {
		return nil
	}
	if err := r.persistJournalLocked(); err != nil {
		r.allocations, r.nextAllocationID, r.pendingNetworkDevices, r.hostPortHolders = prevAllocations, prevNext, prevDevices, prevHolders
		r.routeWithdrawals = prevRoutes
		return err
	}
	return nil
}

func allocationsEqual(a, b allocation) bool {
	return a.ID == b.ID && a.Service == b.Service && a.Phase == b.Phase && a.GuestIP == b.GuestIP &&
		a.Untagged == b.Untagged && slices.Equal(a.PortForwards, b.PortForwards)
}

// journalCreate records the VM createService is about to create: its
// address and port forwards and its devices, in one write before any of them
// exists. A live record left for the service by an earlier attempt is marked
// departing, so its rules stay covered.
func (r *Reconciler) journalCreate(svc config.ServiceConfig) (uint64, error) {
	var id uint64
	err := r.mutateJournal(func() {
		r.addPendingNetworkDevices(svc)
		if guestIPOf(svc) == "" {
			return
		}
		for _, a := range r.sortedAllocationsLocked() {
			if a.Service == svc.Name && a.live() {
				r.departLocked(a)
			}
		}
		id = r.newAllocationLocked(svc.Name, phaseCreating, guestIPOf(svc), svc.PortForwards, false).ID
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// setAllocationPhase moves one record to phase.
func (r *Reconciler) setAllocationPhase(id uint64, phase allocationPhase) error {
	return r.mutateJournal(func() {
		a, ok := r.allocations[id]
		switch {
		case !ok:
		case phase == phaseDeparting:
			r.departLocked(a)
		default:
			a.Phase = phase
			r.allocations[id] = a
		}
	})
}

// journalRestart moves the record of the VM a health restart replaces to
// creating, adding one if the VM has none.
func (r *Reconciler) journalRestart(svc config.ServiceConfig) (uint64, error) {
	if r.networkMgr == nil || guestIPOf(svc) == "" {
		return 0, nil
	}
	var id uint64
	err := r.mutateJournal(func() {
		a, ok := r.liveAllocationLocked(svc.Name, guestIPOf(svc))
		if !ok {
			a = r.newAllocationLocked(svc.Name, phaseCreating, guestIPOf(svc), svc.PortForwards, true)
		}
		a.Phase = phaseCreating
		r.allocations[a.ID] = a
		id = a.ID
	})
	return id, err
}

// liveAllocationLocked returns the service's live record for guestIP.
func (r *Reconciler) liveAllocationLocked(service, guestIP string) (allocation, bool) {
	for _, a := range r.sortedAllocationsLocked() {
		if a.Service == service && a.live() && sameGuestIP(a.GuestIP, guestIP) {
			return a, true
		}
	}
	return allocation{}, false
}

// ensureRunningAllocation records a running VM that has no record yet, such
// as one from before records existed, before any of its rules is installed.
func (r *Reconciler) ensureRunningAllocation(svc config.ServiceConfig) error {
	if guestIPOf(svc) == "" {
		return nil
	}
	return r.mutateJournal(func() {
		if _, ok := r.liveAllocationLocked(svc.Name, guestIPOf(svc)); !ok {
			r.newAllocationLocked(svc.Name, phaseRunning, guestIPOf(svc), svc.PortForwards, true)
		}
	})
}

// markDepartingLocked marks every live record of svc departing. If none
// records svc's own address, a departing record is added from svc, whose
// configuration is trusted, so its rules stay covered.
func (r *Reconciler) markDepartingLocked(svc config.ServiceConfig) {
	covered := false
	for _, a := range r.sortedAllocationsLocked() {
		if a.Service != svc.Name || !a.live() {
			continue
		}
		r.departLocked(a)
		covered = covered || sameGuestIP(a.GuestIP, guestIPOf(svc))
	}
	if !covered && guestIPOf(svc) != "" {
		r.departLocked(r.newAllocationLocked(svc.Name, phaseDeparting, guestIPOf(svc), svc.PortForwards, true))
	}
}

// ruleClaim reports whether a tracked VM's configuration claims rule's exact
// forward, and whether every such VM is still retained.
func ruleClaim(rule network.PortForwardRule, actual map[string]*vm.Instance) (claimed, retained bool) {
	retained = true
	for _, inst := range actual {
		if !sameGuestIP(guestIPOf(inst.Config), rule.GuestIP) {
			continue
		}
		for _, pf := range inst.Config.PortForwards {
			if pf.HostPort == rule.HostPort && pf.VMPort == rule.VMPort {
				claimed = true
				retained = retained && RetainsRuntime(inst.State)
			}
		}
	}
	return claimed, claimed && retained
}

// teardownDepartingLocked deletes the rules a departing record covers that no
// tracked VM claims, and removes the record once none of its rules remains.
// A rule a tracked VM claims is that VM's traffic and is kept; while only a
// retained VM claims it, the record stays as the cleanup intent.
func (r *Reconciler) teardownDepartingLocked(a allocation, listing network.PortForwardListing, actual map[string]*vm.Instance) (removed bool, err error) {
	remaining := 0
	var errs []error
	for _, rule := range listing.Rules {
		if !a.covers(rule) {
			continue
		}
		if claimed, retained := ruleClaim(rule, actual); claimed {
			if retained {
				remaining++
			}
			continue
		}
		if err := r.networkMgr.DeletePortForwardRule(rule); err != nil {
			remaining++
			errs = append(errs, stageError(FailureStageNetwork,
				fmt.Errorf("teardown port forward %d for %s: %w", rule.HostPort, a.Service, err)))
		}
	}
	if remaining > 0 {
		return false, errors.Join(errs...)
	}
	delete(r.allocations, a.ID)
	return true, nil
}

// teardownDeparting tears down the service's departing records now, after its
// VM was removed.
func (r *Reconciler) teardownDeparting(service string) error {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	var departing []allocation
	for _, a := range r.sortedAllocationsLocked() {
		if a.Service == service && a.Phase == phaseDeparting {
			departing = append(departing, a)
		}
	}
	if len(departing) == 0 {
		return nil
	}
	listing, err := r.networkMgr.ListPortForwards()
	if err != nil {
		return stageError(FailureStageNetwork, fmt.Errorf("teardown port forwards for %s: %w", service, err))
	}
	actual := r.vmManager.List()
	var errs []error
	changed := false
	for _, a := range departing {
		removed, err := r.teardownDepartingLocked(a, listing, actual)
		changed = changed || removed
		if err != nil {
			errs = append(errs, err)
		}
	}
	if changed {
		r.logPersistFailure(r.persistJournalLocked())
	}
	return errors.Join(errs...)
}

// settleAllocations brings the records in line with the VMs: a finished start
// becomes running, an interrupted start or restart whose VM is proven absent
// becomes departing, a tracked VM without a record gets one, and, when
// teardown is set, departing records are torn down. It initializes the
// journal if construction could not.
func (r *Reconciler) settleAllocations(ctx context.Context, teardown bool) error {
	if r.networkMgr == nil {
		// Routes still reach guest addresses without host networking.
		if err := r.CompleteRouteWithdrawals(); err != nil {
			r.logger.Warn("route withdrawals cannot be settled yet", "error", err)
		}
		return nil
	}
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	actual := r.vmManager.List()
	if err := r.initializeJournalLocked(actual); err != nil {
		return stageError(FailureStageNetwork, err)
	}
	if r.journalCorrupt {
		return nil // Reconcile reports the corruption; nothing may be recorded.
	}
	before := maps.Clone(r.allocations)
	beforeNext := r.nextAllocationID
	beforeHolders := maps.Clone(r.hostPortHolders)
	beforeRoutes := maps.Clone(r.routeWithdrawals)
	for _, a := range r.sortedAllocationsLocked() {
		inst := actual[a.Service]
		tracked := inst != nil && sameGuestIP(guestIPOf(inst.Config), a.GuestIP)
		if a.Phase != phaseCreating {
			continue
		}
		if tracked {
			if inst.State == vm.StateRunning {
				a.Phase = phaseRunning
				r.allocations[a.ID] = a
			}
			continue
		}
		if inst != nil {
			if RetainsRuntime(inst.State) {
				continue // Its lifecycle or ownership is unresolved; it may still use these paths.
			}
			// The service's tracked VM is another allocation. Both would use
			// the same paths, so this record's VM was never launched or was
			// replaced.
			r.departLocked(a)
			continue
		}
		if prover, ok := r.vmManager.(absenceProver); ok {
			if err := prover.ProveServiceAbsent(ctx, a.Service); err != nil {
				r.logger.Warn("interrupted start keeps its address until the VM is proven absent",
					"service", a.Service, "guest_ip", a.GuestIP, "error", err)
				continue
			}
		}
		r.departLocked(a)
	}
	for _, name := range slices.Sorted(maps.Keys(actual)) {
		inst := actual[name]
		if inst.RecoveryConfigUnknown || guestIPOf(inst.Config) == "" {
			continue
		}
		if _, ok := r.liveAllocationLocked(name, guestIPOf(inst.Config)); !ok {
			r.newAllocationLocked(name, phaseRunning, guestIPOf(inst.Config), inst.Config.PortForwards, true)
		}
	}
	var errs []error
	var departing []allocation
	for _, a := range r.sortedAllocationsLocked() {
		if a.Phase == phaseDeparting {
			departing = append(departing, a)
		}
	}
	if teardown {
		listing, err := r.networkMgr.ListPortForwards()
		switch {
		case err != nil:
			errs = append(errs, stageError(FailureStageNetwork, fmt.Errorf("listing port forwards for cleanup: %w", err)))
		case len(departing) > 0 && unknownRuntimeOwner(actual) != "":
			errs = append(errs, fmt.Errorf("cleanup held by unknown runtime claims for %s: %w", unknownRuntimeOwner(actual), ErrRecoveryHeld))
		default:
			for _, a := range departing {
				if _, err := r.teardownDepartingLocked(a, listing, actual); err != nil {
					r.logger.Warn("port forward teardown still pending, will retry next tick", "service", a.Service, "error", err)
					errs = append(errs, err)
				}
			}
			if err := r.adoptPortForwards(actual, listing); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := r.settleRoutesLocked(); err != nil {
		r.logger.Warn("route withdrawals cannot be settled yet", "error", err)
	}
	if r.nextAllocationID != beforeNext || !maps.EqualFunc(before, r.allocations, allocationsEqual) || !maps.Equal(beforeRoutes, r.routeWithdrawals) {
		if err := r.persistJournalLocked(); err != nil {
			// Records only ever move forward, so the next tick derives the
			// same changes from the same evidence.
			r.allocations, r.nextAllocationID, r.hostPortHolders = before, beforeNext, beforeHolders
			r.routeWithdrawals = beforeRoutes
			errs = append(errs, stageError(FailureStageNetwork, fmt.Errorf("persisting ownership records: %w", err)))
		}
	}
	return combineErrors(errs)
}

// initializeJournalLocked creates the version 2 journal on the first tick. A
// host without a journal, VM state or tagged rules starts with complete
// history. Anything else is migrated from what is known now, and its history
// stays incomplete.
func (r *Reconciler) initializeJournalLocked(actual map[string]*vm.Instance) error {
	if r.journalReady || r.journalCorrupt {
		return nil
	}
	if r.networkMgr == nil {
		// Without host networking nothing is installed or owned, so there is
		// nothing to record. A version 1 journal stays on disk untouched.
		r.journalReady = true
		return nil
	}
	complete := r.legacyJournal == nil
	if complete && r.stateDir != "" {
		if _, err := os.Lstat(filepath.Join(r.stateDir, "vms")); err == nil || !os.IsNotExist(err) {
			complete = false
		}
	}
	if complete {
		listing, err := r.networkMgr.ListPortForwards()
		if err != nil {
			return fmt.Errorf("listing port forwards to initialize ownership records: %w", err)
		}
		// Any rule an earlier agent may have installed, tagged or not, or a
		// rule whose destination cannot be read, is earlier state.
		complete = len(listing.Unparsed) == 0 && !slices.ContainsFunc(listing.Rules, func(rule network.PortForwardRule) bool {
			ip := net.ParseIP(rule.GuestIP)
			return rule.Tagged() || r.guestSubnet == nil || ip == nil || r.guestSubnet.Contains(ip)
		})
	}
	if complete && r.localRoutes != nil {
		found, err := r.localRoutes()
		if err != nil {
			return fmt.Errorf("checking route files to initialize ownership records: %w", err)
		}
		complete = !found
	}
	prevAllocations, prevNext, prevDevices, prevHolders := maps.Clone(r.allocations), r.nextAllocationID, maps.Clone(r.pendingNetworkDevices), maps.Clone(r.hostPortHolders)
	prevRoutes := maps.Clone(r.routeWithdrawals)
	if r.legacyJournal != nil {
		for _, entry := range r.legacyJournal.PortForwards {
			if a, ok := r.departingLegacyLocked(entry.ServiceName, entry.GuestIP); ok {
				a.PortForwards = append(a.PortForwards, entry.PortForward)
				r.allocations[a.ID] = a
				continue
			}
			r.newAllocationLocked(entry.ServiceName, phaseDeparting, entry.GuestIP, []config.PortForward{entry.PortForward}, true)
		}
		for _, entry := range r.legacyJournal.Network {
			r.pendingNetworkDevices[entry.key()] = entry
		}
	}
	for _, name := range slices.Sorted(maps.Keys(actual)) {
		inst := actual[name]
		if inst.RecoveryConfigUnknown || guestIPOf(inst.Config) == "" {
			continue
		}
		if _, ok := r.liveAllocationLocked(name, guestIPOf(inst.Config)); !ok {
			r.newAllocationLocked(name, phaseRunning, guestIPOf(inst.Config), inst.Config.PortForwards, true)
		}
	}
	if !complete && r.routeEvidence != nil {
		// Routes an earlier agent removed are not known, and a running
		// Traefik may still serve them: every address is held for every
		// other service until that process is gone.
		r.routeWithdrawals[routeKey{routeEveryAddress, routeEveryAddress}] = routeWithdrawal{
			Service: routeEveryAddress, GuestIP: routeEveryAddress, State: routeWithdrawing}
	}
	r.history = journalHistory{Complete: complete, InitializedAt: time.Now().UTC()}
	r.journalReady = true
	if err := r.persistJournalLocked(); err != nil {
		r.allocations, r.nextAllocationID, r.pendingNetworkDevices, r.hostPortHolders = prevAllocations, prevNext, prevDevices, prevHolders
		r.routeWithdrawals = prevRoutes
		r.history, r.journalReady = journalHistory{}, false
		return fmt.Errorf("initializing ownership records: %w", err)
	}
	if r.legacyJournal != nil || !complete {
		r.logger.Warn("ownership records were migrated from existing host state; history before this point is incomplete",
			"state_dir", r.stateDir)
	}
	r.legacyJournal = nil
	return nil
}

func (r *Reconciler) departingLegacyLocked(service, guestIP string) (allocation, bool) {
	for _, a := range r.allocations {
		if a.Service == service && a.Phase == phaseDeparting && a.Untagged && sameGuestIP(a.GuestIP, guestIP) {
			return a, true
		}
	}
	return allocation{}, false
}

// allocationHold holds a networked start whose address or host ports a
// record still holds for another service, or whose own interrupted start is
// not resolved yet. Host ports of another service's running VM are checked
// against the VM itself, so a port can move between services within a tick
// once the old VM's rules are gone.
func (r *Reconciler) allocationHold(svc config.ServiceConfig, leaving map[string]bool) Hold {
	ip := guestIPOf(svc)
	if r.networkMgr == nil || ip == "" {
		return Hold{}
	}
	actual := r.vmManager.List()
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	if r.journalCorrupt {
		return Hold{Reason: "the ownership journal is corrupt, so no new network allocation can be recorded"}
	}
	if !r.journalReady {
		return Hold{Reason: "ownership records are not initialized yet"}
	}
	if hold := r.portHistoryHoldLocked(svc); hold.Reason != "" {
		return hold
	}
	for _, a := range r.sortedAllocationsLocked() {
		inst := actual[a.Service]
		tracked := inst != nil && sameGuestIP(guestIPOf(inst.Config), a.GuestIP)
		if a.Service == svc.Name {
			if a.Phase == phaseCreating && !tracked {
				return Hold{Reason: "an interrupted start of this service is not resolved yet"}
			}
			// No exception for the same service: an earlier VM's rule for a
			// host port this configuration no longer claims would deliver
			// that port's traffic, now meant for another service, here.
			if a.Phase == phaseDeparting && sameGuestIP(a.GuestIP, ip) {
				for _, pf := range a.PortForwards {
					if !slices.Contains(svc.PortForwards, pf) {
						return Hold{Reason: fmt.Sprintf("this service's earlier rule for host port %d toward %s must be torn down first", pf.HostPort, ip)}
					}
				}
			}
			continue
		}
		if leaving[a.Service] && a.Phase == phaseRunning && tracked {
			continue // Its VM goes before any start this tick.
		}
		if sameGuestIP(a.GuestIP, ip) {
			return Hold{Reason: fmt.Sprintf("guest IP %s is still recorded for service %s (%s)", ip, a.Service, a.Phase)}
		}
		if a.Phase == phaseRunning && tracked {
			continue
		}
		for _, pf := range svc.PortForwards {
			if slices.ContainsFunc(a.PortForwards, func(other config.PortForward) bool { return other.HostPort == pf.HostPort }) {
				return Hold{Reason: fmt.Sprintf("host port %d is still recorded for service %s (%s)", pf.HostPort, a.Service, a.Phase)}
			}
		}
	}
	return r.routeHoldLocked(svc.Name, ip)
}

// portHistoryHoldLocked applies L6: a host port this node gave to one service
// is never given to another, because peers' routes and guests' cross-node
// links may still send the first service's traffic to it, and this node
// cannot observe when they stop. With incomplete history, a port without a
// recorded holder may have such a past too. Waiting cannot resolve either.
func (r *Reconciler) portHistoryHoldLocked(svc config.ServiceConfig) Hold {
	for _, pf := range svc.PortForwards {
		holder, held := r.hostPortHolders[pf.HostPort]
		switch {
		case held && holder != svc.Name:
			return Hold{Reason: fmt.Sprintf("host port %d was used by service %s on this node, and peers may still send that service's traffic to it; use a fresh host port, or a node without this history", pf.HostPort, holder),
				Action: ActionHostPortReuseBlocked}
		case !held && !r.history.Complete:
			return Hold{Reason: fmt.Sprintf("this node's ownership history is incomplete, so host port %d may still be referenced for another service; use a host port this node already gives %s, or a node without earlier history", pf.HostPort, svc.Name),
				Action: ActionNetworkHistoryIncomplete}
		}
	}
	return Hold{}
}

// PendingTeardownGuestIPs returns, for each guest IP a record still holds
// while no tracked VM of its service uses it, the service it is recorded for.
func (r *Reconciler) PendingTeardownGuestIPs() map[string]string {
	actual := r.vmManager.List()
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	ips := make(map[string]string)
	for _, a := range r.sortedAllocationsLocked() {
		if inst := actual[a.Service]; inst != nil && sameGuestIP(guestIPOf(inst.Config), a.GuestIP) {
			continue
		}
		ips[a.GuestIP] = a.Service
	}
	return ips
}

// HasPendingCleanup keeps cleanup the agent can finish by itself eligible for
// retry even when the revision was applied: departing records, interrupted
// starts and devices. A record only an operator can resolve does not force
// every tick.
func (r *Reconciler) HasPendingCleanup() bool {
	actual := r.vmManager.List()
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	if r.journalCorrupt || len(r.pendingNetworkDevices) != 0 || (r.networkMgr != nil && !r.journalReady) {
		return true
	}
	for _, a := range r.allocations {
		inst := actual[a.Service]
		if a.Phase == phaseDeparting || a.Phase == phaseCreating && (inst == nil || !sameGuestIP(guestIPOf(inst.Config), a.GuestIP)) {
			return true
		}
	}
	// An incomplete route withdrawal finishes by itself; a completed one
	// waits for a Traefik restart that only an operator starts.
	for _, w := range r.routeWithdrawals {
		if w.State == routeWithdrawing {
			return true
		}
	}
	return false
}
