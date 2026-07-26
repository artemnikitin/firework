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
	if manifest.HostBootID == "" || identity.HostBootID != manifest.HostBootID {
		return fmt.Errorf("host boot identity does not match")
	}
	if manifest.ProcessStart == 0 || identity.StartTicks != manifest.ProcessStart {
		return fmt.Errorf("process start time does not match")
	}
	if manifest.ExecutableDev == 0 || manifest.ExecutableIno == 0 ||
		identity.ExecutableDev != manifest.ExecutableDev || identity.ExecutableIno != manifest.ExecutableIno {
		return fmt.Errorf("process executable identity does not match")
	}
	if manifest.Executable == "" || identity.Executable != manifest.Executable {
		return fmt.Errorf("process executable path does not match")
	}
	if !manifest.Legacy && !hasExactArgument(identity.CommandLine, "--id", manifest.InstanceID) {
		return fmt.Errorf("process command line does not contain its instance ID")
	}
	if !hasExactArgument(identity.CommandLine, "--api-sock", manifest.SocketPath) ||
		!hasExactArgument(identity.CommandLine, "--config-file", manifest.ConfigPath) {
		return fmt.Errorf("process command line does not match manifest")
	}
	return nil
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
