package agent

import (
	"context"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/vm"
)

// Status computes holds from the same prepared configuration Reconcile uses,
// so an unchanged running service with environment variables is not reported
// held while unknown claims fence new allocations.
func TestStatusHoldsUsePreparedConfiguration(t *testing.T) {
	yaml := "node: web\nservices:\n- name: app\n  image: /image\n  kernel: /kernel\n  env:\n    FOO: bar\n"
	a := New(testAgentConfig(t), &fakeStore{revision: "r2", data: map[string][]byte{"web": []byte(yaml)}}, testLogger())
	running := config.ServiceConfig{Name: "app", Image: "/image", Kernel: "/kernel", Env: map[string]string{"FOO": "bar"}, KernelArgs: "firework.env.FOO=bar"}
	manager := &recoveryTestVMManager{instances: map[string]*vm.Instance{"app": {Name: "app", Config: running, State: vm.StateRunning}}}
	holdRoutingReplacement(manager)
	a.reconciler = reconciler.NewWithNetworkManager(manager, testLogger(), nil, &recoveryTestNetworkManager{}, "", 0)
	for tick := 0; tick < 2; tick++ {
		a.tick(context.Background())
		for _, service := range a.agentStatusSnapshot().Services {
			if service.Name == "app" && service.ReasonCode == "resource_held" {
				t.Fatalf("tick %d reports unchanged app held: %s", tick, service.Message)
			}
		}
	}
}
