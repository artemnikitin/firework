package vm

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/guestlifecycle"
)

type quiesceMonitorInspector struct {
	*fakeProcessInspector
	mu      sync.Mutex
	gone    bool
	gated   bool
	entered chan struct{}
	release chan struct{}
	result  error
}

func (i *quiesceMonitorInspector) Inspect(pid int) (processIdentity, error) {
	i.mu.Lock()
	if i.entered != nil && !i.gated {
		i.gated = true
		i.mu.Unlock()
		close(i.entered)
		<-i.release
		return processIdentity{}, i.result
	}
	defer i.mu.Unlock()
	if i.gone {
		return processIdentity{}, errProcessNotFound
	}
	return i.fakeProcessInspector.Inspect(pid)
}

func runQuiesceMonitor(t *testing.T, manager *Manager, instance *Instance) <-chan struct{} {
	t.Helper()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		manager.monitorAdopted(instance.Name, instance.instanceID)
	}()
	t.Cleanup(func() {
		manager.mu.Lock()
		delete(manager.instances, instance.Name)
		manager.mu.Unlock()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("adopted monitor did not stop after owner removal")
		}
	})
	return finished
}

func TestAdoptedMonitorSurvivesResizeRefusal(t *testing.T) {
	for _, observation := range []string{"stopping tick", "exit inspection in progress", "failed inspection in progress"} {
		t.Run(observation, func(t *testing.T) {
			manager, instance, original, launcher, _ := lifecycleManagerFixture(t)
			inspector := &quiesceMonitorInspector{fakeProcessInspector: original}
			if observation != "stopping tick" {
				inspector.entered, inspector.release = make(chan struct{}), make(chan struct{})
				inspector.result = errProcessNotFound
				if observation == "failed inspection in progress" {
					inspector.result = errors.New("transient inspection failure")
				}
			}
			manager.inspector = inspector
			finished := runQuiesceMonitor(t, manager, instance)
			var releaseOnce sync.Once
			release := func() {
				if inspector.release != nil {
					releaseOnce.Do(func() { close(inspector.release) })
				}
			}
			t.Cleanup(release)
			if inspector.entered != nil {
				select {
				case <-inspector.entered:
				case <-time.After(3 * time.Second):
					t.Fatal("monitor did not begin its ownership inspection")
				}
			}
			manager.quiescer = guestQuiescerFunc(func(context.Context, string, time.Duration, time.Duration) (guestlifecycle.Response, error) {
				release()
				// Keep StateStopping across a real 500ms monitor tick.
				time.Sleep(750 * time.Millisecond)
				return guestlifecycle.Response{}, errors.New("guest unavailable")
			})
			var refusal *GuestQuiesceRequiredError
			if err := manager.RemoveForResize(instance.Name); !errors.As(err, &refusal) {
				t.Fatalf("expected retryable quiesce refusal: %v", err)
			}
			if manager.List()[instance.Name].State != StateRunning || len(launcher.signals) != 0 {
				t.Fatal("refusal did not retain the live VMM")
			}
			select {
			case <-finished:
				t.Fatal("refusal ended exit monitoring")
			default:
			}
			inspector.mu.Lock()
			inspector.gone = true
			inspector.mu.Unlock()
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if manager.List()[instance.Name].State == StateFailed {
					durable, err := readManifest(manifestPath(instance.manifest.VMDir))
					if err != nil || durable.Lifecycle != lifecycleFailed || durable.PID != 0 {
						t.Fatalf("exit was not persisted: manifest=%#v err=%v", durable, err)
					}
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("later VMM exit remained reported as running")
		})
	}
}

func TestAdoptedMonitorCannotApplyStaleExitToReplacement(t *testing.T) {
	for _, result := range []error{errProcessNotFound, errors.New("stale inspection failure")} {
		t.Run(result.Error(), func(t *testing.T) {
			manager, instance, original, _, _ := lifecycleManagerFixture(t)
			inspector := &quiesceMonitorInspector{fakeProcessInspector: original,
				entered: make(chan struct{}), release: make(chan struct{}), result: result}
			manager.inspector = inspector
			finished := runQuiesceMonitor(t, manager, instance)
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(inspector.release) }) }
			t.Cleanup(release)
			select {
			case <-inspector.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("monitor did not begin inspection")
			}
			before, err := os.ReadFile(manifestPath(instance.manifest.VMDir))
			if err != nil {
				t.Fatal(err)
			}
			replacementManifest := *instance.manifest
			replacementManifest.InstanceID = "replacement"
			replacement := instanceFromManifest(&replacementManifest, StateRunning, "")
			manager.mu.Lock()
			manager.instances[instance.Name] = replacement
			manager.mu.Unlock()
			release()
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("stale monitor did not finish")
			}
			current := manager.List()[instance.Name]
			if current.InstanceID != "replacement" || current.State != StateRunning || current.LastError != "" {
				t.Fatalf("stale observation changed replacement: %#v", current)
			}
			after, err := os.ReadFile(manifestPath(instance.manifest.VMDir))
			if err != nil || string(before) != string(after) {
				t.Fatalf("stale observation changed manifest: %v", err)
			}
		})
	}
}
