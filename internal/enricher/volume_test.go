package enricher

import (
	"fmt"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

func TestEnrichServiceResolvesAndSortsVolumes(t *testing.T) {
	service := EnrichService(ServiceSpec{Name: "app", Image: "/image", Volumes: []VolumeSpec{
		{Name: "logs", Type: config.VolumeTypeShared, MountPath: "/var/log/app"},
		{Name: "data", Type: config.VolumeTypeLocal, MountPath: "/var/lib/app", Size: "20Gi"},
	}}, Defaults{VolumeDefaults: VolumeDefaults{SharedSize: "5Gi"}})
	if len(service.Volumes) != 2 || service.Volumes[0].Name != "data" || service.Volumes[1].Name != "logs" {
		t.Fatalf("volumes not deterministically sorted: %#v", service.Volumes)
	}
	if service.Volumes[0].SizeBytes != 20*config.GiB || service.Volumes[1].SizeBytes != 5*config.GiB {
		t.Fatalf("unexpected resolved sizes: %#v", service.Volumes)
	}
}

func TestValidateInputRejectsUnsafeVolumes(t *testing.T) {
	input := &InputConfig{Services: []ServiceSpec{{
		Name: "app", Image: "/image", NodeType: "node", Volumes: []VolumeSpec{
			{Name: "Data", Type: config.VolumeTypeLocal, MountPath: "/proc/data", Size: "1TB"},
			{Name: "data", Type: config.VolumeTypeLocal, MountPath: "/proc/data/child", Size: "1Gi"},
		},
	}}}
	err := ValidateInput(input)
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, want := range []string{"DNS-label", "reserved", "overlapping", "positive integer"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("validation error %q does not contain %q", err, want)
		}
	}
}

func TestValidateInputRejectsMoreVolumesThanGuestDeviceRange(t *testing.T) {
	volumes := make([]VolumeSpec, config.MaxServiceVolumes+1)
	for i := range volumes {
		volumes[i] = VolumeSpec{
			Name: fmt.Sprintf("data-%d", i), Type: config.VolumeTypeLocal,
			MountPath: fmt.Sprintf("/data/%d", i), Size: "1Gi",
		}
	}
	err := ValidateInput(&InputConfig{Services: []ServiceSpec{{
		Name: "app", Image: "/image", NodeType: "node", Volumes: volumes,
	}}})
	if err == nil || !strings.Contains(err.Error(), "at most 25 volumes") {
		t.Fatalf("expected volume-count validation error, got %v", err)
	}
}

func TestTenantVolumeOverrideReplacesBaseList(t *testing.T) {
	base := []ServiceSpec{{Name: "db", Image: "/db.ext4", NodeType: "node", Volumes: []VolumeSpec{{Name: "base", Type: config.VolumeTypeLocal, MountPath: "/data"}}}}
	tenants := []TenantConfig{{ID: "tenant", Services: []TenantServiceFile{{BaseName: "db", Override: TenantOverride{Volumes: []VolumeSpec{{Name: "tenant", Type: config.VolumeTypeLocal, MountPath: "/tenant"}}}}}}}
	got := ExpandTenants(base, tenants)
	if len(got) != 1 || len(got[0].Volumes) != 1 || got[0].Volumes[0].Name != "tenant" {
		t.Fatalf("tenant volumes were not replaced: %#v", got)
	}
}
