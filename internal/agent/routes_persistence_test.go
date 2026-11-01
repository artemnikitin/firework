package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/traefik"
	"github.com/artemnikitin/firework/internal/vm"
)

func routingPersistenceAgent(t *testing.T, routeDir string, service config.ServiceConfig, owner string) (*Agent, *recoveryTestVMManager) {
	t.Helper()
	a := New(testAgentConfig(t), &fakeStore{}, testLogger())
	manager := &recoveryTestVMManager{instances: map[string]*vm.Instance{
		service.Name: {Name: service.Name, Config: service, State: vm.StateRunning, InstanceID: owner},
	}}
	a.reconciler = reconciler.New(manager, testLogger(), nil, nil, "", 0)
	a.traefikMgr = traefik.NewManager(routeDir, "", testLogger())
	return a, manager
}

func routingPersistenceService() config.ServiceConfig {
	return config.ServiceConfig{
		Name:        "app",
		Image:       "/old",
		Network:     &config.NetworkConfig{GuestIP: "172.16.0.3"},
		HealthCheck: &config.HealthCheckConfig{Port: 80},
		Metadata:    map[string]string{"host": "original.example.com"},
	}
}

func holdRoutingReplacement(manager *recoveryTestVMManager) {
	manager.instances["unknown"] = &vm.Instance{
		Name: "unknown", Config: config.ServiceConfig{Name: "unknown"},
		State: vm.StateRecoveryPending, RecoveryConfigUnknown: true,
	}
}

func requirePublishedRoute(t *testing.T, routeDir, host, backend string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(routeDir, "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "Host(`"+host+"`)") || !strings.Contains(string(body), "url: http://"+backend) {
		t.Fatalf("expected host %s with backend %s; got:\n%s", host, backend, body)
	}
}

func requireNoPublishedRoute(t *testing.T, routeDir string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(routeDir, "app.yaml"))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "Host(") || strings.Contains(string(body), "url: http://") {
		t.Fatalf("expected no active route; got:\n%s", body)
	}
}

func TestUnchangedRevisionKeepsPublishedHostnameWithEnvironmentAndLinks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields string
	}{
		{name: "environment", fields: "  env:\n    MODE: prod\n"},
		{name: "link", fields: "  links:\n  - service: app\n    env: SELF_URL\n    port: 80\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldYAML := []byte("node: web\nservices:\n- name: app\n  image: /img\n  kernel: /kern\n  vcpus: 1\n  memory_mb: 64\n  network:\n    guest_ip: 172.16.0.3\n  port_forwards:\n  - host_port: 8080\n    vm_port: 80\n" + tc.fields + "  metadata:\n    host: original.example.com\n")
			parsed, err := config.ParseNodeConfig(oldYAML)
			if err != nil {
				t.Fatal(err)
			}
			routeDir := t.TempDir()
			a, manager := routingPersistenceAgent(t, routeDir, parsed.Services[0], "owner-old")
			a.resolveLinks(parsed.Services)
			a.injectEnvVars(parsed.Services)
			manager.instances["app"].Config = parsed.Services[0]
			a.store = &fakeStore{
				revision: "metadata-update",
				data:     map[string][]byte{"web": []byte(strings.ReplaceAll(string(oldYAML), "original.example.com", "current.example.com"))},
			}
			for tick := 0; tick < 3; tick++ {
				a.tick(context.Background())
				requirePublishedRoute(t, routeDir, "current.example.com", "172.16.0.3:80")
				if a.lastRevision != "metadata-update" {
					t.Fatalf("tick %d did not apply the metadata revision: %q", tick, a.lastRevision)
				}
			}
			if len(manager.startCalls) != 0 || len(manager.removeCalls) != 0 {
				t.Fatalf("metadata-only update replaced the VM: starts=%v removes=%v", manager.startCalls, manager.removeCalls)
			}
		})
	}
}

// commitInPlace runs reconciliation, which records in-place changes such as a
// hostname or health-check port on the running VM, as a full tick does.
func commitInPlace(t *testing.T, a *Agent, desired config.ServiceConfig) {
	t.Helper()
	if err := a.reconciler.Reconcile(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{desired}}); err != nil {
		t.Fatal(err)
	}
}

func TestCommittedRoutingMetadataSurvivesHeldReplacementAndAgentRestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			routeDir := t.TempDir()
			original := routingPersistenceService()
			a, manager := routingPersistenceAgent(t, routeDir, original, "owner-old")
			desired := original
			desired.Metadata = map[string]string{"host": "current.example.com"}
			desired.HealthCheck = &config.HealthCheckConfig{Port: 90}
			commitInPlace(t, a, desired)
			if len(manager.startCalls) != 0 || len(manager.removeCalls) != 0 {
				t.Fatalf("in-place change replaced the VM: starts=%v removes=%v", manager.startCalls, manager.removeCalls)
			}
			if err := a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{desired}); err != nil {
				t.Fatal(err)
			}
			requirePublishedRoute(t, routeDir, "current.example.com", "172.16.0.3:90")
			if restart {
				// Adoption reads the ownership manifest, which holds the
				// committed configuration.
				a, manager = routingPersistenceAgent(t, routeDir, manager.instances["app"].Config, "owner-old")
			}
			desired.Image = "/new"
			holdRoutingReplacement(manager)
			if err := a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{desired}); err != nil {
				t.Fatal(err)
			}
			requirePublishedRoute(t, routeDir, "current.example.com", "172.16.0.3:90")
		})
	}
}

func TestDisabledHostnameStaysDisabledDuringHeldReplacementAfterRestart(t *testing.T) {
	routeDir := t.TempDir()
	original := routingPersistenceService()
	a, manager := routingPersistenceAgent(t, routeDir, original, "owner-old")
	if err := a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{original}); err != nil {
		t.Fatal(err)
	}
	requirePublishedRoute(t, routeDir, "original.example.com", "172.16.0.3:80")
	desired := original
	desired.Metadata = nil
	commitInPlace(t, a, desired)
	if err := a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{desired}); err != nil {
		t.Fatal(err)
	}
	requireNoPublishedRoute(t, routeDir)
	a, manager = routingPersistenceAgent(t, routeDir, manager.instances["app"].Config, "owner-old")
	holdRoutingReplacement(manager)
	desired.Image = "/new"
	if err := a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{desired}); err != nil {
		t.Fatal(err)
	}
	requireNoPublishedRoute(t, routeDir)
}

// A replacement VM publishes from its own configuration, never from a file an
// earlier owner of the service left behind.
func TestReplacementRuntimePublishesItsOwnConfiguration(t *testing.T) {
	routeDir := t.TempDir()
	original := routingPersistenceService()
	a, _ := routingPersistenceAgent(t, routeDir, original, "owner-old")
	if err := a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{original}); err != nil {
		t.Fatal(err)
	}
	requirePublishedRoute(t, routeDir, "original.example.com", "172.16.0.3:80")
	replacement := original
	replacement.Image = "/replacement"
	replacement.Network = &config.NetworkConfig{GuestIP: "172.16.0.4"}
	a, manager := routingPersistenceAgent(t, routeDir, replacement, "owner-new")
	holdRoutingReplacement(manager)
	desired := original
	desired.Image = "/next"
	if err := a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{desired}); err != nil {
		t.Fatal(err)
	}
	requirePublishedRoute(t, routeDir, "original.example.com", "172.16.0.4:80")
}
