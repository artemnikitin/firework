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
	"sync"
	"syscall"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

const (
	manifestFilename    = "manifest.json"
	transactionFilename = "resize-transaction.json"
	imageFilename       = "volume.ext4"
	// creationMarkerFilename records that a first creation is in flight. It is
	// what separates "we crashed while making an empty image" from "an image
	// Firework did not create", which the manifest's absence alone cannot.
	creationMarkerFilename = "creating.json"
)

var (
	componentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	// ErrSharedUnsupported is returned until durable per-VM lock ownership and
	// backend partition fencing from issue #22 are available.
	ErrSharedUnsupported = errors.New("shared volumes require the durable per-VM supervisor and fencing validation")
)

// ErrShrinkRejected reports that a requested shrink is below the safe minimum
// for the filesystem's current contents. It is a *decision*, not a fault: the
// distinction is what lets the caller keep the workload running instead of
// treating a refusal like a failed operation.
//
// LogicalID is carried because two volumes sharing a size and generation are
// otherwise indistinguishable, and the clamp cannot tell which one it applies
// to.
type ErrShrinkRejected struct {
	LogicalID  string
	Requested  int64
	Minimum    int64
	Generation int64
}

func (e *ErrShrinkRejected) Error() string {
	return fmt.Sprintf("volume %s: shrink target %d is below safe minimum %d", e.LogicalID, e.Requested, e.Minimum)
}

// Rejection is a durable refusal of one volume's size request, as reported to
// status and consumed by the agent-side clamp.
type Rejection struct {
	LogicalID string
	// ResizeGeneration is the *requested* generation — the one that was
	// refused. It is what a reported rejection must carry so the control
	// plane's acknowledgement can match it to the record it has to converge.
	ResizeGeneration int64
	// AppliedGeneration is the generation actually applied to the filesystem.
	// Together with AppliedSizeBytes it is the *effective* configuration: what
	// the node is running, what Plan compares against, and what the clamp
	// substitutes. Keeping the two apart is what lets one rejection be both
	// terminal locally and matchable remotely.
	AppliedGeneration  int64
	RequestedSizeBytes int64
	AppliedSizeBytes   int64
	MinimumSizeBytes   int64
	At                 time.Time
}

