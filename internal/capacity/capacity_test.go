package capacity

import (
	"runtime"
	"testing"
)

func TestOSReader_ReturnsNonZeroOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("capacity reading only supported on Linux")
	}

	r := NewOSReader()
	cap, err := r.Read()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cap.VCPUs <= 0 {
		t.Errorf("expected VCPUs > 0, got %d", cap.VCPUs)
	}
	if cap.MemoryMB <= 0 {
		t.Errorf("expected MemoryMB > 0, got %d", cap.MemoryMB)
	}
}
