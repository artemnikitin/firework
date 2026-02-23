package enricher

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnrich_EndToEnd(t *testing.T) {
	dir := setupTestDir(t)

	result, err := Enrich(dir)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}

	// Two node types: compute, general-purpose (sorted).
	if len(result.NodeConfigs) != 2 {
		t.Fatalf("expected 2 node configs, got %d", len(result.NodeConfigs))
	}

	nc1 := result.NodeConfigs[0] // compute
	nc2 := result.NodeConfigs[1] // general-purpose

	if nc1.Node != "compute" {
		t.Errorf("expected compute, got %s", nc1.Node)
	}
	if nc2.Node != "general-purpose" {
		t.Errorf("expected general-purpose, got %s", nc2.Node)
	}

	// compute should have the worker service.
	if len(nc1.Services) != 1 {
		t.Fatalf("expected 1 service on compute, got %d", len(nc1.Services))
	}
	if nc1.Services[0].Name != "worker" {
		t.Errorf("expected worker on compute, got %s", nc1.Services[0].Name)
	}

	// general-purpose should have web.
	if len(nc2.Services) != 1 {
		t.Fatalf("expected 1 service on general-purpose, got %d", len(nc2.Services))
	}
	web := nc2.Services[0]
	if web.Name != "web" {
		t.Errorf("expected web, got %s", web.Name)
	}

	// Check enrichment for web.
	if web.Kernel != "/var/lib/images/vmlinux-5.10" {
		t.Errorf("expected default kernel, got %s", web.Kernel)
	}
	if web.VCPUs != 2 {
		t.Errorf("expected 2 vcpus, got %d", web.VCPUs)
	}
	if web.MemoryMB != 512 {
		t.Errorf("expected 512 memory, got %d", web.MemoryMB)
	}

	// Network stub should be set (agent fills in IP/MAC at runtime).
	if web.Network == nil {
		t.Fatal("expected non-nil network for service with network: true")
	}
	if web.Network.Interface != "tap-web" {
		t.Errorf("expected interface tap-web, got %s", web.Network.Interface)
	}

	// Health check should have port+path, not target.
	if web.HealthCheck == nil {
		t.Fatal("expected health check for web")
	}
	if web.HealthCheck.Type != "http" {
		t.Errorf("expected http, got %s", web.HealthCheck.Type)
	}
	if web.HealthCheck.Port != 8080 {
		t.Errorf("expected port 8080, got %d", web.HealthCheck.Port)
	}
	if web.HealthCheck.Path != "/health" {
		t.Errorf("expected /health, got %s", web.HealthCheck.Path)
	}
	if web.HealthCheck.Target != "" {
		t.Errorf("expected empty target, got %s", web.HealthCheck.Target)
	}
	if web.HealthCheck.Interval != 10*time.Second {
		t.Errorf("expected 10s interval, got %s", web.HealthCheck.Interval)
	}

	// Metadata should be preserved.
	if web.Metadata["version"] != "1.2.3" {
		t.Errorf("expected version=1.2.3, got %s", web.Metadata["version"])
	}
}

func TestEnrich_Warnings(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "defaults.yaml"), `
kernel: /var/lib/images/vmlinux-5.10
`)
	svcDir := filepath.Join(dir, "services")
	os.MkdirAll(svcDir, 0o755)
	writeFile(t, filepath.Join(svcDir, "web.yaml"), `
name: web
image: /images/web.ext4
node_type: general-purpose
vcpus: 1
memory_mb: 256
network: false
health_check:
  type: http
  port: 8080
`)

	result, err := Enrich(dir)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}

	// Should warn about health check without network.
	found := false
	for _, w := range result.Warnings {
		if w.Message == "service web has health check but network is disabled" {
			found = true
		}
	}
	if !found {
		t.Error("expected health-check-without-network warning")
	}
}

