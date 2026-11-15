//go:build linux

package main

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/guestlifecycle"
	"golang.org/x/sys/unix"
)

func TestLifecycleListenerServesRealAcceptedUnixSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.sock")
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	defer func() { close(done); _ = unix.Shutdown(fd, unix.SHUT_RDWR); _ = unix.Close(fd) }()
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: path}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(fd, 4); err != nil {
		t.Fatal(err)
	}
	calls := make(chan lifecycleCall)
	go acceptLifecycleConnections(fd, calls, done)
	server := make(chan error, 1)
	go func() {
		select {
		case call := <-calls:
			if call.request.ApplicationTimeoutMS != 1000 || call.request.FlushTimeoutMS != 2000 {
				server <- unix.EINVAL
				return
			}
			call.response <- guestlifecycle.Response{Version: guestlifecycle.Version, Operation: guestlifecycle.QuiesceOperation, AppExited: true, SyncDone: true}
			server <- nil
		case <-done:
		}
	}()
	connection, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := guestlifecycle.WriteRequest(connection, guestlifecycle.Request{Version: guestlifecycle.Version, Operation: guestlifecycle.QuiesceOperation, ApplicationTimeoutMS: 1000, FlushTimeoutMS: 2000}); err != nil {
		t.Fatal(err)
	}
	response, err := guestlifecycle.ReadResponse(connection)
	if err != nil {
		t.Fatalf("real accept-and-handle round trip: %v", err)
	}
	if !response.Successful() {
		t.Fatalf("reply=%#v", response)
	}
	if err := <-server; err != nil {
		t.Fatal(err)
	}
}
