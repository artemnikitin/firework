package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/config"
)

type fakeStore struct {
	data              map[string][]byte
	revision          string
	revisionOnFetch   string
	fetchCount        int
	revisionCallCount int

	// listResult/listErr drive the optional NodeConfigLister behavior used by
	// remote Traefik routing.
	listResult []config.NodeConfig
	listErr    error
	listCount  int
}

func (f *fakeStore) ListAllNodeConfigs(context.Context) ([]config.NodeConfig, error) {
	f.listCount++
	return f.listResult, f.listErr
}

// fakeRouteSyncer is an injectable routeSyncer for exercising the agent's
// retry and revision-advance behavior without touching the filesystem.
type fakeRouteSyncer struct {
	calls       int
	err         error
	services    [][]config.ServiceConfig
	remoteNodes [][]config.NodeConfig
}

func (f *fakeRouteSyncer) Sync(services []config.ServiceConfig, remoteNodes []config.NodeConfig) error {
	f.calls++
	f.services = append(f.services, append([]config.ServiceConfig(nil), services...))
	f.remoteNodes = append(f.remoteNodes, append([]config.NodeConfig(nil), remoteNodes...))
	return f.err
}

func (f *fakeStore) Fetch(_ context.Context, nodeName string) ([]byte, error) {
	f.fetchCount++
	if f.revisionOnFetch != "" {
		f.revision = f.revisionOnFetch
	}
	data, ok := f.data[nodeName]
	if !ok {
		return nil, fmt.Errorf("missing node config: %s", nodeName)
	}
	return data, nil
}

func (f *fakeStore) Revision(context.Context) (string, error) {
	f.revisionCallCount++
	return f.revision, nil
}

func (f *fakeStore) Close() error {
	return nil
}

func testAgentConfig(t *testing.T) config.AgentConfig {
	t.Helper()
	disabled := false
	return config.AgentConfig{
		NodeName:            "web",
		NodeNames:           []string{"web"},
		PollInterval:        time.Second,
		FirecrackerBin:      "/usr/bin/firecracker",
		StateDir:            t.TempDir(),
		EnableHealthChecks:  &disabled,
		EnableNetworkSetup:  &disabled,
		EnableCapacityCheck: &disabled,
	}
}

// fakeCapacityReader implements capacity.Reader for tests.
type fakeCapacityReader struct {
	cap capacity.NodeCapacity
	err error
	// reads tracks how many times Read() was called.
	reads int
}

func (f *fakeCapacityReader) Read() (capacity.NodeCapacity, error) {
	f.reads++
	return f.cap, f.err
}

type scriptedCapacityRead struct {
	cap capacity.NodeCapacity
	err error
}

type scriptedCapacityReader struct {
	steps []scriptedCapacityRead
	idx   int
}

