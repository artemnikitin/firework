package traefik

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// localSvc builds a routable local service with a guest IP and one port forward.
func localSvc(name, ip string, hostPort, vmPort int, meta map[string]string) config.ServiceConfig {
	return config.ServiceConfig{
		Name:         name,
		Network:      &config.NetworkConfig{GuestIP: ip},
		PortForwards: []config.PortForward{{HostPort: hostPort, VMPort: vmPort}},
		Metadata:     meta,
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("expected %s to exist: %v", name, err)
	}
	return string(data)
}

func TestSync_LocalSubdomain(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "gcp.example.com", testLogger())

	svc := localSvc("kibana", "172.16.0.2", 5611, 5601, map[string]string{"subdomain": "tenant-1"})
	if err := m.Sync([]config.ServiceConfig{svc}, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := read(t, dir, "kibana.yaml")
	if !strings.Contains(content, "Host(`tenant-1.gcp.example.com`)") {
		t.Errorf("expected composed FQDN rule, got:\n%s", content)
	}
	if !strings.Contains(content, "172.16.0.2:5601") {
		t.Errorf("expected backend URL, got:\n%s", content)
	}
}

func TestSync_RemoteSubdomain(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "example.com", testLogger())

	remote := []config.NodeConfig{{
		Node:   "node-2",
		HostIP: "10.0.1.5",
		Services: []config.ServiceConfig{
			{Name: "tenant-1-kibana", PortForwards: []config.PortForward{{HostPort: 5611, VMPort: 5601}}, Metadata: map[string]string{"subdomain": "tenant-1"}},
		},
	}}
	if err := m.Sync(nil, remote); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := read(t, dir, "remote-tenant-1-kibana.yaml")
	if !strings.Contains(content, "Host(`tenant-1.example.com`)") {
		t.Errorf("expected composed FQDN rule, got:\n%s", content)
	}
	if !strings.Contains(content, "10.0.1.5:5611") {
		t.Errorf("expected peer hostIP:hostPort, got:\n%s", content)
	}
}

func TestSync_ExactHostCompatibility(t *testing.T) {
	for _, domain := range []string{"", "example.com"} {
		dir := t.TempDir()
		m := NewManager(dir, domain, testLogger())
		svc := localSvc("kibana", "172.16.0.2", 5611, 5601, map[string]string{"host": "custom.example.net"})
		if err := m.Sync([]config.ServiceConfig{svc}, nil); err != nil {
			t.Fatalf("domain=%q unexpected error: %v", domain, err)
		}
		content := read(t, dir, "kibana.yaml")
		if !strings.Contains(content, "Host(`custom.example.net`)") {
			t.Errorf("domain=%q expected exact host rule, got:\n%s", domain, content)
		}
	}
}

func TestSync_NeitherKeyWritesNoFile(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "example.com", testLogger())
	svc := localSvc("elasticsearch", "172.16.0.3", 9200, 9200, nil)
	if err := m.Sync([]config.ServiceConfig{svc}, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("expected no files for unrouted service, got %d", len(entries))
	}
}

func TestSync_SubdomainWithoutIngressDomainErrors(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "", testLogger())
	svc := localSvc("kibana", "172.16.0.2", 5611, 5601, map[string]string{"subdomain": "tenant-1"})
	if err := m.Sync([]config.ServiceConfig{svc}, nil); err == nil {
		t.Fatal("expected error for subdomain without ingress_domain")
	}
}

func TestSync_BothKeysErrors(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "example.com", testLogger())
	svc := localSvc("kibana", "172.16.0.2", 5611, 5601, map[string]string{"subdomain": "tenant-1", "host": "x.example.com"})
	if err := m.Sync([]config.ServiceConfig{svc}, nil); err == nil {
		t.Fatal("expected error when both subdomain and host are set")
	}
}

func TestSync_EmptyAndWhitespaceRoutingValuesError(t *testing.T) {
	cases := []map[string]string{
		{"subdomain": ""},
		{"subdomain": "  "},
		{"host": ""},
		{"host": "   "},
	}
	for _, meta := range cases {
		dir := t.TempDir()
		m := NewManager(dir, "example.com", testLogger())
		svc := localSvc("kibana", "172.16.0.2", 5611, 5601, meta)
		if err := m.Sync([]config.ServiceConfig{svc}, nil); err == nil {
			t.Errorf("meta=%v: expected error for present-but-empty routing value", meta)
		}
	}
}

func TestSync_InvalidSubdomainsError(t *testing.T) {
	subs := []string{"a.b", "Tenant-1", "-tenant", "tenant-", strings.Repeat("a", 64)}
	for _, s := range subs {
		dir := t.TempDir()
		m := NewManager(dir, "example.com", testLogger())
		svc := localSvc("kibana", "172.16.0.2", 5611, 5601, map[string]string{"subdomain": s})
		if err := m.Sync([]config.ServiceConfig{svc}, nil); err == nil {
			t.Errorf("subdomain=%q: expected error", s)
		}
	}
}

