package vm

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

// C2: startup recovery, its automatic retry on a later tick and the
// clear-recovery command classify the same ownership evidence the same way.
// Proven absence permits cleanup, a revalidated live owner is adopted, and
// missing identity or a possible live claimant stays quarantined.
func TestC2SameEvidenceSameClassificationAtEveryEntryPoint(t *testing.T) {
	evidence := map[string]struct {
		apply func(*testing.T, *instanceManifest, *fakeProcessInspector)
		want  string
	}{
		"proven absent": {func(_ *testing.T, f *instanceManifest, i *fakeProcessInspector) { delete(i.identities, f.PID) }, "absent"},
		"live owner":    {func(*testing.T, *instanceManifest, *fakeProcessInspector) {}, "adopted"},
		"missing identity": {func(t *testing.T, f *instanceManifest, i *fakeProcessInspector) {
			f.HostBootID, f.ProcessStart, f.ExecutableIno = "", 0, 0
			if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
				t.Fatal(err)
			}
		}, "quarantined"},
		"dead with possible claimant": {func(_ *testing.T, f *instanceManifest, i *fakeProcessInspector) {
			delete(i.identities, f.PID)
			i.find = []processIdentity{{PID: 77}}
		}, "quarantined"},
	}
	for name, ev := range evidence {
		for _, entry := range []string{"startup", "retry", "command"} {
			t.Run(name+"/"+entry, func(t *testing.T) {
				m, f, i := recoveryFixture(t)
				m.inspector = i
				if entry != "startup" {
					// A transient inspection failure quarantines the VM first.
					i.errors[f.PID] = errors.New("temporary process inspection failure")
					if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
						t.Fatal(err)
					}
					if got := m.List()[f.Service]; got == nil || got.State != StateRecoveryPending {
						t.Fatalf("setup did not quarantine: %#v", got)
					}
					delete(i.errors, f.PID)
				}
				ev.apply(t, f, i)
				var err error
				switch entry {
				case "startup", "retry":
					_, err = m.Recover(context.Background(), config.NodeConfig{})
				case "command":
					_, err = m.ClearRecoveryPending(f.Service)
				}
				got := "absent"
				switch instance := m.List()[f.Service]; {
				case instance == nil:
				case instance.State == StateRecoveryPending:
					got = "quarantined"
				case instance.State == StateRunning:
					got = "adopted"
					stopAdoptedMonitors(t, m)
				default:
					got = "left " + string(instance.State)
				}
				if got != ev.want {
					t.Fatalf("classified %s, want %s (err=%v)", got, ev.want, err)
				}
				_, statErr := os.Stat(f.VMDir)
				switch ev.want {
				case "quarantined":
					if statErr != nil {
						t.Fatalf("quarantine lost its state: %v", statErr)
					}
					// The command refuses, so the operator learns why.
					if entry == "command" && err == nil {
						t.Fatal("command reported success for an unproven owner")
					}
				case "absent":
					// Proven absence lets every entry point finish cleanup.
					if err != nil || !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("absent VM not cleaned up: err=%v state=%v", err, statErr)
					}
				case "adopted":
					if err != nil || statErr != nil {
						t.Fatalf("adoption: err=%v state=%v", err, statErr)
					}
				}
			})
		}
	}
}
