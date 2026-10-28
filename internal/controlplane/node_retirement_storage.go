package controlplane

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"time"
)

var errConditionalDeleteUnsupported = errors.New("conditional delete is not honored by the configured object store")

// probeConditionalDelete checks that a custom object-store endpoint compares
// exact provider tokens for deletion. The probe writes only to a fresh key
// under this state's prefix and removes it after checking stale and current
// tokens.
func probeConditionalDelete(ctx context.Context, store StateStore, prefix string) error {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Errorf("generate conditional-delete probe key: %w", err)
	}
	key := path.Join(stateRoot(prefix), "registry", "conditional-delete-probes", hex.EncodeToString(random[:])+".json")
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = store.Delete(cleanupCtx, key)
	}()

	firstBody := []byte("firework conditional-delete probe v1")
	created, staleToken, err := store.PutRawIfAbsent(ctx, key, firstBody, "application/octet-stream")
	if err != nil {
		return fmt.Errorf("create conditional-delete probe: %w", err)
	}
	if !created || staleToken == "" {
		return fmt.Errorf("create conditional-delete probe: unique key unexpectedly existed")
	}
	currentBody := []byte("firework conditional-delete probe v2")
	currentToken, err := store.PutRaw(ctx, key, currentBody, "application/octet-stream")
	if err != nil {
		return fmt.Errorf("replace conditional-delete probe: %w", err)
	}
	if currentToken == "" || currentToken == staleToken {
		return fmt.Errorf("%w: replacement did not produce a distinct provider token", errConditionalDeleteUnsupported)
	}

	deleted, err := store.DeleteIfMatch(ctx, key, staleToken)
	if err != nil {
		return fmt.Errorf("check stale conditional-delete token: %w", err)
	}
	if deleted {
		return fmt.Errorf("%w: stale token deleted the probe object", errConditionalDeleteUnsupported)
	}
	body, observedToken, exists, err := store.GetRaw(ctx, key)
	if err != nil {
		return fmt.Errorf("read conditional-delete probe after stale token: %w", err)
	}
	if !exists || observedToken != currentToken || !bytes.Equal(body, currentBody) {
		return fmt.Errorf("%w: stale token changed the probe object", errConditionalDeleteUnsupported)
	}

	deleted, err = store.DeleteIfMatch(ctx, key, currentToken)
	if err != nil {
		return fmt.Errorf("check current conditional-delete token: %w", err)
	}
	if !deleted {
		return fmt.Errorf("%w: current token did not delete the probe object", errConditionalDeleteUnsupported)
	}
	_, _, exists, err = store.GetRaw(ctx, key)
	if err != nil {
		return fmt.Errorf("verify conditional-delete probe removal: %w", err)
	}
	if exists {
		return fmt.Errorf("%w: probe object remains after matching delete", errConditionalDeleteUnsupported)
	}
	return nil
}
