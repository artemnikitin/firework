package network

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/artemnikitin/firework/internal/config"
)

// Manager handles network setup and teardown for Firecracker microVMs.
// It creates TAP devices and optionally bridges them to a host interface.
type Manager struct {
	logger     *slog.Logger
	bridgeName string // shared bridge name, set by InitBridge
}

// NewManager creates a new network manager.
func NewManager(logger *slog.Logger) *Manager {
	return &Manager{logger: logger}
}

// InitBridge creates a shared bridge for all VMs, assigns the gateway IP,
// and enables IP forwarding. This must be called before Setup() if you want
// VMs to share a single bridge instead of each getting their own.
func (m *Manager) InitBridge(name, gatewayIP, subnet string) error {
	m.logger.Info("initializing shared bridge", "bridge", name, "gateway", gatewayIP)

	// Backward-compatibility cleanup: older deployments used "br-firework".
	// If it still exists, the kernel can prefer its route for vm_subnet and
	// break health checks with "no route to host".
	m.cleanupLegacyBridge(name, "br-firework")

	if !deviceExists(name) {
		if err := run("ip", "link", "add", "name", name, "type", "bridge"); err != nil {
			return fmt.Errorf("creating bridge %s: %w", name, err)
		}
	}

	// Assign gateway IP (with subnet mask).
	gatewayCIDR := gatewayIP + "/" + subnetMask(subnet)
	if err := run("ip", "addr", "add", gatewayCIDR, "dev", name); err != nil {
		// Ignore "already exists" errors.
		if !strings.Contains(err.Error(), "RTNETLINK answers: File exists") {
			return fmt.Errorf("assigning gateway IP: %w", err)
		}
	}

	if err := run("ip", "link", "set", name, "up"); err != nil {
		return fmt.Errorf("bringing bridge up: %w", err)
	}

	// Explicitly pin vm_subnet route to this bridge so stale bridges/routes
	// cannot steal traffic to guest IPs.
	if err := m.pinSubnetRoute(name, gatewayIP, subnet); err != nil {
		m.logger.Warn("failed to pin vm subnet route to bridge",
			"bridge", name, "subnet", subnet, "error", err)
	}

	// Enable IP forwarding globally.
	if err := run("sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		m.logger.Warn("failed to enable ip_forward", "error", err)
	}

	m.bridgeName = name
	return nil
}

func (m *Manager) cleanupLegacyBridge(desired, legacy string) {
	if legacy == "" || legacy == desired || !deviceExists(legacy) {
		return
	}

	m.logger.Warn("removing legacy bridge to avoid route conflicts",
		"legacy_bridge", legacy, "active_bridge", desired)

	_ = run("ip", "link", "set", legacy, "down")
	if err := run("ip", "link", "del", legacy); err != nil {
		m.logger.Warn("failed to delete legacy bridge", "bridge", legacy, "error", err)
	}
}

