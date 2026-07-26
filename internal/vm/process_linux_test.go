//go:build linux

package vm

import (
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startUnreapedChild returns the PID of an exited child that is deliberately
// not waited on, so its /proc entry stays behind in the zombie state.
func startUnreapedChild(t *testing.T) int {
	t.Helper()
	command := exec.Command("/bin/sh", "-c", "exit 0")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	t.Cleanup(func() { _ = command.Wait() })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		statData, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if err != nil {
			t.Fatalf("child %d left /proc before it could be observed: %v", pid, err)
		}
		if exited, err := procStateIsExited(string(statData)); err == nil && exited {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child %d did not become a zombie", pid)
	return 0
}

func TestParseProcStartTicksHandlesSpacesAndParenthesesInCommand(t *testing.T) {
	stat := "123 (fire cracker (vm)) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 424242 20"
	start, err := parseProcStartTicks(stat)
	if err != nil {
		t.Fatal(err)
	}
	if start != 424242 {
		t.Fatalf("start ticks = %d, want 424242", start)
	}
}

func TestProcStateIsExitedOnlyForDeadProcesses(t *testing.T) {
	cases := []struct {
		name    string
		stat    string
		want    bool
		wantErr bool
	}{
		{name: "running", stat: "123 (fire cracker (vm)) R 1 2 3", want: false},
		{name: "sleeping", stat: "123 (firecracker) S 1 2 3", want: false},
		{name: "uninterruptible", stat: "123 (firecracker) D 1 2 3", want: false},
		{name: "zombie", stat: "123 (fire cracker (vm)) Z 1 2 3", want: true},
		{name: "dead upper", stat: "123 (firecracker) X 1 2 3", want: true},
		{name: "dead lower", stat: "123 (firecracker) x 1 2 3", want: true},
		{name: "malformed", stat: "123 firecracker", wantErr: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := procStateIsExited(testCase.stat)
			if testCase.wantErr {
				if err == nil || !strings.Contains(err.Error(), "malformed process stat") {
					t.Fatalf("procStateIsExited(%q) error = %v, want malformed stat", testCase.stat, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != testCase.want {
				t.Fatalf("procStateIsExited(%q) = %t, want %t", testCase.stat, got, testCase.want)
			}
		})
	}
}

func TestProcExitedWithinNeverReportsALiveProcessAsGone(t *testing.T) {
	// errProcessNotFound is what lets recovery delete VM state and stop treat a
	// shutdown as clean, so the live case must hold even under the full timeout.
	if procExitedWithin("/proc/self", 50*time.Millisecond) {
		t.Fatal("the running test process was reported as exited")
	}
	zombie := filepath.Join("/proc", strconv.Itoa(startUnreapedChild(t)))
	if !procExitedWithin(zombie, time.Second) {
		t.Fatalf("zombie %s was not reported as exited", zombie)
	}
	// A PID far above any live one stands in for a process whose entry is gone.
	if !procExitedWithin(filepath.Join("/proc", strconv.Itoa(math.MaxInt32)), 50*time.Millisecond) {
		t.Fatal("a missing /proc entry was not reported as exited")
	}
}

func TestInspectReportsZombieProcessAsNotFound(t *testing.T) {
	// An unreaped child keeps its /proc entry with an empty command line. That
	// must read as an exit, not as an ownership mismatch.
	pid := startUnreapedChild(t)
	if _, err := (osProcessInspector{}).Inspect(pid); !errors.Is(err, errProcessNotFound) {
		t.Fatalf("Inspect(zombie pid %d) = %v, want errProcessNotFound", pid, err)
	}
}
