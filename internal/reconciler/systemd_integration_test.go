//go:build linux && systemd_integration

package reconciler

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/network"
	"github.com/artemnikitin/firework/internal/vm"
)

// Runs in the isolated privileged container of scripts/systemd-recovery,
// whose PID1 is real systemd: real transient units, /proc, TAP devices and
// iptables, with a payload that holds Firecracker's launch arguments and API
// socket. Each agent incarnation gets a new VM manager and reconciler, which
// rebuild their state from the state directory while the VM process and the
// host's rules survive the crash.
func TestSystemdIntegrationOwnershipSurvivesAgentRestart(t *testing.T) {
	if os.Getenv("FIREWORK_REAL_SYSTEMD") != "1" {
		t.Skip("requires isolated real systemd")
	}
	root := t.TempDir()
	src := filepath.Join(root, "fake.go")
	fake := `package main
import("flag";"net";"os";"os/signal";"syscall")
func main(){flag.String("id","","");p:=flag.String("api-sock","","");flag.String("config-file","","");flag.Parse(); l,e:=net.Listen("unix",*p);if e!=nil {panic(e)};defer l.Close(); go func(){for{c,e:=l.Accept();if e!=nil{return};c.Close()}}(); s:=make(chan os.Signal,1);signal.Notify(s,syscall.SIGTERM,syscall.SIGINT);<-s}
`
	if err := os.WriteFile(src, []byte(fake), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "fake-firecracker")
	if out, err := exec.Command("go", "build", "-o", binary, src).CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	stateDir := filepath.Join(root, "state")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	host := network.NewManager(logger)
	svc := config.ServiceConfig{Name: "owned", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128,
		Network:      &config.NetworkConfig{GuestIP: "172.31.0.2", Interface: "tap-owned"},
		PortForwards: []config.PortForward{{HostPort: 18080, VMPort: 80}}}
	desired := config.NodeConfig{Services: []config.ServiceConfig{svc}}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	defer exec.Command("sh", "-c", "systemctl stop 'firework-vm-*.service'").Run()

	// incarnation starts an agent: a fresh VM manager that discovers the
	// state directory, and a reconciler over the same journal and kernel.
	// crashOn, when set, crashes the agent at the journal write it matches:
	// before the write when crashBefore is set, after it otherwise.
	incarnation := func(crashOn string, crashBefore bool) (*vm.Manager, *Reconciler) {
		t.Helper()
		manager := vm.NewManager(binary, stateDir, logger)
		if err := manager.DiscoverRecoveryState(); err != nil {
			t.Fatal(err)
		}
		r := New(manager, logger, nil, host, "", 0)
		r.WithJournalWriter(func(path string, data []byte) error {
			matches := crashOn != "" && strings.Contains(string(data), crashOn)
			if matches && crashBefore {
				panic(errCrash)
			}
			if err := atomicWriteFile(path, data, 0o644); err != nil {
				return err
			}
			if matches {
				panic(errCrash)
			}
			return nil
		})
		r.WithHostHistoryEvidence("172.31.0.0/24", nil).WithStateDir(stateDir)
		return manager, r
	}
	crashes := func(r *Reconciler, desired config.NodeConfig) (crashed bool) {
		defer func() {
			if recovered := recover(); recovered != nil {
				if recovered != errCrash {
					panic(recovered)
				}
				crashed = true
			}
		}()
		_ = r.Reconcile(ctx, desired)
		return false
	}
	tagged := func() []network.PortForwardRule {
		t.Helper()
		listing, err := host.ListPortForwards()
		if err != nil {
			t.Fatal(err)
		}
		var rules []network.PortForwardRule
		for _, rule := range listing.Rules {
			if rule.TaggedFor(svc.Name) {
				rules = append(rules, rule)
			}
		}
		return rules
	}
	converge := func(r *Reconciler, manager *vm.Manager, desired config.NodeConfig, done func() bool) {
		t.Helper()
		var err error
		for attempt := 0; attempt < 40; attempt++ {
			if err = r.Reconcile(ctx, desired); err == nil && done() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("did not converge: %v; VMs %#v; rules %v", err, manager.List(), tagged())
	}

	// Incarnation 1 crashes after the VM started and its rule was installed,
	// before the record says running.
	first, r1 := incarnation(`"phase":"running"`, true)
	if !crashes(r1, desired) {
		t.Fatal("the crash point was not reached")
	}
	before := first.List()[svc.Name]
	journal, _ := os.ReadFile(filepath.Join(stateDir, pendingTeardownsFile))
	if before == nil || len(tagged()) != 1 || !strings.Contains(string(journal), `"phase":"creating"`) {
		t.Fatalf("setup: VM %#v rules %v journal %s", before, tagged(), journal)
	}

	// Incarnation 2 rebuilds everything from disk. It adopts the live VM
	// rather than starting a second one, keeps its rule, and records it.
	second, r2 := incarnation(`"phase":"departing"`, false)
	converge(r2, second, desired, func() bool { return second.IsRunning(svc.Name) && len(recordsIn(r2, phaseRunning)) == 1 })
	if after := second.List()[svc.Name]; after.PID != before.PID {
		t.Fatalf("the live VM was replaced instead of adopted: pid %d, was %d", after.PID, before.PID)
	}
	if rules := tagged(); len(rules) != 1 || !rules[0].Targets(18080, "172.31.0.2", 80) {
		t.Fatalf("rule after adoption: %v", rules)
	}

	// Incarnation 2 then crashes after recording the removal, while the VM
	// still runs.
	if !crashes(r2, config.NodeConfig{}) {
		t.Fatal("the removal crash point was not reached")
	}

	// Incarnation 3 finishes the removal: the VM goes, then its rule, its
	// device and its record.
	third, r3 := incarnation("", false)
	converge(r3, third, config.NodeConfig{}, func() bool {
		return len(third.List()) == 0 && len(tagged()) == 0 && len(r3.allocations) == 0 && !r3.HasPendingCleanup()
	})
	if out, err := exec.Command("ip", "link", "show", "dev", "tap-owned").CombinedOutput(); err == nil {
		t.Fatalf("the VM's device survived its removal: %s", out)
	}
	t.Log("crash after rule install, restart adoption with a new VM manager, crash after the departing record, and resumed removal succeeded")
}
