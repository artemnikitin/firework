//go:build linux && systemd_integration

package agent

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// Runs in the isolated container of scripts/systemd-recovery, whose PID1 is
// real systemd. The observer behind L7 must see a restart of the serving unit
// as a different process, and a stopped unit as none.
func TestSystemdIntegrationRouteProcessObservesRestarts(t *testing.T) {
	if os.Getenv("FIREWORK_REAL_SYSTEMD") != "1" {
		t.Skip("requires isolated real systemd")
	}
	// An installed unit, like the deployment's traefik.service: a transient
	// one is unloaded once stopped.
	unit := "fake-traefik.service"
	path := "/etc/systemd/system/" + unit
	if err := os.WriteFile(path, []byte("[Service]\nExecStart=/bin/sleep 300\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	defer exec.Command("systemctl", "stop", unit).Run()
	for _, args := range [][]string{{"daemon-reload"}, {"start", unit}} {
		if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
			t.Fatalf("systemctl %v: %v %s", args, err, out)
		}
	}
	process := systemdRouteProcess{unit: unit}
	boot, first, err := process.Observe()
	if err != nil || boot == "" || first == "" {
		t.Fatalf("running unit: boot=%q start=%q err=%v", boot, first, err)
	}
	time.Sleep(10 * time.Millisecond)
	if out, err := exec.Command("systemctl", "restart", unit).CombinedOutput(); err != nil {
		t.Fatalf("restarting: %v %s", err, out)
	}
	_, second, err := process.Observe()
	if err != nil || second == "" || second == first {
		t.Fatalf("a restart was not seen: %q then %q err=%v", first, second, err)
	}
	if out, err := exec.Command("systemctl", "stop", unit).CombinedOutput(); err != nil {
		t.Fatalf("stopping: %v %s", err, out)
	}
	if _, stopped, err := process.Observe(); err != nil || stopped != "" {
		t.Fatalf("a stopped unit: start=%q err=%v", stopped, err)
	}
	if _, _, err := (systemdRouteProcess{unit: "missing-unit.service"}).Observe(); err == nil {
		t.Fatal("a missing unit was observed")
	}
	t.Logf("observed %s: %s, then %s after a restart, none once stopped", unit, first, second)
}
