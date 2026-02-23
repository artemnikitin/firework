package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type mockStatus struct{}

func (m *mockStatus) Status() map[string]any {
	return map[string]any{
		"node":          "test-node",
		"last_revision": "abc123",
		"services": []map[string]any{
			{
				"name":  "web",
				"state": "running",
				"pid":   1234,
			},
		},
	}
}

type mockHealth struct{}

func (m *mockHealth) Results() map[string]any {
	return map[string]any{
		"web": map[string]any{
			"status":   "healthy",
			"failures": 0,
		},
	}
}

type mockMetrics struct{}

func (m *mockMetrics) MetricsText() string {
	return "# HELP firework_agent_up Agent up metric.\n# TYPE firework_agent_up gauge\nfirework_agent_up 1\n"
}

func noopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestHandleHealthz(t *testing.T) {
	srv := NewServer(":0", noopLogger(), &mockStatus{}, &mockHealth{}, &mockMetrics{})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	srv.handleHealthz(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp["status"] != "ok" {
		t.Errorf("expected status ok, got %v", resp["status"])
	}
	if _, ok := resp["time"]; !ok {
		t.Error("expected time field in response")
	}
}

func TestHandleStatus(t *testing.T) {
	srv := NewServer(":0", noopLogger(), &mockStatus{}, &mockHealth{}, &mockMetrics{})

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()

	srv.handleStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp["node"] != "test-node" {
		t.Errorf("expected node test-node, got %v", resp["node"])
	}
	if resp["last_revision"] != "abc123" {
		t.Errorf("expected revision abc123, got %v", resp["last_revision"])
	}

	// Should include health checks merged in.
	if _, ok := resp["health_checks"]; !ok {
		t.Error("expected health_checks in status response")
	}
}

func TestHandleHealth(t *testing.T) {
	srv := NewServer(":0", noopLogger(), &mockStatus{}, &mockHealth{}, &mockMetrics{})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	srv.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	checks, ok := resp["checks"].(map[string]any)
	if !ok {
		t.Fatal("expected checks object in response")
	}

	web, ok := checks["web"].(map[string]any)
	if !ok {
		t.Fatal("expected web in checks")
	}
	if web["status"] != "healthy" {
		t.Errorf("expected web healthy, got %v", web["status"])
	}
}

func TestHandleHealth_NilProvider(t *testing.T) {
	srv := NewServer(":0", noopLogger(), &mockStatus{}, nil, &mockMetrics{})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	srv.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestHandleStatus_ContentType(t *testing.T) {
	srv := NewServer(":0", noopLogger(), &mockStatus{}, &mockHealth{}, &mockMetrics{})

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()

	srv.handleStatus(w, req)

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", ct)
	}
}

func TestHandleMetrics(t *testing.T) {
	srv := NewServer(":0", noopLogger(), &mockStatus{}, &mockHealth{}, &mockMetrics{})

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()

	srv.handleMetrics(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("unexpected content type: %s", ct)
	}
	if !strings.Contains(w.Body.String(), "firework_agent_up 1") {
		t.Fatalf("metrics body does not contain expected sample: %q", w.Body.String())
	}
}
