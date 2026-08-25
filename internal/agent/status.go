package agent

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/healthcheck"
	"github.com/artemnikitin/firework/internal/statusmodel"
	"github.com/artemnikitin/firework/internal/version"
	"github.com/artemnikitin/firework/internal/vm"
	"github.com/artemnikitin/firework/internal/volume"
)

// beginTickStatus starts tracking which conditions the current tick evaluates.
// Conditions are observations of the current attempt, not durable failure
// latches; unevaluated conditions are finalized as unknown at tick completion.
func (a *Agent) beginTickStatus() {
	a.statusMu.Lock()
	a.evaluatedConditions = make(map[string]struct{}, len(statusmodel.ReconciliationConditionTypes()))
	a.statusMu.Unlock()
	a.refreshAgentStatus(statusmodel.PhaseReconciling, "", "")
}

func (a *Agent) finishTickStatus() {
	a.statusMu.Lock()
	evaluated := a.evaluatedConditions
	a.evaluatedConditions = nil
	a.statusMu.Unlock()
	if evaluated == nil {
		return
	}
	// Shared with the control plane so the two cannot drift apart.
	for _, conditionType := range statusmodel.ReconciliationConditionTypes() {
		if _, ok := evaluated[conditionType]; !ok {
			a.setStatusCondition(conditionType, statusmodel.ConditionUnknown, "not_reached", "")
		}
	}
	// Conditions just changed. failAgentStatus already snapshotted metrics
	// earlier in the tick, before these were finalized, so without refreshing
	// here Prometheus keeps reporting conditions that /status and the registry
	// have since reported as unknown — or omits them entirely on a first
	// failing tick. All three must describe the same tick.
	a.metrics.setAgentStatusSnapshot(a.agentStatusSnapshot())
}

// markUnchangedRevisionReady records the stages covered by the unchanged
// revision fast path before it publishes PhaseReady. This keeps phase and
// conditions from disagreeing after an earlier failed attempt.
//
// The stages are not all verified equally, and the control plane treats every
// one of them as positive evidence for convergence, so the difference matters:
//   - NetworkReady is genuinely re-asserted — the fast path converges port
//     forwards and only reaches here when that succeeded.
//   - CapacityReady and ImagesReady are inferred from the revision being
//     unchanged: the same revision demands the same capacity and the same
//     images, both already satisfied when it was first applied.
//   - VMsReconciled and Reconciled describe work whose *inputs* have not
//     changed, which is not the same as its outputs still holding. A VM can
//     fail after the revision was applied, and this path skips reconciliation
//     entirely, so those two are asserted only after checking that no VM has
//     actually failed. Without that check a crashed workload keeps reporting
//     converged: the service summary says failed, but the fleet view only
//     requires the service to be present with true conditions.
//
// Returns false when it could not honestly claim readiness, so the caller
// does not go on to publish PhaseReady over the top of a failed condition.
func (a *Agent) markUnchangedRevisionReady() bool {
	return a.markUnchangedRevisionReadyWith(a.failedVMNames())
}

// markUnchangedRevisionReadyWith is markUnchangedRevisionReady's decision,
// separated from the VM state it reads so it can be exercised directly.
func (a *Agent) markUnchangedRevisionReadyWith(failed []string) bool {
	ready := []string{"NetworkReady", "CapacityReady", "ImagesReady", "VMsReconciled", "Reconciled"}
	if len(failed) > 0 {
		message := fmt.Sprintf("VMs in a failed state: %s", strings.Join(failed, ", "))
		for _, conditionType := range []string{"NetworkReady", "CapacityReady", "ImagesReady"} {
			if a.statusConditionIs(conditionType, statusmodel.ConditionTrue) {
				a.markConditionEvaluated(conditionType)
				continue
			}
			a.setStatusCondition(conditionType, statusmodel.ConditionTrue, "unchanged_revision", "")
		}
		// Same condition vocabulary the reconciling path uses for a failed VM,
		// so the two cannot describe the same state differently.
		a.setStatusCondition("VMsReconciled", statusmodel.ConditionFalse, "vm_reconcile_failed", message)
		a.failAgentStatus("Reconciled", "vm_reconcile_failed", message)
		return false
	}
	for _, conditionType := range ready {
		if a.statusConditionIs(conditionType, statusmodel.ConditionTrue) {
			a.markConditionEvaluated(conditionType)
			continue
		}
		a.setStatusCondition(conditionType, statusmodel.ConditionTrue, "unchanged_revision", "")
	}
	return true
}

