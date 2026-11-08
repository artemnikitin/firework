package vm

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

func stoppedRestartIntentFixture(t *testing.T) (*Manager, *instanceManifest, *fakeProcessInspector) {
	t.Helper()
	m, f, inspector := recoveryFixture(t)
	m.inspector = inspector
	m.instances[f.Service] = instanceFromManifest(cloneInstanceManifest(f), StateRunning, "")
	delete(inspector.identities, f.PID)
	if err := m.Stop(f.Service); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.VMDir, firecrackerLogName), []byte("previous launch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return m, f, inspector
}

// C3/C5: reclaiming a stopped VM must record its intent before deleting its
// manifest. A crash before volume preparation, or failed preparation in the
// same process, leaves an explained operation that recovery can finish.
func TestInterruptedRestartReclaimRecoversWithoutForget(t *testing.T) {
	for _, phase := range []string{"agent restart", "volume preparation fails"} {
		t.Run(phase, func(t *testing.T) {
			m, f, inspector := stoppedRestartIntentFixture(t)
			svc := f.Config
			if phase == "agent restart" {
				if _, _, _, err := m.beginStartContext(context.Background(), svc); err != nil {
					t.Fatal(err)
				}
			} else {
				// A missing storage dependency fails after reclaim and before
				// finishStart, through Start's real preparation-failure path.
				svc.Volumes = []config.VolumeConfig{{Name: "data", SizeBytes: 1024}}
				if err := m.Start(context.Background(), svc); err == nil {
					t.Fatal("setup: volume preparation unexpectedly succeeded")
				}
			}
			if _, err := os.Stat(manifestPath(f.VMDir)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("setup: old manifest was not reclaimed: %v", err)
			}
			recovering := m
			if phase == "agent restart" {
				recovering = NewManager(m.firecrackerBin, m.stateDir, m.logger)
				recovering.inspector = inspector
				stopAdoptedMonitors(t, recovering)
			}
			for pass := 0; pass < 3; pass++ {
				if _, err := recovering.Recover(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{svc}}); err != nil {
					t.Fatal(err)
				}
			}
			if inst := recovering.List()[f.Service]; inst != nil {
				t.Fatalf("the interrupted restart requires --forget: %+v", inst)
			}
			if _, err := os.Stat(f.VMDir); !errors.Is(err, os.ErrNotExist) || recovering.hasIntent(f.Service) {
				t.Fatalf("the interrupted operation was not finished: dir=%v intent=%v", err, recovering.hasIntent(f.Service))
			}
			// The unchanged desired service is eligible to start again.
			id, _, _, err := recovering.beginStartContext(context.Background(), svc)
			if err != nil {
				t.Fatalf("restart remains held after recovery: %v", err)
			}
			recovering.discardStart(svc.Name, id)
		})
	}
}

// C1/C3: if the write-ahead intent cannot be saved, the old trusted manifest
// remains intact. Once the write becomes available, recovery clears it.
func TestRestartIntentWriteFailurePreservesTheOldManifest(t *testing.T) {
	m, f, _ := stoppedRestartIntentFixture(t)
	before, err := os.ReadFile(manifestPath(f.VMDir))
	if err != nil {
		t.Fatal(err)
	}
	// A directory at the marker's file path makes its atomic rename fail.
	if err := os.MkdirAll(m.intentPath(f.Service), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := m.beginStartContext(context.Background(), f.Config); err == nil {
		t.Fatal("reclaim proceeded without recording its intent")
	}
	after, err := os.ReadFile(manifestPath(f.VMDir))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("failed intent write removed or changed the old manifest: %v", err)
	}
	if err := os.Remove(m.intentPath(f.Service)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if m.List()[f.Service] != nil {
		t.Fatal("recovery did not resume once the marker could be written")
	}
}
