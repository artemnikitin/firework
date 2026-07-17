package controlplane

import (
	"testing"

	"github.com/artemnikitin/firework/internal/config"
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
