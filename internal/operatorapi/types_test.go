package operatorapi

import (
	"encoding/json"
	"testing"
	"time"
)

func TestServiceDetailJSONContract(t *testing.T) {
	stamp := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	detail := ServiceDetail{
		APIVersion: APIVersion,
		ObservedAt: stamp,
		ServiceSummary: ServiceSummary{
			Name: "api", Node: "node-1", State: "running", Health: "healthy",
			VCPUs: 2, MemoryMB: 512,
		},
		ServiceObservedAt: stamp,
		HealthCheck:       ServiceHealthDetail{Type: "http", State: "healthy"},
		PortForwards:      []PortForward{{HostPort: 8080, VMPort: 80}},
		Volumes: []VolumeStatus{{
			LogicalID: "api/data", Type: "local", MountPath: "/data",
			DesiredSizeBytes: 20, AppliedSizeBytes: 10, ResizeGeneration: 2,
			State: "rejected", RequestedSizeBytes: 5, Rejected: true,
			RejectedReason: "shrink_not_supported",
		}},
	}

	payload, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"api_version":"v1","observed_at":"2026-01-02T03:04:05Z","name":"api","node":"node-1","state":"running","health":"healthy","vcpus":2,"memory_mb":512,"storage":{"local":{"count":0,"desired_bytes":0,"applied_bytes":0,"allocated_bytes":0},"shared":{"count":0,"desired_bytes":0,"applied_bytes":0,"allocated_bytes":0}},"last_transition_at":"0001-01-01T00:00:00Z","service_observed_at":"2026-01-02T03:04:05Z","health_check":{"type":"http","state":"healthy","last_checked_at":"0001-01-01T00:00:00Z","failures":0},"port_forwards":[{"host_port":8080,"vm_port":80}],"restart_count":0,"volumes":[{"logical_id":"api/data","type":"local","mount_path":"/data","desired_size_bytes":20,"applied_size_bytes":10,"resize_generation":2,"state":"rejected","requested_size_bytes":5,"rejected":true,"rejected_reason":"shrink_not_supported"}]}`
	if got := string(payload); got != want {
		t.Fatalf("JSON contract changed:\n got: %s\nwant: %s", got, want)
	}
}

func TestVolumeStatusOmitsInactiveRejectionFields(t *testing.T) {
	payload, err := json.Marshal(VolumeStatus{
		LogicalID: "api/data", Type: "local", MountPath: "/data",
		DesiredSizeBytes: 20, State: "applied",
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"logical_id":"api/data","type":"local","mount_path":"/data","desired_size_bytes":20,"state":"applied"}`
	if got := string(payload); got != want {
		t.Fatalf("JSON contract changed:\n got: %s\nwant: %s", got, want)
	}
}
