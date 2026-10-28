package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimeagent "github.com/artemnikitin/firework/internal/agent"
	"github.com/artemnikitin/firework/internal/agentconfig"
)

func TestClearRecoveryCommandHelper(t *testing.T) {
	if os.Getenv("FIREWORK_CLEAR_RECOVERY_HELPER") != "1" {
		return
	}
	service := os.Getenv("FIREWORK_CLEAR_RECOVERY_SERVICE")
	if service == "" {
		service = "missing"
	}
	os.Args = []string{
		"firework-agent", "clear-recovery", "--config", os.Getenv("FIREWORK_AGENT_CONFIG"), service,
	}
	main()
}

func TestClearRecoveryCommandUsesRunningAgentSocket(t *testing.T) {
	testDir, err := os.MkdirTemp("/tmp", "firework48-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(testDir) })
	stateDir := filepath.Join(testDir, "s")
	configPath := filepath.Join(testDir, "agent.yaml")
	configData := []byte(fmt.Sprintf(`node_name: web
store_type: git
store_url: unused
state_dir: %q
enable_health_checks: false
enable_network_setup: false
enable_capacity_check: false
`, stateDir))
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	quarantinedDir := filepath.Join(stateDir, "vms", "app")
	if err := os.MkdirAll(quarantinedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(quarantinedDir, "instance.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := agentconfig.LoadAgentConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.PollInterval = time.Hour
	store := &commandTestStore{}
	runtime := runtimeagent.New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	finished := false
	t.Cleanup(func() {
		if !finished {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("agent did not stop after cancellation")
			}
		}
	})

	socketPath := filepath.Join(stateDir, "control", "recovery.sock")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Lstat(socketPath); err == nil {
			break
		}
		select {
		case err := <-done:
			finished = true
			t.Fatalf("agent stopped before opening its local command socket: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("agent did not open its local recovery socket")
		}
		time.Sleep(10 * time.Millisecond)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestClearRecoveryCommandHelper$")
	command.Env = append(os.Environ(),
		"FIREWORK_CLEAR_RECOVERY_HELPER=1",
		"FIREWORK_AGENT_CONFIG="+configPath,
		"FIREWORK_CLEAR_RECOVERY_SERVICE=missing",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("firework-agent clear-recovery failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "service missing has no tracked VM") {
		t.Fatalf("command did not report the live agent's response: %s", output)
	}

	command = exec.Command(os.Args[0], "-test.run=^TestClearRecoveryCommandHelper$")
	command.Env = append(os.Environ(),
		"FIREWORK_CLEAR_RECOVERY_HELPER=1",
		"FIREWORK_AGENT_CONFIG="+configPath,
		"FIREWORK_CLEAR_RECOVERY_SERVICE=app",
	)
	output, err = command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "remains recovery_pending") {
		t.Fatalf("command did not refuse the quarantined startup state after the initial reconciliation error: err=%v output=%s", err, output)
	}
}

type commandTestStore struct{}

func (*commandTestStore) Fetch(context.Context, string) ([]byte, error) {
	return []byte("node: web\nservices: []\n"), nil
}

func (*commandTestStore) Revision(context.Context) (string, error) { return "rev-1", nil }

func (*commandTestStore) Close() error { return nil }
