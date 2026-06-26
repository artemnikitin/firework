package enricher

import (
	"strings"
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

func routedSpec(name string, meta map[string]string) ServiceSpec {
	return ServiceSpec{
		Name:         name,
		Image:        "/img/" + name + ".ext4",
		NodeType:     "web",
		Network:      true,
		PortForwards: []config.PortForward{{HostPort: 8081, VMPort: 8080}},
		Metadata:     meta,
	}
}

func TestValidateInput_Routing(t *testing.T) {
	tests := []struct {
		name    string
		specs   []ServiceSpec
		wantErr bool
	}{
		{name: "valid subdomain", specs: []ServiceSpec{routedSpec("a", map[string]string{"subdomain": "tenant-1"})}},
		{name: "valid host", specs: []ServiceSpec{routedSpec("a", map[string]string{"host": "a.example.com"})}},
		{name: "both keys", specs: []ServiceSpec{routedSpec("a", map[string]string{"subdomain": "tenant-1", "host": "a.example.com"})}, wantErr: true},
		{name: "invalid subdomain", specs: []ServiceSpec{routedSpec("a", map[string]string{"subdomain": "Bad.Label"})}, wantErr: true},
		{name: "invalid host injection", specs: []ServiceSpec{routedSpec("a", map[string]string{"host": "a`b.example.com"})}, wantErr: true},
		{name: "empty subdomain value", specs: []ServiceSpec{routedSpec("a", map[string]string{"subdomain": ""})}, wantErr: true},
		{name: "duplicate subdomains", specs: []ServiceSpec{routedSpec("a", map[string]string{"subdomain": "t1"}), routedSpec("b", map[string]string{"subdomain": "t1"})}, wantErr: true},
		{name: "duplicate hosts", specs: []ServiceSpec{routedSpec("a", map[string]string{"host": "x.example.com"}), routedSpec("b", map[string]string{"host": "x.example.com"})}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateInput(&InputConfig{Services: tt.specs})
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateInput err=%v wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateInput_DuplicateSubdomainsNamesBothServices(t *testing.T) {
	err := ValidateInput(&InputConfig{Services: []ServiceSpec{
		routedSpec("svc-a", map[string]string{"subdomain": "t1"}),
		routedSpec("svc-b", map[string]string{"subdomain": "t1"}),
	}})
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	joined := strings.Join(ve.Errors, "\n")
	if !strings.Contains(joined, "svc-a") || !strings.Contains(joined, "svc-b") {
		t.Errorf("expected both conflicting service names in error, got: %v", ve.Errors)
	}
}

func TestValidateInput_RoutedWithoutNetworkOrBackend(t *testing.T) {
	// No network, no port forward, no health check -> two errors.
	err := ValidateInput(&InputConfig{Services: []ServiceSpec{
		{Name: "a", Image: "/img/a", NodeType: "web", Metadata: map[string]string{"subdomain": "t1"}},
	}})
	if err == nil {
		t.Fatal("expected error for routed service without network/backend")
	}
}

// A routed service with only a health-check port (no port_forwards) must pass
// validation but warn that it cannot do remote routing — the seam the agent
// relies on must agree.
func TestValidateInput_SubdomainWithHealthCheckPortValidWithWarning(t *testing.T) {
	input := &InputConfig{Services: []ServiceSpec{
		{
			Name:        "a",
			Image:       "/img/a",
			NodeType:    "web",
			Network:     true,
			HealthCheck: &HealthCheckSpec{Type: "http", Port: 8080},
			Metadata:    map[string]string{"subdomain": "t1"},
		},
	}}
	if err := ValidateInput(input); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
	warns := CheckWarnings(input)
	found := false
	for _, w := range warns {
		if w.Code == WarnRemoteRoutingNoHostPort {
			found = true
		}
	}
	if !found {
		t.Errorf("expected remote-routing-no-host-port warning, got: %v", warns)
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