// failedVMNames lists the running services whose VM is in a failed state, in
// deterministic order.
func (a *Agent) failedVMNames() []string {
	if a.vmManager == nil {
		return nil
	}
	return failedVMNamesFrom(a.vmManager.List())
}

func failedVMNamesFrom(instances map[string]*vm.Instance) []string {
	var failed []string
	for name, instance := range instances {
		if instance != nil && instance.State == vm.StateFailed {
			failed = append(failed, name)
		}
	}
	sort.Strings(failed)
	return failed
}

func (a *Agent) setStatusServices(node config.NodeConfig, fallbackRevision string) {
	services := make([]config.ServiceConfig, len(node.Services))
	for i := range node.Services {
		services[i] = node.Services[i]
		if node.Services[i].Network != nil {
			network := *node.Services[i].Network
			services[i].Network = &network
		}
		if node.Services[i].HealthCheck != nil {
			healthCheck := *node.Services[i].HealthCheck
			services[i].HealthCheck = &healthCheck
		}
		services[i].Volumes = append([]config.VolumeConfig(nil), node.Services[i].Volumes...)
	}
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	a.statusServices = services
	a.currentStatus.DesiredRevision = node.DesiredRevision
	a.currentStatus.PlacementRevision = node.PlacementRevision
	a.currentStatus.ObservedRevision = node.RenderedRevision
	if a.currentStatus.ObservedRevision == "" {
		a.currentStatus.ObservedRevision = fallbackRevision
	}
}

func (a *Agent) recordRestart(name string) {
	a.statusMu.Lock()
	a.restartCounts[name]++
	a.statusMu.Unlock()
}

func (a *Agent) setStatusCondition(kind string, value statusmodel.ConditionStatus, code, message string) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	now := time.Now().UTC()
	message = statusmodel.BoundedMessage(message)
	if a.evaluatedConditions != nil {
		a.evaluatedConditions[kind] = struct{}{}
	}
	for i := range a.currentStatus.Conditions {
		condition := &a.currentStatus.Conditions[i]
		if condition.Type != kind {
			continue
		}
		if condition.Status != value || condition.ReasonCode != code || condition.Message != message {
			condition.LastTransitionAt = now
		}
		condition.Status = value
		condition.ReasonCode = code
		condition.Message = message
		return
	}
	a.currentStatus.Conditions = append(a.currentStatus.Conditions, statusmodel.Condition{
		Type: kind, Status: value, ReasonCode: code, Message: message, LastTransitionAt: now,
	})
	sort.Slice(a.currentStatus.Conditions, func(i, j int) bool {
		return a.currentStatus.Conditions[i].Type < a.currentStatus.Conditions[j].Type
	})
}

func (a *Agent) markConditionEvaluated(kind string) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	if a.evaluatedConditions != nil {
		a.evaluatedConditions[kind] = struct{}{}
	}
}

func (a *Agent) statusConditionIs(kind string, want statusmodel.ConditionStatus) bool {
	a.statusMu.RLock()
	defer a.statusMu.RUnlock()
	for _, condition := range a.currentStatus.Conditions {
		if condition.Type == kind {
			return condition.Status == want
		}
	}
	return false
}

