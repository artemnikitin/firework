package agent

import "github.com/artemnikitin/firework/internal/config"

// syncRuntimeRoutes snapshots and publishes local backends in the same
// lifecycle transaction. Peer discovery happens before this helper so a slow
// store cannot delay health-driven restarts.
func (a *Agent) syncRuntimeRoutes(services []config.ServiceConfig, remoteNodes []config.NodeConfig, localOnly bool) error {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	services = a.reconciler.RoutableServices(services)
	if localOnly {
		return a.traefikMgr.SyncLocal(services)
	}
	return a.traefikMgr.Sync(services, remoteNodes)
}
