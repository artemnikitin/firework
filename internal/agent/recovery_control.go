package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/artemnikitin/firework/internal/vm"
)

const (
	recoveryControlProtocolVersion = 1
	recoveryControlMaxFrameBytes   = 4096
	recoveryControlDeadline        = 10 * time.Second
	recoveryControlSocketName      = "recovery.sock"
	recoveryControlMaxHandlers     = 16
	recoveryAcceptRetryInitial     = 10 * time.Millisecond
	recoveryAcceptRetryMaximum     = time.Second
)

type recoveryControlRequest struct {
	Version int    `json:"version"`
	Service string `json:"service"`
}

type recoveryControlResponse struct {
	Version int               `json:"version"`
	Result  vm.RecoveryResult `json:"result"`
	Error   string            `json:"error,omitempty"`
}

type recoveryControlResolver func(string) (vm.RecoveryResult, error)

func (a *Agent) clearRecoveryPending(service string) (vm.RecoveryResult, error) {
	result, err := a.vmManager.ClearRecoveryPending(service)
	if err != nil {
		return result, err
	}
	if result.Outcome == vm.RecoveryOutcomeCleared || result.Outcome == vm.RecoveryOutcomeReadopted {
		a.queueRecoveryReconcile(service, result.Outcome == vm.RecoveryOutcomeReadopted)
	}
	return result, nil
}

func (a *Agent) queueRecoveryReconcile(service string, adopted bool) {
	a.recoveryMu.Lock()
	if a.recoveryQueue == nil {
		a.recoveryQueue = make(map[string]bool)
	}
	a.recoveryQueue[service] = a.recoveryQueue[service] || adopted
	a.recoveryMu.Unlock()
	select {
	case a.recoveryWake <- struct{}{}:
	default:
	}
}

func (a *Agent) applyRecoveryRequests(ctx context.Context) {
	requests := a.takeRecoveryRequests()
	if len(requests) == 0 {
		return
	}
	for service, adopted := range requests {
		if adopted {
			a.reconciler.MarkRecovered(service)
		} else {
			a.reconciler.ForgetRecovered(service)
		}
	}
	a.forceReconcile = true
	a.tick(ctx)
}

func (a *Agent) takeRecoveryRequests() map[string]bool {
	a.recoveryMu.Lock()
	requests := a.recoveryQueue
	a.recoveryQueue = make(map[string]bool)
	a.recoveryMu.Unlock()
	return requests
}

type recoveryControlServer struct {
	listener   *net.UnixListener
	path       string
	socketInfo os.FileInfo
	lock       io.Closer
	resolve    recoveryControlResolver
	logger     *slog.Logger
	done       chan struct{}
	closing    chan struct{}
	mu         sync.Mutex
	active     map[*net.UnixConn]struct{}
	handlers   sync.WaitGroup
	closed     bool
	closeOnce  sync.Once
	closeErr   error
}

func recoveryControlPaths(stateDir string) (statePath, controlDir, socketPath, lockPath string, err error) {
	if stateDir == "" {
		return "", "", "", "", fmt.Errorf("agent state directory is empty")
	}
	statePath, err = filepath.Abs(stateDir)
	if err != nil {
		return "", "", "", "", fmt.Errorf("resolve agent state directory: %w", err)
	}
	controlDir = filepath.Join(statePath, "control")
	socketPath = filepath.Join(controlDir, recoveryControlSocketName)
	lockPath = filepath.Join(controlDir, ".recovery.lock")
	if len([]byte(socketPath)) > 103 {
		return "", "", "", "", fmt.Errorf("recovery control socket path exceeds the Unix socket path limit")
	}
	return statePath, controlDir, socketPath, lockPath, nil
}

