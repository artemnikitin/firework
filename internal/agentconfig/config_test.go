package agentconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

func TestExampleAgentConfigsLoad(t *testing.T) {
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	examples, err := filepath.Glob(filepath.Join(workingDir, "..", "..", "examples", "agent*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(examples) == 0 {
		t.Fatal("no agent examples found")
	}
	for _, example := range examples {
		t.Run(filepath.Base(example), func(t *testing.T) {
			if _, err := LoadAgentConfig(example); err != nil {
				t.Fatal(err)
			}
		})
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

func TestLoadAgentConfigNormalizesNodeNames(t *testing.T) {
	tests := []struct {
		name          string
		nodeYAML      string
		wantNodeName  string
		wantNodeNames []string
	}{
		{name: "single name", nodeYAML: "node_name: node-a\n", wantNodeName: "node-a", wantNodeNames: []string{"node-a"}},
		{name: "name list", nodeYAML: "node_names: [node-a, node-b]\n", wantNodeName: "node-a", wantNodeNames: []string{"node-a", "node-b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.yaml")
			data := tt.nodeYAML + "store_type: git\nstore_url: https://example.invalid/config.git\n"
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadAgentConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.NodeName != tt.wantNodeName || !reflect.DeepEqual(cfg.NodeNames, tt.wantNodeNames) {
				t.Fatalf("node name = %q, names = %v; want %q, %v", cfg.NodeName, cfg.NodeNames, tt.wantNodeName, tt.wantNodeNames)
			}
		})
	}
}

func TestLoadAgentConfigStorageBudgets(t *testing.T) {
	yaml := `
node_name: "node-1"
store_type: "git"
store_url: "https://example.invalid/config.git"
storage:
  local:
    path: "/var/lib/firework/volumes"
    capacity: "500Gi"
  shared:
    backend_id: "primary"
    path: "/mnt/firework-shared"
    capacity: "1Ti"
`
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.Local.CapacityBytes != 500*config.GiB || cfg.Storage.Shared.CapacityBytes != config.TiB {
		t.Fatalf("unexpected storage config: %#v", cfg.Storage)
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

func TestLoadAgentConfig_IngressDomain(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "valid", value: "example.com", want: "example.com"},
		{name: "multi-label", value: "gcp.example.com", want: "gcp.example.com"},
		{name: "trailing dot normalized", value: "example.com.", want: "example.com"},
		{name: "uppercase normalized", value: "Example.COM", want: "example.com"},
		{name: "scheme rejected", value: "https://example.com", wantErr: true},
		{name: "port rejected", value: "example.com:443", wantErr: true},
		{name: "path rejected", value: "example.com/foo", wantErr: true},
		{name: "wildcard rejected", value: "*.example.com", wantErr: true},
		{name: "empty label rejected", value: "example..com", wantErr: true},
		{name: "leading hyphen rejected", value: "-example.com", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := "node_name: my-node\nstore_type: git\nstore_url: https://example.com/repo.git\ningress_domain: \"" + tt.value + "\"\n"
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "agent.yaml")
			if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
				t.Fatalf("writing test config: %v", err)
			}
			cfg, err := LoadAgentConfig(cfgPath)
			if (err != nil) != tt.wantErr {
				t.Fatalf("LoadAgentConfig ingress_domain=%q err=%v wantErr=%v", tt.value, err, tt.wantErr)
			}
			if !tt.wantErr && cfg.IngressDomain != tt.want {
				t.Fatalf("IngressDomain=%q want %q", cfg.IngressDomain, tt.want)
			}
		})
	}
}

