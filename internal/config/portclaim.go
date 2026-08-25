package config

import (
	"fmt"
	"sort"
	"strings"
)

// ProtocolTCP is the effective protocol of every port forward today. The agent
// installs DNAT rules with "-p tcp", so a claim is currently identified by that
// protocol plus the host port. Protocol is carried explicitly in PortClaim so
// adding UDP forwards later widens the key without changing its callers.
const ProtocolTCP = "tcp"

// PortClaim is a node-exclusive host endpoint requested by a service. Two
// services on the same node may not hold the same claim: their DNAT rules would
// match identical traffic and only the first installed rule would win.
type PortClaim struct {
	Protocol string
	HostPort int
}

func (c PortClaim) String() string {
	return fmt.Sprintf("%s/%d", c.Protocol, c.HostPort)
}

// PortClaims returns the host-port claims a service makes, in declaration
// order. Entries without a positive host port are not forwarded by the agent
// and therefore claim nothing.
func (s ServiceConfig) PortClaims() []PortClaim {
	var claims []PortClaim
	for _, pf := range s.PortForwards {
		if pf.HostPort <= 0 {
			continue
		}
		claims = append(claims, PortClaim{Protocol: ProtocolTCP, HostPort: pf.HostPort})
	}
	return claims
}

// DuplicatePortClaims returns the claims a single service requests more than
// once, sorted by host port. Such a service is never schedulable anywhere: the
// duplicate DNAT rules conflict with each other on any node.
func (s ServiceConfig) DuplicatePortClaims() []PortClaim {
	seen := make(map[PortClaim]int, len(s.PortForwards))
	for _, claim := range s.PortClaims() {
		seen[claim]++
	}
	var dupes []PortClaim
	for claim, count := range seen {
		if count > 1 {
			dupes = append(dupes, claim)
		}
	}
	sortClaims(dupes)
	return dupes
}

// PortClaimConflict reports a claim requested by more than one service in the
// same node scope. Services are sorted so the message is stable.
type PortClaimConflict struct {
	Claim    PortClaim
	Services []string
}

func (c PortClaimConflict) String() string {
	return fmt.Sprintf("host port %d (%s) is claimed by %s",
		c.Claim.HostPort, c.Claim.Protocol, strings.Join(c.Services, ", "))
}

// ConflictingPortClaims returns claims held by more than one of the given
// services. It is the cross-service half of the invariant only; use
// DuplicatePortClaims for claims a single service repeats.
func ConflictingPortClaims(services []ServiceConfig) []PortClaimConflict {
	holders := make(map[PortClaim][]string)
	for _, svc := range services {
		claimed := make(map[PortClaim]bool)
		for _, claim := range svc.PortClaims() {
			if claimed[claim] {
				continue
			}
			claimed[claim] = true
			holders[claim] = append(holders[claim], svc.Name)
		}
	}

	var conflicts []PortClaimConflict
	for claim, names := range holders {
		if len(names) < 2 {
			continue
		}
		sorted := append([]string(nil), names...)
		sort.Strings(sorted)
		conflicts = append(conflicts, PortClaimConflict{Claim: claim, Services: sorted})
	}
	sort.Slice(conflicts, func(i, j int) bool {
		return claimLess(conflicts[i].Claim, conflicts[j].Claim)
	})
	return conflicts
}

// ValidateNodePortClaims rejects a rendered node config whose services cannot
// coexist on one node. It is the agent's admission boundary: a stale,
// hand-written, or older-controller config must be refused before any
// networking or service state changes, because the resulting DNAT rules would
// silently deliver traffic to the wrong guest.
func ValidateNodePortClaims(nc NodeConfig) error {
	var problems []string
	for _, svc := range nc.Services {
		for _, dupe := range svc.DuplicatePortClaims() {
			problems = append(problems, fmt.Sprintf(
				"service %s claims host port %d (%s) more than once", svc.Name, dupe.HostPort, dupe.Protocol))
		}
	}
	for _, conflict := range ConflictingPortClaims(nc.Services) {
		problems = append(problems, conflict.String())
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("conflicting host-port claims on node %s: %s", nc.Node, strings.Join(problems, "; "))
}

func sortClaims(claims []PortClaim) {
	sort.Slice(claims, func(i, j int) bool { return claimLess(claims[i], claims[j]) })
}

func claimLess(a, b PortClaim) bool {
	if a.HostPort != b.HostPort {
		return a.HostPort < b.HostPort
	}
	return a.Protocol < b.Protocol
}
