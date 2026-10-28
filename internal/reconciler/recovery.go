package reconciler

// MarkRecovered queues a live VM for network and health-monitor restoration on
// the next reconciliation. The agent's main loop owns this map.
func (r *Reconciler) MarkRecovered(service string) {
	if service != "" {
		r.pendingRecovery[service] = struct{}{}
	}
}
