// Package scheduler implements bin-packing placement of services onto nodes.
// It is a pure function: given a list of services and a list of nodes with
// available capacity, it returns per-node service assignments.
//
// The algorithm:
//  1. For each service, honour its existing placement if the target node is
//     still alive and has capacity.
//  2. For services that cannot be kept, bin-pack them onto the node with the
//     most remaining capacity (best-fit descending by vCPU).
package scheduler

import (
	"fmt"
	"sort"

	"github.com/artemnikitin/firework/internal/config"
)

// Node describes an active node with its available capacity.
type Node struct {
	// InstanceID is the EC2 instance ID (used as the S3 config key).
	InstanceID string
	// CapacityVCPUs is the total number of vCPUs on the node.
	CapacityVCPUs int
	// CapacityMemMB is the total memory on the node in MB.
	CapacityMemMB int
}

// Schedule distributes services across nodes.
//
// existingAssignment maps service name → instance ID from the previous run.
// The scheduler preserves existing assignments when possible.
//
// Returns a map of instance ID → services assigned to that node.
func Schedule(
	services []config.ServiceConfig,
	nodes []Node,
	existingAssignment map[string]string,
) (map[string][]config.ServiceConfig, error) {
	if len(nodes) == 0 {
		if len(services) > 0 {
			return nil, fmt.Errorf("no active nodes available to schedule %d service(s)", len(services))
		}
		return map[string][]config.ServiceConfig{}, nil
	}

	// Track how much capacity is still available per node.
	usedVCPUs := make(map[string]int, len(nodes))
	usedMemMB := make(map[string]int, len(nodes))
	result := make(map[string][]config.ServiceConfig, len(nodes))
	// nodeGroups tracks which anti-affinity groups are already placed on each node.
	nodeGroups := make(map[string]map[string]bool, len(nodes))
	for _, n := range nodes {
		result[n.InstanceID] = nil
		nodeGroups[n.InstanceID] = make(map[string]bool)
	}

	nodeByID := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		nodeByID[n.InstanceID] = n
	}

	// Phase 1: honour existing placement where possible.
	var unplaced []config.ServiceConfig
	for _, svc := range services {
		existingNode, ok := existingAssignment[svc.Name]
		if !ok {
			unplaced = append(unplaced, svc)
			continue
		}

		n, alive := nodeByID[existingNode]
		if !alive {
			unplaced = append(unplaced, svc)
			continue
		}

		if usedVCPUs[n.InstanceID]+svc.VCPUs > n.CapacityVCPUs ||
			usedMemMB[n.InstanceID]+svc.MemoryMB > n.CapacityMemMB {
			unplaced = append(unplaced, svc)
			continue
		}

		// Re-evaluate anti-affinity: if another service in the same group was
		// already committed to this node during Phase 1, yield to Phase 2 so
		// it can find a node without the conflict (e.g. when a second node
		// becomes available after an initial single-node placement).
		if svc.AntiAffinityGroup != "" && nodeGroups[n.InstanceID][svc.AntiAffinityGroup] {
			unplaced = append(unplaced, svc)
			continue
		}

		// Keep on existing node.
		result[n.InstanceID] = append(result[n.InstanceID], svc)
		usedVCPUs[n.InstanceID] += svc.VCPUs
		usedMemMB[n.InstanceID] += svc.MemoryMB
		if svc.AntiAffinityGroup != "" {
			nodeGroups[n.InstanceID][svc.AntiAffinityGroup] = true
		}
	}

	// Phase 2: bin-pack unplaced services onto the node with most free capacity.
	// Sort services largest-first for better packing.
	sort.Slice(unplaced, func(i, j int) bool {
		return unplaced[i].VCPUs > unplaced[j].VCPUs
	})

	for _, svc := range unplaced {
		target := bestFit(nodes, svc, usedVCPUs, usedMemMB, nodeGroups)
		if target == "" {
			return nil, fmt.Errorf(
				"no node has sufficient capacity for service %q (needs %d vCPU, %d MB)",
				svc.Name, svc.VCPUs, svc.MemoryMB,
			)
		}
		result[target] = append(result[target], svc)
		usedVCPUs[target] += svc.VCPUs
		usedMemMB[target] += svc.MemoryMB
		if svc.AntiAffinityGroup != "" {
			nodeGroups[target][svc.AntiAffinityGroup] = true
		}
	}

	return result, nil
}

// bestFit returns the instance ID of the node with the most remaining vCPU
// capacity that can still fit the service, preferring nodes that don't already
// host the same anti-affinity group. Returns "" if no node has capacity.
func bestFit(nodes []Node, svc config.ServiceConfig, usedVCPUs, usedMemMB map[string]int, nodeGroups map[string]map[string]bool) string {
	best := ""
	bestFree := -1
	bestHasConflict := true

	for _, n := range nodes {
		freeVCPUs := n.CapacityVCPUs - usedVCPUs[n.InstanceID]
		freeMemMB := n.CapacityMemMB - usedMemMB[n.InstanceID]

		if freeVCPUs < svc.VCPUs || freeMemMB < svc.MemoryMB {
			continue
		}

		hasConflict := svc.AntiAffinityGroup != "" && nodeGroups[n.InstanceID][svc.AntiAffinityGroup]

		// Prefer: no conflict over conflict; within same conflict status, prefer more free capacity.
		if best == "" || (bestHasConflict && !hasConflict) || (bestHasConflict == hasConflict && freeVCPUs > bestFree) {
			bestFree = freeVCPUs
			best = n.InstanceID
			bestHasConflict = hasConflict
		}
	}

	return best
}

// BuildNodeConfigs converts a per-instance assignment map into NodeConfig
// slices suitable for writing to S3.
func BuildNodeConfigs(assignment map[string][]config.ServiceConfig) []config.NodeConfig {
	result := make([]config.NodeConfig, 0, len(assignment))
	for instanceID, services := range assignment {
		if len(services) == 0 {
			continue
		}
		result = append(result, config.NodeConfig{
			Node:     instanceID,
			Services: services,
		})
	}
	// Deterministic ordering.
	sort.Slice(result, func(i, j int) bool {
		return result[i].Node < result[j].Node
	})
	return result
}