func TestEnrich_ValidationError(t *testing.T) {
	dir := t.TempDir()
	svcDir := filepath.Join(dir, "services")
	os.MkdirAll(svcDir, 0o755)
	// Missing image — should fail validation.
	writeFile(t, filepath.Join(svcDir, "bad.yaml"), `
name: bad
node_type: compute
`)

	_, err := Enrich(dir)
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestEnrich_NoHealthCheck(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "defaults.yaml"), `
kernel: /var/lib/images/vmlinux-5.10
`)
	svcDir := filepath.Join(dir, "services")
	os.MkdirAll(svcDir, 0o755)
	writeFile(t, filepath.Join(svcDir, "batch.yaml"), `
name: batch
image: /images/batch.ext4
node_type: compute
vcpus: 2
memory_mb: 1024
network: false
`)

	result, err := Enrich(dir)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}

	if len(result.NodeConfigs) != 1 {
		t.Fatalf("expected 1 node config, got %d", len(result.NodeConfigs))
	}
	svc := result.NodeConfigs[0].Services[0]
	if svc.Network != nil {
		t.Errorf("expected no network for batch, got %+v", svc.Network)
	}
	if svc.HealthCheck != nil {
		t.Errorf("expected no health check for batch, got %+v", svc.HealthCheck)
	}
}

func TestEnrich_MultipleServicesPerType(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "defaults.yaml"), `
kernel: /var/lib/images/vmlinux-5.10
`)
	svcDir := filepath.Join(dir, "services")
	os.MkdirAll(svcDir, 0o755)
	writeFile(t, filepath.Join(svcDir, "web.yaml"), `
name: web
image: /images/web.ext4
node_type: general-purpose
vcpus: 2
memory_mb: 512
`)
	writeFile(t, filepath.Join(svcDir, "api.yaml"), `
name: api
image: /images/api.ext4
node_type: general-purpose
vcpus: 4
memory_mb: 1024
`)

	result, err := Enrich(dir)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}

	if len(result.NodeConfigs) != 1 {
		t.Fatalf("expected 1 node config, got %d", len(result.NodeConfigs))
	}
	nc := result.NodeConfigs[0]
	if nc.Node != "general-purpose" {
		t.Errorf("expected general-purpose, got %s", nc.Node)
	}
	if len(nc.Services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(nc.Services))
	}
}

func TestGroupByNodeType(t *testing.T) {
	services := []ServiceSpec{
		{Name: "web", NodeType: "general-purpose"},
		{Name: "worker", NodeType: "compute"},
		{Name: "api", NodeType: "general-purpose"},
	}

	groups := groupByNodeType(services)

	if len(groups["general-purpose"]) != 2 {
		t.Errorf("expected 2 services for general-purpose, got %d", len(groups["general-purpose"]))
	}
	if len(groups["compute"]) != 1 {
		t.Errorf("expected 1 service for compute, got %d", len(groups["compute"]))
	}
}

// setupTestDir creates a temporary directory with a realistic config set.
func setupTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "defaults.yaml"), `
kernel: /var/lib/images/vmlinux-5.10
vcpus: 1
memory_mb: 256
kernel_args: "console=ttyS0 reboot=k panic=1 pci=off"
health_check:
  type: http
  port: 8080
  path: /health
  interval: 10s
  timeout: 5s
  retries: 3
`)

	svcDir := filepath.Join(dir, "services")
	os.MkdirAll(svcDir, 0o755)

	writeFile(t, filepath.Join(svcDir, "web.yaml"), `
name: web
image: /var/lib/images/web-api-rootfs.ext4
node_type: general-purpose
vcpus: 2
memory_mb: 512
network: true
metadata:
  version: "1.2.3"
`)

	writeFile(t, filepath.Join(svcDir, "worker.yaml"), `
name: worker
image: /var/lib/images/worker-rootfs.ext4
node_type: compute
vcpus: 4
memory_mb: 1024
network: true
health_check:
  type: tcp
  port: 9090
`)

	return dir
}
