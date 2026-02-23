package enricher

import (
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

func TestEnrichService_AllSpecified(t *testing.T) {
	spec := ServiceSpec{
		Name:       "web",
		Image:      "/images/web.ext4",
		Kernel:     "/kernels/custom",
		VCPUs:      4,
		MemoryMB:   1024,
		KernelArgs: "custom=args",
		NodeType:   "general-purpose",
		Metadata:   map[string]string{"version": "1.0"},
	}
	defs := Defaults{
		Kernel:     "/kernels/default",
		VCPUs:      2,
		MemoryMB:   512,
		KernelArgs: "default=args",
	}

	svc := EnrichService(spec, defs)

	if svc.Kernel != "/kernels/custom" {
		t.Errorf("expected /kernels/custom, got %s", svc.Kernel)
	}
	if svc.VCPUs != 4 {
		t.Errorf("expected 4 vcpus, got %d", svc.VCPUs)
	}
	if svc.MemoryMB != 1024 {
		t.Errorf("expected 1024 memory, got %d", svc.MemoryMB)
	}
	if svc.KernelArgs != "custom=args" {
		t.Errorf("expected custom=args, got %s", svc.KernelArgs)
	}
	if svc.Metadata["version"] != "1.0" {
		t.Errorf("expected version=1.0 in metadata")
	}
}

func TestEnrichService_FallsBackToDefaults(t *testing.T) {
	spec := ServiceSpec{
		Name:  "web",
		Image: "/images/web.ext4",
	}
	defs := Defaults{
		Kernel:     "/kernels/default",
		VCPUs:      2,
		MemoryMB:   512,
		KernelArgs: "default=args",
	}

	svc := EnrichService(spec, defs)

	if svc.Kernel != "/kernels/default" {
		t.Errorf("expected /kernels/default, got %s", svc.Kernel)
	}
	if svc.VCPUs != 2 {
		t.Errorf("expected 2, got %d", svc.VCPUs)
	}
	if svc.MemoryMB != 512 {
		t.Errorf("expected 512, got %d", svc.MemoryMB)
	}
}

func TestEnrichService_FallsBackToHardcoded(t *testing.T) {
	spec := ServiceSpec{
		Name:  "web",
		Image: "/images/web.ext4",
	}

	svc := EnrichService(spec, Defaults{})

	if svc.Kernel != fallbackKernel {
		t.Errorf("expected %s, got %s", fallbackKernel, svc.Kernel)
	}
	if svc.VCPUs != fallbackVCPUs {
		t.Errorf("expected %d, got %d", fallbackVCPUs, svc.VCPUs)
	}
	if svc.MemoryMB != fallbackMemoryMB {
		t.Errorf("expected %d, got %d", fallbackMemoryMB, svc.MemoryMB)
	}
	if svc.KernelArgs != fallbackKernelArgs {
		t.Errorf("expected %s, got %s", fallbackKernelArgs, svc.KernelArgs)
	}
}

func TestEnrichService_NoNetwork(t *testing.T) {
	spec := ServiceSpec{
		Name:  "batch",
		Image: "/images/batch.ext4",
	}

	svc := EnrichService(spec, Defaults{})

	if svc.Network != nil {
		t.Errorf("expected nil network, got %+v", svc.Network)
	}
}

func TestEnrichService_HealthCheckPortAndPath(t *testing.T) {
	spec := ServiceSpec{
		Name:  "web",
		Image: "/images/web.ext4",
		HealthCheck: &HealthCheckSpec{
			Type:     "http",
			Port:     8080,
			Path:     "/ready",
			Interval: "15s",
			Timeout:  "3s",
			Retries:  5,
		},
	}

	svc := EnrichService(spec, Defaults{})

	hc := svc.HealthCheck
	if hc == nil {
		t.Fatal("expected health check")
	}
	if hc.Type != "http" {
		t.Errorf("expected http, got %s", hc.Type)
	}
	if hc.Port != 8080 {
		t.Errorf("expected port 8080, got %d", hc.Port)
	}
	if hc.Path != "/ready" {
		t.Errorf("expected /ready, got %s", hc.Path)
	}
	if hc.Target != "" {
		t.Errorf("expected empty target (agent fills it), got %s", hc.Target)
	}
	if hc.Interval != 15*time.Second {
		t.Errorf("expected 15s interval, got %s", hc.Interval)
	}
	if hc.Timeout != 3*time.Second {
		t.Errorf("expected 3s timeout, got %s", hc.Timeout)
	}
	if hc.Retries != 5 {
		t.Errorf("expected 5 retries, got %d", hc.Retries)
	}
}

func TestEnrichService_HealthCheckTCP(t *testing.T) {
	spec := ServiceSpec{
		Name:  "db",
		Image: "/images/db.ext4",
		HealthCheck: &HealthCheckSpec{
			Type: "tcp",
			Port: 5432,
		},
	}

	svc := EnrichService(spec, Defaults{})

	if svc.HealthCheck.Port != 5432 {
		t.Errorf("expected port 5432, got %d", svc.HealthCheck.Port)
	}
	if svc.HealthCheck.Path != "" {
		t.Errorf("expected empty path for TCP, got %s", svc.HealthCheck.Path)
	}
}

func TestEnrichService_HealthCheckFromDefaults(t *testing.T) {
	spec := ServiceSpec{
		Name:  "web",
		Image: "/images/web.ext4",
	}
	defs := Defaults{
		HealthCheck: &HealthCheckSpec{
			Type:     "http",
			Port:     8080,
			Path:     "/health",
			Interval: "10s",
			Timeout:  "5s",
			Retries:  3,
		},
	}

	svc := EnrichService(spec, defs)

	if svc.HealthCheck == nil {
		t.Fatal("expected health check from defaults")
	}
	if svc.HealthCheck.Port != 8080 {
		t.Errorf("expected port 8080 from defaults, got %d", svc.HealthCheck.Port)
	}
	if svc.HealthCheck.Path != "/health" {
		t.Errorf("expected /health from defaults, got %s", svc.HealthCheck.Path)
	}
}

func TestEnrichService_HealthCheckMerge(t *testing.T) {
	spec := ServiceSpec{
		Name:  "web",
		Image: "/images/web.ext4",
		HealthCheck: &HealthCheckSpec{
			Type: "http",
			Port: 9090, // Override port.
		},
	}
	defs := Defaults{
		HealthCheck: &HealthCheckSpec{
			Type:     "http",
			Port:     8080,
			Path:     "/health",
			Interval: "10s",
			Timeout:  "5s",
			Retries:  3,
		},
	}

	svc := EnrichService(spec, defs)

	// Port from spec, path from defaults.
	if svc.HealthCheck.Port != 9090 {
		t.Errorf("expected port 9090, got %d", svc.HealthCheck.Port)
	}
	if svc.HealthCheck.Path != "/health" {
		t.Errorf("expected /health from defaults, got %s", svc.HealthCheck.Path)
	}
	if svc.HealthCheck.Retries != 3 {
		t.Errorf("expected 3 retries from defaults, got %d", svc.HealthCheck.Retries)
	}
}

func TestEnrichService_NoHealthCheck(t *testing.T) {
	spec := ServiceSpec{
		Name:  "batch",
		Image: "/images/batch.ext4",
	}

	svc := EnrichService(spec, Defaults{})

	if svc.HealthCheck != nil {
		t.Errorf("expected nil health check, got %+v", svc.HealthCheck)
	}
}

func TestEnrichService_WithNetwork(t *testing.T) {
	spec := ServiceSpec{
		Name:    "web",
		Image:   "/images/web.ext4",
		Network: true,
	}

	svc := EnrichService(spec, Defaults{})

	if svc.Network == nil {
		t.Fatal("expected non-nil network")
	}
	if svc.Network.Interface != "tap-web" {
		t.Errorf("expected interface tap-web, got %s", svc.Network.Interface)
	}
	// IP, MAC, HostDevName should be empty — agent fills them at runtime.
	if svc.Network.GuestIP != "" {
		t.Errorf("expected empty guest IP, got %s", svc.Network.GuestIP)
	}
	if svc.Network.GuestMAC != "" {
		t.Errorf("expected empty guest MAC, got %s", svc.Network.GuestMAC)
	}
}

func TestEnrichService_WithoutNetwork(t *testing.T) {
	spec := ServiceSpec{
		Name:    "batch",
		Image:   "/images/batch.ext4",
		Network: false,
	}

	svc := EnrichService(spec, Defaults{})

	if svc.Network != nil {
		t.Errorf("expected nil network, got %+v", svc.Network)
	}
}

func TestEnrichService_PortForwards(t *testing.T) {
	spec := ServiceSpec{
		Name:  "web",
		Image: "/images/web.ext4",
		PortForwards: []config.PortForward{
			{HostPort: 80, VMPort: 8080},
			{HostPort: 443, VMPort: 8443},
		},
	}

	svc := EnrichService(spec, Defaults{})

	if len(svc.PortForwards) != 2 {
		t.Fatalf("expected 2 port forwards, got %d", len(svc.PortForwards))
	}
	if svc.PortForwards[0].HostPort != 80 || svc.PortForwards[0].VMPort != 8080 {
		t.Errorf("expected 80->8080, got %d->%d", svc.PortForwards[0].HostPort, svc.PortForwards[0].VMPort)
	}
	if svc.PortForwards[1].HostPort != 443 || svc.PortForwards[1].VMPort != 8443 {
		t.Errorf("expected 443->8443, got %d->%d", svc.PortForwards[1].HostPort, svc.PortForwards[1].VMPort)
	}

	// Verify it's a copy, not a shared slice.
	spec.PortForwards[0].HostPort = 9999
	if svc.PortForwards[0].HostPort == 9999 {
		t.Error("port forwards should be copied, not shared")
	}
}

func TestEnrichService_NoPortForwards(t *testing.T) {
	spec := ServiceSpec{
		Name:  "batch",
		Image: "/images/batch.ext4",
	}

	svc := EnrichService(spec, Defaults{})

	if len(svc.PortForwards) != 0 {
		t.Errorf("expected no port forwards, got %d", len(svc.PortForwards))
	}
}

func TestTapIfname_ShortName(t *testing.T) {
	got := tapIfname("web")
	if got != "tap-web" {
		t.Errorf("expected tap-web, got %s", got)
	}
}

func TestTapIfname_MaxLengthName(t *testing.T) {
	// 11-char name fits exactly without truncation.
	got := tapIfname("12345678901")
	if got != "tap-12345678901" {
		t.Errorf("expected tap-12345678901, got %s", got)
	}
	if len(got) > 15 {
		t.Errorf("interface name too long: %d chars", len(got))
	}
}

func TestTapIfname_LongName_MaxLength(t *testing.T) {
	// Names longer than 11 chars must produce a ≤15 char result.
	got := tapIfname("tenant-3-elasticsearch-data-1")
	if len(got) > 15 {
		t.Errorf("interface name too long: %d chars (%s)", len(got), got)
	}
	if !strings.HasPrefix(got, "tap-") {
		t.Errorf("expected tap- prefix, got %s", got)
	}
}

func TestTapIfname_LongNameCollision(t *testing.T) {
	// Two services that share a long common prefix must get distinct names.
	a := tapIfname("tenant-3-elasticsearch-data-1")
	b := tapIfname("tenant-3-elasticsearch-data-2")
	if a == b {
		t.Errorf("collision: both %q and %q produced %s", "tenant-3-elasticsearch-data-1", "tenant-3-elasticsearch-data-2", a)
	}
}

func TestTapIfname_Deterministic(t *testing.T) {
	name := "tenant-3-elasticsearch-data-1"
	a, b := tapIfname(name), tapIfname(name)
	if a != b {
		t.Errorf("tapIfname is not deterministic: %s != %s", a, b)
	}
}
