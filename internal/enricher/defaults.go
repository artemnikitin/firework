package enricher

import (
	"fmt"
	"hash/fnv"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

const (
	fallbackKernel     = "/var/lib/images/vmlinux-5.10"
	fallbackVCPUs      = 1
	fallbackMemoryMB   = 256
	fallbackKernelArgs = "console=ttyS0 reboot=k panic=1 pci=off init=/sbin/fc-init"

	fallbackHealthCheckInterval = "10s"
	fallbackHealthCheckTimeout  = "5s"
	fallbackHealthCheckRetries  = 3
)

// EnrichService takes a lightweight ServiceSpec and fills in all missing
// fields from the provided Defaults, producing a complete ServiceConfig.
//
// Priority: explicit spec field > defaults.yaml value > hardcoded fallback.
//
// Network details (IP, MAC, TAP) are not set here — the agent handles
// per-instance networking at runtime. Health checks output port+path
// instead of a composed target URL for the same reason.
func EnrichService(spec ServiceSpec, defs Defaults) config.ServiceConfig {
	svc := config.ServiceConfig{
		Name:              spec.Name,
		Image:             spec.Image,
		Env:               spec.Env,
		Links:             spec.Links,
		Metadata:          spec.Metadata,
		AntiAffinityGroup: spec.AntiAffinityGroup,
		NodeHostIPEnv:     spec.NodeHostIPEnv,
	}
	if len(spec.CrossNodeLinks) > 0 {
		svc.CrossNodeLinks = make([]config.CrossNodeLink, len(spec.CrossNodeLinks))
		copy(svc.CrossNodeLinks, spec.CrossNodeLinks)
	}

	svc.Kernel = coalesce(spec.Kernel, defs.Kernel, fallbackKernel)
	svc.VCPUs = coalesceInt(spec.VCPUs, defs.VCPUs, fallbackVCPUs)
	svc.MemoryMB = coalesceInt(spec.MemoryMB, defs.MemoryMB, fallbackMemoryMB)
	svc.KernelArgs = coalesce(spec.KernelArgs, defs.KernelArgs, fallbackKernelArgs)

	if spec.Network {
		svc.Network = &config.NetworkConfig{
			Interface: tapIfname(spec.Name),
		}
	}
	if len(spec.PortForwards) > 0 {
		svc.PortForwards = make([]config.PortForward, len(spec.PortForwards))
		copy(svc.PortForwards, spec.PortForwards)
	}

	hcSpec := mergeHealthCheck(spec.HealthCheck, defs.HealthCheck)
	if hcSpec != nil {
		svc.HealthCheck = buildHealthCheck(hcSpec)
	}

	return svc
}

// mergeHealthCheck merges spec-level health check with defaults.
func mergeHealthCheck(spec, defs *HealthCheckSpec) *HealthCheckSpec {
	if spec == nil && defs == nil {
		return nil
	}
	if spec == nil {
		return defs
	}
	if defs == nil {
		return spec
	}

	merged := *spec
	if merged.Type == "" {
		merged.Type = defs.Type
	}
	if merged.Port == 0 {
		merged.Port = defs.Port
	}
	if merged.Path == "" {
		merged.Path = defs.Path
	}
	if merged.Interval == "" {
		merged.Interval = defs.Interval
	}
	if merged.Timeout == "" {
		merged.Timeout = defs.Timeout
	}
	if merged.Retries == 0 {
		merged.Retries = defs.Retries
	}
	return &merged
}

// buildHealthCheck converts a HealthCheckSpec into a config.HealthCheckConfig.
// It sets Port and Path for the agent to compose the full target at runtime.
func buildHealthCheck(spec *HealthCheckSpec) *config.HealthCheckConfig {
	interval, _ := parseDurationWithFallback(spec.Interval, fallbackHealthCheckInterval)
	timeout, _ := parseDurationWithFallback(spec.Timeout, fallbackHealthCheckTimeout)

	retries := spec.Retries
	if retries == 0 {
		retries = fallbackHealthCheckRetries
	}

	return &config.HealthCheckConfig{
		Type:     spec.Type,
		Port:     spec.Port,
		Path:     spec.Path,
		Interval: interval,
		Timeout:  timeout,
		Retries:  retries,
	}
}

func parseDurationWithFallback(value, fallback string) (time.Duration, error) {
	if value == "" {
		value = fallback
	}
	return time.ParseDuration(value)
}

func coalesce(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// tapIfname returns a TAP interface name for a service, guaranteed to fit
// within the Linux IFNAMSIZ-1 (15 character) limit.
//
// For short names (≤ 11 chars): "tap-" + name (e.g. "tap-web").
// For long names: "tap-" + first 6 chars + 5-hex-char FNV hash, ensuring
// uniqueness even when service names share a long common prefix
// (e.g. "tenant-3-elasticsearch-data-1" vs "tenant-3-elasticsearch-data-2").
func tapIfname(serviceName string) string {
	const prefix = "tap-"
	const maxSuffix = 11 // 15 - len("tap-")

	if len(serviceName) <= maxSuffix {
		return prefix + serviceName
	}

	// Use a hash suffix to guarantee uniqueness for truncated names.
	h := fnv.New32a()
	h.Write([]byte(serviceName))
	short := fmt.Sprintf("%s%05x", serviceName[:6], h.Sum32()&0xfffff)
	return prefix + short
}

func coalesceInt(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}
