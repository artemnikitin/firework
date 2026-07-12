package controlplane

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// RegistryServer serves node enrollment and registry APIs.
type RegistryServer struct {
	cfg         Config
	store       StateStore
	logger      *slog.Logger
	signer      *NodeCertSigner
	tokenToNode map[string]string
}

// NewRegistryServer creates a registry API server.
func NewRegistryServer(cfg Config, store StateStore, logger *slog.Logger) (*RegistryServer, error) {
	signer, err := LoadNodeCertSigner(cfg.Enrollment.CAFile, cfg.Enrollment.CAKeyFile, cfg.Enrollment.NodeCertTTL)
	if err != nil {
		return nil, err
	}
	tokenToNode := make(map[string]string, len(cfg.Enrollment.BootstrapTokens))
	for _, t := range cfg.Enrollment.BootstrapTokens {
		if strings.TrimSpace(t.Token) == "" {
			continue
		}
		tokenToNode[t.Token] = strings.TrimSpace(t.NodeID)
	}
	return &RegistryServer{
		cfg:         cfg,
		store:       store,
		logger:      logger,
		signer:      signer,
		tokenToNode: tokenToNode,
	}, nil
}

// HTTPServer builds the registry HTTPS server.
func (s *RegistryServer) HTTPServer() (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC()})
	})
	mux.HandleFunc("POST /v1/nodes/enroll", s.handleEnroll)
	mux.HandleFunc("POST /v1/nodes/renew", s.handleRenew)
	mux.HandleFunc("POST /v1/nodes/register", s.handleRegister)
	mux.HandleFunc("POST /v1/nodes/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("POST /v1/nodes/{id}/state", s.handleState)

	tlsCfg, err := s.registryTLSConfig()
	if err != nil {
		return nil, err
	}

	return &http.Server{
		Addr:              s.cfg.RegistryListenAddr,
		Handler:           mux,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}, nil
}

func (s *RegistryServer) registryTLSConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading tls keypair: %w", err)
	}
	caPEM, err := os.ReadFile(s.cfg.TLS.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("reading client ca file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parsing client ca pem")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		Certificates: []tls.Certificate{
			cert,
		},
		// Enroll endpoint is called before nodes have a cert, so we accept
		// cert-less TLS connections and enforce mTLS per-handler where required.
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  pool,
	}, nil
}

func (s *RegistryServer) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req EnrollRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	req.NodeID = strings.TrimSpace(req.NodeID)
	if req.NodeID == "" || req.CSRPEM == "" || req.BootstrapToken == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "node_id, bootstrap_token and csr_pem are required"})
		return
	}

	boundNode, ok := s.tokenToNode[req.BootstrapToken]
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid bootstrap token"})
		return
	}
	if boundNode != "" && boundNode != req.NodeID {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bootstrap token not valid for this node_id"})
		return
	}

	certPEM, expiresAt, err := s.signer.SignCSR(req.NodeID, req.CSRPEM)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, CertResponse{
		CertPEM:   certPEM,
		ExpiresAt: expiresAt,
	})
}

func (s *RegistryServer) handleRenew(w http.ResponseWriter, r *http.Request) {
	nodeID, err := requireNodeIdentity(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	var req RenewRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if strings.TrimSpace(req.CSRPEM) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "csr_pem is required"})
		return
	}
	certPEM, expiresAt, err := s.signer.SignCSR(nodeID, req.CSRPEM)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, CertResponse{
		CertPEM:   certPEM,
		ExpiresAt: expiresAt,
	})
}

func (s *RegistryServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	certNodeID, err := requireNodeIdentity(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	var req NodeRegisterRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.NodeID == "" || req.Generation == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "node_id and generation are required"})
		return
	}
	if req.NodeID != certNodeID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "node_id does not match mTLS identity"})
		return
	}
	if req.State == "" {
		req.State = NodeStateReady
	}

	rec, err := s.upsertNodeRecord(r.Context(), req.NodeID, func(cur *NodeRecord) error {
		if cur.Generation > req.Generation {
			return errStaleGeneration
		}
		now := time.Now().UTC()
		if cur.RegisteredAt.IsZero() || req.Generation > cur.Generation {
			cur.RegisteredAt = now
		}
		cur.NodeID = req.NodeID
		cur.Generation = req.Generation
		cur.State = req.State
		cur.Labels = req.Labels
		cur.Capacity = req.Capacity
		cur.HostIP = req.HostIP
		cur.LastSeenAt = now
		cur.UpdatedAt = now
		return nil
	})
	if err != nil {
		if errors.Is(err, errStaleGeneration) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "stale generation"})
			return
		}
		s.logger.Error("register failed", "node", req.NodeID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to persist node"})
		return
	}
	writeJSON(w, http.StatusOK, NodeResponse{
		NodeID:     rec.NodeID,
		Generation: rec.Generation,
		State:      rec.State,
		LastSeenAt: rec.LastSeenAt,
	})
}