// PreparedVolume is safe to attach to a stopped/new Firecracker process.
type PreparedVolume struct {
	LogicalID  string
	PathOnHost string
	MountPath  string
	Type       config.VolumeType
	// SizeBytes is the *effective* size: what the image actually is. For a
	// rejected shrink this is the applied size, not the refused request.
	SizeBytes int64
	// ResizeGeneration is always the generation actually applied to the
	// filesystem. Together with SizeBytes it is the effective configuration
	// the caller stores on the instance, which is what makes the next tick
	// compare equal instead of re-planning the same update.
	ResizeGeneration int64
	// Rejected marks a preparation that succeeded at a size other than the one
	// requested. It is not an error, so Prepare continues to the next volume
	// and one pass collects every rejection.
	Rejected bool
	// RequestedGeneration and RequestedSizeBytes describe the refused request.
	// They are reported rather than run.
	RequestedGeneration int64
	RequestedSizeBytes  int64
	MinimumSizeBytes    int64
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
	// The rejection is keyed to one (generation, size) request. Recording it
	// durably is what makes the refusal terminal without depending on the
	// control plane: the agent-side clamp reads it locally, so the stop/restart
	// loop is broken even if the acknowledgement never lands.
	//
	// ResizeGeneration deliberately continues to describe the last generation
	// actually applied to the filesystem. Advancing it here would erase the
	// evidence that this generation was refused.
	RejectedGeneration   int64     `json:"rejected_generation,omitempty"`
	RejectedSizeBytes    int64     `json:"rejected_size_bytes,omitempty"`
	RejectedMinimumBytes int64     `json:"rejected_minimum_bytes,omitempty"`
	RejectedAt           time.Time `json:"rejected_at,omitempty"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// rejectionFor builds the reported rejection for a manifest that carries one.
func (m manifest) rejectionFor(generation int64) Rejection {
	return Rejection{
		LogicalID: m.LogicalID, ResizeGeneration: generation, AppliedGeneration: m.ResizeGeneration,
		RequestedSizeBytes: m.RejectedSizeBytes, AppliedSizeBytes: m.AppliedSizeBytes,
		MinimumSizeBytes: m.RejectedMinimumBytes, At: m.RejectedAt,
	}
}

// matchesRejection reports whether a desired volume config is a request this
// manifest already refused.
//
// The generation must always match. A generation-only match is not enough:
// direct-Git node configs are hand-authored and carry their own
// resize_generation, so an operator correcting a refused shrink by editing
// size_bytes alone presents a different request under the same generation, and
// a generation-only match would clamp that forever.
//
// Two sizes carry the same refused request, and both must be recognized:
//
//   - the refused size itself, which is what a direct-Git config renders and
//     what the control plane renders until it has acknowledged the refusal;
//   - the applied size, which is what the control plane renders *after*
//     acknowledging it — the clamp there substitutes the effective size but
//     keeps the refused generation, because the acknowledgement has to be able
//     to match that generation to its record.
//
// Recognizing only the first leaves the second carrying the refused generation
// while the running instance carries the applied one. needsUpdate compares
// whole volume configs, so the service is stopped and restarted on every
// reconcile that reaches Plan — the loop this whole mechanism exists to end.
func (m manifest) matchesRejection(volume config.VolumeConfig) bool {
	if m.RejectedGeneration == 0 || m.RejectedGeneration != volume.ResizeGeneration {
		return false
	}
	return volume.SizeBytes == m.RejectedSizeBytes || volume.SizeBytes == m.AppliedSizeBytes
}

// refusesRequest reports whether the config in front of the agent is still
// asking for the size that was refused.
//
// This is deliberately narrower than matchesRejection, and the two must not be
// conflated. Clamping has to keep applying to both shapes for as long as the
// refused generation stands, or the generation diverges from the running
// instance and the service restarts on every reconcile. But a *report* of a
// standing refusal is only true while the refused size is actually being
// requested: once the config asks for the size already running — because a
// direct-Git operator withdrew the request, or the control plane acknowledged
// the refusal and now renders the effective size — nothing is being refused
// here any more. Reporting one anyway leaves the node degraded forever with no
// exit but a generation bump.
//
// After the control plane acknowledges, the two shapes become identical bytes
// and the agent genuinely cannot tell whether the operator still wants the
// refused size. Only the record knows, so that half of the visibility belongs
// to the control plane; see §7.3.2 of the hardening plan.
func (m manifest) refusesRequest(volume config.VolumeConfig) bool {
	return m.RejectedGeneration != 0 &&
		m.RejectedGeneration == volume.ResizeGeneration &&
		m.RejectedSizeBytes == volume.SizeBytes
}

func (m *manifest) clearRejection() {
	m.RejectedGeneration = 0
	m.RejectedSizeBytes = 0
	m.RejectedMinimumBytes = 0
	m.RejectedAt = time.Time{}
}

// creationMarker is written before the backing image and removed after the
// manifest. Its presence authorizes deleting an image that has no manifest, so
// its lifetime is deliberately bounded by the condition it describes: every
// path that reads a valid manifest removes a matching marker (see
// clearStaleCreationMarker). A marker that outlived a successful creation would
// otherwise authorize destroying populated data if the manifest were later lost.
type creationMarker struct {
	LogicalID        string    `json:"logical_id"`
	NodeID           string    `json:"node_id"`
	TargetSizeBytes  int64     `json:"target_size_bytes"`
	ResizeGeneration int64     `json:"resize_generation"`
	CreatedAt        time.Time `json:"created_at"`
}

type resizeTransaction struct {
	OldSizeBytes     int64     `json:"old_size_bytes"`
	DesiredSizeBytes int64     `json:"desired_size_bytes"`
	Generation       int64     `json:"generation"`
	Direction        string    `json:"direction"`
	Phase            string    `json:"phase"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// destructiveCommandTimeout bounds a filesystem-mutating command that has been
// detached from the caller's context. It has to accommodate mkfs, e2fsck, and
// resize2fs on a pool-sized image, so it is generous: the point is that the
// operation is not killed by an agent restart, not that it is killed promptly.
const destructiveCommandTimeout = 30 * time.Minute

// destructiveCommandGrace is how long a timed-out destructive command is given
// to handle SIGTERM before the process group is killed.
const destructiveCommandGrace = 10 * time.Second

// CommandRunner isolates filesystem utilities for unit tests.
//
// The split between Run and RunDestructive is the interface's whole point, and
// it lives here rather than in a name match inside the runner so a new
// filesystem-mutating command cannot inherit the cancellable path by omission.
type CommandRunner interface {
	// Run executes a read-only measurement command. It keeps the caller's
	// context and stays promptly cancellable.
	Run(context.Context, string, ...string) ([]byte, error)
	// RunDestructive executes a command that mutates a filesystem. It must not
	// be killed when the caller's context is cancelled: the agent's context is
	// cancelled on SIGINT/SIGTERM, and exec.CommandContext cancellation is
	// SIGKILL, so a systemd restart or node drain during a shrink would
	// SIGKILL resize2fs mid-operation.
	RunDestructive(context.Context, string, ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return runCommand(exec.CommandContext(ctx, name, args...), name, args)
}

func (execRunner) RunDestructive(ctx context.Context, name string, args ...string) ([]byte, error) {
	// WithoutCancel keeps the values (and therefore any tracing) from the
	// caller's context while detaching it from the SIGTERM cancellation chain.
	// The command then gets its own absolute deadline, and that deadline is a
	// SIGTERM with a grace period rather than an unconditional SIGKILL.
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), destructiveCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(detached, name, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = destructiveCommandGrace
	return runCommand(cmd, name, args)
}

func runCommand(cmd *exec.Cmd, name string, args []string) ([]byte, error) {
	output, err := cmd.CombinedOutput()
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

	// rejections is the synchronized per-volume refusal snapshot, updated
	// wherever a rejection is recorded — preflight or post-stop. Status reads
	// it directly rather than inferring state from a running instance's
	// prepared volumes, because a preflight rejection produces no fresh
	// preparation to read: it fails the update before anything is stopped, so
	// the instance still describes the *previous* preparation.
	rejectionMu sync.RWMutex
	rejections  map[string]Rejection
}

func NewManager(nodeID string, storage config.StorageConfig) *Manager {
	return &Manager{nodeID: nodeID, storage: storage, runner: execRunner{}, mounts: procMountVerifier{}, rejections: make(map[string]Rejection)}
}

func NewManagerWithObserver(nodeID string, storage config.StorageConfig, observer Observer) *Manager {
	manager := NewManager(nodeID, storage)
	manager.observer = observer
	return manager
}

func NewManagerWithDependencies(nodeID string, storage config.StorageConfig, runner CommandRunner, mounts MountVerifier) *Manager {
	return &Manager{nodeID: nodeID, storage: storage, runner: runner, mounts: mounts, rejections: make(map[string]Rejection)}
}

// Preflight validates every declaration and retained image without mutating it,
// apart from recording a refusal.
//
// It returns the rejections it found alongside its error rather than returning
// on the first one. A rejection is a decision and a failure is a fault: only
// the latter aborts the batch, so one pass collects every refusal and the
// caller never has to retry per volume.
func (m *Manager) Preflight(ctx context.Context, svc config.ServiceConfig) ([]Rejection, error) {
	if len(svc.Volumes) == 0 {
		return nil, nil
	}
	if err := validateServiceVolumes(svc.Volumes); err != nil {
		return nil, fmt.Errorf("service %s: %w", svc.Name, err)
	}

	var rejections []Rejection
	desiredLocal := make(map[string]int64)
	for _, volume := range svc.Volumes {
		logicalID := svc.Name + "/" + volume.Name
		switch volume.Type {
		case config.VolumeTypeLocal:
			if m.storage.Local == nil {
				return rejections, fmt.Errorf("volume %s: storage.local is not configured", logicalID)
			}
			if volume.BoundNode == "" || volume.BoundNode != m.nodeID {
				return rejections, fmt.Errorf("volume %s: bound_node %q does not match node %q", logicalID, volume.BoundNode, m.nodeID)
			}
			if m.mounts != nil {
				if err := m.mounts.Verify(m.storage.Local.Path); err != nil {
					return rejections, fmt.Errorf("volume %s: verify local storage: %w", logicalID, err)
				}
			}
			desiredLocal[logicalID] = volume.SizeBytes
			if err := m.validateExisting(svc.Name, volume, m.storage.Local.Path); err != nil {
				return rejections, err
			}
			rejection, err := m.preflightResize(ctx, svc.Name, volume, m.storage.Local.Path)
			if err != nil {
				return rejections, err
			}
			if rejection != nil {
				// The effective size is what capacity should be checked
				// against; charging the refused request would reject a
				// configuration the node is already running.
				desiredLocal[logicalID] = rejection.AppliedSizeBytes
				rejections = append(rejections, *rejection)
			}
		case config.VolumeTypeShared:
			return rejections, fmt.Errorf("volume %s: %w", logicalID, ErrSharedUnsupported)
		default:
			return rejections, fmt.Errorf("volume %s: unsupported type %q", logicalID, volume.Type)
		}
	}
	if len(desiredLocal) > 0 {
		if err := m.checkCapacity(m.storage.Local, desiredLocal); err != nil {
			return rejections, err
		}
	}
	m.refreshRejections(svc)
	return rejections, nil
}

// preflightResize measures a requested shrink before anything is stopped, and
// records a refusal durably so the refusal is terminal rather than re-measured
// on every tick forever.
//
// The measurement is advisory: a live resize2fs -P errs in both directions,
// because guest deletions whose bitmap updates are still in the page cache read
// too large and guest writes not yet flushed read too small. Terminality is
// still the right call, because the costs are asymmetric — a false refusal
// costs the operator one re-request, which mints a new generation and
// re-measures from scratch, while a non-terminal preflight costs an unbounded
// measurement loop on every tick.
//
// The whole read → measure → write sequence runs under the volume's lifecycle
// lock, the same lock prepareOne takes. Preflight used to be a pure reader; now
// that it writes the manifest it can interleave with a concurrent Prepare, and
// a measurement taken under one lock and written under another is the same lost
// update with extra steps.
func (m *Manager) preflightResize(ctx context.Context, service string, volume config.VolumeConfig, root string) (*Rejection, error) {
	dir, err := volumeDir(root, service, volume.Name)
	if err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(dir, manifestFilename)
	if _, statErr := os.Stat(manifestPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, nil
		}
		return nil, statErr
	}
	lock, err := lockFile(filepath.Join(dir, "lifecycle.lock"))
	if err != nil {
		return nil, err
	}
	defer unlockFile(lock)

	var current manifest
	if err := readJSON(manifestPath, &current); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if current.refusesRequest(volume) {
		// Already refused, for exactly this request. Re-measuring would be the
		// unbounded loop this record exists to stop. A config that merely
		// carries the refused *generation* at the effective size is not a
		// refusal — nothing is being asked for that was denied — and falls
		// through to the size comparison below, which finds nothing to do.
		rejection := current.rejectionFor(volume.ResizeGeneration)
		return &rejection, nil
	}
	if volume.SizeBytes >= current.AppliedSizeBytes {
		return nil, nil
	}
	err = m.inspectShrinkMinimum(ctx, service, volume, filepath.Join(dir, imageFilename))
	var rejected *ErrShrinkRejected
	if errors.As(err, &rejected) {
		// Nothing has been stopped and no resize has begun, so there is no
		// transaction to clean up here — only the manifest write applies.
		current.RejectedGeneration = volume.ResizeGeneration
		current.RejectedSizeBytes = volume.SizeBytes
		current.RejectedMinimumBytes = rejected.Minimum
		current.RejectedAt = time.Now().UTC()
		current.UpdatedAt = current.RejectedAt
		if writeErr := writeJSONAtomic(manifestPath, current); writeErr != nil {
			return nil, writeErr
		}
		if syncErr := syncDir(dir); syncErr != nil {
			return nil, syncErr
		}
		rejection := current.rejectionFor(volume.ResizeGeneration)
		return &rejection, nil
	}
	return nil, err
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
		return &ErrShrinkRejected{
			LogicalID: service + "/" + volume.Name, Requested: volume.SizeBytes,
			Minimum: minimumWithHeadroom, Generation: volume.ResizeGeneration,
		}
	}
	return nil
}

