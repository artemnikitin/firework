package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

func newRevision(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().UTC().UnixNano(), hex.EncodeToString(b))
}

func branchFromRef(ref string) string {
	const p = "refs/heads/"
	if strings.HasPrefix(ref, p) {
		return ref[len(p):]
	}
	return ref
}

func upsertPointer(ctx context.Context, store *S3StateStore, key, rev string) error {
	ptr := RevisionPointer{
		Revision:  rev,
		UpdatedAt: time.Now().UTC(),
	}

	for i := 0; i < 6; i++ {
		var current RevisionPointer
		etag, exists, err := store.GetJSON(ctx, key, &current)
		if err != nil {
			return err
		}
		if !exists {
			ok, _, err := store.PutJSONIfAbsent(ctx, key, ptr)
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
			continue
		}

		ok, _, err := store.PutJSONIfMatch(ctx, key, etag, ptr)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return fmt.Errorf("pointer update conflict for %s", key)
}