func (s *RegistryServer) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	certNodeID, err := requireNodeIdentity(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	var req NodeHeartbeatRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.NodeID == "" || req.Generation == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "node_id and generation are required"})
		return
	}
	if req.NodeID != certNodeID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "node_id does not match mTLS identity"})
		return
	}

	rec, err := s.upsertNodeRecord(r.Context(), req.NodeID, func(cur *NodeRecord) error {
		if cur.Generation > req.Generation {
			return errStaleGeneration
		}
		now := time.Now().UTC()
		cur.NodeID = req.NodeID
		cur.Generation = req.Generation
		if cur.State == "" {
			cur.State = NodeStateReady
		}
		if cur.RegisteredAt.IsZero() {
			cur.RegisteredAt = now
		}
		if req.Capacity.VCPUs > 0 {
			cur.Capacity.VCPUs = req.Capacity.VCPUs
		}
		if req.Capacity.MemoryMB > 0 {
			cur.Capacity.MemoryMB = req.Capacity.MemoryMB
		}
		cur.Used = req.Used
		if req.HostIP != "" {
			cur.HostIP = req.HostIP
		}
		cur.LastSeenAt = now
		cur.UpdatedAt = now
		return nil
	})
	if err != nil {
		if errors.Is(err, errStaleGeneration) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "stale generation"})
			return
		}
		s.logger.Error("heartbeat failed", "node", req.NodeID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to persist node"})
		return
	}
	writeJSON(w, http.StatusOK, NodeResponse{
		NodeID:     rec.NodeID,
		Generation: rec.Generation,
		State:      rec.State,
		LastSeenAt: rec.LastSeenAt,
	})
}

func (s *RegistryServer) handleState(w http.ResponseWriter, r *http.Request) {
	certNodeID, err := requireNodeIdentity(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	nodeID := r.PathValue("id")
	if nodeID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing node id"})
		return
	}
	if nodeID != certNodeID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "path node id does not match mTLS identity"})
		return
	}
	var req NodeStateRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.State != NodeStateReady && req.State != NodeStateDraining && req.State != NodeStateDown {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid node state"})
		return
	}

	rec, err := s.upsertNodeRecord(r.Context(), nodeID, func(cur *NodeRecord) error {
		now := time.Now().UTC()
		cur.NodeID = nodeID
		if cur.Generation == 0 {
			cur.Generation = now.UnixNano()
		}
		if cur.RegisteredAt.IsZero() {
			cur.RegisteredAt = now
		}
		cur.State = req.State
		cur.LastSeenAt = now
		cur.UpdatedAt = now
		return nil
	})
	if err != nil {
		s.logger.Error("state update failed", "node", nodeID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to persist node"})
		return
	}
	writeJSON(w, http.StatusOK, NodeResponse{
		NodeID:     rec.NodeID,
		Generation: rec.Generation,
		State:      rec.State,
		LastSeenAt: rec.LastSeenAt,
	})
}

var errStaleGeneration = errors.New("stale generation")

func (s *RegistryServer) upsertNodeRecord(ctx context.Context, nodeID string, mutator func(*NodeRecord) error) (*NodeRecord, error) {
	key, err := nodeRecordKey(s.cfg.State.Prefix, nodeID)
	if err != nil {
		return nil, err
	}

	for i := 0; i < 6; i++ {
		var current NodeRecord
		token, exists, err := s.store.GetJSON(ctx, key, &current)
		if err != nil {
			return nil, err
		}
		if !exists {
			current = NodeRecord{NodeID: nodeID}
		}
		if err := mutator(&current); err != nil {
			return nil, err
		}

		if !exists {
			ok, _, err := s.store.PutJSONIfAbsent(ctx, key, current)
			if err != nil {
				return nil, err
			}
			if ok {
				return &current, nil
			}
			continue
		}
		ok, _, err := s.store.PutJSONIfMatch(ctx, key, token, current)
		if err != nil {
			return nil, err
		}
		if ok {
			return &current, nil
		}
	}
	return nil, fmt.Errorf("too many concurrent updates for node %q", nodeID)
}

func requireNodeIdentity(r *http.Request) (string, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", fmt.Errorf("client certificate is required")
	}
	nodeID := nodeIDFromCertificate(r.TLS.PeerCertificates[0])
	if strings.TrimSpace(nodeID) == "" {
		return "", fmt.Errorf("unable to derive node identity from certificate")
	}
	return nodeID, nil
}
