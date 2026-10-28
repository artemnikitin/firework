package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/artemnikitin/firework/internal/objectstorage"
)

type ignoringConditionalDeleteStore struct {
	StateStore
}

func (s ignoringConditionalDeleteStore) DeleteIfMatch(ctx context.Context, key string, _ objectstorage.WriteToken) (bool, error) {
	if err := s.StateStore.Delete(ctx, key); err != nil {
		return false, err
	}
	return true, nil
}

func TestProbeConditionalDelete(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	if err := probeConditionalDelete(ctx, store, "cp/v1"); err != nil {
		t.Fatalf("probe supported store: %v", err)
	}
	keys, err := store.ListKeys(ctx, "cp/v1/registry/conditional-delete-probes/")
	if err != nil || len(keys) != 0 {
		t.Fatalf("probe objects left behind: %v, err %v", keys, err)
	}
}

func TestProbeConditionalDeleteRejectsIgnoredPrecondition(t *testing.T) {
	ctx := context.Background()
	store := newBlobStateStore(newMemBlob())
	err := probeConditionalDelete(ctx, ignoringConditionalDeleteStore{StateStore: store}, "cp/v1")
	if !errors.Is(err, errConditionalDeleteUnsupported) {
		t.Fatalf("probe error = %v, want unsupported", err)
	}
	keys, listErr := store.ListKeys(ctx, "cp/v1/registry/conditional-delete-probes/")
	if listErr != nil || len(keys) != 0 {
		t.Fatalf("probe objects left behind: %v, err %v", keys, listErr)
	}
}
