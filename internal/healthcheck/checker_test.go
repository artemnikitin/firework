package healthcheck

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

func TestCheckHTTP_Healthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	err := checkHTTP(context.Background(), srv.URL+"/health", 5*time.Second)
	if err != nil {
		t.Errorf("expected healthy, got error: %v", err)
	}
}

func TestCheckHTTP_Unhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := checkHTTP(context.Background(), srv.URL+"/health", 5*time.Second)
	if err == nil {
		t.Error("expected error for 503 response")
	}
}

func TestCheckHTTP_ConnectionRefused(t *testing.T) {
	err := checkHTTP(context.Background(), "http://127.0.0.1:1", 1*time.Second)
	if err == nil {
		t.Error("expected error for refused connection")
	}
}

func TestCheckTCP_Healthy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()

	// Accept connections in background.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	err = checkTCP(context.Background(), ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Errorf("expected healthy, got error: %v", err)
	}
}

func TestCheckTCP_Unhealthy(t *testing.T) {
	err := checkTCP(context.Background(), "127.0.0.1:1", 1*time.Second)
	if err == nil {
		t.Error("expected error for refused connection")
	}
}

func TestMonitor_RegisterAndResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	restartCalled := false
	var mu sync.Mutex
	restartFn := func(ctx context.Context, name string) error {
		mu.Lock()
		restartCalled = true
		mu.Unlock()
		return nil
	}

	mon := NewMonitor(noopLogger(), restartFn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc := config.ServiceConfig{
		Name: "test-svc",
		HealthCheck: &config.HealthCheckConfig{
			Type:     "http",
			Target:   srv.URL + "/health",
			Interval: 100 * time.Millisecond,
			Timeout:  1 * time.Second,
			Retries:  3,
		},
	}

	mon.Register(ctx, svc)

	// Wait for at least one check to complete.
	time.Sleep(400 * time.Millisecond)

	results := mon.Results()
	r, ok := results["test-svc"]
	if !ok {
		t.Fatal("expected result for test-svc")
	}
	if r.Status != StatusHealthy {
		t.Errorf("expected healthy, got %s", r.Status)
	}

	mu.Lock()
	if restartCalled {
		t.Error("restart should not have been called for healthy service")
	}
	mu.Unlock()

	mon.Stop()
}

func TestMonitor_Deregister(t *testing.T) {
	mon := NewMonitor(noopLogger(), func(ctx context.Context, name string) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc := config.ServiceConfig{
		Name: "temp-svc",
		HealthCheck: &config.HealthCheckConfig{
			Type:     "tcp",
			Target:   "127.0.0.1:1",
			Interval: 1 * time.Second,
			Timeout:  1 * time.Second,
			Retries:  3,
		},
	}

	mon.Register(ctx, svc)

	results := mon.Results()
	if _, ok := results["temp-svc"]; !ok {
		t.Fatal("expected result after register")
	}

	mon.Deregister("temp-svc")

	results = mon.Results()
	if _, ok := results["temp-svc"]; ok {
		t.Error("expected no result after deregister")
	}
}

func TestResolveTarget_ExplicitTarget(t *testing.T) {
	hc := &config.HealthCheckConfig{
		Type:   "http",
		Target: "http://10.0.0.1:8080/health",
		Port:   9090,
		Path:   "/other",
	}
	got := resolveTarget(hc, "10.0.0.2")
	if got != "http://10.0.0.1:8080/health" {
		t.Errorf("expected explicit target, got %q", got)
	}
}

func TestResolveTarget_HTTPFromPortPath(t *testing.T) {
	hc := &config.HealthCheckConfig{
		Type: "http",
		Port: 8080,
		Path: "/health",
	}
	got := resolveTarget(hc, "10.0.0.5")
	if got != "http://10.0.0.5:8080/health" {
		t.Errorf("expected composed URL, got %q", got)
	}
}

func TestResolveTarget_TCPFromPort(t *testing.T) {
	hc := &config.HealthCheckConfig{
		Type: "tcp",
		Port: 3306,
	}
	got := resolveTarget(hc, "172.16.0.2/24")
	if got != "172.16.0.2:3306" {
		t.Errorf("expected stripped CIDR, got %q", got)
	}
}

func TestResolveTarget_NoPortNoTarget(t *testing.T) {
	hc := &config.HealthCheckConfig{Type: "http"}
	got := resolveTarget(hc, "10.0.0.1")
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestResolveTarget_NoGuestIP(t *testing.T) {
	hc := &config.HealthCheckConfig{Type: "http", Port: 8080}
	got := resolveTarget(hc, "")
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestMonitor_SkipNilHealthCheck(t *testing.T) {
	mon := NewMonitor(noopLogger(), func(ctx context.Context, name string) error { return nil })
	ctx := context.Background()

	// Service without health check should be a no-op.
	svc := config.ServiceConfig{
		Name:        "no-check",
		HealthCheck: nil,
	}

	mon.Register(ctx, svc)

	results := mon.Results()
	if _, ok := results["no-check"]; ok {
		t.Error("should not register a result for a service without health check")
	}
}

func TestMonitor_RestartOnFailure(t *testing.T) {
	// Use a port that's guaranteed to refuse connections.
	restartCount := 0
	var mu sync.Mutex
	restartFn := func(ctx context.Context, name string) error {
		mu.Lock()
		restartCount++
		mu.Unlock()
		return nil
	}

	mon := NewMonitor(noopLogger(), restartFn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc := config.ServiceConfig{
		Name: "failing-svc",
		HealthCheck: &config.HealthCheckConfig{
			Type:     "tcp",
			Target:   "127.0.0.1:1", // will fail
			Interval: 50 * time.Millisecond,
			Timeout:  50 * time.Millisecond,
			Retries:  2,
		},
	}

	mon.Register(ctx, svc)

	// Wait for initial delay + enough checks to trigger restart.
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	rc := restartCount
	mu.Unlock()

	if rc == 0 {
		t.Error("expected restart to be triggered after consecutive failures")
	}

	mon.Stop()
}
