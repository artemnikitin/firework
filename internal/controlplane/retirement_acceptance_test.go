package controlplane

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/registryapi"
)

func TestRetiredNodeFencesEveryIncarnationThroughControllerPasses(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	c := retirementPublishingFixture(t, store)
	c.runReconcile(ctx)
	if _, exists := readNode(t, store, c.cfg, "node-1"); exists {
		t.Fatal("real controller passes did not retire node-1")
	}
	server, _ := newTestRegistry(t)
	server.store = store
	for _, generation := range []int64{7, 8} {
		for _, request := range []struct {
			path   string
			body   any
			code   int
			reason string
		}{
			{"/v1/nodes/register", register("node-1", generation), http.StatusConflict, registryapi.ErrorCodeNodeRetired},
			{"/v1/nodes/heartbeat", heartbeat("node-1", generation), http.StatusNotFound, registryapi.ErrorCodeNodeNotRegistered},
			{"/v1/nodes/node-1/state", registryapi.StateRequest{State: registryapi.NodeStateDown, Generation: generation}, http.StatusNotFound, registryapi.ErrorCodeNodeNotRegistered},
		} {
			code, reason := registryCall(t, server, "node-1", request.path, request.body)
			if code != request.code || reason != request.reason {
				t.Fatalf("generation %d %s = %d %q, want %d %q", generation, request.path, code, reason, request.code, request.reason)
			}
			if _, exists := readNode(t, store, c.cfg, "node-1"); exists {
				t.Fatalf("generation %d %s recreated a retired record", generation, request.path)
			}
		}
	}
}

func TestNewerRegistrationWinsPendingRetirementThroughControllerPasses(t *testing.T) {
	ctx := context.Background()
	store := &retirementStepFailureStore{StateStore: newBlobStateStore(newMemBlob()), operation: "create", key: mustNodeTombstoneKey(emptyStatePrefix, "node-1")}
	c := retirementPublishingFixture(t, store)
	store.armed = true
	c.runReconcile(ctx)
	before, exists := readNode(t, store, c.cfg, "node-1")
	if !store.failed || !exists || before.RetirementPendingAt.IsZero() {
		t.Fatalf("real pass did not reach pending boundary: %+v", before)
	}
	key := mustNodeRecordKey(c.cfg.State.Prefix, "node-1")
	saved, _, exists, err := store.GetRaw(ctx, key)
	if err != nil || !exists {
		t.Fatalf("read pending record: exists=%v err=%v", exists, err)
	}
	server, _ := newTestRegistry(t)
	server.store = store
	for _, generation := range []int64{6, 7} {
		want := registryapi.ErrorCodeNodeRetiring
		if generation < 7 {
			want = registryapi.ErrorCodeStaleGeneration
		}
		for _, request := range []struct {
			path string
			body any
		}{
			{"/v1/nodes/register", register("node-1", generation)},
			{"/v1/nodes/heartbeat", heartbeat("node-1", generation)},
			{"/v1/nodes/node-1/state", registryapi.StateRequest{State: registryapi.NodeStateDown, Generation: generation}},
		} {
			if code, reason := registryCall(t, server, "node-1", request.path, request.body); code != http.StatusConflict || reason != want {
				t.Fatalf("generation %d %s = %d %q, want 409 %q", generation, request.path, code, reason, want)
			}
		}
	}
	after, _, exists, err := store.GetRaw(ctx, key)
	if err != nil || !exists || !bytes.Equal(saved, after) {
		t.Fatalf("rejected mutations changed pending record: exists=%v err=%v", exists, err)
	}
	if code, reason := registryCall(t, server, "node-1", "/v1/nodes/register", register("node-1", 8)); code != http.StatusOK || reason != "" {
		t.Fatalf("newer registration: %d %s", code, reason)
	}
	for i := 0; i < 3; i++ {
		c.runReconcile(ctx)
	}
	if record, exists := readNode(t, store, c.cfg, "node-1"); !exists || record.Generation != 8 || record.State != NodeStateReady || !record.RetirementPendingAt.IsZero() {
		t.Fatalf("newer registration did not stay active: %+v exists=%v", record, exists)
	}
}