func TestSync_InvalidExactHostInjectionError(t *testing.T) {
	hosts := []string{"a`b.example.com", "x.com`) || Host(`y.com", "http://x.example.com", "x.example.com:8080", "*.example.com"}
	for _, h := range hosts {
		dir := t.TempDir()
		m := NewManager(dir, "example.com", testLogger())
		svc := localSvc("kibana", "172.16.0.2", 5611, 5601, map[string]string{"host": h})
		if err := m.Sync([]config.ServiceConfig{svc}, nil); err == nil {
			t.Errorf("host=%q: expected error", h)
		}
	}
}

func TestSync_TrailingDotDomainNormalization(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "gcp.example.com.", testLogger())
	svc := localSvc("kibana", "172.16.0.2", 5611, 5601, map[string]string{"subdomain": "tenant-1"})
	if err := m.Sync([]config.ServiceConfig{svc}, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	content := read(t, dir, "kibana.yaml")
	if !strings.Contains(content, "Host(`tenant-1.gcp.example.com`)") {
		t.Errorf("expected trailing dot stripped, got:\n%s", content)
	}
}

func TestSync_IdenticalLocalAndRemoteResolution(t *testing.T) {
	localDir := t.TempDir()
	lm := NewManager(localDir, "example.com", testLogger())
	if err := lm.Sync([]config.ServiceConfig{localSvc("tenant-1-kibana", "172.16.0.2", 5611, 5601, map[string]string{"subdomain": "tenant-1"})}, nil); err != nil {
		t.Fatalf("local sync: %v", err)
	}
	local := read(t, localDir, "tenant-1-kibana.yaml")

	remoteDir := t.TempDir()
	rm := NewManager(remoteDir, "example.com", testLogger())
	remote := []config.NodeConfig{{Node: "node-2", HostIP: "10.0.1.5", Services: []config.ServiceConfig{
		{Name: "tenant-1-kibana", PortForwards: []config.PortForward{{HostPort: 5611, VMPort: 5601}}, Metadata: map[string]string{"subdomain": "tenant-1"}},
	}}}
	if err := rm.Sync(nil, remote); err != nil {
		t.Fatalf("remote sync: %v", err)
	}
	remoteContent := read(t, remoteDir, "remote-tenant-1-kibana.yaml")

	const rule = "Host(`tenant-1.example.com`)"
	if !strings.Contains(local, rule) || !strings.Contains(remoteContent, rule) {
		t.Errorf("expected identical host rule in local and remote routes")
	}
}

func TestSync_DuplicateLocalHostnamesError(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "example.com", testLogger())
	svcs := []config.ServiceConfig{
		localSvc("svc-a", "172.16.0.2", 5611, 5601, map[string]string{"subdomain": "tenant-1"}),
		localSvc("svc-b", "172.16.0.3", 5612, 5601, map[string]string{"subdomain": "tenant-1"}),
	}
	if err := m.Sync(svcs, nil); err == nil {
		t.Fatal("expected duplicate hostname error for two local services")
	}
}

// A hostname claimed by a local service must win over a peer claim: during a
// reschedule the stale peer file still lists the service this node now runs.
// The remote entry is skipped with a warning instead of failing the sync.
func TestSync_LocalWinsRemoteHostnameConflict(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "example.com", testLogger())
	local := []config.ServiceConfig{localSvc("svc-a", "172.16.0.2", 5611, 5601, map[string]string{"subdomain": "tenant-1"})}
	remote := []config.NodeConfig{{Node: "node-2", HostIP: "10.0.1.5", Services: []config.ServiceConfig{
		{Name: "svc-a", PortForwards: []config.PortForward{{HostPort: 5612, VMPort: 5601}}, Metadata: map[string]string{"subdomain": "tenant-1"}},
	}}}
	if err := m.Sync(local, remote); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(read(t, dir, "svc-a.yaml"), "172.16.0.2:5601") {
		t.Error("expected local route to win the hostname claim")
	}
	if _, err := os.Stat(filepath.Join(dir, "remote-svc-a.yaml")); !os.IsNotExist(err) {
		t.Error("expected conflicting remote route to be skipped")
	}
}

