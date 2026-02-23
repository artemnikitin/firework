package capacity

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// NodeCapacity holds the resource capacity of the node.
type NodeCapacity struct {
	VCPUs    int
	MemoryMB int
}

// Reader reads node capacity.
type Reader interface {
	Read() (NodeCapacity, error)
}

// osReader reads capacity from the OS (Linux only).
type osReader struct{}

// NewOSReader returns a Reader that reads capacity from the OS.
func NewOSReader() Reader {
	return &osReader{}
}

// Read returns the node's vCPU count and total memory.
// Returns an error on non-Linux systems.
func (r *osReader) Read() (NodeCapacity, error) {
	if runtime.GOOS != "linux" {
		return NodeCapacity{}, fmt.Errorf("capacity reading not supported on %s", runtime.GOOS)
	}

	vcpus := runtime.NumCPU()

	memMB, err := readMemTotalMB()
	if err != nil {
		return NodeCapacity{}, fmt.Errorf("reading /proc/meminfo: %w", err)
	}

	return NodeCapacity{VCPUs: vcpus, MemoryMB: memMB}, nil
}

// readMemTotalMB parses /proc/meminfo and returns MemTotal in MB.
func readMemTotalMB() (int, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		// Format: "MemTotal:       16384000 kB"
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("unexpected MemTotal line: %q", line)
		}
		kb, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0, fmt.Errorf("parsing MemTotal value %q: %w", fields[1], err)
		}
		return kb / 1024, nil
	}

	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}