func (m *Manager) pinSubnetRoute(bridgeName, gatewayIP, subnet string) error {
	src := gatewayIP
	if idx := strings.Index(src, "/"); idx != -1 {
		src = src[:idx]
	}

	args := []string{"route", "replace", subnet, "dev", bridgeName}
	if src != "" {
		args = append(args, "src", src)
	}

	if err := run("ip", args...); err != nil {
		return fmt.Errorf("ip %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// SetupPortForward creates an iptables DNAT rule forwarding traffic from
// a host port to a VM port on the given guest IP.
func (m *Manager) SetupPortForward(hostPort int, guestIP string, vmPort int) error {
	m.logger.Info("setting up port forward", "host_port", hostPort, "guest_ip", guestIP, "vm_port", vmPort)

	// Scope DNAT to traffic arriving on the external interface and addressed
	// to the host IP. This avoids hijacking guest-to-peer traffic that also
	// targets the same host port (e.g. cross-node ES transport on 19300).
	outInterface, hostIP, err := m.resolveHostIngressContext()
	if err != nil {
		return fmt.Errorf("resolving host ingress context: %w", err)
	}

	spec := scopedPortForwardSpec(outInterface, hostIP, hostPort, guestIP, vmPort)
	if err := ensureIPTablesRule("nat", "PREROUTING", spec...); err != nil {
		return fmt.Errorf("adding scoped port-forward rule: %w", err)
	}
	return nil
}

// TeardownPortForward removes the iptables DNAT rule for a port forward.
func (m *Manager) TeardownPortForward(hostPort int, guestIP string, vmPort int) error {
	m.logger.Info("tearing down port forward", "host_port", hostPort, "guest_ip", guestIP, "vm_port", vmPort)

	var errs []error

	// Remove scoped rule first (new behavior).
	if outInterface, hostIP, err := m.resolveHostIngressContext(); err == nil {
		spec := scopedPortForwardSpec(outInterface, hostIP, hostPort, guestIP, vmPort)
		if err := removeIPTablesRule("nat", "PREROUTING", spec...); err != nil {
			errs = append(errs, fmt.Errorf("removing scoped port-forward rule: %w", err))
		}
	} else {
		m.logger.Warn("failed to resolve host ingress context for scoped cleanup, trying legacy rule only",
			"host_port", hostPort, "error", err)
	}

	// Backward-compatible cleanup for older unscoped rules.
	legacySpec := legacyPortForwardSpec(hostPort, guestIP, vmPort)
	if err := removeIPTablesRule("nat", "PREROUTING", legacySpec...); err != nil {
		errs = append(errs, fmt.Errorf("removing legacy port-forward rule: %w", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("tearing down port-forward rules: %v", errs)
	}
	return nil
}

func scopedPortForwardSpec(outInterface, hostIP string, hostPort int, guestIP string, vmPort int) []string {
	return []string{
		"-i", outInterface,
		"-d", hostIP + "/32",
		"-p", "tcp",
		"-m", "tcp",
		"--dport", fmt.Sprintf("%d", hostPort),
		"-j", "DNAT",
		"--to-destination", fmt.Sprintf("%s:%d", guestIP, vmPort),
	}
}

func legacyPortForwardSpec(hostPort int, guestIP string, vmPort int) []string {
	return []string{
		"-p", "tcp",
		"-m", "tcp",
		"--dport", fmt.Sprintf("%d", hostPort),
		"-j", "DNAT",
		"--to-destination", fmt.Sprintf("%s:%d", guestIP, vmPort),
	}
}

func (m *Manager) resolveHostIngressContext() (string, string, error) {
	routeOut, err := runOutput("ip", "route", "show", "default")
	if err != nil {
		return "", "", fmt.Errorf("detecting default route: %w", err)
	}
	outInterface, err := parseDefaultRouteInterface(routeOut)
	if err != nil {
		return "", "", err
	}

	addrOut, err := runOutput("ip", "-4", "-o", "addr", "show", "dev", outInterface, "scope", "global")
	if err != nil {
		return "", "", fmt.Errorf("detecting host IPv4 on %s: %w", outInterface, err)
	}
	hostIP, err := parseInterfaceIPv4(addrOut)
	if err != nil {
		return "", "", fmt.Errorf("parsing host IPv4 on %s: %w", outInterface, err)
	}
	return outInterface, hostIP, nil
}

func parseDefaultRouteInterface(output string) (string, error) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 || fields[0] != "default" {
			continue
		}
		for i := 0; i < len(fields)-1; i++ {
			if fields[i] == "dev" && fields[i+1] != "" {
				return fields[i+1], nil
			}
		}
	}
	return "", fmt.Errorf("default route interface not found in output: %q", strings.TrimSpace(output))
}

func parseInterfaceIPv4(output string) (string, error) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		for i := 0; i < len(fields)-1; i++ {
			if fields[i] != "inet" {
				continue
			}
			cidr := fields[i+1]
			ip := cidr
			if slash := strings.Index(cidr, "/"); slash != -1 {
				ip = cidr[:slash]
			}
			if ip != "" {
				return ip, nil
			}
		}
	}
	return "", fmt.Errorf("IPv4 address not found in output: %q", strings.TrimSpace(output))
}

func ensureIPTablesRule(table, chain string, spec ...string) error {
	checkArgs := append([]string{"-t", table, "-C", chain}, spec...)
	if err := run("iptables", checkArgs...); err == nil {
		return nil
	} else if !isRuleMissingError(err) {
		return fmt.Errorf("checking existing iptables rule: %w", err)
	}

	addArgs := append([]string{"-t", table, "-A", chain}, spec...)
	if err := run("iptables", addArgs...); err != nil {
		return err
	}
	return nil
}

func removeIPTablesRule(table, chain string, spec ...string) error {
	delArgs := append([]string{"-t", table, "-D", chain}, spec...)
	if err := run("iptables", delArgs...); err != nil && !isRuleMissingError(err) {
		return err
	}
	return nil
}

func isRuleMissingError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "No chain/target/match by that name") ||
		strings.Contains(msg, "Bad rule (does a matching rule exist in that chain?)")
}

