package controlplane

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/objectstorage"
	"gopkg.in/yaml.v3"
)

const emptyStatePrefix = "cp/v1/"

func emptyStateController(t *testing.T) (*Controller, StateStore) {
	t.Helper()
	store := newBlobStateStore(newMemBlob())
	c := NewController(Config{
		State:          StateConfig{Prefix: emptyStatePrefix},
		LeaderLeaseTTL: time.Minute,
		NodeStaleTTL:   time.Minute,
	}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.renewLeadership(context.Background()); err != nil || !c.leader {
		t.Fatalf("controller did not acquire leadership: leader=%v err=%v", c.leader, err)
	}
	return c, store
}

func putDesired(t *testing.T, store StateStore, revision string, services ...config.ServiceConfig) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.PutJSON(ctx, desiredRevisionKey(emptyStatePrefix, revision), DesiredRevision{Revision: revision, Services: services}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(ctx, desiredCurrentKey(emptyStatePrefix), RevisionPointer{Revision: revision}); err != nil {
		t.Fatal(err)
	}
}

func putEmptyStateNode(t *testing.T, store StateStore, nodeID string, state NodeState, lastSeen time.Time) {
	t.Helper()
	key, err := nodeRecordKey(emptyStatePrefix, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(context.Background(), key, NodeRecord{
		NodeID:     nodeID,
		State:      state,
		Capacity:   Resources{VCPUs: 4, MemoryMB: 4096},
		LastSeenAt: lastSeen,
	}); err != nil {
		t.Fatal(err)
	}
}

func putNodeConfig(t *testing.T, store StateStore, nodeID string, data []byte) {
	t.Helper()
	if _, err := store.PutRaw(context.Background(), legacyNodeConfigKey(emptyStatePrefix, nodeID), data, "application/x-yaml"); err != nil {
		t.Fatal(err)
	}
}

func nodeConfigYAML(t *testing.T, nodeID string, services ...string) []byte {
	t.Helper()
	nc := config.NodeConfig{Node: nodeID, Services: []config.ServiceConfig{}}
	for _, name := range services {
		nc.Services = append(nc.Services, testService(name))
	}
	data, err := yaml.Marshal(nc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testService(name string) config.ServiceConfig {
	return config.ServiceConfig{Name: name, Image: "/img/" + name, Kernel: "/kernel", VCPUs: 1, MemoryMB: 256}
}

func readNodeConfig(t *testing.T, store StateStore, nodeID string) ([]byte, bool) {
	t.Helper()
	data, _, exists, err := store.GetRaw(context.Background(), legacyNodeConfigKey(emptyStatePrefix, nodeID))
	if err != nil {
		t.Fatal(err)
	}
	return data, exists
}

func currentPlacement(t *testing.T, store StateStore) PlacementRevision {
	t.Helper()
	ctx := context.Background()
	var ptr RevisionPointer
	if _, exists, err := store.GetJSON(ctx, placementCurrentKey(emptyStatePrefix), &ptr); err != nil || !exists {
		t.Fatalf("no placement published: exists=%v err=%v", exists, err)
	}
	var rev PlacementRevision
	if _, exists, err := store.GetJSON(ctx, placementRevisionKey(emptyStatePrefix, ptr.Revision), &rev); err != nil || !exists {
		t.Fatalf("placement revision unreadable: exists=%v err=%v", exists, err)
	}
	return rev
}

func requireExplicitEmpty(t *testing.T, store StateStore, nodeID string, placement PlacementRevision) {
	t.Helper()
	data, exists := readNodeConfig(t, store, nodeID)
	if !exists {
		t.Fatalf("%s config was deleted instead of emptied", nodeID)
	}
	nc, err := config.ParseNodeConfig(data)
	if err != nil {
		t.Fatalf("%s tombstone is not an authoritative config: %v\n%s", nodeID, err, data)
	}
	if nc.Node != nodeID || len(nc.Services) != 0 {
		t.Fatalf("%s tombstone is not empty: %#v", nodeID, nc)
	}
	if nc.DesiredRevision != placement.DesiredRevision || nc.PlacementRevision != placement.Revision || nc.RenderedRevision == "" {
		t.Fatalf("%s tombstone carries no usable revision metadata: %#v", nodeID, nc)
	}
}

// Moving the last service off a node that left scheduling (here: draining)
// must leave that node an explicit empty config. Deleting it made the agent
// treat the missing file as a fetch failure and keep the old copy running
// beside the new one indefinitely.
func TestInactiveNodeLosingItsLastServiceIsTombstoned(t *testing.T) {
	c, store := emptyStateController(t)
	now := time.Now().UTC()
	putDesired(t, store, "desired-1", testService("web"))
	putEmptyStateNode(t, store, "node-a", NodeStateReady, now)
	putEmptyStateNode(t, store, "node-b", NodeStateDraining, now)
	putNodeConfig(t, store, "node-b", nodeConfigYAML(t, "node-b", "web"))

	c.runReconcile(context.Background())

	placement := currentPlacement(t, store)
	data, _ := readNodeConfig(t, store, "node-a")
	if nc, err := config.ParseNodeConfig(data); err != nil || len(nc.Services) != 1 || nc.Services[0].Name != "web" {
		t.Fatalf("web was not moved to node-a: %#v err=%v", nc, err)
	}
	requireExplicitEmpty(t, store, "node-b", placement)
	// The fleet view's relevant set comes from the placement. A node that may
	// never return must not be added there, or no revision could converge
	// while it stays down.
	for _, nc := range placement.NodeConfigs {
		if nc.Node == "node-b" {
			t.Fatalf("inactive node was added to the placement: %#v", placement.NodeConfigs)
		}
	}
	// The immutable rendered revision holds the same tombstone its metadata names.
	rendered, _, exists, err := store.GetRaw(context.Background(), renderedNodeKey(emptyStatePrefix, placement.NodeConfigs[0].RenderedRevision, "node-b"))
	if err != nil || !exists {
		t.Fatalf("tombstone missing from rendered revision: exists=%v err=%v", exists, err)
	}
	current, _ := readNodeConfig(t, store, "node-b")
	if !bytes.Equal(rendered, current) {
		t.Fatalf("rendered tombstone differs from current:\n%s\nvs\n%s", rendered, current)
	}
}

// When every node goes stale at once — a registry outage — nothing is
// schedulable and every desired service is pending. Tombstoning every known
// node would turn the outage into a fleet-wide wipe, so a config that still
// holds a desired, unplaced service is left exactly as it was. A config whose
// services were all removed from desired state is still emptied.
func TestStaleNodesKeepConfigsHoldingUnplacedServices(t *testing.T) {
	c, store := emptyStateController(t)
	stale := time.Now().UTC().Add(-time.Hour)
	putDesired(t, store, "desired-1", testService("web"))
	putEmptyStateNode(t, store, "node-a", NodeStateReady, stale)
	putEmptyStateNode(t, store, "node-b", NodeStateReady, stale)
	webConfig := nodeConfigYAML(t, "node-a", "web")
	putNodeConfig(t, store, "node-a", webConfig)
	putNodeConfig(t, store, "node-b", nodeConfigYAML(t, "node-b", "removed"))
	// node-c's config was deleted by an older controller; it may be running
	// the only copy of web.
	putEmptyStateNode(t, store, "node-c", NodeStateReady, stale)

	c.runReconcile(context.Background())

	placement := currentPlacement(t, store)
	if len(placement.PendingServices) != 1 || placement.PendingServices[0].Service != "web" {
		t.Fatalf("expected web to be pending with no schedulable node: %#v", placement.PendingServices)
	}
	if got, _ := readNodeConfig(t, store, "node-a"); !bytes.Equal(got, webConfig) {
		t.Fatalf("config holding an unplaced desired service was rewritten:\n%s", got)
	}
	requireExplicitEmpty(t, store, "node-b", placement)
	if got, exists := readNodeConfig(t, store, "node-c"); exists {
		t.Fatalf("missing config on a stale node was emptied:\n%s", got)
	}
}

// Unknown contents are not evidence that a node may be emptied, and a key
// without a registry record is not the controller's to rewrite or delete.
// Missing is unknown too: an older controller deleted the config of a node
// that left the placement while its agent kept running the services, so an
// absent config can describe a node still running the only copies.
func TestUnknownNodeConfigsAreLeftUntouched(t *testing.T) {
	c, store := emptyStateController(t)
	now := time.Now().UTC()
	putDesired(t, store, "desired-1", testService("web"))
	putEmptyStateNode(t, store, "node-a", NodeStateReady, now)
	putEmptyStateNode(t, store, "node-corrupt", NodeStateDown, now)
	putEmptyStateNode(t, store, "node-missing", NodeStateDraining, now)
	corrupt := []byte("{not yaml")
	putNodeConfig(t, store, "node-corrupt", corrupt)
	orphan := nodeConfigYAML(t, "node-orphan", "legacy")
	putNodeConfig(t, store, "node-orphan", orphan)

	c.runReconcile(context.Background())

	currentPlacement(t, store)
	if got, _ := readNodeConfig(t, store, "node-corrupt"); !bytes.Equal(got, corrupt) {
		t.Fatalf("unparseable config was rewritten:\n%s", got)
	}
	if got, exists := readNodeConfig(t, store, "node-orphan"); !exists || !bytes.Equal(got, orphan) {
		t.Fatalf("config with no registry record was changed or deleted: exists=%v\n%s", exists, got)
	}
	if got, exists := readNodeConfig(t, store, "node-missing"); exists {
		t.Fatalf("missing config was treated as empty and written:\n%s", got)
	}
}

// A newly registered node with no assignment, under a desired revision with no
// services at all, converges on an explicit empty config, and repeating the
// reconcile stays empty.
func TestZeroServiceRevisionRendersExplicitEmptyConfigs(t *testing.T) {
	c, store := emptyStateController(t)
	now := time.Now().UTC()
	putDesired(t, store, "desired-1")
	putEmptyStateNode(t, store, "node-a", NodeStateReady, now)
	putEmptyStateNode(t, store, "node-b", NodeStateDown, now)
	putNodeConfig(t, store, "node-b", nodeConfigYAML(t, "node-b", "web"))

	for pass := 0; pass < 2; pass++ {
		c.lastInputSignature = ""
		c.runReconcile(context.Background())
		placement := currentPlacement(t, store)
		requireExplicitEmpty(t, store, "node-a", placement)
		requireExplicitEmpty(t, store, "node-b", placement)
	}
}

func TestTombstoneIsNotWrittenOverAConcurrentChange(t *testing.T) {
	c, store := emptyStateController(t)
	ctx := context.Background()
	putNodeConfig(t, store, "node-b", nodeConfigYAML(t, "node-b", "web"))
	tombstones, deferred := c.inactiveNodeTombstones(ctx, []string{"node-b"}, nil, nil)
	if len(tombstones) != 1 || deferred {
		t.Fatalf("expected node-b to be tombstoned, got %#v", tombstones)
	}
	changed := nodeConfigYAML(t, "node-b", "web", "api")
	putNodeConfig(t, store, "node-b", changed)

	written, err := c.publishTombstone(ctx, "rendered-1", tombstones[0])
	if err != nil {
		t.Fatal(err)
	}
	if written {
		t.Fatal("a lost write conflict was reported as written")
	}
	if got, _ := readNodeConfig(t, store, "node-b"); !bytes.Equal(got, changed) {
		t.Fatalf("tombstone overwrote a config written after it was evaluated:\n%s", got)
	}
	// The immutable revision must not record an empty config that was never applied.
	if _, _, exists, err := store.GetRaw(ctx, renderedNodeKey(emptyStatePrefix, "rendered-1", "node-b")); err != nil || exists {
		t.Fatalf("rendered revision recorded an unapplied tombstone: exists=%v err=%v", exists, err)
	}
}

// failingGetStore fails GetRaw for one key a set number of times.
type failingGetStore struct {
	StateStore
	key   string
	fails int
}

func (s *failingGetStore) GetRaw(ctx context.Context, key string) ([]byte, objectstorage.WriteToken, bool, error) {
	if key == s.key && s.fails > 0 {
		s.fails--
		return nil, "", false, errors.New("transient read failure")
	}
	return s.StateStore.GetRaw(ctx, key)
}

// A transient failure reading an inactive node's config defers its tombstone.
// The controller caches a cycle by its scheduling inputs, which do not change
// while that node stays down, so caching the deferred cycle would skip every
// later one and leave the node's old workloads running indefinitely.
func TestDeferredTombstoneIsRetriedOnTheNextCycle(t *testing.T) {
	c, store := emptyStateController(t)
	now := time.Now().UTC()
	putDesired(t, store, "desired-1", testService("web"))
	putEmptyStateNode(t, store, "node-a", NodeStateReady, now)
	putEmptyStateNode(t, store, "node-b", NodeStateDown, now)
	original := nodeConfigYAML(t, "node-b", "web")
	putNodeConfig(t, store, "node-b", original)
	c.store = &failingGetStore{StateStore: store, key: legacyNodeConfigKey(emptyStatePrefix, "node-b"), fails: 1}

	c.runReconcile(context.Background())
	if got, _ := readNodeConfig(t, store, "node-b"); !bytes.Equal(got, original) {
		t.Fatalf("config was rewritten despite the failed read:\n%s", got)
	}

	c.runReconcile(context.Background())
	requireExplicitEmpty(t, store, "node-b", currentPlacement(t, store))

	// With nothing deferred the cycle is cached again.
	if c.lastInputSignature == "" {
		t.Fatal("a fully applied cycle was not cached")
	}
}

// failingRecordStore fails GetJSON for one key a set number of times; a
// negative count fails it on every read.
type failingRecordStore struct {
	StateStore
	key   string
	fails int
}

func (s *failingRecordStore) GetJSON(ctx context.Context, key string, out any) (objectstorage.WriteToken, bool, error) {
	if key == s.key && s.fails != 0 {
		if s.fails > 0 {
			s.fails--
		}
		return "", false, errors.New("transient registry read failure")
	}
	return s.StateStore.GetJSON(ctx, key, out)
}

func (s *failingRecordStore) GetRaw(ctx context.Context, key string) ([]byte, objectstorage.WriteToken, bool, error) {
	if key == s.key && s.fails != 0 {
		if s.fails > 0 {
			s.fails--
		}
		return nil, "", false, errors.New("transient registry read failure")
	}
	return s.StateStore.GetRaw(ctx, key)
}

func placementPointer(t *testing.T, store StateStore) string {
	t.Helper()
	var ptr RevisionPointer
	if _, _, err := store.GetJSON(context.Background(), placementCurrentKey(emptyStatePrefix), &ptr); err != nil {
		t.Fatal(err)
	}
	return ptr.Revision
}

// An inactive node whose registry record cannot be read drops out of
// discovery without changing the active set. If that cycle were cached under
// a signature that ignores the known set, recovering the read would leave the
// signature unchanged, and the node's tombstone would never be evaluated.
func TestRegistryReadFailureDoesNotStrandRetirement(t *testing.T) {
	c, store := emptyStateController(t)
	now := time.Now().UTC()
	putDesired(t, store, "desired-1", testService("web"))
	putEmptyStateNode(t, store, "node-a", NodeStateReady, now)
	putEmptyStateNode(t, store, "node-b", NodeStateDown, now)
	original := nodeConfigYAML(t, "node-b", "web")
	putNodeConfig(t, store, "node-b", original)
	recordKey, err := nodeRecordKey(emptyStatePrefix, "node-b")
	if err != nil {
		t.Fatal(err)
	}
	// The quarantine maintenance scan reads before discovery, so fail both
	// reads in this cycle; the next cycle then exercises recovery.
	c.store = &failingRecordStore{StateStore: store, key: recordKey, fails: 2}

	c.runReconcile(context.Background())
	if got, _ := readNodeConfig(t, store, "node-b"); !bytes.Equal(got, original) {
		t.Fatalf("node with an unreadable registry record was rewritten:\n%s", got)
	}

	c.runReconcile(context.Background())
	requireExplicitEmpty(t, store, "node-b", currentPlacement(t, store))
}

// A record that stays unreadable must not defeat the cache: every uncached
// cycle publishes a new placement and rendered revision.
func TestPersistentRegistryReadFailureStillCaches(t *testing.T) {
	c, store := emptyStateController(t)
	now := time.Now().UTC()
	putDesired(t, store, "desired-1", testService("web"))
	putEmptyStateNode(t, store, "node-a", NodeStateReady, now)
	putEmptyStateNode(t, store, "node-b", NodeStateDown, now)
	recordKey, err := nodeRecordKey(emptyStatePrefix, "node-b")
	if err != nil {
		t.Fatal(err)
	}
	c.store = &failingRecordStore{StateStore: store, key: recordKey, fails: -1}

	c.runReconcile(context.Background())
	first := placementPointer(t, store)
	c.runReconcile(context.Background())
	if second := placementPointer(t, store); second != first {
		t.Fatalf("a persistently unreadable record republished placement: %s then %s", first, second)
	}
}

func putSizedNode(t *testing.T, store StateStore, nodeID string, vcpus int, lastSeen time.Time) {
	t.Helper()
	key, err := nodeRecordKey(emptyStatePrefix, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(context.Background(), key, NodeRecord{
		NodeID:     nodeID,
		State:      NodeStateReady,
		Capacity:   Resources{VCPUs: vcpus, MemoryMB: 4096},
		LastSeenAt: lastSeen,
	}); err != nil {
		t.Fatal(err)
	}
}

func nodeServices(t *testing.T, store StateStore, nodeID string) []string {
	t.Helper()
	data, exists := readNodeConfig(t, store, nodeID)
	if !exists {
		t.Fatalf("%s has no config", nodeID)
	}
	nc, err := config.ParseNodeConfig(data)
	if err != nil {
		t.Fatalf("%s config is not authoritative: %v", nodeID, err)
	}
	var names []string
	for _, svc := range nc.Services {
		names = append(names, svc.Name)
	}
	return names
}

// A deferred cycle publishes a new placement without completing it. If the
// cache kept the signature of the cycle before it, inputs returning to their
// earlier values — a stale node whose heartbeat recovers — would match that
// old signature and skip the cycle, leaving the service on both nodes.
func TestDeferredCycleDoesNotLeaveAnOlderSignatureCached(t *testing.T) {
	c, store := emptyStateController(t)
	ctx := context.Background()
	now := time.Now().UTC()
	putDesired(t, store, "desired-1", testService("web"))
	// node-a has more free vCPU, so web lands there first.
	putSizedNode(t, store, "node-a", 8, now)
	putSizedNode(t, store, "node-b", 4, now)

	c.runReconcile(ctx)
	if got := nodeServices(t, store, "node-a"); len(got) != 1 || got[0] != "web" {
		t.Fatalf("setup: web was not placed on node-a: %v", got)
	}
	cached := c.lastInputSignature
	if cached == "" {
		t.Fatal("setup: the first cycle was not cached")
	}

	// node-a goes stale: web moves to node-b, but reading node-a's config
	// fails, so its tombstone is deferred.
	putSizedNode(t, store, "node-a", 8, now.Add(-time.Hour))
	c.store = &failingGetStore{StateStore: store, key: legacyNodeConfigKey(emptyStatePrefix, "node-a"), fails: 1}
	c.runReconcile(ctx)
	if got := nodeServices(t, store, "node-b"); len(got) != 1 || got[0] != "web" {
		t.Fatalf("web was not moved to node-b: %v", got)
	}
	if got := nodeServices(t, store, "node-a"); len(got) != 1 || got[0] != "web" {
		t.Fatalf("setup: node-a's config changed despite the failed read: %v", got)
	}

	// node-a's heartbeat recovers before the next tick, restoring the inputs
	// the first cycle was cached under.
	putSizedNode(t, store, "node-a", 8, time.Now().UTC())
	c.runReconcile(ctx)

	if got := nodeServices(t, store, "node-a"); len(got) != 0 {
		t.Fatalf("node-a still runs %v beside node-b's copy; the cycle was skipped under signature %s", got, cached)
	}
	if got := nodeServices(t, store, "node-b"); len(got) != 1 || got[0] != "web" {
		t.Fatalf("web left node-b: %v", got)
	}
}
