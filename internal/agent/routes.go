package agent

import (
	"fmt"
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/traefik"
)

type runtimeRouteSyncer interface {
	SyncRuntime([]traefik.RuntimeRoute, []config.NodeConfig, bool) error
}

// Peer discovery precedes this lifecycle transaction. Each persisted local
// projection is committed by the same atomic write that publishes its route.
func (a *Agent) syncRuntimeRoutes(services []config.ServiceConfig, remoteNodes []config.NodeConfig, localOnly bool) error {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	snapshot := a.reconciler.RuntimeRoutes(services)
	if publisher, ok := a.traefikMgr.(runtimeRouteSyncer); ok {
		routes := make([]traefik.RuntimeRoute, 0, len(snapshot))
		for _, route := range snapshot {
			routes = append(routes, traefik.RuntimeRoute{Service: route.Service, InstanceID: route.InstanceID, RetainPublished: route.RetainPublished})
		}
		return publisher.SyncRuntime(routes, remoteNodes, localOnly)
	}
	// Legacy adapters may publish fresh snapshots, but cannot preserve a held
	// projection without owner-bound publication support.
	selected := make([]config.ServiceConfig, 0, len(snapshot))
	for _, route := range snapshot {
		if route.RetainPublished {
			return fmt.Errorf("route publisher cannot retain runtime projection for %s", route.Service.Name)
		}
		selected = append(selected, route.Service)
	}
	if localOnly {
		return a.traefikMgr.SyncLocal(selected)
	}
	return a.traefikMgr.Sync(selected, remoteNodes)
}
