package network

import (
	"strings"
	"testing"
)

func TestParseDefaultRouteInterface(t *testing.T) {
	t.Parallel()

	out := "default via 10.0.100.1 dev enp3s0 proto dhcp src 10.0.100.91 metric 512"
	iface, err := parseDefaultRouteInterface(out)
	if err != nil {
		t.Fatalf("parseDefaultRouteInterface returned error: %v", err)
	}
	if iface != "enp3s0" {
		t.Fatalf("unexpected interface: got %q, want %q", iface, "enp3s0")
	}
}

func TestParseDefaultRouteInterfaceError(t *testing.T) {
	t.Parallel()

	_, err := parseDefaultRouteInterface("10.0.100.0/24 dev enp3s0 proto kernel scope link")
	if err == nil {
		t.Fatal("expected error for output without default route, got nil")
	}
}

func TestParseInterfaceIPv4(t *testing.T) {
	t.Parallel()

	out := strings.Join([]string{
		"2: enp3s0    inet 10.0.100.91/24 metric 512 brd 10.0.100.255 scope global dynamic enp3s0",
		"2: enp3s0    inet6 fe80::1234/64 scope link",
	}, "\n")

	ip, err := parseInterfaceIPv4(out)
	if err != nil {
		t.Fatalf("parseInterfaceIPv4 returned error: %v", err)
	}
	if ip != "10.0.100.91" {
		t.Fatalf("unexpected ip: got %q, want %q", ip, "10.0.100.91")
	}
}

func TestParseInterfaceIPv4Error(t *testing.T) {
	t.Parallel()

	_, err := parseInterfaceIPv4("2: enp3s0    inet6 fe80::1234/64 scope link")
	if err == nil {
		t.Fatal("expected error for output without inet IPv4, got nil")
	}
}

func TestScopedPortForwardSpec(t *testing.T) {
	t.Parallel()

	spec := scopedPortForwardSpec("enp3s0", "10.0.100.91", 19300, "172.16.0.6", 9300)
	got := strings.Join(spec, " ")

	wants := []string{
		"-i enp3s0",
		"-d 10.0.100.91/32",
		"--dport 19300",
		"--to-destination 172.16.0.6:9300",
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Fatalf("scoped spec missing %q: %s", want, got)
		}
	}
}