// Setup creates the network devices needed for a service's microVM.
// It creates a TAP device and optionally attaches it to a bridge.
func (m *Manager) Setup(svc config.ServiceConfig) error {
	if svc.Network == nil {
		return nil
	}

	tapName := svc.Network.Interface
	if tapName == "" {
		tapName = fmt.Sprintf("tap-%s", svc.Name)
	}

	m.logger.Info("setting up network", "service", svc.Name, "tap", tapName)

	// Create TAP device.
	if err := m.createTAP(tapName); err != nil {
		return fmt.Errorf("creating TAP device %s: %w", tapName, err)
	}

	// Attach TAP to shared bridge if initialized, otherwise fall back to
	// per-service bridge when a host device is specified.
	if m.bridgeName != "" {
		if err := run("ip", "link", "set", tapName, "master", m.bridgeName); err != nil {
			_ = m.deleteTAP(tapName)
			return fmt.Errorf("attaching TAP to shared bridge: %w", err)
		}
	} else if svc.Network.HostDevName != "" {
		bridgeName := fmt.Sprintf("br-%s", svc.Name)
		if err := m.setupBridge(bridgeName, tapName, svc.Network.HostDevName); err != nil {
			_ = m.deleteTAP(tapName)
			return fmt.Errorf("setting up bridge: %w", err)
		}
	}

	m.logger.Info("network setup complete", "service", svc.Name, "tap", tapName)
	return nil
}

// Teardown removes the network devices created for a service.
func (m *Manager) Teardown(svc config.ServiceConfig) error {
	if svc.Network == nil {
		return nil
	}

	tapName := svc.Network.Interface
	if tapName == "" {
		tapName = fmt.Sprintf("tap-%s", svc.Name)
	}

	m.logger.Info("tearing down network", "service", svc.Name, "tap", tapName)

	var errs []error

	// Remove bridge if it exists.
	if svc.Network.HostDevName != "" {
		bridgeName := fmt.Sprintf("br-%s", svc.Name)
		if err := m.deleteBridge(bridgeName); err != nil {
			errs = append(errs, err)
		}
	}

	// Remove TAP device.
	if err := m.deleteTAP(tapName); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("network teardown errors: %v", errs)
	}
	return nil
}

// createTAP creates a TAP device and brings it up.
func (m *Manager) createTAP(name string) error {
	// Check if TAP already exists.
	if deviceExists(name) {
		m.logger.Debug("TAP device already exists", "tap", name)
		return nil
	}

	if err := run("ip", "tuntap", "add", "dev", name, "mode", "tap"); err != nil {
		return fmt.Errorf("creating TAP: %w", err)
	}

	if err := run("ip", "link", "set", name, "up"); err != nil {
		return fmt.Errorf("bringing TAP up: %w", err)
	}

	return nil
}

