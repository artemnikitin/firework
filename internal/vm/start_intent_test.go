package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
)

// C1/C2/L4: Start must apply the same intent requirement when it discovers
// manifestless state after the tick's earlier recovery inventory.
func TestStartRequiresIntentForManifestlessState(t *testing.T) {
	for _, own := range []bool{false, true} {
		for _, live := range []bool{false, true} {
			t.Run(fmt.Sprintf("intent=%v/live=%v", own, live), func(t *testing.T) {
				m, f, inspector := recoveryFixture(t)
				m.inspector = inspector
				if !live {
					delete(inspector.identities, f.PID)
				}
				if err := os.Remove(manifestPath(f.VMDir)); err != nil {
					t.Fatal(err)
				}
				if own {
					if err := m.markIntent(f.Service); err != nil {
						t.Fatal(err)
					}
				}
				id, _, _, err := m.beginStartContext(context.Background(), f.Config)
				if own && !live {
					if err != nil {
						t.Fatal(err)
					}
					m.discardStart(f.Service, id)
					return
				}
				if err == nil {
					t.Fatal("Start cleared unexplained or potentially live VM state")
				}
				if _, err := os.Stat(f.VMDir); err != nil {
					t.Fatalf("the refused start removed runtime state: %v", err)
				}
				if inst := m.List()[f.Service]; inst == nil || inst.State != StateRecoveryPending {
					t.Fatalf("refused state was not fenced: %+v", inst)
				}
				if !own && !live {
					if result, err := m.ClearRecoveryPending(f.Service); err == nil || result.Outcome != RecoveryOutcomeAmbiguous {
						t.Fatalf("plain clear lost the untrusted fence: %+v %v", result, err)
					}
					if result, err := m.ForgetRecoveryPending(f.Service); err != nil || result.Outcome != RecoveryOutcomeCleared {
						t.Fatalf("absence-proven forget did not clear state: %+v %v", result, err)
					}
					if _, err := os.Stat(f.VMDir); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("forgotten state remains: %v", err)
					}
				}
			})
		}
	}
}
