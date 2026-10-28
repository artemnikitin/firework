package controlplane

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/registryapi"
)

func newTestRegistry(t *testing.T) (*RegistryServer, StateStore) {
	t.Helper()
	store := newBlobStateStore(newMemBlob())
	cfg := validConfigForRole(RoleRegistry)
	return &RegistryServer{cfg: cfg, store: store, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, store
}

// registryCall sends body to path as the mTLS identity nodeID and returns the
// status and decoded error code, if any.
func registryCall(t *testing.T, server *RegistryServer, nodeID, path string, body any) (int, string) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return registryCallRaw(t, server, nodeID, path, payload)
}

func registryCallRaw(t *testing.T, server *RegistryServer, nodeID, path string, payload []byte) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	if nodeID != "" {
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Subject: pkix.Name{CommonName: nodeID}}}}
	}
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code >= 200 && rec.Code < 300 {
		return rec.Code, ""
	}
	var envelope registryapi.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("%s returned %d with a non-envelope body %q: %v", path, rec.Code, rec.Body.String(), err)
	}
	if envelope.Error.Code == "" || envelope.Error.Message == "" {
		t.Fatalf("%s returned %d with an incomplete error envelope %q", path, rec.Code, rec.Body.String())
	}
	return rec.Code, envelope.Error.Code
}

func readNode(t *testing.T, store StateStore, cfg Config, nodeID string) (NodeRecord, bool) {
	t.Helper()
	key, err := nodeRecordKey(cfg.State.Prefix, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	var record NodeRecord
	_, exists, err := store.GetJSON(context.Background(), key, &record)
	if err != nil {
		t.Fatal(err)
	}
	return record, exists
}

func register(nodeID string, generation int64) registryapi.RegisterRequest {
	return registryapi.RegisterRequest{NodeID: nodeID, Generation: generation, Capacity: registryapi.Resources{VCPUs: 2, MemoryMB: 1024}}
}

func heartbeat(nodeID string, generation int64) registryapi.HeartbeatRequest {
	return registryapi.HeartbeatRequest{NodeID: nodeID, Generation: generation}
}

// The shutdown of a replaced process must not take its replacement down: the
// old process's state request names its own generation and is rejected.
func TestOldIncarnationShutdownCannotMarkReplacementDown(t *testing.T) {
	server, store := newTestRegistry(t)
	if code, _ := registryCall(t, server, "node-1", "/v1/nodes/register", register("node-1", 100)); code != http.StatusOK {
		t.Fatalf("old register: %d", code)
	}
	if code, _ := registryCall(t, server, "node-1", "/v1/nodes/register", register("node-1", 200)); code != http.StatusOK {
		t.Fatalf("new register: %d", code)
	}
	if code, _ := registryCall(t, server, "node-1", "/v1/nodes/heartbeat", heartbeat("node-1", 200)); code != http.StatusOK {
		t.Fatalf("new heartbeat: %d", code)
	}

	code, errCode := registryCall(t, server, "node-1", "/v1/nodes/node-1/state", registryapi.StateRequest{State: registryapi.NodeStateDown, Generation: 100})
	if code != http.StatusConflict || errCode != registryapi.ErrorCodeStaleGeneration {
		t.Fatalf("old shutdown = %d %q, want 409 %q", code, errCode, registryapi.ErrorCodeStaleGeneration)
	}
	record, _ := readNode(t, store, server.cfg, "node-1")
	if record.State != NodeStateReady || record.Generation != 200 || !record.DownAt.IsZero() {
		t.Fatalf("replacement was changed by the old process: %+v", record)
	}

	// The current incarnation's own shutdown still applies.
	if code, _ := registryCall(t, server, "node-1", "/v1/nodes/node-1/state", registryapi.StateRequest{State: registryapi.NodeStateDown, Generation: 200}); code != http.StatusOK {
		t.Fatalf("current shutdown: %d", code)
	}
	record, _ = readNode(t, store, server.cfg, "node-1")
	if record.State != NodeStateDown || record.DownAt.IsZero() || record.StateChangedAt.IsZero() {
		t.Fatalf("current shutdown not recorded: %+v", record)
	}
}

func TestHeartbeatAndStateRequireAnExistingRecordAndExactGeneration(t *testing.T) {
	tests := []struct {
		name       string
		registered int64
		path       string
		body       any
		wantStatus int
		wantCode   string
	}{
		{name: "heartbeat without record", path: "/v1/nodes/heartbeat", body: heartbeat("node-1", 5), wantStatus: http.StatusNotFound, wantCode: registryapi.ErrorCodeNodeNotRegistered},
		{name: "state without record", path: "/v1/nodes/node-1/state", body: registryapi.StateRequest{State: registryapi.NodeStateDown, Generation: 5}, wantStatus: http.StatusNotFound, wantCode: registryapi.ErrorCodeNodeNotRegistered},
		{name: "older heartbeat", registered: 5, path: "/v1/nodes/heartbeat", body: heartbeat("node-1", 4), wantStatus: http.StatusConflict, wantCode: registryapi.ErrorCodeStaleGeneration},
		// Only registration advances the generation.
		{name: "newer heartbeat", registered: 5, path: "/v1/nodes/heartbeat", body: heartbeat("node-1", 6), wantStatus: http.StatusConflict, wantCode: registryapi.ErrorCodeStaleGeneration},
		{name: "newer state", registered: 5, path: "/v1/nodes/node-1/state", body: registryapi.StateRequest{State: registryapi.NodeStateDraining, Generation: 6}, wantStatus: http.StatusConflict, wantCode: registryapi.ErrorCodeStaleGeneration},
		{name: "generation-less state", registered: 5, path: "/v1/nodes/node-1/state", body: registryapi.StateRequest{State: registryapi.NodeStateDown}, wantStatus: http.StatusBadRequest, wantCode: registryapi.ErrorCodeInvalidRequest},
		{name: "current heartbeat", registered: 5, path: "/v1/nodes/heartbeat", body: heartbeat("node-1", 5), wantStatus: http.StatusOK},
		{name: "current state", registered: 5, path: "/v1/nodes/node-1/state", body: registryapi.StateRequest{State: registryapi.NodeStateDraining, Generation: 5}, wantStatus: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, store := newTestRegistry(t)
			if tt.registered != 0 {
				if code, _ := registryCall(t, server, "node-1", "/v1/nodes/register", register("node-1", tt.registered)); code != http.StatusOK {
					t.Fatalf("register: %d", code)
				}
			}
			before, existed := readNode(t, store, server.cfg, "node-1")
			code, errCode := registryCall(t, server, "node-1", tt.path, tt.body)
			if code != tt.wantStatus || errCode != tt.wantCode {
				t.Fatalf("got %d %q, want %d %q", code, errCode, tt.wantStatus, tt.wantCode)
			}
			after, exists := readNode(t, store, server.cfg, "node-1")
			if tt.wantStatus == http.StatusOK {
				return
			}
			if exists != existed {
				t.Fatalf("rejected request changed record existence: %v -> %v", existed, exists)
			}
			if exists && (after.Generation != before.Generation || after.State != before.State || !after.UpdatedAt.Equal(before.UpdatedAt)) {
				t.Fatalf("rejected request mutated the record: %+v -> %+v", before, after)
			}
		})
	}
}

