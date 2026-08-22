package controlplane

import (
	"testing"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/scheduler"
)

func TestApplyHostIPAndCrossNodeLinks(t *testing.T) {
	tests := []struct {
		name     string
		protocol string
		want     string
	}{
		{
			name: "legacy bare address",
			want: "10.0.1.5:9200",
		},
		{
			name:     "http URL",
			protocol: "http",
			want:     "http://10.0.1.5:9200",
		},
		{
			name:     "https URL",
			protocol: "https",
			want:     "https://10.0.1.5:9200",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeConfigs := []config.NodeConfig{
				{
					Node: "node-client",
					Services: []config.ServiceConfig{
						{
							Name: "client",
							CrossNodeLinks: []config.CrossNodeLink{
								{
									Service:  "backend",
									Env:      "BACKEND_URL",
									HostPort: 9200,
									Protocol: tt.protocol,
								},
							},
						},
					},
				},
				{
					Node: "node-backend",
					Services: []config.ServiceConfig{
						{Name: "backend"},
					},
				},
			}

			applyHostIPAndCrossNodeLinks(nodeConfigs, map[string]string{
				"node-client":  "10.0.1.4",
				"node-backend": "10.0.1.5",
			})

			got := nodeConfigs[0].Services[0].Env["BACKEND_URL"]
			if got != tt.want {
				t.Fatalf("injected BACKEND_URL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyCrossNodeLinksSharedEnvKey(t *testing.T) {
	tests := []struct {
		name      string
		links     []config.CrossNodeLink
		staticEnv map[string]string
		want      string
	}{
		{
			name: "two links join comma separated in spec order",
			links: []config.CrossNodeLink{
				{Service: "backend-1", Env: "SEED_HOSTS", HostPort: 9300},
				{Service: "backend-2", Env: "SEED_HOSTS", HostPort: 9301},
			},
			want: "10.0.1.5:9300,10.0.1.6:9301",
		},
		{
			name: "unresolvable link is skipped without a dangling comma",
			links: []config.CrossNodeLink{
				{Service: "missing", Env: "SEED_HOSTS", HostPort: 9300},
				{Service: "backend-2", Env: "SEED_HOSTS", HostPort: 9301},
			},
			want: "10.0.1.6:9301",
		},
		{
			name: "first resolved link replaces static env value",
			links: []config.CrossNodeLink{
				{Service: "backend-1", Env: "SEED_HOSTS", HostPort: 9300},
				{Service: "backend-2", Env: "SEED_HOSTS", HostPort: 9301},
			},
			staticEnv: map[string]string{"SEED_HOSTS": "static-host:9300"},
			want:      "10.0.1.5:9300,10.0.1.6:9301",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeConfigs := []config.NodeConfig{
				{
					Node: "node-client",
					Services: []config.ServiceConfig{
						{
							Name:           "client",
							Env:            tt.staticEnv,
							CrossNodeLinks: tt.links,
						},
					},
				},
				{
					Node: "node-backend-1",
					Services: []config.ServiceConfig{
						{Name: "backend-1"},
					},
				},
				{
					Node: "node-backend-2",
					Services: []config.ServiceConfig{
						{Name: "backend-2"},
					},
				},
			}

			applyHostIPAndCrossNodeLinks(nodeConfigs, map[string]string{
				"node-client":    "10.0.1.4",
				"node-backend-1": "10.0.1.5",
				"node-backend-2": "10.0.1.6",
			})

			got := nodeConfigs[0].Services[0].Env["SEED_HOSTS"]
			if got != tt.want {
				t.Fatalf("injected SEED_HOSTS = %q, want %q", got, tt.want)
			}
		})
	}
}

// The regression: scheduler.BuildNodeConfigs omits a node that ended up with
// no services, publishRendered then deleted its nodes/<node>.yaml, and the
// agent treated the missing file as a fetch failure and kept running every VM
// it already had. The fleet view derives its relevant node set from the same
// placement, so the retired node stopped being counted and the revision could
// report converged on the strength of the node that received the work.
func TestAppendRetiredNodeConfigs(t *testing.T) {
	activeNodes := []scheduler.Node{{InstanceID: "node-a"}, {InstanceID: "node-b"}}
	// node-b took over the only service; node-a was left with nothing.
	placed := []config.NodeConfig{{Node: "node-b", Services: []config.ServiceConfig{{Name: "web"}}}}

	got := appendRetiredNodeConfigs(placed, activeNodes)

	if len(got) != 2 {
		t.Fatalf("expected the retired node to be published explicitly, got %#v", got)
	}
	if got[0].Node != "node-a" || got[1].Node != "node-b" {
		t.Fatalf("expected deterministic ordering by node, got %#v", got)
	}
	if len(got[0].Services) != 0 {
		t.Fatalf("retired node must be published with no services, got %#v", got[0].Services)
	}
	if len(got[1].Services) != 1 || got[1].Services[0].Name != "web" {
		t.Fatalf("the placed node's services were altered: %#v", got[1])
	}
}

// A node absent from activeNodes entirely (down, stale, decommissioned) is not
// part of this placement and must not be resurrected into it.
func TestAppendRetiredNodeConfigsIgnoresInactiveNodes(t *testing.T) {
	got := appendRetiredNodeConfigs(
		[]config.NodeConfig{{Node: "node-b", Services: []config.ServiceConfig{{Name: "web"}}}},
		[]scheduler.Node{{InstanceID: "node-b"}},
	)
	if len(got) != 1 || got[0].Node != "node-b" {
		t.Fatalf("expected only the active placed node, got %#v", got)
	}
}
