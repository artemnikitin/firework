package vm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

func TestQuarantinedChildExitRetainsRecoveryEligibility(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		clean, command, stopping bool
		writeFailure             string
	}{
		{name: "abnormal automatic"}, {name: "clean automatic", clean: true},
		{name: "abnormal command", command: true}, {name: "clean command", clean: true, command: true},
		{name: "stopping automatic", stopping: true}, {name: "stopping command", stopping: true, command: true},
		{name: "exit persistence failure", writeFailure: "before rename"},
		{name: "exit directory sync failure", writeFailure: "after rename"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f, i := recoveryFixture(t)
			m.inspector = i
			i.identities = map[int]processIdentity{}
			script := "exec sleep 30"
			if tc.clean {
				script = "exit 0"
			}
			cmd := exec.Command("/bin/sh", "-c", script)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			f.PID = cmd.Process.Pid
			if tc.stopping {
				f.Lifecycle = lifecycleStopping
			}
			if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
				t.Fatal(err)
			}
			m.addRecoveryPending(f.Service, f, "temporary socket failure")
			if tc.writeFailure != "" {
				m.recoveryManifestWriter = func(path string, terminal *instanceManifest) error {
					if tc.writeFailure == "after rename" {
						if err := writeManifest(path, terminal); err != nil {
							return err
						}
					}
					return errors.New("injected exit persistence failure")
				}
			}
			log, err := os.CreateTemp(t.TempDir(), "log")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				m.monitor(f.Service, f.InstanceID, &launchedProcess{PID: f.PID, Cmd: cmd, LogFile: log})
				close(done)
			}()
			if !tc.clean {
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("Wait did not complete")
			}
			got := m.List()[f.Service]
			if got.State != StateRecoveryPending || !m.HasPendingRecovery() {
				t.Fatalf("lost recovery eligibility: %#v", got)
			}
			durable, err := readManifest(manifestPath(f.VMDir))
			if err != nil {
				t.Fatal(err)
			}
			if !sameRecoveryIdentity(got.manifest, durable) || got.PID != durable.PID {
				t.Fatal("exit persistence split memory from disk")
			}
			if tc.stopping && durable.Lifecycle != lifecycleStopping {
				t.Fatal("Wait erased shutdown marker")
			}
			m.recoveryManifestWriter = nil
			if tc.command {
				r, err := m.ClearRecoveryPending(f.Service)
				if tc.stopping {
					if err == nil {
						t.Fatal("stopping state cleared without guarded forget")
					}
				} else if err != nil || r.Outcome != RecoveryOutcomeCleared {
					t.Fatalf("command retry: %#v %v", r, err)
				}
			} else {
				if _, err := m.Recover(context.Background(), config.NodeConfig{Services: []config.ServiceConfig{f.Config}}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.stopping {
				if got := m.List()[f.Service]; got == nil || got.State != StateRecoveryPending {
					t.Fatal("shutdown uncertainty lost")
				}
				if r, err := m.ForgetRecoveryPending(f.Service); err != nil || r.Outcome != RecoveryOutcomeCleared {
					t.Fatalf("guarded forget: %#v %v", r, err)
				}
			}
			if m.List()[f.Service] != nil || m.HasPendingRecovery() {
				t.Fatal("known-dead runtime did not clear")
			}
			if _, err := os.Stat(f.VMDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("runtime directory remains: %v", err)
			}
		})
	}
}
