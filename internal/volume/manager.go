// Package volume manages provider-neutral persistent ext4 images on host
// storage pools supplied by the deployment operator.
package volume

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

const (
	manifestFilename    = "manifest.json"
	transactionFilename = "resize-transaction.json"
	imageFilename       = "volume.ext4"
)

var (
	componentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	// ErrSharedUnsupported is returned until durable per-VM lock ownership and
	// backend partition fencing from issue #22 are available.
	ErrSharedUnsupported = errors.New("shared volumes require the durable per-VM supervisor and fencing validation")
)

// PreparedVolume is safe to attach to a stopped/new Firecracker process.
type PreparedVolume struct {
	LogicalID        string
	PathOnHost       string
	MountPath        string
	Type             config.VolumeType
	SizeBytes        int64
	ResizeGeneration int64
}

// Status is the agent-observed state of one logical volume.
type Status struct {
	LogicalID        string
	Type             config.VolumeType
	MountPath        string
	DesiredSizeBytes int64
	AppliedSizeBytes int64
	ResizeGeneration int64
	State            string
	LastError        string
}

type manifest struct {
	LogicalID        string            `json:"logical_id"`
	Type             config.VolumeType `json:"type"`
	BoundNode        string            `json:"bound_node,omitempty"`
	SharedBackendID  string            `json:"shared_backend_id,omitempty"`
	Filesystem       string            `json:"filesystem"`
	AppliedSizeBytes int64             `json:"applied_size_bytes"`
	ResizeGeneration int64             `json:"resize_generation"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

type resizeTransaction struct {
	OldSizeBytes     int64     `json:"old_size_bytes"`
	DesiredSizeBytes int64     `json:"desired_size_bytes"`
	Generation       int64     `json:"generation"`
	Direction        string    `json:"direction"`
	Phase            string    `json:"phase"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// CommandRunner isolates filesystem utilities for unit tests.
type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

// MountVerifier prevents an absent disk/NFS mount from silently degrading to
// a directory on the root filesystem.
type MountVerifier interface {
	Verify(string) error
}

// Observer receives low-cardinality operation and pool measurements. Logical
// volume or service identifiers are deliberately excluded from metric labels.
type Observer interface {
	ObserveVolumeOperation(volumeType, operation, outcome string, duration time.Duration)
	ObserveVolumePool(volumeType string, reservedBytes, capacityBytes, availableBytes int64)
}

type procMountVerifier struct{}

func (procMountVerifier) Verify(path string) error {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("read mountinfo: %w", err)
	}
	defer f.Close()
	want := filepath.Clean(path)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 4 && decodeMountPath(fields[4]) == want {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan mountinfo: %w", err)
	}
	return fmt.Errorf("%s is not a mount point", want)
}

func decodeMountPath(value string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(value)
}

// Manager manages retained images. Shared execution deliberately remains
// disabled until its durable supervisor safety gate is satisfied.
type Manager struct {
	nodeID   string
	storage  config.StorageConfig
	runner   CommandRunner
	mounts   MountVerifier
	observer Observer
}

func NewManager(nodeID string, storage config.StorageConfig) *Manager {
	return &Manager{nodeID: nodeID, storage: storage, runner: execRunner{}, mounts: procMountVerifier{}}
}

func NewManagerWithObserver(nodeID string, storage config.StorageConfig, observer Observer) *Manager {
	manager := NewManager(nodeID, storage)
	manager.observer = observer
	return manager
}

func NewManagerWithDependencies(nodeID string, storage config.StorageConfig, runner CommandRunner, mounts MountVerifier) *Manager {
	return &Manager{nodeID: nodeID, storage: storage, runner: runner, mounts: mounts}
}