func prepareRecoveryControlDirectory(statePath, controlDir string, create, allowRoot bool) error {
	if create {
		if err := os.MkdirAll(statePath, 0o700); err != nil {
			return fmt.Errorf("create agent state directory: %w", err)
		}
	}
	stateInfo, err := os.Lstat(statePath)
	if err != nil {
		return fmt.Errorf("inspect agent state directory: %w", err)
	}
	if !stateInfo.IsDir() || stateInfo.Mode()&os.ModeSymlink != 0 ||
		!recoveryControlOwned(stateInfo, allowRoot) || stateInfo.Mode().Perm()&0o022 != 0 || stateInfo.Mode().Perm()&0o300 != 0o300 {
		return fmt.Errorf("agent state directory must be owned by the agent and not writable by other users")
	}
	if create {
		if err := os.Mkdir(controlDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create recovery control directory: %w", err)
		}
	}
	controlInfo, err := os.Lstat(controlDir)
	if err != nil {
		return fmt.Errorf("inspect recovery control directory: %w", err)
	}
	if !controlInfo.IsDir() || controlInfo.Mode()&os.ModeSymlink != 0 ||
		!recoveryControlOwned(controlInfo, allowRoot) || controlInfo.Mode().Perm() != 0o700 {
		return fmt.Errorf("recovery control directory must be an agent-owned directory with mode 0700")
	}
	return nil
}

func startRecoveryControlServer(stateDir string, logger *slog.Logger, resolve recoveryControlResolver) (*recoveryControlServer, error) {
	if logger == nil {
		logger = slog.Default()
	}
	statePath, controlDir, socketPath, lockPath, err := recoveryControlPaths(stateDir)
	if err != nil {
		return nil, err
	}
	if err := prepareRecoveryControlDirectory(statePath, controlDir, true, false); err != nil {
		return nil, err
	}
	lock, err := acquireRecoveryControlLock(lockPath)
	if err != nil {
		return nil, fmt.Errorf("lock recovery control socket: %w", err)
	}
	cleanupLock := true
	defer func() {
		if cleanupLock {
			_ = lock.Close()
		}
	}()
	if err := removeStaleRecoveryControlSocket(socketPath); err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on recovery control socket: %w", err)
	}
	listener.SetUnlinkOnClose(false)
	var socketInfo os.FileInfo
	closeListener := true
	defer func() {
		if closeListener {
			_ = listener.Close()
			_ = removeRecoveryControlSocket(socketPath, socketInfo)
		}
	}()
	socketInfo, err = os.Lstat(socketPath)
	if err != nil {
		return nil, fmt.Errorf("inspect recovery control socket: %w", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return nil, fmt.Errorf("restrict recovery control socket: %w", err)
	}
	securedSocketInfo, err := os.Lstat(socketPath)
	if err != nil {
		return nil, fmt.Errorf("inspect recovery control socket: %w", err)
	}
	if !os.SameFile(socketInfo, securedSocketInfo) {
		return nil, fmt.Errorf("recovery control socket changed while setting its permissions")
	}
	socketInfo = securedSocketInfo
	if socketInfo.Mode()&os.ModeSocket == 0 || !recoveryControlOwned(socketInfo, false) || socketInfo.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("recovery control socket does not have the expected owner and mode")
	}
	server := &recoveryControlServer{
		listener: listener, path: socketPath, socketInfo: socketInfo, lock: lock,
		resolve: resolve, logger: logger, done: make(chan struct{}), closing: make(chan struct{}),
		active: make(map[*net.UnixConn]struct{}),
	}
	cleanupLock = false
	closeListener = false
	go server.serve()
	return server, nil
}

func (s *recoveryControlServer) serve() {
	defer close(s.done)
	defer s.listener.Close()
	s.serveAccepting(s.listener.AcceptUnix)
}

func (s *recoveryControlServer) serveAccepting(accept func() (*net.UnixConn, error)) {
	if s.listener != nil {
		defer s.listener.Close()
	}
	delay := recoveryAcceptRetryInitial
	for {
		connection, err := accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if isRetryableRecoveryAcceptError(err) {
				s.logger.Warn("temporary recovery control accept failure", "error", err)
				timer := time.NewTimer(delay)
				select {
				case <-s.closing:
					if !timer.Stop() {
						<-timer.C
					}
					return
				case <-timer.C:
				}
				if delay < recoveryAcceptRetryMaximum {
					delay *= 2
					if delay > recoveryAcceptRetryMaximum {
						delay = recoveryAcceptRetryMaximum
					}
				}
				continue
			}
			s.logger.Error("recovery control socket stopped", "error", err)
			return
		}
		delay = recoveryAcceptRetryInitial
		s.startConnection(connection)
	}
}

