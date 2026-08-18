package agent

import (
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/statusmodel"
	"github.com/artemnikitin/firework/internal/volume"
)

// TestBuildVolumeStatusesBoundsMountPath exercises the real send-side status
// path (BuildVolumeStatuses), not a hand-built VolumeStatus. A config store
// not run through the enricher's mount_path length check (internal/enricher's
// new bound, or an older stored config predating it) could still hand the
// agent an oversized mount_path; without truncation here, the resulting
// heartbeat fails controlplane's validateVolumeStatus outright and the node
// loses its entire agent_status, not just this one field.
func TestBuildVolumeStatusesBoundsMountPath(t *testing.T) {
	longPath := "/data/" + strings.Repeat("x", statusmodel.MaxMountPathLen*2)
	service := config.ServiceConfig{
		Name: "web",
		Volumes: []config.VolumeConfig{
			{Name: "data", Type: config.VolumeTypeLocal, MountPath: longPath},
		},
	}

	statuses := BuildVolumeStatuses(service, map[string]volume.PreparedVolume{})
	if len(statuses) != 1 {
		t.Fatalf("got %d volume statuses, want 1", len(statuses))
	}
	got := statuses[0].MountPath
	if len(got) > statusmodel.MaxMountPathLen {
		t.Fatalf("mount_path is %d bytes, exceeding MaxMountPathLen (%d): sent unbounded to the registry",
			len(got), statusmodel.MaxMountPathLen)
	}
	if want := statusmodel.BoundedPath(longPath); got != want {
		t.Fatalf("mount_path = %q, want the BoundedPath truncation %q", got, want)
	}
}

// TestBuildVolumeStatusesPreservesNormalMountPath guards against
// over-truncation: an ordinary mount_path well under the bound must round
// through unchanged.
func TestBuildVolumeStatusesPreservesNormalMountPath(t *testing.T) {
	service := config.ServiceConfig{
		Name: "web",
		Volumes: []config.VolumeConfig{
			{Name: "data", Type: config.VolumeTypeLocal, MountPath: "/data/es"},
		},
	}
	statuses := BuildVolumeStatuses(service, map[string]volume.PreparedVolume{})
	if len(statuses) != 1 || statuses[0].MountPath != "/data/es" {
		t.Fatalf("got %#v, want mount_path /data/es unchanged", statuses)
	}
}
