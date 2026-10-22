package controlplane

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

func TestVerifyGitHubSignature(t *testing.T) {
	secret := "super-secret"
	body := []byte(`{"zen":"keep it logically awesome"}`)

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	h := http.Header{}
	h.Set("X-Hub-Signature-256", signature)
	if err := verifyGitHubSignature(secret, body, h); err != nil {
		t.Fatalf("expected valid signature, got error: %v", err)
	}

	h.Set("X-Hub-Signature-256", "sha256=bad")
	if err := verifyGitHubSignature(secret, body, h); err == nil {
		t.Fatal("expected invalid signature error")
	}
}

func TestFlattenNodeConfigs_PreservesNodeType(t *testing.T) {
	services := flattenNodeConfigs([]config.NodeConfig{
		{Node: "backend", Services: []config.ServiceConfig{{Name: "db", NodeType: "backend"}}},
		{Node: "web", Services: []config.ServiceConfig{{Name: "ui", NodeType: "web"}}},
	})
	got := map[string]string{}
	for _, service := range services {
		got[service.Name] = service.NodeType
	}
	if got["db"] != "backend" || got["ui"] != "web" {
		t.Fatalf("flattening must keep each service's node_type as its placement requirement, got %v", got)
	}
}
