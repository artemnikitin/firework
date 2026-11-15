//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

const applicationBootstrapArg = "--firework-application-child"

type applicationLaunch struct {
	Metadata runtimeMetadata
	Volumes  []guestVolume
	Argv     []string
}

func init() {
	// PID 1 never dispatches a bootstrap based on image argv or environment.
	if os.Getpid() != 1 && len(os.Args) == 2 && os.Args[1] == applicationBootstrapArg {
		os.Exit(runApplicationBootstrap())
	}
}

func runApplicationBootstrap() int {
	setup := os.NewFile(3, "application-setup")
	status := os.NewFile(4, "application-status")
	if setup == nil || status == nil {
		return 1
	}
	defer setup.Close()
	defer status.Close()
	for _, file := range []*os.File{setup, status} {
		info, err := file.Stat()
		if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			return 1
		}
	}
	syscall.CloseOnExec(int(status.Fd()))
	var launch applicationLaunch
	err := json.NewDecoder(io.LimitReader(setup, 4<<20)).Decode(&launch)
	_ = setup.Close()
	if err == nil && len(launch.Argv) == 0 {
		err = fmt.Errorf("application bootstrap has no argv")
	}
	if err == nil {
		err = launchApplication(launch, realApplicationOperations())
	}
	if err != nil {
		message := err.Error()
		if len(message) > 4096 {
			message = message[:4096]
		}
		_, _ = io.WriteString(status, message)
		fmt.Fprintln(os.Stderr, "fc-init:", message)
		return 1
	}
	return 0
}

func startSupervisedApplication(launch applicationLaunch) (int, error) {
	setupRead, setupWrite, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer setupRead.Close()
	defer setupWrite.Close()
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer statusRead.Close()
	defer statusWrite.Close()
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	pid, err := syscall.ForkExec(self, []string{self, applicationBootstrapArg}, &syscall.ProcAttr{
		Env: os.Environ(), Files: []uintptr{os.Stdin.Fd(), os.Stdout.Fd(), os.Stderr.Fd(), setupRead.Fd(), statusWrite.Fd()},
		Sys: &syscall.SysProcAttr{Setpgid: true},
	})
	if err != nil {
		return 0, err
	}
	_ = setupRead.Close()
	_ = statusWrite.Close()
	if err := json.NewEncoder(setupWrite).Encode(launch); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_, _ = syscall.Wait4(pid, nil, 0, nil)
		return 0, fmt.Errorf("send application setup: %w", err)
	}
	_ = setupWrite.Close()
	message, err := io.ReadAll(io.LimitReader(statusRead, 4097))
	if err != nil || len(message) > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_, _ = syscall.Wait4(pid, nil, 0, nil)
		if err != nil {
			return 0, fmt.Errorf("read application setup: %w", err)
		}
		return 0, fmt.Errorf("application setup: %s", message)
	}
	return pid, nil
}

type applicationOperations struct {
	chdir       func(string) error
	prepareUser func(string, []string, []guestVolume) (*syscall.Credential, error)
	setgroups   func([]int) error
	setgid      func(int) error
	setuid      func(int) error
	resolve     func(string) string
	exec        func(string, []string, []string) error
	environ     func() []string
	warn        func(string, error)
}

func realApplicationOperations() applicationOperations {
	return applicationOperations{
		chdir: os.Chdir, prepareUser: prepareUserSpec, setgroups: syscall.Setgroups,
		setgid: syscall.Setgid, setuid: syscall.Setuid, resolve: resolveBinary,
		exec: syscall.Exec, environ: os.Environ,
		warn: func(path string, err error) { fmt.Fprintf(os.Stderr, "fc-init: chdir %s: %v\n", path, err) },
	}
}

func launchApplication(launch applicationLaunch, operations applicationOperations) error {
	meta := launch.Metadata
	if meta.Workdir != "" {
		if err := operations.chdir(meta.Workdir); err != nil {
			operations.warn(meta.Workdir, err)
		}
	}
	if spec := strings.TrimSpace(meta.User); spec != "" {
		credential, err := operations.prepareUser(spec, meta.WritablePaths, launch.Volumes)
		if err != nil {
			return fmt.Errorf("apply user %q: %w", spec, err)
		}
		groups := make([]int, len(credential.Groups))
		for i, group := range credential.Groups {
			groups[i] = int(group)
		}
		if err := operations.setgroups(groups); err != nil {
			return fmt.Errorf("setgroups: %w", err)
		}
		if err := operations.setgid(int(credential.Gid)); err != nil {
			return fmt.Errorf("setgid(%d): %w", credential.Gid, err)
		}
		if err := operations.setuid(int(credential.Uid)); err != nil {
			return fmt.Errorf("setuid(%d): %w", credential.Uid, err)
		}
	}
	bin := operations.resolve(launch.Argv[0])
	if err := operations.exec(bin, launch.Argv, operations.environ()); err != nil {
		return fmt.Errorf("exec %s: %w", bin, err)
	}
	return nil
}
