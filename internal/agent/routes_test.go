package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/traefik"
	"github.com/artemnikitin/firework/internal/vm"
)

func TestRecoveryHeldServicesNeverPublishAnotherGuestsRoute(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "known-IP-conflict", true: "unknown-auto-assignment"}[unknown], func(t *testing.T) {
			a := retainedResourceAgent(t)
			if unknown {
				// A restart cannot reconstruct the live survivor's IP from corrupt
				// metadata. It must not expose an unstarted service at that IP.
				if err := os.WriteFile(filepath.Join(a.cfg.StateDir, "vms", "removed", "instance.json"), []byte("{invalid"), 0600); err != nil {
					t.Fatal(err)
				}
				a = New(a.cfg, &fakeStore{}, testLogger())
			} else {
				a.cfg.VMSubnet = "" // Explicit network assignments remain supported.
			}
			networkYAML := "network: {}"
			if !unknown {
				networkYAML = "network:\n    guest_ip: 172.16.0.2"
			}
			a.store = &fakeStore{revision: "new", data: map[string][]byte{"web": []byte("node: web\nservices:\n- name: new-tenant\n  image: /image\n  kernel: /kernel\n  vcpus: 1\n  memory_mb: 64\n  " + networkYAML + "\n  port_forwards:\n  - host_port: 8080\n    vm_port: 80\n  metadata:\n    host: new-tenant.example.com\n")}}
			routeDir := t.TempDir()
			a.traefikMgr = traefik.NewManager(routeDir, "", testLogger())
			for tick := 0; tick < 2; tick++ {
				a.tick(context.Background())
				if a.vmManager.List()["new-tenant"] != nil {
					t.Fatal("held service unexpectedly started")
				}
				if _, err := os.Stat(filepath.Join(routeDir, "new-tenant.yaml")); !os.IsNotExist(err) {
					t.Fatalf("held service published a route to its retained or unknown guest: %v", err)
				}
			}
		})
	}
}

func TestRuntimeRoutesKeepHeldRunningBackendAndHostnameUntilReplacement(t *testing.T) {
	a := New(testAgentConfig(t), &fakeStore{}, testLogger())
	old := config.ServiceConfig{Name: "app", Image: "/old", Network: &config.NetworkConfig{GuestIP: "172.16.0.3"}, PortForwards: []config.PortForward{{HostPort: 8080, VMPort: 80}}, Metadata: map[string]string{"host": "old.example.com"}}
	manager := &recoveryTestVMManager{instances: map[string]*vm.Instance{
		old.Name:  {Name: old.Name, Config: old, State: vm.StateRunning},
		"unknown": {Name: "unknown", Config: config.ServiceConfig{Name: "unknown"}, State: vm.StateRecoveryPending, RecoveryConfigUnknown: true},
	}}
	a.reconciler = reconciler.New(manager, testLogger(), nil, nil, "", 0)
	routeDir := t.TempDir()
	a.traefikMgr = traefik.NewManager(routeDir, "", testLogger())
	assertRoute := func(desired []config.ServiceConfig, host, backend string) {
		t.Helper()
		if err := a.syncTraefikConfigs(context.Background(), desired); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(routeDir, "app.yaml"))
		if err != nil || !strings.Contains(string(data), "Host(`"+host+"`)") || !strings.Contains(string(data), "url: http://"+backend) {
			t.Fatalf("unexpected runtime route: %s (read error %v)", data, err)
		}
	}
	assertRoute([]config.ServiceConfig{old}, "old.example.com", "172.16.0.3:80")
	desired := old
	desired.Image = "/new"
	desired.Network = &config.NetworkConfig{GuestIP: "172.16.0.2"}
	desired.PortForwards = []config.PortForward{{HostPort: 9090, VMPort: 90}}
	desired.Metadata = map[string]string{"host": "new.example.com"}
	assertRoute([]config.ServiceConfig{desired}, "old.example.com", "172.16.0.3:80")
	delete(manager.instances, "unknown")
	manager.instances[old.Name].Config = desired
	assertRoute([]config.ServiceConfig{desired}, "new.example.com", "172.16.0.2:90")
	if err := a.syncTraefikConfigs(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(routeDir, "app.yaml")); !os.IsNotExist(err) {
		t.Fatalf("removed service retained its route: %v", err)
	}
}

