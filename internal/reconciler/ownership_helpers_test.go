package reconciler

import (
	"testing"

	"github.com/artemnikitin/firework/internal/network"
)

// pendingRules returns the installed rules that departing records still
// cover and no running VM claims: the port forwards whose teardown is still
// pending.
func pendingRules(t *testing.T, r *Reconciler) []network.PortForwardRule {
	t.Helper()
	listing, err := r.networkMgr.ListPortForwards()
	if err != nil {
		t.Fatal(err)
	}
	actual := r.vmManager.List()
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	var pending []network.PortForwardRule
	for _, rule := range listing.Rules {
		if claimed, retained := ruleClaim(rule, actual); claimed && !retained {
			continue
		}
		for _, a := range r.allocations {
			if a.Phase == phaseDeparting && a.covers(rule) {
				pending = append(pending, rule)
				break
			}
		}
	}
	return pending
}

// recordsIn returns the records in the given phase.
func recordsIn(r *Reconciler, phase allocationPhase) []allocation {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	var records []allocation
	for _, a := range r.sortedAllocationsLocked() {
		if a.Phase == phase {
			records = append(records, a)
		}
	}
	return records
}

func pendingHostPorts(t *testing.T, r *Reconciler) []int {
	t.Helper()
	var ports []int
	for _, rule := range pendingRules(t, r) {
		ports = append(ports, rule.HostPort)
	}
	return ports
}
