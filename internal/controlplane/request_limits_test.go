package controlplane

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/registryapi"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

// oversizedJSON is syntactically valid JSON one byte above limit, so a 400
// would mean the cap was not applied before decoding.
func oversizedJSON(limit int64) []byte {
	prefix, suffix := `{"csr_pem":"`, `"}`
	return []byte(prefix + strings.Repeat("a", int(limit)-len(prefix)-len(suffix)+1) + suffix)
}

func TestRegistryEndpointsCapBodiesBeforeDecoding(t *testing.T) {
	tests := []struct {
		name   string
		nodeID string
		path   string
		limit  int64
	}{
		// Enrollment is pre-authentication: the cap must fire before the
		// bootstrap token is ever examined.
		{name: "enroll", path: "/v1/nodes/enroll", limit: maxEnrollRequestBytes},
		{name: "renew", nodeID: "node-1", path: "/v1/nodes/renew", limit: maxRegistryControlRequestBytes},
		{name: "register", nodeID: "node-1", path: "/v1/nodes/register", limit: maxRegistryControlRequestBytes},
		{name: "state", nodeID: "node-1", path: "/v1/nodes/node-1/state", limit: maxRegistryControlRequestBytes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, _ := newTestRegistry(t)
			code, errCode := registryCallRaw(t, server, tt.nodeID, tt.path, oversizedJSON(tt.limit))
			if code != http.StatusRequestEntityTooLarge || errCode != registryapi.ErrorCodeRequestTooLarge {
				t.Fatalf("got %d %q, want 413 %q", code, errCode, registryapi.ErrorCodeRequestTooLarge)
			}
		})
	}
}

func TestHeartbeatAboveCapIsRequestTooLarge(t *testing.T) {
	server, _ := newTestRegistry(t)
	registryCall(t, server, "node-1", "/v1/nodes/register", register("node-1", 1))
	prefix, suffix := `{"node_id":"node-1","generation":1,"host_ip":"`, `"}`
	body := prefix + strings.Repeat("a", maxRegistryRequestBytes-len(prefix)-len(suffix)+1) + suffix
	code, errCode := registryCallRaw(t, server, "node-1", "/v1/nodes/heartbeat", []byte(body))
	if code != http.StatusRequestEntityTooLarge || errCode != registryapi.ErrorCodeRequestTooLarge {
		t.Fatalf("got %d %q, want 413", code, errCode)
	}
}

func TestGitHubWebhookCapsBodyBeforeSignatureVerification(t *testing.T) {
	cfg := validConfigForRole(RoleEvents)
	server := NewEventsServer(cfg, newBlobStateStore(newMemBlob()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/v1/events/github", bytes.NewReader(make([]byte, maxGitHubWebhookBytes+1)))
	rec := httptest.NewRecorder()
	server.handleGitHubEvent(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d %s, want 413 before the unsigned body reaches signature verification", rec.Code, rec.Body.String())
	}
}

func validTestAgentStatus(nodeID string) *statusmodel.AgentStatus {
	return &statusmodel.AgentStatus{
		SchemaVersion: statusmodel.SchemaVersion, NodeID: nodeID, ObservedAt: time.Now().UTC(), Phase: statusmodel.PhaseReady,
		DesiredServices: 1, ReadyServices: 1,
		ActualOnlyServices: []statusmodel.ServiceStatus{},
		Services: []statusmodel.ServiceStatus{{Name: "web", VMState: "running", Health: "healthy", Volumes: []statusmodel.VolumeStatus{
			{LogicalID: "web/data", Type: "local", State: "prepared", BoundNode: nodeID},
		}}},
	}
}

// Invalid telemetry is reported to the agent as 422, but the liveness half of
// the heartbeat is still persisted and the stored status records why it is
// absent.
func TestInvalidAgentStatusIsUnprocessableButKeepsLiveness(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*statusmodel.AgentStatus)
		wantRejected string
	}{
		{name: "unknown vm state", mutate: func(s *statusmodel.AgentStatus) { s.Services[0].VMState = "exploded" }, wantRejected: agentStatusRejectedInvalid},
		{name: "unknown health", mutate: func(s *statusmodel.AgentStatus) { s.Services[0].Health = "fine" }, wantRejected: agentStatusRejectedInvalid},
		{name: "unknown volume type", mutate: func(s *statusmodel.AgentStatus) { s.Services[0].Volumes[0].Type = "nfs" }, wantRejected: agentStatusRejectedInvalid},
		{name: "unknown volume state", mutate: func(s *statusmodel.AgentStatus) { s.Services[0].Volumes[0].State = "done" }, wantRejected: agentStatusRejectedInvalid},
		{name: "negative volume size", mutate: func(s *statusmodel.AgentStatus) { s.Services[0].Volumes[0].AppliedSizeBytes = -1 }, wantRejected: agentStatusRejectedInvalid},
		{name: "duplicate volume", mutate: func(s *statusmodel.AgentStatus) {
			s.Services[0].Volumes = append(s.Services[0].Volumes, s.Services[0].Volumes[0])
		}, wantRejected: agentStatusRejectedInvalid},
		{name: "newer schema", mutate: func(s *statusmodel.AgentStatus) { s.SchemaVersion = statusmodel.SchemaVersion + 1 }, wantRejected: agentStatusRejectedUnsupportedSchema},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, store := newTestRegistry(t)
			registryCall(t, server, "node-1", "/v1/nodes/register", register("node-1", 1))
			req := heartbeat("node-1", 1)
			req.AgentStatus = validTestAgentStatus("node-1")
			if code, _ := registryCall(t, server, "node-1", "/v1/nodes/heartbeat", req); code != http.StatusOK {
				t.Fatalf("valid heartbeat: %d", code)
			}
			valid, _ := readNode(t, store, server.cfg, "node-1")
			if valid.AgentStatus == nil || valid.AgentStatusReceivedAt.IsZero() || valid.AgentStatusRejected != "" {
				t.Fatalf("valid status not stored: %+v", valid)
			}

			time.Sleep(time.Millisecond)
			req.AgentStatus = validTestAgentStatus("node-1")
			tt.mutate(req.AgentStatus)
			code, errCode := registryCall(t, server, "node-1", "/v1/nodes/heartbeat", req)
			if code != http.StatusUnprocessableEntity || errCode != registryapi.ErrorCodeInvalidAgentStatus {
				t.Fatalf("got %d %q, want 422 %q", code, errCode, registryapi.ErrorCodeInvalidAgentStatus)
			}
			record, _ := readNode(t, store, server.cfg, "node-1")
			if !record.LastSeenAt.After(valid.LastSeenAt) {
				t.Fatal("rejected telemetry suppressed the liveness heartbeat")
			}
			if record.AgentStatus != nil || !record.AgentStatusReceivedAt.IsZero() || record.AgentStatusRejected != tt.wantRejected {
				t.Fatalf("status=%v received=%v rejected=%q, want cleared status rejected as %q", record.AgentStatus, record.AgentStatusReceivedAt, record.AgentStatusRejected, tt.wantRejected)
			}
		})
	}
}
