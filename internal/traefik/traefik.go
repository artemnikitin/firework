// Package traefik manages Traefik dynamic configuration files for the
// firework agent. The agent uses Traefik's file provider: it writes a
// small YAML file per service when a service is created and deletes it
// when the service is removed. Traefik watches the directory and picks
// up changes without a reload.
package traefik

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/ingress"
	"gopkg.in/yaml.v3"
)

// stageDirName is the staging subdirectory (a sibling of the watched config
// dir, on the same filesystem) where complete files are written before being
// atomically renamed into place.
const stageDirName = ".firework-traefik-stage"

// Manager writes and deletes Traefik dynamic config files.
type Manager struct {
	configDir     string
	ingressDomain string
}

// NewManager creates a Manager that writes files to configDir. ingressDomain is
// the deployment-owned DNS suffix used to resolve metadata.subdomain into a
// public hostname; it may be empty when only exact metadata.host is used.
func NewManager(configDir, ingressDomain string) *Manager {
	return &Manager{configDir: configDir, ingressDomain: ingressDomain}
}

// Sync brings the Traefik dynamic config directory in line with services.
//
// It resolves the complete local and remote route set first, rendering every
// intended YAML document in memory and rejecting invalid routing metadata and
// duplicate final hostnames before touching the filesystem. Files are written
// to a staging directory on the same filesystem and renamed into place, so a
// watcher never observes a partially written document. Stale files are removed
// only after every desired file has been renamed successfully; if application
// fails partway through, no stale file is deleted and the next tick converges.
func (m *Manager) Sync(services []config.ServiceConfig, remoteNodes []config.NodeConfig) error {
	desired, err := m.render(services, remoteNodes)
	if err != nil {
		return err
	}
	return m.apply(desired)
}

// render resolves and validates the full route set and returns the desired
// filename -> YAML document map. It returns an error (mutating nothing) for
// invalid routing metadata, an unroutable service, or a duplicate hostname.
func (m *Manager) render(services []config.ServiceConfig, remoteNodes []config.NodeConfig) (map[string][]byte, error) {
	desired := make(map[string][]byte)
	owner := make(map[string]string) // final hostname -> service identity

	claim := func(host, identity string) error {
		if prev, ok := owner[host]; ok {
			return fmt.Errorf("duplicate Traefik hostname %q requested by %s and %s", host, prev, identity)
		}
		owner[host] = identity
		return nil
	}

	// Local services: proxy to the VM guest IP.
	for _, svc := range services {
		host, err := ingress.Resolve(svc.Name, svc.Metadata, m.ingressDomain)
		if err != nil {
			return nil, err
		}
		if host == "" {
			continue
		}
		if svc.Network == nil || svc.Network.GuestIP == "" {
			return nil, fmt.Errorf("service %s requests routing (host %q) but has no resolved guest network", svc.Name, host)
		}
		port := backendPort(svc)
		if port == 0 {
			return nil, fmt.Errorf("service %s requests routing (host %q) but has no usable backend port", svc.Name, host)
		}
		if err := claim(host, "local service "+svc.Name); err != nil {
			return nil, err
		}
		data, err := marshalConfig(svc.Name, host, svc.Network.GuestIP, port)
		if err != nil {
			return nil, fmt.Errorf("marshaling traefik config for %s: %w", svc.Name, err)
		}
		desired[configFileName(svc.Name)] = data
	}

	// Remote services: proxy to the peer node's host IP + forwarded host port.
	for _, nc := range remoteNodes {
		for _, svc := range nc.Services {
			host, err := ingress.Resolve(svc.Name, svc.Metadata, m.ingressDomain)
			if err != nil {
				return nil, err
			}
			if host == "" {
				continue
			}
			if nc.HostIP == "" {
				// Peer host IP not yet resolved; cannot proxy. Skip without
				// deleting any existing route — last-known-good is preserved and
				// the next tick converges once the IP is known.
				continue
			}
			if len(svc.PortForwards) == 0 || svc.PortForwards[0].HostPort == 0 {
				return nil, fmt.Errorf("remote service %s (node %s) requests routing (host %q) but has no host port to proxy through", svc.Name, nc.Node, host)
			}
			if err := claim(host, "remote service "+svc.Name+" on node "+nc.Node); err != nil {
				return nil, err
			}
			data, err := marshalConfig(svc.Name, host, nc.HostIP, svc.PortForwards[0].HostPort)
			if err != nil {
				return nil, fmt.Errorf("marshaling remote traefik config for %s: %w", svc.Name, err)
			}
			desired[remoteConfigFileName(svc.Name)] = data
		}
	}

	return desired, nil
}

// apply writes the desired documents through a staging directory and removes
// stale files only after every desired file has been renamed into place.
func (m *Manager) apply(desired map[string][]byte) error {
	if err := os.MkdirAll(m.configDir, 0755); err != nil {
		return fmt.Errorf("creating traefik config dir: %w", err)
	}

	stageDir := filepath.Join(filepath.Dir(m.configDir), stageDirName)
	if err := os.MkdirAll(stageDir, 0755); err != nil {
		return fmt.Errorf("creating traefik staging dir: %w", err)
	}
	defer os.RemoveAll(stageDir)

	// Apply in deterministic order for predictable behavior and tests.
	names := make([]string, 0, len(desired))
	for n := range desired {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		tmp := filepath.Join(stageDir, name)
		if err := os.WriteFile(tmp, desired[name], 0644); err != nil {
			return fmt.Errorf("staging traefik config %s: %w", name, err)
		}
		// Rename within the same filesystem is atomic; a watcher sees either the
		// old complete file or the new complete file, never a partial write.
		if err := os.Rename(tmp, filepath.Join(m.configDir, name)); err != nil {
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
	return "remote-" + serviceName + ".yaml"
}
