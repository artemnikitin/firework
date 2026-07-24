package vm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/volume"
)

const maxKernelCommandLineBytes = 2047

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
	// LastError is the bounded-at-publication process failure reported by Wait.
	LastError string
	// SocketPath is the path to the Firecracker API socket.
	SocketPath string
	// Volumes is the last successfully prepared persistent-volume set.
	Volumes []volume.PreparedVolume
}

// Manager manages the lifecycle of Firecracker microVMs on the local host.
type Manager struct {
	firecrackerBin string
	stateDir       string
	logger         *slog.Logger
	volumeManager  *volume.Manager

	mu           sync.Mutex
	instances    map[string]*Instance
	volumeErrors map[string]string
}

// NewManager creates a new VM manager.
func NewManager(firecrackerBin, stateDir string, logger *slog.Logger) *Manager {
	return NewManagerWithVolumes(firecrackerBin, stateDir, logger, nil)
}

// NewManagerWithVolumes creates a VM manager with persistent-volume support.
func NewManagerWithVolumes(firecrackerBin, stateDir string, logger *slog.Logger, volumeManager *volume.Manager) *Manager {
	return &Manager{
		firecrackerBin: firecrackerBin,
		stateDir:       stateDir,
		logger:         logger,
		volumeManager:  volumeManager,
		instances:      make(map[string]*Instance),
		volumeErrors:   make(map[string]string),
	}
}

// Preflight validates persistent volumes without changing them. Reconciliation
// calls this before stopping an existing VM so a failed resize leaves it live.
func (m *Manager) Preflight(ctx context.Context, svc config.ServiceConfig) error {
	if len(svc.Volumes) == 0 {
		m.setVolumeError(svc.Name, nil)
		return nil
	}
	if err := validateVolumeKernelArgs(svc); err != nil {
		m.setVolumeError(svc.Name, err)
		return err
	}
	if m.volumeManager == nil {
		err := fmt.Errorf("service %s declares volumes but agent storage is not configured", svc.Name)
		m.setVolumeError(svc.Name, err)
		return err
	}
	err := m.volumeManager.Preflight(ctx, svc)
	m.setVolumeError(svc.Name, err)
	return err
}

// VolumeError returns the latest persistent-volume preparation failure for a
// service. It remains visible until a later successful preflight or start.
func (m *Manager) VolumeError(service string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.volumeErrors[service]
}

func (m *Manager) setVolumeError(service string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		delete(m.volumeErrors, service)
		return
	}
	m.volumeErrors[service] = err.Error()
}