func isRetryableRecoveryAcceptError(err error) bool {
	return errors.Is(err, syscall.EINTR) || errors.Is(err, syscall.EAGAIN) ||
		errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ENOBUFS) || errors.Is(err, syscall.ENOMEM)
}

func (s *recoveryControlServer) startConnection(connection *net.UnixConn) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = connection.Close()
		return
	}
	if len(s.active) >= recoveryControlMaxHandlers {
		s.mu.Unlock()
		_ = connection.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
		s.writeResponse(connection, recoveryControlResponse{
			Version: recoveryControlProtocolVersion,
			Error:   "recovery control server is handling its maximum number of requests",
		})
		_ = connection.Close()
		return
	}
	s.active[connection] = struct{}{}
	s.handlers.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.handlers.Done()
		defer func() {
			s.mu.Lock()
			delete(s.active, connection)
			s.mu.Unlock()
		}()
		s.serveConnection(connection)
	}()
}

func (s *recoveryControlServer) serveConnection(connection *net.UnixConn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(recoveryControlDeadline))
	body, err := readRecoveryControlFrame(connection)
	if err != nil {
		s.writeResponse(connection, recoveryControlResponse{Version: recoveryControlProtocolVersion, Error: err.Error()})
		return
	}
	request, err := decodeRecoveryControlRequest(body)
	if err != nil {
		s.writeResponse(connection, recoveryControlResponse{Version: recoveryControlProtocolVersion, Error: err.Error()})
		return
	}
	result, resolveErr := s.resolve(request.Service)
	response := recoveryControlResponse{Version: recoveryControlProtocolVersion, Result: result}
	if resolveErr != nil {
		response.Error = resolveErr.Error()
	}
	s.writeResponse(connection, response)
}

func (s *recoveryControlServer) writeResponse(connection io.Writer, response recoveryControlResponse) {
	body, err := json.Marshal(response)
	if err != nil || len(body) > recoveryControlMaxFrameBytes {
		body, _ = json.Marshal(recoveryControlResponse{
			Version: recoveryControlProtocolVersion,
			Error:   "recovery control response exceeds the size limit",
		})
	}
	if err := writeRecoveryControlFrame(connection, body); err != nil {
		s.logger.Debug("could not write recovery control response", "error", err)
	}
}

func (s *recoveryControlServer) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.closing)
		_ = s.listener.Close()
		for connection := range s.active {
			_ = connection.Close()
		}
		s.mu.Unlock()
		<-s.done
		s.handlers.Wait()
		s.closeErr = removeRecoveryControlSocket(s.path, s.socketInfo)
		if err := s.lock.Close(); s.closeErr == nil && err != nil {
			s.closeErr = err
		}
	})
	return s.closeErr
}

