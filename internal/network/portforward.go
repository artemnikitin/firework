package network

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// portForwardTagPrefix marks the DNAT rules the agent installs. The kernel's
// own rule list, not any record, is the inventory of what can deliver traffic,
// and the tag says which service a listed rule was installed for.
const portForwardTagPrefix = "firework:pf:"

var plainTagName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// PortForwardTag returns the comment that tags a service's DNAT rules. A name
// outside a conservative character set is hashed, so a tag never needs
// quoting and always fits iptables' 256-byte comment limit.
func PortForwardTag(service string) string {
	if plainTagName.MatchString(service) {
		return portForwardTagPrefix + service
	}
	sum := sha256.Sum256([]byte(service))
	return portForwardTagPrefix + "h-" + hex.EncodeToString(sum[:8])
}

// PortForwardRule is one TCP DNAT rule of the nat PREROUTING chain, as the
// kernel lists it.
type PortForwardRule struct {
	// Args is the listed rule after "-A PREROUTING". Deleting by these exact
	// arguments removes this rule and no other.
	Args      []string
	Interface string
	// HostIP is the -d match without a /32 suffix, or empty when the rule
	// matches every destination.
	HostIP   string
	HostPort int
	GuestIP  string
	VMPort   int
	// Tag is the rule's comment, or empty.
	Tag string
}

// Tagged reports whether the agent installed this rule.
func (r PortForwardRule) Tagged() bool { return strings.HasPrefix(r.Tag, portForwardTagPrefix) }

// TaggedFor reports whether the agent installed this rule for service.
func (r PortForwardRule) TaggedFor(service string) bool { return r.Tag == PortForwardTag(service) }

// Targets reports whether the rule forwards hostPort to guestIP:vmPort.
func (r PortForwardRule) Targets(hostPort int, guestIP string, vmPort int) bool {
	return r.HostPort == hostPort && r.VMPort == vmPort && sameIP(r.GuestIP, guestIP)
}

// String is the rule as iptables lists it, for status and logs.
func (r PortForwardRule) String() string { return "-A PREROUTING " + strings.Join(r.Args, " ") }

// PortForwardListing is the parsed nat PREROUTING chain.
type PortForwardListing struct {
	Rules []PortForwardRule
	// Unparsed holds DNAT rules whose host port or destination could not be
	// read, such as port ranges or negated matches. Any of them may deliver
	// traffic anywhere, so callers must treat them as unknown claims.
	Unparsed []string
}

// ParsePortForwardListing parses `iptables -t nat -S PREROUTING`. It keeps
// every DNAT rule, whatever its destination: a foreign rule for a host port
// takes that port's traffic as surely as one of the agent's own. Rules for
// another protocol, and rules with another target, cannot reach a TCP port
// forward and are skipped.
func ParsePortForwardListing(output string) (PortForwardListing, error) {
	var listing PortForwardListing
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "-A PREROUTING ") {
			continue
		}
		args, err := splitRuleArgs(strings.TrimPrefix(line, "-A PREROUTING "))
		if err != nil {
			return PortForwardListing{}, fmt.Errorf("parsing %q: %w", line, err)
		}
		rule, dnat, ok := parseDNATRule(args)
		switch {
		case !dnat:
		case ok:
			listing.Rules = append(listing.Rules, rule)
		default:
			listing.Unparsed = append(listing.Unparsed, line)
		}
	}
	return listing, nil
}

// parseDNATRule reads one listed rule. dnat reports whether it is a DNAT rule
// that may apply to TCP; ok whether every field this package relies on was
// read unambiguously.
func parseDNATRule(args []string) (rule PortForwardRule, dnat, ok bool) {
	rule.Args = args
	protocol, target := "", ""
	ok = true
	for i := 0; i < len(args); i++ {
		value := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			ok = false
			return ""
		}
		switch args[i] {
		case "!":
			ok = false
		case "-i":
			rule.Interface = value()
		case "-d":
			rule.HostIP = strings.TrimSuffix(value(), "/32")
		case "-p":
			protocol = value()
		case "--dport":
			port, err := strconv.Atoi(value())
			if err != nil || port < 1 || port > 65535 {
				ok = false
			}
			rule.HostPort = port
		case "--comment":
			rule.Tag = value()
		case "-j":
			target = value()
		case "--to-destination":
			destination := value()
			host, port, err := net.SplitHostPort(destination)
			if err != nil {
				// Without a port, DNAT keeps the original destination port.
				host, port = destination, ""
			}
			if net.ParseIP(host) == nil {
				ok = false
			}
			rule.GuestIP = host
			if port != "" {
				vmPort, err := strconv.Atoi(port)
				if err != nil || vmPort < 1 || vmPort > 65535 {
					ok = false
				}
				rule.VMPort = vmPort
			}
		}
	}
	if target != "DNAT" || (protocol != "" && protocol != "tcp" && protocol != "6") {
		return PortForwardRule{}, false, false
	}
	if rule.VMPort == 0 {
		rule.VMPort = rule.HostPort
	}
	if protocol == "" || rule.HostPort == 0 || rule.GuestIP == "" {
		ok = false
	}
	return rule, true, ok
}

// splitRuleArgs splits one listed rule into its arguments. iptables quotes an
// argument that contains spaces or quotes, such as a comment, and escapes
// embedded quotes and backslashes.
func splitRuleArgs(rule string) ([]string, error) {
	var args []string
	var current strings.Builder
	inArg, quoted := false, false
	for i := 0; i < len(rule); i++ {
		c := rule[i]
		switch {
		case quoted && c == '\\' && i+1 < len(rule):
			i++
			current.WriteByte(rule[i])
		case c == '"':
			quoted = !quoted
			inArg = true
		case !quoted && (c == ' ' || c == '\t'):
			if inArg {
				args = append(args, current.String())
				current.Reset()
				inArg = false
			}
		default:
			current.WriteByte(c)
			inArg = true
		}
	}
	if quoted {
		return nil, fmt.Errorf("unterminated quote")
	}
	if inArg {
		args = append(args, current.String())
	}
	return args, nil
}

func sameIP(a, b string) bool {
	if a == b {
		return true
	}
	pa, pb := net.ParseIP(a), net.ParseIP(b)
	return pa != nil && pb != nil && pa.Equal(pb)
}

// ListPortForwards lists the host's TCP DNAT rules in the nat PREROUTING
// chain, the only chain the agent installs port forwards in.
func (m *Manager) ListPortForwards() (PortForwardListing, error) {
	out, err := runOutput("iptables", "-t", "nat", "-S", "PREROUTING")
	if err != nil {
		return PortForwardListing{}, fmt.Errorf("listing port forwards: %w", err)
	}
	return ParsePortForwardListing(out)
}

// DeletePortForwardRule deletes one listed rule by its exact arguments, so
// only that rule goes, whatever host address or interface it was installed
// with. A rule that is already gone counts as deleted.
func (m *Manager) DeletePortForwardRule(rule PortForwardRule) error {
	m.logger.Info("deleting port forward", "rule", rule.String())
	if err := removeIPTablesRule("nat", "PREROUTING", rule.Args...); err != nil {
		return fmt.Errorf("deleting port forward %s: %w", rule.String(), err)
	}
	return nil
}
