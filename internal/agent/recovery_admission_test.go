package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/config"
)

func TestAdmissionUnknownOwnerMustNotMaskTrueCapacityHold(t *testing.T) {
	cfg := testAgentConfig(t)
	dir := filepath.Join(cfg.StateDir, "vms", "stale")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instance.json"), []byte("{invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &fakeStore{data: map[string][]byte{"web": []byte("node: web\nservices:\n- name: heavy\n  image: /img\n  kernel: /kern\n  vcpus: 6\n  memory_mb: 128\n")}, revision: "one"}
	a := New(cfg, s, testLogger())
	a.capacityReader = &fakeCapacityReader{cap: capacity.NodeCapacity{VCPUs: 4, MemoryMB: 4096}}
	a.tick(context.Background())
	status := a.agentStatusSnapshot()
	t.Logf("phase=%s applied=%s reason=%s services=%#v conditions=%#v", status.Phase, status.AppliedRevision, status.ReasonCode, status.Services, status.Conditions)
	if status.AppliedRevision == "one" {
		t.Fatal("true over-capacity masked by unknown owner and applied as recovery-only")
	}
	if condition, ok := agentCondition(status, "CapacityReady"); !ok || condition.Status != "false" {
		t.Fatal("genuine capacity failure reported ready")
	}
	// Once demand fits, the remaining hold is genuinely caused by recovery.
	s.data["web"] = []byte(strings.ReplaceAll(string(s.data["web"]), "vcpus: 6", "vcpus: 3"))
	a.tick(context.Background())
	if got := a.agentStatusSnapshot(); got.AppliedRevision != "one" {
		t.Fatalf("fitting recovery hold did not become degraded apply: %#v", got)
	}
}

func TestAdmissionRecoveryOnlyCapacityMustIncludeAllDesired(t *testing.T) {
	a := retainedResourceAgent(t)
	services := []config.ServiceConfig{{Name: "a", VCPUs: 3, MemoryMB: 128}, {Name: "b", VCPUs: 3, MemoryMB: 128}}
	used, holds, _ := a.admit(services, capacity.NodeCapacity{VCPUs: 4, MemoryMB: 4096}, true, nil)
	t.Logf("used=%#v holds=%#v", used, holds)
	for _, h := range holds {
		if !h.Recovery {
			return
		}
	}
	t.Fatal("all holds classified recovery-only although desired total6 exceeds cap4 after recovery")
}

func TestAdmissionCollectivelyOverCapacityMustNotApplyAsRecovery(t *testing.T) {
	a := retainedResourceAgent(t)
	a.store = &fakeStore{data: map[string][]byte{"web": []byte("node: web\nservices:\n- name: a\n  image: /img\n  kernel: /kern\n  vcpus: 3\n  memory_mb: 128\n- name: b\n  image: /img\n  kernel: /kern\n  vcpus: 3\n  memory_mb: 128\n")}, revision: "one"}
	a.capacityReader = &fakeCapacityReader{cap: capacity.NodeCapacity{VCPUs: 4, MemoryMB: 4096}}
	a.tick(context.Background())
	status := a.agentStatusSnapshot()
	t.Logf("phase=%s applied=%s services=%#v", status.Phase, status.AppliedRevision, status.Services)
	if status.AppliedRevision == "one" {
		t.Fatal("desired aggregate6 on cap4 applied as recovery-only")
	}
}
