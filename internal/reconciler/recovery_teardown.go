package reconciler

import (
	"fmt"

	"github.com/artemnikitin/firework/internal/config"
)

// Recovery removal uses the same write-ahead journal as normal VM removal.
// There is no separate queue or acknowledgement transaction to recover.
type recoveryCleanupRecorder interface {
	SetRecoveryCleanupRecorder(func(config.ServiceConfig) error)
}

func (r *Reconciler) installRecoveryCleanupRecorder() {
	if manager, ok := r.vmManager.(recoveryCleanupRecorder); ok {
		manager.SetRecoveryCleanupRecorder(func(service config.ServiceConfig) error {
			if r.networkMgr == nil {
				return nil // Host networking is explicitly disabled for this agent.
			}
			if r.stateDir == "" {
				return fmt.Errorf("recovery cleanup requires a durable cleanup journal")
			}
			return r.journalIntendedTeardowns(service)
		})
	}
}