// Preflight validates every declaration and retained image without mutating it.
func (m *Manager) Preflight(ctx context.Context, svc config.ServiceConfig) error {
	_ = ctx
	if len(svc.Volumes) == 0 {
		return nil
	}
	if err := validateServiceVolumes(svc.Volumes); err != nil {
		return fmt.Errorf("service %s: %w", svc.Name, err)
	}

	desiredLocal := make(map[string]int64)
	for _, volume := range svc.Volumes {
		logicalID := svc.Name + "/" + volume.Name
		switch volume.Type {
		case config.VolumeTypeLocal:
			if m.storage.Local == nil {
				return fmt.Errorf("volume %s: storage.local is not configured", logicalID)
			}
			if volume.BoundNode == "" || volume.BoundNode != m.nodeID {
				return fmt.Errorf("volume %s: bound_node %q does not match node %q", logicalID, volume.BoundNode, m.nodeID)
			}
			if m.mounts != nil {
				if err := m.mounts.Verify(m.storage.Local.Path); err != nil {
					return fmt.Errorf("volume %s: verify local storage: %w", logicalID, err)
				}
			}
			desiredLocal[logicalID] = volume.SizeBytes
			if err := m.validateExisting(svc.Name, volume, m.storage.Local.Path); err != nil {
				return err
			}
			if err := m.preflightResize(ctx, svc.Name, volume, m.storage.Local.Path); err != nil {
				return err
			}
		case config.VolumeTypeShared:
			return fmt.Errorf("volume %s: %w", logicalID, ErrSharedUnsupported)
		default:
			return fmt.Errorf("volume %s: unsupported type %q", logicalID, volume.Type)
		}
	}
	if len(desiredLocal) > 0 {
		if err := m.checkCapacity(m.storage.Local, desiredLocal); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) preflightResize(ctx context.Context, service string, volume config.VolumeConfig, root string) error {
	dir, err := volumeDir(root, service, volume.Name)
	if err != nil {
		return err
	}
	var current manifest
	if err := readJSON(filepath.Join(dir, manifestFilename), &current); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if volume.SizeBytes >= current.AppliedSizeBytes {
		return nil
	}
	imagePath := filepath.Join(dir, imageFilename)
	return m.inspectShrinkMinimum(ctx, service, volume, imagePath)
}

func (m *Manager) inspectShrinkMinimum(ctx context.Context, service string, volume config.VolumeConfig, imagePath string) error {
	minimumOutput, err := m.runner.Run(ctx, "resize2fs", "-P", imagePath)
	if err != nil {
		return fmt.Errorf("inspect minimum filesystem size: %w", err)
	}
	blockOutput, err := m.runner.Run(ctx, "tune2fs", "-l", imagePath)
	if err != nil {
		return fmt.Errorf("inspect filesystem block size: %w", err)
	}
	minimumBlocks, err := lastInteger(string(minimumOutput))
	if err != nil {
		return fmt.Errorf("parse resize2fs minimum size: %w", err)
	}
	blockSize, err := valueAfterLabel(string(blockOutput), "Block size:")
	if err != nil {
		return fmt.Errorf("parse ext4 block size: %w", err)
	}
	minimumBytes := minimumBlocks * blockSize
	// Keep 5% headroom above resize2fs's estimate because it is not a
	// guarantee and can change after a final fsck.
	minimumWithHeadroom := minimumBytes + minimumBytes/20
	if volume.SizeBytes < minimumWithHeadroom {
		return fmt.Errorf("volume %s/%s: shrink target %d is below safe minimum %d", service, volume.Name, volume.SizeBytes, minimumWithHeadroom)
	}
	return nil
}

// Prepare creates/reuses/resizes all service images in deterministic order.
// Callers must invoke Preflight before stopping a running VM.
func (m *Manager) Prepare(ctx context.Context, svc config.ServiceConfig) ([]PreparedVolume, error) {
	if err := m.Preflight(ctx, svc); err != nil {
		if m.observer != nil {
			outcome := "failure"
			if strings.Contains(err.Error(), "quarantined") {
				outcome = "quarantined"
			}
			for _, volume := range svc.Volumes {
				m.observer.ObserveVolumeOperation(string(volume.Type), "preflight", outcome, 0)
			}
		}
		return nil, err
	}
	volumes := append([]config.VolumeConfig(nil), svc.Volumes...)
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
	prepared := make([]PreparedVolume, 0, len(volumes))
	for _, volume := range volumes {
		root := m.storage.Local.Path
		p, err := m.prepareOne(ctx, svc.Name, volume, root)
		if err != nil {
			return nil, err
		}
		prepared = append(prepared, p)
	}
	return prepared, nil
}

func (m *Manager) validateExisting(service string, volume config.VolumeConfig, root string) error {
	dir, err := volumeDir(root, service, volume.Name)
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(dir, manifestFilename)
	imagePath := filepath.Join(dir, imageFilename)
	var found manifest
	if err := readJSON(manifestPath, &found); err != nil {
		if os.IsNotExist(err) {
			if _, statErr := os.Stat(imagePath); statErr == nil {
				return fmt.Errorf("volume %s/%s: image exists without manifest; quarantined", service, volume.Name)
			}
			return nil
		}
		return fmt.Errorf("volume %s/%s: read manifest: %w", service, volume.Name, err)
	}
	if err := verifyManifest(found, service, volume, m.nodeID); err != nil {
		return err
	}
	info, err := os.Stat(imagePath)
	if err != nil {
		return fmt.Errorf("volume %s/%s: stat image: %w", service, volume.Name, err)
	}
	if info.Size() < found.AppliedSizeBytes {
		var tx resizeTransaction
		txErr := readJSON(filepath.Join(dir, transactionFilename), &tx)
		if txErr != nil || tx.Direction != "shrink" || tx.OldSizeBytes != found.AppliedSizeBytes || tx.DesiredSizeBytes != volume.SizeBytes || tx.Generation != volume.ResizeGeneration || info.Size() != tx.DesiredSizeBytes {
			return fmt.Errorf("volume %s/%s: image is smaller than applied filesystem without a recoverable shrink transaction; quarantined", service, volume.Name)
		}
	}
	return nil
}

func (m *Manager) prepareOne(ctx context.Context, service string, volume config.VolumeConfig, root string) (prepared PreparedVolume, retErr error) {
	started := time.Now()
	operation := "prepare"
	defer func() {
		if m.observer == nil {
			return
		}
		outcome := "success"
		if retErr != nil {
			outcome = "failure"
			if strings.Contains(retErr.Error(), "quarantined") {
				outcome = "quarantined"
			}
		}
		m.observer.ObserveVolumeOperation(string(volume.Type), operation, outcome, time.Since(started))
	}()
	dir, err := volumeDir(root, service, volume.Name)
	if err != nil {
		return PreparedVolume{}, err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return PreparedVolume{}, fmt.Errorf("create volume directory: %w", err)
	}
	lock, err := lockFile(filepath.Join(dir, "lifecycle.lock"))
	if err != nil {
		return PreparedVolume{}, err
	}
	defer unlockFile(lock)

	manifestPath := filepath.Join(dir, manifestFilename)
	imagePath := filepath.Join(dir, imageFilename)
	var current manifest
	err = readJSON(manifestPath, &current)
	if os.IsNotExist(err) {
		operation = "create"
		if err := createSparseImage(imagePath, volume.SizeBytes); err != nil {
			return PreparedVolume{}, err
		}
		if _, err := m.runner.Run(ctx, "mkfs.ext4", "-F", "-m", "0", imagePath); err != nil {
			return PreparedVolume{}, err
		}
		current = manifestFor(service, volume, m.nodeID)
		if err := writeJSONAtomic(manifestPath, current); err != nil {
			return PreparedVolume{}, err
		}
	} else if err != nil {
		return PreparedVolume{}, fmt.Errorf("read manifest: %w", err)
	} else {
		operation = "reuse"
		if err := verifyManifest(current, service, volume, m.nodeID); err != nil {
			return PreparedVolume{}, err
		}
		transactionPath := filepath.Join(dir, transactionFilename)
		var stale resizeTransaction
		transactionErr := readJSON(transactionPath, &stale)
		if transactionErr != nil && !os.IsNotExist(transactionErr) {
			return PreparedVolume{}, fmt.Errorf("volume %s/%s: unreadable resize transaction; quarantined: %w", service, volume.Name, transactionErr)
		}
		if transactionErr == nil && current.AppliedSizeBytes == volume.SizeBytes && current.ResizeGeneration == volume.ResizeGeneration {
			if stale.DesiredSizeBytes != current.AppliedSizeBytes || stale.Generation != current.ResizeGeneration {
				return PreparedVolume{}, fmt.Errorf("volume %s/%s: resize transaction conflicts with applied manifest; quarantined", service, volume.Name)
			}
			if err := os.Remove(transactionPath); err != nil && !os.IsNotExist(err) {
				return PreparedVolume{}, err
			}
			if err := syncDir(dir); err != nil {
				return PreparedVolume{}, err
			}
		}
		if current.AppliedSizeBytes != volume.SizeBytes || current.ResizeGeneration != volume.ResizeGeneration {
			operation = "grow"
			if volume.SizeBytes < current.AppliedSizeBytes {
				operation = "shrink"
			}
			if transactionErr == nil {
				direction := "grow"
				if volume.SizeBytes < current.AppliedSizeBytes {
					direction = "shrink"
				}
				if stale.OldSizeBytes != current.AppliedSizeBytes || stale.DesiredSizeBytes != volume.SizeBytes || stale.Generation != volume.ResizeGeneration || stale.Direction != direction {
					return PreparedVolume{}, fmt.Errorf("volume %s/%s: resize transaction does not match desired generation; quarantined", service, volume.Name)
				}
			}
			if err := m.resize(ctx, dir, imagePath, &current, volume); err != nil {
				return PreparedVolume{}, err
			}
		}
	}

	return PreparedVolume{
		LogicalID: service + "/" + volume.Name, PathOnHost: imagePath,
		MountPath: volume.MountPath, Type: volume.Type, SizeBytes: current.AppliedSizeBytes,
		ResizeGeneration: current.ResizeGeneration,
	}, nil
}

func (m *Manager) resize(ctx context.Context, dir, imagePath string, current *manifest, desired config.VolumeConfig) error {
	transactionPath := filepath.Join(dir, transactionFilename)
	direction := "grow"
	if desired.SizeBytes < current.AppliedSizeBytes {
		direction = "shrink"
	}
	tx := resizeTransaction{
		OldSizeBytes: current.AppliedSizeBytes, DesiredSizeBytes: desired.SizeBytes,
		Generation: desired.ResizeGeneration, Direction: direction, Phase: "checking", UpdatedAt: time.Now().UTC(),
	}
	if err := writeJSONAtomic(transactionPath, tx); err != nil {
		return fmt.Errorf("write resize transaction: %w", err)
	}
	if _, err := m.runner.Run(ctx, "e2fsck", "-f", "-y", imagePath); err != nil {
		return err
	}
	if direction == "shrink" {
		parts := strings.SplitN(current.LogicalID, "/", 2)
		service := parts[0]
		if err := m.inspectShrinkMinimum(ctx, service, desired, imagePath); err != nil {
			return err
		}
	}

	if direction == "grow" {
		tx.Phase = "file_extended"
		if err := writeJSONAtomic(transactionPath, tx); err != nil {
			return err
		}
		if err := os.Truncate(imagePath, desired.SizeBytes); err != nil {
			return fmt.Errorf("extend backing image: %w", err)
		}
		if _, err := m.runner.Run(ctx, "resize2fs", imagePath); err != nil {
			return err
		}
	} else {
		tx.Phase = "filesystem_shrinking"
		if err := writeJSONAtomic(transactionPath, tx); err != nil {
			return err
		}
		if _, err := m.runner.Run(ctx, "resize2fs", imagePath, strconv.FormatInt(desired.SizeBytes/1024, 10)+"K"); err != nil {
			return err
		}
		tx.Phase = "filesystem_shrunk"
		if err := writeJSONAtomic(transactionPath, tx); err != nil {
			return err
		}
		if err := os.Truncate(imagePath, desired.SizeBytes); err != nil {
			return fmt.Errorf("truncate backing image after filesystem shrink: %w", err)
		}
	}

	if _, err := m.runner.Run(ctx, "e2fsck", "-f", "-y", imagePath); err != nil {
		return err
	}
	current.AppliedSizeBytes = desired.SizeBytes
	current.ResizeGeneration = desired.ResizeGeneration
	current.UpdatedAt = time.Now().UTC()
	if err := writeJSONAtomic(filepath.Join(dir, manifestFilename), current); err != nil {
		return err
	}
	if err := os.Remove(transactionPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove resize transaction: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("sync volume directory: %w", err)
	}
	return nil
}

func (m *Manager) checkCapacity(pool *config.LocalStorageConfig, desired map[string]int64) error {
	retained, err := readRetained(pool.Path)
	if err != nil {
		return err
	}
	for id, size := range desired {
		retained[id] = size
	}
	var reserved int64
	for _, size := range retained {
		if size > 0 && reserved > (1<<63-1)-size {
			return fmt.Errorf("local volume reservations overflow")
		}
		reserved += size
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(pool.Path, &stat); err != nil {
		return fmt.Errorf("read local storage free space: %w", err)
	}
	available := int64(stat.Bavail) * int64(stat.Bsize)
	if m.observer != nil {
		m.observer.ObserveVolumePool(string(config.VolumeTypeLocal), reserved, pool.CapacityBytes, available)
	}
	if reserved > pool.CapacityBytes {
		return fmt.Errorf("local volume capacity exceeded: reserved %d bytes, configured %d bytes", reserved, pool.CapacityBytes)
	}
	var growth int64
	existing, err := readRetained(pool.Path)
	if err != nil {
		return err
	}
	for id, size := range desired {
		if size > existing[id] {
			growth += size - existing[id]
		}
	}
	if growth > available {
		return fmt.Errorf("local storage free space is insufficient: growth needs %d bytes, %d available", growth, available)
	}
	return nil
}

func readRetained(root string) (map[string]int64, error) {
	retained := make(map[string]int64)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || entry.Name() != manifestFilename {
			return nil
		}
		var m manifest
		if err := readJSON(path, &m); err != nil {
			return fmt.Errorf("read retained manifest %s: %w", path, err)
		}
		retained[m.LogicalID] = m.AppliedSizeBytes
		return nil
	})
	if os.IsNotExist(err) {
		return retained, nil
	}
	return retained, err
}

