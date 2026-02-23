package enricher

import (
	"path/filepath"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

func TestLoadTenants_NotExist(t *testing.T) {
	dir := t.TempDir()
	tenants, err := LoadTenants(dir)
	if err != nil {
		t.Fatalf("expected no error for missing tenants/, got: %v", err)
	}
	if len(tenants) != 0 {
		t.Errorf("expected empty slice, got %d tenants", len(tenants))
	}
}

func TestLoadTenants_TwoTenants(t *testing.T) {
	// Use the checked-in testdata fixtures.
	dir := filepath.Join("testdata")
	tenants, err := LoadTenants(dir)
	if err != nil {
		t.Fatalf("LoadTenants: %v", err)
	}

	if len(tenants) != 2 {
		t.Fatalf("expected 2 tenants, got %d", len(tenants))
	}

	byID := make(map[string]TenantConfig, len(tenants))
	for _, tc := range tenants {
		byID[tc.ID] = tc
	}

	t1, ok := byID["tenant-1"]
	if !ok {
		t.Fatal("tenant-1 not found")
	}
	if len(t1.Services) != 2 {
		t.Errorf("tenant-1: expected 2 services, got %d", len(t1.Services))
	}

	t2, ok := byID["tenant-2"]
	if !ok {
		t.Fatal("tenant-2 not found")
	}
	if len(t2.Services) != 2 {
		t.Errorf("tenant-2: expected 2 services, got %d", len(t2.Services))
	}

	// Verify a specific override value.
	byBase := make(map[string]TenantServiceFile, len(t1.Services))
	for _, sf := range t1.Services {
		byBase[sf.BaseName] = sf
	}
	kib, ok := byBase["kibana"]
	if !ok {
		t.Fatal("tenant-1 kibana service not found")
	}
	if kib.Override.Env["TENANT_ID"] != "tenant-1" {
		t.Errorf("expected TENANT_ID=tenant-1, got %q", kib.Override.Env["TENANT_ID"])
	}
	if len(kib.Override.PortForwards) != 1 || kib.Override.PortForwards[0].HostPort != 5611 {
		t.Errorf("expected port_forward host_port=5611, got %+v", kib.Override.PortForwards)
	}
}

func TestExpandTenants_NameAndImage(t *testing.T) {
	base := []ServiceSpec{
		{
			Name:     "kibana",
			Image:    "/var/lib/images/kibana-rootfs.ext4",
			NodeType: "web",
		},
	}
	tenants := []TenantConfig{
		{
			ID: "tenant-1",
			Services: []TenantServiceFile{
				{BaseName: "kibana", Override: TenantOverride{}},
			},
		},
	}

	expanded := ExpandTenants(base, tenants)
	if len(expanded) != 1 {
		t.Fatalf("expected 1 expanded spec, got %d", len(expanded))
	}

	spec := expanded[0]
	if spec.Name != "tenant-1-kibana" {
		t.Errorf("expected name tenant-1-kibana, got %s", spec.Name)
	}
	if spec.Image != "/var/lib/images/tenant-1-kibana-rootfs.ext4" {
		t.Errorf("expected derived image path, got %s", spec.Image)
	}
	if spec.NodeType != "web" {
		t.Errorf("expected NodeType=web inherited from base, got %s", spec.NodeType)
	}
}

func TestExpandTenants_EnvMerge(t *testing.T) {
	base := []ServiceSpec{
		{
			Name:     "kibana",
			Image:    "/var/lib/images/kibana-rootfs.ext4",
			NodeType: "web",
			Env: map[string]string{
				"LOG_LEVEL": "info",
				"BASE_KEY":  "base-value",
			},
		},
	}
	tenants := []TenantConfig{
		{
			ID: "tenant-1",
			Services: []TenantServiceFile{
				{
					BaseName: "kibana",
					Override: TenantOverride{
						Env: map[string]string{
							"TENANT_ID": "tenant-1",
							"LOG_LEVEL": "debug", // override wins
						},
					},
				},
			},
		},
	}

	expanded := ExpandTenants(base, tenants)
	if len(expanded) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(expanded))
	}

	env := expanded[0].Env
	if env["TENANT_ID"] != "tenant-1" {
		t.Errorf("expected TENANT_ID=tenant-1, got %q", env["TENANT_ID"])
	}
	if env["LOG_LEVEL"] != "debug" {
		t.Errorf("expected LOG_LEVEL=debug (override wins), got %q", env["LOG_LEVEL"])
	}
	if env["BASE_KEY"] != "base-value" {
		t.Errorf("expected BASE_KEY=base-value from base, got %q", env["BASE_KEY"])
	}

	// Base spec env must not be mutated.
	if base[0].Env["LOG_LEVEL"] != "info" {
		t.Errorf("base spec env was mutated")
	}
}

