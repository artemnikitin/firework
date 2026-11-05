package vm

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

// C2: startup recovery, its automatic retry on a later tick and the
// clear-recovery command classify the same ownership evidence the same way.
// Proven absence permits cleanup, a revalidated live owner is adopted, and
// missing identity, a missing manifest or a possible live claimant stays
// quarantined.
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
		"no manifest, live process": {func(t *testing.T, f *instanceManifest, i *fakeProcessInspector) {
			if err := os.Remove(manifestPath(f.VMDir)); err != nil {
				t.Fatal(err)
			}
			i.find = []processIdentity{i.identities[f.PID]}
		}, "quarantined"},
		"no manifest, proven absent": {func(t *testing.T, f *instanceManifest, i *fakeProcessInspector) {
			if err := os.Remove(manifestPath(f.VMDir)); err != nil {
				t.Fatal(err)
			}
			delete(i.identities, f.PID)
		}, "absent"},
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

// C1 and the command contract: every refusal leaves the quarantined VM and
// its state untouched and tells the operator why, as "service S remains
// recovery_pending: <reason>", with the reason also in the result.
func TestC1RefusalsKeepTheVMAndSayWhy(t *testing.T) {
	cases := map[string]struct {
		apply  func(*testing.T, *Manager, *instanceManifest, *fakeProcessInspector)
		forget bool
		reason string
	}{
		"a live process still uses the VM paths": {apply: func(_ *testing.T, _ *Manager, f *instanceManifest, i *fakeProcessInspector) {
			delete(i.identities, f.PID)
			i.find = []processIdentity{{PID: 77}}
		}, reason: "absence remains unproved: found 1 live process(es) using this service's VM socket or config paths"},
		"the process table cannot be scanned": {apply: func(_ *testing.T, _ *Manager, f *instanceManifest, i *fakeProcessInspector) {
			delete(i.identities, f.PID)
			i.findErr = errors.New("host scan unavailable")
		}, reason: "cannot scan for a live process using this VM state: host scan unavailable"},
		"the durable identity changed": {apply: func(t *testing.T, _ *Manager, f *instanceManifest, _ *fakeProcessInspector) {
			f.HostBootID, f.ProcessStart, f.ExecutableIno = "", 0, 0
			if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
				t.Fatal(err)
			}
		}, reason: "in-memory and durable ownership identities do not match"},
		"the live process's API socket is not ready": {apply: func(_ *testing.T, _ *Manager, _ *instanceManifest, i *fakeProcessInspector) {
			i.socketErr = os.ErrNotExist
		}, reason: "surviving process API socket is not ready"},
		"forget of a VM not proven absent": {apply: func(*testing.T, *Manager, *instanceManifest, *fakeProcessInspector) {}, forget: true,
			reason: "cannot forget a VM whose absence is unproved"},
		"the ownership manifest is corrupt": {apply: func(t *testing.T, _ *Manager, f *instanceManifest, _ *fakeProcessInspector) {
			if err := os.WriteFile(manifestPath(f.VMDir), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, reason: "cannot read ownership manifest"},
		"another verification is in progress": {apply: func(_ *testing.T, m *Manager, f *instanceManifest, _ *fakeProcessInspector) {
			m.mu.Lock()
			if m.recoveryReservations == nil {
				m.recoveryReservations = map[string]bool{}
			}
			m.recoveryReservations[f.Service] = true
			m.mu.Unlock()
		}, reason: "recovery verification in progress"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m, f, i := recoveryFixture(t)
			m.inspector = i
			i.errors[f.PID] = errors.New("temporary process inspection failure")
			if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
				t.Fatal(err)
			}
			delete(i.errors, f.PID)
			tc.apply(t, m, f, i)
			resolve := m.ClearRecoveryPending
			if tc.forget {
				resolve = m.ForgetRecoveryPending
			}
			result, err := resolve(f.Service)
			if err == nil || result.Outcome != RecoveryOutcomeAmbiguous || result.State != StateRecoveryPending {
				t.Fatalf("not refused: result=%#v err=%v", result, err)
			}
			if !strings.Contains(result.Message, tc.reason) || err.Error() != "service app remains recovery_pending: "+result.Message {
				t.Fatalf("refusal does not say why: message=%q err=%q, want reason %q", result.Message, err, tc.reason)
			}
			if got := m.List()[f.Service]; got == nil || got.State != StateRecoveryPending {
				t.Fatalf("refusal changed the quarantine: %#v", got)
			}
			if _, err := os.Stat(manifestPath(f.VMDir)); err != nil {
				t.Fatalf("refusal removed VM state: %v", err)
			}
		})
	}
}

// C3: when the recovery deadline expires while an adoption is being
// persisted, the VM stays quarantined for a retry, and the reason says the
// deadline expired rather than that the process changed.
func TestAdoptionInterruptedByDeadlineSaysSoAndRetries(t *testing.T) {
	m, f, i := recoveryFixture(t)
	m.inspector = i
	i.errors[f.PID] = errors.New("temporary process inspection failure")
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	delete(i.errors, f.PID)
	ctx, cancel := context.WithCancel(context.Background())
	m.recoveryManifestWriter = func(path string, manifest *instanceManifest) error {
		cancel() // The deadline passes while the adoption record is written.
		return writeManifest(path, manifest)
	}
	result, err := m.resolveRecovery(ctx, f.Service)
	// C3: the adoption record was written, so the outcome must not claim that
	// nothing changed.
	if err == nil || result.Outcome != RecoveryOutcomeIncomplete || !strings.Contains(result.Message, "recovery deadline expired before the adopted process was revalidated") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	m.recoveryManifestWriter = nil
	if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	stopAdoptedMonitors(t, m)
	if got := m.List()[f.Service]; got == nil || got.State != StateRunning {
		t.Fatalf("interrupted adoption did not retry: %#v", got)
	}
}

