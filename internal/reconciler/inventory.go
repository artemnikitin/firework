package reconciler

import (
	"fmt"
	"slices"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/network"
	"github.com/artemnikitin/firework/internal/vm"
)

// claimsForward reports whether svc's configuration forwards rule's host port
// to rule's guest and VM port.
func claimsForward(svc config.ServiceConfig, rule network.PortForwardRule) bool {
	return slices.ContainsFunc(svc.PortForwards, func(pf config.PortForward) bool {
		return rule.Targets(pf.HostPort, guestIPOf(svc), pf.VMPort)
	})
}

func claimsHostPort(svc config.ServiceConfig, hostPort int) bool {
	return slices.ContainsFunc(svc.PortForwards, func(pf config.PortForward) bool { return pf.HostPort == hostPort })
}

// describeRule names a rule and what an operator can do about it.
func describeRule(rule network.PortForwardRule) string {
	if !rule.Tagged() {
		return fmt.Sprintf("rule %q, which firework did not install (inspect it and delete it if it is stale)", rule.String())
	}
	return fmt.Sprintf("rule %q", rule.String())
}

// inventoryHold holds a start of svc while the kernel's rules could deliver
// traffic to its guest address or host ports that is not its own: any rule
// toward its address or for one of its host ports must be one of its own
// claimed forwards. There is no exception for the same service, so an earlier
// VM's rule for a released port is torn down first. When replacing is set,
// the service's own tagged rules are those of the VM the update removes, and
// they are torn down before the new VM starts; the start itself is checked
// again then. A rule list that cannot be read, or a DNAT rule whose
// destination cannot be read, holds every networked start.
//
// leaving names services whose VMs this tick's plan removes before any
// start: at plan time their rules and records do not hold a replacement,
// which the gate checks again once they are gone.
func (r *Reconciler) inventoryHold(svc config.ServiceConfig, replacing bool, leaving map[string]bool) Hold {
	ip := guestIPOf(svc)
	if r.networkMgr == nil || ip == "" {
		return Hold{}
	}
	listing, err := r.networkMgr.ListPortForwards()
	if err != nil {
		return Hold{Reason: fmt.Sprintf("the host's port-forward rules cannot be read: %v", err)}
	}
	if len(listing.Unparsed) > 0 {
		return Hold{Reason: fmt.Sprintf("DNAT rule %q may deliver to any address (inspect it and delete it if it is stale)", listing.Unparsed[0])}
	}
	for _, rule := range listing.Rules {
		own := rule.TaggedFor(svc.Name)
		if replacing && own || taggedForAny(rule, leaving) {
			continue
		}
		if claimsForward(svc, rule) && (own || !rule.Tagged()) {
			continue
		}
		if sameGuestIP(rule.GuestIP, ip) {
			return Hold{Reason: fmt.Sprintf("%s still reaches guest IP %s", describeRule(rule), ip)}
		}
		if claimsHostPort(svc, rule.HostPort) {
			return Hold{Reason: fmt.Sprintf("%s already forwards host port %d", describeRule(rule), rule.HostPort)}
		}
	}
	return Hold{}
}

func taggedForAny(rule network.PortForwardRule, services map[string]bool) bool {
	for service := range services {
		if rule.TaggedFor(service) {
			return true
		}
	}
	return false
}

// startHold is the full start gate: ownership records and the kernel's rules.
//
// keepsAddress says the service already has this address, as an update that
// keeps it does: an untracked VM elsewhere cannot then take it from it.
func (r *Reconciler) startHold(svc config.ServiceConfig, replacing bool, leaving map[string]bool, keepsAddress bool) Hold {
	if hold := r.allocationHold(svc, leaving, keepsAddress); hold.Reason != "" {
		return hold
	}
	return r.inventoryHold(svc, replacing, leaving)
}

// StartHold reports why a health restart of a tracked VM must wait. The VM
// keeps its address and ports, so its own rules never hold it.
func (r *Reconciler) StartHold(svc config.ServiceConfig) Hold {
	return r.inventoryHold(svc, true, nil)
}

// adoptPortForwards removes duplicates of a running VM's rules once its own
// tagged rule is installed: an untagged rule from before rules were tagged,
// or one tagged for another service, that forwards exactly the same claimed
// port to the same guest. Delivery is unchanged, and afterwards every rule
// toward the VM says whose it is.
func (r *Reconciler) adoptPortForwards(actual map[string]*vm.Instance, listing network.PortForwardListing) error {
	var errs []error
	for _, inst := range actual {
		if inst.State != vm.StateRunning || inst.RecoveryConfigUnknown || guestIPOf(inst.Config) == "" {
			continue
		}
		for _, rule := range listing.Rules {
			if rule.TaggedFor(inst.Config.Name) || !claimsForward(inst.Config, rule) {
				continue
			}
			tagged := slices.ContainsFunc(listing.Rules, func(other network.PortForwardRule) bool {
				return other.TaggedFor(inst.Config.Name) && other.Targets(rule.HostPort, rule.GuestIP, rule.VMPort) &&
					other.Interface == rule.Interface && other.HostIP == rule.HostIP
			})
			if !tagged {
				continue // Keep it until the VM's own rule delivers the same traffic.
			}
			if err := r.networkMgr.DeletePortForwardRule(rule); err != nil {
				errs = append(errs, stageError(FailureStageNetwork, fmt.Errorf("adopting port forward %d for %s: %w", rule.HostPort, inst.Config.Name, err)))
			}
		}
	}
	return combineErrors(errs)
}