// deleteTAP removes a TAP device.
func (m *Manager) deleteTAP(name string) error {
	if !deviceExists(name) {
		return nil
	}

	if err := run("ip", "link", "del", name); err != nil {
		return fmt.Errorf("deleting TAP %s: %w", name, err)
	}
	return nil
}

// setupBridge creates a bridge and attaches the TAP and host device to it.
func (m *Manager) setupBridge(bridgeName, tapName, hostDev string) error {
	// Create bridge if it doesn't exist.
	if !deviceExists(bridgeName) {
		if err := run("ip", "link", "add", "name", bridgeName, "type", "bridge"); err != nil {
			return fmt.Errorf("creating bridge: %w", err)
		}
	}

	// Attach TAP to bridge.
	if err := run("ip", "link", "set", tapName, "master", bridgeName); err != nil {
		return fmt.Errorf("attaching TAP to bridge: %w", err)
	}

	// Attach host device to bridge.
	if err := run("ip", "link", "set", hostDev, "master", bridgeName); err != nil {
		return fmt.Errorf("attaching host device to bridge: %w", err)
	}

	// Bring bridge up.
	if err := run("ip", "link", "set", bridgeName, "up"); err != nil {
		return fmt.Errorf("bringing bridge up: %w", err)
	}

	// Enable IP forwarding for this bridge.
	if err := run("sysctl", "-w", fmt.Sprintf("net.ipv4.conf.%s.forwarding=1", bridgeName)); err != nil {
		m.logger.Warn("failed to enable forwarding on bridge", "bridge", bridgeName, "error", err)
	}

	return nil
}

// deleteBridge removes a bridge device.
func (m *Manager) deleteBridge(name string) error {
	if !deviceExists(name) {
		return nil
	}

	// Bring it down first.
	_ = run("ip", "link", "set", name, "down")

	if err := run("ip", "link", "del", name); err != nil {
		return fmt.Errorf("deleting bridge %s: %w", name, err)
	}
	return nil
}

// SetupMasquerade configures iptables masquerading so VMs can reach the
// internet through the host. The subnet should be in CIDR notation (e.g. "172.16.0.0/24").
func (m *Manager) SetupMasquerade(subnet, outInterface string) error {
	m.logger.Info("setting up masquerade", "subnet", subnet, "interface", outInterface)

	if err := run("iptables", "-t", "nat", "-A", "POSTROUTING",
		"-s", subnet, "-o", outInterface, "-j", "MASQUERADE"); err != nil {
		return fmt.Errorf("setting up masquerade: %w", err)
	}

	// Allow forwarding for the subnet.
	if err := run("iptables", "-A", "FORWARD",
		"-s", subnet, "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("allowing forward from subnet: %w", err)
	}
	if err := run("iptables", "-A", "FORWARD",
		"-d", subnet, "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("allowing forward to subnet: %w", err)
	}

	return nil
}

// deviceExists checks if a network device exists.
func deviceExists(name string) bool {
	err := run("ip", "link", "show", name)
	return err == nil
}

// run executes a command and returns an error if it fails.
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return nil
}

func runOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// subnetMask extracts the prefix length from a CIDR string.
// e.g. "172.16.0.0/24" → "24".
func subnetMask(cidr string) string {
	if idx := strings.LastIndex(cidr, "/"); idx != -1 {
		return cidr[idx+1:]
	}
	return "24"
}

// SubnetMaskBits returns the dotted-decimal netmask for a CIDR prefix length.
// e.g. "172.16.0.0/24" → "255.255.255.0".
func SubnetMaskBits(cidr string) string {
	mask := subnetMask(cidr)
	switch mask {
	case "8":
		return "255.0.0.0"
	case "16":
		return "255.255.0.0"
	case "24":
		return "255.255.255.0"
	case "28":
		return "255.255.255.240"
	default:
		return "255.255.255.0"
	}
}
