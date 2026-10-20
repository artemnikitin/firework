package config

import (
	"errors"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestParseNodeConfig(t *testing.T) {
	yaml := `
node: "test-node"
services:
  - name: "web"
    image: "/images/web.ext4"
    kernel: "/images/vmlinux"
    vcpus: 2
    memory_mb: 512
    kernel_args: "console=ttyS0"
    network:
      interface: "tap-web"
      guest_mac: "AA:FC:00:00:00:01"
      guest_ip: "172.16.0.2/24"
    health_check:
      type: "http"
      target: "http://172.16.0.2:8080/health"
      interval: 10s
      timeout: 5s
      retries: 3
    metadata:
      env: "prod"
  - name: "worker"
    image: "/images/worker.ext4"
    kernel: "/images/vmlinux"
    vcpus: 4
    memory_mb: 1024
`

	nc, err := ParseNodeConfig([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if nc.Node != "test-node" {
		t.Errorf("expected node test-node, got %s", nc.Node)
	}

	if len(nc.Services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(nc.Services))
	}

	web := nc.Services[0]
	if web.Name != "web" {
		t.Errorf("expected service name web, got %s", web.Name)
	}
	if web.VCPUs != 2 {
		t.Errorf("expected 2 vcpus, got %d", web.VCPUs)
	}
	if web.MemoryMB != 512 {
		t.Errorf("expected 512 MB, got %d", web.MemoryMB)
	}
	if web.KernelArgs != "console=ttyS0" {
		t.Errorf("expected kernel args console=ttyS0, got %s", web.KernelArgs)
	}

	if web.Network == nil {
		t.Fatal("expected network config")
	}
	if web.Network.Interface != "tap-web" {
		t.Errorf("expected tap-web, got %s", web.Network.Interface)
	}
	if web.Network.GuestMAC != "AA:FC:00:00:00:01" {
		t.Errorf("expected guest MAC AA:FC:00:00:00:01, got %s", web.Network.GuestMAC)
	}

	if web.HealthCheck == nil {
		t.Fatal("expected health check config")
	}
	if web.HealthCheck.Type != "http" {
		t.Errorf("expected http check type, got %s", web.HealthCheck.Type)
	}
	if web.HealthCheck.Interval != 10*time.Second {
		t.Errorf("expected 10s interval, got %v", web.HealthCheck.Interval)
	}
	if web.HealthCheck.Retries != 3 {
		t.Errorf("expected 3 retries, got %d", web.HealthCheck.Retries)
	}

	if web.Metadata["env"] != "prod" {
		t.Errorf("expected metadata env=prod, got %s", web.Metadata["env"])
	}

	worker := nc.Services[1]
	if worker.Name != "worker" {
		t.Errorf("expected service name worker, got %s", worker.Name)
	}
	if worker.Network != nil {
		t.Error("expected nil network for worker")
	}
	if worker.HealthCheck != nil {
		t.Error("expected nil health check for worker")
	}
}

// Only an explicit services list is authoritative. The agent deletes every
// running service a config omits, so anything that merely fails to mention
// services — a truncated object, a half-synced file, a typo — must be a parse
// error that preserves the node, never a decoded empty assignment.
func TestParseNodeConfig_ExplicitEmptyState(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr bool
	}{
		{name: "empty list", data: "node: web\nservices: []\n"},
		{name: "empty list with revisions", data: "node: web\nservices: []\nrendered_revision: rendered-1\n"},
		{name: "empty body", data: "", wantErr: true},
		{name: "whitespace only", data: " \n\t\n", wantErr: true},
		{name: "document marker only", data: "---\n", wantErr: true},
		{name: "comments only", data: "# nothing here\n", wantErr: true},
		{name: "omitted services", data: "node: web\n", wantErr: true},
		{name: "null services", data: "node: web\nservices: null\n", wantErr: true},
		{name: "tilde services", data: "node: web\nservices: ~\n", wantErr: true},
		{name: "bare services key", data: "node: web\nservices:\n", wantErr: true},
		{name: "scalar services", data: "node: web\nservices: web\n", wantErr: true},
		{name: "mapping services", data: "node: web\nservices: {}\n", wantErr: true},
		{name: "top-level list", data: "- node: web\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nc, err := ParseNodeConfig([]byte(tt.data))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected a parse error, got %#v", nc)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if nc.Services == nil || len(nc.Services) != 0 {
				t.Fatalf("expected a non-nil empty service list, got %#v", nc.Services)
			}
		})
	}
}

func TestParseNodeConfig_MissingServicesIsTyped(t *testing.T) {
	_, err := ParseNodeConfig([]byte("node: web\n"))
	if !errors.Is(err, ErrNoServicesList) {
		t.Fatalf("expected ErrNoServicesList, got %v", err)
	}
}

// The control plane renders an empty node from a zero-value NodeConfig whose
// Services slice is nil. That must marshal to a document the strict parser
// accepts, or every empty node would read as unknown instead of empty.
func TestParseNodeConfig_AcceptsMarshaledEmptyConfig(t *testing.T) {
	for _, nc := range []NodeConfig{
		{Node: "web"},
		{Node: "web", Services: []ServiceConfig{}},
	} {
		data, err := yaml.Marshal(nc)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseNodeConfig(data)
		if err != nil {
			t.Fatalf("marshaled empty config %q was rejected: %v", data, err)
		}
		if parsed.Node != "web" || parsed.Services == nil || len(parsed.Services) != 0 {
			t.Fatalf("unexpected round trip of %q: %#v", data, parsed)
		}
	}
}

func TestParseNodeConfig_Invalid(t *testing.T) {
	_, err := ParseNodeConfig([]byte("{{invalid yaml"))
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}
