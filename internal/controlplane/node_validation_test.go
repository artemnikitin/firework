package controlplane

import (
	"context"
	"testing"
	"time"
)

func TestMalformedNodeIsolationDoesNotWriteQuarantine(t *testing.T) {
	for _, raw := range []string{
		`{`,
		`{"node_id":"other","generation":1,"state":"ready"}`,
		`{"node_id":"node-bad","generation":-1,"state":"ready"}`,
		`{"node_id":"node-bad","generation":1,"state":"surprise"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			ctx := context.Background()
			store := newBlobStateStore(newMemBlob())
			c := retirementTestController(t, store, false, true)
			putDesired(t, store, "desired-1", testService("web"))
			putEmptyStateNode(t, store, "node-live", NodeStateReady, time.Now().UTC())
			if _, err := store.PutRaw(ctx, mustNodeRecordKey(c.cfg.State.Prefix, "node-bad"), []byte(raw), "application/json"); err != nil {
				t.Fatal(err)
			}
			old := nodeConfigYAML(t, "node-bad", "web")
			putNodeConfig(t, store, "node-bad", old)
			c.runReconcile(ctx)
			if len(currentPlacement(t, store).NodeConfigs) != 1 {
				t.Fatal("healthy node did not schedule independently")
			}
			got, exists := readNodeConfig(t, store, "node-bad")
			if !exists || string(got) != string(old) {
				t.Fatal("malformed node's configuration was changed")
			}
			keys, err := store.ListKeys(ctx, stateRoot(c.cfg.State.Prefix)+"/quarantine/")
			if err != nil || len(keys) != 0 {
				t.Fatalf("unexpected quarantine objects: %v %v", keys, err)
			}
			nodes, err := NewVisibilityService(c.cfg, store).Nodes(ctx, "")
			if err != nil || len(nodes.Problems) != 1 || nodes.Problems[0].ID != "node-bad" {
				t.Fatalf("malformed node problem missing: %+v %v", nodes.Problems, err)
			}
		})
	}
}
