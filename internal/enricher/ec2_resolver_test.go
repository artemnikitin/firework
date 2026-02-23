package enricher

import (
	"testing"

	"github.com/artemnikitin/firework/internal/config"
)

func TestResolveCrossNodeLinks_InjectsEnvVar(t *testing.T) {
	nodeConfigs := []config.NodeConfig{
		{
			Node:   "i-aaa",
			HostIP: "10.0.0.7",
			Services: []config.ServiceConfig{
				{
					Name: "tenant-3-elasticsearch-data-1",
					CrossNodeLinks: []config.CrossNodeLink{
						{Service: "tenant-3-elasticsearch-data-2", Env: "ES_SEED_HOST", HostPort: 19300},
					},
				},
			},
		},
		{
			Node:   "i-bbb",
			HostIP: "10.0.1.5",
			Services: []config.ServiceConfig{
				{Name: "tenant-3-elasticsearch-data-2"},
			},
		},
	}

	result := resolveCrossNodeLinks(nodeConfigs)

	var data1 config.ServiceConfig
	for _, nc := range result {
		for _, svc := range nc.Services {
			if svc.Name == "tenant-3-elasticsearch-data-1" {
				data1 = svc
			}
		}
	}

	if data1.Env["ES_SEED_HOST"] != "10.0.1.5:19300" {
		t.Errorf("expected ES_SEED_HOST=10.0.1.5:19300, got %q", data1.Env["ES_SEED_HOST"])
	}
}

func TestResolveCrossNodeLinks_SkipsMissingPeer(t *testing.T) {
	nodeConfigs := []config.NodeConfig{
		{
			Node:   "i-aaa",
			HostIP: "10.0.0.7",
			Services: []config.ServiceConfig{
				{
					Name: "es-1",
					CrossNodeLinks: []config.CrossNodeLink{
						{Service: "es-unknown", Env: "ES_SEED_HOST", HostPort: 19300},
					},
				},
			},
		},
	}

	result := resolveCrossNodeLinks(nodeConfigs)

	for _, nc := range result {
		for _, svc := range nc.Services {
			if svc.Name == "es-1" {
				if _, ok := svc.Env["ES_SEED_HOST"]; ok {
					t.Error("ES_SEED_HOST should not be injected when peer is missing")
				}
			}
		}
	}
}

func TestResolveCrossNodeLinks_SkipsMissingHostIP(t *testing.T) {
	nodeConfigs := []config.NodeConfig{
		{
			Node:   "i-aaa",
			HostIP: "10.0.0.7",
			Services: []config.ServiceConfig{
				{
					Name: "es-1",
					CrossNodeLinks: []config.CrossNodeLink{
						{Service: "es-2", Env: "ES_SEED_HOST", HostPort: 19300},
					},
				},
			},
		},
		{
			Node:   "i-bbb",
			HostIP: "", // empty — not yet resolved
			Services: []config.ServiceConfig{
				{Name: "es-2"},
			},
		},
	}

	result := resolveCrossNodeLinks(nodeConfigs)

	for _, nc := range result {
		for _, svc := range nc.Services {
			if svc.Name == "es-1" {
				if _, ok := svc.Env["ES_SEED_HOST"]; ok {
					t.Error("ES_SEED_HOST should not be injected when peer HostIP is empty")
				}
			}
		}
	}
}