func TestRegistrationIsTheOnlyGenerationAdvance(t *testing.T) {
	server, store := newTestRegistry(t)
	for _, step := range []struct {
		generation int64
		want       int
	}{{10, http.StatusOK}, {10, http.StatusOK}, {9, http.StatusConflict}, {11, http.StatusOK}} {
		if code, _ := registryCall(t, server, "node-1", "/v1/nodes/register", register("node-1", step.generation)); code != step.want {
			t.Fatalf("register generation %d = %d, want %d", step.generation, code, step.want)
		}
	}
	if record, _ := readNode(t, store, server.cfg, "node-1"); record.Generation != 11 {
		t.Fatalf("generation = %d, want 11", record.Generation)
	}
}

func TestRetirementPendingFencesEveryRegistryMutation(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		body        any
		wantStatus  int
		wantErrCode string
	}{
		{name: "older registration", path: "/v1/nodes/register", body: register("node-1", 6), wantStatus: http.StatusConflict, wantErrCode: registryapi.ErrorCodeStaleGeneration},
		{name: "same generation registration", path: "/v1/nodes/register", body: register("node-1", 7), wantStatus: http.StatusConflict, wantErrCode: registryapi.ErrorCodeNodeRetiring},
		{name: "higher generation registration", path: "/v1/nodes/register", body: register("node-1", 8), wantStatus: http.StatusOK},
		{name: "heartbeat", path: "/v1/nodes/heartbeat", body: heartbeat("node-1", 7), wantStatus: http.StatusConflict, wantErrCode: registryapi.ErrorCodeNodeRetiring},
		{name: "state", path: "/v1/nodes/node-1/state", body: registryapi.StateRequest{State: registryapi.NodeStateDown, Generation: 7}, wantStatus: http.StatusConflict, wantErrCode: registryapi.ErrorCodeNodeRetiring},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, store := newTestRegistry(t)
			if code, _ := registryCall(t, server, "node-1", "/v1/nodes/register", register("node-1", 7)); code != http.StatusOK {
				t.Fatalf("register: %d", code)
			}
			before, _ := readNode(t, store, server.cfg, "node-1")
			before.RetirementPendingAt = time.Now().UTC()
			key, err := nodeRecordKey(server.cfg.State.Prefix, "node-1")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.PutJSON(context.Background(), key, before); err != nil {
				t.Fatal(err)
			}
			before, _ = readNode(t, store, server.cfg, "node-1")
			code, errCode := registryCall(t, server, "node-1", tt.path, tt.body)
			if code != tt.wantStatus || errCode != tt.wantErrCode {
				t.Fatalf("got %d %q, want %d %q", code, errCode, tt.wantStatus, tt.wantErrCode)
			}
			after, _ := readNode(t, store, server.cfg, "node-1")
			if tt.wantStatus == http.StatusOK {
				if after.Generation != 8 || !after.RetirementPendingAt.IsZero() {
					t.Fatalf("newer registration did not replace the pending incarnation: %+v", after)
				}
			} else if after.Generation != before.Generation || after.State != before.State || !after.RetirementPendingAt.Equal(before.RetirementPendingAt) {
				t.Fatalf("rejected request changed the record: %+v -> %+v", before, after)
			}
		})
	}
}

