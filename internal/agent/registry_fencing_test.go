package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/agentconfig"
	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/registryapi"
	"github.com/artemnikitin/firework/internal/statusmodel"
	"github.com/artemnikitin/firework/internal/vm"
)

// fakeRegistry answers each path with a scripted status and records calls.
type fakeRegistry struct {
	mu        sync.Mutex
	calls     []string
	responses map[string]int
	states    []registryapi.StateRequest
}

func (f *fakeRegistry) post(_ context.Context, path string, payload, _ any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, path)
	if state, ok := payload.(registryapi.StateRequest); ok {
		f.states = append(f.states, state)
	}
	if status := f.responses[path]; status != 0 && status != http.StatusOK {
		return &requestError{StatusCode: status}
	}
	return nil
}

func (f *fakeRegistry) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, call := range f.calls {
		if call == path {
			n++
		}
	}
	return n
}

func newFencingTestClient(registry *fakeRegistry, generation int64) *registryClient {
	c := newRegistryClient(agentconfig.AgentConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)), generation)
	c.certExpiry = time.Now().Add(24 * time.Hour)
	c.post = registry.post
	return c
}

func syncN(c *registryClient, n int) {
	for i := 0; i < n; i++ {
		c.sync(context.Background(), "node-1", nil, capacity.NodeCapacity{}, capacity.NodeCapacity{}, nil)
	}
}

const (
	registerPath  = "/v1/nodes/register"
	heartbeatPath = "/v1/nodes/heartbeat"
	statePath     = "/v1/nodes/node-1/state"
)

// A replaced process gets 409 on heartbeat, makes one registration attempt
// that also conflicts, and then stops all registry traffic — including its
// shutdown state update — instead of retrying forever.
func TestReplacedAgentFencesAfterOneRegistrationAttempt(t *testing.T) {
	registry := &fakeRegistry{responses: map[string]int{}}
	c := newFencingTestClient(registry, 100)
	syncN(c, 1)
	if !c.isRegistered() {
		t.Fatal("first sync did not register")
	}

	registry.responses[heartbeatPath] = http.StatusConflict
	registry.responses[registerPath] = http.StatusConflict
	syncN(c, 10)

	if !c.isFenced() {
		t.Fatal("client is not fenced after a registration conflict")
	}
	if got := registry.count(registerPath); got != 2 {
		t.Fatalf("register calls = %d, want initial + exactly one retry", got)
	}
	if got := registry.count(heartbeatPath); got != 2 {
		t.Fatalf("heartbeat calls = %d, want 2 (success, then the conflicting one)", got)
	}
	c.markDown(context.Background(), "node-1")
	if got := registry.count(statePath); got != 0 {
		t.Fatalf("fenced client sent %d state updates", got)
	}
}

// A newer process whose record was lost (404) or that sees a different
// generation (409) re-registers once and carries on when that succeeds.
func TestHeartbeatRejectionReRegistersOnce(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusConflict} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			registry := &fakeRegistry{responses: map[string]int{}}
			c := newFencingTestClient(registry, 100)
			syncN(c, 1)
			registry.responses[heartbeatPath] = status
			syncN(c, 1)
			registry.responses[heartbeatPath] = http.StatusOK
			syncN(c, 3)
			if c.isFenced() || !c.isRegistered() {
				t.Fatalf("fenced=%v registered=%v, want an active registration", c.isFenced(), c.isRegistered())
			}
			if got := registry.count(registerPath); got != 2 {
				t.Fatalf("register calls = %d, want 2", got)
			}
		})
	}
}

func TestShutdownStateCarriesGeneration(t *testing.T) {
	registry := &fakeRegistry{responses: map[string]int{}}
	c := newFencingTestClient(registry, 4242)
	c.markDown(context.Background(), "node-1")
	if len(registry.states) != 1 || registry.states[0].Generation != 4242 || registry.states[0].State != registryapi.NodeStateDown {
		t.Fatalf("state requests = %+v", registry.states)
	}
}

// The heartbeat loop and shutdown run on different goroutines.
func TestFencingStateIsRaceFree(t *testing.T) {
	registry := &fakeRegistry{responses: map[string]int{heartbeatPath: http.StatusConflict, registerPath: http.StatusConflict}}
	c := newFencingTestClient(registry, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); syncN(c, 20) }()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			c.markDown(context.Background(), "node-1")
		}
	}()
	wg.Wait()
	if !c.isFenced() {
		t.Fatal("client did not fence")
	}
}

// A 422 means liveness was accepted and only telemetry refused, so the agent
// keeps its registration instead of re-registering every tick.
func TestRejectedTelemetryKeepsRegistration(t *testing.T) {
	registry := &fakeRegistry{responses: map[string]int{heartbeatPath: http.StatusUnprocessableEntity}}
	c := newFencingTestClient(registry, 1)
	syncN(c, 5)
	if got := registry.count(registerPath); got != 1 {
		t.Fatalf("register calls = %d, want 1", got)
	}
	if !c.isRegistered() || c.isFenced() {
		t.Fatalf("registered=%v fenced=%v", c.isRegistered(), c.isFenced())
	}
}

// An oversized heartbeat is resent without status so liveness still lands.
func TestOversizedHeartbeatIsResentWithoutStatus(t *testing.T) {
	var sent []bool
	registry := &fakeRegistry{responses: map[string]int{}}
	c := newFencingTestClient(registry, 1)
	c.post = func(ctx context.Context, path string, payload, out any) error {
		if hb, ok := payload.(registryapi.HeartbeatRequest); ok {
			sent = append(sent, hb.AgentStatus != nil)
			if hb.AgentStatus != nil {
				return &requestError{StatusCode: http.StatusRequestEntityTooLarge}
			}
		}
		return registry.post(ctx, path, payload, out)
	}
	c.sync(context.Background(), "node-1", nil, capacity.NodeCapacity{}, capacity.NodeCapacity{}, &statusmodel.AgentStatus{SchemaVersion: 1})
	if len(sent) != 2 || !sent[0] || sent[1] {
		t.Fatalf("heartbeats with status = %v, want [true false]", sent)
	}
	if !c.isRegistered() {
		t.Fatal("registration dropped after the status-free resend succeeded")
	}
}

// statusmodel mirrors the VM states because it must not import the runtime.
// Every state the VM manager can report must be accepted by the registry, or a
// real agent's whole status is rejected.
func TestStatusModelAcceptsEveryVMState(t *testing.T) {
	for _, state := range []vm.State{vm.StateRunning, vm.StateStopping, vm.StateStopped, vm.StateFailed, vm.StateRecoveryPending, vm.StateStarting, vm.StateStartAborting} {
		if !statusmodel.IsVMState(string(state)) {
			t.Errorf("statusmodel rejects vm state %q", state)
		}
	}
	if !statusmodel.IsVMState("unknown") {
		t.Error("statusmodel rejects the agent's default unknown vm state")
	}
}
