//go:build linux

package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestApplicationLaunchPreservesDirectoryAndCredentialSemantics(t *testing.T) {
	for _, test := range []struct {
		name, workdir, expected string
		directoryError          error
		user                    string
	}{
		{name: "empty", expected: "/original", user: "1000:1000"},
		{name: "absolute", workdir: "/data/app", expected: "/data/app", user: "1000:1000"},
		{name: "relative", workdir: "app", expected: "/original/app", user: "1000:1000"},
		{name: "dot", workdir: ".", expected: "/original", user: "1000:1000"},
		{name: "parent", workdir: "..", expected: "/", user: "1000:1000"},
		{name: "symlink", workdir: "/link", expected: "/data/app", user: "1000:1000"},
		{name: "missing", workdir: "/missing", expected: "/original", directoryError: syscall.ENOENT, user: "1000:1000"},
		{name: "not directory", workdir: "/file", expected: "/original", directoryError: syscall.ENOTDIR, user: "1000:1000"},
		{name: "directory denied", workdir: "/denied", expected: "/original", directoryError: syscall.EACCES, user: "1000:1000"},
		{name: "inherited credentials", workdir: "/data/app", expected: "/data/app"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cwd, uid, warnings := "/original", 0, 0
			groups := []int{7, 8}
			argv := []string{"./app", "literal argument"}
			env := []string{"HOME=/existing", "USER=existing", "FROM_KERNEL=preserved"}
			called := false
			ops := applicationOperations{
				chdir: func(path string) error {
					if uid != 0 {
						return syscall.EACCES
					}
					if test.directoryError != nil {
						return test.directoryError
					}
					if path == "/link" {
						cwd = "/data/app"
					} else if filepath.IsAbs(path) {
						cwd = filepath.Clean(path)
					} else {
						cwd = filepath.Join(cwd, path)
					}
					return nil
				},
				warn: func(string, error) { warnings++ },
				prepareUser: func(spec string, paths []string, volumes []guestVolume) (*syscall.Credential, error) {
					if uid != 0 || cwd != test.expected || spec != test.user {
						t.Fatalf("preparation changed startup ordering: uid=%d cwd=%s spec=%s", uid, cwd, spec)
					}
					return &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{1000}}, nil
				},
				setgroups: func(values []int) error { groups = values; return nil },
				setgid:    func(int) error { return nil },
				setuid:    func(value int) error { uid = value; return nil },
				resolve: func(name string) string {
					if test.user != "" && uid != 1000 {
						t.Fatal("executable resolved before dropping credentials")
					}
					return filepath.Join(cwd, name)
				},
				environ: func() []string { return env },
				exec: func(binary string, args, environment []string) error {
					called = true
					if cwd != test.expected || binary != filepath.Join(test.expected, "app") || !reflect.DeepEqual(args, argv) || !reflect.DeepEqual(environment, env) {
						t.Fatalf("startup mismatch: cwd=%s binary=%s argv=%v env=%v", cwd, binary, args, environment)
					}
					return nil
				},
			}
			if err := launchApplication(applicationLaunch{Metadata: runtimeMetadata{User: test.user, Workdir: test.workdir}, Argv: argv}, ops); err != nil {
				t.Fatal(err)
			}
			if !called || (warnings != 0) != (test.directoryError != nil) {
				t.Fatalf("called=%v warnings=%d", called, warnings)
			}
			if test.user == "" && (uid != 0 || !reflect.DeepEqual(groups, []int{7, 8})) {
				t.Fatal("empty user changed inherited credentials")
			}
		})
	}
}

func TestApplicationExecutableLookupPreservesMainSearch(t *testing.T) {
	for _, test := range []struct{ name, binary, existing, want string }{
		{"absolute", "/opt/app", "", "/opt/app"},
		{"bare name", "app", "/usr/local/bin/app", "/usr/local/bin/app"},
		{"dot path searched literally", "./app", "/usr/sbin/./app", "/usr/sbin/./app"},
		{"relative slash searched literally", "tools/app", "/bin/tools/app", "/bin/tools/app"},
		{"relative fallback", "./app", "", "./app"},
		{"bare fallback", "app", "", "app"},
		{"slash fallback", "tools/app", "", "tools/app"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var visited []string
			got := resolveBinaryWithStat(test.binary, func(path string) (fs.FileInfo, error) {
				visited = append(visited, path)
				if path == test.existing {
					return nil, nil
				}
				return nil, syscall.EACCES
			})
			if got != test.want {
				t.Fatalf("binary=%q want=%q tried=%v", got, test.want, visited)
			}
			if filepath.IsAbs(test.binary) {
				if len(visited) != 0 {
					t.Fatal("absolute executable was searched")
				}
				return
			}
			var want []string
			for _, dir := range []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
				candidate := dir + "/" + test.binary
				want = append(want, candidate)
				if candidate == test.existing {
					break
				}
			}
			if !slices.Equal(visited, want) {
				t.Fatalf("search order changed: %v want %v", visited, want)
			}
		})
	}
}

func TestApplicationUserSetupPreservesPrimaryGroupAndEnvironment(t *testing.T) {
	for _, spec := range []string{"0", "0:", "0:1234", "root", "root:", "root:root", "root:1234"} {
		t.Run(spec, func(t *testing.T) {
			t.Setenv("HOME", "/inherited")
			t.Setenv("USER", "inherited")
			credential, err := prepareUserSpec(spec, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(credential.Groups, []uint32{credential.Gid}) {
				t.Fatalf("unexpected supplementary groups: %#v", credential)
			}
			if _, err := strconv.Atoi(strings.Split(spec, ":")[0]); err == nil {
				if os.Getenv("HOME") != "/inherited" || os.Getenv("USER") != "inherited" {
					t.Fatal("numeric user changed inherited account environment")
				}
			} else if os.Getenv("USER") != "root" || os.Getenv("HOME") != "/root" {
				t.Fatalf("named user environment changed: HOME=%q USER=%q", os.Getenv("HOME"), os.Getenv("USER"))
			}
		})
	}
}

func TestApplicationLaunchFailureDoesNotRetryWithElevatedCredentials(t *testing.T) {
	for _, stage := range []string{"prepare", "groups", "gid", "uid", "exec"} {
		t.Run(stage, func(t *testing.T) {
			failure := errors.New("startup refused")
			check := func(at string) error {
				if at == stage {
					return failure
				}
				return nil
			}
			execs := 0
			ops := applicationOperations{
				chdir: func(string) error { return nil }, warn: func(string, error) {},
				prepareUser: func(string, []string, []guestVolume) (*syscall.Credential, error) {
					return &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{1000}}, check("prepare")
				},
				setgroups: func([]int) error { return check("groups") }, setgid: func(int) error { return check("gid") }, setuid: func(int) error { return check("uid") },
				resolve: func(value string) string { return value }, environ: func() []string { return nil },
				exec: func(string, []string, []string) error { execs++; return check("exec") },
			}
			if err := launchApplication(applicationLaunch{Metadata: runtimeMetadata{User: "1000"}, Argv: []string{"/app"}}, ops); !errors.Is(err, failure) {
				t.Fatalf("failure=%v", err)
			}
			if execs > 1 || (stage != "exec" && execs != 0) {
				t.Fatalf("startup retried: %d execs", execs)
			}
		})
	}
}
