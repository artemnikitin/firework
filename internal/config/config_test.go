package config

import (
	"os"
	"path/filepath"
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

func TestLoadAgentConfig(t *testing.T) {
	yaml := `
node_name: "my-node"
store_type: "git"
store_url: "https://github.com/example/configs.git"
store_branch: "main"
poll_interval: 60s
firecracker_bin: "/usr/local/bin/firecracker"
state_dir: "/tmp/test-state"
log_level: "debug"
api_listen_addr: ":9090"
`

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	cfg, err := LoadAgentConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.NodeName != "my-node" {
		t.Errorf("expected node name my-node, got %s", cfg.NodeName)
	}
	if cfg.StoreURL != "https://github.com/example/configs.git" {
		t.Errorf("unexpected store URL: %s", cfg.StoreURL)
	}
	if cfg.PollInterval != 60*time.Second {
		t.Errorf("expected 60s poll interval, got %v", cfg.PollInterval)
	}
	if cfg.FirecrackerBin != "/usr/local/bin/firecracker" {
		t.Errorf("unexpected firecracker bin: %s", cfg.FirecrackerBin)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("expected log level debug, got %s", cfg.LogLevel)
	}
	if cfg.APIListenAddr != ":9090" {
		t.Errorf("expected API listen addr :9090, got %s", cfg.APIListenAddr)
	}
}

func TestLoadAgentConfig_Defaults(t *testing.T) {
	yaml := `
store_url: "https://github.com/example/configs.git"
`

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	cfg, err := LoadAgentConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.StoreType != "git" {
		t.Errorf("expected default store type git, got %s", cfg.StoreType)
	}
	if cfg.StoreBranch != "main" {
		t.Errorf("expected default branch main, got %s", cfg.StoreBranch)
	}
	if cfg.PollInterval != 30*time.Second {
		t.Errorf("expected default 30s poll interval, got %v", cfg.PollInterval)
	}
	if cfg.FirecrackerBin != "/usr/bin/firecracker" {
		t.Errorf("expected default firecracker bin, got %s", cfg.FirecrackerBin)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("expected default log level info, got %s", cfg.LogLevel)
	}
}

func TestLoadAgentConfig_MissingStoreURL(t *testing.T) {
	yaml := `
node_name: "my-node"
store_type: "git"
`

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	_, err := LoadAgentConfig(cfgPath)
	if err == nil {
		t.Error("expected error for missing store_url")
	}
}

func TestLoadAgentConfig_S3Store(t *testing.T) {
	yaml := `
node_name: "my-node"
store_type: "s3"
s3_bucket: "my-configs-bucket"
s3_prefix: "prod/"
s3_region: "us-west-2"
poll_interval: 15s
`

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	cfg, err := LoadAgentConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.StoreType != "s3" {
		t.Errorf("expected store type s3, got %s", cfg.StoreType)
	}
	if cfg.S3Bucket != "my-configs-bucket" {
		t.Errorf("expected bucket my-configs-bucket, got %s", cfg.S3Bucket)
	}
	if cfg.S3Prefix != "prod/" {
		t.Errorf("expected prefix prod/, got %s", cfg.S3Prefix)
	}
	if cfg.S3Region != "us-west-2" {
		t.Errorf("expected region us-west-2, got %s", cfg.S3Region)
	}
	if cfg.PollInterval != 15*time.Second {
		t.Errorf("expected 15s poll interval, got %v", cfg.PollInterval)
	}
}

func TestLoadAgentConfig_S3MissingBucket(t *testing.T) {
	yaml := `
node_name: "my-node"
store_type: "s3"
`

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	_, err := LoadAgentConfig(cfgPath)
	if err == nil {
		t.Error("expected error for missing s3_bucket")
	}
}

func TestLoadAgentConfig_UnsupportedStoreType(t *testing.T) {
	yaml := `
node_name: "my-node"
store_type: "consul"
`

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	_, err := LoadAgentConfig(cfgPath)
	if err == nil {
		t.Error("expected error for unsupported store type")
	}
}

func TestLoadAgentConfig_FileNotFound(t *testing.T) {
	_, err := LoadAgentConfig("/nonexistent/agent.yaml")
	if err == nil {
		t.Error("expected error for missing file")
	}
}
