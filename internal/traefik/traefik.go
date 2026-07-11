// Package traefik manages Traefik dynamic configuration files for the
// firework agent. The agent uses Traefik's file provider: it writes a
// small YAML file per service when a service is created and deletes it
// when the service is removed. Traefik watches the directory and picks
// up changes without a reload.
package traefik

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/ingress"
	"gopkg.in/yaml.v3"
)

// stageDirName is the staging subdirectory inside the watched config dir
// (guaranteeing same-filesystem renames without needing write access to the
// config dir's parent). Staged files carry the stageSuffix while incomplete:
// Traefik's file provider recurses into subdirectories but only parses
// .toml/.yaml/.yml files, so suffixed files are never read mid-write.
const stageDirName = ".firework-traefik-stage"

// stageSuffix keeps a staged file invisible to Traefik's extension filter
// until it is renamed to its final .yaml name.
const stageSuffix = ".tmp"

// remoteFilePrefix marks route files for services running on peer nodes.
const remoteFilePrefix = "remote-"

// Manager writes and deletes Traefik dynamic config files.
type Manager struct {
	configDir     string
	ingressDomain string
	logger        *slog.Logger
}

// NewManager creates a Manager that writes files to configDir. ingressDomain is
// the deployment-owned DNS suffix used to resolve metadata.subdomain into a
// public hostname; it may be empty when only exact metadata.host is used.
func NewManager(configDir, ingressDomain string, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{configDir: filepath.Clean(configDir), ingressDomain: ingressDomain, logger: logger}
}

// Sync brings the Traefik dynamic config directory in line with services.
//
// Local routes are strict: invalid routing metadata, an unroutable local
// service, or two local services resolving to the same hostname fail the sync
// without touching the filesystem, so the caller can refuse to treat the
// revision as applied. Remote routes never fail the sync: a peer entry that
// cannot be rendered or that conflicts with an already-claimed hostname is
// skipped with a warning, and conflicts resolve deterministically — local
// services first, then peers in node-name order, services in name order.
// Genuine duplicates are rejected earlier by enricher input validation; agent
// side conflicts are transient reschedule windows that converge once the
// controller removes the stale peer config.
//
// Files are written to a staging directory on the same filesystem and renamed
// into place, so a watcher never observes a partially written document. Stale
// files are removed only after every desired file has been renamed
// successfully; if application fails partway through, no stale file is deleted
// and the next tick converges.
func (m *Manager) Sync(services []config.ServiceConfig, remoteNodes []config.NodeConfig) error {
	desired, owner, err := m.renderLocal(services)
	if err != nil {
		return err
	}
	m.renderRemote(desired, owner, remoteNodes)
	return m.apply(desired, false)
}

// SyncLocal applies routes for local services only and leaves every existing
// remote-* route file untouched. The agent uses it when the peer node configs
// cannot be listed: last-known-good remote routes are preserved (synchronizing
// against an empty peer set would delete valid routes) while local routes stay
// current.
func (m *Manager) SyncLocal(services []config.ServiceConfig) error {
	desired, _, err := m.renderLocal(services)
	if err != nil {
		return err
	}
	return m.apply(desired, true)
}

// renderLocal resolves and validates local routes into a filename -> YAML
// document map plus the hostname -> owner claims made so far. It returns an
// error (mutating nothing) for invalid routing metadata, an unroutable
// service, or a duplicate hostname among local services.
func (m *Manager) renderLocal(services []config.ServiceConfig) (map[string][]byte, map[string]string, error) {
	desired := make(map[string][]byte)
	owner := make(map[string]string) // final hostname -> service identity

	// Local services: proxy to the VM guest IP.
	for _, svc := range services {
		host, err := ingress.Resolve(svc.Name, svc.Metadata, m.ingressDomain)
		if err != nil {
			return nil, nil, err
		}
		if host == "" {
			continue
		}
		if svc.Network == nil || svc.Network.GuestIP == "" {
			return nil, nil, fmt.Errorf("service %s requests routing (host %q) but has no resolved guest network", svc.Name, host)
		}
		port := backendPort(svc)
		if port == 0 {
			return nil, nil, fmt.Errorf("service %s requests routing (host %q) but has no usable backend port", svc.Name, host)
		}
		identity := "local service " + svc.Name
		if prev, ok := owner[host]; ok {
			return nil, nil, fmt.Errorf("duplicate Traefik hostname %q requested by %s and %s", host, prev, identity)
		}
		owner[host] = identity
		data, err := marshalConfig(svc.Name, host, svc.Network.GuestIP, port)
		if err != nil {
			return nil, nil, fmt.Errorf("marshaling traefik config for %s: %w", svc.Name, err)
		}
		desired[configFileName(svc.Name)] = data
	}

	return desired, owner, nil
}