func TestRetirementPreservesEmptyConfigAndVolumeThroughControllerPasses(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	c := retirementPublishingFixture(t, store)
	volumeKey := mustVolumeRecordKey(c.cfg.State.Prefix, "removed", "data")
	if _, err := store.PutJSON(ctx, volumeKey, VolumeRecord{
		LogicalID: "removed/data", Type: config.VolumeTypeLocal, BoundNode: "node-1",
		DesiredSizeBytes: config.GiB, AppliedSizeBytes: config.GiB, ResizeGeneration: 1, ResizeState: VolumeResizeApplied,
	}); err != nil {
		t.Fatal(err)
	}
	volumeBefore, _, exists, err := store.GetRaw(ctx, volumeKey)
	if err != nil || !exists {
		t.Fatalf("read retained volume: exists=%v err=%v", exists, err)
	}
	configKey := legacyNodeConfigKey(c.cfg.State.Prefix, "node-1")
	configBefore, _, exists, err := store.GetRaw(ctx, configKey)
	if err != nil || !exists {
		t.Fatalf("read published empty config: exists=%v err=%v", exists, err)
	}
	for _, revision := range []string{"desired-1", "desired-2", "desired-3"} {
		putDesired(t, store, revision, testService("web"))
		c.runReconcile(ctx)
		if _, exists := readNode(t, store, c.cfg, "node-1"); exists {
			t.Fatal("node did not retire through publishing passes")
		}
		volumeAfter, _, exists, err := store.GetRaw(ctx, volumeKey)
		if err != nil || !exists || !bytes.Equal(volumeBefore, volumeAfter) {
			t.Fatalf("retirement or later publication changed retained volume: exists=%v err=%v", exists, err)
		}
		configAfter, _, exists, err := store.GetRaw(ctx, configKey)
		if err != nil || !exists || !bytes.Equal(configBefore, configAfter) {
			t.Fatalf("retirement or later publication changed empty config: exists=%v err=%v", exists, err)
		}
		if _, ok := readExplicitEmptyNodeConfig(ctx, store, c.cfg.State.Prefix, "node-1"); !ok {
			t.Fatal("empty document no longer matches its immutable copy")
		}
	}
}

func TestRetirementSwitchesProtectRecordsThroughControllerPasses(t *testing.T) {
	defaults := DefaultConfig()
	if defaults.NodeRetirementEnabled || !defaults.NodeRetirementDryRun {
		t.Fatal("retirement defaults must remain disabled and dry-run")
	}
	for _, tc := range []struct {
		name            string
		enabled, dryRun bool
	}{
		{"defaults", defaults.NodeRetirementEnabled, defaults.NodeRetirementDryRun},
		{"disabled with dry-run off", false, false},
		{"enabled with dry-run on", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := newBlobStateStore(newMemBlob())
			c := retirementTestController(t, store, tc.enabled, tc.dryRun)
			putDesired(t, store, "desired-1", testService("web"))
			putEmptyStateNode(t, store, "node-live", NodeStateReady, time.Now().UTC())
			putRetirementCandidate(t, store, c.cfg, NodeStateDown, 7, time.Now().UTC().Add(-3*time.Hour))
			putNodeConfig(t, store, "node-1", nodeConfigYAML(t, "node-1", "web"))
			for i := 0; i < 3; i++ {
				c.runReconcile(ctx)
			}
			if _, ok := readExplicitEmptyNodeConfig(ctx, store, c.cfg.State.Prefix, "node-1"); !ok {
				t.Fatal("test did not reach a proven empty config eligible for retirement")
			}
			if record, exists := readNode(t, store, c.cfg, "node-1"); !exists || !record.RetirementPendingAt.IsZero() {
				t.Fatalf("protected record was retired or marked pending: %+v exists=%v", record, exists)
			}
			for _, key := range []string{mustNodeTombstoneKey(c.cfg.State.Prefix, "node-1"), nodeTombstoneIndexKey(c.cfg.State.Prefix)} {
				if _, _, exists, err := store.GetRaw(ctx, key); err != nil || exists {
					t.Fatalf("protected pass wrote lifecycle metadata %s: exists=%v err=%v", key, exists, err)
				}
			}
		})
	}
}
