package agent

import (
	"sort"
	"time"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/healthcheck"
	"github.com/artemnikitin/firework/internal/statusmodel"
	"github.com/artemnikitin/firework/internal/version"
	"github.com/artemnikitin/firework/internal/vm"
	"github.com/artemnikitin/firework/internal/volume"
)

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

func (a *Agent) failAgentStatus(condition, code, message string) {
	a.setStatusCondition(condition, statusmodel.ConditionFalse, code, message)
	a.refreshAgentStatus(statusmodel.PhaseFailed, code, message)
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
			preparedByID := make(map[string]volume.PreparedVolume, len(instance.Volumes))
			for _, prepared := range instance.Volumes {
				preparedByID[prepared.LogicalID] = prepared
			}
			service.Volumes = buildVolumeStatuses(desired, preparedByID)
		} else {
			service.Volumes = buildVolumeStatuses(desired, nil)
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

	a.currentStatus.SchemaVersion = statusmodel.SchemaVersion
	a.currentStatus.AgentVersion = version.Version
	a.currentStatus.NodeID = a.cfg.NodeID
	a.currentStatus.ObservedAt = now
	a.currentStatus.Phase = phase
	a.currentStatus.DesiredServices = len(a.statusServices)
	a.currentStatus.ReadyServices = ready
	a.currentStatus.ReasonCode = code
	a.currentStatus.Message = statusmodel.BoundedMessage(message)
	a.currentStatus.Services = services
}

func buildVolumeStatuses(service config.ServiceConfig, prepared map[string]volume.PreparedVolume) []statusmodel.VolumeStatus {
	statuses := make([]statusmodel.VolumeStatus, 0, len(service.Volumes))
	for _, desired := range service.Volumes {
		logicalID := service.Name + "/" + desired.Name
		status := statusmodel.VolumeStatus{
			LogicalID: logicalID, Type: string(desired.Type), MountPath: desired.MountPath,
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
