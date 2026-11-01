package reconciler

import (
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/vm"
)

// RuntimeRoute binds a routing candidate to the VM that owns its backend.
type RuntimeRoute struct {
	Service         config.ServiceConfig
	InstanceID      string
	RetainPublished bool
}

// RuntimeRoutes selects local backends from running VMs, rather than from
// allocations that desired state requests but recovery may still prevent.
// Callers serialize publication with VM replacement so a selected address
// cannot be reassigned while its route is being installed.
func (r *Reconciler) RuntimeRoutes(desired []config.ServiceConfig) []RuntimeRoute {
	held := r.HeldServices(config.NodeConfig{Services: desired})
	actual := r.vmManager.List()
	routes := make([]RuntimeRoute, 0, len(desired))
	for _, service := range desired {
		instance := actual[service.Name]
		if instance == nil || instance.State != vm.StateRunning {
			continue
		}
		route := instance.Config
		_, blocked := held[service.Name]
		retain := blocked || needsUpdate(instance, service)
		if !retain {
			// Hostname metadata can change without recreating a VM. Its backend
			// still comes from the configuration that actually owns the guest.
			route.Metadata = service.Metadata
			// Health-check ports are also routing metadata when no forward
			// supplies a backend. They can change without recreating the VM.
			route.HealthCheck = service.HealthCheck
		}
		routes = append(routes, RuntimeRoute{Service: route, InstanceID: instance.InstanceID, RetainPublished: retain})
	}
	return routes
}