// Prepare creates/reuses/resizes all service images in deterministic order.
// Callers must invoke Preflight before stopping a running VM.
func (m *Manager) Prepare(ctx context.Context, svc config.ServiceConfig) ([]PreparedVolume, error) {
	if _, err := m.Preflight(ctx, svc); err != nil {
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
			// A genuine failure still aborts the batch: a rejection is a
			// decision, a failure is a fault, and only the latter means the
			// remaining volumes cannot be trusted.
			return nil, err
		}
		prepared = append(prepared, p)
	}
	m.refreshRejections(svc)
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
				// An image with no manifest is either a creation this node
				// crashed partway through — recoverable, because the image is
				// empty and nothing is protected by failing closed — or an
				// image Firework did not create, which is exactly what
				// fail-closed exists for. Only a matching marker tells them
				// apart, so an absent, unreadable, or mismatched marker still
				// quarantines.
				if matchingCreationMarker(dir, service, volume, m.nodeID) {
					return nil
				}
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
	var rejection *Rejection
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
		if err := m.clearInterruptedCreation(dir, imagePath, service, volume); err != nil {
			return PreparedVolume{}, err
		}
		if err := writeCreationMarker(dir, service, volume, m.nodeID); err != nil {
			return PreparedVolume{}, err
		}
		if err := createSparseImage(imagePath, volume.SizeBytes); err != nil {
			return PreparedVolume{}, err
		}
		if _, err := m.runner.RunDestructive(ctx, "mkfs.ext4", "-F", "-m", "0", imagePath); err != nil {
			return PreparedVolume{}, err
		}
		current = manifestFor(service, volume, m.nodeID)
		if err := writeJSONAtomic(manifestPath, current); err != nil {
			return PreparedVolume{}, err
		}
		if err := removeCreationMarker(dir); err != nil {
			return PreparedVolume{}, err
		}
	} else if err != nil {
		return PreparedVolume{}, fmt.Errorf("read manifest: %w", err)
	} else {
		operation = "reuse"
		if err := verifyManifest(current, service, volume, m.nodeID); err != nil {
			return PreparedVolume{}, err
		}
		// The manifest is valid, so any surviving marker describes a condition
		// that has already ended — a crash between the manifest write and the
		// marker removal. Clearing it here is what stops it from authorizing a
		// delete later, if the manifest is ever lost.
		if err := removeCreationMarker(dir); err != nil {
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
		// The clamped configuration needs its own branch, evaluated before the
		// resize condition. The clamp substitutes the applied size but keeps
		// the *requested* generation — which is what the acknowledgement has
		// to match — so it still satisfies the generation arm below and would
		// re-enter resize forever. A short-circuit keyed on the requested
		// (generation, size) pair cannot help either: the manifest records the
		// rejection at the refused size while the clamped input presents the
		// applied one, so the two never match by construction.
		//
		// The applied-size equality is what keeps a genuinely new request from
		// being clamped: a raw config arriving at a matching generation but a
		// non-applied size falls through to resize and re-measures.
		if current.RejectedGeneration != 0 && current.RejectedGeneration == volume.ResizeGeneration &&
			volume.SizeBytes == current.AppliedSizeBytes {
			operation = "rejected"
			rejection = &Rejection{
				LogicalID: current.LogicalID, ResizeGeneration: volume.ResizeGeneration,
				RequestedSizeBytes: current.RejectedSizeBytes, AppliedSizeBytes: current.AppliedSizeBytes,
				MinimumSizeBytes: current.RejectedMinimumBytes, At: current.RejectedAt,
			}
		} else if current.AppliedSizeBytes != volume.SizeBytes || current.ResizeGeneration != volume.ResizeGeneration {
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
			resized, err := m.resize(ctx, dir, imagePath, &current, volume)
			if err != nil {
				return PreparedVolume{}, err
			}
			rejection = resized
		}
	}

	prepared = PreparedVolume{
		LogicalID: service + "/" + volume.Name, PathOnHost: imagePath,
		MountPath: volume.MountPath, Type: volume.Type, SizeBytes: current.AppliedSizeBytes,
		ResizeGeneration: current.ResizeGeneration,
	}
	if rejection != nil {
		// A rejection is a non-fatal outcome of a *successful* preparation, so
		// no error is returned and Prepare continues to the next volume. One
		// pass therefore collects every rejection, without a retry budget that
		// a second rejected volume would exhaust.
		prepared.Rejected = true
		prepared.RequestedSizeBytes = rejection.RequestedSizeBytes
		prepared.RequestedGeneration = rejection.ResizeGeneration
		prepared.MinimumSizeBytes = rejection.MinimumSizeBytes
	}
	return prepared, nil
}

