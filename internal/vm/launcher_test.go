package vm

import (
	"path/filepath"
	"testing"
)

func TestSystemdLaunchSupportedRequiresRootOnLinux(t *testing.T) {
	present := t.TempDir()
	absent := filepath.Join(present, "missing")

	cases := []struct {
		name       string
		goos       string
		euid       int
		systemdDir string
		want       bool
	}{
		{name: "linux root with systemd", goos: "linux", euid: 0, systemdDir: present, want: true},
		{name: "linux unprivileged with systemd", goos: "linux", euid: 1001, systemdDir: present, want: false},
		{name: "linux root without systemd", goos: "linux", euid: 0, systemdDir: absent, want: false},
		{name: "darwin root", goos: "darwin", euid: 0, systemdDir: present, want: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := systemdLaunchSupported(testCase.goos, testCase.euid, testCase.systemdDir); got != testCase.want {
				t.Fatalf("systemdLaunchSupported(%q, %d, systemd present=%t) = %t, want %t",
					testCase.goos, testCase.euid, testCase.systemdDir == present, got, testCase.want)
			}
		})
	}
}
