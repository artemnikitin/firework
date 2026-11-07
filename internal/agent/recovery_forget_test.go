package agent

import (
	"context"
	"testing"

	"github.com/artemnikitin/firework/internal/healthcheck"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/vm"
)

// A clear can complete on the control goroutine while a tick recreates the
// service. The queued ForgetRecovered must not deregister the new VM's health
// check, or the service runs unmonitored until its next update.
func TestForgetRecoveredKeepsRecreatedServiceHealthCheck(t *testing.T) {
	yaml := "node: web\nservices:\n- name: app\n  image: /image\n  kernel: /kernel\n  network:\n    guest_ip: 172.16.0.2\n  health_check:\n    type: tcp\n    port: 1\n    interval: 1h\n    timeout: 1s\n"
	a := New(testAgentConfig(t), &fakeStore{revision: "r1", data: map[string][]byte{"web": []byte(yaml)}}, testLogger())
	manager := &recoveryTestVMManager{instances: map[string]*vm.Instance{}}
	health := healthcheck.NewMonitor(testLogger(), func(context.Context, string) error { return nil })
	defer health.Stop()
	a.reconciler = reconciler.NewWithNetworkManager(manager, testLogger(), health, &recoveryTestNetworkManager{}, "", 0)
	a.queueRecoveryReconcile("app", false)
	a.tick(context.Background())
	if _, ok := health.GetResult("app"); !ok {
		t.Fatalf("setup: no health check after create; starts=%v", manager.startCalls)
	}
	a.applyRecoveryRequests(context.Background())
	a.tick(context.Background())
	if _, ok := health.GetResult("app"); !ok {
		t.Fatalf("recreated app lost its health check; starts=%v", manager.startCalls)
	}
}
