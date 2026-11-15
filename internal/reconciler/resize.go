package reconciler

import (
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/vm"
)

type resizeVMRemover interface{ RemoveForResize(string) error }

type resizeAttempt struct {
	instanceID        string
	request           []config.VolumeConfig
	reason            string
	applicationExited bool
}

func hasEffectiveVolumeResize(previous, effective config.ServiceConfig) bool {
	for _, volume := range effective.Volumes {
		for _, old := range previous.Volumes {
			if old.Name == volume.Name && old.SizeBytes != volume.SizeBytes {
				return true
			}
		}
	}
	return false
}

func (r *Reconciler) deleteForUpdate(previous, effective config.ServiceConfig) error {
	instance := r.vmManager.List()[previous.Name]
	if instance == nil || instance.State != vm.StateRunning || !hasEffectiveVolumeResize(previous, effective) {
		return r.deleteService(previous)
	}
	manager, ok := r.vmManager.(resizeVMRemover)
	var err error
	if !ok {
		err = &vm.GuestQuiesceRequiredError{Cause: errors.New("VM manager cannot confirm guest quiesce")}
	} else {
		err = r.deleteServiceWith(previous, manager.RemoveForResize)
	}
	var refusal *vm.GuestQuiesceRequiredError
	if !errors.As(err, &refusal) {
		return err
	}
	current := r.vmManager.List()[previous.Name]
	if current == nil || current.State != vm.StateRunning || current.InstanceID != instance.InstanceID {
		return fmt.Errorf("VM owner changed during resize refusal: %w", vm.ErrStartInProgress)
	}
	request := effective.Volumes
	if r.planned != nil {
		for _, wanted := range r.planned.Services {
			if wanted.Name == previous.Name {
				request = wanted.Volumes
				break
			}
		}
	}
	reason := "guest_quiesce_required: " + refusal.Error()
	if len(reason) > 512 {
		reason = reason[:512]
	}
	r.heldMu.Lock()
	if r.resizeAttempts == nil {
		r.resizeAttempts = make(map[string]resizeAttempt)
	}
	if refusal.ApplicationExited {
		if r.applicationExits == nil {
			r.applicationExits = make(map[string]string)
		}
		r.applicationExits[previous.Name] = instance.InstanceID
	}
	r.resizeAttempts[previous.Name] = resizeAttempt{instanceID: instance.InstanceID, request: slices.Clone(request), reason: reason, applicationExited: refusal.ApplicationExited}
	r.heldMu.Unlock()
	return (Hold{Reason: reason}).err(previous.Name)
}

func (r *Reconciler) validResizeAttempts(desired config.NodeConfig, actual map[string]*vm.Instance) map[string]resizeAttempt {
	r.heldMu.RLock()
	defer r.heldMu.RUnlock()
	valid := make(map[string]resizeAttempt)
	for _, service := range desired.Services {
		attempt, ok := r.resizeAttempts[service.Name]
		instance := actual[service.Name]
		if ok && instance != nil && instance.State == vm.StateRunning && instance.InstanceID == attempt.instanceID &&
			reflect.DeepEqual(attempt.request, service.Volumes) && hasEffectiveVolumeResize(instance.Config, service) {
			valid[service.Name] = attempt
		}
	}
	return valid
}

func (r *Reconciler) resizeOwners(desired config.NodeConfig, actual map[string]*vm.Instance) []*vm.Instance {
	var owners []*vm.Instance
	for name := range r.validResizeAttempts(desired, actual) {
		owners = append(owners, actual[name])
	}
	return owners
}

func (r *Reconciler) pruneResizeAttempts(desired config.NodeConfig) {
	actual := r.vmManager.List()
	valid := r.validResizeAttempts(desired, actual)
	r.heldMu.Lock()
	defer r.heldMu.Unlock()
	r.resizeAttempts = valid
	for name, id := range r.applicationExits {
		if instance := actual[name]; instance == nil || instance.InstanceID != id {
			delete(r.applicationExits, name)
		}
	}
}

func (r *Reconciler) applicationExited(name string, instance *vm.Instance) bool {
	if instance == nil || instance.State != vm.StateRunning {
		return false
	}
	r.heldMu.RLock()
	defer r.heldMu.RUnlock()
	id, known := r.applicationExits[name]
	return known && id == instance.InstanceID
}

func (r *Reconciler) clearResizeAttempt(name string) {
	r.heldMu.Lock()
	defer r.heldMu.Unlock()
	delete(r.resizeAttempts, name)
}
