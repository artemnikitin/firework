package reconciler

import (
	"fmt"
	"maps"
	"reflect"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/vm"
)

// LocalRoute is the configuration a local route is published from: that of the
// VM that actually owns the service's backend, with in-place changes such as
// hostname or health-check port edits already committed to it. Current reports
// whether its routing metadata is the desired one; a held update keeps the
// previous hostname until the VM is replaced.
type LocalRoute struct {
	Service config.ServiceConfig
	Current bool
}

// LocalRoutes selects route sources from tracked VMs rather than from
// allocations that desired state requests but a hold may still prevent. Every
// tracked VM keeps its reserved guest address whatever its state, so its route
// cannot reach another guest. A VM whose configuration is unknown names no
// trustworthy backend and publishes nothing. Callers serialize publication
// with VM replacement so a selected address cannot be reassigned meanwhile.
func (r *Reconciler) LocalRoutes(desired []config.ServiceConfig) []LocalRoute {
	actual := r.vmManager.List()
	routes := make([]LocalRoute, 0, len(desired))
	for _, service := range desired {
		instance := actual[service.Name]
		if instance == nil || instance.RecoveryConfigUnknown {
			continue
		}
		routes = append(routes, LocalRoute{Service: instance.Config, Current: maps.Equal(instance.Config.Metadata, service.Metadata)})
	}
	return routes
}

type configCommitter interface {
	CommitConfig(name string, svc config.ServiceConfig) error
}

// commitInPlaceChanges records desired fields that apply without replacing
// the VM, such as routing metadata and health checks, on running instances
// whose update is not held. Routes and health restarts read the instance, so
// they follow these edits, including after an agent restart.
func (r *Reconciler) commitInPlaceChanges(desired config.NodeConfig, held map[string]Hold) error {
	committer, ok := r.vmManager.(configCommitter)
	if !ok {
		return nil
	}
	actual := r.vmManager.List()
	var errs []error
	for _, service := range desired.Services {
		instance := actual[service.Name]
		if _, blocked := held[service.Name]; blocked || instance == nil || instance.State != vm.StateRunning ||
			needsUpdate(instance, service) || reflect.DeepEqual(instance.Config, service) {
			continue
		}
		if err := committer.CommitConfig(service.Name, service); err != nil {
			errs = append(errs, stageError(FailureStageVM, fmt.Errorf("record in-place changes for %s: %w", service.Name, err)))
		}
	}
	return combineErrors(errs)
}