// resize applies a size change, or refuses one.
//
// A refusal returns a Rejection and no error, because by this point
// deleteService has already stopped the VM: treating the refusal as a failure
// would leave the workload down, which is precisely what the caller must be
// able to avoid.
func (m *Manager) resize(ctx context.Context, dir, imagePath string, current *manifest, desired config.VolumeConfig) (*Rejection, error) {
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
		return nil, fmt.Errorf("write resize transaction: %w", err)
	}
	if _, err := m.runner.RunDestructive(ctx, "e2fsck", "-f", "-y", imagePath); err != nil {
		return nil, err
	}
	if direction == "shrink" {
		parts := strings.SplitN(current.LogicalID, "/", 2)
		service := parts[0]
		err := m.inspectShrinkMinimum(ctx, service, desired, imagePath)
		var rejected *ErrShrinkRejected
		if errors.As(err, &rejected) {
			rejection, cleanupErr := m.recordShrinkRejection(dir, current, desired, rejected)
			if cleanupErr != nil {
				return nil, cleanupErr
			}
			return rejection, nil
		}
		if err != nil {
			return nil, err
		}
	}

	if direction == "grow" {
		tx.Phase = "file_extended"
		if err := writeJSONAtomic(transactionPath, tx); err != nil {
			return nil, err
		}
		if err := os.Truncate(imagePath, desired.SizeBytes); err != nil {
			return nil, fmt.Errorf("extend backing image: %w", err)
		}
		if _, err := m.runner.RunDestructive(ctx, "resize2fs", imagePath); err != nil {
			return nil, err
		}
	} else {
		tx.Phase = "filesystem_shrinking"
		if err := writeJSONAtomic(transactionPath, tx); err != nil {
			return nil, err
		}
		if _, err := m.runner.RunDestructive(ctx, "resize2fs", imagePath, strconv.FormatInt(desired.SizeBytes/1024, 10)+"K"); err != nil {
			return nil, err
		}
		tx.Phase = "filesystem_shrunk"
		if err := writeJSONAtomic(transactionPath, tx); err != nil {
			return nil, err
		}
		if err := os.Truncate(imagePath, desired.SizeBytes); err != nil {
			return nil, fmt.Errorf("truncate backing image after filesystem shrink: %w", err)
		}
	}

	if _, err := m.runner.RunDestructive(ctx, "e2fsck", "-f", "-y", imagePath); err != nil {
		return nil, err
	}
	current.AppliedSizeBytes = desired.SizeBytes
	current.ResizeGeneration = desired.ResizeGeneration
	// A size actually applied supersedes any earlier refusal.
	current.clearRejection()
	current.UpdatedAt = time.Now().UTC()
	if err := writeJSONAtomic(filepath.Join(dir, manifestFilename), current); err != nil {
		return nil, err
	}
	if err := os.Remove(transactionPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove resize transaction: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return nil, fmt.Errorf("sync volume directory: %w", err)
	}
	return nil, nil
}