func validateServiceVolumes(volumes []config.VolumeConfig) error {
	if len(volumes) > config.MaxServiceVolumes {
		return fmt.Errorf("at most %d volumes are supported", config.MaxServiceVolumes)
	}
	names := make(map[string]struct{}, len(volumes))
	paths := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		if !componentPattern.MatchString(volume.Name) || strings.Contains(volume.Name, "..") {
			return fmt.Errorf("invalid volume name %q", volume.Name)
		}
		if _, exists := names[volume.Name]; exists {
			return fmt.Errorf("duplicate volume name %q", volume.Name)
		}
		names[volume.Name] = struct{}{}
		if volume.SizeBytes <= 0 {
			return fmt.Errorf("volume %s has non-positive size", volume.Name)
		}
		if !filepath.IsAbs(volume.MountPath) || filepath.Clean(volume.MountPath) != volume.MountPath || volume.MountPath == "/" {
			return fmt.Errorf("volume %s has invalid mount path %q", volume.Name, volume.MountPath)
		}
		for _, reserved := range []string{"/proc", "/sys", "/dev", "/run", "/tmp"} {
			if volume.MountPath == reserved || strings.HasPrefix(volume.MountPath, reserved+"/") {
				return fmt.Errorf("volume %s uses reserved mount path %q", volume.Name, volume.MountPath)
			}
		}
		for _, existing := range paths {
			if existing == volume.MountPath || strings.HasPrefix(existing, volume.MountPath+"/") || strings.HasPrefix(volume.MountPath, existing+"/") {
				return fmt.Errorf("volume %s has overlapping mount path %q", volume.Name, volume.MountPath)
			}
		}
		paths = append(paths, volume.MountPath)
	}
	return nil
}