// ClearRecovery sends one bounded request to the running agent's private local
// control socket. The caller must run as root or as the agent's Unix user.
func ClearRecovery(ctx context.Context, stateDir, service string) (vm.RecoveryResult, error) {
	if err := vm.ValidateRecoveryServiceName(service); err != nil {
		return vm.RecoveryResult{}, err
	}
	statePath, controlDir, socketPath, _, err := recoveryControlPaths(stateDir)
	if err != nil {
		return vm.RecoveryResult{}, err
	}
	if err := prepareRecoveryControlDirectory(statePath, controlDir, false, true); err != nil {
		return vm.RecoveryResult{}, err
	}
	controlInfo, err := os.Lstat(controlDir)
	if err != nil {
		return vm.RecoveryResult{}, fmt.Errorf("inspect recovery control directory: %w", err)
	}
	socketInfo, err := os.Lstat(socketPath)
	if err != nil {
		return vm.RecoveryResult{}, fmt.Errorf("inspect recovery control socket: %w", err)
	}
	if socketInfo.Mode()&os.ModeSocket == 0 || !recoveryControlOwned(socketInfo, true) ||
		!recoveryControlSameOwner(socketInfo, controlInfo) || socketInfo.Mode().Perm() != 0o600 {
		return vm.RecoveryResult{}, fmt.Errorf("recovery control socket does not have the expected owner and mode")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(ctx, recoveryControlDeadline)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(requestCtx, "unix", socketPath)
	if err != nil {
		return vm.RecoveryResult{}, fmt.Errorf("connect to running agent recovery control socket: %w", err)
	}
	defer connection.Close()
	deadline, ok := requestCtx.Deadline()
	if ok {
		_ = connection.SetDeadline(deadline)
	}
	body, err := json.Marshal(recoveryControlRequest{Version: recoveryControlProtocolVersion, Service: service})
	if err != nil {
		return vm.RecoveryResult{}, err
	}
	if err := writeRecoveryControlFrame(connection, body); err != nil {
		return vm.RecoveryResult{}, fmt.Errorf("send recovery request: %w", err)
	}
	responseBody, err := readRecoveryControlFrame(connection)
	if err != nil {
		return vm.RecoveryResult{}, fmt.Errorf("recovery response unavailable; outcome is unknown, inspect service status before retrying: %w", err)
	}
	response, err := decodeRecoveryControlResponse(responseBody)
	if err != nil {
		return vm.RecoveryResult{}, err
	}
	if response.Version != recoveryControlProtocolVersion {
		return response.Result, fmt.Errorf("unsupported recovery control response version %d", response.Version)
	}
	if response.Result.Service != "" && response.Result.Service != service {
		return response.Result, fmt.Errorf("recovery control response names service %q instead of %q", response.Result.Service, service)
	}
	if response.Error != "" {
		return response.Result, errors.New(response.Error)
	}
	return response.Result, nil
}

func decodeRecoveryControlRequest(body []byte) (recoveryControlRequest, error) {
	var request recoveryControlRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, fmt.Errorf("decode recovery request: %w", err)
	}
	if err := ensureRecoveryJSONEnd(decoder); err != nil {
		return request, err
	}
	if request.Version != recoveryControlProtocolVersion {
		return request, fmt.Errorf("unsupported recovery request version %d", request.Version)
	}
	if err := vm.ValidateRecoveryServiceName(request.Service); err != nil {
		return request, err
	}
	return request, nil
}

func decodeRecoveryControlResponse(body []byte) (recoveryControlResponse, error) {
	var response recoveryControlResponse
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return response, fmt.Errorf("decode recovery response: %w", err)
	}
	if err := ensureRecoveryJSONEnd(decoder); err != nil {
		return response, err
	}
	return response, nil
}

func ensureRecoveryJSONEnd(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("recovery control frame contains multiple JSON values")
		}
		return fmt.Errorf("decode recovery control frame end: %w", err)
	}
	return nil
}

func writeRecoveryControlFrame(writer io.Writer, body []byte) error {
	if len(body) == 0 || len(body) > recoveryControlMaxFrameBytes {
		return fmt.Errorf("recovery control frame size %d is outside the allowed range", len(body))
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if err := writeRecoveryControlBytes(writer, header[:]); err != nil {
		return err
	}
	return writeRecoveryControlBytes(writer, body)
}

func readRecoveryControlFrame(reader io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, fmt.Errorf("read recovery control frame header: %w", err)
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > recoveryControlMaxFrameBytes {
		return nil, fmt.Errorf("recovery control frame size %d is outside the allowed range", size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, fmt.Errorf("read recovery control frame body: %w", err)
	}
	return body, nil
}

func writeRecoveryControlBytes(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func removeStaleRecoveryControlSocket(path string) error {
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing recovery control path: %w", err)
	}
	if before.Mode()&os.ModeSocket == 0 || !recoveryControlOwned(before, false) || before.Mode().Perm() != 0o600 {
		return fmt.Errorf("existing recovery control path is not a private agent-owned socket")
	}
	connection, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		return fmt.Errorf("another recovery control server is accepting requests")
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		if errors.Is(dialErr, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cannot prove existing recovery socket is stale: %w", dialErr)
	}
	after, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !os.SameFile(before, after) {
		return fmt.Errorf("recovery control path changed while checking for a stale socket")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale recovery control socket: %w", err)
	}
	return nil
}

func removeRecoveryControlSocket(path string, expected os.FileInfo) error {
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect recovery control socket during shutdown: %w", err)
	}
	if current.Mode()&os.ModeSocket == 0 || !recoveryControlOwned(current, false) ||
		(expected != nil && !os.SameFile(expected, current)) {
		return fmt.Errorf("recovery control path changed before shutdown cleanup")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove recovery control socket: %w", err)
	}
	return nil
}
