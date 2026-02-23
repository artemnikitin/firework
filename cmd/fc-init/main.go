//go:build linux

// fc-init is a minimal init process for Firecracker microVMs.
//
// It runs as PID 1 (init=/sbin/fc-init) inside the guest and:
//  1. Mounts /proc, /sys, /dev/pts
//  2. Reads /proc/cmdline and exports any firework.env.KEY=VALUE pairs
//     as environment variables for the child process.
//  3. Execs the remainder of argv (os.Args[1:]), or falls back to
//     /sbin/init if no arguments are given.
//
// Usage in kernel args:
//
//	init=/sbin/fc-init /path/to/service --flag
//
// Or when called without arguments it falls back to /sbin/init.
package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const runtimeMetadataPath = "/etc/firework/runtime.json"

type runtimeMetadata struct {
	User          string            `json:"user,omitempty"`
	Workdir       string            `json:"workdir,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	WritablePaths []string          `json:"writable_paths,omitempty"`
}

func main() {
	mountAll()
	applyKernelSettings()
	setHostname()
	meta := loadRuntimeMetadata()
	applyImageEnv(meta.Env)
	exportFireworkEnv()
	execService(meta)
}

// mountAll mounts the virtual filesystems the guest needs.
func mountAll() {
	mounts := []struct{ fstype, src, dst, opts string }{
		{"proc", "proc", "/proc", ""},
		{"sysfs", "sys", "/sys", ""},
		{"devtmpfs", "dev", "/dev", ""},
		{"devpts", "devpts", "/dev/pts", ""},
		{"tmpfs", "tmpfs", "/run", ""},
		{"tmpfs", "tmpfs", "/tmp", ""},
	}
	for _, m := range mounts {
		_ = os.MkdirAll(m.dst, 0755)
		if err := syscall.Mount(m.src, m.dst, m.fstype, 0, m.opts); err != nil {
			// Non-fatal: log and continue. Some mounts may already exist.
			fmt.Fprintf(os.Stderr, "fc-init: mount %s: %v\n", m.dst, err)
		}
	}
}

// applyKernelSettings raises kernel limits that production workloads require.
// It runs after mountAll (so /proc is available) and before exec-ing the service.
//
//   - vm.max_map_count: Elasticsearch enforces ≥262144 when bound to a
//     non-loopback address. The kernel default inside a Firecracker VM is 65530.
//   - RLIMIT_NOFILE: Elasticsearch requires ≥65535 file descriptors. The default
//     kernel limit inside a Firecracker VM is 4096.
//
// Failures are logged but not fatal: some environments already have suitable
// values, and we do not want to block startup if a sysctl is unavailable.
func applyKernelSettings() {
	// vm.max_map_count
	if err := os.WriteFile("/proc/sys/vm/max_map_count", []byte("262144\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "fc-init: set vm.max_map_count: %v\n", err)
	}

	// File descriptor limit
	limit := syscall.Rlimit{Cur: 65535, Max: 65535}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		fmt.Fprintf(os.Stderr, "fc-init: set RLIMIT_NOFILE: %v\n", err)
	}
}

// setHostname sets a deterministic hostname so apps that require hostname
// resolution (e.g. Elasticsearch) don't fail with "(none)".
func setHostname() {
	hostname := "fc-guest"
	if data, err := os.ReadFile("/etc/hostname"); err == nil {
		if h := strings.TrimSpace(string(data)); h != "" {
			hostname = h
		}
	}
	if err := syscall.Sethostname([]byte(hostname)); err != nil {
		fmt.Fprintf(os.Stderr, "fc-init: sethostname %s: %v\n", hostname, err)
	}
}

// loadRuntimeMetadata reads optional Docker-derived runtime metadata.
func loadRuntimeMetadata() runtimeMetadata {
	var meta runtimeMetadata
	data, err := os.ReadFile(runtimeMetadataPath)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "fc-init: read %s: %v\n", runtimeMetadataPath, err)
		}
		return meta
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		fmt.Fprintf(os.Stderr, "fc-init: parse %s: %v\n", runtimeMetadataPath, err)
	}
	return meta
}

// applyImageEnv exports environment variables from image metadata.
// firework.env.KEY runtime values are applied afterwards and override these.
func applyImageEnv(env map[string]string) {
	if len(env) == 0 {
		return
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		if err := os.Setenv(k, env[k]); err != nil {
			fmt.Fprintf(os.Stderr, "fc-init: set image env %s: %v\n", k, err)
		}
	}
}

// exportFireworkEnv reads /proc/cmdline and exports any
// firework.env.KEY=VALUE entries into the process environment.
func exportFireworkEnv() {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fc-init: read /proc/cmdline: %v\n", err)
		return
	}
	for _, arg := range strings.Fields(string(data)) {
		rest, ok := strings.CutPrefix(arg, "firework.env.")
		if !ok {
			continue
		}
		key, val, found := strings.Cut(rest, "=")
		if !found {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			fmt.Fprintf(os.Stderr, "fc-init: setenv %s: %v\n", key, err)
		}
	}
}

// execService execs argv[1:] if provided, otherwise /sbin/init.
func execService(meta runtimeMetadata) {
	argv := os.Args[1:]
	if len(argv) == 0 {
		argv = []string{"/sbin/init"}
	}

	if meta.Workdir != "" {
		if err := os.Chdir(meta.Workdir); err != nil {
			fmt.Fprintf(os.Stderr, "fc-init: chdir %s: %v\n", meta.Workdir, err)
		}
	}

	if spec := strings.TrimSpace(meta.User); spec != "" {
		if err := applyUserSpec(spec, meta.WritablePaths); err != nil {
			fmt.Fprintf(os.Stderr, "fc-init: apply user %q: %v\n", spec, err)
			os.Exit(1)
		}
	}

	// Resolve PATH for the binary if it's not absolute.
	bin := resolveBinary(argv[0])

	if err := syscall.Exec(bin, argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "fc-init: exec %s: %v\n", bin, err)
		os.Exit(1)
	}
}

func resolveBinary(bin string) string {
	if strings.HasPrefix(bin, "/") {
		return bin
	}
	paths := []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}
	for _, dir := range paths {
		candidate := dir + "/" + bin
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return bin
}

func applyUserSpec(spec string, writablePaths []string) error {
	uid, gid, username, home, err := resolveUserSpec(spec)
	if err != nil {
		return err
	}

	if err := ensureWritablePaths(writablePaths, uid, gid); err != nil {
		return err
	}

	if home != "" {
		_ = os.Setenv("HOME", home)
	}
	if username != "" {
		_ = os.Setenv("USER", username)
	}

	if err := syscall.Setgroups([]int{gid}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid(%d): %w", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid(%d): %w", uid, err)
	}
	return nil
}

// ensureWritablePaths recursively changes ownership for paths that apps need
// write access to after dropping privileges. Missing paths are ignored.
func ensureWritablePaths(paths []string, uid, gid int) error {
	var errs []string
	for _, path := range normalizeWritablePaths(paths) {
		if err := chownPathRecursive(path, uid, gid); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			errs = append(errs, fmt.Sprintf("%s: %v", path, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("prepare writable paths: %s", strings.Join(errs, "; "))
	}
	return nil
}

func chownPathRecursive(path string, uid, gid int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return os.Lchown(path, uid, gid)
	}

	return filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return os.Lchown(p, uid, gid)
	})
}

func normalizeWritablePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" || !strings.HasPrefix(p, "/") {
			continue
		}
		p = filepath.Clean(p)
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// resolveUserSpec resolves Docker-style USER values:
//   - username
//   - uid
//   - username:group
//   - uid:gid
//
// If only numeric uid is provided, gid defaults to uid.
func resolveUserSpec(spec string) (uid, gid int, username, home string, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, 0, "", "", fmt.Errorf("empty user spec")
	}

	userPart, groupPart, hasGroup := strings.Cut(spec, ":")
	if userPart == "" {
		return 0, 0, "", "", fmt.Errorf("invalid user spec %q", spec)
	}

	parsedUID, uidIsNumeric, err := parseNumericID(userPart)
	if err != nil {
		return 0, 0, "", "", err
	}
	if uidIsNumeric {
		uid = parsedUID
		gid = parsedUID
	} else {
		u, lookupErr := user.Lookup(userPart)
		if lookupErr != nil {
			return 0, 0, "", "", fmt.Errorf("lookup user %q: %w", userPart, lookupErr)
		}
		uid, err = parseRequiredID(u.Uid)
		if err != nil {
			return 0, 0, "", "", fmt.Errorf("invalid uid for %q: %w", userPart, err)
		}
		gid, err = parseRequiredID(u.Gid)
		if err != nil {
			return 0, 0, "", "", fmt.Errorf("invalid gid for %q: %w", userPart, err)
		}
		username = u.Username
		home = u.HomeDir
	}

	if hasGroup && groupPart != "" {
		parsedGID, gidIsNumeric, err := parseNumericID(groupPart)
		if err != nil {
			return 0, 0, "", "", err
		}
		if gidIsNumeric {
			gid = parsedGID
		} else {
			g, lookupErr := user.LookupGroup(groupPart)
			if lookupErr != nil {
				return 0, 0, "", "", fmt.Errorf("lookup group %q: %w", groupPart, lookupErr)
			}
			gid, err = parseRequiredID(g.Gid)
			if err != nil {
				return 0, 0, "", "", fmt.Errorf("invalid gid for group %q: %w", groupPart, err)
			}
		}
	}

	return uid, gid, username, home, nil
}

func parseNumericID(s string) (int, bool, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false, nil
	}
	if n < 0 {
		return 0, false, fmt.Errorf("invalid negative id %q", s)
	}
	return n, true, nil
}

func parseRequiredID(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("negative id %d", n)
	}
	return n, nil
}
