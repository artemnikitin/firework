package agent

import (
	"context"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/vm"
)

// A health restart does not wait for a whole rolling update: the tick
// releases the lifecycle lock while the rollout pauses between updates.
func TestHealthRestartRunsDuringRollingPause(t *testing.T) {
	yaml := "node: web\nservices:\n- name: a\n  image: /new\n  kernel: /k\n- name: b\n  image: /new\n  kernel: /k\n"
	a := New(testAgentConfig(t), &fakeStore{revision: "r1", data: map[string][]byte{"web": []byte(yaml)}}, testLogger())
	manager := &recoveryTestVMManager{instances: map[string]*vm.Instance{
		"a": {Name: "a", Config: config.ServiceConfig{Name: "a", Image: "/old", Kernel: "/k"}, State: vm.StateRunning},
		"b": {Name: "b", Config: config.ServiceConfig{Name: "b", Image: "/old", Kernel: "/k"}, State: vm.StateRunning},
	}}
	a.reconciler = reconciler.NewWithNetworkManager(manager, testLogger(), nil, &recoveryTestNetworkManager{}, "rolling", 2*time.Second)
	done := make(chan struct{})
	go func() {
		a.tick(context.Background())
		close(done)
	}()
	time.Sleep(500 * time.Millisecond) // the first update is done; the rollout pauses
	restarted := make(chan error, 1)
	go func() { restarted <- a.restartService(context.Background(), "a") }()
	select {
	case <-restarted:
	case <-done:
		t.Fatal("the rollout finished before the restart ran")
	case <-time.After(time.Second):
		t.Fatal("health restart waited for the rolling pause")
	}
	<-done
}
