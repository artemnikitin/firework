package vm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

// State represents the lifecycle state of a microVM.
type State string

const (
	StateRunning State = "running"
	StateStopped State = "stopped"
	StateFailed  State = "failed"
)

// Instance represents a running Firecracker microVM.
type Instance struct {
	// Name is the service name from the config.
	Name string
	// Config is the desired service configuration.
	Config config.ServiceConfig
	// State is the current lifecycle state.
	State State
	// PID is the Firecracker process ID (0 if not running).
	PID int
	// SocketPath is the path to the Firecracker API socket.
	SocketPath string
}

// Manager manages the lifecycle of Firecracker microVMs on the local host.
type Manager struct {
	firecrackerBin string
	stateDir       string
	logger         *slog.Logger

	mu        sync.Mutex
	instances map[string]*Instance
}

// NewManager creates a new VM manager.
func NewManager(firecrackerBin, stateDir string, logger *slog.Logger) *Manager {
	return &Manager{
		firecrackerBin: firecrackerBin,
		stateDir:       stateDir,
		logger:         logger,
		instances:      make(map[string]*Instance),
	}
}

// List returns a snapshot of all known VM instances.
func (m *Manager) List() map[string]*Instance {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make(map[string]*Instance, len(m.instances))
	for k, v := range m.instances {
		cp := *v
		result[k] = &cp
	}
	return result
}

// Start launches a new Firecracker microVM for the given service config.
func (m *Manager) Start(ctx context.Context, svc config.ServiceConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if inst, exists := m.instances[svc.Name]; exists && inst.State == StateRunning {
		return fmt.Errorf("service %s is already running (pid %d)", svc.Name, inst.PID)
	}

	m.logger.Info("starting microVM", "service", svc.Name, "vcpus", svc.VCPUs, "memory_mb", svc.MemoryMB)

	vmDir := filepath.Join(m.stateDir, "vms", svc.Name)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return fmt.Errorf("creating vm dir: %w", err)
	}

	socketPath := filepath.Join(vmDir, "firecracker.sock")
	// Remove stale socket if it exists.
	_ = os.Remove(socketPath)

	configPath, err := m.writeVMConfig(vmDir, svc)
	if err != nil {
		return fmt.Errorf("writing vm config: %w", err)
	}

	cmd := exec.CommandContext(ctx, m.firecrackerBin,
		"--api-sock", socketPath,
		"--config-file", configPath,
	)

	logFile, err := os.Create(filepath.Join(vmDir, "firecracker.log"))
	if err != nil {
		return fmt.Errorf("creating log file: %w", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	// Start firecracker in its own process group so it survives agent restart.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("starting firecracker: %w", err)
	}

	m.instances[svc.Name] = &Instance{
		Name:       svc.Name,
		Config:     svc,
		State:      StateRunning,
		PID:        cmd.Process.Pid,
		SocketPath: socketPath,
	}

	// Monitor the process in a goroutine.
	go m.monitor(svc.Name, cmd, logFile)

	m.logger.Info("microVM started", "service", svc.Name, "pid", cmd.Process.Pid)
	return nil
}

// Stop gracefully shuts down a running microVM.
func (m *Manager) Stop(name string) error {
	m.mu.Lock()
	inst, exists := m.instances[name]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("service %s not found", name)
	}
	m.mu.Unlock()

	m.logger.Info("stopping microVM", "service", name, "pid", inst.PID)

	proc, err := os.FindProcess(inst.PID)
	if err != nil {
		return fmt.Errorf("finding process %d: %w", inst.PID, err)
	}

	// Send SIGTERM first, giving the VM a chance to shut down.
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		m.logger.Warn("SIGTERM failed, sending SIGKILL", "service", name, "error", err)
		_ = proc.Signal(syscall.SIGKILL)
	}

	// Wait for the process to actually exit so device handles (TAP, sockets)
	// are released before a subsequent start.
	if !waitForPIDExit(inst.PID, 5*time.Second) {
		m.logger.Warn("microVM did not exit after SIGTERM, sending SIGKILL",
			"service", name, "pid", inst.PID)
		_ = proc.Signal(syscall.SIGKILL)
		_ = waitForPIDExit(inst.PID, 2*time.Second)
	}

	m.mu.Lock()
	inst.State = StateStopped
	m.mu.Unlock()

	// Clean up socket.
	_ = os.Remove(inst.SocketPath)

	m.logger.Info("microVM stopped", "service", name)
	return nil
}

