package vm

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

// An adopted or systemd-launched VM has no Wait monitor, so a stop that
// outlives SIGKILL must hand the VM to recovery instead of leaving it stopping.
func TestStopTimeoutHandsVMToRecoveryUntilExit(t *testing.T) {
	manager, manifest, inspector := recoveryFixture(t)
	// Signals go to a real child of this test; the fake inspector keeps
	// reporting the recorded identity alive after SIGKILL to model a VM wedged
	// in the kernel.
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	identity := inspector.identities[manifest.PID]
	delete(inspector.identities, manifest.PID)
	identity.PID = child.Process.Pid
	inspector.identities[identity.PID] = identity
	manifest.PID = identity.PID
	if err := writeManifest(manifestPath(manifest.VMDir), manifest); err != nil {
		t.Fatal(err)
	}
	manager.inspector = inspector
	manager.stopGrace, manager.stopKill = 10*time.Millisecond, 10*time.Millisecond
	desired := config.NodeConfig{}

	if adopted, err := manager.Recover(context.Background(), desired); err != nil || len(adopted) != 1 {
		t.Fatalf("survivor was not adopted: adopted=%v err=%v", adopted, err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if err := manager.Stop(manifest.Service); err == nil || !strings.Contains(err.Error(), "did not exit after SIGKILL") {
			t.Fatalf("attempt %d: expected SIGKILL timeout, got %v", attempt, err)
		}
		instance := manager.List()[manifest.Service]
		if instance == nil || instance.State != StateRecoveryPending {
			t.Fatalf("attempt %d: timed-out stop was left outside recovery: %#v", attempt, instance)
		}
		durable, err := readManifest(manifestPath(manifest.VMDir))
		if err != nil || durable.Lifecycle != lifecycleStopping || durable.PID != identity.PID {
			t.Fatalf("attempt %d: durable stop intent was not retained: %#v err=%v", attempt, durable, err)
		}
		// While the owner is still live, recovery re-adopts it so Plan can
		// retry the stop instead of holding the service forever.
		if adopted, err := manager.Recover(context.Background(), desired); err != nil || len(adopted) != 1 {
			t.Fatalf("attempt %d: live owner was not re-adopted: adopted=%v err=%v", attempt, adopted, err)
		}
		if instance := manager.List()[manifest.Service]; instance == nil || instance.State != StateRunning {
			t.Fatalf("attempt %d: re-adopted owner is not running: %#v", attempt, instance)
		}
	}

	if err := manager.Stop(manifest.Service); err == nil {
		t.Fatal("expected a third timeout before the process exits")
	}
	delete(inspector.identities, identity.PID)
	if _, err := manager.Recover(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	if instance := manager.List()[manifest.Service]; instance != nil {
		t.Fatalf("exited owner was not cleared: %#v", instance)
	}
	if _, err := os.Stat(manifest.VMDir); !os.IsNotExist(err) {
		t.Fatalf("exited owner's state directory was not removed: %v", err)
	}
}
