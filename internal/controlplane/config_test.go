package controlplane

import "testing"

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
