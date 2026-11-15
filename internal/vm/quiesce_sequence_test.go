package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/guestlifecycle"
)

func TestRemoveForResizeRetriesAfterPartialGuestQuiesce(t *testing.T) {
	manager, instance, inspector, launcher, volumePath := lifecycleManagerFixture(t)
	attempts := 0
	manager.quiescer = guestQuiescerFunc(func(context.Context, string, time.Duration, time.Duration) (guestlifecycle.Response, error) {
		attempts++
		response := successfulQuiesceResponse()
		if attempts == 1 {
			response.Mounts[0].Unmounted = false
			response.Mounts[0].Error = "busy"
		}
		return response, nil
	})
	launcher.onStop = func(syscall.Signal) { delete(inspector.identities, instance.manifest.PID) }
	var refusal *GuestQuiesceRequiredError
	if err := manager.RemoveForResize(instance.Name); !errors.As(err, &refusal) || !refusal.ApplicationExited {
		t.Fatalf("partial shutdown result lost: %v", err)
	}
	if len(launcher.signals) != 0 {
		t.Fatal("partial shutdown killed the VMM")
	}
	if err := manager.RemoveForResize(instance.Name); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if attempts != 2 || len(launcher.signals) != 1 || manager.List()[instance.Name] != nil {
		t.Fatalf("retry did not converge: attempts=%d signals=%v", attempts, launcher.signals)
	}
	if data, err := os.ReadFile(volumePath); err != nil || string(data) != "old-volume-content" {
		t.Fatalf("removal changed retained volume: %q %v", data, err)
	}
}

func TestVMMExitDuringQuiesceAllowsResizeRemovalWithoutSignal(t *testing.T) {
	for _, replyLost := range []bool{false, true} {
		t.Run(fmt.Sprint(replyLost), func(t *testing.T) {
			manager, instance, inspector, launcher, _ := lifecycleManagerFixture(t)
			manager.quiescer = guestQuiescerFunc(func(context.Context, string, time.Duration, time.Duration) (guestlifecycle.Response, error) {
				delete(inspector.identities, instance.manifest.PID)
				if replyLost {
					return guestlifecycle.Response{}, errors.New("VMM exited before reply")
				}
				return successfulQuiesceResponse(), nil
			})
			if err := manager.RemoveForResize(instance.Name); err != nil {
				t.Fatalf("proven absence remained blocked: %v", err)
			}
			if len(launcher.signals) != 0 || manager.List()[instance.Name] != nil {
				t.Fatalf("stale signal or retained owner: signals=%v instance=%#v", launcher.signals, manager.List()[instance.Name])
			}
		})
	}
}

func TestQuiesceReplyCannotActOnReplacementInstance(t *testing.T) {
	manager, instance, _, launcher, _ := lifecycleManagerFixture(t)
	replacementManifest := *instance.manifest
	replacementManifest.InstanceID = "replacement"
	replacement := instanceFromManifest(&replacementManifest, StateRunning, "")
	manager.quiescer = guestQuiescerFunc(func(context.Context, string, time.Duration, time.Duration) (guestlifecycle.Response, error) {
		manager.mu.Lock()
		manager.instances[instance.Name] = replacement
		manager.mu.Unlock()
		return successfulQuiesceResponse(), nil
	})
	if err := manager.RemoveForResize(instance.Name); err == nil {
		t.Fatal("old confirmation authorized replacement removal")
	}
	if len(launcher.signals) != 0 || manager.List()[instance.Name].InstanceID != "replacement" || manager.List()[instance.Name].State != StateRunning {
		t.Fatalf("replacement changed: %#v signals=%v", manager.List()[instance.Name], launcher.signals)
	}
}

