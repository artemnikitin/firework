package traefik

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

func TestSync_WritesConfigForServiceWithHost(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	services := []config.ServiceConfig{
		{
			Name:         "kibana",
			Network:      &config.NetworkConfig{GuestIP: "172.16.0.2"},
			PortForwards: []config.PortForward{{HostPort: 5611, VMPort: 5601}},
			Metadata:     map[string]string{"host": "tenant-1.example.com"},
		},
	}

	if err := m.Sync(services, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "kibana.yaml"))
	if err != nil {
		t.Fatalf("expected config file to be written: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "tenant-1.example.com") {
		t.Errorf("expected host rule in config, got:\n%s", content)
	}
	if !strings.Contains(content, "172.16.0.2:5601") {
		t.Errorf("expected backend URL in config, got:\n%s", content)
	}
}

func TestSync_SkipsServiceWithoutHost(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	services := []config.ServiceConfig{
		{
			Name:         "elasticsearch",
			Network:      &config.NetworkConfig{GuestIP: "172.16.0.3"},
			PortForwards: []config.PortForward{{HostPort: 9200, VMPort: 9200}},
			// No metadata["host"]
		},
	}

	if err := m.Sync(services, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected no config files for service without host, got %d", len(entries))
	}
}

func TestSync_SkipsServiceWithoutGuestIP(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	services := []config.ServiceConfig{
		{
			Name:         "kibana",
			Network:      &config.NetworkConfig{}, // no GuestIP
			PortForwards: []config.PortForward{{HostPort: 5611, VMPort: 5601}},
			Metadata:     map[string]string{"host": "tenant-1.example.com"},
		},
	}

	if err := m.Sync(services, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected no config files without guest IP, got %d", len(entries))
	}
}

func TestSync_SkipsServiceWithoutPort(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	services := []config.ServiceConfig{
		{
			Name:     "kibana",
			Network:  &config.NetworkConfig{GuestIP: "172.16.0.2"},
			Metadata: map[string]string{"host": "tenant-1.example.com"},
			// No port_forwards, no health check
		},
	}

	if err := m.Sync(services, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected no config files without port, got %d", len(entries))
	}
}

func TestSync_FallsBackToHealthCheckPort(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	services := []config.ServiceConfig{
		{
			Name:        "kibana",
			Network:     &config.NetworkConfig{GuestIP: "172.16.0.2"},
			HealthCheck: &config.HealthCheckConfig{Port: 5601},
			Metadata:    map[string]string{"host": "tenant-1.example.com"},
		},
	}

	if err := m.Sync(services, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "kibana.yaml"))
	if err != nil {
		t.Fatalf("expected config file to be written: %v", err)
	}

	if !strings.Contains(string(data), "172.16.0.2:5601") {
		t.Errorf("expected health check port as backend, got:\n%s", string(data))
	}
}

func TestSync_DeletesStaleFiles(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	// Write a stale config file that belongs to a deleted service.
	stale := filepath.Join(dir, "old-service.yaml")
	if err := os.WriteFile(stale, []byte("http: {}"), 0644); err != nil {
		t.Fatal(err)
	}

	// Sync with no matching services.
	services := []config.ServiceConfig{
		{
			Name:         "kibana",
			Network:      &config.NetworkConfig{GuestIP: "172.16.0.2"},
			PortForwards: []config.PortForward{{HostPort: 5611, VMPort: 5601}},
			Metadata:     map[string]string{"host": "tenant-1.example.com"},
		},
	}

	if err := m.Sync(services, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("expected stale config file to be deleted")
	}

	if _, err := os.ReadFile(filepath.Join(dir, "kibana.yaml")); err != nil {
		t.Errorf("expected kibana config to still exist: %v", err)
	}
}

func TestSync_PreservesNonYAMLFiles(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	// Non-YAML files should be left alone.
	other := filepath.Join(dir, "traefik.toml")
	if err := os.WriteFile(other, []byte("# static config"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := m.Sync(nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(other); err != nil {
		t.Errorf("expected non-YAML file to be preserved: %v", err)
	}
}

func TestSync_WritesRemoteConfig(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	remoteNodes := []config.NodeConfig{
		{
			Node:   "node-2",
			HostIP: "10.0.1.5",
			Services: []config.ServiceConfig{
				{
					Name:         "tenant-1-kibana",
					PortForwards: []config.PortForward{{HostPort: 5611, VMPort: 5601}},
					Metadata:     map[string]string{"host": "tenant-1.example.com"},
				},
			},
		},
	}

	if err := m.Sync(nil, remoteNodes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "remote-tenant-1-kibana.yaml"))
	if err != nil {
		t.Fatalf("expected remote config file to be written: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "tenant-1.example.com") {
		t.Errorf("expected host rule in remote config, got:\n%s", content)
	}
	if !strings.Contains(content, "10.0.1.5:5611") {
		t.Errorf("expected peer hostIP:hostPort in remote config, got:\n%s", content)
	}
}

func TestSync_SkipsRemoteServiceWithoutHostIP(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	remoteNodes := []config.NodeConfig{
		{
			Node:   "node-2",
			HostIP: "", // no host IP
			Services: []config.ServiceConfig{
				{
					Name:         "tenant-1-kibana",
					PortForwards: []config.PortForward{{HostPort: 5611, VMPort: 5601}},
					Metadata:     map[string]string{"host": "tenant-1.example.com"},
				},
			},
		},
	}

	if err := m.Sync(nil, remoteNodes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected no config files when peer HostIP is empty, got %d", len(entries))
	}
}

func TestSync_SkipsRemoteServiceWithoutHost(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	remoteNodes := []config.NodeConfig{
		{
			Node:   "node-2",
			HostIP: "10.0.1.5",
			Services: []config.ServiceConfig{
				{
					Name:         "tenant-1-kibana",
					PortForwards: []config.PortForward{{HostPort: 5611, VMPort: 5601}},
					// No metadata["host"]
				},
			},
		},
	}

	if err := m.Sync(nil, remoteNodes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected no config files for remote service without host, got %d", len(entries))
	}
}

func TestSync_CleansUpStaleRemoteConfigs(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	// Pre-existing remote config for a service that is no longer scheduled anywhere.
	stale := filepath.Join(dir, "remote-old-service.yaml")
	if err := os.WriteFile(stale, []byte("http: {}"), 0644); err != nil {
		t.Fatal(err)
	}

	// Sync with a different remote node (no "old-service").
	remoteNodes := []config.NodeConfig{
		{
			Node:   "node-2",
			HostIP: "10.0.1.5",
			Services: []config.ServiceConfig{
				{
					Name:         "tenant-1-kibana",
					PortForwards: []config.PortForward{{HostPort: 5611, VMPort: 5601}},
					Metadata:     map[string]string{"host": "tenant-1.example.com"},
				},
			},
		},
	}

	if err := m.Sync(nil, remoteNodes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("expected stale remote config file to be deleted")
	}

	if _, err := os.ReadFile(filepath.Join(dir, "remote-tenant-1-kibana.yaml")); err != nil {
		t.Errorf("expected remote kibana config to exist: %v", err)
	}
}
