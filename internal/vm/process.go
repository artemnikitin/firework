package vm

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var errProcessNotFound = errors.New("process not found")

// errIdentityNotRecorded marks a manifest that never captured the identity of
// the process it describes. It is distinct from a mismatch: an absent field
// proves nothing about the recorded process, so the VM stays quarantined
// instead of having its state cleaned while the process may still be alive.
var errIdentityNotRecorded = errors.New("process ownership identity was never recorded")

type processIdentity struct {
	PID           int
	HostBootID    string
	StartTicks    uint64
	Executable    string
	ExecutableDev uint64
	ExecutableIno uint64
	CommandLine   []string
}

type processInspector interface {
	Inspect(int) (processIdentity, error)
	FindByArguments(string, string) ([]processIdentity, error)
	SocketReady(string) error
}

type osProcessInspector struct{}

func (osProcessInspector) SocketReady(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s is not a Unix socket", path)
	}
	connection, err := net.DialTimeout("unix", path, 250*time.Millisecond)
	if err != nil {
		return fmt.Errorf("connect to Firecracker API socket: %w", err)
	}
	return connection.Close()
}

func validateOwnedProcess(inspector processInspector, manifest *instanceManifest) error {
	if manifest.PID <= 0 {
		return errProcessNotFound
	}
	identity, err := inspector.Inspect(manifest.PID)
	if err != nil {
		return err
	}
	if manifest.HostBootID == "" {
		return fmt.Errorf("host boot ID: %w", errIdentityNotRecorded)
	}
	// The boot ID is host-global and read fresh on every inspection, so a real
	// mismatch proves the recorded process did not survive: PIDs do not persist
	// across a reboot. Report it as a gone process rather than as ambiguity.
	if identity.HostBootID != manifest.HostBootID {
		return fmt.Errorf("host booted since the manifest was written: %w", errProcessNotFound)
	}
	if manifest.ProcessStart == 0 {
		return fmt.Errorf("process start time: %w", errIdentityNotRecorded)
	}
	if identity.StartTicks != manifest.ProcessStart {
		return fmt.Errorf("process start time does not match")
	}
	if manifest.ExecutableDev == 0 || manifest.ExecutableIno == 0 {
		return fmt.Errorf("process executable device and inode: %w", errIdentityNotRecorded)
	}
	if identity.ExecutableDev != manifest.ExecutableDev || identity.ExecutableIno != manifest.ExecutableIno {
		return fmt.Errorf("process executable identity does not match")
	}
	if manifest.Executable == "" {
		return fmt.Errorf("process executable path: %w", errIdentityNotRecorded)
	}
	if identity.Executable != manifest.Executable {
		return fmt.Errorf("process executable path does not match")
	}
	if !matchesOwnedArguments(identity, manifest) {
		if !manifest.Legacy && !hasExactArgument(identity.CommandLine, "--id", manifest.InstanceID) {
			return fmt.Errorf("process command line does not contain its instance ID")
		}
		return fmt.Errorf("process command line does not match manifest")
	}
	return nil
}

// matchesOwnedArguments reports whether a process is running the exact command
// line Firework launched for this instance. The instance ID is 128 random bits
// generated once per launch and passed to Firecracker as --id, so a match on it
// together with this instance's socket and config paths proves ownership on its
// own: a recycled PID cannot reproduce arguments it never received.
func matchesOwnedArguments(identity processIdentity, manifest *instanceManifest) bool {
	if !manifest.Legacy && !hasExactArgument(identity.CommandLine, "--id", manifest.InstanceID) {
		return false
	}
	return hasExactArgument(identity.CommandLine, "--api-sock", manifest.SocketPath) &&
		hasExactArgument(identity.CommandLine, "--config-file", manifest.ConfigPath)
}

func validateOwnedSocket(inspector processInspector, manifest *instanceManifest) error {
	vmDir, err := filepath.Abs(manifest.VMDir)
	if err != nil {
		return err
	}
	socketPath, err := filepath.Abs(manifest.SocketPath)
	if err != nil {
		return err
	}
	if socketPath != filepath.Join(vmDir, "firecracker.sock") || !strings.HasPrefix(socketPath, vmDir+string(os.PathSeparator)) {
		return fmt.Errorf("API socket is outside the VM state directory")
	}
	return inspector.SocketReady(socketPath)
}

func hasExactArgument(args []string, name, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name && args[i+1] == value {
			return true
		}
	}
	return false
}
