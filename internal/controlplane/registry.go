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
	"unicode"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/registryapi"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

// maxRegistryRequestBytes must stay above the largest agent_status a
// well-behaved agent can send. A heartbeat over the cap is truncated by
// MaxBytesReader and fails JSON decoding, so the handler returns 400 before it
// can even reach the req.AgentStatus != nil check in handleHeartbeat that
// would otherwise drop just the telemetry — the node's liveness heartbeat is
// rejected too, LastSeenAt stops advancing, and the node goes stale and then
// down. That fallback only helps once the body has actually decoded.
//
// The bound is driven by MaxServices x config.MaxServiceVolumes = 6400
// volumes, each field bounded in validateVolumeStatus. Message-shaped fields
// (AgentStatus.Message, Condition.Message, ServiceStatus.Message,
// VolumeStatus.LastError) are deliberately excluded from that per-field
// enforcement — they are accept-then-truncate via BoundedMessage, not
// rejected. That holds for any agent built from this codebase: the send path
// (internal/agent/status.go) runs every one of these through BoundedMessage
// before marshaling, so statusmodel.MaxMessageLen is an actual limit on what
// gets sent, not just an assumption; only a non-standard client bypassing
// BoundedMessage could exceed it. Measured worst case at that limit is about
// 13.6 MiB; see TestMaxRegistryRequestBytesExceedsLargestValidHeartbeat, which
// fails if the bounds grow past this cap.
const maxRegistryRequestBytes = 16 << 20

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
		Addr: s.cfg.RegistryListenAddr,
		// The status bounds allow 256 services with up to 25 volume
		// observations each. Keep the request cap above that worst-case JSON
		// while retaining a finite registry request limit.
		Handler:           http.MaxBytesHandler(mux, maxRegistryRequestBytes),
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
	var req registryapi.EnrollRequest
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
	writeJSON(w, http.StatusOK, registryapi.CertResponse{
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
	var req registryapi.RenewRequest
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
	writeJSON(w, http.StatusOK, registryapi.CertResponse{
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
	var req registryapi.RegisterRequest
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
		req.State = registryapi.NodeStateReady
	}

	rec, err := s.upsertNodeRecord(r.Context(), req.NodeID, func(cur *NodeRecord) error {
		if cur.Generation > req.Generation {
			return errStaleGeneration
		}
		now := time.Now().UTC()
		generationChanged := req.Generation > cur.Generation
		if cur.RegisteredAt.IsZero() || generationChanged {
			cur.RegisteredAt = now
		}
		if generationChanged {
			cur.AgentStatus = nil
			cur.Used = Resources{}
		}
		cur.NodeID = req.NodeID
		cur.Generation = req.Generation
		cur.State = NodeState(req.State)
		cur.Labels = req.Labels
		cur.Capacity = resourcesFromRegistry(req.Capacity)
		cur.HostIP = req.HostIP
		cur.Storage = storageFromRegistry(req.Storage)
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
	writeJSON(w, http.StatusOK, nodeResponse(rec))
}

func (s *RegistryServer) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	certNodeID, err := requireNodeIdentity(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	var req registryapi.HeartbeatRequest
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
	if req.AgentStatus != nil {
		validated, err := validatedHeartbeatAgentStatus(req.NodeID, req.AgentStatus)
		if err != nil {
			// Runtime status is optional telemetry. Do not reject the liveness
			// heartbeat when a newer or malformed status payload is unusable;
			// clearing it also prevents stale status from being presented as
			// current.
			s.logger.Warn("invalid agent status; accepting heartbeat without status", "node", req.NodeID, "error", err)
			req.AgentStatus = nil
		} else {
			req.AgentStatus = validated
		}
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
		cur.Used = resourcesFromRegistry(req.Used)
		cur.Storage = storageFromRegistry(req.Storage)
		if req.HostIP != "" {
			cur.HostIP = req.HostIP
		}
		cur.AgentStatus = req.AgentStatus
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
	writeJSON(w, http.StatusOK, nodeResponse(rec))
}

func validatedHeartbeatAgentStatus(nodeID string, incoming *statusmodel.AgentStatus) (*statusmodel.AgentStatus, error) {
	if incoming == nil {
		return nil, nil
	}
	var validated NodeRecord
	if err := applyHeartbeatAgentStatus(&validated, nodeID, incoming); err != nil {
		return nil, err
	}
	return validated.AgentStatus, nil
}

func applyHeartbeatAgentStatus(cur *NodeRecord, nodeID string, incoming *statusmodel.AgentStatus) error {
	if incoming == nil {
		// Older agents omit agent_status. Clear any value left by a newer
		// process immediately instead of presenting cached runtime state as
		// current until its observation timestamp expires.
		cur.AgentStatus = nil
		return nil
	}
	if incoming.SchemaVersion <= 0 {
		return fmt.Errorf("agent_status.schema_version is required")
	}
	if incoming.NodeID != "" && incoming.NodeID != nodeID {
		return fmt.Errorf("agent_status.node does not match node_id")
	}
	status := *incoming
	if status.Phase == "" {
		status.Phase = statusmodel.PhaseUnknown
	}
	status.Conditions = append([]statusmodel.Condition(nil), incoming.Conditions...)
	status.Services = append([]statusmodel.ServiceStatus(nil), incoming.Services...)
	if err := validateAgentStatus(status); err != nil {
		return err
	}
	status.Message = statusmodel.BoundedMessage(status.Message)
	for i := range status.Conditions {
		status.Conditions[i].Message = statusmodel.BoundedMessage(status.Conditions[i].Message)
	}
	for i := range status.Services {
		status.Services[i].Message = statusmodel.BoundedMessage(status.Services[i].Message)
		status.Services[i].Volumes = append([]statusmodel.VolumeStatus(nil), status.Services[i].Volumes...)
		for j := range status.Services[i].Volumes {
			status.Services[i].Volumes[j].LastError = statusmodel.BoundedMessage(status.Services[i].Volumes[j].LastError)
		}
	}
	cur.AgentStatus = &status
	return nil
}

func validateAgentStatus(status statusmodel.AgentStatus) error {
	if status.SchemaVersion <= 0 {
		return fmt.Errorf("agent_status.schema_version is required")
	}
	if len(status.Conditions) > statusmodel.MaxConditions {
		return fmt.Errorf("agent_status has %d conditions; maximum is %d", len(status.Conditions), statusmodel.MaxConditions)
	}
	if len(status.Services) > statusmodel.MaxServices {
		return fmt.Errorf("agent_status has %d services; maximum is %d", len(status.Services), statusmodel.MaxServices)
	}
	if status.DesiredServices < 0 || status.ReadyServices < 0 || status.ReadyServices > status.DesiredServices {
		return fmt.Errorf("agent_status service counts are invalid")
	}
	if !validStatusPhase(status.Phase) {
		return fmt.Errorf("agent_status phase %q is invalid", status.Phase)
	}
	if len(status.AgentVersion) > statusmodel.MaxAgentVersionLen {
		return fmt.Errorf("agent_status.agent_version exceeds %d bytes", statusmodel.MaxAgentVersionLen)
	}
	// status.NodeID is deliberately not length-checked: it must already equal
	// the mTLS-authenticated nodeID (checked above this function, in
	// applyHeartbeatAgentStatus) or be empty, so it contributes nothing to
	// the MaxServices/config.MaxServiceVolumes multiplication this bounding
	// pass exists for — it appears once per heartbeat, not per service or
	// volume — and node IDs are not independently length-bounded at
	// enrollment, so adding a cap here risks rejecting (and so silently
	// dropping the telemetry of) a legitimately longer node ID for no size
	// benefit.
	// Message is deliberately not length-checked here: it is optional
	// free-text telemetry that applyHeartbeatAgentStatus truncates to
	// statusmodel.MaxMessageLen with BoundedMessage after validation succeeds
	// (see TestApplyHeartbeatAgentStatusValidatesIdentityAndBoundsMessages),
	// the same accept-then-truncate contract as Condition.Message,
	// ServiceStatus.Message, and VolumeStatus.LastError below. What
	// maxRegistryRequestBytes actually depends on is bounding the fields that
	// have no truncation fallback — see validateVolumeStatus.
	for _, revision := range []string{status.DesiredRevision, status.PlacementRevision, status.ObservedRevision, status.AppliedRevision} {
		if len(revision) > statusmodel.MaxRevisionLen {
			return fmt.Errorf("agent_status revision exceeds %d bytes", statusmodel.MaxRevisionLen)
		}
	}
	conditionTypes := make(map[string]struct{}, len(status.Conditions))
	for _, condition := range status.Conditions {
		if condition.Type == "" || len(condition.Type) > statusmodel.MaxConditionTypeLen {
			return fmt.Errorf("agent_status condition type is invalid")
		}
		if _, duplicate := conditionTypes[condition.Type]; duplicate {
			return fmt.Errorf("agent_status condition %q is duplicated", condition.Type)
		}
		conditionTypes[condition.Type] = struct{}{}
		if condition.Status != statusmodel.ConditionTrue && condition.Status != statusmodel.ConditionFalse && condition.Status != statusmodel.ConditionUnknown {
			return fmt.Errorf("agent_status condition %q has invalid status %q", condition.Type, condition.Status)
		}
		if !validReasonCode(condition.ReasonCode) {
			return fmt.Errorf("agent_status condition %q has invalid reason code", condition.Type)
		}
	}
	serviceNames := make(map[string]struct{}, len(status.Services))
	for _, service := range status.Services {
		if service.Name == "" || len(service.Name) > statusmodel.MaxServiceNameLen {
			return fmt.Errorf("agent_status service name is invalid")
		}
		if _, duplicate := serviceNames[service.Name]; duplicate {
			return fmt.Errorf("agent_status service %q is duplicated", service.Name)
		}
		serviceNames[service.Name] = struct{}{}
		if !validReasonCode(service.ReasonCode) {
			return fmt.Errorf("agent_status service %q has invalid reason code", service.Name)
		}
		if len(service.VMState) > statusmodel.MaxEnumLen {
			return fmt.Errorf("agent_status service %q vm_state exceeds %d bytes", service.Name, statusmodel.MaxEnumLen)
		}
		if len(service.Health) > statusmodel.MaxEnumLen {
			return fmt.Errorf("agent_status service %q health exceeds %d bytes", service.Name, statusmodel.MaxEnumLen)
		}
		if len(service.HealthCheckType) > statusmodel.MaxEnumLen {
			return fmt.Errorf("agent_status service %q health_check_type exceeds %d bytes", service.Name, statusmodel.MaxEnumLen)
		}
		if len(service.NetworkAddress) > statusmodel.MaxNetworkAddressLen {
			return fmt.Errorf("agent_status service %q network_address exceeds %d bytes", service.Name, statusmodel.MaxNetworkAddressLen)
		}
		if len(service.Volumes) > config.MaxServiceVolumes {
			return fmt.Errorf("agent_status service %q has too many volumes", service.Name)
		}
		for _, volume := range service.Volumes {
			if err := validateVolumeStatus(service.Name, volume); err != nil {
				return err
			}
		}
	}
	if !validReasonCode(status.ReasonCode) {
		return fmt.Errorf("agent_status has invalid reason code")
	}
	return nil
}

// validateVolumeStatus bounds every VolumeStatus string field. A service can
// carry up to config.MaxServiceVolumes of these and there can be up to
// statusmodel.MaxServices services in one heartbeat, so an unbounded field
// here is what would actually let a "valid" heartbeat exceed
// maxRegistryRequestBytes: see TestMaxRegistryRequestBytesExceedsLargestValidHeartbeat.
func validateVolumeStatus(serviceName string, volume statusmodel.VolumeStatus) error {
	if len(volume.LogicalID) > statusmodel.MaxLogicalIDLen {
		return fmt.Errorf("agent_status service %q volume logical_id exceeds %d bytes", serviceName, statusmodel.MaxLogicalIDLen)
	}
	if len(volume.Type) > statusmodel.MaxEnumLen {
		return fmt.Errorf("agent_status service %q volume type exceeds %d bytes", serviceName, statusmodel.MaxEnumLen)
	}
	if len(volume.MountPath) > statusmodel.MaxMountPathLen {
		return fmt.Errorf("agent_status service %q volume mount_path exceeds %d bytes", serviceName, statusmodel.MaxMountPathLen)
	}
	if len(volume.BoundNode) > statusmodel.MaxVolumeIDLen {
		return fmt.Errorf("agent_status service %q volume bound_node exceeds %d bytes", serviceName, statusmodel.MaxVolumeIDLen)
	}
	if len(volume.SharedBackendID) > statusmodel.MaxVolumeIDLen {
		return fmt.Errorf("agent_status service %q volume shared_backend_id exceeds %d bytes", serviceName, statusmodel.MaxVolumeIDLen)
	}
	if len(volume.State) > statusmodel.MaxEnumLen {
		return fmt.Errorf("agent_status service %q volume state exceeds %d bytes", serviceName, statusmodel.MaxEnumLen)
	}
	if len(volume.RejectedReason) > statusmodel.MaxReasonCodeLen {
		return fmt.Errorf("agent_status service %q volume rejected_reason exceeds %d bytes", serviceName, statusmodel.MaxReasonCodeLen)
	}
	// LastError, like Message elsewhere in AgentStatus, is accept-then-
	// truncate rather than rejected: applyHeartbeatAgentStatus runs it
	// through BoundedMessage after validation succeeds.
	return nil
}

func validStatusPhase(phase statusmodel.Phase) bool {
	return phase == statusmodel.PhaseUnknown || phase == statusmodel.PhaseReconciling || phase == statusmodel.PhaseReady || phase == statusmodel.PhaseFailed
}

func validReasonCode(code string) bool {
	if len(code) > statusmodel.MaxReasonCodeLen {
		return false
	}
	for _, char := range code {
		if unicode.IsControl(char) || unicode.IsSpace(char) {
			return false
		}
	}
	return true
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
	var req registryapi.StateRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.State != registryapi.NodeStateReady && req.State != registryapi.NodeStateDraining && req.State != registryapi.NodeStateDown {
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
		cur.State = NodeState(req.State)
		cur.LastSeenAt = now
		cur.UpdatedAt = now
		return nil
	})
	if err != nil {
		s.logger.Error("state update failed", "node", nodeID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to persist node"})
		return
	}
	writeJSON(w, http.StatusOK, nodeResponse(rec))
}

func resourcesFromRegistry(resources registryapi.Resources) Resources {
	return Resources(resources)
}

func storageFromRegistry(storage registryapi.StorageResources) StorageResources {
	return StorageResources(storage)
}

func nodeResponse(record *NodeRecord) registryapi.NodeResponse {
	return registryapi.NodeResponse{
		NodeID: record.NodeID, Generation: record.Generation,
		State: registryapi.NodeState(record.State), LastSeenAt: record.LastSeenAt,
	}
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
