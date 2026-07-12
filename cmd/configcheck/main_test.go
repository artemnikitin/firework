package main

import (
	"os"
	"path/filepath"
	"testing"
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
