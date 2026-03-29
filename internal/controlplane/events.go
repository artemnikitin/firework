package controlplane

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/enricher"
)

// EventsServer serves GitHub event ingestion endpoints.
type EventsServer struct {
	cfg    Config
	store  *S3StateStore
	logger *slog.Logger
}

// NewEventsServer creates a new events server.
func NewEventsServer(cfg Config, store *S3StateStore, logger *slog.Logger) *EventsServer {
	return &EventsServer{
		cfg:    cfg,
		store:  store,
		logger: logger,
	}
}

// HTTPServer builds the events HTTPS server.
func (s *EventsServer) HTTPServer() (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC()})
	})
	mux.HandleFunc("POST /v1/events/github", s.handleGitHubEvent)

	tlsCfg, err := serverTLSConfig(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
	if err != nil {
		return nil, err
	}

	return &http.Server{
		Addr:              s.cfg.EventsListenAddr,
		Handler:           mux,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}, nil
}

type gitHubWebhookPayload struct {
	Ref        string `json:"ref"`
	Repository struct {
		CloneURL string `json:"clone_url"`
	} `json:"repository"`
}

func (s *EventsServer) handleGitHubEvent(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}
	_ = r.Body.Close()

	if err := verifyGitHubSignature(s.cfg.GitHubWebhookSecret, body, r.Header); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}

	eventType := headerValue(r.Header, "X-GitHub-Event")
	if eventType != "push" {
		writeJSON(w, http.StatusAccepted, map[string]any{"ignored": true, "reason": "unsupported event type"})
		return
	}

	deliveryID := headerValue(r.Header, "X-GitHub-Delivery")
	claimedDeliveryID := ""
	if deliveryID != "" {
		ok, err := s.markEventOnce(r.Context(), deliveryID)
		if err != nil {
			s.logger.Error("event dedupe write failed", "delivery_id", deliveryID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to persist dedupe marker"})
			return
		}
		if !ok {
			writeJSON(w, http.StatusOK, map[string]any{"deduped": true, "delivery_id": deliveryID})
			return
		}
		claimedDeliveryID = deliveryID
	}

	var payload gitHubWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid github payload"})
		return
	}
	branch := branchFromRef(payload.Ref)
	if branch != s.cfg.TargetBranch {
		writeJSON(w, http.StatusOK, map[string]any{
			"ignored": true,
			"reason":  "non-target branch",
			"branch":  branch,
			"target":  s.cfg.TargetBranch,
		})
		return
	}
	if payload.Repository.CloneURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repository.clone_url is required"})
		return
	}

	desiredRev, err := s.buildDesiredRevision(r.Context(), payload.Repository.CloneURL, branch, deliveryID)
	if err != nil {
		s.releaseEventClaim(r.Context(), claimedDeliveryID)
		s.logger.Error("building desired revision failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	if err := s.publishDesiredRevision(r.Context(), desiredRev); err != nil {
		s.releaseEventClaim(r.Context(), claimedDeliveryID)
		s.logger.Error("publishing desired revision failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"revision": desiredRev.Revision,
		"services": len(desiredRev.Services),
	})
}

func (s *EventsServer) releaseEventClaim(ctx context.Context, deliveryID string) {
	if deliveryID == "" {
		return
	}
	cleanupCtx := ctx
	if err := cleanupCtx.Err(); err != nil {
		var cancel context.CancelFunc
		cleanupCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	key := dedupeKey(s.cfg.State.Prefix, deliveryID)
	if err := s.store.Delete(cleanupCtx, key); err != nil {
		s.logger.Warn("failed to release dedupe marker after processing error", "delivery_id", deliveryID, "error", err)
	}
}

func (s *EventsServer) markEventOnce(ctx context.Context, deliveryID string) (bool, error) {
	key := dedupeKey(s.cfg.State.Prefix, deliveryID)
	ok, _, err := s.store.PutJSONIfAbsent(ctx, key, EventDedupeMarker{
		EventID:    deliveryID,
		ReceivedAt: time.Now().UTC(),
	})
	return ok, err
}

func (s *EventsServer) buildDesiredRevision(ctx context.Context, repoURL, branch, source string) (*DesiredRevision, error) {
	reader, err := enricher.NewGitReader(ctx, repoURL, branch)
	if err != nil {
		return nil, fmt.Errorf("cloning repo: %w", err)
	}
	defer reader.Close()

	inputDir := reader.Dir()
	if strings.TrimSpace(s.cfg.ConfigDir) != "" {
		inputDir = filepath.Join(inputDir, s.cfg.ConfigDir)
	}
	result, err := enricher.Enrich(inputDir)
	if err != nil {
		return nil, fmt.Errorf("enriching desired state: %w", err)
	}

	services := flattenNodeConfigs(result.NodeConfigs)
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })

	return &DesiredRevision{
		Revision:  newRevision("desired"),
		Source:    source,
		CreatedAt: time.Now().UTC(),
		Services:  services,
	}, nil
}

func (s *EventsServer) publishDesiredRevision(ctx context.Context, desired *DesiredRevision) error {
	revKey := desiredRevisionKey(s.cfg.State.Prefix, desired.Revision)
	if _, err := s.store.PutJSON(ctx, revKey, desired); err != nil {
		return err
	}
	return upsertPointer(ctx, s.store, desiredCurrentKey(s.cfg.State.Prefix), desired.Revision)
}

func flattenNodeConfigs(configs []config.NodeConfig) []config.ServiceConfig {
	var out []config.ServiceConfig
	for _, nc := range configs {
		out = append(out, nc.Services...)
	}
	return out
}

func verifyGitHubSignature(secret string, body []byte, headers http.Header) error {
	signature := strings.TrimSpace(headerValue(headers, "X-Hub-Signature-256"))
	if signature == "" {
		return fmt.Errorf("missing X-Hub-Signature-256 header")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(signature), []byte(expected)) {
		return fmt.Errorf("invalid github signature")
	}
	return nil
}
