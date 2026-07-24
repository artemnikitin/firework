package controlplane

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

func TestVolumeRecordsRetainBindingAndAdvanceResizeGeneration(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	controller := NewController(Config{State: StateConfig{Prefix: "cp/v1/"}}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now().UTC()
	record := VolumeRecord{
		LogicalID: "db/data", Type: config.VolumeTypeLocal, BoundNode: "node-1",
		DesiredSizeBytes: 10 * config.GiB, AppliedSizeBytes: 10 * config.GiB,
		ResizeGeneration: 1, ResizeState: VolumeResizeApplied, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := store.PutJSON(ctx, mustVolumeRecordKey("cp/v1/", "db", "data"), record); err != nil {
		t.Fatal(err)
	}
	records, err := controller.loadVolumeRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	services := []config.ServiceConfig{{Name: "db", Volumes: []config.VolumeConfig{{
		Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data", SizeBytes: 20 * config.GiB,
	}}}}
	if err := controller.applyExistingVolumeRecords(ctx, services, records); err != nil {
		t.Fatal(err)
	}
	volume := services[0].Volumes[0]
	if volume.BoundNode != "node-1" || volume.ResizeGeneration != 2 {
		t.Fatalf("resolved volume = %#v", volume)
	}
	stored := records["db/data"].Record
	if stored.DesiredSizeBytes != 20*config.GiB || stored.AppliedSizeBytes != 10*config.GiB || stored.ResizeState != VolumeResizePending {
		t.Fatalf("stored volume = %#v", stored)
	}
	reservations := storageReservations(records)
	if got := reservations.LocalByNode["node-1"]; got != 20*config.GiB {
		t.Fatalf("reservation = %d", got)
	}
}

func TestCreateAssignedVolumeRecordUsesSchedulerBinding(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	controller := NewController(Config{State: StateConfig{Prefix: "cp/v1/"}}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	records := make(map[string]storedVolumeRecord)
	nodes := []config.NodeConfig{{Node: "node-1", Services: []config.ServiceConfig{{Name: "db", Volumes: []config.VolumeConfig{{
		Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data", SizeBytes: config.GiB, BoundNode: "node-1", ResizeGeneration: 1,
	}}}}}}
	if err := controller.createAssignedVolumeRecords(ctx, nodes, records); err != nil {
		t.Fatal(err)
	}
	if got := records["db/data"].Record; got.BoundNode != "node-1" || got.ResizeState != VolumeResizePending {
		t.Fatalf("created record = %#v", got)
	}
}

func TestAcknowledgeLocalVolumeRequiresMatchingNodeIdentityAndSize(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	controller := NewController(Config{State: StateConfig{Prefix: "cp/v1/"}}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	recordKey := mustVolumeRecordKey("cp/v1/", "db", "data")
	record := VolumeRecord{
		LogicalID: "db/data", Type: config.VolumeTypeLocal, BoundNode: "node-1",
		DesiredSizeBytes: config.GiB, ResizeGeneration: 1, ResizeState: VolumeResizePending,
	}
	if _, err := store.PutJSON(ctx, recordKey, record); err != nil {
		t.Fatal(err)
	}

	writeNode := func(nodeID, boundNode string, size int64, state, lastError string) {
		t.Helper()
		key, err := nodeRecordKey("cp/v1/", nodeID)
		if err != nil {
			t.Fatal(err)
		}
		node := NodeRecord{NodeID: nodeID, AgentStatus: &statusmodel.AgentStatus{Services: []statusmodel.ServiceStatus{{
			Name: "db", Volumes: []statusmodel.VolumeStatus{{
				LogicalID: "db/data", Type: "local", BoundNode: boundNode,
				AppliedSizeBytes: size, ResizeGeneration: 1, State: state, LastError: lastError,
			}},
		}}}}
		if _, err := store.PutJSON(ctx, key, node); err != nil {
			t.Fatal(err)
		}
	}
	readRecord := func() VolumeRecord {
		t.Helper()
		var got VolumeRecord
		if _, exists, err := store.GetJSON(ctx, recordKey, &got); err != nil || !exists {
			t.Fatalf("read volume record: exists=%v err=%v", exists, err)
		}
		return got
	}

	writeNode("node-2", "node-1", config.GiB, "prepared", "")
	if err := controller.acknowledgeVolumeRecords(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readRecord(); got.ResizeState != VolumeResizePending || got.AppliedSizeBytes != 0 {
		t.Fatalf("wrong node acknowledged local volume: %#v", got)
	}

	writeNode("node-1", "node-1", config.GiB/2, "prepared", "")
	if err := controller.acknowledgeVolumeRecords(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readRecord(); got.ResizeState != VolumeResizePending || got.AppliedSizeBytes != 0 {
		t.Fatalf("wrong applied size acknowledged local volume: %#v", got)
	}

	writeNode("node-1", "node-1", 0, "error", "resize failed")
	if err := controller.acknowledgeVolumeRecords(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readRecord(); got.ResizeState != VolumeResizeFailed || got.LastError != "resize failed" || got.AppliedSizeBytes != 0 {
		t.Fatalf("volume failure was not retained: %#v", got)
	}

	writeNode("node-1", "node-1", config.GiB, "prepared", "")
	if err := controller.acknowledgeVolumeRecords(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readRecord(); got.ResizeState != VolumeResizeApplied || got.AppliedSizeBytes != config.GiB || got.LastError != "" {
		t.Fatalf("matching node did not acknowledge local volume: %#v", got)
	}
}