// Remove stops (if running) and removes all state for a service.
func (m *Manager) Remove(name string) error {
	m.mu.Lock()
	inst, exists := m.instances[name]
	m.mu.Unlock()

	if exists && inst.State == StateRunning {
		if err := m.Stop(name); err != nil {
			m.logger.Warn("error stopping VM during remove", "service", name, "error", err)
		}
	}

	m.mu.Lock()
	delete(m.instances, name)
	m.mu.Unlock()

	vmDir := filepath.Join(m.stateDir, "vms", name)
	if err := os.RemoveAll(vmDir); err != nil {
		return fmt.Errorf("removing vm dir: %w", err)
	}

	return nil
}

// IsRunning checks if the process for a service is still alive.
func (m *Manager) IsRunning(name string) bool {
	m.mu.Lock()
	inst, exists := m.instances[name]
	m.mu.Unlock()

	if !exists || inst.State != StateRunning {
		return false
	}

	proc, err := os.FindProcess(inst.PID)
	if err != nil {
		return false
	}

	// Signal 0 checks process existence without actually sending a signal.
	return proc.Signal(syscall.Signal(0)) == nil
}

// monitor waits for the firecracker process to exit and updates state.
func (m *Manager) monitor(name string, cmd *exec.Cmd, logFile *os.File) {
	defer logFile.Close()

	err := cmd.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()

	inst, exists := m.instances[name]
	if !exists {
		return
	}

	if err != nil {
		// Stop() marks instances as stopped before process exit. In that case
		// a non-zero Wait result is expected and should not flip state to failed.
		if inst.State == StateStopped {
			m.logger.Debug("microVM exited after stop", "service", name)
			return
		}
		m.logger.Error("microVM exited with error", "service", name, "error", err)
		inst.State = StateFailed
	} else {
		m.logger.Info("microVM exited cleanly", "service", name)
		inst.State = StateStopped
	}
}

func waitForPIDExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		proc, err := os.FindProcess(pid)
		if err != nil {
			return true
		}
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// writeVMConfig writes a Firecracker JSON config file for the given service.
func (m *Manager) writeVMConfig(vmDir string, svc config.ServiceConfig) (string, error) {
	kernelArgs := svc.KernelArgs
	if kernelArgs == "" {
		kernelArgs = "console=ttyS0 reboot=k panic=1 pci=off"
	}

	// Build network config snippet.
	networkCfg := ""
	if svc.Network != nil {
		guestMAC := svc.Network.GuestMAC
		if guestMAC == "" {
			guestMAC = "AA:FC:00:00:00:01"
		}
		networkCfg = fmt.Sprintf(`,
  "network-interfaces": [{
    "iface_id": "eth0",
    "guest_mac": %q,
    "host_dev_name": %q
  }]`, guestMAC, svc.Network.Interface)
	}

	configJSON := fmt.Sprintf(`{
  "boot-source": {
    "kernel_image_path": %q,
    "boot_args": %q
  },
  "drives": [{
    "drive_id": "rootfs",
    "path_on_host": %q,
    "is_root_device": true,
    "is_read_only": false
  }],
  "machine-config": {
    "vcpu_count": %d,
    "mem_size_mib": %d
  }%s
}`, svc.Kernel, kernelArgs, svc.Image, svc.VCPUs, svc.MemoryMB, networkCfg)

	configPath := filepath.Join(vmDir, "vm-config.json")
	if err := os.WriteFile(configPath, []byte(configJSON), 0o644); err != nil {
		return "", err
	}
	return configPath, nil
}
