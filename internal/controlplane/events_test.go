package controlplane

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
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