// recordShrinkRejection cleans up the checking transaction and records the
// refusal, in that order.
//
// The order is fixed and crash-consistent. Writing the rejection first risks
// "rejection recorded plus stale checking transaction", which is exactly the
// state that quarantines the corrected retry: prepareOne compares the stale
// transaction's generation against the new one and refuses to proceed. Crashing
// after the removal instead loses only the rejection record — the request is
// re-measured, refused again, and recorded on the next pass, which is
// idempotent and self-healing.
//
// Removing the transaction is safe here not because nothing has touched the
// image (e2fsck ran, and may have replayed a journal) but because the checking
// phase completes without changing the filesystem's *geometry*. No
// partially-applied resize exists for the transaction to describe. Every later
// phase has moved geometry, and its transaction must survive for recovery.
func (m *Manager) recordShrinkRejection(dir string, current *manifest, desired config.VolumeConfig, rejected *ErrShrinkRejected) (*Rejection, error) {
	if err := os.Remove(filepath.Join(dir, transactionFilename)); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove checking transaction after rejection: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return nil, err
	}
	current.RejectedGeneration = desired.ResizeGeneration
	current.RejectedSizeBytes = desired.SizeBytes
	current.RejectedMinimumBytes = rejected.Minimum
	current.RejectedAt = time.Now().UTC()
	current.UpdatedAt = current.RejectedAt
	if err := writeJSONAtomic(filepath.Join(dir, manifestFilename), current); err != nil {
		return nil, err
	}
	if err := syncDir(dir); err != nil {
		return nil, err
	}
	rejection := current.rejectionFor(desired.ResizeGeneration)
	return &rejection, nil
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
	// Pool observation deliberately does not happen here. checkCapacity runs
	// only when a service declares local volumes, so reporting from it made
	// the gauges vanish on a node holding retained-but-unplaced volumes —
	// exactly the state an operator needs them for. ObservePool now publishes
	// them once per tick from the agent loop, independent of desired state.
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

