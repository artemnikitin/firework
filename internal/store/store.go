package store

import (
	"context"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

// Store is the interface for a central configuration store.
// The agent pulls desired state through this interface.
type Store interface {
	// Fetch retrieves the latest node configuration (raw YAML) for the given
	// node name. It returns the raw bytes so the caller can parse them.
	Fetch(ctx context.Context, nodeName string) ([]byte, error)

	// Revision returns an opaque string representing the current version of
	// the config. The agent uses this to detect changes without re-parsing.
	Revision(ctx context.Context) (string, error)

	// Close releases any resources held by the store.
	Close() error
}

// EnrichmentTimestampProvider is an optional interface implemented by stores
// that can report when an enriched node config was last produced.
//
// For S3-backed stores this maps to the object LastModified timestamp.
type EnrichmentTimestampProvider interface {
	LastEnrichmentTimestamp(nodeName string) (time.Time, bool)
}

// NodeConfigLister is implemented by stores that can enumerate all node configs.
// The agent uses this to discover peer-node services for cross-node Traefik routing.
type NodeConfigLister interface {
	ListAllNodeConfigs(ctx context.Context) ([]config.NodeConfig, error)
}
