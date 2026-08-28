package main

import (
	"fmt"
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

// --node-config must validate, not just parse: a plainly invalid node config
// reporting OK defeats the point of running this in CI.
func TestNodeConfigIsSemanticallyValidated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.yaml")
	if err := os.WriteFile(path, []byte(`node: ""
services:
  - name: broken
    vcpus: 0
    memory_mb: 0
    volumes:
      - name: data
        type: local
        mount_path: /var/lib/db
        size_bytes: -1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runNodeConfig(path); err == nil {
		t.Fatal("a node config with no name, no image/kernel, zero compute and a negative volume size must not validate")
	}
}

// ValidateOutput covers generic service fields, not the volume invariants the
// agent enforces. A local volume with no bound_node is unusable, and
// configcheck must say so rather than printing OK.
func TestLocalVolumeWithoutBoundNodeIsRejected(t *testing.T) {
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
        resize_generation: 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runNodeConfig(path); err == nil {
		t.Fatal("a local volume with no bound_node is unusable and must not validate")
	}
}

// The volume contract is checked through the agent's own rules, so the two
// cannot drift into a config that passes CI and then fails to start.
func TestNodeConfigVolumeContractMatchesTheAgent(t *testing.T) {
	valid := `node: node-1
services:
  - name: db
    image: /images/db.ext4
    kernel: /images/vmlinux
    vcpus: 1
    memory_mb: 512
    volumes:
      - name: data
        type: local
        mount_path: %s
        size_bytes: 10737418240
        bound_node: %s
        resize_generation: 1
`
	tests := []struct {
		name      string
		mountPath string
		boundNode string
		wantErr   bool
	}{
		{name: "valid", mountPath: "/var/lib/db", boundNode: "node-1"},
		{name: "reserved mount path", mountPath: "/proc/db", boundNode: "node-1", wantErr: true},
		{name: "relative mount path", mountPath: "var/lib/db", boundNode: "node-1", wantErr: true},
		// A bound_node naming another node is only probably wrong — the agent
		// matches its stable node_id, which need not equal the config key — so
		// it warns rather than failing.
		{name: "bound elsewhere warns only", mountPath: "/var/lib/db", boundNode: "node-2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "node.yaml")
			if err := os.WriteFile(path, []byte(fmt.Sprintf(valid, test.mountPath, test.boundNode)), 0o644); err != nil {
				t.Fatal(err)
			}
			err := runNodeConfig(path)
			if test.wantErr && err == nil {
				t.Fatal("expected the volume contract to reject this config")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("expected the config to validate, got %v", err)
			}
		})
	}
}

func TestNodeConfigEdgeInputs(t *testing.T) {
	dir := t.TempDir()
	t.Run("missing file", func(t *testing.T) {
		if err := runNodeConfig(filepath.Join(dir, "nope.yaml")); err == nil {
			t.Fatal("a missing node config must be an error")
		}
	})
	t.Run("directory", func(t *testing.T) {
		if err := runNodeConfig(dir); err == nil {
			t.Fatal("a directory must be an error")
		}
	})
	t.Run("empty file", func(t *testing.T) {
		p := filepath.Join(dir, "empty.yaml")
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := runNodeConfig(p); err == nil {
			t.Fatal("an empty node config has no node name and must be an error")
		}
	})
	t.Run("service with volumes but no name", func(t *testing.T) {
		p := filepath.Join(dir, "noname.yaml")
		if err := os.WriteFile(p, []byte(`node: node-1
services:
  - image: /i
    kernel: /k
    vcpus: 1
    memory_mb: 128
    volumes:
      - name: data
        type: local
        mount_path: /var/lib/app
        size_bytes: 1048576
        bound_node: node-1
        resize_generation: 1
`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := runNodeConfig(p); err == nil {
			t.Fatal("a service with no name must be an error")
		}
	})
}
