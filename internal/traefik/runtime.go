package traefik

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/ingress"
)

// RuntimeRoute describes a running guest and whether its last published route
// must be retained while its desired update is held. InstanceID is the VM's
// immutable ownership identity, rather than its service name or process ID.
type RuntimeRoute struct {
	Service         config.ServiceConfig
	InstanceID      string
	RetainPublished bool
}

const runtimeRouteHeader = "# firework-runtime-route: "
const runtimeRouteVersion = 1
const maxRuntimeRouteHeaderBytes = 4096

// publishedRuntimeRoute is committed in the same file as the Traefik document.
// Consequently a failed batch or daemon restart can recover the projection
// actually published for each guest, without a second persistence transaction.
type publishedRuntimeRoute struct {
	Version    int    `json:"version"`
	InstanceID string `json:"instance_id"`
	GuestIP    string `json:"guest_ip"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
}

// SyncRuntime publishes routes for running VMs. Held updates reuse only a
// projection previously committed for the exact same VM and guest address.
// A disabled route still has an owner-bound empty document, so a later held
// update cannot resurrect an earlier hostname. localOnly preserves peer files
// when discovery could not establish their current desired set.
func (m *Manager) SyncRuntime(routes []RuntimeRoute, remote []config.NodeConfig, localOnly bool) error {
	desired, owner, err := m.renderRuntime(routes)
	if err != nil {
		return err
	}
	if !localOnly {
		m.renderRemote(desired, owner, remote)
	}
	return m.apply(desired, localOnly)
}

func (m *Manager) renderRuntime(routes []RuntimeRoute) (map[string][]byte, map[string]string, error) {
	desired := make(map[string][]byte)
	owner := make(map[string]string)
	for _, route := range routes {
		svc := route.Service
		guestIP := ""
		if svc.Network != nil {
			guestIP = svc.Network.GuestIP
		}
		projection := publishedRuntimeRoute{Version: runtimeRouteVersion, InstanceID: route.InstanceID, GuestIP: guestIP}
		if route.RetainPublished {
			published, ok, err := m.readPublishedRuntimeRoute(svc.Name)
			if err != nil {
				return nil, nil, err
			}
			if !ok || route.InstanceID == "" || published.InstanceID != route.InstanceID || published.GuestIP != guestIP {
				continue
			}
			projection = published
		} else {
			if route.InstanceID == "" {
				return nil, nil, fmt.Errorf("service %s has no runtime identity for route publication", svc.Name)
			}
			host, err := ingress.Resolve(svc.Name, svc.Metadata, m.ingressDomain)
			if err != nil {
				return nil, nil, err
			}
			projection.Host = host
			if host != "" {
				projection.Port = backendPort(svc)
			}
			if !validRuntimeProjection(svc.Name, projection) {
				return nil, nil, fmt.Errorf("service %s requests routing (host %q) but has no resolved guest network or usable backend port", svc.Name, host)
			}
		}
		if projection.Host != "" {
			identity := "local service " + svc.Name
			if previous, duplicate := owner[projection.Host]; duplicate {
				return nil, nil, fmt.Errorf("duplicate Traefik hostname %q requested by %s and %s", projection.Host, previous, identity)
			}
			owner[projection.Host] = identity
		}
		data, err := marshalRuntimeRoute(svc.Name, projection)
		if err != nil {
			return nil, nil, fmt.Errorf("marshaling runtime route for %s: %w", svc.Name, err)
		}
		desired[configFileName(svc.Name)] = data
	}
	return desired, owner, nil
}

func validRuntimeProjection(service string, projection publishedRuntimeRoute) bool {
	if projection.Version != runtimeRouteVersion || projection.InstanceID == "" {
		return false
	}
	if projection.Host == "" {
		return projection.Port == 0
	}
	host, err := ingress.Resolve(service, map[string]string{ingress.MetadataHost: projection.Host}, "")
	return err == nil && host == projection.Host && projection.GuestIP != "" && projection.Port > 0 && projection.Port <= 65535
}

func marshalRuntimeRoute(service string, projection publishedRuntimeRoute) ([]byte, error) {
	record, err := json.Marshal(projection)
	if err != nil {
		return nil, err
	}
	header := runtimeRouteHeader + base64.RawStdEncoding.EncodeToString(record) + "\n"
	if len(header) > maxRuntimeRouteHeaderBytes {
		return nil, fmt.Errorf("runtime route ownership header exceeds %d bytes", maxRuntimeRouteHeaderBytes)
	}
	body := []byte("http: {}\n")
	if projection.Host != "" {
		body, err = marshalConfig(service, projection.Host, projection.GuestIP, projection.Port)
		if err != nil {
			return nil, err
		}
	}
	return append([]byte(header), body...), nil
}

func (m *Manager) readPublishedRuntimeRoute(service string) (publishedRuntimeRoute, bool, error) {
	var projection publishedRuntimeRoute
	path := filepath.Join(m.configDir, configFileName(service))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return projection, false, nil
	}
	if err != nil {
		return projection, false, fmt.Errorf("inspect published route for %s: %w", service, err)
	}
	if !info.Mode().IsRegular() {
		return projection, false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return projection, false, fmt.Errorf("open published route for %s: %w", service, err)
	}
	defer file.Close()
	reader := bufio.NewReader(io.LimitReader(file, maxRuntimeRouteHeaderBytes+1))
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return projection, false, fmt.Errorf("read published route for %s: %w", service, err)
	}
	if len(line) > maxRuntimeRouteHeaderBytes || !strings.HasPrefix(line, runtimeRouteHeader) || !strings.HasSuffix(line, "\n") {
		return projection, false, nil
	}
	data, err := base64.RawStdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(line, runtimeRouteHeader), "\n"))
	if err != nil || json.Unmarshal(data, &projection) != nil || !validRuntimeProjection(service, projection) {
		return publishedRuntimeRoute{}, false, nil
	}
	return projection, true, nil
}
