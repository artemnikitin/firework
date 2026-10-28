package vm

import "sync"

type serviceOperationLock struct {
	mu   sync.Mutex
	refs int
}

// lockService serializes lifecycle work for one service while leaving manager
// readers and operations on other services independent.
func (m *Manager) lockService(name string) func() {
	m.operationMu.Lock()
	lock := m.operationLocks[name]
	if lock == nil {
		lock = &serviceOperationLock{}
		m.operationLocks[name] = lock
	}
	lock.refs++
	m.operationMu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		m.operationMu.Lock()
		lock.refs--
		if lock.refs == 0 && m.operationLocks[name] == lock {
			delete(m.operationLocks, name)
		}
		m.operationMu.Unlock()
	}
}

func (m *Manager) quarantineIfCurrent(name string, expected *Instance, instanceID string, reason error) bool {
	m.mu.Lock()
	instance := m.instances[name]
	if instance == nil || instance != expected || instance.instanceID != instanceID || instance.State != StateRunning || instance.manifest == nil || instance.manifest.InstanceID != instanceID {
		m.mu.Unlock()
		return false
	}
	instance.State = StateRecoveryPending
	instance.LastError = reason.Error()
	instance.manifest.LastError = reason.Error()
	_ = writeManifest(manifestPath(instance.manifest.VMDir), instance.manifest)
	m.mu.Unlock()
	m.logger.Error("VM lifecycle requires operator intervention",
		"service", name, "error", reason, "command", "firework-agent clear-recovery "+name)
	return true
}