func (s *scriptedCapacityReader) Read() (capacity.NodeCapacity, error) {
	if s.idx >= len(s.steps) {
		return capacity.NodeCapacity{}, fmt.Errorf("no scripted read at index %d", s.idx)
	}
	step := s.steps[s.idx]
	s.idx++
	return step.cap, step.err
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestTick_SingleLabelFetchesBeforeRevisionCheck(t *testing.T) {
	store := &fakeStore{
		data: map[string][]byte{
			"web": []byte("node: web\nservices: []\n"),
		},
		revision: "rev-1",
	}

	a := New(testAgentConfig(t), store, testLogger())
	a.lastRevision = "rev-1"

	a.tick(context.Background())

	if store.fetchCount != 1 {
		t.Fatalf("expected one fetch before revision check, got %d", store.fetchCount)
	}
	if store.revisionCallCount != 1 {
		t.Fatalf("expected one revision call, got %d", store.revisionCallCount)
	}
}

func TestTick_SingleLabelRefreshesRevisionAfterFetch(t *testing.T) {
	store := &fakeStore{
		data: map[string][]byte{
			"web": []byte("node: web\nservices: []\n"),
		},
		revision:        "rev-old",
		revisionOnFetch: "rev-new",
	}

	a := New(testAgentConfig(t), store, testLogger())
	a.lastRevision = "rev-old"

	a.tick(context.Background())

	if store.fetchCount != 1 {
		t.Fatalf("expected one fetch, got %d", store.fetchCount)
	}
	if got := a.lastRevision; got != "rev-new" {
		t.Fatalf("expected last revision to update to rev-new, got %q", got)
	}
}

func TestTick_TraefikSyncFailureDoesNotAdvanceRevisionAndRetries(t *testing.T) {
	s := &fakeStore{
		data:     map[string][]byte{"web": []byte("node: web\nservices: []\n")},
		revision: "rev-1",
	}
	a := New(testAgentConfig(t), s, testLogger())
	rs := &fakeRouteSyncer{err: fmt.Errorf("route apply failed")}
	a.traefikMgr = rs

	a.tick(context.Background())
	if a.lastRevision != "" {
		t.Fatalf("expected lastRevision unchanged after sync failure, got %q", a.lastRevision)
	}
	if rs.calls != 1 {
		t.Fatalf("expected one sync attempt, got %d", rs.calls)
	}

	// The next tick must retry the same revision rather than treat the
	// unchanged revision as already applied.
	a.tick(context.Background())
	if rs.calls != 2 {
		t.Fatalf("expected the next tick to retry the sync, got %d sync calls", rs.calls)
	}
}

func TestTick_UnchangedRevisionRefreshesTraefikRoutes(t *testing.T) {
	s := &fakeStore{
		data: map[string][]byte{"web": []byte(`node: web
services:
  - name: local
    network: {}
    port_forwards:
      - host_port: 8080
        vm_port: 8080
    metadata:
      subdomain: local
`)},
		revision: "rev-1",
		listResult: []config.NodeConfig{{
			Node:   "node-2",
			HostIP: "10.0.1.5",
		}},
	}
	cfg := testAgentConfig(t)
	cfg.VMSubnet = "172.16.0.0/24"
	cfg.VMGateway = "172.16.0.1"
	a := New(cfg, s, testLogger())
	rs := &fakeRouteSyncer{}
	a.traefikMgr = rs
	a.lastRevision = "rev-1"

	a.tick(context.Background())
	if a.lastRevision != "rev-1" {
		t.Fatalf("expected lastRevision rev-1 after success, got %q", a.lastRevision)
	}
	if rs.calls != 1 {
		t.Fatalf("expected one sync call, got %d", rs.calls)
	}

	// The local revision is unchanged, but a peer host IP changed. The fast path
	// must still refresh the remote route set and recreate local guest IPs.
	s.listResult[0].HostIP = "10.0.1.9"
	a.tick(context.Background())
	if rs.calls != 2 {
		t.Fatalf("expected route refresh for unchanged local revision, got %d sync calls", rs.calls)
	}
	if got := rs.services[1][0].Network.GuestIP; got != "172.16.0.2" {
		t.Fatalf("expected route refresh to assign local guest IP, got %q", got)
	}
	if got := rs.remoteNodes[1][0].HostIP; got != "10.0.1.9" {
		t.Fatalf("expected refreshed peer host IP, got %q", got)
	}
}

func TestTick_PeerListFailurePreservesLastKnownGoodRoutes(t *testing.T) {
	traefikDir := t.TempDir()
	lkg := filepath.Join(traefikDir, "remote-tenant-1-kibana.yaml")
	if err := os.WriteFile(lkg, []byte("http: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	s := &fakeStore{
		data:     map[string][]byte{"web": []byte("node: web\nservices: []\n")},
		revision: "rev-1",
		listErr:  fmt.Errorf("transient peer-list failure"),
	}
	cfg := testAgentConfig(t)
	cfg.TraefikConfigDir = traefikDir
	a := New(cfg, s, testLogger())

	a.tick(context.Background())

	if _, err := os.Stat(lkg); err != nil {
		t.Fatalf("expected last-known-good remote route preserved on peer-list failure: %v", err)
	}
	if a.lastRevision != "" {
		t.Fatalf("expected revision not advanced on peer-list failure, got %q", a.lastRevision)
	}
}

func TestAssignNetworking_InsertsIPBeforeAppSeparator(t *testing.T) {
	cfg := testAgentConfig(t)
	cfg.VMSubnet = "172.16.0.0/24"
	cfg.VMGateway = "172.16.0.1"

	a := &Agent{
		cfg:    cfg,
		logger: testLogger(),
	}

	services := []config.ServiceConfig{
		{
			Name:       "kibana",
			KernelArgs: "console=ttyS0 init=/sbin/fc-init /bin/tini -- /usr/local/bin/kibana-docker",
			Network:    &config.NetworkConfig{Interface: "tap-kibana"},
		},
	}

	a.assignNetworking(services)

	got := strings.Fields(services[0].KernelArgs)
	ipArg := "ip=172.16.0.2::172.16.0.1:255.255.255.0::eth0:off"

	ipIdx := tokenIndex(got, ipArg)
	sepIdx := tokenIndex(got, "--")
	if ipIdx == -1 {
		t.Fatalf("expected kernel args to include %q, got %q", ipArg, services[0].KernelArgs)
	}
	if sepIdx == -1 {
		t.Fatalf("expected kernel args to include app separator \"--\", got %q", services[0].KernelArgs)
	}
	if ipIdx > sepIdx {
		t.Fatalf("expected %q before \"--\", got %q", ipArg, services[0].KernelArgs)
	}
}

func TestAssignNetworking_AllocatesMACPastLastOctet(t *testing.T) {
	cfg := testAgentConfig(t)
	cfg.VMSubnet = "172.16.0.0/16"
	cfg.VMGateway = "172.16.0.1"

	a := &Agent{
		cfg:    cfg,
		logger: testLogger(),
	}

	services := make([]config.ServiceConfig, 256)
	for i := range services {
		services[i] = config.ServiceConfig{
			Name:    fmt.Sprintf("svc-%03d", i),
			Network: &config.NetworkConfig{Interface: fmt.Sprintf("tap-%03d", i)},
		}
	}

	a.assignNetworking(services)

	if got := services[254].Network.GuestMAC; got != "AA:FC:00:00:00:FF" {
		t.Fatalf("expected 255th MAC to be AA:FC:00:00:00:FF, got %s", got)
	}
	if got := services[255].Network.GuestMAC; got != "AA:FC:00:00:01:00" {
		t.Fatalf("expected 256th MAC to be AA:FC:00:00:01:00, got %s", got)
	}
}

func TestAssignNetworking_StopsWhenSubnetExhausted(t *testing.T) {
	cfg := testAgentConfig(t)
	cfg.VMSubnet = "172.16.0.0/30"
	cfg.VMGateway = "172.16.0.1"

	a := &Agent{
		cfg:    cfg,
		logger: testLogger(),
	}

	services := []config.ServiceConfig{
		{Name: "first", Network: &config.NetworkConfig{Interface: "tap-first"}},
		{Name: "second", Network: &config.NetworkConfig{Interface: "tap-second"}},
	}

	a.assignNetworking(services)

	if got := services[0].Network.GuestIP; got != "172.16.0.2" {
		t.Fatalf("expected first guest IP 172.16.0.2, got %s", got)
	}
	if got := services[0].Network.GuestMAC; got != "AA:FC:00:00:00:01" {
		t.Fatalf("expected first guest MAC AA:FC:00:00:00:01, got %s", got)
	}
	if got := services[1].Network.GuestIP; got != "" {
		t.Fatalf("expected second guest IP to remain unset, got %s", got)
	}
	if got := services[1].Network.GuestMAC; got != "" {
		t.Fatalf("expected second guest MAC to remain unset, got %s", got)
	}
}

func TestInjectEnvVars_InsertsBeforeAppSeparatorAndSortsKeys(t *testing.T) {
	a := &Agent{logger: testLogger()}

	services := []config.ServiceConfig{
		{
			Name:       "kibana",
			KernelArgs: "console=ttyS0 init=/sbin/fc-init /bin/tini -- /usr/local/bin/kibana-docker",
			Env: map[string]string{
				"ELASTICSEARCH_HOSTS": "http://172.16.0.2:9200",
				"A_FLAG":              "1",
			},
		},
	}

	a.injectEnvVars(services)

	got := strings.Fields(services[0].KernelArgs)
	argA := "firework.env.A_FLAG=1"
	argES := "firework.env.ELASTICSEARCH_HOSTS=http://172.16.0.2:9200"

	aIdx := tokenIndex(got, argA)
	esIdx := tokenIndex(got, argES)
	sepIdx := tokenIndex(got, "--")
	if aIdx == -1 || esIdx == -1 {
		t.Fatalf("expected env args in kernel args, got %q", services[0].KernelArgs)
	}
	if sepIdx == -1 {
		t.Fatalf("expected app separator \"--\" in kernel args, got %q", services[0].KernelArgs)
	}
	if !(aIdx < esIdx && esIdx < sepIdx) {
		t.Fatalf("expected sorted env args before \"--\", got %q", services[0].KernelArgs)
	}
}

func TestInjectEnvVars_EncodesValuesWithWhitespace(t *testing.T) {
	a := &Agent{logger: testLogger()}

	services := []config.ServiceConfig{
		{
			Name:       "worker",
			KernelArgs: "console=ttyS0 init=/sbin/fc-init -- /app",
			Env: map[string]string{
				"MESSAGE": "hello world",
			},
		},
	}

	a.injectEnvVars(services)

	got := strings.Fields(services[0].KernelArgs)
	arg := "firework.env64.MESSAGE=aGVsbG8gd29ybGQ"

	argIdx := tokenIndex(got, arg)
	sepIdx := tokenIndex(got, "--")
	if argIdx == -1 {
		t.Fatalf("expected encoded env arg in kernel args, got %q", services[0].KernelArgs)
	}
	if sepIdx == -1 {
		t.Fatalf("expected app separator \"--\" in kernel args, got %q", services[0].KernelArgs)
	}
	if argIdx > sepIdx {
		t.Fatalf("expected %q before \"--\", got %q", arg, services[0].KernelArgs)
	}
	if strings.Contains(services[0].KernelArgs, "firework.env.MESSAGE=hello world") {
		t.Fatalf("expected whitespace value to be encoded, got %q", services[0].KernelArgs)
	}
}

func tokenIndex(tokens []string, want string) int {
	for i, tok := range tokens {
		if tok == want {
			return i
		}
	}
	return -1
}

func TestSumResources_Empty(t *testing.T) {
	got := sumResources(nil)
	if got.VCPUs != 0 || got.MemoryMB != 0 {
		t.Errorf("expected zero capacity for empty services, got %+v", got)
	}
}

func TestSumResources_Multiple(t *testing.T) {
	services := []config.ServiceConfig{
		{Name: "a", VCPUs: 2, MemoryMB: 256},
		{Name: "b", VCPUs: 4, MemoryMB: 1024},
		{Name: "c", VCPUs: 1, MemoryMB: 512},
	}
	got := sumResources(services)
	if got.VCPUs != 7 {
		t.Errorf("expected 7 VCPUs, got %d", got.VCPUs)
	}
	if got.MemoryMB != 1792 {
		t.Errorf("expected 1792 MB, got %d", got.MemoryMB)
	}
}

func TestTick_CapacityCheck_SkipsReconcile_WhenExceeded(t *testing.T) {
	nodeYAML := []byte("node: web\nservices:\n- name: heavy\n  image: /img/heavy\n  kernel: /kern\n  vcpus: 4\n  memory_mb: 256\n")
	s := &fakeStore{
		data:     map[string][]byte{"web": nodeYAML},
		revision: "rev-1",
	}

	a := New(testAgentConfig(t), s, testLogger())
	// Inject fake reader: node has only 1 vCPU, service wants 4.
	a.capacityReader = &fakeCapacityReader{
		cap: capacity.NodeCapacity{VCPUs: 1, MemoryMB: 4096},
	}

	a.tick(context.Background())

	// Reconciliation should have been skipped — no VMs should be running.
	instances := a.vmManager.List()
	if len(instances) != 0 {
		t.Errorf("expected no running VMs when capacity exceeded, got %d", len(instances))
	}
}

func TestTick_CapacityCheck_ProceedsWhenSufficient(t *testing.T) {
	nodeYAML := []byte("node: web\nservices:\n- name: light\n  image: /img/light\n  kernel: /kern\n  vcpus: 1\n  memory_mb: 256\n")
	s := &fakeStore{
		data:     map[string][]byte{"web": nodeYAML},
		revision: "rev-1",
	}

	a := New(testAgentConfig(t), s, testLogger())
	// Inject fake reader: large capacity.
	a.capacityReader = &fakeCapacityReader{
		cap: capacity.NodeCapacity{VCPUs: 64, MemoryMB: 131072},
	}

	a.tick(context.Background())

	// Reconciliation ran — VM should have been attempted.
	// (It will fail because firecracker isn't present, but the reconciler was called.)
	// We verify this indirectly via the metrics reconcile counter.
	out := a.metrics.render()
	if !strings.Contains(out, "firework_agent_reconcile_runs_total") {
		t.Error("expected reconcile counter in metrics output")
	}
	// Capacity gauges should be set.
	if !strings.Contains(out, "firework_node_capacity_vcpus") {
		t.Error("expected capacity vcpus gauge in metrics output")
	}
}

func TestTick_CapacityReaderReadOncePerTick(t *testing.T) {
	nodeYAML := []byte("node: web\nservices:\n- name: light\n  image: /img/light\n  kernel: /kern\n  vcpus: 1\n  memory_mb: 256\n")
	s := &fakeStore{
		data:     map[string][]byte{"web": nodeYAML},
		revision: "rev-1",
	}

	a := New(testAgentConfig(t), s, testLogger())
	reader := &fakeCapacityReader{
		cap: capacity.NodeCapacity{VCPUs: 64, MemoryMB: 131072},
	}
	a.capacityReader = reader

	a.tick(context.Background())

	if reader.reads != 1 {
		t.Fatalf("expected one capacity read per tick, got %d", reader.reads)
	}
}

func TestReadNodeCapacity_UsesLastKnownOnError(t *testing.T) {
	reader := &scriptedCapacityReader{
		steps: []scriptedCapacityRead{
			{cap: capacity.NodeCapacity{VCPUs: 8, MemoryMB: 16384}},
			{err: fmt.Errorf("transient read error")},
		},
	}
	a := &Agent{
		capacityReader: reader,
		logger:         testLogger(),
	}

	first, ok := a.readNodeCapacity()
	if !ok {
		t.Fatal("expected first capacity read to succeed")
	}
	second, ok := a.readNodeCapacity()
	if !ok {
		t.Fatal("expected fallback to last-known capacity")
	}
	if second != first {
		t.Fatalf("expected last-known capacity %+v, got %+v", first, second)
	}
}