// renderRemote adds routes for peer-node services to desired. Peer configs are
// read from the store and treated as authoritative but never fatal: entries
// that cannot be rendered (unresolvable metadata on this agent, no host port)
// or that lose a hostname/filename claim are skipped with a warning. Peers are
// visited in node-name order and services in name order, so the outcome is
// deterministic for a given input set.
func (m *Manager) renderRemote(desired map[string][]byte, owner map[string]string, remoteNodes []config.NodeConfig) {
	nodes := append([]config.NodeConfig(nil), remoteNodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Node < nodes[j].Node })

	for _, nc := range nodes {
		services := append([]config.ServiceConfig(nil), nc.Services...)
		sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })

		for _, svc := range services {
			host, err := ingress.Resolve(svc.Name, svc.Metadata, m.ingressDomain)
			if err != nil {
				m.logger.Warn("skipping remote route with unresolvable metadata",
					"service", svc.Name, "node", nc.Node, "error", err)
				continue
			}
			if host == "" {
				continue
			}
			if nc.HostIP == "" {
				// Peer host IP not yet resolved; cannot proxy, so no route is
				// rendered (and any previous one is removed as stale). The next
				// tick converges once the registry knows the IP.
				continue
			}
			if len(svc.PortForwards) == 0 || svc.PortForwards[0].HostPort == 0 {
				m.logger.Warn("skipping remote route without a host port to proxy through",
					"service", svc.Name, "node", nc.Node, "host", host)
				continue
			}
			identity := "remote service " + svc.Name + " on node " + nc.Node
			if prev, ok := owner[host]; ok {
				m.logger.Warn("skipping remote route for already-claimed hostname",
					"host", host, "kept", prev, "skipped", identity)
				continue
			}
			filename := remoteConfigFileName(svc.Name)
			if _, ok := desired[filename]; ok {
				m.logger.Warn("skipping remote route with conflicting file name",
					"file", filename, "skipped", identity)
				continue
			}
			data, err := marshalConfig(svc.Name, host, nc.HostIP, svc.PortForwards[0].HostPort)
			if err != nil {
				m.logger.Warn("skipping remote route that failed to marshal",
					"service", svc.Name, "node", nc.Node, "error", err)
				continue
			}
			owner[host] = identity
			desired[filename] = data
		}
	}
}

// apply writes the desired documents through a staging directory and removes
// stale files only after every desired file has been renamed into place. When
// preserveRemotes is set, existing remote-* files are exempt from the stale
// cleanup (used when the peer set is unknown).
func (m *Manager) apply(desired map[string][]byte, preserveRemotes bool) error {
	if err := os.MkdirAll(m.configDir, 0755); err != nil {
		return fmt.Errorf("creating traefik config dir: %w", err)
	}

	stageDir := filepath.Join(m.configDir, stageDirName)
	if err := os.MkdirAll(stageDir, 0755); err != nil {
		return fmt.Errorf("creating traefik staging dir: %w", err)
	}
	// Removing the whole staging dir also collects leftovers from a previous
	// crashed run. Traefik never reads it: staged files carry stageSuffix,
	// which its extension filter skips.
	defer os.RemoveAll(stageDir)

	// Stage everything before renaming anything, in deterministic order: a
	// write failure (e.g. disk full) then aborts with zero renames applied.
	names := make([]string, 0, len(desired))
	for n := range desired {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		if err := os.WriteFile(filepath.Join(stageDir, name+stageSuffix), desired[name], 0644); err != nil {
			return fmt.Errorf("staging traefik config %s: %w", name, err)
		}
	}
	for _, name := range names {
		// Rename within the same filesystem is atomic; a watcher sees either the
		// old complete file or the new complete file, never a partial write.
		if err := os.Rename(filepath.Join(stageDir, name+stageSuffix), filepath.Join(m.configDir, name)); err != nil {
			return fmt.Errorf("applying traefik config %s: %w", name, err)
		}
	}

	// Remove stale files only after every desired rename has succeeded.
	entries, err := os.ReadDir(m.configDir)
	if err != nil {
		return fmt.Errorf("reading traefik config dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		if _, ok := desired[entry.Name()]; ok {
			continue
		}
		if preserveRemotes && strings.HasPrefix(entry.Name(), remoteFilePrefix) {
			continue
		}
		if err := os.Remove(filepath.Join(m.configDir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing stale traefik config %s: %w", entry.Name(), err)
		}
	}

	return nil
}

// --- Traefik dynamic config YAML structures ---

type fileConfig struct {
	HTTP httpSection `yaml:"http"`
}

type httpSection struct {
	Routers  map[string]routerDef  `yaml:"routers"`
	Services map[string]serviceDef `yaml:"services"`
}

type routerDef struct {
	Rule        string   `yaml:"rule"`
	EntryPoints []string `yaml:"entryPoints"`
	Service     string   `yaml:"service"`
}

type serviceDef struct {
	LoadBalancer lbDef `yaml:"loadBalancer"`
}

type lbDef struct {
	Servers []serverDef `yaml:"servers"`
}

type serverDef struct {
	URL string `yaml:"url"`
}

func marshalConfig(name, host, guestIP string, port int) ([]byte, error) {
	cfg := fileConfig{
		HTTP: httpSection{
			Routers: map[string]routerDef{
				name: {
					Rule:        fmt.Sprintf("Host(`%s`)", host),
					EntryPoints: []string{"web"},
					Service:     name,
				},
			},
			Services: map[string]serviceDef{
				name: {
					LoadBalancer: lbDef{
						Servers: []serverDef{
							{URL: fmt.Sprintf("http://%s:%d", guestIP, port)},
						},
					},
				},
			},
		},
	}
	return yaml.Marshal(cfg)
}

// backendPort returns the VM-side port Traefik should proxy to.
// Prefers the first port forward's VM port; falls back to the health check port.
func backendPort(svc config.ServiceConfig) int {
	if len(svc.PortForwards) > 0 {
		return svc.PortForwards[0].VMPort
	}
	if svc.HealthCheck != nil && svc.HealthCheck.Port > 0 {
		return svc.HealthCheck.Port
	}
	return 0
}

func configFileName(serviceName string) string {
	return serviceName + ".yaml"
}

func remoteConfigFileName(serviceName string) string {
	return remoteFilePrefix + serviceName + ".yaml"
}