// C3: once the adoption record has been written, every failure that follows
// reports an incomplete outcome, so the CLI never says that nothing changed
// or suggests stopping a VM it has just validated. The VM stays quarantined
// and a later retry adopts it.
func TestC3FailuresAfterTheAdoptionWriteAreIncomplete(t *testing.T) {
	for name, tc := range map[string]struct {
		duringWrite func(*Manager, *instanceManifest, *fakeProcessInspector)
		message     string
	}{
		"process changed": {
			func(_ *Manager, f *instanceManifest, i *fakeProcessInspector) {
				i.errors[f.PID] = errors.New("process inspection failed")
			},
			"process changed during adoption persistence",
		},
		"ownership changed": {
			func(m *Manager, f *instanceManifest, _ *fakeProcessInspector) {
				m.mu.Lock()
				replaced := *m.instances[f.Service]
				m.instances[f.Service] = &replaced
				m.mu.Unlock()
			},
			"ownership changed during adoption persistence",
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, f, i := recoveryFixture(t)
			m.inspector = i
			i.errors[f.PID] = errors.New("temporary process inspection failure")
			if _, err := m.Recover(context.Background(), config.NodeConfig{}); err != nil {
				t.Fatal(err)
			}
			delete(i.errors, f.PID)
			m.recoveryManifestWriter = func(path string, manifest *instanceManifest) error {
				tc.duringWrite(m, f, i)
				return writeManifest(path, manifest)
			}
			result, err := m.resolveRecovery(context.Background(), f.Service)
			if err == nil || result.Outcome != RecoveryOutcomeIncomplete || !strings.Contains(result.Message, tc.message) {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
}

// C1: VM state without an ownership manifest records no identity, so no
// process presenting its paths is adopted: not one whose argv[0] names the
// configured binary while its executable is a shell, and not the configured
// binary itself. Neither is signalled or loses its state.
func TestC1ManifestlessProcessIsNeverAdopted(t *testing.T) {
	for name, executable := range map[string]string{"shell claiming to be Firecracker": "/bin/sh", "configured Firecracker binary": ""} {
		t.Run(name, func(t *testing.T) {
			manager, inspector, identity, vmDir, service := manifestlessFixture(t)
			if executable != "" {
				identity.Executable = executable
				inspector.identities[identity.PID] = identity
				inspector.find = []processIdentity{identity}
			}
			adopted, err := manager.Recover(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{service}})
			if err != nil || len(adopted) != 0 {
				t.Fatalf("adopted=%v err=%v", adopted, err)
			}
			if got := manager.List()[service.Name]; got == nil || got.State != StateRecoveryPending || got.PID != 0 {
				t.Fatalf("manifest-less process was adopted: %#v", got)
			}
			if _, err := os.Stat(vmDir); err != nil {
				t.Fatalf("state of a possibly live VM was removed: %v", err)
			}
		})
	}
}

// C2: the same manifest-less evidence with a live process is quarantined
// whether startup recovery or the command meets it first.
func TestC2ManifestlessEvidenceIsQuarantinedAtStartupAndByCommand(t *testing.T) {
	commandFirst, _, _, _, service := manifestlessFixture(t)
	if err := commandFirst.DiscoverRecoveryState(); err != nil {
		t.Fatal(err)
	}
	result, err := commandFirst.ClearRecoveryPending(service.Name)
	if err == nil || result.Outcome != RecoveryOutcomeAmbiguous {
		t.Fatalf("command: %#v %v", result, err)
	}
	startupFirst, _, _, _, _ := manifestlessFixture(t)
	if _, err := startupFirst.Recover(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{service}}); err != nil {
		t.Fatal(err)
	}
	if got := startupFirst.List()[service.Name]; got == nil || got.State != StateRecoveryPending {
		t.Fatalf("startup classified the evidence differently from the command: %#v", got)
	}
}

// C1: the reconciler releases an interrupted start's address only after this
// proof. It refuses while the service has a tracked VM or VM state with a
// manifest, and while a process presents the service's VM paths.
func TestProveServiceAbsentRefusesAnyClaimant(t *testing.T) {
	m, f, i := recoveryFixture(t)
	m.inspector = i
	if err := m.ProveServiceAbsent(context.Background(), "../escape"); err == nil {
		t.Fatal("an unsafe service name was accepted")
	}
	if err := m.ProveServiceAbsent(context.Background(), f.Service); err == nil {
		t.Fatal("VM state with a manifest was proven absent")
	}
	if err := m.ProveServiceAbsent(context.Background(), "fresh"); err != nil {
		t.Fatalf("a service with no VM state and no claimant: %v", err)
	}
	i.find = []processIdentity{{PID: 77}}
	if err := m.ProveServiceAbsent(context.Background(), "fresh"); err == nil {
		t.Fatal("a process presenting the VM paths was ignored")
	}
}

func TestVMServiceUnderNamesTheVMDirectory(t *testing.T) {
	root := "/var/lib/firework/vms"
	for args, want := range map[string]string{
		"/usr/bin/firecracker --api-sock /var/lib/firework/vms/web/firecracker.sock":  "web",
		"/usr/bin/firecracker --config-file=/var/lib/firework/vms/api/vm-config.json": "api",
		"/usr/bin/firecracker --api-sock /var/lib/firework/vmsx/web/firecracker.sock": "",
		"/usr/bin/firecracker --api-sock /var/lib/firework/vms/../etc/sock":           "",
		"/usr/bin/sleep 300": "",
	} {
		if got := vmServiceUnder(strings.Fields(args), root); got != want {
			t.Errorf("%q: got %q, want %q", args, got, want)
		}
	}
}
