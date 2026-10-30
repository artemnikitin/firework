package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

type recordingImageSyncer struct {
	calls int
	err   error
}

func (s *recordingImageSyncer) Sync(context.Context, []config.ServiceConfig) error {
	s.calls++
	return s.err
}
func (s *recordingImageSyncer) Close() error { return nil }

func TestRecoveryRetryImageSyncCommitsOnlySuccessfulRevision(t *testing.T) {
	s := &recordingImageSyncer{}
	a := &Agent{imageSyncer: s}
	ctx := context.Background()
	if err := a.syncImages(ctx, nil, "one"); err != nil {
		t.Fatal(err)
	}
	s.err = errors.New("object store unavailable")
	if err := a.syncImages(ctx, nil, "one"); err != nil {
		t.Fatal("recovery repeated completed download", err)
	}
	if err := a.syncImages(ctx, nil, "two"); err == nil {
		t.Fatal("expected download failure")
	}
	if err := a.syncImages(ctx, nil, "two"); err == nil {
		t.Fatal("failed download was cached")
	}
	s.err = nil
	if err := a.syncImages(ctx, nil, "two"); err != nil {
		t.Fatal(err)
	}
	if err := a.syncImages(ctx, nil, "two"); err != nil {
		t.Fatal(err)
	}
	if s.calls != 4 {
		t.Fatalf("sync calls = %d", s.calls)
	}
}

func TestQuarantineDoesNotFreezeAppliedRevisionOrRepeatImages(t *testing.T) {
	cfg := testAgentConfig(t)
	dir := filepath.Join(cfg.StateDir, "vms", "service")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instance.json"), []byte("{invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &fakeStore{data: map[string][]byte{"web": []byte("node: web\nservices:\n- name: service\n  image: /img/service\n  kernel: /kern\n  vcpus: 1\n  memory_mb: 128\n")}, revision: "one"}
	a := New(cfg, s, testLogger())
	images := &recordingImageSyncer{}
	a.imageSyncer = images
	for _, rev := range []string{"one", "one", "two", "two"} {
		s.revision = rev
		a.tick(context.Background())
		status := a.agentStatusSnapshot()
		if status.AppliedRevision != rev {
			t.Fatalf("quarantine froze revision: %#v", status)
		}
		if len(status.Services) != 1 || status.Services[0].ReasonCode != "vm_recovery_pending" {
			t.Fatalf("quarantine lost service status: %#v", status.Services)
		}
		condition, ok := agentCondition(status, "VMRecoveryReady")
		if !ok || condition.Status != "false" || condition.ReasonCode != "vm_recovery_pending" {
			t.Fatalf("quarantine lost degraded condition: %#v", status.Conditions)
		}
	}
	s.data["web"] = []byte("node: web\nservices: []\n")
	s.revision = "three"
	a.tick(context.Background())
	status := a.agentStatusSnapshot()
	if status.AppliedRevision != "three" || status.DesiredServices != 0 || len(status.Services) != 1 || status.Services[0].ReasonCode != "vm_recovery_pending" {
		t.Fatalf("removed quarantine lost or froze revision: %#v", status)
	}
	if condition, ok := agentCondition(status, "VMRecoveryReady"); !ok || condition.Status != "false" {
		t.Fatalf("removed quarantine lost degraded condition: %#v", status.Conditions)
	}
	if images.calls != 3 {
		t.Fatalf("pending retries repeated image sync: %d", images.calls)
	}
}

func TestFailedImageSyncInvalidatesPreviousRevision(t *testing.T) {
	s := &recordingImageSyncer{}
	a := &Agent{imageSyncer: s}
	ctx := context.Background()
	if err := a.syncImages(ctx, nil, "one"); err != nil {
		t.Fatal(err)
	}
	s.err = errors.New("partial download")
	if err := a.syncImages(ctx, nil, "two"); err == nil {
		t.Fatal("expected failure")
	}
	s.err = nil
	if err := a.syncImages(ctx, nil, "one"); err != nil {
		t.Fatal(err)
	}
	if s.calls != 3 {
		t.Fatalf("rollback reused partial artifacts: calls=%d", s.calls)
	}
}
