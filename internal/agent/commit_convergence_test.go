package agent

import (
	"context"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/vm"
)

type countingCommitManager struct {
	*recoveryTestVMManager
	commits []string
}

func (m *countingCommitManager) CommitConfig(name string, service config.ServiceConfig) error {
	m.commits = append(m.commits, name)
	return m.recoveryTestVMManager.CommitConfig(name, service)
}

// Pending recovery forces a full tick on every poll. An in-place change must
// be committed once, not rewrite the ownership manifest on every tick, for a
// service whose prepared config carries env, links, an address and a health
// check.
func TestInPlaceCommitConverges(t *testing.T) {
	yaml := "node: web\nservices:\n- name: api\n  image: /api\n  kernel: /k\n  network:\n    guest_ip: 172.16.0.2\n- name: app\n  image: /app\n  kernel: /k\n  env:\n    MODE: prod\n  links:\n  - service: api\n    env: API_URL\n    port: 80\n  network:\n    guest_ip: 172.16.0.3\n  port_forwards:\n  - host_port: 8080\n    vm_port: 80\n  health_check:\n    type: tcp\n    port: 80\n  metadata:\n    host: new.example.com\n"
	a := New(testAgentConfig(t), &fakeStore{revision: "r1", data: map[string][]byte{"web": []byte(yaml)}}, testLogger())
	parsed, err := config.ParseNodeConfig([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	a.prepareDesired(parsed.Services)
	manager := &countingCommitManager{recoveryTestVMManager: &recoveryTestVMManager{instances: map[string]*vm.Instance{}}}
	for _, service := range parsed.Services {
		running := service
		if service.Name == "app" {
			running.Metadata = map[string]string{"host": "old.example.com"}
		}
		manager.instances[service.Name] = &vm.Instance{Name: service.Name, Config: running, State: vm.StateRunning}
	}
	a.reconciler = reconciler.NewWithNetworkManager(manager, testLogger(), nil, &recoveryTestNetworkManager{}, "", 0)
	a.tick(context.Background())
	if len(manager.commits) != 1 || manager.commits[0] != "app" {
		t.Fatalf("first tick commits = %v, want app once", manager.commits)
	}
	for tick := 0; tick < 3; tick++ {
		a.forceReconcile = true
		a.tick(context.Background())
	}
	if len(manager.commits) != 1 {
		t.Fatalf("in-place commit did not converge: %v", manager.commits)
	}
	if len(manager.startCalls) != 0 || len(manager.removeCalls) != 0 {
		t.Fatalf("VMs replaced: starts=%v removes=%v", manager.startCalls, manager.removeCalls)
	}
}
