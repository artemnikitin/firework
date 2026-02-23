package enricher

import (
	"context"
	"fmt"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/artemnikitin/firework/internal/config"
)

// resolveNodeIPs calls ec2:DescribeInstances for all instance-ID nodes and
// sets HostIP on each matched NodeConfig. Non-fatal: returns unchanged configs
// on AWS error so the caller can decide whether to skip cross-node links.
func resolveNodeIPs(ctx context.Context, region string, nodeConfigs []config.NodeConfig) ([]config.NodeConfig, error) {
	// Collect instance IDs (non-instance-ID nodes are skipped).
	var ids []string
	for _, nc := range nodeConfigs {
		if strings.HasPrefix(nc.Node, "i-") {
			ids = append(ids, nc.Node)
		}
	}
	if len(ids) == 0 {
		return nodeConfigs, nil
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	client := ec2.NewFromConfig(awsCfg)
	out, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: ids,
	})
	if err != nil {
		return nil, fmt.Errorf("ec2:DescribeInstances: %w", err)
	}

	ipByID := make(map[string]string, len(ids))
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			if inst.InstanceId != nil && inst.PrivateIpAddress != nil {
				ipByID[*inst.InstanceId] = *inst.PrivateIpAddress
			}
		}
	}

	result := make([]config.NodeConfig, len(nodeConfigs))
	for i, nc := range nodeConfigs {
		result[i] = nc
		if ip, ok := ipByID[nc.Node]; ok {
			result[i].HostIP = ip
		}
	}
	return result, nil
}

// resolveCrossNodeLinks injects env vars from cross-node peer references.
// For each service with CrossNodeLinks, it looks up the peer node's HostIP
// and injects "<link.Env> = <hostIP>:<link.HostPort>". Peers not found or
// with an empty HostIP are silently skipped.
func resolveCrossNodeLinks(nodeConfigs []config.NodeConfig) []config.NodeConfig {
	// Build service name → NodeConfig index.
	serviceNode := make(map[string]config.NodeConfig)
	for _, nc := range nodeConfigs {
		for _, svc := range nc.Services {
			serviceNode[svc.Name] = nc
		}
	}

	result := make([]config.NodeConfig, len(nodeConfigs))
	for i, nc := range nodeConfigs {
		nc2 := nc
		nc2.Services = make([]config.ServiceConfig, len(nc.Services))
		for j, svc := range nc.Services {
			svc2 := svc
			needsEnv := len(svc.CrossNodeLinks) > 0 || svc.NodeHostIPEnv != ""
			if needsEnv {
				if svc2.Env == nil {
					svc2.Env = make(map[string]string)
				}
				for _, link := range svc.CrossNodeLinks {
					peerNC, ok := serviceNode[link.Service]
					if !ok || peerNC.HostIP == "" {
						continue
					}
					svc2.Env[link.Env] = fmt.Sprintf("%s:%d", peerNC.HostIP, link.HostPort)
				}
				// Inject own node's host IP if requested.
				if svc.NodeHostIPEnv != "" && nc.HostIP != "" {
					svc2.Env[svc.NodeHostIPEnv] = nc.HostIP
				}
			}
			nc2.Services[j] = svc2
		}
		result[i] = nc2
	}
	return result
}
