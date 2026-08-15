package config

import (
	"strings"
	"testing"
)

func TestPortClaims_SkipsEntriesWithoutHostPort(t *testing.T) {
	svc := ServiceConfig{Name: "a", PortForwards: []PortForward{
		{HostPort: 9200, VMPort: 9200},
		{VMPort: 9300},
		{HostPort: 0, VMPort: 5601},
	}}

	claims := svc.PortClaims()
	if len(claims) != 1 || claims[0] != (PortClaim{Protocol: ProtocolTCP, HostPort: 9200}) {
		t.Fatalf("unexpected claims: %#v", claims)
	}
}

func TestDuplicatePortClaims(t *testing.T) {
	svc := ServiceConfig{Name: "a", PortForwards: []PortForward{
		{HostPort: 9200, VMPort: 9200},
		{HostPort: 9200, VMPort: 9201},
		{HostPort: 5601, VMPort: 5601},
	}}

	dupes := svc.DuplicatePortClaims()
	if len(dupes) != 1 || dupes[0].HostPort != 9200 {
		t.Fatalf("expected 9200 reported once, got %#v", dupes)
	}
}

func TestConflictingPortClaims_ReportsHoldersSorted(t *testing.T) {
	services := []ServiceConfig{
		{Name: "tenant-2-elasticsearch", PortForwards: []PortForward{{HostPort: 9200, VMPort: 9200}}},
		{Name: "tenant-1-elasticsearch", PortForwards: []PortForward{{HostPort: 9200, VMPort: 9200}}},
		{Name: "tenant-1-kibana", PortForwards: []PortForward{{HostPort: 5601, VMPort: 5601}}},
	}

	conflicts := ConflictingPortClaims(services)
	if len(conflicts) != 1 {
		t.Fatalf("expected one conflict, got %#v", conflicts)
	}
	if conflicts[0].Claim.HostPort != 9200 {
		t.Errorf("expected conflict on 9200, got %d", conflicts[0].Claim.HostPort)
	}
	if got := strings.Join(conflicts[0].Services, ","); got != "tenant-1-elasticsearch,tenant-2-elasticsearch" {
		t.Errorf("unexpected holders: %s", got)
	}
}

// A service repeating one claim conflicts with itself, not with its peers.
func TestConflictingPortClaims_IgnoresRepeatsWithinOneService(t *testing.T) {
	services := []ServiceConfig{
		{Name: "a", PortForwards: []PortForward{{HostPort: 9200, VMPort: 9200}, {HostPort: 9200, VMPort: 9201}}},
	}

	if conflicts := ConflictingPortClaims(services); len(conflicts) != 0 {
		t.Fatalf("expected no cross-service conflict, got %#v", conflicts)
	}
}

func TestValidateNodePortClaims_RejectsCollocatedDuplicate(t *testing.T) {
	nc := NodeConfig{Node: "i-001", Services: []ServiceConfig{
		{Name: "tenant-1-elasticsearch", PortForwards: []PortForward{{HostPort: 9200, VMPort: 9200}}},
		{Name: "tenant-2-elasticsearch", PortForwards: []PortForward{{HostPort: 9200, VMPort: 9200}}},
	}}

	err := ValidateNodePortClaims(nc)
	if err == nil {
		t.Fatal("expected conflicting claims to be rejected")
	}
	for _, want := range []string{"i-001", "9200", "tenant-1-elasticsearch", "tenant-2-elasticsearch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not identify %q", err, want)
		}
	}
}

func TestValidateNodePortClaims_RejectsSelfDuplicate(t *testing.T) {
	nc := NodeConfig{Node: "i-001", Services: []ServiceConfig{
		{Name: "a", PortForwards: []PortForward{{HostPort: 8080, VMPort: 80}, {HostPort: 8080, VMPort: 81}}},
	}}

	if err := ValidateNodePortClaims(nc); err == nil {
		t.Fatal("expected a service claiming one host port twice to be rejected")
	}
}

func TestValidateNodePortClaims_AllowsDistinctClaims(t *testing.T) {
	nc := NodeConfig{Node: "i-001", Services: []ServiceConfig{
		{Name: "a", PortForwards: []PortForward{{HostPort: 9200, VMPort: 9200}, {HostPort: 9300, VMPort: 9300}}},
		{Name: "b", PortForwards: []PortForward{{HostPort: 9201, VMPort: 9200}}},
		{Name: "c"},
	}}

	if err := ValidateNodePortClaims(nc); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
