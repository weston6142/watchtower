package store

// FinalizationBoundaries returns verification-to-merge checkpoints in
// production order as a fresh slice. The claimed, preserved, and
// capability_recovery_needed ownership, hold, and recovery states are excluded.
func FinalizationBoundaries() []string {
	return []string{
		IntegrationVerificationReady,
		IntegrationPendingReverification,
		IntegrationReverificationFailed,
		IntegrationPublishPending,
		IntegrationCleanupNeeded,
		IntegrationMerged,
	}
}