func volumeDir(root, service, volume string) (string, error) {
	if !componentPattern.MatchString(service) || strings.Contains(service, "..") {
		return "", fmt.Errorf("invalid service name %q for volume path", service)
	}
	if !componentPattern.MatchString(volume) || strings.Contains(volume, "..") {
		return "", fmt.Errorf("invalid volume name %q", volume)
	}
	return filepath.Join(root, service, volume), nil
}

func verifyManifest(found manifest, service string, volume config.VolumeConfig, nodeID string) error {
	wantID := service + "/" + volume.Name
	if found.LogicalID != wantID || found.Type != volume.Type || found.Filesystem != "ext4" {
		return fmt.Errorf("volume %s: retained manifest identity mismatch", wantID)
	}
	if volume.Type == config.VolumeTypeLocal && (found.BoundNode != nodeID || volume.BoundNode != nodeID) {
		return fmt.Errorf("volume %s: retained local binding mismatch", wantID)
	}
	if volume.Type == config.VolumeTypeShared && found.SharedBackendID != volume.SharedBackendID {
		return fmt.Errorf("volume %s: retained shared backend mismatch", wantID)
	}
	return nil
}

func manifestFor(service string, volume config.VolumeConfig, nodeID string) manifest {
	m := manifest{
		LogicalID: service + "/" + volume.Name, Type: volume.Type, Filesystem: "ext4",
		AppliedSizeBytes: volume.SizeBytes, ResizeGeneration: volume.ResizeGeneration, UpdatedAt: time.Now().UTC(),
	}
	if volume.Type == config.VolumeTypeLocal {
		m.BoundNode = nodeID
	} else {
		m.SharedBackendID = volume.SharedBackendID
	}
	return m
}

func createSparseImage(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o640)
	if err != nil {
		return fmt.Errorf("create backing image: %w", err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		return fmt.Errorf("size backing image: %w", err)
	}
	return f.Sync()
}

func readJSON(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func lastInteger(value string) (int64, error) {
	fields := strings.Fields(value)
	for i := len(fields) - 1; i >= 0; i-- {
		cleaned := strings.Trim(fields[i], ".,;:")
		if n, err := strconv.ParseInt(cleaned, 10, 64); err == nil && n > 0 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("no positive integer in %q", strings.TrimSpace(value))
}

func valueAfterLabel(value, label string) (int64, error) {
	for _, line := range strings.Split(value, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), label) {
			continue
		}
		return lastInteger(line)
	}
	return 0, fmt.Errorf("label %q not found", label)
}
