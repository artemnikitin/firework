package reconciler

import (
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/vm"
)

// RoutableServices selects local backends from running VMs, rather than from
// allocations that desired state requests but recovery may still prevent.
// Callers serialize publication with VM replacement so a selected address
// cannot be reassigned while its route is being installed.
func (r *Reconciler) RoutableServices(desired []config.ServiceConfig) []config.ServiceConfig {
	held := r.HeldServices(config.NodeConfig{Services: desired})
	actual := r.vmManager.List()
	routes := make([]config.ServiceConfig, 0, len(desired))
	for _, service := range desired {
		instance := actual[service.Name]
		if instance == nil || instance.State != vm.StateRunning {
			continue
		}
		route := instance.Config
		if _, blocked := held[service.Name]; !blocked && !needsUpdate(instance, service) {
			// Hostname metadata can change without recreating a VM. Its backend
			// still comes from the configuration that actually owns the guest.
			route.Metadata = service.Metadata
			// Health-check ports are also routing metadata when no forward
			// supplies a backend. They can change without recreating the VM.
			route.HealthCheck = service.HealthCheck
		}
		routes = append(routes, route)
	}
	return routes
}
