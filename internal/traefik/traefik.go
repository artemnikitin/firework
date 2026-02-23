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
	"strings"

	"github.com/artemnikitin/firework/internal/config"
	"gopkg.in/yaml.v3"
)

// Manager writes and deletes Traefik dynamic config files.
type Manager struct {
	configDir string
}

// NewManager creates a Manager that writes files to configDir.
func NewManager(configDir string) *Manager {
	return &Manager{configDir: configDir}
}

// Sync brings the Traefik dynamic config directory in line with services.
// It writes one file per local service that has metadata["host"] and a
// resolved guest IP, and one file per remote service (on peer nodes) that has
// metadata["host"] and a host-port forward. Stale files are removed.
func (m *Manager) Sync(services []config.ServiceConfig, remoteNodes []config.NodeConfig) error {
	if err := os.MkdirAll(m.configDir, 0755); err != nil {
		return fmt.Errorf("creating traefik config dir: %w", err)
	}

	active := make(map[string]bool)

	// Local services: proxy to VM guest IP.
	for _, svc := range services {
		host := svc.Metadata["host"]
		if host == "" {
			continue
		}
		if svc.Network == nil || svc.Network.GuestIP == "" {
			continue
		}

		port := backendPort(svc)
		if port == 0 {
			continue
		}

		filename := configFileName(svc.Name)
		active[filename] = true

		data, err := marshalConfig(svc.Name, host, svc.Network.GuestIP, port)
		if err != nil {
			return fmt.Errorf("marshaling traefik config for %s: %w", svc.Name, err)
		}

		path := filepath.Join(m.configDir, filename)
		if err := os.WriteFile(path, data, 0644); err != nil {
			return fmt.Errorf("writing traefik config for %s: %w", svc.Name, err)
		}
	}

	// Remote services: proxy to peer node's host IP + forwarded port.
	for _, nc := range remoteNodes {
		if nc.HostIP == "" {
			continue
		}
		for _, svc := range nc.Services {
			host := svc.Metadata["host"]
			if host == "" {
				continue
			}
			if len(svc.PortForwards) == 0 {
				continue
			}
			hostPort := svc.PortForwards[0].HostPort

			filename := remoteConfigFileName(svc.Name)
			active[filename] = true

			data, err := marshalConfig(svc.Name, host, nc.HostIP, hostPort)
			if err != nil {
				return fmt.Errorf("marshaling remote traefik config for %s: %w", svc.Name, err)
			}

			path := filepath.Join(m.configDir, filename)
			if err := os.WriteFile(path, data, 0644); err != nil {
				return fmt.Errorf("writing remote traefik config for %s: %w", svc.Name, err)
			}
		}
	}

	// Remove files that no longer correspond to an active service.
	entries, err := os.ReadDir(m.configDir)
	if err != nil {
		return fmt.Errorf("reading traefik config dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		if !active[entry.Name()] {
			path := filepath.Join(m.configDir, entry.Name())
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing stale traefik config %s: %w", entry.Name(), err)
			}
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
