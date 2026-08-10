package engine

import (
	"fmt"

	"github.com/weston6142/watchtower/internal/store"
)

type integrationCheckpointSource interface {
	IssueIntegration(string) (store.IssueIntegration, bool, error)
}

type dependencyReadiness interface {
	Ready(parentID string) (bool, error)
}

type durableDependencyReadiness struct {
	source integrationCheckpointSource
}

func (r durableDependencyReadiness) Ready(parentID string) (bool, error) {
	integration, found, err := r.source.IssueIntegration(parentID)
	if err != nil {
		return false, fmt.Errorf("read integration checkpoint for dependency %s: %w", parentID, err)
	}
	return found && (integration.State == store.IntegrationMerged ||
		integration.State == store.IntegrationCleanupNeeded), nil
}