func TestAgentRestartDuringQuiesceReadoptsAndRequestsAgain(t *testing.T) {
	for _, guestCompleted := range []bool{false, true} {
		t.Run(fmt.Sprint(guestCompleted), func(t *testing.T) {
			manager, manifest, inspector := recoveryFixture(t)
			// This is the only durable host change before waiting for the guest.
			manifest.Lifecycle = lifecycleStopping
			if err := writeManifest(manifestPath(manifest.VMDir), manifest); err != nil {
				t.Fatal(err)
			}
			manager.inspector = inspector
			adopted, err := manager.Recover(context.Background(), config.NodeConfig{})
			if err != nil || len(adopted) != 1 || manager.List()[manifest.Service].State != StateRunning {
				t.Fatalf("restart did not readopt stopping survivor: %v %v", adopted, err)
			}
			attempts := 0
			manager.quiescer = guestQuiescerFunc(func(context.Context, string, time.Duration, time.Duration) (guestlifecycle.Response, error) {
				attempts++
				if !guestCompleted {
					guestCompleted = true
				}
				return guestlifecycle.Response{Version: guestlifecycle.Version, Operation: guestlifecycle.QuiesceOperation, AppExited: true, SyncDone: true}, nil
			})
			launcher := &lifecycleTestLauncher{onStop: func(syscall.Signal) { delete(inspector.identities, manifest.PID) }}
			manager.launcher = launcher
			if err := manager.RemoveForResize(manifest.Service); err != nil || attempts != 1 || len(launcher.signals) != 1 {
				t.Fatalf("fresh operation did not converge: attempts=%d signals=%v err=%v", attempts, launcher.signals, err)
			}
		})
	}
}

func TestGuestQuiesceBudgetRejectsOverflowAndKeepsSeparateTimeouts(t *testing.T) {
	budget, err := guestQuiesceBudget(2*time.Second, 3*time.Second)
	if err != nil || budget != 20*time.Second {
		t.Fatalf("total budget=%s err=%v", budget, err)
	}
	for _, durations := range [][2]time.Duration{{1, time.Second}, {time.Second, 0}, {1<<63 - 1, time.Second}, {time.Second, 1<<63 - 1}} {
		if _, err := guestQuiesceBudget(durations[0], durations[1]); err == nil {
			t.Fatalf("invalid or overflowing budget accepted: %v", durations)
		}
	}
	manager, instance, _, _, _ := lifecycleManagerFixture(t)
	manager.guestApplicationShutdownTimeout = time.Millisecond
	manager.guestFlushTimeout = 2 * time.Millisecond
	manager.quiescer = guestQuiescerFunc(func(ctx context.Context, _ string, app, flush time.Duration) (guestlifecycle.Response, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > vsockTransportTimeMargin+3*time.Millisecond || app != time.Millisecond || flush != 2*time.Millisecond {
			t.Fatalf("independent budgets changed: deadline=%v app=%v flush=%v", deadline, app, flush)
		}
		return successfulQuiesceResponse(), nil
	})
	if _, err := manager.requestGuestQuiesce(instance.manifest); err != nil {
		t.Fatal(err)
	}
}

func TestQuiesceRefusalPersistenceFailureDoesNotSignal(t *testing.T) {
	manager, instance, _, launcher, _ := lifecycleManagerFixture(t)
	manager.quiescer = guestQuiescerFunc(func(context.Context, string, time.Duration, time.Duration) (guestlifecycle.Response, error) {
		return guestlifecycle.Response{}, errors.New("unavailable")
	})
	manager.recoveryManifestWriter = func(path string, manifest *instanceManifest) error {
		if manifest.Lifecycle == lifecycleRunning {
			return errors.New("disk failure")
		}
		return writeManifest(path, manifest)
	}
	if err := manager.RemoveForResize(instance.Name); err == nil || !strings.Contains(err.Error(), "persist quiesce refusal") {
		t.Fatalf("write failure lost: %v", err)
	}
	if len(launcher.signals) != 0 || manager.List()[instance.Name].State != StateRecoveryPending {
		t.Fatalf("write failure did not retain owner safely: %#v signals=%v", manager.List()[instance.Name], launcher.signals)
	}
}