// rebuildRejections replaces the whole refusal snapshot from the durable
// manifests of the currently desired services. It is the complete
// reconciliation; refreshRejections keeps one service fresh within a tick.
func (m *Manager) rebuildRejections(services []config.ServiceConfig) {
	if m == nil || m.storage.Local == nil {
		return
	}
	rebuilt := make(map[string]Rejection)
	for _, svc := range services {
		for _, declared := range svc.Volumes {
			if declared.Type != config.VolumeTypeLocal {
				continue
			}
			// Evaluated against the *raw* desired config, before the clamp
			// below rewrites it. After clamping, the size is the applied one
			// and the request that was refused is no longer visible.
			if rejection, refusing, _ := m.storedRejection(svc.Name, declared); refusing {
				rebuilt[svc.Name+"/"+declared.Name] = rejection
			}
		}
	}
	m.rejectionMu.Lock()
	defer m.rejectionMu.Unlock()
	m.rejections = rebuilt
}

// storedRejection reads one volume's durable refusal and reports whether it
// still describes the request being made. hasRecord distinguishes "no refusal
// recorded at all" from "recorded, but no longer being requested", which the
// callers need in order to prune correctly.
func (m *Manager) storedRejection(service string, declared config.VolumeConfig) (rejection Rejection, refusing, hasRecord bool) {
	dir, err := volumeDir(m.storage.Local.Path, service, declared.Name)
	if err != nil {
		return Rejection{}, false, false
	}
	var current manifest
	if err := readJSON(filepath.Join(dir, manifestFilename), &current); err != nil || current.RejectedGeneration == 0 {
		return Rejection{}, false, false
	}
	return current.rejectionFor(current.RejectedGeneration), current.refusesRequest(declared), true
}

