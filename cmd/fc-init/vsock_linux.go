//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/artemnikitin/firework/internal/guestlifecycle"
)

const maxLifecycleConnectionTime = time.Duration(guestlifecycle.MaxTimeoutMS)*time.Millisecond + 15*time.Second

func startLifecycleServer() (<-chan lifecycleCall, func(), error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("create vsock listener: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: guestlifecycle.GuestPort}); err != nil {
		unix.Close(fd)
		return nil, nil, fmt.Errorf("bind vsock port %d: %w", guestlifecycle.GuestPort, err)
	}
	if err := unix.Listen(fd, 4); err != nil {
		unix.Close(fd)
		return nil, nil, fmt.Errorf("listen on vsock port %d: %w", guestlifecycle.GuestPort, err)
	}
	requests := make(chan lifecycleCall)
	done := make(chan struct{})
	go acceptLifecycleConnections(fd, requests, done)
	closeServer := func() {
		close(done)
		_ = unix.Close(fd)
	}
	return requests, closeServer, nil
}

func acceptLifecycleConnections(listener int, requests chan<- lifecycleCall, done <-chan struct{}) {
	for {
		fd, _, err := unix.Accept4(listener, unix.SOCK_CLOEXEC)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			select {
			case <-done:
				return
			default:
			}
			return
		}
		go handleLifecycleConnection(fd, requests, done)
	}
}

func handleLifecycleConnection(fd int, requests chan<- lifecycleCall, done <-chan struct{}) {
	connection := os.NewFile(uintptr(fd), "fc-init-vsock")
	if connection == nil {
		_ = unix.Close(fd)
		return
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(maxLifecycleConnectionTime)); err != nil {
		return
	}
	request, err := guestlifecycle.ReadRequest(connection)
	if err != nil {
		return
	}
	call := lifecycleCall{request: request, response: make(chan guestlifecycle.Response, 1)}
	select {
	case requests <- call:
	case <-done:
		return
	}
	select {
	case response := <-call.response:
		_ = guestlifecycle.WriteResponse(connection, response)
	case <-done:
	}
}

func guestPathMounted(path string) (bool, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, fmt.Errorf("read mountinfo: %w", err)
	}
	defer file.Close()
	wanted := filepath.Clean(path)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 4 && decodeGuestMountPath(fields[4]) == wanted {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("scan mountinfo: %w", err)
	}
	return false, nil
}

func decodeGuestMountPath(value string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(value)
}
