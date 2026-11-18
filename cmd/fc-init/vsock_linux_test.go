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
	for _, test := range []struct {
		name    string
		failure error
	}{
		{"normal accept", nil}, {"aborted accept", unix.ECONNABORTED},
		{"descriptor exhaustion", unix.EMFILE}, {"memory pressure", unix.ENOMEM},
	} {
		t.Run(test.name, func(t *testing.T) { lifecycleListenerRoundTrip(t, test.failure) })
	}
}

func lifecycleListenerRoundTrip(t *testing.T, failure error) {
	t.Helper()
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
	accept := unix.Accept4
	if failure != nil {
		failNext := true
		accept = func(fd, flags int) (int, unix.Sockaddr, error) {
			if failNext {
				failNext = false
				return -1, nil, failure
			}
			return unix.Accept4(fd, flags)
		}
	}
	go acceptLifecycleConnections(fd, calls, done, accept)
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

func TestLifecycleListenerStopsOnInvalidListener(t *testing.T) {
	for _, failure := range []error{unix.EBADF, unix.EINVAL, unix.ENOTSOCK, unix.EOPNOTSUPP} {
		t.Run(failure.Error(), func(t *testing.T) {
			done, finished := make(chan struct{}), make(chan struct{})
			defer close(done)
			calls := 0
			go func() {
				defer close(finished)
				acceptLifecycleConnections(-1, nil, done, func(int, int) (int, unix.Sockaddr, error) {
					calls++
					return -1, nil, failure
				})
			}()
			select {
			case <-finished:
				if calls != 1 {
					t.Fatalf("terminal listener error retried %d times", calls)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("invalid listener was retried indefinitely")
			}
		})
	}
}

func TestLifecycleListenerBackoffHonorsShutdown(t *testing.T) {
	done, finished := make(chan struct{}), make(chan struct{})
	accepted := make(chan struct{}, 2)
	go func() {
		defer close(finished)
		acceptLifecycleConnections(-1, nil, done, func(int, int) (int, unix.Sockaddr, error) {
			select {
			case accepted <- struct{}{}:
			default:
			}
			return -1, nil, unix.EMFILE
		})
	}()
	for range 2 {
		select {
		case <-accepted:
		case <-time.After(2 * time.Second):
			close(done)
			t.Fatal("transient failure was not retried")
		}
	}
	close(done)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("accept backoff delayed shutdown")
	}
}
