package reconciler

import (
	"fmt"
	"sort"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/vm"
)

type recoveryTeardownSource interface {
	PendingRecoveryTeardowns() (map[string]config.ServiceConfig, error)
	AcknowledgeRecoveryTeardown(string) error
}

func (r *Reconciler) acceptRecoveryTeardowns() error {
	source, ok := r.vmManager.(recoveryTeardownSource)
	if !ok {
		return nil
	}
	pending, readErr := source.PendingRecoveryTeardowns()
	var errs []error
	if readErr != nil {
		errs = append(errs, stageError(FailureStageNetwork, fmt.Errorf("read recovery cleanup outbox: %w", readErr)))
	}
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		service := pending[id]
		if instance := r.vmManager.List()[service.Name]; instance != nil && (instance.State == vm.StateRecoveryPending || instance.State == vm.StateStarting || instance.State == vm.StateStartAborting) {
			// Deletion may have failed after the outbox was committed. Retain
			// the event until clearance finishes; claimant guards deliberately
			// discard cleanup journal entries still used by quarantined VMs.
			errs = append(errs, stageError(FailureStageVM, fmt.Errorf("recovery cleanup for %s awaits completed runtime clearance", service.Name)))
			continue
		}
		if r.networkMgr == nil && service.Network != nil {
			errs = append(errs, stageError(FailureStageNetwork, fmt.Errorf("recovery cleanup for %s requires host network cleanup to be enabled", service.Name)))
			continue
		}
		if r.networkMgr != nil && r.stateDir == "" {
			errs = append(errs, stageError(FailureStageNetwork, fmt.Errorf("recovery cleanup for %s requires a durable cleanup journal", service.Name)))
			continue
		}
		if err := r.journalIntendedTeardowns(service); err != nil {
			errs = append(errs, stageError(FailureStageNetwork, fmt.Errorf("journal recovery cleanup for %s: %w", service.Name, err)))
			continue
		}
		if err := source.AcknowledgeRecoveryTeardown(id); err != nil {
			errs = append(errs, stageError(FailureStageNetwork, fmt.Errorf("acknowledge recovery cleanup for %s: %w", service.Name, err)))
		}
	}
	return combineErrors(errs)
}
