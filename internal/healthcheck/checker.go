package healthcheck

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

// Status represents the health status of a service.
type Status string

const (
	StatusHealthy   Status = "healthy"
	StatusUnhealthy Status = "unhealthy"
	StatusUnknown   Status = "unknown"
)

// Result holds the outcome of a health check.
type Result struct {
	Service     string
	Status      Status
	LastChecked time.Time
	Failures    int
	LastError   string
}

// RestartFunc is called when a service needs to be restarted due to health
// check failures. It receives the service name.
type RestartFunc func(ctx context.Context, name string) error

// Monitor runs periodic health checks for all configured services and
// triggers restarts when a service becomes unhealthy.
type Monitor struct {
	logger    *slog.Logger
	restartFn RestartFunc

	mu      sync.Mutex
	checks  map[string]*serviceCheck
	results map[string]*Result
}

// serviceCheck tracks the goroutine and config for a single service's health check.
type serviceCheck struct {
	svc    config.ServiceConfig
	cancel context.CancelFunc
}

// NewMonitor creates a new health check monitor.
func NewMonitor(logger *slog.Logger, restartFn RestartFunc) *Monitor {
	return &Monitor{
		logger:    logger,
		restartFn: restartFn,
		checks:    make(map[string]*serviceCheck),
		results:   make(map[string]*Result),
	}
}

// Register starts health checking for a service. If the service is already
// registered, it is replaced.
func (m *Monitor) Register(ctx context.Context, svc config.ServiceConfig) {
	if svc.HealthCheck == nil {
		return
	}

	m.Deregister(svc.Name)

	checkCtx, cancel := context.WithCancel(ctx)

	m.mu.Lock()
	m.checks[svc.Name] = &serviceCheck{svc: svc, cancel: cancel}
	m.results[svc.Name] = &Result{
		Service: svc.Name,
		Status:  StatusUnknown,
	}
	m.mu.Unlock()

	m.logger.Info("registered health check",
		"service", svc.Name,
		"type", svc.HealthCheck.Type,
		"interval", svc.HealthCheck.Interval,
	)

	go m.run(checkCtx, svc)
}

// Deregister stops health checking for a service.
func (m *Monitor) Deregister(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if sc, exists := m.checks[name]; exists {
		sc.cancel()
		delete(m.checks, name)
		delete(m.results, name)
	}
}

// Results returns a snapshot of all current health check results.
func (m *Monitor) Results() map[string]Result {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]Result, len(m.results))
	for k, v := range m.results {
		out[k] = *v
	}
	return out
}

// GetResult returns the health check result for a specific service.
func (m *Monitor) GetResult(name string) (Result, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.results[name]
	if !ok {
		return Result{}, false
	}
	return *r, true
}

// Stop stops all health checks.
func (m *Monitor) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, sc := range m.checks {
		sc.cancel()
		delete(m.checks, name)
	}
}

// run is the per-service health check loop.
func (m *Monitor) run(ctx context.Context, svc config.ServiceConfig) {
	hc := svc.HealthCheck

	interval := hc.Interval
	if interval == 0 {
		interval = 10 * time.Second
	}

	// Wait a bit before the first check to give the VM time to boot.
	select {
	case <-ctx.Done():
		return
	case <-time.After(interval):
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Extract guest IP for target composition when Target is not set directly.
	var guestIP string
	if svc.Network != nil {
		guestIP = svc.Network.GuestIP
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := m.check(ctx, hc, guestIP)
			m.recordResult(ctx, svc.Name, hc, err)
		}
	}
}

// check performs a single health check based on the config type.
func (m *Monitor) check(ctx context.Context, hc *config.HealthCheckConfig, guestIP string) error {
	timeout := hc.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}

	target := resolveTarget(hc, guestIP)
	if target == "" {
		return fmt.Errorf("health check has no target (no Target, Port, or guest IP)")
	}

	switch hc.Type {
	case "http":
		return checkHTTP(ctx, target, timeout)
	case "tcp":
		return checkTCP(ctx, target, timeout)
	default:
		return fmt.Errorf("unsupported health check type: %s", hc.Type)
	}
}

// resolveTarget returns the health check target address. If Target is set
// explicitly it is used as-is. Otherwise the target is composed from Port,
// Path, and the guest IP allocated at runtime.
func resolveTarget(hc *config.HealthCheckConfig, guestIP string) string {
	if hc.Target != "" {
		return hc.Target
	}
	if hc.Port == 0 || guestIP == "" {
		return ""
	}
	host := stripCIDR(guestIP)
	switch hc.Type {
	case "http":
		return fmt.Sprintf("http://%s:%d%s", host, hc.Port, hc.Path)
	case "tcp":
		return fmt.Sprintf("%s:%d", host, hc.Port)
	default:
		return ""
	}
}

// stripCIDR removes the /prefix from a CIDR string (e.g. "10.0.0.2/24" → "10.0.0.2").
func stripCIDR(s string) string {
	if idx := strings.Index(s, "/"); idx != -1 {
		return s[:idx]
	}
	return s
}

// recordResult updates the result for a service and triggers a restart if needed.
func (m *Monitor) recordResult(ctx context.Context, name string, hc *config.HealthCheckConfig, err error) {
	m.mu.Lock()
	r, exists := m.results[name]
	if !exists {
		m.mu.Unlock()
		return
	}
	tenant := "shared"
	if sc, ok := m.checks[name]; ok {
		if t := strings.TrimSpace(sc.svc.Metadata["tenant"]); t != "" {
			tenant = t
		}
	}

	r.LastChecked = time.Now()

	if err != nil {
		r.Failures++
		r.LastError = err.Error()
		r.Status = StatusUnhealthy
		m.logger.Warn("health check failed",
			"service", name,
			"tenant", tenant,
			"failures", r.Failures,
			"error", err,
		)
	} else {
		if r.Status == StatusUnhealthy {
			m.logger.Info("service recovered", "service", name, "tenant", tenant)
		}
		r.Failures = 0
		r.LastError = ""
		r.Status = StatusHealthy
	}

	retries := hc.Retries
	if retries == 0 {
		retries = 3
	}
	needsRestart := r.Failures >= retries
	m.mu.Unlock()

	if needsRestart {
		m.logger.Warn("service exceeded failure threshold, restarting",
			"service", name,
			"tenant", tenant,
			"failures", r.Failures,
			"threshold", retries,
		)

		if restartErr := m.restartFn(ctx, name); restartErr != nil {
			m.logger.Error("failed to restart service", "service", name, "tenant", tenant, "error", restartErr)
		} else {
			m.mu.Lock()
			r.Failures = 0
			r.Status = StatusUnknown
			m.mu.Unlock()
		}
	}
}

// checkHTTP performs an HTTP GET and expects a 2xx response.
func checkHTTP(ctx context.Context, target string, timeout time.Duration) error {
	client := &http.Client{Timeout: timeout}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP check returned status %d", resp.StatusCode)
	}
	return nil
}

// checkTCP attempts to open a TCP connection to the target.
func checkTCP(ctx context.Context, target string, timeout time.Duration) error {
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		return fmt.Errorf("TCP check failed: %w", err)
	}
	conn.Close()
	return nil
}
