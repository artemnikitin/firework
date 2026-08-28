package registryapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/statusmodel"
)

func TestJSONContracts(t *testing.T) {
	stamp := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{
			name:  "register",
			value: RegisterRequest{NodeID: "node-1", Generation: 7, Labels: []string{"worker"}, Capacity: Resources{VCPUs: 4, MemoryMB: 8192}, State: NodeStateReady, HostIP: "10.0.0.2", Storage: StorageResources{LocalCapacityBytes: 10}},
			want:  `{"node_id":"node-1","generation":7,"labels":["worker"],"capacity":{"vcpus":4,"memory_mb":8192},"state":"ready","host_ip":"10.0.0.2","storage":{"local_capacity_bytes":10}}`,
		},
		{
			name:  "heartbeat",
			value: HeartbeatRequest{NodeID: "node-1", Generation: 7, Capacity: Resources{VCPUs: 4, MemoryMB: 8192}, Used: Resources{VCPUs: 2, MemoryMB: 1024}, AgentStatus: &statusmodel.AgentStatus{SchemaVersion: 1, NodeID: "node-1", ObservedAt: stamp, Phase: statusmodel.PhaseReady}},
			want:  `{"node_id":"node-1","generation":7,"capacity":{"vcpus":4,"memory_mb":8192},"used":{"vcpus":2,"memory_mb":1024},"agent_status":{"schema_version":1,"node":"node-1","observed_at":"2026-01-02T03:04:05Z","phase":"ready","last_applied_at":"0001-01-01T00:00:00Z","desired_services":0,"ready_services":0},"storage":{}}`,
		},
		{name: "state", value: StateRequest{State: NodeStateDraining}, want: `{"state":"draining"}`},
		{name: "enroll", value: EnrollRequest{NodeID: "node-1", BootstrapToken: "token", CSRPEM: "csr"}, want: `{"node_id":"node-1","bootstrap_token":"token","csr_pem":"csr"}`},
		{name: "renew", value: RenewRequest{CSRPEM: "csr"}, want: `{"csr_pem":"csr"}`},
		{name: "certificate", value: CertResponse{CertPEM: "cert", ExpiresAt: stamp}, want: `{"cert_pem":"cert","expires_at":"2026-01-02T03:04:05Z"}`},
		{name: "node", value: NodeResponse{NodeID: "node-1", Generation: 7, State: NodeStateReady, LastSeenAt: stamp}, want: `{"node_id":"node-1","generation":7,"state":"ready","last_seen_at":"2026-01-02T03:04:05Z"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(payload); got != tt.want {
				t.Fatalf("JSON contract changed:\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}
