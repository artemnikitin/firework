package enricher

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadInput(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "defaults.yaml"), `
kernel: /var/lib/images/vmlinux
vcpus: 2
memory_mb: 512
`)
	svcDir := filepath.Join(dir, "services")
	os.MkdirAll(svcDir, 0o755)
	writeFile(t, filepath.Join(svcDir, "web.yaml"), `
name: web
image: /images/web.ext4
node_type: general-purpose
vcpus: 4
network: true
health_check:
  type: http
  port: 8080
  path: /health
`)

	input, err := LoadInput(dir)
	if err != nil {
		t.Fatalf("LoadInput: %v", err)
	}

	if input.Defaults.Kernel != "/var/lib/images/vmlinux" {
		t.Errorf("expected kernel default, got %s", input.Defaults.Kernel)
	}
	if input.Defaults.VCPUs != 2 {
		t.Errorf("expected vcpus default 2, got %d", input.Defaults.VCPUs)
	}

	if len(input.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(input.Services))
	}
	svc := input.Services[0]
	if svc.Name != "web" {
		t.Errorf("expected service name web, got %s", svc.Name)
	}
	if svc.NodeType != "general-purpose" {
		t.Errorf("expected node_type general-purpose, got %s", svc.NodeType)
	}
	if svc.VCPUs != 4 {
		t.Errorf("expected vcpus 4, got %d", svc.VCPUs)
	}
	if !svc.Network {
		t.Errorf("expected network=true")
	}
	if svc.HealthCheck == nil || svc.HealthCheck.Port != 8080 {
		t.Errorf("expected health check with port 8080")
	}
}

func TestLoadInput_MissingDefaults(t *testing.T) {
	dir := t.TempDir()
	svcDir := filepath.Join(dir, "services")
	os.MkdirAll(svcDir, 0o755)
	writeFile(t, filepath.Join(svcDir, "app.yaml"), `
name: app
image: /images/app.ext4
node_type: general-purpose
`)

	input, err := LoadInput(dir)
	if err != nil {
		t.Fatalf("LoadInput: %v", err)
	}

	if input.Defaults.Kernel != "" {
		t.Errorf("expected empty kernel default, got %s", input.Defaults.Kernel)
	}
	if input.Defaults.VCPUs != 0 {
		t.Errorf("expected zero vcpus default, got %d", input.Defaults.VCPUs)
	}
}

func TestLoadInput_MissingServicesDir(t *testing.T) {
	// services/ is optional — standalone tenant mode uses tenants/ only.
	dir := t.TempDir()

	input, err := LoadInput(dir)
	if err != nil {
		t.Fatalf("expected no error for missing services directory, got: %v", err)
	}
	if len(input.Services) != 0 {
		t.Errorf("expected empty services, got %d", len(input.Services))
	}
}

func TestLoadInput_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	svcDir := filepath.Join(dir, "services")
	os.MkdirAll(svcDir, 0o755)
	writeFile(t, filepath.Join(svcDir, "bad.yaml"), `invalid: [`)

	_, err := LoadInput(dir)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestParseServices_SkipsNonYAML(t *testing.T) {
	dir := t.TempDir()
	svcDir := filepath.Join(dir, "services")
	os.MkdirAll(svcDir, 0o755)
	writeFile(t, filepath.Join(svcDir, "readme.txt"), "not a service")
	writeFile(t, filepath.Join(svcDir, "web.yml"), `
name: web
image: /images/web.ext4
node_type: compute
`)

	services, err := parseServices(svcDir)
	if err != nil {
		t.Fatalf("parseServices: %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(services))
	}
	if services[0].Name != "web" {
		t.Errorf("expected web, got %s", services[0].Name)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