func TestLoadAgentConfig_IngressDomainOmittedDefaultsEmpty(t *testing.T) {
	yaml := "node_name: my-node\nstore_type: git\nstore_url: https://example.com/repo.git\n"
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	cfg, err := LoadAgentConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.IngressDomain != "" {
		t.Fatalf("expected empty IngressDomain default, got %q", cfg.IngressDomain)
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

func TestLoadAgentConfig_GCSStore(t *testing.T) {
	yaml := `
node_name: "gcp-node"
store_type: "gcs"
gcs_bucket: "firework-configs"
gcs_prefix: "cp/v1/"
gcs_project: "test-project"
gcs_credentials_file: "/tmp/gcp.json"
gcs_images_bucket: "firework-images-amd64"
`
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	cfg, err := LoadAgentConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadAgentConfig: %v", err)
	}
	if cfg.GCSBucket != "firework-configs" || cfg.GCSPrefix != "cp/v1/" || cfg.GCSProject != "test-project" || cfg.GCSCredentialsFile != "/tmp/gcp.json" || cfg.GCSImagesBucket != "firework-images-amd64" {
		t.Fatalf("unexpected GCS config: %#v", cfg)
	}
}

func TestLoadAgentConfig_GCSMissingBucket(t *testing.T) {
	yaml := "node_name: gcp-node\nstore_type: gcs\n"
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	if _, err := LoadAgentConfig(cfgPath); err == nil {
		t.Fatal("expected error for missing gcs_bucket")
	}
}

func TestLoadAgentConfig_ImageBucketsMutuallyExclusive(t *testing.T) {
	yaml := `
node_name: "my-node"
store_type: "s3"
s3_bucket: "my-configs-bucket"
s3_images_bucket: "my-images"
gcs_images_bucket: "my-gcs-images"
`
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	if _, err := LoadAgentConfig(cfgPath); err == nil {
		t.Fatal("expected error when both image buckets are set")
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

func TestLoadAgentConfig_NodeIDDefaultsToNodeName(t *testing.T) {
	yaml := `
node_name: "node-a"
store_type: "s3"
s3_bucket: "bucket-a"
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
	if cfg.NodeID != "node-a" {
		t.Fatalf("expected node_id default to node_name, got %q", cfg.NodeID)
	}
}

func TestLoadAgentConfig_RegistryRequiresCertPaths(t *testing.T) {
	yaml := `
node_name: "node-a"
store_type: "s3"
s3_bucket: "bucket-a"
registry_url: "https://registry.internal:9443"
`
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	_, err := LoadAgentConfig(cfgPath)
	if err == nil {
		t.Fatal("expected validation error for missing registry cert paths")
	}
}

func TestLoadAgentConfig_RegistryRejectsBlankNodeID(t *testing.T) {
	yaml := `
node_id: "   "
node_name: "node-a"
store_type: "s3"
s3_bucket: "bucket-a"
registry_url: "https://registry.internal:9443"
registry_cert_file: "/tmp/node.crt"
registry_key_file: "/tmp/node.key"
registry_ca_file: "/tmp/ca.crt"
`
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	_, err := LoadAgentConfig(cfgPath)
	if err == nil {
		t.Fatal("expected validation error for blank node_id")
	}
}

func TestLoadAgentConfig_RegistryRejectsOverlongNodeID(t *testing.T) {
	yaml := fmt.Sprintf(`
node_id: %q
node_name: "node-a"
store_type: "s3"
s3_bucket: "bucket-a"
registry_url: "https://registry.internal:9443"
registry_cert_file: "/tmp/node.crt"
registry_key_file: "/tmp/node.key"
registry_ca_file: "/tmp/ca.crt"
`, strings.Repeat("a", statusmodel.MaxVolumeIDLen+1))
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	_, err := LoadAgentConfig(cfgPath)
	if err == nil || !strings.Contains(err.Error(), "node_id") {
		t.Fatalf("expected validation error for overlong node_id, got %v", err)
	}
}

func TestLoadAgentConfigStorageRejectsOverlongSharedBackendID(t *testing.T) {
	yaml := fmt.Sprintf(`
node_name: "node-1"
store_type: "git"
store_url: "https://example.invalid/config.git"
storage:
  shared:
    backend_id: %q
    path: "/mnt/firework-shared"
`, strings.Repeat("b", statusmodel.MaxVolumeIDLen+1))
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadAgentConfig(path)
	if err == nil || !strings.Contains(err.Error(), "backend_id") {
		t.Fatalf("expected validation error for overlong shared backend_id, got %v", err)
	}
}

func TestLoadAgentConfig_RegistryBootstrapTokenFromEnv(t *testing.T) {
	t.Setenv("FW_BOOTSTRAP_TOKEN", "env-token-123")
	yaml := `
node_name: "node-a"
store_type: "s3"
s3_bucket: "bucket-a"
registry_url: "https://registry.internal:9443"
registry_cert_file: "/tmp/node.crt"
registry_key_file: "/tmp/node.key"
registry_ca_file: "/tmp/ca.crt"
registry_bootstrap_token: "${FW_BOOTSTRAP_TOKEN}"
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
	if cfg.RegistryBootstrapToken != "env-token-123" {
		t.Fatalf("expected expanded bootstrap token, got %q", cfg.RegistryBootstrapToken)
	}
}

func TestLoadAgentConfig_RegistryBootstrapTokenFromFile(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "bootstrap-token")
	if err := os.WriteFile(tokenFile, []byte("file-token-456\n"), 0o600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}
	yaml := `
node_name: "node-a"
store_type: "s3"
s3_bucket: "bucket-a"
registry_url: "https://registry.internal:9443"
registry_cert_file: "/tmp/node.crt"
registry_key_file: "/tmp/node.key"
registry_ca_file: "/tmp/ca.crt"
registry_bootstrap_token_file: "` + tokenFile + `"
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
	if cfg.RegistryBootstrapToken != "file-token-456" {
		t.Fatalf("expected token loaded from file, got %q", cfg.RegistryBootstrapToken)
	}
}

func TestLoadAgentConfig_RegistryBootstrapTokenAndFileMutuallyExclusive(t *testing.T) {
	yaml := `
node_name: "node-a"
store_type: "s3"
s3_bucket: "bucket-a"
registry_url: "https://registry.internal:9443"
registry_cert_file: "/tmp/node.crt"
registry_key_file: "/tmp/node.key"
registry_ca_file: "/tmp/ca.crt"
registry_bootstrap_token: "inline-token"
registry_bootstrap_token_file: "/tmp/bootstrap-token"
`
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	_, err := LoadAgentConfig(cfgPath)
	if err == nil {
		t.Fatal("expected validation error when both bootstrap token fields are set")
	}
}
