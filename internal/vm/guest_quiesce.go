package vm

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/artemnikitin/firework/internal/guestlifecycle"
)

const (
	maxVsockHandshakeBytes   = 128
	vsockTransportTimeMargin = 15 * time.Second
)

type guestQuiescer interface {
	Quiesce(context.Context, string, time.Duration) (guestlifecycle.Response, error)
}

type firecrackerGuestQuiescer struct{}

// Quiesce connects to Firecracker's host-side vsock Unix socket, completes its
// text handshake, and exchanges one bounded lifecycle frame with the guest.
func (firecrackerGuestQuiescer) Quiesce(ctx context.Context, socketPath string, appTimeout time.Duration) (guestlifecycle.Response, error) {
	var empty guestlifecycle.Response
	if strings.TrimSpace(socketPath) == "" {
		return empty, fmt.Errorf("guest lifecycle socket is not configured")
	}
	if appTimeout < time.Millisecond || appTimeout > time.Duration(guestlifecycle.MaxTimeoutMS)*time.Millisecond {
		return empty, fmt.Errorf("guest application timeout must be between 1ms and %s", time.Duration(guestlifecycle.MaxTimeoutMS)*time.Millisecond)
	}
	request := guestlifecycle.Request{
		Version: guestlifecycle.Version, Operation: guestlifecycle.QuiesceOperation,
		TimeoutMS: uint32(appTimeout / time.Millisecond),
	}
	if err := request.Validate(); err != nil {
		return empty, err
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, appTimeout+vsockTransportTimeMargin)
		defer cancel()
	}

	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return empty, fmt.Errorf("connect to guest lifecycle socket: %w", err)
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return empty, fmt.Errorf("set guest lifecycle deadline: %w", err)
		}
	}

	if _, err := fmt.Fprintf(connection, "CONNECT %d\n", guestlifecycle.GuestPort); err != nil {
		return empty, fmt.Errorf("write Firecracker vsock connect request: %w", err)
	}
	reader := bufio.NewReaderSize(connection, maxVsockHandshakeBytes)
	if err := readVsockHandshake(reader); err != nil {
		return empty, err
	}
	if err := guestlifecycle.WriteRequest(connection, request); err != nil {
		return empty, err
	}
	response, err := guestlifecycle.ReadResponse(reader)
	if err != nil {
		return empty, fmt.Errorf("read guest lifecycle response: %w", err)
	}
	return response, nil
}

func readVsockHandshake(reader *bufio.Reader) error {
	line, err := reader.ReadSlice('\n')
	if err != nil {
		if err == bufio.ErrBufferFull {
			return fmt.Errorf("firecracker vsock handshake exceeds %d bytes", maxVsockHandshakeBytes)
		}
		return fmt.Errorf("read Firecracker vsock handshake: %w", err)
	}
	if len(line) > maxVsockHandshakeBytes || len(line) < 5 || !strings.HasPrefix(string(line), "OK ") || line[len(line)-1] != '\n' {
		return fmt.Errorf("invalid Firecracker vsock handshake %q", line)
	}
	portText := string(line[3 : len(line)-1])
	port, err := strconv.ParseUint(portText, 10, 32)
	if err != nil || port == 0 || strconv.FormatUint(port, 10) != portText {
		return fmt.Errorf("invalid Firecracker vsock host port %q", portText)
	}
	return nil
}

func guestVsockPath(manifest *instanceManifest) (string, error) {
	if manifest == nil || manifest.GuestVsockPath == "" {
		return "", fmt.Errorf("guest lifecycle socket is not configured by this VM image")
	}
	vmDir, err := filepath.Abs(manifest.VMDir)
	if err != nil {
		return "", fmt.Errorf("resolve VM state directory: %w", err)
	}
	path, err := filepath.Abs(manifest.GuestVsockPath)
	if err != nil {
		return "", fmt.Errorf("resolve guest lifecycle socket path: %w", err)
	}
	if path != filepath.Join(vmDir, "guest.vsock") {
		return "", fmt.Errorf("guest lifecycle socket path does not match the VM state directory")
	}
	return path, nil
}

func validateGuestQuiesceResponse(response guestlifecycle.Response, manifest *instanceManifest) error {
	if err := response.Validate(); err != nil {
		return fmt.Errorf("invalid guest quiesce response: %w", err)
	}
	if response.ErrorCode != "" {
		return fmt.Errorf("guest quiesce failed: %s", response.ErrorCode)
	}
	if !response.AppExited {
		return fmt.Errorf("guest quiesce did not confirm application exit")
	}
	if !response.SyncDone {
		return fmt.Errorf("guest quiesce did not confirm filesystem sync")
	}
	expected := make(map[string]string, len(manifest.Volumes))
	for _, prepared := range manifest.Volumes {
		name := filepath.Base(prepared.LogicalID)
		if _, duplicate := expected[name]; duplicate {
			return fmt.Errorf("ownership manifest repeats persistent volume %q", name)
		}
		expected[name] = prepared.MountPath
	}
	for _, result := range response.Mounts {
		path, ok := expected[result.Name]
		if !ok {
			return fmt.Errorf("guest quiesce reported unexpected volume %q", result.Name)
		}
		if path != result.Path {
			return fmt.Errorf("guest quiesce reported volume %q at unexpected path %q", result.Name, result.Path)
		}
		delete(expected, result.Name)
		if !result.Unmounted {
			return fmt.Errorf("guest did not unmount volume %q at %s: %s", result.Name, result.Path, result.Error)
		}
	}
	if len(expected) > 0 {
		for name, path := range expected {
			return fmt.Errorf("guest quiesce did not report volume %q at %s", name, path)
		}
	}
	return nil
}
