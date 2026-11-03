package reconciler

import (
	"fmt"
	"maps"
	"net"
	"slices"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/vm"
)

// Hold explains why a desired service's work is deferred. Recovery marks holds
// that unresolved VM ownership caused: those alone let a revision apply as
// degraded, because pending recovery explains them and keeps retrying them.
type Hold struct {
	Reason   string
	Recovery bool
	// Admission is the reason code of a hold waiting cannot resolve, such as
	// desired services that exceed node capacity. It fails the revision.
	Admission string
	// Capacity marks a hold made by capacity admission, which re-evaluates
	// the service on every admission pass.
	Capacity bool
}

func (h Hold) err(service string) error {
	sentinel := ErrServiceHeld
	if h.Recovery {
		sentinel = ErrRecoveryHeld
	}
	return fmt.Errorf("hold %s (%s): %w", service, h.Reason, sentinel)
}

// SetHeldServices replaces the caller's capacity and address holds. Telemetry
// may read them concurrently with the serialized reconciliation loop.
func (r *Reconciler) SetHeldServices(held map[string]Hold) {
	r.heldMu.Lock()
	defer r.heldMu.Unlock()
	r.heldServices = maps.Clone(held)
}

// RetainsRuntime reports whether a VM in this state keeps its resources until
// its lifecycle or ownership resolves, whatever this tick's plan says.
func RetainsRuntime(state vm.State) bool {
	return state == vm.StateRecoveryPending || state == vm.StateStarting || state == vm.StateStartAborting || state == vm.StateStopping
}

// Instances returns the runtime inventory the reconciler plans against, so
// admission decisions read the same view as holds.
func (r *Reconciler) Instances() map[string]*vm.Instance {
	return r.vmManager.List()
}

// RetainedServices returns the actual configurations whose resources cannot
// safely be reused yet. Desired configuration may differ from these claims.
func (r *Reconciler) RetainedServices() []config.ServiceConfig {
	var retained []config.ServiceConfig
	for _, instance := range r.retainedInstances() {
		retained = append(retained, instance.Config)
	}
	return retained
}

func (r *Reconciler) retainedInstances() []*vm.Instance {
	return retainedIn(r.vmManager.List())
}

func retainedIn(actual map[string]*vm.Instance) []*vm.Instance {
	var retained []*vm.Instance
	for _, instance := range actual {
		if RetainsRuntime(instance.State) {
			retained = append(retained, instance)
		}
	}
	slices.SortFunc(retained, func(a, b *vm.Instance) int {
		if a.Config.Name < b.Config.Name {
			return -1
		}
		if a.Config.Name > b.Config.Name {
			return 1
		}
		return 0
	})
	return retained
}

// Holds returns the holds of the desired services if external were the
// caller's capacity and address holds, with every hold they cause. It ignores
// what SetHeldServices installed, so admission can weigh a hold set without
// publishing it.
func (r *Reconciler) Holds(desired config.NodeConfig, external map[string]Hold) map[string]Hold {
	return HoldsIn(r.vmManager.List(), desired, external)
}

// HoldsIn is Holds over the given inventory instead of the current one, so
// admission can read one snapshot throughout, or weigh what would hold if
// part of the runtime were gone.
func HoldsIn(actual map[string]*vm.Instance, desired config.NodeConfig, external map[string]Hold) map[string]Hold {
	unknown := unknownRuntimeOwner(actual)
	owners := retainedIn(actual)
	held := make(map[string]Hold)
	// A tracked VM whose update is held keeps its actual guest IP, MAC, host
	// ports and devices, so it is an owner for every other service. That
	// includes a crashed or stopped VM, whose DNAT rules stay installed until
	// it is replaced. Holding one service can therefore hold another; iterate
	// until nothing changes.
	for changed := true; changed; {
		changed = false
		for _, service := range desired.Services {
			if _, done := held[service.Name]; done {
				continue
			}
			hold := serviceHold(service, actual, owners, unknown, external, held)
			if hold.Reason == "" {
				continue
			}
			held[service.Name] = hold
			changed = true
			if instance := actual[service.Name]; instance != nil && !RetainsRuntime(instance.State) {
				owners = append(owners, instance) // Retained VMs already are.
			}
		}
	}
	return held
}

// DeleteHolds returns, for tracked VMs that desired state no longer names,
// why their removal must wait: a host claim a retained VM shares with them.
// Such a VM keeps running, and keeps its resources, until the hold clears.
func (r *Reconciler) DeleteHolds(desired config.NodeConfig) map[string]Hold {
	return DeleteHoldsIn(r.vmManager.List(), desired)
}

