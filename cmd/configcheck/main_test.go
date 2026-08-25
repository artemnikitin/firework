package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

func writeTenant(t *testing.T, root, tenant, body string) {
	t.Helper()
	td := filepath.Join(root, "tenants", tenant)
	if err := os.MkdirAll(td, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "kibana.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRun_Valid(t *testing.T) {
	dir := t.TempDir()
	writeTenant(t, dir, "tenant-1", `
node_type: web
image: /images/kibana.ext4
vcpus: 1
memory_mb: 256
network: true
port_forwards:
  - host_port: 5611
    vm_port: 5601
metadata:
  subdomain: tenant-1
`)
	if err := run(dir, true); err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
}

func TestRun_InvalidErrors(t *testing.T) {
	dir := t.TempDir()
	writeTenant(t, dir, "tenant-1", `
node_type: web
image: /images/kibana.ext4
vcpus: 1
memory_mb: 256
network: true
port_forwards:
  - host_port: 5611
    vm_port: 5601
metadata:
  subdomain: tenant-1
  host: kibana.example.com
`)
	if err := run(dir, false); err == nil {
		t.Fatal("expected error for service with both routing keys")
	}
}

func TestRun_RequireRemoteRoutingPromotesWarning(t *testing.T) {
	dir := t.TempDir()
	// Routed via a health-check port, no port_forwards: valid locally but not
	// remote-routable.
	writeTenant(t, dir, "tenant-1", `
node_type: web
image: /images/kibana.ext4
vcpus: 1
memory_mb: 256
network: true
health_check:
  type: http
  port: 5601
metadata:
  subdomain: tenant-1
`)
	if err := run(dir, false); err != nil {
		t.Fatalf("expected success without remote-routing enforcement, got: %v", err)
	}
	if err := run(dir, true); err == nil {
		t.Fatal("expected failure with --require-remote-routing")
	}
}

func TestRunNodeConfig_WarnsOnVolumeSizeWithoutGeneration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.yaml")
	if err := os.WriteFile(path, []byte(`node: node-1
services:
  - name: db
    image: /images/db.ext4
    kernel: /images/vmlinux
    vcpus: 1
    memory_mb: 512
    volumes:
      - name: data
        type: local
        mount_path: /var/lib/db
        size_bytes: 10737418240
        bound_node: node-1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runNodeConfig(path); err != nil {
		t.Fatalf("an absent generation is advisory, not a failure: %v", err)
	}

	nc, err := config.ParseNodeConfig([]byte(`node: node-1
services:
  - name: db
    volumes:
      - name: data
        type: local
        mount_path: /var/lib/db
        size_bytes: 10737418240
`))
	if err != nil {
		t.Fatal(err)
	}
	warnings := config.NodeConfigWarnings(nc)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "resize_generation") {
		t.Fatalf("expected a resize_generation warning, got %#v", warnings)
	}

	nc.Services[0].Volumes[0].ResizeGeneration = 1
	if got := config.NodeConfigWarnings(nc); len(got) != 0 {
		t.Fatalf("a declared generation must not warn, got %#v", got)
	}
}
