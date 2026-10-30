package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/config"
)

func retainedResourceAgent(t *testing.T) *Agent {
	t.Helper()
	cfg := testAgentConfig(t)
	cfg.VMSubnet = "172.16.0.0/24"
	cfg.VMGateway = "172.16.0.1"
	a := New(cfg, &fakeStore{}, testLogger())
	svc := config.ServiceConfig{Name: "removed", Image: "/image", Kernel: "/kernel", VCPUs: 2, MemoryMB: 256, Network: &config.NetworkConfig{GuestIP: "172.16.0.2", GuestMAC: guestMACForIndex(0)}}
	body, err := json.Marshal(svc)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	dir := filepath.Join(cfg.StateDir, "vms", svc.Name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{"schema_version": 1, "service": svc.Name, "instance_id": "retained-instance", "lifecycle": "running", "config": svc, "config_hash": hex.EncodeToString(hash[:]), "pid": 42, "host_boot_id": "old", "process_start_ticks": 1, "executable": "/firecracker", "executable_device": 1, "executable_inode": 2, "vm_dir": dir, "socket_path": filepath.Join(dir, "firecracker.sock"), "config_path": filepath.Join(dir, "vm-config.json"), "launcher": "direct"}
	body, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instance.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.vmManager.DiscoverRecoveryState(); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNetworkAndCapacityReserveRemovedQuarantine(t *testing.T) {
	a := retainedResourceAgent(t)
	desired := []config.ServiceConfig{{Name: "new", VCPUs: 3, MemoryMB: 128, Network: &config.NetworkConfig{}}}
	a.assignNetworking(desired)
	if desired[0].Network.GuestIP == "172.16.0.2" || desired[0].Network.GuestMAC == guestMACForIndex(0) {
		t.Fatal("reused retained VM IP/MAC")
	}
	used, held := a.reserveCapacity(desired, capacity.NodeCapacity{VCPUs: 4, MemoryMB: 1024}, true)
	if held["new"] == "" || used.VCPUs != 2 {
		t.Fatalf("retained VM capacity forgotten: used=%#v held=%v", used, held)
	}
	a.reconciler.SetHeldServices(held)
	if got := a.reconciler.Plan(config.NodeConfig{Services: desired}); len(got) != 0 {
		t.Fatalf("held VM entered plan: %#v", got)
	}
}

func TestHealthRestartWaitsForReconciliationLifecycleTransaction(t *testing.T) {
	a := New(testAgentConfig(t), &fakeStore{}, testLogger())
	a.lifecycleMu.Lock()
	done := make(chan struct{})
	go func() { _ = a.restartService(context.Background(), "missing"); close(done) }()
	select {
	case <-done:
		a.lifecycleMu.Unlock()
		t.Fatal("health restart bypassed lifecycle transaction")
	case <-time.After(20 * time.Millisecond):
	}
	a.lifecycleMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("health restart did not resume")
	}
}
