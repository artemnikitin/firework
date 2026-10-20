package controlplane

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/operatorapi"
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

func TestVisibilityLoginBodyLimit(t *testing.T) {
	cfg := validConfigForRole(RoleAPI)
	server := NewVisibilityServer(cfg, newBlobStateStore(newMemBlob()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	validForm := url.Values{"token": {cfg.OperatorToken}}.Encode()
	paddedForm := validForm + "&padding="
	atLimit := paddedForm + strings.Repeat("a", maxLoginFormBytes-len(paddedForm))
	tests := []struct {
		name          string
		body          string
		unknownLength bool
		wantStatus    int
		wantLocation  string
		wantSession   bool
	}{
		{name: "valid token", body: validForm, wantStatus: http.StatusSeeOther, wantLocation: "/", wantSession: true},
		{name: "valid token at limit", body: atLimit, wantStatus: http.StatusSeeOther, wantLocation: "/", wantSession: true},
		{name: "invalid token", body: "token=wrong", wantStatus: http.StatusSeeOther, wantLocation: "/login?error=1"},
		{name: "malformed form", body: validForm + "&padding=%ZZ", wantStatus: http.StatusSeeOther, wantLocation: "/login?error=1"},
		{name: "valid token above limit", body: atLimit + "a", wantStatus: http.StatusRequestEntityTooLarge},
		{name: "invalid token above limit", body: "token=" + strings.Repeat("a", maxLoginFormBytes), wantStatus: http.StatusRequestEntityTooLarge},
		{name: "unknown length above limit", body: atLimit + "a", unknownLength: true, wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(tt.body))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tt.unknownLength {
				request.ContentLength = -1
			}
			recorder := httptest.NewRecorder()
			server.handleLogin(recorder, request)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if got := recorder.Header().Get("Location"); got != tt.wantLocation {
				t.Errorf("Location = %q, want %q", got, tt.wantLocation)
			}
			cookies := recorder.Result().Cookies()
			if tt.wantSession {
				if len(cookies) != 1 {
					t.Fatalf("cookies = %v, want one session cookie", cookies)
				}
				authenticated := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
				authenticated.AddCookie(cookies[0])
				if !server.authorized(authenticated) {
					t.Error("login cookie does not authorize requests")
				}
			} else if len(cookies) != 0 {
				t.Errorf("rejected login set cookies: %v", cookies)
			}
			if tt.wantStatus == http.StatusRequestEntityTooLarge {
				var response struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if response.Error != "request_too_large" {
					t.Errorf("error = %q, want request_too_large", response.Error)
				}
			}
		})
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

func TestVisibilityStatusEndpointReturnsRevisionDerivation(t *testing.T) {
	cfg := validConfigForRole(RoleAPI)
	server := NewVisibilityServer(cfg, newBlobStateStore(newMemBlob()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	recorder := httptest.NewRecorder()
	server.handleStatus(recorder, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status endpoint returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var status operatorapi.RevisionStatus
	if err := json.NewDecoder(recorder.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.Phase != "unknown" || status.ReasonCode != "desired_revision_missing" {
		t.Fatalf("unexpected empty deployment status: %#v", status)
	}
}

func TestVisibilityWebUsesServiceObservationAndCapacityBars(t *testing.T) {
	data, err := visibilityWeb.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, want := range []string{"service.service_observed_at", "<progress", "service.actual_node", "node.storage?.local", "service.storage", "serviceDisk", "service-disk", "'B', 'KiB', 'MiB', 'GiB', 'TiB'", "service.public_url", "rel=\"noopener noreferrer\"", "/v1/status", "Promise.allSettled", "Revision convergence"} {
		if !strings.Contains(script, want) {
			t.Errorf("embedded UI is missing %q", want)
		}
	}
	if strings.Contains(script, "service.actual_node || service.node") {
		t.Fatal("embedded UI still presents desired placement as the actual node")
	}
}
