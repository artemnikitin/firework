package vm

import (
	"bufio"
	"context"
	"fmt"
	"net"
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
			return fmt.Errorf("Firecracker vsock handshake exceeds %d bytes", maxVsockHandshakeBytes)
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
