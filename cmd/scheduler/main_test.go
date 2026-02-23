package main

import (
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/scheduler"
)

// TestScheduleServices_ErrorWhenNoNodes guards against the critical regression
// where an empty-node response is returned as success, causing the enricher to
// pass an empty NodeConfigs slice to WriteAll and wipe all S3 node configs.
func TestScheduleServices_ErrorWhenNoNodes(t *testing.T) {
	req := Request{
		Services: []config.ServiceConfig{
			{Name: "es-1", VCPUs: 2, MemoryMB: 512},
		},
	}

	_, err := scheduleServices(req, nil, nil)
	if err == nil {
		t.Fatal("expected error when no nodes available, got nil — empty success would wipe S3 configs")
	}
	if !strings.Contains(err.Error(), "no active nodes") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestScheduleServices_PlacesServicesOnNodes(t *testing.T) {
	req := Request{
		Services: []config.ServiceConfig{
			{Name: "svc-a", VCPUs: 2, MemoryMB: 512},
			{Name: "svc-b", VCPUs: 2, MemoryMB: 512},
		},
	}
	nodes := []scheduler.Node{
		{InstanceID: "i-001", CapacityVCPUs: 8, CapacityMemMB: 4096},
		{InstanceID: "i-002", CapacityVCPUs: 8, CapacityMemMB: 4096},
	}

	resp, err := scheduleServices(req, nodes, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	total := 0
	for _, nc := range resp.NodeConfigs {
		total += len(nc.Services)
	}
	if total != 2 {
		t.Errorf("expected 2 services placed, got %d across %d nodes", total, len(resp.NodeConfigs))
	}
}

func TestScheduleServices_RespectsExistingPlacement(t *testing.T) {
	req := Request{
		Services: []config.ServiceConfig{
			{Name: "svc-a", VCPUs: 1, MemoryMB: 256},
		},
	}
	nodes := []scheduler.Node{
		{InstanceID: "i-001", CapacityVCPUs: 8, CapacityMemMB: 4096},
		{InstanceID: "i-002", CapacityVCPUs: 8, CapacityMemMB: 4096},
	}
	existing := map[string]string{"svc-a": "i-002"}

	resp, err := scheduleServices(req, nodes, existing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, nc := range resp.NodeConfigs {
		for _, svc := range nc.Services {
			if svc.Name == "svc-a" && nc.Node != "i-002" {
				t.Errorf("svc-a should stay on i-002 (existing placement), got %s", nc.Node)
			}
		}
	}
}
