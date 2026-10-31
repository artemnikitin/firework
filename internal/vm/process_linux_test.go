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

func TestReadProcessCommandLineFailsClosedForUnreadableLiveEntry(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = readProcessCommandLineWith("/proc/123", func(string) ([]byte, error) {
		return nil, os.ErrPermission
	}, func(string) (os.FileInfo, error) {
		return info, nil
	})
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unreadable live process command line error = %v, want permission error", err)
	}
}

func TestReadProcessCommandLineSkipsOnlyVanishedEntry(t *testing.T) {
	_, err := readProcessCommandLineWith("/proc/123", func(string) ([]byte, error) {
		return nil, os.ErrNotExist
	}, func(string) (os.FileInfo, error) {
		return nil, os.ErrNotExist
	})
	if !errors.Is(err, errProcessNotFound) {
		t.Fatalf("vanished process error = %v, want errProcessNotFound", err)
	}
}

func TestVMArgumentScansKeepLegacyToleranceAndFailClosedForCleanup(t *testing.T) {
	procRoot := t.TempDir()
	for _, pid := range []string{"101", "102", "103"} {
		if err := os.Mkdir(filepath.Join(procRoot, pid), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		t.Fatal(err)
	}
	read := func(path string) ([]string, error) {
		switch filepath.Base(path) {
		case "101":
			return nil, os.ErrPermission
		case "102":
			return []string{"other-process"}, nil
		default:
			return nil, errProcessNotFound
		}
	}
	inspectCalls := 0
	inspect := func(pid int) (processIdentity, error) {
		inspectCalls++
		return processIdentity{PID: pid}, nil
	}
	legacy, err := findByVMArgumentsWith(entries, procRoot, "/vm/firecracker.sock", "/vm/vm-config.json", false, false, read, inspect)
	if err != nil || len(legacy) != 0 {
		t.Fatalf("legacy discovery failed on an unrelated unreadable PID: matches=%v err=%v", legacy, err)
	}
	if inspectCalls != 0 {
		t.Fatalf("legacy discovery inspected %d unrelated processes", inspectCalls)
	}
	if _, err := findByVMArgumentsWith(entries, procRoot, "/vm/firecracker.sock", "/vm/vm-config.json", true, true, read, inspect); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("strict cleanup scan treated an incomplete process table as complete: %v", err)
	}
}

func TestCleanupClaimantScanMatchesEitherVMPathArgument(t *testing.T) {
	procRoot := t.TempDir()
	for _, pid := range []string{"201", "202", "203"} {
		if err := os.Mkdir(filepath.Join(procRoot, pid), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		t.Fatal(err)
	}
	arguments := map[string][]string{
		"201": {"firecracker", "--api-sock", "/vm/firecracker.sock"},
		"202": {"firecracker", "--config-file", "/vm/vm-config.json"},
		"203": {"firecracker", "--api-sock", "/vm/other.sock", "--config-file", "/vm/other.json"},
	}
	read := func(path string) ([]string, error) { return arguments[filepath.Base(path)], nil }
	matches, err := findByVMArgumentsWith(entries, procRoot, "/vm/firecracker.sock", "/vm/vm-config.json", true, true, read,
		func(pid int) (processIdentity, error) { return processIdentity{PID: pid}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 || matches[0].PID != 201 || matches[1].PID != 202 {
		t.Fatalf("cleanup scan matches = %#v, want the socket-only and config-only processes", matches)
	}
}

func TestCleanupRefusesHiddenProcessTable(t *testing.T) {
	for _, option := range []string{"hidepid=1", "hidepid=2", "hidepid=invisible"} {
		if verifyProcessVisibilityFor("proc /proc proc rw,"+option+" 0 0", 1000, nil) == nil {
			t.Fatal(option)
		}
	}
	if err := verifyProcessTableVisibility("proc /proc proc rw,hidepid=0 0 0"); err != nil {
		t.Fatal(err)
	}
}

func TestHidepidExemptions(t *testing.T) {
	mounts := "proc /proc proc rw,hidepid=invisible,gid=0 0 0"
	if err := verifyProcessVisibilityFor(mounts, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := verifyProcessVisibilityFor(mounts, 1000, []int{0}); err != nil {
		t.Fatal(err)
	}
	if err := verifyProcessVisibilityFor(mounts, 1000, []int{1000}); err == nil {
		t.Fatal("nonexempt viewer accepted")
	}
}