// Two peers claiming the same hostname resolve deterministically: peers are
// visited in node-name order and the first claim wins, regardless of the
// order the peer configs were listed in.
func TestSync_RemoteRemoteHostnameConflictDeterministic(t *testing.T) {
	peerSvc := func() config.ServiceConfig {
		return config.ServiceConfig{
			Name:         "svc-a",
			PortForwards: []config.PortForward{{HostPort: 5611, VMPort: 5601}},
			Metadata:     map[string]string{"subdomain": "tenant-1"},
		}
	}
	nodeA := config.NodeConfig{Node: "node-a", HostIP: "10.0.1.5", Services: []config.ServiceConfig{peerSvc()}}
	nodeB := config.NodeConfig{Node: "node-b", HostIP: "10.0.1.9", Services: []config.ServiceConfig{peerSvc()}}

	for _, remote := range [][]config.NodeConfig{{nodeA, nodeB}, {nodeB, nodeA}} {
		dir := t.TempDir()
		m := NewManager(dir, "example.com", testLogger())
		if err := m.Sync(nil, remote); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(read(t, dir, "remote-svc-a.yaml"), "10.0.1.5:5611") {
			t.Error("expected node-a (first in node-name order) to win the claim")
		}
	}
}

func TestSync_RoutedWithoutGuestIPError(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "example.com", testLogger())
	svc := config.ServiceConfig{
		Name:         "kibana",
		Network:      &config.NetworkConfig{}, // no guest IP
		PortForwards: []config.PortForward{{HostPort: 5611, VMPort: 5601}},
		Metadata:     map[string]string{"subdomain": "tenant-1"},
	}
	if err := m.Sync([]config.ServiceConfig{svc}, nil); err == nil {
		t.Fatal("expected error for routed service without guest IP")
	}
}

func TestSync_RoutedWithoutBackendPortError(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "example.com", testLogger())
	svc := config.ServiceConfig{
		Name:     "kibana",
		Network:  &config.NetworkConfig{GuestIP: "172.16.0.2"},
		Metadata: map[string]string{"subdomain": "tenant-1"},
		// no port_forwards and no health check port
	}
	if err := m.Sync([]config.ServiceConfig{svc}, nil); err == nil {
		t.Fatal("expected error for routed service without backend port")
	}
}

// Peer configs are authoritative but never fatal: a routed remote service
// without a host port cannot be proxied, so it is skipped (and any stale route
// for it removed) instead of failing the sync for every other route.
func TestSync_RoutedRemoteWithoutHostPortSkipped(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "example.com", testLogger())
	stale := filepath.Join(dir, "remote-tenant-1-kibana.yaml")
	if err := os.WriteFile(stale, []byte("http: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	remote := []config.NodeConfig{{Node: "node-2", HostIP: "10.0.1.5", Services: []config.ServiceConfig{
		{Name: "tenant-1-kibana", Metadata: map[string]string{"subdomain": "tenant-1"}}, // no port forwards
	}}}
	if err := m.Sync(nil, remote); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("expected route for unroutable remote service to be removed")
	}
}

// SyncLocal keeps every existing remote-* file while still applying and
// cleaning up local routes — the degraded mode used when the peer set is
// unknown.
func TestSyncLocal_PreservesRemoteRoutesAndSyncsLocal(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "example.com", testLogger())

	remoteLKG := filepath.Join(dir, "remote-peer-svc.yaml")
	if err := os.WriteFile(remoteLKG, []byte("http: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	staleLocal := filepath.Join(dir, "old-local.yaml")
	if err := os.WriteFile(staleLocal, []byte("http: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	svc := localSvc("kibana", "172.16.0.2", 5611, 5601, map[string]string{"subdomain": "tenant-1"})
	if err := m.SyncLocal([]config.ServiceConfig{svc}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(remoteLKG); err != nil {
		t.Errorf("expected last-known-good remote route preserved: %v", err)
	}
	if _, err := os.Stat(staleLocal); !os.IsNotExist(err) {
		t.Error("expected stale local route to be removed")
	}
	if !strings.Contains(read(t, dir, "kibana.yaml"), "Host(`tenant-1.example.com`)") {
		t.Error("expected local route to be applied")
	}
}

// SyncLocal enforces the same strict local validation as Sync.
func TestSyncLocal_InvalidLocalMetadataErrors(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "example.com", testLogger())
	svc := localSvc("kibana", "172.16.0.2", 5611, 5601, map[string]string{"subdomain": "Bad.Label"})
	if err := m.SyncLocal([]config.ServiceConfig{svc}); err == nil {
		t.Fatal("expected error for invalid local routing metadata")
	}
}

// TestSync_SubdomainWithHealthCheckPortBackend proves the enricher/traefik
// backend-port logic agrees: a routed service with only a health-check port
// (no port_forwards) still renders a local route using that port.
func TestSync_SubdomainWithHealthCheckPortBackend(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "example.com", testLogger())
	svc := config.ServiceConfig{
		Name:        "kibana",
		Network:     &config.NetworkConfig{GuestIP: "172.16.0.2"},
		HealthCheck: &config.HealthCheckConfig{Port: 5601},
		Metadata:    map[string]string{"subdomain": "tenant-1"},
	}
	if err := m.Sync([]config.ServiceConfig{svc}, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	content := read(t, dir, "kibana.yaml")
	if !strings.Contains(content, "172.16.0.2:5601") {
		t.Errorf("expected health-check port as backend, got:\n%s", content)
	}
}

func TestSync_SkipsRemoteServiceWithoutHostIP(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "example.com", testLogger())
	remote := []config.NodeConfig{{Node: "node-2", HostIP: "", Services: []config.ServiceConfig{
		{Name: "tenant-1-kibana", PortForwards: []config.PortForward{{HostPort: 5611, VMPort: 5601}}, Metadata: map[string]string{"subdomain": "tenant-1"}},
	}}}
	if err := m.Sync(nil, remote); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("expected no files when peer HostIP empty, got %d", len(entries))
	}
}

func TestSync_DeletesStaleFiles(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "example.com", testLogger())
	stale := filepath.Join(dir, "old-service.yaml")
	if err := os.WriteFile(stale, []byte("http: {}"), 0644); err != nil {
		t.Fatal(err)
	}
	svc := localSvc("kibana", "172.16.0.2", 5611, 5601, map[string]string{"subdomain": "tenant-1"})
	if err := m.Sync([]config.ServiceConfig{svc}, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("expected stale file to be deleted")
	}
	read(t, dir, "kibana.yaml")
}

