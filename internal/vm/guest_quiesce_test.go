package vm

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/guestlifecycle"
)

func TestFirecrackerGuestQuiescerUsesVsockHandshakeAndLifecycleFrame(t *testing.T) {
	listener, path := listenGuestTestSocket(t)
	serverResult := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			serverResult <- err
			return
		}
		defer connection.Close()
		reader := bufio.NewReader(connection)
		command, err := reader.ReadString('\n')
		if err != nil {
			serverResult <- err
			return
		}
		if command != "CONNECT 4050\n" {
			serverResult <- fmt.Errorf("connect command = %q", command)
			return
		}
		// Firecracker assigns a host-side port. It is expected to differ from
		// the guest destination port in CONNECT.
		if _, err := connection.Write([]byte("OK 1073741824\n")); err != nil {
			serverResult <- err
			return
		}
		request, err := guestlifecycle.ReadRequest(reader)
		if err != nil {
			serverResult <- err
			return
		}
		if request.Version != guestlifecycle.Version || request.Operation != guestlifecycle.QuiesceOperation || request.TimeoutMS != 12000 {
			serverResult <- fmt.Errorf("unexpected lifecycle request: %#v", request)
			return
		}
		serverResult <- guestlifecycle.WriteResponse(connection, guestlifecycle.Response{
			Version: guestlifecycle.Version, Operation: guestlifecycle.QuiesceOperation,
			AppExited: true, SyncDone: true, Mounts: []guestlifecycle.MountResult{},
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := (firecrackerGuestQuiescer{}).Quiesce(ctx, path, 12*time.Second)
	if err != nil {
		t.Fatalf("Quiesce failed: %v", err)
	}
	if !response.Successful() {
		t.Fatalf("guest did not confirm quiesce: %#v", response)
	}
	if err := <-serverResult; err != nil {
		t.Fatalf("vsock peer: %v", err)
	}
}

func TestReadVsockHandshakeRejectsMalformedOrUnboundedAcknowledgements(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{name: "zero host port", line: "OK 0\n"},
		{name: "CRLF", line: "OK 4\r\n"},
		{name: "nondecimal", line: "OK +4\n"},
		{name: "extra data", line: "OK 4 extra\n"},
		{name: "unbounded", line: "OK " + strings.Repeat("1", maxVsockHandshakeBytes) + "\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			listener, path := listenGuestTestSocket(t)
			go func() {
				connection, err := listener.Accept()
				if err != nil {
					return
				}
				defer connection.Close()
				_, _ = bufio.NewReader(connection).ReadString('\n')
				_, _ = connection.Write([]byte(test.line))
			}()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := (firecrackerGuestQuiescer{}).Quiesce(ctx, path, time.Second); err == nil {
				t.Fatal("malformed handshake unexpectedly succeeded")
			}
		})
	}
}

func TestFirecrackerGuestQuiescerHonorsContextDeadline(t *testing.T) {
	listener, path := listenGuestTestSocket(t)
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		_, _ = bufio.NewReader(connection).ReadString('\n')
		// Hold the handshake open until the bounded client closes it.
		buffer := make([]byte, 1)
		_, _ = connection.Read(buffer)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := (firecrackerGuestQuiescer{}).Quiesce(ctx, path, time.Second); err == nil {
		t.Fatal("quiesce unexpectedly ignored context deadline")
	}
	select {
	case <-peerDone:
	case <-time.After(time.Second):
		t.Fatal("peer remained blocked after client deadline")
	}
}

func TestFirecrackerGuestQuiescerRefusedSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sock")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := (firecrackerGuestQuiescer{}).Quiesce(ctx, path, time.Second); err == nil {
		t.Fatal("missing lifecycle socket unexpectedly succeeded")
	}
}

func listenGuestTestSocket(t *testing.T) (net.Listener, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "guest.vsock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("Unix domain sockets unavailable: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(path)
	})
	return listener, path
}