// refreshRejections updates the refusal snapshot for one service from the
// durable manifests.
//
// It reads the manifests rather than only the outcomes of this pass because
// the clamp erases the evidence from the desired configuration: once the
// effective size and generation are substituted, neither the preflight nor
// prepareOne has anything left to refuse, and a snapshot built from outcomes
// alone would clear itself on the very tick that proves the rejection is
// working. The manifest is where the rejection actually lives, and a resize
// that succeeds clears it there.
func (m *Manager) refreshRejections(svc config.ServiceConfig) {
	if m == nil || m.storage.Local == nil {
		return
	}
	type outcome struct {
		rejection Rejection
		refusing  bool
		hasRecord bool
	}
	current := make(map[string]outcome, len(svc.Volumes))
	for _, declared := range svc.Volumes {
		if declared.Type != config.VolumeTypeLocal {
			continue
		}
		rejection, refusing, hasRecord := m.storedRejection(svc.Name, declared)
		current[svc.Name+"/"+declared.Name] = outcome{rejection, refusing, hasRecord}
	}
	m.rejectionMu.Lock()
	defer m.rejectionMu.Unlock()
	for logicalID, got := range current {
		switch {
		case !got.hasRecord:
			// The refusal is gone from the manifest — a resize applied — so
			// it stops being reported immediately rather than a tick later.
			delete(m.rejections, logicalID)
		case got.refusing:
			m.rejections[logicalID] = got.rejection
		}
		// Otherwise leave the entry alone. By this point the config has
		// already been normalized, so the refused size is no longer visible in
		// it and this function cannot tell a withdrawn request from a standing
		// one. rebuildRejections makes that call once per tick against the raw
		// config; this pass only ever adds a refusal it has just discovered.
	}
}

// Rejections returns the current refusal snapshot, keyed by logical ID.
func (m *Manager) Rejections() map[string]Rejection {
	if m == nil {
		return nil
	}
	m.rejectionMu.RLock()
	defer m.rejectionMu.RUnlock()
	out := make(map[string]Rejection, len(m.rejections))
	for id, rejection := range m.rejections {
		out[id] = rejection
	}
	return out
}

// SeedRejectionsForTest installs a refusal snapshot directly. Production only
// ever populates it from the durable manifests, through refreshRejections.
func (m *Manager) SeedRejectionsForTest(rejections map[string]Rejection) {
	if m == nil {
		return
	}
	m.rejectionMu.Lock()
	defer m.rejectionMu.Unlock()
	m.rejections = make(map[string]Rejection, len(rejections))
	for id, rejection := range rejections {
		m.rejections[id] = rejection
	}
}

