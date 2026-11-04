package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/config"
	netpkg "github.com/artemnikitin/firework/internal/network"
	"github.com/artemnikitin/firework/internal/network/networktest"
	"github.com/artemnikitin/firework/internal/reconciler"
	"github.com/artemnikitin/firework/internal/vm"
)

// C4: a guest IP is not given to another service while a removed VM's host
// rules or published route may still reach it, so the new service cannot
// receive the removed one's traffic. The address is free again once the rules
// are gone and, for a route, once the Traefik that may still serve it has
// restarted (L7).
func TestC4DepartedAddressIsNotReassigned(t *testing.T) {
	limit := capacity.NodeCapacity{VCPUs: 8, MemoryMB: 4096}
	alpha := func() []config.ServiceConfig {
		return []config.ServiceConfig{{Name: "alpha", Network: &config.NetworkConfig{}, PortForwards: []config.PortForward{{HostPort: 9090, VMPort: 80}}}}
	}
	zeta := admissionService("zeta", 1, 64, 8080, "172.16.0.2")
	zeta.Network.GuestMAC = guestMACForIndex(0)

	t.Run("host rules awaiting teardown", func(t *testing.T) {
		a, m := admissionAgent(t, "node: web\nservices: []\n", limit, map[string]*vm.Instance{
			"zeta": {Name: "zeta", Config: zeta, State: vm.StateRunning},
		}, "172.16.0.0/24")
		network := &recoveryTestNetworkManager{teardownErr: errors.New("iptables: resource temporarily unavailable")}
		network.kernel.Append(networktest.Rule(netpkg.PortForwardTag("zeta"), 8080, "172.16.0.2", 80)) // zeta's VM left its rule installed.
		a.reconciler = reconciler.NewWithNetworkManager(m, testLogger(), nil, network, "", 0)
		a.tick(context.Background()) // zeta is removed; its rule's teardown fails and stays journalled.
		if m.instances["zeta"] != nil || a.reconciler.PendingTeardownGuestIPs()["172.16.0.2"] != "zeta" {
			t.Fatalf("setup: zeta=%v pending=%v", m.instances["zeta"], a.reconciler.PendingTeardownGuestIPs())
		}
		next := alpha()
		a.assignNetworking(next)
		if next[0].Network.GuestIP == "172.16.0.2" {
			t.Fatal("alpha was given 172.16.0.2 while zeta's 8080 -> 172.16.0.2 rule is still installed")
		}
		network.teardownErr = nil
		a.tick(context.Background()) // The teardown retry succeeds.
		next = alpha()
		a.assignNetworking(next)
		if next[0].Network.GuestIP != "172.16.0.2" {
			t.Fatalf("address not released after teardown: %s", next[0].Network.GuestIP)
		}
	})

	t.Run("published route", func(t *testing.T) {
		routed := zeta
		routed.Metadata = map[string]string{"host": "zeta.example.com"}
		a, m := admissionAgent(t, "node: web\nservices: []\n", limit, map[string]*vm.Instance{
			"zeta": {Name: "zeta", Config: routed, State: vm.StateRunning},
		}, "172.16.0.0/24")
		a.reconciler = testReconciler(t, m, &recoveryTestNetworkManager{}, "", 0) // clear-recovery needs a durable journal
		traefikProcess := useRoutes(t, a)
		if err := a.syncTraefikConfigs(context.Background(), []config.ServiceConfig{routed}); err != nil {
			t.Fatal(err)
		}
		m.clear(t, "zeta") // Cleared by clear-recovery between ticks.
		next := alpha()
		a.assignNetworking(next)
		if next[0].Network.GuestIP == "172.16.0.2" {
			t.Fatal("alpha was given 172.16.0.2 while zeta's route still points there")
		}
		if err := a.syncTraefikConfigs(context.Background(), next); err != nil {
			t.Fatal(err)
		}
		a.tick(context.Background()) // Settles zeta's record, which has no rule left.
		// The route file is gone, but the running Traefik may still serve the
		// route it read before (L7).
		next = alpha()
		a.assignNetworking(next)
		if next[0].Network.GuestIP == "172.16.0.2" {
			t.Fatal("alpha was given 172.16.0.2 while the Traefik that served zeta's route still runs")
		}
		traefikProcess.start = "2000" // The operator restarts Traefik.
		a.tick(context.Background())
		next = alpha()
		a.assignNetworking(next)
		if next[0].Network.GuestIP != "172.16.0.2" {
			t.Fatalf("address not released once Traefik restarted: %s", next[0].Network.GuestIP)
		}
	})
}
