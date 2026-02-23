package enricher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/artemnikitin/firework/internal/config"
	"gopkg.in/yaml.v3"
)

// TenantOverride holds per-tenant configuration for one service.
//
// Override mode (services/ exists): only the non-zero fields are applied on
// top of the matching base service. source_image is CI-only in this mode.
//
// Standalone mode (no services/ directory): node_type must be set; the file
// is treated as a complete service definition. image defaults to
// /var/lib/images/<tenantID>-<baseName>-rootfs.ext4 if not specified.
type TenantOverride struct {
	SourceImage       string                 `yaml:"source_image,omitempty"`
	NodeType          string                 `yaml:"node_type,omitempty"`
	Image             string                 `yaml:"image,omitempty"`
	VCPUs             int                    `yaml:"vcpus,omitempty"`
	MemoryMB          int                    `yaml:"memory_mb,omitempty"`
	KernelArgs        string                 `yaml:"kernel_args,omitempty"`
	Network           bool                   `yaml:"network,omitempty"`
	PortForwards      []config.PortForward   `yaml:"port_forwards,omitempty"`
	HealthCheck       *HealthCheckSpec       `yaml:"health_check,omitempty"`
	Links             []config.ServiceLink   `yaml:"links,omitempty"`
	Env               map[string]string      `yaml:"env,omitempty"`
	Metadata          map[string]string      `yaml:"metadata,omitempty"`
	AntiAffinityGroup string                 `yaml:"anti_affinity_group,omitempty"`
	CrossNodeLinks    []config.CrossNodeLink `yaml:"cross_node_links,omitempty"`
	NodeHostIPEnv     string                 `yaml:"node_host_ip_env,omitempty"`
}

// TenantServiceFile is a parsed override file for one base service.
type TenantServiceFile struct {
	BaseName string // derived from filename, e.g. "kibana"
	Override TenantOverride
}

// TenantConfig holds all service overrides for one tenant.
type TenantConfig struct {
	ID       string // tenant directory name, e.g. "tenant-1"
	Services []TenantServiceFile
}

// LoadTenants reads <inputDir>/tenants/*/. Returns empty slice if tenants/ doesn't exist.
func LoadTenants(inputDir string) ([]TenantConfig, error) {
	tenantsDir := filepath.Join(inputDir, "tenants")
	entries, err := os.ReadDir(tenantsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading tenants directory: %w", err)
	}

	var tenants []TenantConfig
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		tc, err := loadTenant(tenantsDir, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("loading tenant %s: %w", entry.Name(), err)
		}
		tenants = append(tenants, tc)
	}
	return tenants, nil
}

func loadTenant(tenantsDir, tenantID string) (TenantConfig, error) {
	tenantDir := filepath.Join(tenantsDir, tenantID)
	entries, err := os.ReadDir(tenantDir)
	if err != nil {
		return TenantConfig{}, fmt.Errorf("reading tenant directory: %w", err)
	}

	var services []TenantServiceFile
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(tenantDir, name))
		if err != nil {
			return TenantConfig{}, fmt.Errorf("reading %s: %w", name, err)
		}

		var override TenantOverride
		if err := yaml.Unmarshal(data, &override); err != nil {
			return TenantConfig{}, fmt.Errorf("unmarshaling %s: %w", name, err)
		}

		baseName := strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml")
		services = append(services, TenantServiceFile{
			BaseName: baseName,
			Override: override,
		})
	}

	return TenantConfig{
		ID:       tenantID,
		Services: services,
	}, nil
}