// NormalizeVolumes rewrites a desired node configuration so every volume whose
// exact request has already been refused renders its effective size instead.
//
// This closes the window before the control plane's own clamp catches up:
// acknowledging a rejection and re-rendering takes at least one control-plane
// cycle, and during that window the node config still carries the refused size.
// Running the clamp here means needsUpdate, Prepare, and writeVMConfig all see
// one configuration, and the instance stores that same configuration — so it
// compares equal on the very next tick rather than one convergence cycle later.
//
// It reads the manifests rather than the in-memory snapshot so it is correct on
// the first tick after an agent restart, when nothing has been measured yet.
//
// The match is on both generation and requested size (see manifest.matchesRejection),
// which is what lets a hand-authored direct-Git config correct a refused shrink
// by editing size_bytes alone.
func (m *Manager) NormalizeVolumes(services []config.ServiceConfig) {
	if m == nil || m.storage.Local == nil {
		return
	}
	// Reconcile the refusal snapshot against the durable manifests for the
	// whole desired set, not just the volumes some later Prepare happens to
	// touch. Two things depend on it being complete:
	//
	//   - after an agent restart the snapshot is empty, and if normalization
	//     clamps the config so that no action is planned, nothing else would
	//     ever repopulate it — the node would report every size applied while
	//     running an effective one;
	//   - a volume that is no longer declared has to drop out, or its stale
	//     entry keeps VolumeSizesApplied false forever.
	m.rebuildRejections(services)
	for si := range services {
		for vi := range services[si].Volumes {
			volume := &services[si].Volumes[vi]
			if volume.Type != config.VolumeTypeLocal {
				continue
			}
			dir, err := volumeDir(m.storage.Local.Path, services[si].Name, volume.Name)
			if err != nil {
				continue
			}
			var current manifest
			if err := readJSON(filepath.Join(dir, manifestFilename), &current); err != nil {
				continue
			}
			if !current.matchesRejection(*volume) {
				continue
			}
			// Substitute the whole effective configuration — size *and*
			// generation. Clamping only the size leaves the generation
			// differing forever, and needsUpdate compares whole volume
			// configs: the service would be re-planned on every tick, which is
			// exactly the loop this is here to end. The refused request is not
			// lost: it is reported from the rejection snapshot, which is where
			// the acknowledgement reads it.
			volume.SizeBytes = current.AppliedSizeBytes
			volume.ResizeGeneration = current.ResizeGeneration
		}
	}
}

// ObservePool publishes the local pool gauges from retained state alone. It is
// called once per agent tick, independent of any desired configuration, so a
// node with retained but unplaced volumes — or with no desired local volumes at
// all — keeps reporting reserved, capacity, and available bytes.
//
// It never fails a tick: a pool that is not configured or not readable is
// simply not reported, because a metrics side effect must not be able to block
// reconciliation.
func (m *Manager) ObservePool() {
	if m == nil || m.observer == nil || m.storage.Local == nil {
		return
	}
	pool := m.storage.Local
	retained, err := readRetained(pool.Path)
	if err != nil {
		return
	}
	var reserved int64
	for _, size := range retained {
		if size > 0 && reserved > (1<<63-1)-size {
			return
		}
		reserved += size
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(pool.Path, &stat); err != nil {
		return
	}
	m.observer.ObserveVolumePool(string(config.VolumeTypeLocal), reserved, pool.CapacityBytes, int64(stat.Bavail)*int64(stat.Bsize))
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

// clearInterruptedCreation removes an image left behind by a crashed first
// creation. It refuses — leaving the volume quarantined — for any image whose
// creation this node cannot prove it started.
func (m *Manager) clearInterruptedCreation(dir, imagePath, service string, volume config.VolumeConfig) error {
	if _, err := os.Stat(imagePath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("volume %s/%s: stat image: %w", service, volume.Name, err)
	}
	if !matchingCreationMarker(dir, service, volume, m.nodeID) {
		return fmt.Errorf("volume %s/%s: image exists without manifest; quarantined", service, volume.Name)
	}
	if err := os.Remove(imagePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("volume %s/%s: remove interrupted image: %w", service, volume.Name, err)
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	return nil
}

func matchingCreationMarker(dir, service string, volume config.VolumeConfig, nodeID string) bool {
	var marker creationMarker
	if err := readJSON(filepath.Join(dir, creationMarkerFilename), &marker); err != nil {
		return false
	}
	return marker.LogicalID == service+"/"+volume.Name && marker.NodeID == nodeID
}

func writeCreationMarker(dir, service string, volume config.VolumeConfig, nodeID string) error {
	marker := creationMarker{
		LogicalID: service + "/" + volume.Name, NodeID: nodeID,
		TargetSizeBytes: volume.SizeBytes, ResizeGeneration: volume.ResizeGeneration,
		CreatedAt: time.Now().UTC(),
	}
	if err := writeJSONAtomic(filepath.Join(dir, creationMarkerFilename), marker); err != nil {
		return fmt.Errorf("write creation marker: %w", err)
	}
	return nil
}

// removeCreationMarker is idempotent: the common case is that there is no
// marker to remove, and it must stay cheap enough to call on every reuse.
func removeCreationMarker(dir string) error {
	err := os.Remove(filepath.Join(dir, creationMarkerFilename))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove creation marker: %w", err)
	}
	return syncDir(dir)
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