func TestSync_PreservesNonYAMLFiles(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "example.com", testLogger())
	other := filepath.Join(dir, "traefik.toml")
	if err := os.WriteFile(other, []byte("# static"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := m.Sync(nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("expected non-YAML file preserved: %v", err)
	}
}

func TestSync_ValidationFailureMakesNoChanges(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "example.com", testLogger())
	// Seed a last-known-good file.
	good := localSvc("kibana", "172.16.0.2", 5611, 5601, map[string]string{"subdomain": "tenant-1"})
	if err := m.Sync([]config.ServiceConfig{good}, nil); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	before := read(t, dir, "kibana.yaml")

	// A new revision with an invalid service must fail without touching files.
	bad := []config.ServiceConfig{
		good,
		localSvc("broken", "172.16.0.3", 5612, 5601, map[string]string{"subdomain": "Bad.Label"}),
	}
	if err := m.Sync(bad, nil); err == nil {
		t.Fatal("expected validation error")
	}
	if got := read(t, dir, "kibana.yaml"); got != before {
		t.Error("expected existing file unchanged after validation failure")
	}
	if _, err := os.Stat(filepath.Join(dir, "broken.yaml")); !os.IsNotExist(err) {
		t.Error("expected no file written for invalid service")
	}
}

func TestSync_WriteFailureKeepsStaleThenConverges(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, "example.com", testLogger())

	// A stale file from a previous revision.
	stale := filepath.Join(dir, "old-service.yaml")
	if err := os.WriteFile(stale, []byte("http: {}"), 0644); err != nil {
		t.Fatal(err)
	}
	// Obstruct the rename target for "kibana.yaml" with a non-empty directory so
	// renaming the staged file onto it fails. "aaa.yaml" sorts first and renames
	// successfully before the failure.
	obstacle := filepath.Join(dir, "kibana.yaml")
	if err := os.MkdirAll(obstacle, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(obstacle, "keep"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	svcs := []config.ServiceConfig{
		localSvc("aaa", "172.16.0.2", 5610, 5601, map[string]string{"subdomain": "aaa"}),
		localSvc("kibana", "172.16.0.3", 5611, 5601, map[string]string{"subdomain": "tenant-1"}),
	}
	if err := m.Sync(svcs, nil); err == nil {
		t.Fatal("expected apply error from obstructed rename")
	}

	// First file renamed successfully and is complete.
	if !strings.Contains(read(t, dir, "aaa.yaml"), "Host(`aaa.example.com`)") {
		t.Error("expected first file to be applied completely")
	}
	// Stale file must NOT be deleted on a partial failure.
	if _, err := os.Stat(stale); err != nil {
		t.Error("expected stale file preserved after apply failure")
	}
	// Staging directory must be cleaned up.
	if _, err := os.Stat(filepath.Join(dir, stageDirName)); !os.IsNotExist(err) {
		t.Error("expected staging dir cleaned up after failure")
	}

	// Remove the obstacle and retry: the sync converges.
	if err := os.RemoveAll(obstacle); err != nil {
		t.Fatal(err)
	}
	if err := m.Sync(svcs, nil); err != nil {
		t.Fatalf("retry sync: %v", err)
	}
	read(t, dir, "kibana.yaml")
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("expected stale file deleted after successful convergence")
	}
}
