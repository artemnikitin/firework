package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
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
	if help := os.Getenv("FIREWORK_AGENT_HELP_TEST"); help != "" {
		os.Args = []string{"firework-agent"}
		if help == "clear-recovery" {
			os.Args = append(os.Args, "clear-recovery", "--forget")
		}
		os.Args = append(os.Args, "-h")
		main()
		return
	}
	service := os.Getenv("FIREWORK_CLEAR_RECOVERY_SERVICE")
	if service == "" {
		service = "missing"
	}
	configPath := os.Getenv("FIREWORK_AGENT_CONFIG")
	if os.Getenv("FIREWORK_CLEAR_RECOVERY_ROOT_FLAGS") == "1" {
		os.Args = []string{"firework-agent", "--config", configPath, "clear-recovery"}
	} else {
		os.Args = []string{"firework-agent", "clear-recovery", "--config", configPath}
	}
	if os.Getenv("FIREWORK_CLEAR_RECOVERY_FORGET") == "1" {
		os.Args = append(os.Args, "--forget")
	}
	os.Args = append(os.Args, service)
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
		"FIREWORK_CLEAR_RECOVERY_SERVICE=missing",
		"FIREWORK_CLEAR_RECOVERY_ROOT_FLAGS=1",
	)
	output, err = command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "service missing has no tracked VM") {
		t.Fatalf("root-level config placement failed: err=%v output=%s", err, output)
	}
	if err := runCommand([]string{"clear-recovery", "missing"}, configPath); err != nil {
		t.Fatalf("clear-recovery did not use the provided default config path: %v", err)
	}
	for _, rootFlags := range []string{"0", "1"} {
		command = exec.Command(os.Args[0], "-test.run=^TestClearRecoveryCommandHelper$")
		command.Env = append(os.Environ(),
			"FIREWORK_CLEAR_RECOVERY_HELPER=1",
			"FIREWORK_AGENT_CONFIG="+configPath,
			"FIREWORK_CLEAR_RECOVERY_SERVICE=missing",
			"FIREWORK_CLEAR_RECOVERY_FORGET=1",
			"FIREWORK_CLEAR_RECOVERY_ROOT_FLAGS="+rootFlags,
		)
		output, err = command.CombinedOutput()
		if err != nil || !strings.Contains(string(output), "service missing has no tracked VM") {
			t.Fatalf("guarded forget command failed with root flags %s: err=%v output=%s", rootFlags, err, output)
		}
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
	command = exec.Command(os.Args[0], "-test.run=^TestClearRecoveryCommandHelper$")
	command.Env = append(os.Environ(),
		"FIREWORK_CLEAR_RECOVERY_HELPER=1",
		"FIREWORK_AGENT_CONFIG="+configPath,
		"FIREWORK_CLEAR_RECOVERY_SERVICE=app",
		"FIREWORK_CLEAR_RECOVERY_FORGET=1",
	)
	output, err = command.CombinedOutput()
	// Forget deletes corrupt metadata only once a process scan proves that
	// nothing owns it. Platforms that cannot scan processes keep refusing.
	if goruntime.GOOS == "linux" {
		if err != nil || !strings.Contains(string(output), "cleared") {
			t.Fatalf("guarded forget did not clear absence-proven corrupt state: err=%v output=%s", err, output)
		}
		if _, err := os.Stat(quarantinedDir); !os.IsNotExist(err) {
			t.Fatalf("forgotten corrupt state remains: %v", err)
		}
	} else if err == nil || !strings.Contains(string(output), "remains recovery_pending") {
		t.Fatalf("guarded forget did not preserve the agent's refusal of unproven runtime state: err=%v output=%s", err, output)
	}
}

func TestClearRecoveryCommandSendsForgetOnlyWhenRequested(t *testing.T) {
	for _, forget := range []bool{false, true} {
		t.Run(fmt.Sprintf("forget=%t", forget), func(t *testing.T) {
			stateDir, err := os.MkdirTemp("/tmp", "firework48-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
			controlDir := filepath.Join(stateDir, "control")
			if err := os.Mkdir(controlDir, 0o700); err != nil {
				t.Fatal(err)
			}
			socketPath := filepath.Join(controlDir, "recovery.sock")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			if err := os.Chmod(socketPath, 0o600); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(stateDir, "agent.yaml")
			if err := os.WriteFile(configPath, []byte(fmt.Sprintf("node_name: web\nstore_type: git\nstore_url: unused\nstate_dir: %q\n", stateDir)), 0o600); err != nil {
				t.Fatal(err)
			}
			checked := make(chan error, 1)
			go func() {
				checked <- checkRecoveryCommandRequest(listener, forget)
			}()
			args := []string{"clear-recovery", "--config", configPath}
			if forget {
				args = append(args, "--forget")
			}
			if err := runCommand(append(args, "app"), "unused"); err != nil {
				t.Fatal(err)
			}
			if err := <-checked; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func checkRecoveryCommandRequest(listener *net.UnixListener, forget bool) error {
	connection, err := listener.AcceptUnix()
	if err != nil {
		return err
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	var size uint32
	if err := binary.Read(connection, binary.BigEndian, &size); err != nil {
		return err
	}
	if size == 0 || size > 4096 {
		return fmt.Errorf("invalid recovery request size %d", size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(connection, body); err != nil {
		return err
	}
	var request struct {
		Version int    `json:"version"`
		Service string `json:"service"`
		Forget  bool   `json:"forget"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return err
	}
	if request.Version != 1 || request.Service != "app" || request.Forget != forget {
		return fmt.Errorf("unexpected recovery command request: %#v; want forget=%t", request, forget)
	}
	response := []byte(`{"version":1,"result":{"service":"app","outcome":"no_op"}}`)
	if err := binary.Write(connection, binary.BigEndian, uint32(len(response))); err != nil {
		return err
	}
	_, err = connection.Write(response)
	return err
}

type commandTestStore struct{}

func (*commandTestStore) Fetch(context.Context, string) ([]byte, error) {
	return []byte("node: web\nservices: []\n"), nil
}

func (*commandTestStore) Revision(context.Context) (string, error) { return "rev-1", nil }

func (*commandTestStore) Close() error { return nil }

func TestAgentHelpExitsSuccessfully(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		want string
	}{
		{name: "agent", mode: "1", want: "Usage of firework-agent"},
		{name: "guarded forget", mode: "clear-recovery", want: "[--forget] SERVICE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestClearRecoveryCommandHelper$")
			cmd.Env = append(os.Environ(), "FIREWORK_CLEAR_RECOVERY_HELPER=1", "FIREWORK_AGENT_HELP_TEST="+tc.mode)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("help failed: %v %s", err, out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("missing help: %s", out)
			}
		})
	}
}