func TestTombstoneFencesRegistrationAfterActiveRecordRemoval(t *testing.T) {
	server, store := newTestRegistry(t)
	tombstoneKey, err := nodeTombstoneKey(server.cfg.State.Prefix, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PutJSONIfAbsent(context.Background(), tombstoneKey, NodeTombstone{
		NodeID:        "node-1",
		MaxGeneration: 7,
		RetiredAt:     time.Now().UTC(),
		Reason:        "test",
	}); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []int64{7, 8} {
		code, errCode := registryCall(t, server, "node-1", "/v1/nodes/register", register("node-1", generation))
		if code != http.StatusConflict || errCode != registryapi.ErrorCodeNodeRetired {
			t.Fatalf("register generation %d = %d %q, want 409 %q", generation, code, errCode, registryapi.ErrorCodeNodeRetired)
		}
	}
	if _, exists := readNode(t, store, server.cfg, "node-1"); exists {
		t.Fatal("tombstoned registration recreated the active node record")
	}
}

// A registration revives a down node, so the down timestamp must not survive
// into the new incarnation's record.
func TestRegistrationClearsDownTimestamp(t *testing.T) {
	server, store := newTestRegistry(t)
	registryCall(t, server, "node-1", "/v1/nodes/register", register("node-1", 1))
	registryCall(t, server, "node-1", "/v1/nodes/node-1/state", registryapi.StateRequest{State: registryapi.NodeStateDown, Generation: 1})
	if code, _ := registryCall(t, server, "node-1", "/v1/nodes/register", register("node-1", 2)); code != http.StatusOK {
		t.Fatalf("register: %d", code)
	}
	record, _ := readNode(t, store, server.cfg, "node-1")
	if record.State != NodeStateReady || !record.DownAt.IsZero() {
		t.Fatalf("revived record = %+v", record)
	}
}

func TestNodeLifecycleStateKeepsExplicitDownPastStaleTTL(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-time.Hour)
	tests := []struct {
		record NodeRecord
		want   string
	}{
		{NodeRecord{State: NodeStateDown, LastSeenAt: old}, "down"},
		{NodeRecord{State: NodeStateDown, LastSeenAt: now}, "down"},
		{NodeRecord{State: NodeStateReady, LastSeenAt: old}, "stale"},
		{NodeRecord{State: NodeStateDraining, LastSeenAt: old}, "stale"},
		{NodeRecord{State: NodeStateReady}, "stale"},
		{NodeRecord{State: NodeStateReady, LastSeenAt: now}, "ready"},
		{NodeRecord{State: NodeStateDraining, LastSeenAt: now}, "draining"},
		{NodeRecord{State: "retired-by-newer-version", LastSeenAt: now}, "unknown"},
	}
	for _, tt := range tests {
		if got := nodeLifecycleState(tt.record, now, time.Minute); got != tt.want {
			t.Errorf("nodeLifecycleState(%s, seen %s ago) = %q, want %q", tt.record.State, now.Sub(tt.record.LastSeenAt), got, tt.want)
		}
	}
}

// A new incarnation must not inherit the previous process's telemetry,
// including the reason its last status was refused.
func TestRegistrationOfNewGenerationClearsPreviousTelemetry(t *testing.T) {
	server, store := newTestRegistry(t)
	registryCall(t, server, "node-1", "/v1/nodes/register", register("node-1", 1))
	req := heartbeat("node-1", 1)
	req.AgentStatus = validTestAgentStatus("node-1")
	req.AgentStatus.Services[0].VMState = "exploded"
	if code, _ := registryCall(t, server, "node-1", "/v1/nodes/heartbeat", req); code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid heartbeat: %d", code)
	}
	registryCall(t, server, "node-1", "/v1/nodes/register", register("node-1", 2))
	record, _ := readNode(t, store, server.cfg, "node-1")
	if record.AgentStatus != nil || record.AgentStatusRejected != "" || !record.AgentStatusReceivedAt.IsZero() {
		t.Fatalf("new generation inherited telemetry: %+v", record)
	}
}