func validateVolumeKernelArgs(svc config.ServiceConfig) error {
	volumes := append([]config.VolumeConfig(nil), svc.Volumes...)
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
	guestVolumes := make([]guestVolume, 0, len(volumes))
	for i, volume := range volumes {
		device, err := guestBlockDevice(i)
		if err != nil {
			return err
		}
		guestVolumes = append(guestVolumes, guestVolume{
			Name: volume.Name, Device: device, MountPath: volume.MountPath, Type: volume.Type,
		})
	}
	payload, err := json.Marshal(guestVolumePayload{Version: 1, Volumes: guestVolumes})
	if err != nil {
		return fmt.Errorf("marshal guest volume payload: %w", err)
	}
	kernelArgs := svc.KernelArgs
	if kernelArgs == "" {
		kernelArgs = "console=ttyS0 reboot=k panic=1 pci=off"
	}
	kernelArgs = insertBeforeApplicationSeparator(kernelArgs, "firework.volumes64="+base64.RawURLEncoding.EncodeToString(payload))
	if len(kernelArgs) > maxKernelCommandLineBytes {
		return fmt.Errorf("service %s: kernel command line with volume payload is %d bytes; maximum is %d", svc.Name, len(kernelArgs), maxKernelCommandLineBytes)
	}
	return nil
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

	var prepared []volume.PreparedVolume
	var err error
	if len(svc.Volumes) > 0 {
		if m.volumeManager == nil {
			err := fmt.Errorf("service %s declares volumes but agent storage is not configured", svc.Name)
			m.volumeErrors[svc.Name] = err.Error()
			return err
		}
		prepared, err = m.volumeManager.Prepare(ctx, svc)
		if err != nil {
			m.volumeErrors[svc.Name] = err.Error()
			return fmt.Errorf("preparing volumes: %w", err)
		}
	}

	configPath, err := m.writeVMConfig(vmDir, svc, prepared)
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
		Volumes:    append([]volume.PreparedVolume(nil), prepared...),
	}
	delete(m.volumeErrors, svc.Name)

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
	pid := inst.PID
	socketPath := inst.SocketPath
	m.mu.Unlock()

	m.logger.Info("stopping microVM", "service", name, "pid", pid)

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding process %d: %w", pid, err)
	}

	// Send SIGTERM first, giving the VM a chance to shut down.
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		m.logger.Warn("SIGTERM failed, sending SIGKILL", "service", name, "error", err)
		_ = proc.Signal(syscall.SIGKILL)
	}

	// Wait for the process to actually exit so device handles (TAP, sockets)
	// are released before a subsequent start.
	if !waitForPIDExit(pid, 5*time.Second) {
		m.logger.Warn("microVM did not exit after SIGTERM, sending SIGKILL",
			"service", name, "pid", pid)
		_ = proc.Signal(syscall.SIGKILL)
		_ = waitForPIDExit(pid, 2*time.Second)
	}

	m.mu.Lock()
	inst.State = StateStopped
	inst.PID = 0
	m.mu.Unlock()

	// Clean up socket.
	_ = os.Remove(socketPath)

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
		inst.LastError = err.Error()
	} else {
		m.logger.Info("microVM exited cleanly", "service", name)
		inst.State = StateStopped
	}
	inst.PID = 0
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
func (m *Manager) writeVMConfig(vmDir string, svc config.ServiceConfig, prepared []volume.PreparedVolume) (string, error) {
	kernelArgs := svc.KernelArgs
	if kernelArgs == "" {
		kernelArgs = "console=ttyS0 reboot=k panic=1 pci=off"
	}

	sort.Slice(prepared, func(i, j int) bool { return prepared[i].LogicalID < prepared[j].LogicalID })
	drives := []firecrackerDrive{{DriveID: "rootfs", PathOnHost: svc.Image, IsRootDevice: true, IsReadOnly: false}}
	guestVolumes := make([]guestVolume, 0, len(prepared))
	for i, preparedVolume := range prepared {
		device, err := guestBlockDevice(i)
		if err != nil {
			return "", err
		}
		drives = append(drives, firecrackerDrive{
			DriveID: fmt.Sprintf("volume-%d", i), PathOnHost: preparedVolume.PathOnHost,
			IsRootDevice: false, IsReadOnly: false,
		})
		guestVolumes = append(guestVolumes, guestVolume{
			Name: filepath.Base(preparedVolume.LogicalID), Device: device,
			MountPath: preparedVolume.MountPath, Type: preparedVolume.Type,
		})
	}
	if len(guestVolumes) > 0 {
		payload, err := json.Marshal(guestVolumePayload{Version: 1, Volumes: guestVolumes})
		if err != nil {
			return "", fmt.Errorf("marshal guest volume payload: %w", err)
		}
		arg := "firework.volumes64=" + base64.RawURLEncoding.EncodeToString(payload)
		kernelArgs = insertBeforeApplicationSeparator(kernelArgs, arg)
	}

	var networkInterfaces []firecrackerNetworkInterface
	if svc.Network != nil {
		guestMAC := svc.Network.GuestMAC
		if guestMAC == "" {
			guestMAC = "AA:FC:00:00:00:01"
		}
		networkInterfaces = []firecrackerNetworkInterface{{IfaceID: "eth0", GuestMAC: guestMAC, HostDevName: svc.Network.Interface}}
	}

	vmConfig := firecrackerConfig{
		BootSource:        firecrackerBootSource{KernelImagePath: svc.Kernel, BootArgs: kernelArgs},
		Drives:            drives,
		MachineConfig:     firecrackerMachineConfig{VCPUCount: svc.VCPUs, MemSizeMiB: svc.MemoryMB},
		NetworkInterfaces: networkInterfaces,
	}
	configJSON, err := json.MarshalIndent(vmConfig, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal firecracker config: %w", err)
	}

	configPath := filepath.Join(vmDir, "vm-config.json")
	if err := os.WriteFile(configPath, append(configJSON, '\n'), 0o644); err != nil {
		return "", err
	}
	return configPath, nil
}

type firecrackerConfig struct {
	BootSource        firecrackerBootSource         `json:"boot-source"`
	Drives            []firecrackerDrive            `json:"drives"`
	MachineConfig     firecrackerMachineConfig      `json:"machine-config"`
	NetworkInterfaces []firecrackerNetworkInterface `json:"network-interfaces,omitempty"`
}

type firecrackerBootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type firecrackerDrive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type firecrackerMachineConfig struct {
	VCPUCount  int `json:"vcpu_count"`
	MemSizeMiB int `json:"mem_size_mib"`
}

type firecrackerNetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	GuestMAC    string `json:"guest_mac"`
	HostDevName string `json:"host_dev_name"`
}

type guestVolumePayload struct {
	Version int           `json:"version"`
	Volumes []guestVolume `json:"volumes"`
}

type guestVolume struct {
	Name      string            `json:"name"`
	Device    string            `json:"device"`
	MountPath string            `json:"mount_path"`
	Type      config.VolumeType `json:"type"`
}

func guestBlockDevice(index int) (string, error) {
	if index < 0 || index >= 25 {
		return "", fmt.Errorf("too many additional drives: %d", index+1)
	}
	return fmt.Sprintf("/dev/vd%c", 'b'+rune(index)), nil
}

func insertBeforeApplicationSeparator(args, value string) string {
	fields := strings.Fields(args)
	for i, field := range fields {
		if field == "--" {
			out := append([]string(nil), fields[:i]...)
			out = append(out, value)
			out = append(out, fields[i:]...)
			return strings.Join(out, " ")
		}
	}
	if args == "" {
		return value
	}
	return args + " " + value
}