// DeleteHoldsIn is DeleteHolds over the given inventory.
func DeleteHoldsIn(actual map[string]*vm.Instance, desired config.NodeConfig) map[string]Hold {
	wanted := make(map[string]bool, len(desired.Services))
	for _, service := range desired.Services {
		wanted[service.Name] = true
	}
	held := make(map[string]Hold)
	retained := retainedIn(actual)
	for name, instance := range actual {
		if wanted[name] || RetainsRuntime(instance.State) {
			continue
		}
		if hold := deleteHold(instance, retained); hold.Reason != "" {
			held[name] = hold
		}
	}
	return held
}

func deleteHold(instance *vm.Instance, retained []*vm.Instance) Hold {
	for _, owner := range retained {
		if owner.Config.Name == instance.Config.Name {
			continue
		}
		if resource := conflictingResource(instance.Config, owner.Config); resource != "" {
			return Hold{Reason: fmt.Sprintf("%s retained by %s", resource, owner.Config.Name), Recovery: owner.State == vm.StateRecoveryPending}
		}
	}
	return Hold{}
}

// HeldServices identifies desired services whose work must wait for retained
// runtime resources or capacity. Quarantine itself remains service scoped;
// independent services can still converge while these holds are retried.
func (r *Reconciler) HeldServices(desired config.NodeConfig) map[string]string {
	reasons := make(map[string]string)
	for name, hold := range r.holds(desired) {
		reasons[name] = hold.Reason
	}
	return reasons
}

// holds returns the holds of the desired services under the installed
// capacity and address holds.
func (r *Reconciler) holds(desired config.NodeConfig) map[string]Hold {
	r.heldMu.RLock()
	external := maps.Clone(r.heldServices)
	r.heldMu.RUnlock()
	return r.Holds(desired, external)
}

func serviceHold(service config.ServiceConfig, actual map[string]*vm.Instance, owners []*vm.Instance, unknown string, external, held map[string]Hold) Hold {
	instance := actual[service.Name]
	// Independent admission failures remain blockers even when another VM's
	// unknown claims or this service's quarantine also prevent its update.
	if hold, ok := external[service.Name]; ok && !hold.Recovery {
		return hold
	}
	if instance != nil && instance.State == vm.StateRecoveryPending {
		return Hold{} // Its quarantine already reports why no desired work applies.
	}
	if instance != nil && RetainsRuntime(instance.State) {
		return Hold{Reason: "runtime lifecycle is in progress"}
	}
	changes := instance == nil || needsUpdate(instance, service)
	if unknown != "" {
		// Unknown claims can collide only with new allocations. A running
		// service that needs no change keeps its network, health and port
		// forwards.
		if changes {
			return Hold{Reason: "runtime resource claims are unknown for " + unknown, Recovery: true}
		}
		return Hold{}
	}
	if hold, ok := external[service.Name]; ok {
		return hold
	}
	if !changes {
		return Hold{} // Nothing new is allocated.
	}
	for _, owner := range owners {
		if owner.Config.Name == service.Name {
			continue
		}
		resource := conflictingResource(service, owner.Config)
		// An update also removes the old runtime's host resources. Protect
		// shared claims even when its desired config no longer names them.
		if resource == "" && instance != nil {
			resource = conflictingResource(instance.Config, owner.Config)
		}
		if resource != "" {
			// A held owner passes on the cause of its own hold, so a chain
			// that starts at a quarantined VM stays a recovery hold.
			recovery := owner.State == vm.StateRecoveryPending || held[owner.Config.Name].Recovery
			return Hold{Reason: fmt.Sprintf("%s retained by %s", resource, owner.Config.Name), Recovery: recovery}
		}
	}
	return Hold{}
}

func conflictingResource(service, owner config.ServiceConfig) string {
	if service.Network != nil && owner.Network != nil {
		a, b := service.Network, owner.Network
		if a.GuestIP != "" && b.GuestIP != "" && (a.GuestIP == b.GuestIP || net.ParseIP(a.GuestIP) != nil && net.ParseIP(a.GuestIP).Equal(net.ParseIP(b.GuestIP))) {
			return "guest IP " + a.GuestIP
		}
		if a.GuestMAC != "" && b.GuestMAC != "" {
			am, ae := net.ParseMAC(a.GuestMAC)
			bm, be := net.ParseMAC(b.GuestMAC)
			if a.GuestMAC == b.GuestMAC || ae == nil && be == nil && slices.Equal(am, bm) {
				return "guest MAC " + a.GuestMAC
			}
		}
	}
	for _, forward := range service.PortForwards {
		for _, retained := range owner.PortForwards {
			if forward.HostPort == retained.HostPort {
				return fmt.Sprintf("host port %d", forward.HostPort)
			}
		}
	}
	for _, device := range networkDevicesForService(service) {
		for _, retained := range networkDevicesForService(owner) {
			// Device names occupy one host namespace, regardless of kind.
			if device.name == retained.name {
				return "network device " + device.name
			}
		}
	}
	return ""
}