func TestExpandTenants_LinksRewritten(t *testing.T) {
	base := []ServiceSpec{
		{
			Name:     "kibana",
			Image:    "/var/lib/images/kibana-rootfs.ext4",
			NodeType: "web",
			Links: []config.ServiceLink{
				{Service: "elasticsearch", EnvVar: "ELASTICSEARCH_HOSTS", Port: 9200},
			},
		},
	}
	tenants := []TenantConfig{
		{
			ID: "tenant-1",
			Services: []TenantServiceFile{
				{BaseName: "kibana", Override: TenantOverride{}},
			},
		},
	}

	expanded := ExpandTenants(base, tenants)
	if len(expanded) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(expanded))
	}

	links := expanded[0].Links
	if len(links) != 1 {
		t.Fatalf("expected 1 link, got %d", len(links))
	}
	if links[0].Service != "tenant-1-elasticsearch" {
		t.Errorf("expected tenant-1-elasticsearch, got %s", links[0].Service)
	}

	// Base spec links must not be mutated.
	if base[0].Links[0].Service != "elasticsearch" {
		t.Errorf("base spec links were mutated")
	}
}

func TestExpandTenants_MissingBase(t *testing.T) {
	base := []ServiceSpec{
		{
			Name:     "kibana",
			Image:    "/var/lib/images/kibana-rootfs.ext4",
			NodeType: "web",
		},
	}
	tenants := []TenantConfig{
		{
			ID: "tenant-1",
			Services: []TenantServiceFile{
				{BaseName: "kibana", Override: TenantOverride{}},
				{BaseName: "nonexistent", Override: TenantOverride{}}, // no base — should be skipped
			},
		},
	}

	expanded := ExpandTenants(base, tenants)
	if len(expanded) != 1 {
		t.Errorf("expected 1 spec (nonexistent skipped), got %d", len(expanded))
	}
	if expanded[0].Name != "tenant-1-kibana" {
		t.Errorf("expected tenant-1-kibana, got %s", expanded[0].Name)
	}
}

func TestExpandTenants_PortForwardReplace(t *testing.T) {
	base := []ServiceSpec{
		{
			Name:     "kibana",
			Image:    "/var/lib/images/kibana-rootfs.ext4",
			NodeType: "web",
			PortForwards: []config.PortForward{
				{HostPort: 5601, VMPort: 5601},
			},
		},
	}
	tenants := []TenantConfig{
		{
			ID: "tenant-2",
			Services: []TenantServiceFile{
				{
					BaseName: "kibana",
					Override: TenantOverride{
						PortForwards: []config.PortForward{
							{HostPort: 5602, VMPort: 5601},
						},
					},
				},
			},
		},
	}

	expanded := ExpandTenants(base, tenants)
	if len(expanded) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(expanded))
	}
	pf := expanded[0].PortForwards
	if len(pf) != 1 || pf[0].HostPort != 5602 {
		t.Errorf("expected HostPort=5602 (override), got %+v", pf)
	}
}

func TestExpandTenants_Standalone(t *testing.T) {
	// No base services — tenant files are self-contained.
	tenants := []TenantConfig{
		{
			ID: "tenant-1",
			Services: []TenantServiceFile{
				{
					BaseName: "kibana",
					Override: TenantOverride{
						NodeType: "web",
						VCPUs:    2,
						MemoryMB: 2048,
						PortForwards: []config.PortForward{
							{HostPort: 5611, VMPort: 5601},
						},
						Links: []config.ServiceLink{
							{Service: "elasticsearch", EnvVar: "ELASTICSEARCH_HOSTS", Port: 9200},
						},
						Env: map[string]string{"TENANT_ID": "tenant-1"},
					},
				},
				{
					// no node_type → should be skipped even with no base
					BaseName: "ignored",
					Override: TenantOverride{},
				},
			},
		},
	}

	expanded := ExpandTenants(nil, tenants)
	if len(expanded) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(expanded))
	}

	spec := expanded[0]
	if spec.Name != "tenant-1-kibana" {
		t.Errorf("expected name tenant-1-kibana, got %s", spec.Name)
	}
	// Image derived from convention when not explicitly set.
	if spec.Image != "/var/lib/images/tenant-1-kibana-rootfs.ext4" {
		t.Errorf("expected derived image path, got %s", spec.Image)
	}
	if spec.NodeType != "web" {
		t.Errorf("expected NodeType=web, got %s", spec.NodeType)
	}
	if spec.VCPUs != 2 {
		t.Errorf("expected VCPUs=2, got %d", spec.VCPUs)
	}
	if len(spec.Links) != 1 || spec.Links[0].Service != "tenant-1-elasticsearch" {
		t.Errorf("expected link rewritten to tenant-1-elasticsearch, got %+v", spec.Links)
	}
}

func TestExpandTenants_PortForwardInherited(t *testing.T) {
	base := []ServiceSpec{
		{
			Name:     "kibana",
			Image:    "/var/lib/images/kibana-rootfs.ext4",
			NodeType: "web",
			PortForwards: []config.PortForward{
				{HostPort: 5601, VMPort: 5601},
			},
		},
	}
	tenants := []TenantConfig{
		{
			ID: "tenant-1",
			Services: []TenantServiceFile{
				{BaseName: "kibana", Override: TenantOverride{}}, // no port override
			},
		},
	}

	expanded := ExpandTenants(base, tenants)
	if len(expanded) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(expanded))
	}
	pf := expanded[0].PortForwards
	if len(pf) != 1 || pf[0].HostPort != 5601 {
		t.Errorf("expected inherited HostPort=5601, got %+v", pf)
	}
}
