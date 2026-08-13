package store

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