// actionHold re-checks one planned action against the runtime as it is now,
// after earlier actions of this tick. Holds are evaluated across the whole
// planned revision, so a held running owner protects its claims here too. A
// create also waits for any other tracked VM that still uses one of its claims, such
// as a service whose update releasing that port comes later in a rolling
// apply; the next tick creates it. A crashed or stopped VM counts too, because
// its host rules stay installed until it is replaced. A create or update that
// would grow its VM also waits unless the capacity is free now; see
// capacityHold.
//
// Each call recomputes the revision's holds against the current runtime, so a
// tick costs on the order of actions × services² claim comparisons. That is
// cheap at the tens of services a node runs; compute holds incrementally if
// nodes grow far beyond that.
func (r *Reconciler) actionHold(action Action) Hold {
	actual := r.vmManager.List()
	var hold Hold
	var plannedHolds map[string]Hold
	if action.Type != ActionDelete {
		planned := config.NodeConfig{Services: []config.ServiceConfig{action.Service}}
		if r.planned != nil {
			planned = *r.planned
			planned.Services = slices.Clone(planned.Services)
			for i := range planned.Services {
				if planned.Services[i].Name == action.Service.Name {
					planned.Services[i] = action.Service
				}
			}
		}
		plannedHolds = r.holds(planned)
		hold = plannedHolds[action.Service.Name]
		if hold.Reason != "" && !hold.Recovery {
			return hold
		}
	}
	if instance := actual[action.Service.Name]; instance != nil && RetainsRuntime(instance.State) {
		return Hold{Reason: "runtime ownership or lifecycle is unresolved", Recovery: instance.State == vm.StateRecoveryPending}
	}
	if action.Type == ActionCreate && hold.Reason == "" {
		retained := r.retainedInstances()
		names := slices.Sorted(maps.Keys(actual))
		for _, name := range names {
			owner := actual[name]
			if name == action.Service.Name {
				continue
			}
			if resource := conflictingResource(action.Service, owner.Config); resource != "" {
				// The owner stays for whatever holds it, so a create waiting
				// on it inherits that cause.
				ownerHold, ownerHeld := plannedHolds[name]
				if !ownerHeld && r.planned != nil && !slices.ContainsFunc(r.planned.Services, func(s config.ServiceConfig) bool { return s.Name == name }) {
					ownerHold = deleteHold(owner, retained)
				}
				recovery := owner.State == vm.StateRecoveryPending || ownerHold.Recovery
				return Hold{Reason: fmt.Sprintf("%s still used by %s", resource, name), Recovery: recovery}
			}
		}
	}
	if action.Type != ActionDelete {
		if hold.Reason == "" {
			hold = r.departedAddressHold(action.Service)
		}
		if hold.Reason == "" {
			hold = r.capacityHold(action.Service, actual)
		}
		return hold
	}
	return deleteHold(&vm.Instance{Config: action.Service}, r.retainedInstances())
}

func unknownRuntimeOwner(actual map[string]*vm.Instance) string {
	var names []string
	for name, instance := range actual {
		if instance.State == vm.StateRecoveryPending && instance.RecoveryConfigUnknown {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// HasPendingRecovery reports whether a tracked VM awaits ownership recovery,
// from the same inventory that holds and admission read.
func (r *Reconciler) HasPendingRecovery() bool {
	for _, instance := range r.vmManager.List() {
		if instance.State == vm.StateRecoveryPending {
			return true
		}
	}
	return false
}

// PendingTeardownGuestIPs returns, for each guest IP that pending cleanup
// still has host rules for, the removed service those rules belong to.
func (r *Reconciler) PendingTeardownGuestIPs() map[string]string {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	ips := make(map[string]string, len(r.pendingPortForwards))
	for _, entry := range r.pendingPortForwards {
		if entry.GuestIP != "" {
			ips[entry.GuestIP] = entry.ServiceName
		}
	}
	return ips
}

// departedAddressHold holds a service whose guest IP still has another
// service's host rule waiting to be torn down: until it is gone, that
// service's host port would forward to this guest. A rule the service itself
// now claims is its own traffic, and the same service may take its own
// address back, since its leftover rules reach only itself.
func (r *Reconciler) departedAddressHold(svc config.ServiceConfig) Hold {
	if svc.Network == nil || svc.Network.GuestIP == "" {
		return Hold{}
	}
	claims := portForwardKeys(svc)
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	for key, entry := range r.pendingPortForwards {
		if _, claimed := claims[key]; key.guestIP == svc.Network.GuestIP && entry.ServiceName != svc.Name && !claimed {
			return Hold{Reason: fmt.Sprintf("guest IP %s still has host port %d of removed service %s awaiting teardown", key.guestIP, key.hostPort, entry.ServiceName)}
		}
	}
	return Hold{}
}

// HasPendingCleanup keeps retained write-ahead cleanup eligible for retry even
// when its owner temporarily prevented teardown and the revision was applied.
func (r *Reconciler) HasPendingCleanup() bool {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	return r.journalCorrupt || len(r.pendingNetworkDevices) != 0 || len(r.pendingPortForwards) != 0
}
