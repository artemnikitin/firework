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
