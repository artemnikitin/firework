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

func retainsRuntime(state vm.State) bool {
	return state == vm.StateRecoveryPending || state == vm.StateStarting || state == vm.StateStartAborting || state == vm.StateStopping
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
	var retained []*vm.Instance
	for _, instance := range r.vmManager.List() {
		if retainsRuntime(instance.State) {
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

func (r *Reconciler) holds(desired config.NodeConfig) map[string]Hold {
	held := make(map[string]Hold)
	retained := r.retainedInstances()
	actual := r.vmManager.List()
	unknown := unknownRuntimeOwner(actual)
	r.heldMu.RLock()
	capacityHolds := maps.Clone(r.heldServices)
	r.heldMu.RUnlock()
	for _, service := range desired.Services {
		if instance := actual[service.Name]; instance != nil && instance.State == vm.StateRecoveryPending {
			continue // Its quarantine already reports why no desired work applies.
		}
		if unknown != "" {
			// Unknown claims can collide only with new allocations. A running
			// service that needs no change keeps its network, health and
			// port forwards.
			if instance := actual[service.Name]; instance == nil || needsUpdate(instance, service) {
				held[service.Name] = Hold{Reason: "runtime resource claims are unknown for " + unknown, Recovery: true}
			}
			continue
		}
		if instance := actual[service.Name]; instance != nil && retainsRuntime(instance.State) && instance.State != vm.StateRecoveryPending {
			held[service.Name] = Hold{Reason: "runtime lifecycle is in progress"}
			continue
		}
		if hold, ok := capacityHolds[service.Name]; ok {
			held[service.Name] = hold
			continue
		}
		for _, owner := range retained {
			if owner.Config.Name == service.Name {
				continue
			}
			resource := conflictingResource(service, owner.Config)
			// An update also removes the old runtime's host resources. Protect
			// shared claims even when its desired config no longer names them.
			if resource == "" && actual[service.Name] != nil {
				resource = conflictingResource(actual[service.Name].Config, owner.Config)
			}
			if resource != "" {
				held[service.Name] = Hold{Reason: fmt.Sprintf("%s retained by %s", resource, owner.Config.Name), Recovery: owner.State == vm.StateRecoveryPending}
				break
			}
		}
	}
	return held
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

func (r *Reconciler) actionHold(action Action) Hold {
	actual := r.vmManager.List()
	// A delete releases resources; deleteService defers its host cleanup to
	// the journal while unknown claims remain.
	if unknown := unknownRuntimeOwner(actual); unknown != "" && action.Type != ActionDelete {
		return Hold{Reason: "runtime resource claims are unknown for " + unknown, Recovery: true}
	}
	if instance := actual[action.Service.Name]; instance != nil && retainsRuntime(instance.State) {
		return Hold{Reason: "runtime ownership or lifecycle is unresolved", Recovery: instance.State == vm.StateRecoveryPending}
	}
	if action.Type != ActionDelete {
		return r.holds(config.NodeConfig{Services: []config.ServiceConfig{action.Service}})[action.Service.Name]
	}
	for _, owner := range r.retainedInstances() {
		if owner.Config.Name != action.Service.Name {
			if resource := conflictingResource(action.Service, owner.Config); resource != "" {
				return Hold{Reason: fmt.Sprintf("%s retained by %s", resource, owner.Config.Name), Recovery: owner.State == vm.StateRecoveryPending}
			}
		}
	}
	return Hold{}
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

// HasPendingCleanup keeps retained write-ahead cleanup eligible for retry even
// when its owner temporarily prevented teardown and the revision was applied.
func (r *Reconciler) HasPendingCleanup() bool {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	return r.journalCorrupt || len(r.pendingNetworkDevices) != 0 || len(r.pendingPortForwards) != 0
}
