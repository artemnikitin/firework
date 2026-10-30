//go:build linux && systemd_integration

package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

// Runs in an isolated privileged container whose PID1 is real systemd.
// No Firecracker/KVM or host networking needed: the payload holds the real
// launch arguments and an API socket through systemd start/recovery/stop.
func TestSystemdIntegrationLifecycle(t *testing.T) {
	if os.Getenv("FIREWORK_REAL_SYSTEMD") != "1" {
		t.Skip("requires isolated real systemd")
	}
	root := t.TempDir()
	src := filepath.Join(root, "fake.go")
	fake := `package main
import("flag";"net";"os";"os/signal";"syscall")
func main(){flag.String("id","","");p:=flag.String("api-sock","","");flag.String("config-file","","");flag.Parse(); l,e:=net.Listen("unix",*p);if e!=nil {panic(e)};defer l.Close(); go func(){for{c,e:=l.Accept();if e!=nil{return};c.Close()}}(); s:=make(chan os.Signal,1);signal.Notify(s,syscall.SIGTERM,syscall.SIGINT);<-s}
`
	if err := os.WriteFile(src, []byte(fake), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "fake-firecracker")
	if out, err := exec.Command("go", "build", "-o", binary, src).CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(binary, root, logger)
	if _, ok := m.launcher.(*systemdLauncher); !ok {
		t.Fatalf("not using real systemd: %T", m.launcher)
	}
	svc := config.ServiceConfig{Name: "probe", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.Start(ctx, svc); err != nil {
		t.Fatal(err)
	}
	original := cloneInstanceManifest(m.List()[svc.Name].manifest)
	defer func() {
		out, err := exec.Command("systemctl", "stop", original.LauncherUnit).CombinedOutput()
		t.Logf("final unit cleanup: %v %s", err, out)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for validateOwnedSocket(m.inspector, original) != nil {
		if time.Now().After(deadline) {
			t.Fatal("socket not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := validateOwnedProcess(m.inspector, original); err != nil {
		t.Fatalf("real launch identity: %v", err)
	}
	t.Logf("started real unit %s pid=%d", original.LauncherUnit, original.PID)
	// Prevent the first manager's observer from modifying the shared manifest.
	m.mu.Lock()
	delete(m.instances, svc.Name)
	m.mu.Unlock()
	m2 := NewManager(binary, root, logger)
	if adopted, err := m2.Recover(ctx, config.NodeConfig{Services: []config.ServiceConfig{svc}}); err != nil || len(adopted) != 1 || !m2.IsRunning(svc.Name) {
		t.Fatalf("real restart recovery: adopted=%v err=%v state=%#v", adopted, err, m2.List())
	}
	// Exercise the interrupted systemd launch with no captured MainPID.
	m2.mu.Lock()
	delete(m2.instances, svc.Name)
	m2.mu.Unlock()
	partial := cloneInstanceManifest(original)
	partial.PID = 0
	partial.ProcessStart = 0
	partial.HostBootID = ""
	partial.ExecutableDev = 0
	partial.ExecutableIno = 0
	partial.Lifecycle = lifecycleStarting
	if err := writeManifest(manifestPath(partial.VMDir), partial); err != nil {
		t.Fatal(err)
	}
	m3 := NewManager(binary, root, logger)
	if adopted, err := m3.Recover(ctx, config.NodeConfig{Services: []config.ServiceConfig{svc}}); err != nil || len(adopted) != 1 || !m3.IsRunning(svc.Name) {
		t.Fatalf("real PID0 recovery: adopted=%v err=%v state=%#v", adopted, err, m3.List())
	}
	// Fail only the final stopped manifest write. Confirmed child exit must
	// still permit a replacement after the actual transient unit is collected.
	m3.mu.Lock()
	m3.recoveryManifestWriter = func(path string, f *instanceManifest) error {
		if f.PID == 0 {
			return errors.New("injected stopped manifest write failure")
		}
		return writeManifest(path, f)
	}
	m3.mu.Unlock()
	if err := m3.Stop(svc.Name); err == nil {
		t.Fatal("real systemd Stop ignored terminal persistence error")
	}
	if got := m3.List()[svc.Name]; got.State != StateStopped || got.PID != 0 {
		t.Fatalf("terminal write failure lost confirmed exit: %#v", got)
	}
	m3.mu.Lock()
	m3.recoveryManifestWriter = nil
	m3.mu.Unlock()
	// Restart must wait until the old systemd unit is fully terminal/collected.
	deadline = time.Now().Add(4 * time.Second)
	for {
		err := m3.Start(ctx, svc)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(fmt.Errorf("real systemd replacement failed: %w", err))
		}
		_, _ = m3.Recover(ctx, config.NodeConfig{Services: []config.ServiceConfig{svc}})
		time.Sleep(50 * time.Millisecond)
	}
	replacement := m3.List()[svc.Name]
	if replacement.PID == original.PID || replacement.instanceID == original.InstanceID {
		t.Fatal("replacement reused owner")
	}
	if err := m3.Remove(svc.Name); err != nil {
		t.Fatalf("real systemd Remove: %v", err)
	}
	if _, err := os.Stat(original.VMDir); !os.IsNotExist(err) {
		t.Fatalf("state not removed: %v", err)
	}
	t.Log("real systemd launch, restart adoption, PID0 recovery, stop, replacement, remove succeeded")
}

// Real kernel TAP retention while a live owner remains ambiguous. Runs after
// or independently of Lifecycle and uses an entirely private container device.
func TestSystemdIntegrationTapRetainedWhileAmbiguous(t *testing.T) {
	if os.Getenv("FIREWORK_REAL_SYSTEMD") != "1" {
		t.Skip("requires isolated real systemd")
	}
	root := t.TempDir()
	src := filepath.Join(root, "fake.go")
	binary := filepath.Join(root, "fake-firecracker")
	source := `package main
import("flag";"net";"os";"os/signal";"syscall";"unsafe")
func main(){flag.String("id","","");p:=flag.String("api-sock","","");flag.String("config-file","","");flag.Parse();fd,e:=syscall.Open("/dev/net/tun",syscall.O_RDWR,0);if e!=nil{panic(e)};defer syscall.Close(fd);var req [40]byte;copy(req[:16],"tap-pr66-probe");*(*uint16)(unsafe.Pointer(&req[16]))=0x1002;_,_,en:=syscall.Syscall(syscall.SYS_IOCTL,uintptr(fd),uintptr(0x400454ca),uintptr(unsafe.Pointer(&req[0])));if en!=0{panic(en)};l,e:=net.Listen("unix",*p);if e!=nil{panic(e)};defer l.Close();go func(){for{c,e:=l.Accept();if e!=nil{return};c.Close()}}();s:=make(chan os.Signal,1);signal.Notify(s,syscall.SIGTERM);<-s}
`
	if err := os.WriteFile(src, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", binary, src).CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	if out, err := exec.Command("ip", "tuntap", "add", "dev", "tap-pr66-probe", "mode", "tap").CombinedOutput(); err != nil {
		t.Fatalf("create TAP: %v %s", err, out)
	}
	defer exec.Command("ip", "link", "delete", "tap-pr66-probe").Run()
	m := NewManager(binary, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc := config.ServiceConfig{Name: "tap-probe", Image: "/image", Kernel: "/kernel", VCPUs: 1, MemoryMB: 128}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Start(ctx, svc); err != nil {
		t.Fatal(err)
	}
	f := cloneInstanceManifest(m.List()[svc.Name].manifest)
	defer exec.Command("systemctl", "stop", f.LauncherUnit).Run()
	deadline := time.Now().Add(3 * time.Second)
	for validateOwnedSocket(m.inspector, f) != nil {
		if time.Now().After(deadline) {
			t.Fatal("payload failed to open TAP/socket")
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertTap := func() {
		t.Helper()
		out, err := exec.Command("ip", "-details", "link", "show", "dev", "tap-pr66-probe").CombinedOutput()
		if err != nil {
			t.Fatalf("TAP lost: %v %s", err, out)
		}
		if !strings.Contains(string(out), "tun type tap") {
			t.Fatalf("not a TAP: %s", out)
		}
	}
	assertTap()
	m.mu.Lock()
	delete(m.instances, svc.Name)
	m.mu.Unlock()
	corrupt := cloneInstanceManifest(f)
	corrupt.ProcessStart = 0
	corrupt.ExecutableIno = 0
	if err := writeManifest(manifestPath(f.VMDir), corrupt); err != nil {
		t.Fatal(err)
	}
	restarted := NewManager(binary, root, m.logger)
	if _, err := restarted.Recover(ctx, config.NodeConfig{}); err != nil {
		t.Fatal(err)
	}
	if got := restarted.List()[svc.Name]; got == nil || got.State != StateRecoveryPending {
		t.Fatalf("missing identity not quarantined: %#v", got)
	}
	for _, op := range []func() error{func() error { _, e := restarted.ClearRecoveryPending(svc.Name); return e }, func() error { _, e := restarted.ForgetRecoveryPending(svc.Name); return e }, func() error { return restarted.Remove(svc.Name) }, func() error { return restarted.Stop(svc.Name) }} {
		if err := op(); err == nil {
			t.Fatal("ambiguous live owner was changed")
		}
		assertTap()
		if err := validateOwnedProcess(restarted.inspector, f); err != nil {
			t.Fatalf("live owner changed: %v", err)
		}
	}
	if err := writeManifest(manifestPath(f.VMDir), f); err != nil {
		t.Fatal(err)
	}
	// Simulate a fresh daemon rereading the repaired durable record.
	final := NewManager(binary, root, m.logger)
	if adopted, err := final.Recover(ctx, config.NodeConfig{}); err != nil || len(adopted) != 1 || !final.IsRunning(svc.Name) {
		t.Fatalf("repaired owner failed readoption: %v %v", adopted, err)
	}
	assertTap()
	if err := final.Stop(svc.Name); err != nil {
		t.Fatal(err)
	}
	t.Log("live ambiguous VM and kernel TAP survived clear, forget, Remove, Stop; repaired record readopted")
}
