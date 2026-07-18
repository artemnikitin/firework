package controlplane

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVisibilityServerAuthentication(t *testing.T) {
	cfg := validConfigForRole(RoleAPI)
	server := NewVisibilityServer(cfg, newBlobStateStore(newMemBlob()), slog.New(slog.NewTextHandler(io.Discard, nil)))

	request := httptest.NewRequest(http.MethodGet, "/v1/nodes", nil)
	if server.authorized(request) {
		t.Fatal("request without credentials was authorized")
	}
	request.Header.Set("Authorization", "Bearer wrong")
	if server.authorized(request) {
		t.Fatal("wrong bearer token was authorized")
	}
	request.Header.Set("Authorization", "Bearer "+cfg.OperatorToken)
	if !server.authorized(request) {
		t.Fatal("valid bearer token was rejected")
	}

}