func TestRuntimeRoutesApplyUnheldHostnameAndHealthPortWithoutVMReplacement(t *testing.T) {
	a := New(testAgentConfig(t), &fakeStore{}, testLogger())
	old := config.ServiceConfig{Name: "app", Network: &config.NetworkConfig{GuestIP: "172.16.0.3"}, HealthCheck: &config.HealthCheckConfig{Port: 80}, Metadata: map[string]string{"host": "old.example.com"}}
	manager := &recoveryTestVMManager{instances: map[string]*vm.Instance{old.Name: {Name: old.Name, Config: old, State: vm.StateRunning}}}
	a.reconciler = reconciler.New(manager, testLogger(), nil, nil, "", 0)
	routeDir := t.TempDir()
	a.traefikMgr = traefik.NewManager(routeDir, "", testLogger())
	desired := old
	desired.Metadata = map[string]string{"host": "new.example.com"}
	desired.HealthCheck = &config.HealthCheckConfig{Port: 90}
	for _, held := range []bool{true, false} {
		if held {
			a.reconciler.SetHeldServices(map[string]reconciler.Hold{old.Name: {Reason: "capacity retained by another VM", Recovery: true}})
		} else {
			a.reconciler.SetHeldServices(nil)
		}
		if err := a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{desired}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(routeDir, "app.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		host, backend := "new.example.com", "172.16.0.3:90"
		if held {
			host, backend = "old.example.com", "172.16.0.3:80"
		}
		if !strings.Contains(string(data), "Host(`"+host+"`)") || !strings.Contains(string(data), "url: http://"+backend) {
			t.Fatalf("held=%v: unexpected route after metadata change without replacing the VM: %s", held, data)
		}
	}
}

func TestUnchangedRevisionDoesNotRouteAbsentOrNonRunningGuests(t *testing.T) {
	for _, state := range []vm.State{"absent", vm.StateStarting, vm.StateStartAborting, vm.StateStopping, vm.StateStopped, vm.StateFailed, vm.StateRecoveryPending} {
		t.Run(string(state), func(t *testing.T) {
			store := &fakeStore{revision: "same", data: map[string][]byte{"web": []byte("node: web\nservices:\n- name: app\n  image: /image\n  kernel: /kernel\n  network:\n    guest_ip: 172.16.0.2\n  port_forwards:\n  - host_port: 8080\n    vm_port: 80\n  metadata:\n    host: app.example.com\n")}}
			a := New(testAgentConfig(t), store, testLogger())
			manager := &recoveryTestVMManager{instances: map[string]*vm.Instance{}}
			if state != "absent" {
				manager.instances["app"] = &vm.Instance{Name: "app", State: state, Config: config.ServiceConfig{Name: "app", Network: &config.NetworkConfig{GuestIP: "172.16.0.2"}}}
			}
			a.reconciler = reconciler.New(manager, testLogger(), nil, nil, "", 0)
			routeDir := t.TempDir()
			a.traefikMgr = traefik.NewManager(routeDir, "", testLogger())
			a.lastRevision = "same"
			a.tick(context.Background())
			if _, err := os.Stat(filepath.Join(routeDir, "app.yaml")); !os.IsNotExist(err) {
				t.Fatalf("non-running service published its desired route: %v", err)
			}
		})
	}
}

type blockingRouteStore struct {
	*fakeStore
	entered chan struct{}
	release chan struct{}
}

func (s *blockingRouteStore) ListAllNodeConfigs(context.Context) ([]config.NodeConfig, error) {
	close(s.entered)
	<-s.release
	return nil, s.listErr
}

func TestRoutePublicationSnapshotsRuntimeAfterPeerDiscovery(t *testing.T) {
	for _, peersUnavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "full-sync", true: "local-sync"}[peersUnavailable], func(t *testing.T) {
			store := &blockingRouteStore{fakeStore: &fakeStore{}, entered: make(chan struct{}), release: make(chan struct{})}
			if peersUnavailable {
				store.listErr = os.ErrPermission
			}
			a := New(testAgentConfig(t), store, testLogger())
			service := config.ServiceConfig{Name: "app", Network: &config.NetworkConfig{GuestIP: "172.16.0.2"}, PortForwards: []config.PortForward{{HostPort: 8080, VMPort: 80}}, Metadata: map[string]string{"host": "app.example.com"}}
			manager := &recoveryTestVMManager{instances: map[string]*vm.Instance{service.Name: {Name: service.Name, Config: service, State: vm.StateRunning}}}
			a.reconciler = reconciler.New(manager, testLogger(), nil, nil, "", 0)
			routeDir := t.TempDir()
			a.traefikMgr = traefik.NewManager(routeDir, "", testLogger())
			done := make(chan error, 1)
			go func() { done <- a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{service}) }()
			<-store.entered
			// A health restart can change runtime state while peer discovery is
			// blocked. Take the same lifecycle lock that protects Stop/Start.
			changed := make(chan struct{})
			go func() {
				a.lifecycleMu.Lock()
				manager.instances[service.Name].State = vm.StateRecoveryPending
				a.lifecycleMu.Unlock()
				close(changed)
			}()
			select {
			case <-changed:
			case <-time.After(time.Second):
				close(store.release)
				<-done
				t.Fatal("peer discovery blocked a lifecycle transition")
			}
			close(store.release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(routeDir, "app.yaml")); !os.IsNotExist(err) {
				t.Fatalf("published a runtime snapshot taken before peer discovery: %v", err)
			}
		})
	}
}
