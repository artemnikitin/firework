// Package networktest provides an in-memory host network for tests: a fake
// kernel whose DNAT rules are appended, listed and deleted the way iptables
// does it, so tests can assert on what traffic would reach, not on calls.
package networktest

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/network"
)

// HostIP and Interface are the fake host's ingress address and interface.
const (
	HostIP    = "10.0.0.5"
	Interface = "eth0"
)

// Kernel is a fake host network. Its zero value is ready to use. It is safe
// for concurrent use.
type Kernel struct {
	mu      sync.Mutex
	rules   []network.PortForwardRule
	devices map[string]bool
	// Fault injection. Each hook may be nil.
	SetupErr  func(service string, hostPort int) error
	ListErr   func() error
	DeleteErr func(network.PortForwardRule) error
	DeviceErr func(name string) error
	// Deleted records every rule a delete removed, in order.
	Deleted []network.PortForwardRule
	// Unparsed is listed as DNAT rules whose destination cannot be read.
	Unparsed []string
}

func (k *Kernel) Setup(svc config.ServiceConfig) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if svc.Network == nil {
		return nil
	}
	if k.devices == nil {
		k.devices = make(map[string]bool)
	}
	tap := svc.Network.Interface
	if tap == "" {
		tap = "tap-" + svc.Name
	}
	if k.DeviceErr != nil {
		if err := k.DeviceErr(tap); err != nil {
			return err
		}
	}
	k.devices[tap] = true
	if svc.Network.HostDevName != "" {
		k.devices["br-"+svc.Name] = true
	}
	return nil
}

func (k *Kernel) DeleteTAP(name string) error    { return k.deleteDevice(name) }
func (k *Kernel) DeleteBridge(name string) error { return k.deleteDevice(name) }

func (k *Kernel) deleteDevice(name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.DeviceErr != nil {
		if err := k.DeviceErr(name); err != nil {
			return err
		}
	}
	delete(k.devices, name)
	return nil
}

// HasDevice reports whether a TAP or bridge exists.
func (k *Kernel) HasDevice(name string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.devices[name]
}

// SetupPortForward appends the service's tagged rule unless an identical one
// exists, like the real manager's check-then-append.
func (k *Kernel) SetupPortForward(service string, hostPort int, guestIP string, vmPort int) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.SetupErr != nil {
		if err := k.SetupErr(service, hostPort); err != nil {
			return err
		}
	}
	k.appendLocked(Rule(network.PortForwardTag(service), hostPort, guestIP, vmPort))
	return nil
}

// Rule builds the rule the real manager lists for a port forward on the fake
// host's ingress, carrying tag as its comment (none when empty).
func Rule(tag string, hostPort int, guestIP string, vmPort int) network.PortForwardRule {
	args := []string{"-d", HostIP + "/32", "-i", Interface, "-p", "tcp", "-m", "tcp", "--dport", strconv.Itoa(hostPort)}
	if tag != "" {
		args = append(args, "-m", "comment", "--comment", tag)
	}
	args = append(args, "-j", "DNAT", "--to-destination", fmt.Sprintf("%s:%d", guestIP, vmPort))
	return network.PortForwardRule{Args: args, Interface: Interface, HostIP: HostIP, HostPort: hostPort, GuestIP: guestIP, VMPort: vmPort, Tag: tag}
}

// Append adds a rule as if something else had installed it, such as an
// earlier agent or an operator.
func (k *Kernel) Append(rule network.PortForwardRule) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.appendLocked(rule)
}

func (k *Kernel) appendLocked(rule network.PortForwardRule) {
	for _, existing := range k.rules {
		if slices.Equal(existing.Args, rule.Args) {
			return
		}
	}
	rule.Args = slices.Clone(rule.Args)
	k.rules = append(k.rules, rule)
}

func (k *Kernel) ListPortForwards() (network.PortForwardListing, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.ListErr != nil {
		if err := k.ListErr(); err != nil {
			return network.PortForwardListing{}, err
		}
	}
	rules := make([]network.PortForwardRule, len(k.rules))
	for i, rule := range k.rules {
		rule.Args = slices.Clone(rule.Args)
		rules[i] = rule
	}
	return network.PortForwardListing{Rules: rules, Unparsed: slices.Clone(k.Unparsed)}, nil
}

func (k *Kernel) DeletePortForwardRule(rule network.PortForwardRule) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.DeleteErr != nil {
		if err := k.DeleteErr(rule); err != nil {
			return err
		}
	}
	for i, existing := range k.rules {
		if slices.Equal(existing.Args, rule.Args) {
			k.rules = slices.Delete(k.rules, i, i+1)
			k.Deleted = append(k.Deleted, existing)
			return nil
		}
	}
	return nil // Already gone counts as deleted, as with iptables.
}

// Rules returns the installed rules in chain order.
func (k *Kernel) Rules() []network.PortForwardRule {
	listing, _ := (&Kernel{rules: k.snapshot()}).ListPortForwards()
	return listing.Rules
}

func (k *Kernel) snapshot() []network.PortForwardRule {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.rules)
}

// Delivers reports where external traffic to hostPort goes: the first
// matching rule wins, as in the kernel.
func (k *Kernel) Delivers(hostPort int) (network.PortForwardRule, bool) {
	for _, rule := range k.snapshot() {
		if rule.HostPort == hostPort && (rule.Interface == "" || rule.Interface == Interface) {
			return rule, true
		}
	}
	return network.PortForwardRule{}, false
}

// RulesTo returns the rules forwarding to guestIP.
func (k *Kernel) RulesTo(guestIP string) []network.PortForwardRule {
	var rules []network.PortForwardRule
	for _, rule := range k.snapshot() {
		if rule.GuestIP == guestIP {
			rules = append(rules, rule)
		}
	}
	return rules
}

// ErrInjected is a convenient error for fault hooks.
var ErrInjected = errors.New("injected network failure")
