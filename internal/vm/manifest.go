package vm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/volume"
)

const (
	manifestSchemaVersion = 1
	manifestFileName      = "instance.json"
)

type manifestLifecycle string

const (
	lifecycleStarting manifestLifecycle = "starting"
	lifecycleRunning  manifestLifecycle = "running"
	lifecycleStopping manifestLifecycle = "stopping"
	lifecycleStopped  manifestLifecycle = "stopped"
	lifecycleFailed   manifestLifecycle = "failed"
)

// instanceManifest is the durable ownership record for one Firecracker
// process. The process identity fields prevent a recycled PID from being
// mistaken for a VM owned by Firework.
type instanceManifest struct {
	SchemaVersion int                     `json:"schema_version"`
	Service       string                  `json:"service"`
	InstanceID    string                  `json:"instance_id"`
	Lifecycle     manifestLifecycle       `json:"lifecycle"`
	Config        config.ServiceConfig    `json:"config"`
	ConfigHash    string                  `json:"config_hash"`
	PID           int                     `json:"pid,omitempty"`
	HostBootID    string                  `json:"host_boot_id,omitempty"`
	ProcessStart  uint64                  `json:"process_start_ticks,omitempty"`
	Executable    string                  `json:"executable,omitempty"`
	ExecutableDev uint64                  `json:"executable_device,omitempty"`
	ExecutableIno uint64                  `json:"executable_inode,omitempty"`
	SocketPath    string                  `json:"socket_path"`
	ConfigPath    string                  `json:"config_path"`
	VMDir         string                  `json:"vm_dir"`
	Launcher      string                  `json:"launcher"`
	LauncherUnit  string                  `json:"launcher_unit,omitempty"`
	Legacy        bool                    `json:"legacy_process,omitempty"`
	StartedAt     time.Time               `json:"started_at"`
	Volumes       []volume.PreparedVolume `json:"volumes,omitempty"`
	LastError     string                  `json:"last_error,omitempty"`
}

func serviceConfigHash(svc config.ServiceConfig) (string, error) {
	data, err := json.Marshal(svc)
	if err != nil {
		return "", fmt.Errorf("marshal service config: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func manifestPath(vmDir string) string {
	return filepath.Join(vmDir, manifestFileName)
}

func readManifest(path string) (*instanceManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var manifest instanceManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if manifest.SchemaVersion != manifestSchemaVersion {
		return nil, fmt.Errorf("unsupported manifest schema %d", manifest.SchemaVersion)
	}
	if manifest.Service == "" || manifest.InstanceID == "" || manifest.VMDir == "" {
		return nil, fmt.Errorf("manifest is missing required identity fields")
	}
	return &manifest, nil
}

func writeManifest(path string, manifest *instanceManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create manifest directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".instance-*.tmp")
	if err != nil {
		return fmt.Errorf("create manifest temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close manifest: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace manifest: %w", err)
	}
	dirHandle, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer dirHandle.Close()
	return dirHandle.Sync()
}
