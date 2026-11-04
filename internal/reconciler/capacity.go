package reconciler

import (
	"context"
	"fmt"
	"maps"

	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/vm"
)

// capacityLimit is the node capacity the caller measured and, for each desired
// service it let proceed this tick, the size that service may occupy.
type capacityLimit struct {
	node      capacity.NodeCapacity
	allowance map[string]capacity.NodeCapacity
}

type vmStopper interface {
	Stop(string) error
}

// SetCapacity installs the node capacity that every start is checked against
// and the size admission allowed each desired service it let proceed. A
// service with no allowance counts at what its VM uses now. Pass known=false
// when capacity checks are disabled or the capacity is unknown: nothing is
// checked then.
func (r *Reconciler) SetCapacity(node capacity.NodeCapacity, known bool, allowance map[string]capacity.NodeCapacity) {
	r.heldMu.Lock()
	defer r.heldMu.Unlock()
	if !known {
		r.capacity = nil
		return
	}
	r.capacity = &capacityLimit{node: node, allowance: maps.Clone(allowance)}
}

// UsesCapacity reports whether a VM in this state still holds its vCPUs and
// memory. A failed or stopped VM's process has exited.
func UsesCapacity(state vm.State) bool {
	return state == vm.StateRunning || RetainsRuntime(state)
}

// CapacityHold reports whether starting svc now would take capacity the node
// does not have. See capacityHold.
func (r *Reconciler) CapacityHold(svc config.ServiceConfig) Hold {
	return r.capacityHold(svc, r.vmManager.List())
}

// capacityHold checks a start against the runtime as it is now, not as
// admission expected it: a create or growth may take only capacity that is
// free at this moment, and none while a quarantined VM's size is unknown.
// Every other VM counts at what it uses, or at the size admission allowed it
// if larger, so work admitted earlier keeps its share and a shrink or removal
// frees capacity only once it has happened. svc's own VM counts at what it
// uses now. Only a resource svc would grow is checked, so a shrink or a
// same-size restart proceeds even on a node already over capacity.
func (r *Reconciler) capacityHold(svc config.ServiceConfig, actual map[string]*vm.Instance) Hold {
	r.heldMu.RLock()
	limit := r.capacity
	r.heldMu.RUnlock()
	if limit == nil {
		return Hold{}
	}
	for name, instance := range actual {
		// A quarantined VM whose configuration is unknown may still use any
		// amount of capacity; nothing it might use can be presumed free.
		if name == svc.Name || instance.State != vm.StateRecoveryPending || !instance.RecoveryConfigUnknown {
			continue
		}
		if own := actual[svc.Name]; own == nil || !UsesCapacity(own.State) ||
			svc.VCPUs > own.Config.VCPUs || svc.MemoryMB > own.Config.MemoryMB {
			return Hold{Reason: "capacity used by " + name + " is unknown", Capacity: true}
		}
	}
	var own, used capacity.NodeCapacity
	occupy := func(name string, size capacity.NodeCapacity) {
		if name == svc.Name {
			own = size
			return
		}
		allowed := limit.allowance[name]
		used.VCPUs += max(size.VCPUs, allowed.VCPUs)
		used.MemoryMB += max(size.MemoryMB, allowed.MemoryMB)
	}
	for name, instance := range actual {
		var size capacity.NodeCapacity
		if UsesCapacity(instance.State) {
			size = capacity.NodeCapacity{VCPUs: instance.Config.VCPUs, MemoryMB: instance.Config.MemoryMB}
		}
		occupy(name, size)
	}
	for name := range limit.allowance {
		if actual[name] == nil {
			occupy(name, capacity.NodeCapacity{})
		}
	}
	growsVCPUs := svc.VCPUs > own.VCPUs && used.VCPUs+svc.VCPUs > limit.node.VCPUs
	growsMemory := svc.MemoryMB > own.MemoryMB && used.MemoryMB+svc.MemoryMB > limit.node.MemoryMB
	if !growsVCPUs && !growsMemory {
		return Hold{}
	}
	return Hold{
		Reason: fmt.Sprintf("needs %d vCPUs and %d MB while other VMs use or were admitted %d vCPUs and %d MB of %d vCPUs and %d MB",
			svc.VCPUs, svc.MemoryMB, used.VCPUs, used.MemoryMB, limit.node.VCPUs, limit.node.MemoryMB),
		Capacity: true,
	}
}

// Restart replaces a tracked VM with its current configuration, as a health
// restart does. The caller checks CapacityHold first: a failed or stopped VM
// takes its resources again.
//
// The VM's record moves to creating before the stop, so a crash before the
// new VM is tracked recovers like an interrupted create, and a failed start
// leaves its rules covered by a departing record.
func (r *Reconciler) Restart(ctx context.Context, svc config.ServiceConfig) error {
	id, err := r.journalRestart(svc)
	if err != nil {
		return stageError(FailureStageNetwork, fmt.Errorf("recording the restart of %s: %w", svc.Name, err))
	}
	if stopper, ok := r.vmManager.(vmStopper); ok {
		if err := stopper.Stop(svc.Name); err != nil {
			r.logger.Warn("failed to stop service during health restart", "service", svc.Name, "error", err)
		}
	}
	if err := r.vmManager.Start(ctx, svc); err != nil {
		if _, tracked := r.vmManager.List()[svc.Name]; !tracked && id != 0 {
			r.logPersistFailure(r.setAllocationPhase(id, phaseDeparting))
		}
		return err
	}
	if id != 0 {
		r.logPersistFailure(r.setAllocationPhase(id, phaseRunning))
	}
	return nil
}