func (a *Agent) failAgentStatus(condition, code, message string) {
	a.setStatusCondition(condition, statusmodel.ConditionFalse, code, message)
	a.refreshAgentStatus(statusmodel.PhaseFailed, code, message)
	a.metrics.setAgentStatusSnapshot(a.agentStatusSnapshot())
}

// incompleteAgentStatus records a stage that neither succeeded nor failed. The
// node stays in the reconciling phase and retries, rather than being published
// as failed for what is a benign race.
func (a *Agent) incompleteAgentStatus(condition, code, message string) {
	a.setStatusCondition(condition, statusmodel.ConditionUnknown, code, message)
	a.refreshAgentStatus(statusmodel.PhaseReconciling, code, message)
	a.metrics.setAgentStatusSnapshot(a.agentStatusSnapshot())
}

func (a *Agent) markAgentStatusApplied(revision string) {
	a.statusMu.Lock()
	if observed := a.currentStatus.ObservedRevision; observed != "" {
		revision = observed
	}
	a.currentStatus.AppliedRevision = revision
	a.currentStatus.LastAppliedAt = time.Now().UTC()
	a.statusMu.Unlock()
	a.refreshAgentStatus(statusmodel.PhaseReady, "", "")
	a.metrics.setAgentStatusSnapshot(a.agentStatusSnapshot())
}

