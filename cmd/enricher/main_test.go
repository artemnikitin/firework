package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyGitHubWebhookSignature_Valid(t *testing.T) {
	secret := "super-secret"
	body := []byte(`{"ref":"refs/heads/main"}`)
	headers := map[string]string{
		"x-hub-signature-256": sign(secret, body),
	}

	if err := verifyGitHubWebhookSignature(secret, body, headers); err != nil {
		t.Fatalf("expected valid signature, got error: %v", err)
	}
}

func TestVerifyGitHubWebhookSignature_MissingHeader(t *testing.T) {
	err := verifyGitHubWebhookSignature("secret", []byte("payload"), map[string]string{})
	if err == nil {
		t.Fatal("expected error for missing signature header")
	}
	if !strings.Contains(err.Error(), "missing X-Hub-Signature-256") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVerifyGitHubWebhookSignature_Invalid(t *testing.T) {
	headers := map[string]string{
		"X-Hub-Signature-256": "sha256=deadbeef",
	}
	err := verifyGitHubWebhookSignature("secret", []byte("payload"), headers)
	if err == nil {
		t.Fatal("expected error for invalid signature")
	}
	if !strings.Contains(err.Error(), "invalid X-Hub-Signature-256") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHeaderValue_CaseInsensitive(t *testing.T) {
	headers := map[string]string{
		"x-hub-signature-256": "value-1",
		"X-Other":             "value-2",
	}
	if got := headerValue(headers, "X-Hub-Signature-256"); got != "value-1" {
		t.Fatalf("expected value-1, got %q", got)
	}
}

func TestIsScheduledEvent(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "eventbridge scheduled event",
			raw:  `{"source":"aws.events","detail-type":"Scheduled Event","detail":{}}`,
			want: true,
		},
		{
			name: "github webhook",
			raw:  `{"ref":"refs/heads/main","repository":{"clone_url":"https://github.com/foo/bar"}}`,
			want: false,
		},
		{
			name: "api gateway v2 envelope",
			raw:  `{"version":"2.0","body":"{}", "headers":{}}`,
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isScheduledEvent(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Fatalf("isScheduledEvent(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
