package reconciler

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// RouteTarget is one local route: the service it was written for and the
// guest address it routes to.
type RouteTarget struct {
	Service string
	GuestIP string
}

// RouteEvidence reads the local routes and the process that serves them.
type RouteEvidence interface {
	// LocalRoutes returns the routes in the route directory, and the files
	// that may route to any address because they cannot be attributed.
	LocalRoutes() (routes []RouteTarget, unreadable []string, err error)
	// ServingProcess identifies the process serving the route directory now:
	// the host's boot ID and the process's start, or an empty start when none
	// runs.
	ServingProcess() (bootID, start string, err error)
	// Unit names the serving process for status.
	Unit() string
}

// routeState is how far a route withdrawal has come.
type routeState string

const (
	// routeWithdrawing: the route file may still route to the address. It is
	// recorded before the file changes.
	routeWithdrawing routeState = "withdrawing"
	// routeWithdrawn: the file change is durable and verified. The process
	// that was serving routes then may still serve the old route.
	routeWithdrawn routeState = "withdrawn"
)

// routeEveryAddress is the baseline withdrawal a migrated journal records for
// every address: routes removed before the migration are not known.
const routeEveryAddress = "*"

// routeWithdrawal holds a guest address for every other service until no
// process that could still serve the withdrawn route is running (L7).
type routeWithdrawal struct {
	Service string     `json:"service"`
	GuestIP string     `json:"guest_ip"`
	State   routeState `json:"state"`
	// BootID and ProcessStart identify the process serving routes when the
	// withdrawal completed. Any other process, a stopped one, or another boot
	// can only have read the route directory after the route was gone.
	BootID       string `json:"boot_id,omitempty"`
	ProcessStart string `json:"process_start,omitempty"`
}

type routeKey struct{ service, guestIP string }

func (w routeWithdrawal) key() routeKey { return routeKey{w.Service, w.GuestIP} }

// WithRouteEvidence enables route ownership: live routes and withdrawal
// records hold guest addresses for other services. Call it before
// WithStateDir.
func (r *Reconciler) WithRouteEvidence(evidence RouteEvidence) *Reconciler {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	r.routeEvidence = evidence
	return r
}

// BeginRouteWithdrawals durably records each route about to be removed or
// moved, before its file changes. everyAddress records a withdrawal whose
// address cannot be read, such as an unreadable route file about to go.
func (r *Reconciler) BeginRouteWithdrawals(withdrawn []RouteTarget, everyAddress bool) error {
	if r.routeEvidence == nil || len(withdrawn) == 0 && !everyAddress {
		return nil
	}
	return r.mutateJournal(func() {
		if everyAddress {
			withdrawn = append(withdrawn, RouteTarget{Service: routeEveryAddress, GuestIP: routeEveryAddress})
		}
		for _, target := range withdrawn {
			key := routeKey{target.Service, target.GuestIP}
			// A repeated withdrawal restarts: the route may have come back.
			r.routeWithdrawals[key] = routeWithdrawal{Service: target.Service, GuestIP: target.GuestIP, State: routeWithdrawing}
		}
	})
}

// CompleteRouteWithdrawals settles the route records: see settleRoutesLocked.
// The agent calls it right after publishing routes.
func (r *Reconciler) CompleteRouteWithdrawals() error {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	before := maps.Clone(r.routeWithdrawals)
	err := r.settleRoutesLocked()
	if !maps.Equal(before, r.routeWithdrawals) {
		if perr := r.persistJournalLocked(); perr != nil {
			r.routeWithdrawals = before
			return perr
		}
	}
	return err
}

