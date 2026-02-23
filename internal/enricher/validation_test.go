package enricher

import (
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

func TestValidateInput_Valid(t *testing.T) {
	input := &InputConfig{
		Services: []ServiceSpec{
			{Name: "web", Image: "/images/web.ext4", NodeType: "general-purpose"},
		},
	}

	if err := ValidateInput(input); err != nil {
		t.Errorf("expected valid, got: %v", err)
	}
}

func TestValidateInput_DuplicateServiceNames(t *testing.T) {
	input := &InputConfig{
		Services: []ServiceSpec{
			{Name: "web", Image: "/img/a.ext4", NodeType: "compute"},
			{Name: "web", Image: "/img/b.ext4", NodeType: "compute"},
		},
	}

	err := ValidateInput(input)
	if err == nil {
		t.Fatal("expected error for duplicate service names")
	}
}

func TestValidateInput_MissingServiceName(t *testing.T) {
	input := &InputConfig{
		Services: []ServiceSpec{{Image: "/img/a.ext4", NodeType: "compute"}},
	}

	err := ValidateInput(input)
	if err == nil {
		t.Fatal("expected error for missing service name")
	}
}

func TestValidateInput_MissingImage(t *testing.T) {
	input := &InputConfig{
		Services: []ServiceSpec{{Name: "web", NodeType: "compute"}},
	}

	err := ValidateInput(input)
	if err == nil {
		t.Fatal("expected error for missing image")
	}
}

func TestValidateInput_MissingNodeType(t *testing.T) {
	input := &InputConfig{
		Services: []ServiceSpec{{Name: "web", Image: "/img/web.ext4"}},
	}

	err := ValidateInput(input)
	if err == nil {
		t.Fatal("expected error for missing node_type")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	found := false
	for _, e := range ve.Errors {
		if e == "service web: missing node_type" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected missing node_type error, got: %v", ve.Errors)
	}
}

func TestValidateInput_InvalidHealthCheckType(t *testing.T) {
	input := &InputConfig{
		Services: []ServiceSpec{
			{
				Name:     "web",
				Image:    "/img/web.ext4",
				NodeType: "compute",
				HealthCheck: &HealthCheckSpec{
					Type: "grpc",
					Port: 8080,
				},
			},
		},
	}

	err := ValidateInput(input)
	if err == nil {
		t.Fatal("expected error for invalid health check type")
	}
}

func TestValidateOutput_Valid(t *testing.T) {
	nc := config.NodeConfig{
		Node: "general-purpose",
		Services: []config.ServiceConfig{
			{
				Name:     "web",
				Image:    "/images/web.ext4",
				Kernel:   "/kernels/vmlinux",
				VCPUs:    2,
				MemoryMB: 512,
			},
		},
	}

	if err := ValidateOutput(nc); err != nil {
		t.Errorf("expected valid, got: %v", err)
	}
}

func TestValidateOutput_MissingKernel(t *testing.T) {
	nc := config.NodeConfig{
		Node: "compute",
		Services: []config.ServiceConfig{
			{Name: "web", Image: "/img/a", VCPUs: 1, MemoryMB: 256},
		},
	}

	err := ValidateOutput(nc)
	if err == nil {
		t.Fatal("expected error for missing kernel")
	}
}

func TestValidateOutput_DuplicateService(t *testing.T) {
	nc := config.NodeConfig{
		Node: "compute",
		Services: []config.ServiceConfig{
			{Name: "web", Image: "/img/a", Kernel: "/k", VCPUs: 1, MemoryMB: 256},
			{Name: "web", Image: "/img/b", Kernel: "/k", VCPUs: 1, MemoryMB: 256},
		},
	}

	err := ValidateOutput(nc)
	if err == nil {
		t.Fatal("expected error for duplicate service name")
	}
}

func TestCheckWarnings_HealthCheckWithoutNetwork(t *testing.T) {
	input := &InputConfig{
		Services: []ServiceSpec{
			{
				Name:        "web",
				Image:       "/img/web.ext4",
				NodeType:    "compute",
				Network:     false,
				HealthCheck: &HealthCheckSpec{Type: "http", Port: 8080},
			},
		},
	}

	warns := CheckWarnings(input)
	found := false
	for _, w := range warns {
		if w.Message == "service web has health check but network is disabled" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected health-check-without-network warning, got: %v", warns)
	}
}

func TestCheckWarnings_NoWarnings(t *testing.T) {
	input := &InputConfig{
		Services: []ServiceSpec{
			{
				Name:        "web",
				Image:       "/img/web.ext4",
				NodeType:    "compute",
				Network:     true,
				HealthCheck: &HealthCheckSpec{Type: "http", Port: 8080},
			},
		},
	}

	warns := CheckWarnings(input)
	if len(warns) != 0 {
		t.Errorf("expected no warnings, got: %v", warns)
	}
}
