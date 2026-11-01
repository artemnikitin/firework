package reconciler

// MarkRecovered queues a live VM for network and health-monitor restoration on
// the next reconciliation. The agent's main loop owns this map.
func (r *Reconciler) MarkRecovered(service string) {
	if service != "" {
		r.pendingRecovery[service] = struct{}{}
	}
}

// ForgetRecovered retires health state for a VM whose dead runtime was cleared.
// Main-loop ownership keeps this serialized with normal reconciliation. A tick
// that ran after the clear may already have recreated the service; its health
// check belongs to the new VM and stays registered.
func (r *Reconciler) ForgetRecovered(service string) {
	delete(r.pendingRecovery, service)
	if r.healthMon != nil && r.vmManager.List()[service] == nil {
		r.healthMon.Deregister(service)
	}
}
