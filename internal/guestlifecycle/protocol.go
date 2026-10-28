package guestlifecycle

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
)

const (
	Version           uint16 = 1
	QuiesceOperation         = "quiesce"
	GuestPort         uint32 = 4050
	MaxFrameBytes            = 32 * 1024
	MaxMountResults          = 25
	MaxMountNameBytes        = 128
	MaxMountPathBytes        = 256
	MaxErrorBytes            = 256
	MaxErrorCodeBytes        = 64
	MaxTimeoutMS      uint32 = 5 * 60 * 1000
)

// Request is the only host-to-guest operation currently supported.
type Request struct {
	Version   uint16 `json:"version"`
	Operation string `json:"operation"`
	TimeoutMS uint32 `json:"timeout_ms"`
}

// MountResult records one persistent mount's outcome in guest unmount order,
// which is the reverse of the mount order.
type MountResult struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Unmounted bool   `json:"unmounted"`
	Error     string `json:"error,omitempty"`
}

// Response describes whether application exit, sync, and every unmount were
// confirmed by the guest. A successful application exit code is not required
// for quiesce success.
type Response struct {
	Version     uint16        `json:"version"`
	Operation   string        `json:"operation"`
	AppExited   bool          `json:"app_exited"`
	AppExitCode int           `json:"app_exit_code,omitempty"`
	SyncDone    bool          `json:"sync_done"`
	Mounts      []MountResult `json:"mounts"`
	ErrorCode   string        `json:"error_code,omitempty"`
}

func (r Request) Validate() error {
	if r.Version != Version {
		return fmt.Errorf("unsupported lifecycle protocol version %d", r.Version)
	}
	if r.Operation != QuiesceOperation {
		return fmt.Errorf("unsupported lifecycle operation %q", r.Operation)
	}
	if r.TimeoutMS == 0 || r.TimeoutMS > MaxTimeoutMS {
		return fmt.Errorf("lifecycle timeout must be between 1 and %d milliseconds", MaxTimeoutMS)
	}
	return nil
}

func (r Response) Validate() error {
	if r.Version != Version {
		return fmt.Errorf("unsupported lifecycle protocol version %d", r.Version)
	}
	if r.Operation != QuiesceOperation {
		return fmt.Errorf("unsupported lifecycle operation %q", r.Operation)
	}
	if len(r.Mounts) > MaxMountResults {
		return fmt.Errorf("lifecycle response contains %d mounts, maximum is %d", len(r.Mounts), MaxMountResults)
	}
	if !r.AppExited && r.AppExitCode != 0 {
		return fmt.Errorf("lifecycle response has an exit code before application exit")
	}
	if !r.AppExited && r.ErrorCode == "" {
		return fmt.Errorf("lifecycle response has neither application exit nor an error code")
	}
	if r.AppExited && !r.SyncDone && r.ErrorCode == "" {
		return fmt.Errorf("lifecycle response has neither a completed sync nor an error code")
	}
	if r.AppExitCode < 0 || r.AppExitCode > 255 {
		return fmt.Errorf("application exit code %d is outside the supported range", r.AppExitCode)
	}
	if len(r.ErrorCode) > MaxErrorCodeBytes {
		return fmt.Errorf("lifecycle error code exceeds %d bytes", MaxErrorCodeBytes)
	}
	seen := make(map[string]struct{}, len(r.Mounts))
	for i, mount := range r.Mounts {
		if mount.Name == "" || len(mount.Name) > MaxMountNameBytes {
			return fmt.Errorf("lifecycle mount %d has invalid name", i)
		}
		if !filepath.IsAbs(mount.Path) || filepath.Clean(mount.Path) != mount.Path || len(mount.Path) > MaxMountPathBytes {
			return fmt.Errorf("lifecycle mount %q has invalid path", mount.Name)
		}
		if _, duplicate := seen[mount.Name]; duplicate {
			return fmt.Errorf("lifecycle response repeats mount %q", mount.Name)
		}
		seen[mount.Name] = struct{}{}
		if len(mount.Error) > MaxErrorBytes {
			return fmt.Errorf("lifecycle mount %q error exceeds %d bytes", mount.Name, MaxErrorBytes)
		}
		if mount.Unmounted && mount.Error != "" {
			return fmt.Errorf("lifecycle mount %q has both success and error", mount.Name)
		}
		if !mount.Unmounted && mount.Error == "" {
			return fmt.Errorf("lifecycle mount %q has no outcome", mount.Name)
		}
	}
	return nil
}

func (r Response) Successful() bool {
	if r.Validate() != nil || !r.AppExited || !r.SyncDone || r.ErrorCode != "" {
		return false
	}
	for _, mount := range r.Mounts {
		if !mount.Unmounted || mount.Error != "" {
			return false
		}
	}
	return true
}

func ReadRequest(reader io.Reader) (Request, error) {
	var request Request
	if err := readFrame(reader, &request); err != nil {
		return request, err
	}
	if err := request.Validate(); err != nil {
		return request, err
	}
	return request, nil
}

func WriteRequest(writer io.Writer, request Request) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return writeFrame(writer, request)
}

func ReadResponse(reader io.Reader) (Response, error) {
	var response Response
	if err := readFrame(reader, &response); err != nil {
		return response, err
	}
	if err := response.Validate(); err != nil {
		return response, err
	}
	return response, nil
}

func WriteResponse(writer io.Writer, response Response) error {
	if err := response.Validate(); err != nil {
		return err
	}
	return writeFrame(writer, response)
}

func writeFrame(writer io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode lifecycle frame: %w", err)
	}
	if len(data) == 0 || len(data) > MaxFrameBytes {
		return fmt.Errorf("lifecycle frame has invalid size %d", len(data))
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if err := writeAll(writer, header[:]); err != nil {
		return fmt.Errorf("write lifecycle frame length: %w", err)
	}
	if err := writeAll(writer, data); err != nil {
		return fmt.Errorf("write lifecycle frame body: %w", err)
	}
	return nil
}

func readFrame(reader io.Reader, target any) error {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return fmt.Errorf("read lifecycle frame length: %w", err)
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > MaxFrameBytes {
		return fmt.Errorf("lifecycle frame has invalid size %d", size)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(reader, data); err != nil {
		return fmt.Errorf("read lifecycle frame body: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode lifecycle frame: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return fmt.Errorf("lifecycle frame contains multiple JSON values")
		}
		return fmt.Errorf("decode lifecycle frame trailer: %w", err)
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
