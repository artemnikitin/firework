//go:build linux

package vm

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

func TestRecoverRealSurvivingProcess(t *testing.T) {
	stateDir := t.TempDir()
	vmDir := filepath.Join(stateDir, "vms", "app")
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(vmDir, "firecracker.sock")
	configPath := filepath.Join(vmDir, "vm-config.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	instanceID := "integration-instance"
	command := exec.Command(os.Args[0], "-test.run=TestRecoveryHelperProcess", "--",
		"--id", instanceID, "--api-sock", socketPath, "--config-file", configPath)
	command.Env = append(os.Environ(), "GO_WANT_RECOVERY_HELPER=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- command.Wait() }()
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		select {
		case <-waitCh:
		case <-time.After(time.Second):
		}
	})

	inspector := osProcessInspector{}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := inspector.SocketReady(socketPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper API socket did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	identity, err := inspector.Inspect(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	service := config.ServiceConfig{Name: "app", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}
	hash, err := serviceConfigHash(service)
	if err != nil {
		t.Fatal(err)
	}
	manifest := &instanceManifest{
		SchemaVersion: manifestSchemaVersion, Service: service.Name, InstanceID: instanceID,
		Lifecycle: lifecycleRunning, Config: service, ConfigHash: hash, PID: identity.PID,
		HostBootID: identity.HostBootID, ProcessStart: identity.StartTicks,
		Executable: identity.Executable, ExecutableDev: identity.ExecutableDev, ExecutableIno: identity.ExecutableIno,
		SocketPath: socketPath, ConfigPath: configPath, VMDir: vmDir, Launcher: "direct", StartedAt: time.Now().UTC(),
	}
	if err := writeManifest(manifestPath(vmDir), manifest); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(os.Args[0], stateDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	adopted, err := manager.Recover(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{service}})
	if err != nil {
		t.Fatal(err)
	}
	if len(adopted) != 1 || manager.List()[service.Name].PID != command.Process.Pid {
		t.Fatalf("surviving process was not adopted: %v %#v", adopted, manager.List())
	}
	if err := manager.Remove(service.Name); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waitCh:
	case <-time.After(3 * time.Second):
		t.Fatal("adopted process was not stopped during removal")
	}
	if _, err := os.Stat(vmDir); !os.IsNotExist(err) {
		t.Fatalf("VM state directory still exists after removal: %v", err)
	}
}

func TestRecoveryHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_RECOVERY_HELPER") != "1" {
		return
	}
	var socketPath string
	for index, argument := range os.Args {
		if argument == "--api-sock" && index+1 < len(os.Args) {
			socketPath = os.Args[index+1]
		}
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		os.Exit(2)
	}
	defer listener.Close()
	for {
		connection, err := listener.Accept()
		if err != nil {
			os.Exit(0)
		}
		_ = connection.Close()
	}
}