// settleRoutesLocked completes a withdrawal once the live routes no longer
// contain it, recording the process serving routes then, and releases a
// completed one once that process no longer runs. With no process running at
// completion, the withdrawal is released at once. Nothing completes while a
// route file cannot be read, since it may still route to the address.
func (r *Reconciler) settleRoutesLocked() error {
	if r.routeEvidence == nil || len(r.routeWithdrawals) == 0 {
		return nil
	}
	routes, unreadable, err := r.routeEvidence.LocalRoutes()
	if err != nil {
		return fmt.Errorf("reading local routes: %w", err)
	}
	boot, start, err := r.routeEvidence.ServingProcess()
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", r.routeEvidence.Unit(), err)
	}
	live := make(map[routeKey]bool, len(routes))
	for _, route := range routes {
		live[routeKey{route.Service, route.GuestIP}] = true
	}
	for key, w := range r.routeWithdrawals {
		switch w.State {
		case routeWithdrawing:
			if len(unreadable) > 0 || live[key] {
				continue
			}
			if start == "" {
				delete(r.routeWithdrawals, key)
				continue
			}
			w.State, w.BootID, w.ProcessStart = routeWithdrawn, boot, start
			r.routeWithdrawals[key] = w
		case routeWithdrawn:
			if boot != w.BootID || start == "" || start != w.ProcessStart {
				delete(r.routeWithdrawals, key)
			}
		}
	}
	return nil
}

// routeHoldLocked holds a start at ip while a route still points there for
// another service, while a route file cannot be attributed, or while a
// withdrawn route of another service may still be served (L7). ownsAddress
// says the address is already the service's: its tracked VM, its records or
// the update replacing it hold it. Such a service keeps it through a
// baseline withdrawal, which holds addresses only for new owners.
func (r *Reconciler) routeHoldLocked(svc string, ip string, ownsAddress bool) Hold {
	if r.routeEvidence == nil {
		return Hold{}
	}
	routes, unreadable, err := r.routeEvidence.LocalRoutes()
	if err != nil {
		return Hold{Reason: fmt.Sprintf("local routes cannot be read: %v", err)}
	}
	if len(unreadable) > 0 {
		return Hold{Reason: fmt.Sprintf("route file %s may route to any address (inspect it and remove it if it is stale)", unreadable[0])}
	}
	for _, route := range routes {
		if route.Service != svc && sameGuestIP(route.GuestIP, ip) {
			return Hold{Reason: fmt.Sprintf("the route of service %s still points at guest IP %s", route.Service, ip)}
		}
	}
	for _, key := range slices.SortedFunc(maps.Keys(r.routeWithdrawals), func(a, b routeKey) int {
		return strings.Compare(a.service+"/"+a.guestIP, b.service+"/"+b.guestIP)
	}) {
		w := r.routeWithdrawals[key]
		if w.Service == svc || w.GuestIP == routeEveryAddress && ownsAddress || w.GuestIP != routeEveryAddress && !sameGuestIP(w.GuestIP, ip) {
			continue
		}
		owner := "service " + w.Service
		if w.GuestIP == routeEveryAddress {
			owner = "a route removed before this node's ownership records were migrated"
		}
		if w.State == routeWithdrawing {
			return Hold{Reason: fmt.Sprintf("the withdrawal of %s from guest IP %s is not complete yet", owner, ip)}
		}
		return Hold{Reason: fmt.Sprintf("guest IP %s was routed for %s, and %s may still serve that route; restart it to release the address", ip, owner, r.routeEvidence.Unit()),
			Action: ActionGuestAddressWithdrawalUnconfirmed}
	}
	return Hold{}
}

// RouteHeldGuestIPs returns the guest addresses live routes or unreleased
// withdrawals hold, with the service each is held for, which of them only a
// restart of the serving process releases, and whether a baseline withdrawal
// holds every address.
func (r *Reconciler) RouteHeldGuestIPs() (held map[string]string, unconfirmed map[string]bool, everyAddress bool) {
	r.cleanupMu.Lock()
	defer r.cleanupMu.Unlock()
	held, unconfirmed = map[string]string{}, map[string]bool{}
	if r.routeEvidence == nil {
		return held, unconfirmed, false
	}
	if routes, _, err := r.routeEvidence.LocalRoutes(); err == nil {
		for _, route := range routes {
			held[route.GuestIP] = route.Service
		}
	}
	for _, w := range r.routeWithdrawals {
		if w.GuestIP == routeEveryAddress {
			everyAddress = true
			continue
		}
		held[w.GuestIP] = w.Service
		unconfirmed[w.GuestIP] = unconfirmed[w.GuestIP] || w.State == routeWithdrawn
	}
	return held, unconfirmed, everyAddress
}
