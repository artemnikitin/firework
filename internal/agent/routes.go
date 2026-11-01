package agent

import (
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/ingress"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/traefik"
)

// Peer discovery precedes this lifecycle transaction, so a slow store cannot
// delay health-driven restarts, while selecting and publishing the local
// snapshot is serialized with VM replacement.
func (a *Agent) syncRuntimeRoutes(services []config.ServiceConfig, remoteNodes []config.NodeConfig, localOnly bool) error {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	selected := a.selectLocalRoutes(a.reconciler.LocalRoutes(services), services, remoteNodes)
	if localOnly {
		return a.traefikMgr.SyncLocal(selected)
	}
	return a.traefikMgr.Sync(selected, remoteNodes)
}

// selectLocalRoutes publishes each tracked VM from its own configuration. A
// held update keeps its previous hostname only while desired state does not
// give that hostname to another service, local or on a peer: the desired owner
// always wins. A stale route is dropped rather than failing publication, so
// one held service can never freeze every other route on the node.
func (a *Agent) selectLocalRoutes(routes []reconciler.LocalRoute, desired []config.ServiceConfig, remoteNodes []config.NodeConfig) []config.ServiceConfig {
	resolve := func(svc config.ServiceConfig) (string, error) {
		return ingress.Resolve(svc.Name, svc.Metadata, a.cfg.IngressDomain)
	}
	claimed := make(map[string]string)
	for _, svc := range desired {
		if host, err := resolve(svc); err == nil && host != "" {
			claimed[host] = svc.Name
		}
	}
	for _, node := range remoteNodes {
		for _, svc := range node.Services {
			if host, err := resolve(svc); err == nil && host != "" {
				if _, local := claimed[host]; !local {
					claimed[host] = ""
				}
			}
		}
	}
	selected := make([]config.ServiceConfig, 0, len(routes))
	kept := make(map[string]bool)
	for _, route := range routes {
		svc := route.Service
		if route.Current {
			selected = append(selected, svc)
			continue
		}
		host, err := resolve(svc)
		if err != nil {
			a.logger.Warn("dropping held route whose previous metadata no longer resolves", "service", svc.Name, "error", err)
			continue
		}
		if host == "" || kept[host] || svc.Network == nil || svc.Network.GuestIP == "" || traefik.BackendPort(svc) == 0 {
			continue
		}
		// Peer claims are recorded with no local owner, so they always win.
		if owner, taken := claimed[host]; taken && owner != svc.Name {
			a.logger.Info("dropping held route; desired state gives its hostname to another service", "service", svc.Name, "host", host)
			continue
		}
		kept[host] = true
		selected = append(selected, svc)
	}
	return selected
}
