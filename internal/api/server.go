package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// StatusProvider is an interface for getting agent status.
type StatusProvider interface {
	Status() map[string]any
}

// HealthResultsProvider is an interface for getting health check results.
type HealthResultsProvider interface {
	Results() map[string]any
}

// MetricsProvider is an interface that renders metrics in text format.
type MetricsProvider interface {
	MetricsText() string
}

// Server is a lightweight HTTP API that exposes agent status and health
// check results.
type Server struct {
	addr    string
	logger  *slog.Logger
	status  StatusProvider
	health  HealthResultsProvider
	metrics MetricsProvider
	httpSrv *http.Server
}

// NewServer creates a new API server.
func NewServer(addr string, logger *slog.Logger, status StatusProvider, health HealthResultsProvider, metrics MetricsProvider) *Server {
	return &Server{
		addr:    addr,
		logger:  logger,
		status:  status,
		health:  health,
		metrics: metrics,
	}
}

// Start starts the HTTP server in a goroutine. Call Stop() to shut it down.
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	if s.metrics != nil {
		mux.HandleFunc("GET /metrics", s.handleMetrics)
	}

	s.httpSrv = &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	s.logger.Info("starting API server", "addr", s.addr)

	go func() {
		if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("API server error", "error", err)
		}
	}()

	return nil
}

// Stop gracefully shuts down the HTTP server.
func (s *Server) Stop(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	s.logger.Info("stopping API server")
	return s.httpSrv.Shutdown(ctx)
}

// handleStatus returns the full agent status including running services.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	status := s.status.Status()

	// Merge health check results into the status if available.
	if s.health != nil {
		status["health_checks"] = s.health.Results()
	}

	s.writeJSON(w, http.StatusOK, status)
}

// handleHealth returns just the health check results.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.health == nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"checks": map[string]any{}})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"checks": s.health.Results(),
	})
}

// handleHealthz is a simple liveness probe for the agent itself.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

// handleMetrics returns Prometheus/OpenMetrics text exposition.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.metrics == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if _, err := w.Write([]byte(s.metrics.MetricsText())); err != nil {
		s.logger.Error("failed to write metrics response", "error", err)
	}
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(data); err != nil {
		s.logger.Error("failed to encode JSON response", "error", err)
	}
}
