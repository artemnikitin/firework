package controlplane

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestVisibilityAssetsDisableBrowserCaching(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	noCache(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(recorder, request)
	if got := recorder.Header().Get("Cache-Control"); got != "no-cache, must-revalidate" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestVisibilityWebUsesServiceObservationAndCapacityBars(t *testing.T) {
	data, err := visibilityWeb.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, want := range []string{"service.service_observed_at", "<progress", "service.actual_node", "node.storage?.local", "service.storage", "serviceDisk", "'B', 'KiB', 'MiB', 'GiB', 'TiB'", "service.public_url", "rel=\"noopener noreferrer\""} {
		if !strings.Contains(script, want) {
			t.Errorf("embedded UI is missing %q", want)
		}
	}
	if strings.Contains(script, "service.actual_node || service.node") {
		t.Fatal("embedded UI still presents desired placement as the actual node")
	}
}
