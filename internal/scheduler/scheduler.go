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
	CapacityMemMB       int
	LocalCapacityBytes  int64
	SharedBackendID     string
	SharedCapacityBytes int64
}

// StorageReservations contains retained quota that must remain admitted even
// when its workload is absent. RecordedLogicalIDs prevents double counting
// volumes that are currently desired.
type StorageReservations struct {
	LocalByNode        map[string]int64
	SharedByBackend    map[string]int64
	RecordedLogicalIDs map[string]bool
	SharedEnabled      bool
}

type Pending struct {
	Service    string
	ReasonCode string
	Message    string
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

// ScheduleWithStorage preserves the legacy CPU/memory behavior while adding
// retained-volume constraints and per-service pending results. It is kept
// separate from Schedule so existing direct callers retain error semantics.
func ScheduleWithStorage(services []config.ServiceConfig, nodes []Node, existing map[string]string, reservations StorageReservations) (map[string][]config.ServiceConfig, []Pending) {
	result := make(map[string][]config.ServiceConfig, len(nodes))
	usedVCPU := make(map[string]int, len(nodes))
	usedMem := make(map[string]int, len(nodes))
	usedLocal := make(map[string]int64, len(nodes))
	usedShared := make(map[string]int64)
	groups := make(map[string]map[string]bool, len(nodes))
	nodeByID := make(map[string]Node, len(nodes))
	for _, node := range nodes {
		result[node.InstanceID] = nil
		groups[node.InstanceID] = make(map[string]bool)
		nodeByID[node.InstanceID] = node
	}

	ordered := append([]config.ServiceConfig(nil), services...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].VCPUs != ordered[j].VCPUs {
			return ordered[i].VCPUs > ordered[j].VCPUs
		}
		return ordered[i].Name < ordered[j].Name
	})

	var pending []Pending
	for _, service := range ordered {
		boundNode, split := localBinding(service)
		if split {
			pending = append(pending, Pending{Service: service.Name, ReasonCode: "local_volume_binding_conflict", Message: "local volumes are retained on different nodes"})
			continue
		}
		if hasSharedVolume(service) && !reservations.SharedEnabled {
			pending = append(pending, Pending{Service: service.Name, ReasonCode: "shared_volume_runtime_unavailable", Message: "shared volumes await durable supervisor and fencing validation"})
			continue
		}

		preferred := existing[service.Name]
		if boundNode != "" {
			preferred = boundNode
			if _, active := nodeByID[boundNode]; !active {
				pending = append(pending, Pending{Service: service.Name, ReasonCode: "local_volume_node_unavailable", Message: fmt.Sprintf("bound node %s is unavailable", boundNode)})
				continue
			}
		}

		candidates := append([]Node(nil), nodes...)
		sort.SliceStable(candidates, func(i, j int) bool {
			iConflict := service.AntiAffinityGroup != "" && groups[candidates[i].InstanceID][service.AntiAffinityGroup]
			jConflict := service.AntiAffinityGroup != "" && groups[candidates[j].InstanceID][service.AntiAffinityGroup]
			if iConflict != jConflict {
				return !iConflict
			}
			iPreferred := candidates[i].InstanceID == preferred
			jPreferred := candidates[j].InstanceID == preferred
			if iPreferred != jPreferred {
				return iPreferred
			}
			freeI := candidates[i].CapacityVCPUs - usedVCPU[candidates[i].InstanceID]
			freeJ := candidates[j].CapacityVCPUs - usedVCPU[candidates[j].InstanceID]
			if freeI != freeJ {
				return freeI > freeJ
			}
			return candidates[i].InstanceID < candidates[j].InstanceID
		})

		chosen := ""
		chosenService := service
		for _, node := range candidates {
			if boundNode != "" && node.InstanceID != boundNode {
				continue
			}
			if usedVCPU[node.InstanceID]+service.VCPUs > node.CapacityVCPUs || usedMem[node.InstanceID]+service.MemoryMB > node.CapacityMemMB {
				continue
			}
			candidateService, localDelta, sharedDelta, ok := fitStorage(service, node, reservations, usedLocal, usedShared)
			if !ok {
				continue
			}
			chosen = node.InstanceID
			chosenService = candidateService
			usedLocal[node.InstanceID] += localDelta
			if node.SharedBackendID != "" {
				usedShared[node.SharedBackendID] += sharedDelta
			}
			break
		}
		if chosen == "" {
			reason := "insufficient_compute_capacity"
			message := "no active node satisfies compute capacity"
			if len(service.Volumes) > 0 {
				reason = "volume_capacity_unavailable"
				message = "no active node satisfies volume binding and capacity"
			}
			pending = append(pending, Pending{Service: service.Name, ReasonCode: reason, Message: message})
			continue
		}
		result[chosen] = append(result[chosen], chosenService)
		usedVCPU[chosen] += service.VCPUs
		usedMem[chosen] += service.MemoryMB
		if service.AntiAffinityGroup != "" {
			groups[chosen][service.AntiAffinityGroup] = true
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Service < pending[j].Service })
	return result, pending
}

func localBinding(service config.ServiceConfig) (string, bool) {
	bound := ""
	for _, volume := range service.Volumes {
		if volume.Type != config.VolumeTypeLocal || volume.BoundNode == "" {
			continue
		}
		if bound != "" && bound != volume.BoundNode {
			return "", true
		}
		bound = volume.BoundNode
	}
	return bound, false
}

func hasSharedVolume(service config.ServiceConfig) bool {
	for _, volume := range service.Volumes {
		if volume.Type == config.VolumeTypeShared {
			return true
		}
	}
	return false
}

func fitStorage(service config.ServiceConfig, node Node, reservations StorageReservations, usedLocal, usedShared map[string]int64) (config.ServiceConfig, int64, int64, bool) {
	candidate := service
	candidate.Volumes = append([]config.VolumeConfig(nil), service.Volumes...)
	var localDelta, sharedDelta int64
	for i := range candidate.Volumes {
		volume := &candidate.Volumes[i]
		logicalID := service.Name + "/" + volume.Name
		switch volume.Type {
		case config.VolumeTypeLocal:
			if node.LocalCapacityBytes <= 0 || (volume.BoundNode != "" && volume.BoundNode != node.InstanceID) {
				return service, 0, 0, false
			}
			volume.BoundNode = node.InstanceID
			if !reservations.RecordedLogicalIDs[logicalID] {
				localDelta += volume.SizeBytes
			}
		case config.VolumeTypeShared:
			if node.SharedBackendID == "" || (volume.SharedBackendID != "" && volume.SharedBackendID != node.SharedBackendID) {
				return service, 0, 0, false
			}
			volume.SharedBackendID = node.SharedBackendID
			if !reservations.RecordedLogicalIDs[logicalID] {
				sharedDelta += volume.SizeBytes
			}
		}
	}
	if reservations.LocalByNode[node.InstanceID]+usedLocal[node.InstanceID]+localDelta > node.LocalCapacityBytes {
		return service, 0, 0, false
	}
	if sharedDelta > 0 && node.SharedCapacityBytes > 0 && reservations.SharedByBackend[node.SharedBackendID]+usedShared[node.SharedBackendID]+sharedDelta > node.SharedCapacityBytes {
		return service, 0, 0, false
	}
	return candidate, localDelta, sharedDelta, true
}
