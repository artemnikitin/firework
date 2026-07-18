package controlplane

import (
	"os"
	"path/filepath"
	"testing"
)

func validConfigForRole(role string) Config {
	cfg := DefaultConfig()
	cfg.Role = role
	cfg.State.S3.Bucket = "test-bucket"
	cfg.TLS.CertFile = "/tmp/cert.pem"
	cfg.TLS.KeyFile = "/tmp/key.pem"
	cfg.TLS.ClientCAFile = "/tmp/ca.pem"
	cfg.Enrollment.CAFile = "/tmp/ca.pem"
	cfg.Enrollment.CAKeyFile = "/tmp/ca.key"
	cfg.GitHubWebhookSecret = "secret"
	cfg.OperatorToken = "operator-secret"
	return cfg
}

func TestConfigValidate_BackendAndRole(t *testing.T) {
	cfg := validConfigForRole(RoleAll)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}

	cfg.State.Backend = "gcs"
	cfg.State.GCS.Bucket = "test-gcs-bucket"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid GCS config, got error: %v", err)
	}

	cfg.State.GCS.Bucket = ""
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected missing GCS bucket validation error")
	}

	cfg.State.Backend = "azure"
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected unsupported backend validation error")
	}

	cfg = validConfigForRole("bad-role")
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected role validation error")
	}
}

func TestConfigValidate_RoleSpecificRequirements(t *testing.T) {
	reg := validConfigForRole(RoleRegistry)
	reg.GitHubWebhookSecret = ""
	if err := reg.Validate(); err != nil {
		t.Fatalf("registry role should not require github secret: %v", err)
	}

	events := validConfigForRole(RoleEvents)
	events.TLS.ClientCAFile = ""
	events.Enrollment.CAFile = ""
	events.Enrollment.CAKeyFile = ""
	if err := events.Validate(); err != nil {
		t.Fatalf("events role should not require registry enrollment settings: %v", err)
	}

	events.GitHubWebhookSecret = ""
	if err := events.Validate(); err == nil {
		t.Fatalf("events role must require github_webhook_secret")
	}
}

func TestConfigValidate_NodeStaleTTL(t *testing.T) {
	cfg := validConfigForRole(RoleController)
	cfg.NodeStaleTTL = 0
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected node_stale_ttl validation error")
	}
}

func TestConfigResolve_WebhookSecretFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("file resolved", func(t *testing.T) {
		f := filepath.Join(dir, "webhook-secret")
		if err := os.WriteFile(f, []byte("  mysecret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := validConfigForRole(RoleEvents)
		cfg.GitHubWebhookSecret = ""
		cfg.GitHubWebhookSecretFile = f
		if err := cfg.resolve(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.GitHubWebhookSecret != "mysecret" {
			t.Errorf("want %q, got %q", "mysecret", cfg.GitHubWebhookSecret)
		}
	})

	t.Run("mutually exclusive", func(t *testing.T) {
		f := filepath.Join(dir, "webhook-secret2")
		if err := os.WriteFile(f, []byte("other"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := validConfigForRole(RoleEvents)
		cfg.GitHubWebhookSecretFile = f
		if err := cfg.resolve(); err == nil {
			t.Fatal("expected error for both inline and file set")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		cfg := validConfigForRole(RoleEvents)
		cfg.GitHubWebhookSecret = ""
		cfg.GitHubWebhookSecretFile = filepath.Join(dir, "nonexistent")
		if err := cfg.resolve(); err == nil {
			t.Fatal("expected error for missing file")
		}
	})
}

func TestConfigResolve_TokenFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("file resolved", func(t *testing.T) {
		f := filepath.Join(dir, "bootstrap-token")
		if err := os.WriteFile(f, []byte("  tok123\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := validConfigForRole(RoleRegistry)
		cfg.Enrollment.BootstrapTokens = []BootstrapToken{{TokenFile: f}}
		if err := cfg.resolve(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Enrollment.BootstrapTokens[0].Token != "tok123" {
			t.Errorf("want %q, got %q", "tok123", cfg.Enrollment.BootstrapTokens[0].Token)
		}
	})

	t.Run("mutually exclusive", func(t *testing.T) {
		f := filepath.Join(dir, "bootstrap-token2")
		if err := os.WriteFile(f, []byte("tok456"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := validConfigForRole(RoleRegistry)
		cfg.Enrollment.BootstrapTokens = []BootstrapToken{{Token: "existing", TokenFile: f}}
		if err := cfg.resolve(); err == nil {
			t.Fatal("expected error for both token and token_file set")
		}
	})
}

func TestConfigResolve_OperatorTokenFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "operator-token")
	if err := os.WriteFile(file, []byte(" operator-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := validConfigForRole(RoleAPI)
	cfg.OperatorToken = ""
	cfg.OperatorTokenFile = file
	if err := cfg.resolve(); err != nil {
		t.Fatal(err)
	}
	if cfg.OperatorToken != "operator-token" {
		t.Fatalf("operator token = %q", cfg.OperatorToken)
	}
}
