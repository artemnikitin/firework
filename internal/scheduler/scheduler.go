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
	// LocalUnknownByNode and SharedUnknownByBackend mark a scope whose
	// remaining capacity cannot be proved because a retained record was only
	// partially readable. Its lower bound is still charged above; the flag is
	// what stops the unaccounted remainder from being handed out again.
	LocalUnknownByNode     map[string]bool
	SharedUnknownByBackend map[string]bool
	// LocalClassUnknown and SharedClassUnknown widen that block to a whole
	// storage class, for a record so unreadable that no binding — and
	// therefore no narrower scope — could be determined.
	LocalClassUnknown  bool
	SharedClassUnknown bool
}

// Pending reason codes. They are a bounded vocabulary because the status API,
// fireworkctl, and the web UI all render them.
const (
	// ReasonInsufficientCompute means vCPU or memory, and nothing else.
	ReasonInsufficientCompute = "insufficient_compute_capacity"
	// ReasonVolumeCapacityUnavailable means the volume cannot bind to any
	// candidate at all: no pool is configured there, or its retained binding
	// names somewhere else. A configuration or placement fact.
	ReasonVolumeCapacityUnavailable = "volume_capacity_unavailable"
	// ReasonNodeStorageExhausted means the volume could bind, but the pool has
	// no room for the new reservation. A capacity fact, resolved by freeing
	// retained volumes or growing the pool.
	ReasonNodeStorageExhausted = "node_storage_exhausted"
	// ReasonStorageCapacityUnknown means remaining capacity cannot be proved,
	// so new volume-bearing placement is withheld rather than guessed.
	ReasonStorageCapacityUnknown = "storage_capacity_unknown"
	// ReasonVolumeRecordInvalid means the service's own retained record could
	// not be parsed, so it is not placed for the first time.
	ReasonVolumeRecordInvalid = "volume_record_invalid"
	// ReasonHostPortConflict means every candidate node already holds one of
	// the service's (tcp, host_port) claims. It outranks the storage reasons
	// below because the port check runs first: a node rejected on ports is
	// never evaluated for storage, so a storage reason recorded elsewhere
	// describes a different node than the one the operator has to fix.
	ReasonHostPortConflict = "host_port_conflict"
)

// storageRank orders storage rejection causes from least to most actionable so
// the dominant one survives across candidate nodes.
func storageRank(reason string) int {
	switch reason {
	case ReasonVolumeCapacityUnavailable:
		return 1
	case ReasonStorageCapacityUnknown:
		return 2
	case ReasonNodeStorageExhausted:
		return 3
	default:
		return 0
	}
}

