package vm

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestRecoveryAbsenceDistinguishesClaimsFromUnavailableInspections(t *testing.T) {
	for _, tc := range []struct {
		name     string
		prepare  func(*Manager, *instanceManifest, *fakeProcessInspector)
		claimant bool
		cause    error
	}{
		{"recorded live process", func(*Manager, *instanceManifest, *fakeProcessInspector) {}, true, nil},
		{"process using the VM paths", func(_ *Manager, f *instanceManifest, _ *fakeProcessInspector) { f.PID = 0 }, true, nil},
		{"queued systemd launch", func(m *Manager, f *instanceManifest, _ *fakeProcessInspector) {
			f.PID, f.Launcher = 0, "systemd"
			m.recoveryUnitInspectorContext = func(context.Context, *instanceManifest) (int, bool, error) { return 0, false, nil }
		}, true, nil},
		{"recorded process inspection timed out", func(_ *Manager, f *instanceManifest, i *fakeProcessInspector) {
			i.errors[f.PID] = context.DeadlineExceeded
		}, false, context.DeadlineExceeded},
		{"process scan unreadable", func(_ *Manager, f *instanceManifest, i *fakeProcessInspector) {
			f.PID, i.findErr = 0, os.ErrPermission
		}, false, os.ErrPermission},
		{"unit inspection timed out", func(m *Manager, f *instanceManifest, _ *fakeProcessInspector) {
			f.PID, f.Launcher = 0, "systemd"
			m.recoveryUnitInspectorContext = func(context.Context, *instanceManifest) (int, bool, error) {
				return 0, false, context.DeadlineExceeded
			}
		}, false, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f, inspector := recoveryFixture(t)
			m.inspector = inspector
			tc.prepare(m, f, inspector)
			err := m.proveRecoveryAbsentContext(context.Background(), f)
			if err == nil || IsRecoveryClaimant(err) != tc.claimant {
				t.Fatalf("absence error=%v claimant=%v, want claimant=%v", err, IsRecoveryClaimant(err), tc.claimant)
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatalf("inspection cause was lost: %v", err)
			}
		})
	}
}

// The agent receives the wrapped command error, so its retry policy must
// still distinguish a found claimant from unavailable evidence for both
// trusted and corrupt quarantines.
func TestForgetPreservesAbsenceEvidenceInItsError(t *testing.T) {
	for _, untrusted := range []bool{false, true} {
		for _, unavailable := range []bool{false, true} {
			name := "trusted"
			if untrusted {
				name = "untrusted"
			}
			if unavailable {
				name += "/inspection timed out"
			} else {
				name += "/live claimant"
			}
			t.Run(name, func(t *testing.T) {
				m, f, inspector := recoveryFixture(t)
				m.inspector = inspector
				if untrusted {
					if err := os.WriteFile(manifestPath(f.VMDir), []byte("{corrupt"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if unavailable {
					delete(inspector.identities, f.PID)
					inspector.findErr = context.DeadlineExceeded
				}
				if err := m.DiscoverRecoveryState(); err != nil {
					t.Fatal(err)
				}
				result, err := m.ForgetRecoveryPending(f.Service)
				if err == nil || result.Outcome != RecoveryOutcomeAmbiguous || IsRecoveryClaimant(err) != !unavailable {
					t.Fatalf("forget=%+v err=%v claimant=%v", result, err, IsRecoveryClaimant(err))
				}
				if unavailable && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("the command lost the inspection cause: %v", err)
				}
				if _, err := os.Stat(f.VMDir); err != nil {
					t.Fatalf("unproved absence authorized cleanup: %v", err)
				}
				if inst := m.List()[f.Service]; inst == nil || inst.State != StateRecoveryPending {
					t.Fatalf("unproved absence released quarantine: %+v", inst)
				}
			})
		}
	}
}
