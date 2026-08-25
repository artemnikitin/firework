package volume

import (
	"context"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

// The snapshot is maintained by two passes with different jobs, and a standing
// refusal must survive every path through them: rebuildRejections decides
// against the raw config once per tick, refreshRejections only ever adds a
// refusal it just discovered or drops one the manifest no longer has.
func TestSnapshotSurvivesEveryUpdatePath(t *testing.T) {
	manager, _ := hardeningManager(t, &fakeRunner{})
	if _, err := manager.Prepare(context.Background(), localService(16*config.MiB, 1)); err != nil {
		t.Fatal(err)
	}

	// A brand-new refusal must be reported in the very tick it happens, from
	// the pre-clamp config the preflight sees.
	raw := localService(2*config.MiB, 2)
	if _, err := manager.Preflight(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	if len(manager.Rejections()) != 1 {
		t.Fatalf("a fresh refusal must be reported in the same tick: %#v", manager.Rejections())
	}

	// A later tick: rebuild sees the raw config and keeps it, then Prepare
	// runs against the clamped config and must not drop it.
	services := []config.ServiceConfig{localService(2*config.MiB, 2)}
	manager.NormalizeVolumes(services)
	if len(manager.Rejections()) != 1 {
		t.Fatalf("rebuild dropped a standing refusal: %#v", manager.Rejections())
	}
	if _, err := manager.Prepare(context.Background(), services[0]); err != nil {
		t.Fatal(err)
	}
	if len(manager.Rejections()) != 1 {
		t.Fatalf("the clamped-config pass dropped a standing refusal: %#v", manager.Rejections())
	}

	// A resize that actually applies clears it in the same tick, not the next.
	if _, err := manager.Prepare(context.Background(), localService(8*config.MiB, 3)); err != nil {
		t.Fatal(err)
	}
	if len(manager.Rejections()) != 0 {
		t.Fatalf("an applied resize must clear the refusal immediately: %#v", manager.Rejections())
	}
}
