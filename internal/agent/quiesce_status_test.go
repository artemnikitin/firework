package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/healthcheck"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/statusmodel"
	"github.com/artemnikitin/firework/internal/vm"
)

func TestQuiesceExitObservationCannotCountStoppedApplicationReady(t *testing.T) {
	for _, health := range []string{"healthy", "not_configured", "unhealthy"} {
		t.Run(health, func(t *testing.T) {
			service := statusmodel.ServiceStatus{VMState: "running", Health: health}
			if health == "unhealthy" {
				service.ReasonCode = "health_check_failed"
				service.Message = "connection refused"
			}
			applyGuestQuiesceObservation(&service, reconciler.Hold{ApplicationExited: true})
			if serviceReady(service) {
				t.Fatalf("known exited application counted ready: %#v", service)
			}
			if health == "unhealthy" {
				if service.Health != "unhealthy" || service.ReasonCode != "health_check_failed" || service.Message != "connection refused" {
					t.Fatalf("health diagnosis overwritten: %#v", service)
				}
			} else if service.ReasonCode != "guest_application_stopped" {
				t.Fatalf("withdrawn resize lost exit observation: %#v", service)
			}
		})
	}
	oldGuest := statusmodel.ServiceStatus{VMState: "running", Health: "healthy"}
	applyGuestQuiesceObservation(&oldGuest, reconciler.Hold{Reason: "guest has no listener"})
	if !serviceReady(oldGuest) {
		t.Fatalf("unavailable listener falsely stopped a healthy app: %#v", oldGuest)
	}
}

type quiesceRefusingVMManager struct {
	*recoveryTestVMManager
	attempts int
}

func (m *quiesceRefusingVMManager) RemoveForResize(string) error {
	m.attempts++
	return &vm.GuestQuiesceRequiredError{Cause: errors.New("unmount busy"), ApplicationExited: true}
}

func TestRefusedResizeKeepsRoutesHealthAndActualConfigOnHealthRestart(t *testing.T) {
	for _, strategy := range []string{"", "rolling"} {
		t.Run(strategy, func(t *testing.T) {
			original := routingPersistenceService()
			original.Kernel, original.VCPUs, original.MemoryMB = "/kernel", 2, 128
			original.HealthCheck.Interval = time.Hour
			original.PortForwards = []config.PortForward{{HostPort: 8080, VMPort: 80}}
			original.Volumes = []config.VolumeConfig{{Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data", SizeBytes: 8 * config.MiB}}
			routeDir := t.TempDir()
			a, underlying := routingPersistenceAgent(t, routeDir, original, "original-owner")
			manager := &quiesceRefusingVMManager{recoveryTestVMManager: underlying}
			monitor := healthcheck.NewMonitor(testLogger(), nil)
			t.Cleanup(monitor.Stop)
			monitor.Register(context.Background(), original)
			network := &recoveryTestNetworkManager{}
			a.reconciler = reconciler.NewWithNetworkManager(manager, testLogger(), monitor, network, strategy, 0).WithStateDir(t.TempDir())
			a.reconciler.SetCapacity(capacity.NodeCapacity{VCPUs: 2, MemoryMB: 256}, true, nil)
			if err := a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{original}); err != nil {
				t.Fatal(err)
			}
			wanted := original
			wanted.Image, wanted.VCPUs = "/new", 1
			wanted.Volumes = append([]config.VolumeConfig(nil), original.Volumes...)
			wanted.Volumes[0].SizeBytes *= 2
			desired := config.NodeConfig{Services: []config.ServiceConfig{wanted}}
			if err := a.reconciler.Reconcile(context.Background(), desired); !errors.Is(err, reconciler.ErrServiceHeld) {
				t.Fatalf("expected retryable refusal: %v", err)
			}
			if _, ok := monitor.GetResult(original.Name); !ok {
				t.Fatal("refusal deregistered health")
			}
			if rule, ok := network.kernel.Delivers(8080); !ok || rule.GuestIP != original.Network.GuestIP {
				t.Fatalf("refusal lost original forward: %#v", rule)
			}
			if err := a.syncTraefikConfigs(context.Background(), desired.Services); err != nil {
				t.Fatal(err)
			}
			requirePublishedRoute(t, routeDir, "original.example.com", "172.16.0.3:80")
			if hold := a.reconciler.CapacityHold(config.ServiceConfig{Name: "other", VCPUs: 1}); hold.Reason == "" {
				t.Fatal("refused CPU shrink freed capacity")
			}
			if err := a.restartService(context.Background(), original.Name); err != nil {
				t.Fatalf("ordinary health restart blocked by resize refusal: %v", err)
			}
			actual := underlying.instances[original.Name].Config
			if manager.attempts != 1 || actual.Image != original.Image || actual.VCPUs != original.VCPUs || actual.Volumes[0].SizeBytes != original.Volumes[0].SizeBytes {
				t.Fatalf("health restart used pending config: attempts=%d actual=%#v", manager.attempts, actual)
			}
			if a.reconciler.HeldServiceHolds(desired)[original.Name].ApplicationExited {
				t.Fatal("old exit observation applied to restarted VM")
			}
			if err := a.syncTraefikConfigs(context.Background(), desired.Services); err != nil {
				t.Fatal(err)
			}
			requirePublishedRoute(t, routeDir, "original.example.com", "172.16.0.3:80")
		})
	}
}
