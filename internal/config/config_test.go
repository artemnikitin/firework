package config

import (
	"testing"
	"time"
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

func TestParseNodeConfig_Empty(t *testing.T) {
	nc, err := ParseNodeConfig([]byte(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Services) != 0 {
		t.Errorf("expected 0 services, got %d", len(nc.Services))
	}
}

func TestParseNodeConfig_Invalid(t *testing.T) {
	_, err := ParseNodeConfig([]byte("{{invalid yaml"))
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}