// ExpandTenants generates per-tenant ServiceSpecs from base services + tenant overrides.
//
// Two modes:
//   - Override mode: base service found by name → clone base and apply overrides.
//   - Standalone mode: no matching base service AND node_type is set in the
//     tenant file → treat the file as a complete service definition.
func ExpandTenants(base []ServiceSpec, tenants []TenantConfig) []ServiceSpec {
	baseByName := make(map[string]ServiceSpec, len(base))
	for _, svc := range base {
		baseByName[svc.Name] = svc
	}

	var expanded []ServiceSpec
	for _, tenant := range tenants {
		for _, tsf := range tenant.Services {
			baseSvc, ok := baseByName[tsf.BaseName]
			if !ok {
				// Standalone mode: use the tenant file as a full spec if node_type is set.
				if tsf.Override.NodeType != "" {
					expanded = append(expanded, standaloneSpec(tenant.ID, tsf))
				}
				continue
			}

			spec := cloneServiceSpec(baseSvc)
			spec.Name = tenant.ID + "-" + baseSvc.Name
			spec.Image = deriveTenantImage(baseSvc.Image, tenant.ID)

			ov := tsf.Override
			if ov.VCPUs != 0 {
				spec.VCPUs = ov.VCPUs
			}
			if ov.MemoryMB != 0 {
				spec.MemoryMB = ov.MemoryMB
			}
			if ov.KernelArgs != "" {
				spec.KernelArgs = ov.KernelArgs
			}
			if ov.HealthCheck != nil {
				spec.HealthCheck = ov.HealthCheck
			}

			// Merge Env: base env + override env (override wins).
			if len(ov.Env) > 0 {
				merged := make(map[string]string, len(spec.Env)+len(ov.Env))
				for k, v := range spec.Env {
					merged[k] = v
				}
				for k, v := range ov.Env {
					merged[k] = v
				}
				spec.Env = merged
			}

			// Merge Metadata: same pattern.
			if len(ov.Metadata) > 0 {
				merged := make(map[string]string, len(spec.Metadata)+len(ov.Metadata))
				for k, v := range spec.Metadata {
					merged[k] = v
				}
				for k, v := range ov.Metadata {
					merged[k] = v
				}
				spec.Metadata = merged
			}

			// PortForwards: replace if override set, else inherit base.
			if len(ov.PortForwards) > 0 {
				spec.PortForwards = ov.PortForwards
			}

			// Rewrite Links so each points to the tenant-namespaced service.
			for i := range spec.Links {
				spec.Links[i].Service = tenant.ID + "-" + spec.Links[i].Service
			}

			expanded = append(expanded, spec)
		}
	}
	return expanded
}

// standaloneSpec builds a ServiceSpec directly from a self-contained tenant file.
// Links are rewritten to reference tenant-namespaced service names.
func standaloneSpec(tenantID string, tsf TenantServiceFile) ServiceSpec {
	ov := tsf.Override
	img := ov.Image
	if img == "" {
		img = fmt.Sprintf("/var/lib/images/%s-%s-rootfs.ext4", tenantID, tsf.BaseName)
	}

	spec := ServiceSpec{
		Name:              tenantID + "-" + tsf.BaseName,
		Image:             img,
		NodeType:          ov.NodeType,
		VCPUs:             ov.VCPUs,
		MemoryMB:          ov.MemoryMB,
		KernelArgs:        ov.KernelArgs,
		Network:           ov.Network,
		PortForwards:      ov.PortForwards,
		HealthCheck:       ov.HealthCheck,
		Env:               ov.Env,
		Metadata:          ov.Metadata,
		AntiAffinityGroup: ov.AntiAffinityGroup,
		NodeHostIPEnv:     ov.NodeHostIPEnv,
	}

	for _, link := range ov.Links {
		link.Service = tenantID + "-" + link.Service
		spec.Links = append(spec.Links, link)
	}

	for _, link := range ov.CrossNodeLinks {
		link.Service = tenantID + "-" + link.Service
		spec.CrossNodeLinks = append(spec.CrossNodeLinks, link)
	}

	return spec
}

// cloneServiceSpec returns a deep copy of src.
func cloneServiceSpec(src ServiceSpec) ServiceSpec {
	dst := src

	if src.PortForwards != nil {
		dst.PortForwards = make([]config.PortForward, len(src.PortForwards))
		copy(dst.PortForwards, src.PortForwards)
	}
	if src.Links != nil {
		dst.Links = make([]config.ServiceLink, len(src.Links))
		copy(dst.Links, src.Links)
	}
	if src.CrossNodeLinks != nil {
		dst.CrossNodeLinks = make([]config.CrossNodeLink, len(src.CrossNodeLinks))
		copy(dst.CrossNodeLinks, src.CrossNodeLinks)
	}
	if src.Env != nil {
		dst.Env = make(map[string]string, len(src.Env))
		for k, v := range src.Env {
			dst.Env[k] = v
		}
	}
	if src.Metadata != nil {
		dst.Metadata = make(map[string]string, len(src.Metadata))
		for k, v := range src.Metadata {
			dst.Metadata[k] = v
		}
	}

	return dst
}

// deriveTenantImage prepends the tenant ID to the image filename.
// e.g. /var/lib/images/kibana-rootfs.ext4 → /var/lib/images/tenant-1-kibana-rootfs.ext4
func deriveTenantImage(baseImage, tenantID string) string {
	dir := filepath.Dir(baseImage)
	filename := filepath.Base(baseImage)
	return filepath.Join(dir, tenantID+"-"+filename)
}