func storageReasonMessage(reason string) string {
	switch reason {
	case ReasonNodeStorageExhausted:
		return "no active node has room for the requested volume reservation"
	case ReasonStorageCapacityUnknown:
		return "remaining volume capacity cannot be verified; repair the quarantined volume record"
	default:
		return "no active node satisfies volume binding and capacity"
	}
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
// It does not enforce host-port claims; use ScheduleWithStorage for placement
// that keeps colocated services from conflicting on a host port.
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
// retained-volume constraints, host-port claims, and per-service pending
// results. It is kept separate from Schedule so existing direct callers retain
// error semantics.
//
// Host ports are node-scoped resources: two colocated services claiming the
// same port produce DNAT rules with identical match criteria and different
// guest destinations, so traffic silently reaches only one of them. A service
// is therefore placed only on a node where all of its claims are free, and its
// claims are taken atomically.
// pinned describes services the caller will render itself, outside this
// scheduler, on a node it has already chosen. They still occupy node-exclusive
// resources, so the scheduler has to be told about them or it will hand the
// same host port to something else — the exact collision node-exclusive claims
// exist to prevent. Compute is reserved by the caller adjusting node capacity;
// ports cannot be expressed that way, so they are passed here.
//
// ScheduleWithStorage takes pinnedClaims as node -> claim -> holding service.
// A nil map means nothing is pinned, which is the ordinary case.
func ScheduleWithStorage(services []config.ServiceConfig, nodes []Node, existing map[string]string, reservations StorageReservations, pinnedClaims map[string]map[config.PortClaim]string) (map[string][]config.ServiceConfig, []Pending) {
	result := make(map[string][]config.ServiceConfig, len(nodes))
	usedVCPU := make(map[string]int, len(nodes))
	usedMem := make(map[string]int, len(nodes))
	usedLocal := make(map[string]int64, len(nodes))
	usedShared := make(map[string]int64)
	groups := make(map[string]map[string]bool, len(nodes))
	claimedPorts := make(map[string]map[config.PortClaim]string, len(nodes))
	nodeByID := make(map[string]Node, len(nodes))
	for _, node := range nodes {
		result[node.InstanceID] = nil
		groups[node.InstanceID] = make(map[string]bool)
		claimedPorts[node.InstanceID] = make(map[config.PortClaim]string)
		for claim, holder := range pinnedClaims[node.InstanceID] {
			claimedPorts[node.InstanceID][claim] = holder
		}
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
		if dupes := service.DuplicatePortClaims(); len(dupes) > 0 {
			pending = append(pending, Pending{
				Service:    service.Name,
				ReasonCode: "duplicate_host_port_claims",
				Message:    fmt.Sprintf("service claims host port %d more than once", dupes[0].HostPort),
			})
			continue
		}
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

		claims := service.PortClaims()
		chosen := ""
		chosenService := service
		dominantStorageReason := ""
		portConflict := ""
		for _, node := range candidates {
			if boundNode != "" && node.InstanceID != boundNode {
				continue
			}
			if usedVCPU[node.InstanceID]+service.VCPUs > node.CapacityVCPUs || usedMem[node.InstanceID]+service.MemoryMB > node.CapacityMemMB {
				continue
			}
			// All claims must fit on the same node, and the check runs before
			// fitStorage so a port-rejected node commits no storage usage.
			if conflict, blocked := firstPortConflict(node.InstanceID, claimedPorts[node.InstanceID], claims); blocked {
				if portConflict == "" {
					portConflict = conflict
				}
				continue
			}
			candidateService, localDelta, sharedDelta, storageReason := fitStorage(service, node, reservations, usedLocal, usedShared)
			if storageReason != "" {
				// Keep the most actionable cause seen across candidates. The
				// dominant reason tells the operator whether the placement is
				// wrong or the chosen node is simply full.
				if storageRank(storageReason) > storageRank(dominantStorageReason) {
					dominantStorageReason = storageReason
				}
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
			reason := ReasonInsufficientCompute
			message := "no active node satisfies compute capacity"
			if dominantStorageReason != "" {
				reason = dominantStorageReason
				message = storageReasonMessage(dominantStorageReason)
			}
			if portConflict != "" {
				reason = ReasonHostPortConflict
				message = portConflict
			}
			pending = append(pending, Pending{Service: service.Name, ReasonCode: reason, Message: message})
			continue
		}
		result[chosen] = append(result[chosen], chosenService)
		usedVCPU[chosen] += service.VCPUs
		usedMem[chosen] += service.MemoryMB
		for _, claim := range claims {
			claimedPorts[chosen][claim] = service.Name
		}
		if service.AntiAffinityGroup != "" {
			groups[chosen][service.AntiAffinityGroup] = true
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Service < pending[j].Service })
	return result, pending
}

// firstPortConflict reports the first claim already held on a node, together
// with a message naming the port, the holding service, and the node. Only the
// conflicting claim is named so the reason stays actionable without exposing
// unrelated configuration.
func firstPortConflict(node string, held map[config.PortClaim]string, claims []config.PortClaim) (string, bool) {
	for _, claim := range claims {
		if holder, taken := held[claim]; taken {
			return fmt.Sprintf("host port %d (%s) is already claimed by service %s on node %s",
				claim.HostPort, claim.Protocol, holder, node), true
		}
	}
	return "", false
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

// fitStorage reports whether a service's volumes can bind to a node, and why
// not when they cannot. The reason separates a placement fact (the volume
// cannot bind here at all) from a capacity fact (it could bind, but the pool
// has no room), because the two have opposite operator remedies.
func fitStorage(service config.ServiceConfig, node Node, reservations StorageReservations, usedLocal, usedShared map[string]int64) (config.ServiceConfig, int64, int64, string) {
	candidate := service
	candidate.Volumes = append([]config.VolumeConfig(nil), service.Volumes...)
	var localDelta, sharedDelta int64
	for i := range candidate.Volumes {
		volume := &candidate.Volumes[i]
		logicalID := service.Name + "/" + volume.Name
		switch volume.Type {
		case config.VolumeTypeLocal:
			if node.LocalCapacityBytes <= 0 || (volume.BoundNode != "" && volume.BoundNode != node.InstanceID) {
				return service, 0, 0, ReasonVolumeCapacityUnavailable
			}
			volume.BoundNode = node.InstanceID
			if !reservations.RecordedLogicalIDs[logicalID] {
				localDelta += volume.SizeBytes
			}
		case config.VolumeTypeShared:
			if node.SharedBackendID == "" || (volume.SharedBackendID != "" && volume.SharedBackendID != node.SharedBackendID) {
				return service, 0, 0, ReasonVolumeCapacityUnavailable
			}
			volume.SharedBackendID = node.SharedBackendID
			if !reservations.RecordedLogicalIDs[logicalID] {
				sharedDelta += volume.SizeBytes
			}
		}
	}
	// A service that adds no new local reservation cannot recover capacity by
	// being rejected, it can only be evicted. Volumes already counted in
	// LocalByNode contribute a zero delta, and a service with no volumes at
	// all contributes nothing — so retained reservations above the pool must
	// not make the node reject either of them. Only a genuinely new
	// allocation is checked against the pool.
	if localDelta > 0 && reservations.LocalByNode[node.InstanceID]+usedLocal[node.InstanceID]+localDelta > node.LocalCapacityBytes {
		return service, 0, 0, ReasonNodeStorageExhausted
	}
	if sharedDelta > 0 && node.SharedCapacityBytes > 0 && reservations.SharedByBackend[node.SharedBackendID]+usedShared[node.SharedBackendID]+sharedDelta > node.SharedCapacityBytes {
		return service, 0, 0, ReasonNodeStorageExhausted
	}
	// A quarantined record whose reservation could not be read makes the
	// node's remaining pool unknowable. New volume-bearing placement is
	// withheld there rather than allocated against capacity that may already
	// be occupied; an already-placed service is re-rendered untouched.
	if localDelta > 0 && reservations.LocalUnknownByNode[node.InstanceID] {
		return service, 0, 0, ReasonStorageCapacityUnknown
	}
	if sharedDelta > 0 && reservations.SharedUnknownByBackend[node.SharedBackendID] {
		return service, 0, 0, ReasonStorageCapacityUnknown
	}
	if localDelta > 0 && reservations.LocalClassUnknown {
		return service, 0, 0, ReasonStorageCapacityUnknown
	}
	if sharedDelta > 0 && reservations.SharedClassUnknown {
		return service, 0, 0, ReasonStorageCapacityUnknown
	}
	return candidate, localDelta, sharedDelta, ""
}