func (a *Agent) refreshAgentStatus(phase statusmodel.Phase, code, message string) {
	instances := a.vmManager.List()
	results := make(map[string]healthcheck.Result)
	if a.healthMon != nil {
		results = a.healthMon.Results()
	}

	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	now := time.Now().UTC()
	previous := make(map[string]statusmodel.ServiceStatus, len(a.currentStatus.Services))
	for _, service := range a.currentStatus.Services {
		previous[service.Name] = service
	}

	services := make([]statusmodel.ServiceStatus, 0, len(a.statusServices))
	ready := 0
	for _, desired := range a.statusServices {
		service := statusmodel.ServiceStatus{Name: desired.Name, VMState: "unknown", Health: "unknown"}
		if desired.Network != nil {
			service.NetworkAddress = desired.Network.GuestIP
		}
		if desired.HealthCheck == nil {
			service.Health = "not_configured"
		} else {
			service.HealthCheckType = desired.HealthCheck.Type
		}
		if instance := instances[desired.Name]; instance != nil {
			service.VMState = string(instance.State)
			if instance.State == vm.StateRunning {
				service.PID = instance.PID
			}
			if instance.State == vm.StateFailed {
				service.ReasonCode = "vm_failed"
				service.Message = statusmodel.BoundedMessage(instance.LastError)
			}
			if instance.State == vm.StateRecoveryPending {
				service.ReasonCode = "vm_recovery_pending"
				service.Message = statusmodel.BoundedMessage(instance.LastError)
			}
			preparedByID := make(map[string]volume.PreparedVolume, len(instance.Volumes))
			for _, prepared := range instance.Volumes {
				preparedByID[prepared.LogicalID] = prepared
			}
			service.Volumes = BuildVolumeStatuses(desired, preparedByID)
		} else {
			service.Volumes = BuildVolumeStatuses(desired, nil)
		}
		if volumeError := a.vmManager.VolumeError(desired.Name); volumeError != "" {
			service.ReasonCode = "volume_failed"
			service.Message = statusmodel.BoundedMessage(volumeError)
			desiredGeneration := make(map[string]int64, len(desired.Volumes))
			for _, desiredVolume := range desired.Volumes {
				desiredGeneration[desired.Name+"/"+desiredVolume.Name] = desiredVolume.ResizeGeneration
			}
			for i := range service.Volumes {
				service.Volumes[i].State = "error"
				service.Volumes[i].LastError = statusmodel.BoundedMessage(volumeError)
				service.Volumes[i].ResizeGeneration = desiredGeneration[service.Volumes[i].LogicalID]
			}
		}
		if result, ok := results[desired.Name]; ok && service.VMState == string(vm.StateRunning) {
			service.Health = string(result.Status)
			service.HealthLastCheckedAt = result.LastChecked.UTC()
			service.HealthFailures = result.Failures
			service.Message = statusmodel.BoundedMessage(result.LastError)
			if result.LastError != "" {
				service.ReasonCode = "health_check_failed"
			}
		}
		if service.VMState == "running" && service.Health != "unhealthy" {
			ready++
		}
		service.RestartCount = a.restartCounts[desired.Name]
		prev, existed := previous[desired.Name]
		if !existed || prev.VMState != service.VMState || prev.Health != service.Health {
			service.LastTransitionAt = now
		} else {
			service.LastTransitionAt = prev.LastTransitionAt
		}
		services = append(services, service)
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	servicesTruncated := len(services) > statusmodel.MaxServices
	if servicesTruncated {
		services = services[:statusmodel.MaxServices]
	}

	a.currentStatus.SchemaVersion = statusmodel.SchemaVersion
	a.currentStatus.AgentVersion = version.Version
	a.currentStatus.NodeID = a.cfg.NodeID
	a.currentStatus.ObservedAt = now
	a.currentStatus.Phase = phase
	a.currentStatus.DesiredServices = len(a.statusServices)
	a.currentStatus.ReadyServices = ready
	a.currentStatus.ServicesTruncated = servicesTruncated
	a.currentStatus.ReasonCode = code
	a.currentStatus.Message = statusmodel.BoundedMessage(message)
	a.currentStatus.Services = services
}

// BuildVolumeStatuses builds the reported volume statuses for a service from
// its desired config and (if the VM is running) its prepared volumes.
// Exported so other packages can construct a status through the same path a
// real agent uses, rather than hand-building a statusmodel.VolumeStatus and
// only coincidentally matching what this function actually sends.
func BuildVolumeStatuses(service config.ServiceConfig, prepared map[string]volume.PreparedVolume) []statusmodel.VolumeStatus {
	statuses := make([]statusmodel.VolumeStatus, 0, len(service.Volumes))
	for _, desired := range service.Volumes {
		// logicalID (untruncated) is the map key shared with volume.Manager's
		// PreparedVolume.LogicalID, so the prepared lookup below must use it
		// as-is; only the reported field is bounded for the wire.
		logicalID := service.Name + "/" + desired.Name
		status := statusmodel.VolumeStatus{
			LogicalID: statusmodel.BoundedLogicalID(logicalID), Type: string(desired.Type), MountPath: statusmodel.BoundedPath(desired.MountPath),
			BoundNode: desired.BoundNode, SharedBackendID: desired.SharedBackendID,
			DesiredSizeBytes: desired.SizeBytes, ResizeGeneration: desired.ResizeGeneration,
			State: "pending",
		}
		if applied, ok := prepared[logicalID]; ok {
			status.AppliedSizeBytes = applied.SizeBytes
			status.ResizeGeneration = applied.ResizeGeneration
			status.State = "prepared"
		}
		statuses = append(statuses, status)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].LogicalID < statuses[j].LogicalID })
	return statuses
}

func (a *Agent) agentStatusSnapshot() statusmodel.AgentStatus {
	a.refreshAgentStatusFromRuntime()
	a.statusMu.RLock()
	defer a.statusMu.RUnlock()
	out := a.currentStatus
	out.Conditions = append([]statusmodel.Condition(nil), a.currentStatus.Conditions...)
	out.Services = append([]statusmodel.ServiceStatus(nil), a.currentStatus.Services...)
	return out
}

func (a *Agent) refreshAgentStatusFromRuntime() {
	a.statusMu.RLock()
	phase := a.currentStatus.Phase
	code := a.currentStatus.ReasonCode
	message := a.currentStatus.Message
	a.statusMu.RUnlock()
	a.refreshAgentStatus(phase, code, message)
}
