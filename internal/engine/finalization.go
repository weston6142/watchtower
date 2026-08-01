package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/workspace"
)

type preparedFinalization struct {
	Decision     marshal.MergeDecision
	Verification marshal.Verification
}

func (e *Engine) prepareFinalization(is *issueState) (preparedFinalization, error) {
	artifactDir := filepath.Join(e.issueDir(is.id), "artifacts")
	decision, err := marshal.LoadMergeDecision(filepath.Join(artifactDir, "merge-decision.json"))
	if err != nil {
		return preparedFinalization{}, fmt.Errorf("load merge decision: %w", err)
	}
	verification, err := marshal.LoadVerification(filepath.Join(artifactDir, "verification.json"))
	if err != nil {
		return preparedFinalization{}, fmt.Errorf("load verification receipt: %w", err)
	}
	if err := e.validateFinalIdentity(is, decision, verification); err != nil {
		return preparedFinalization{}, err
	}
	return preparedFinalization{Decision: decision, Verification: verification}, nil
}

func (e *Engine) validateFinalIdentity(
	is *issueState, decision marshal.MergeDecision, receipt marshal.Verification,
) error {
	branchSHA, err := gitRevision(is.wsPath, "HEAD")
	if err != nil {
		return fmt.Errorf("read verification branch commit: %w", err)
	}
	treeSHA, err := gitRevision(is.wsPath, "HEAD^{tree}")
	if err != nil {
		return fmt.Errorf("read verified tree: %w", err)
	}
	if receipt.BaseSHA != is.baseRef {
		return fmt.Errorf("verification base %s does not match issue base %s",
			receipt.BaseSHA, is.baseRef)
	}
	if receipt.BranchSHA != branchSHA {
		return fmt.Errorf("verification branch %s does not match current branch %s",
			receipt.BranchSHA, branchSHA)
	}
	if !receipt.AppliesTo(treeSHA) {
		return fmt.Errorf("verified tree %s does not match current tree %s",
			receipt.TreeSHA, treeSHA)
	}
	if decision.BranchCommit != "" && decision.BranchCommit != branchSHA {
		return fmt.Errorf("merge decision branch %s does not match current branch %s",
			decision.BranchCommit, branchSHA)
	}
	if decision.BaseCommit != "" && decision.BaseCommit != is.baseRef {
		return fmt.Errorf("merge decision base %s does not match issue base %s",
			decision.BaseCommit, is.baseRef)
	}
	if e.cfg.Train != nil && len(e.cfg.Train.TestCmd) > 0 &&
		!receipt.Includes(e.cfg.Train.TestCmd) {
		return fmt.Errorf(
			"verification receipt does not include configured verification command %q",
			e.cfg.Train.TestCmd)
	}
	return nil
}

func (e *Engine) checkpointVerificationReady(is *issueState) error {
	return e.cfg.Store.SetIssueIntegration(store.IssueIntegration{
		IssueID: is.id, State: store.IntegrationVerificationReady, PreSHA: is.baseRef,
		Worktree: is.wsPath, Branch: is.branch,
	})
}

func (e *Engine) recordFinalizationFailure(is *issueState, cause error) error {
	integration, ok, err := e.cfg.Store.IssueIntegration(is.id)
	if err != nil {
		return fmt.Errorf("%v (read finalization checkpoint: %w)", cause, err)
	}
	if !ok || integration.State != store.IntegrationVerificationReady {
		return cause
	}
	integration.LastError = cause.Error()
	if err := e.cfg.Store.SetIssueIntegration(integration); err != nil {
		return fmt.Errorf("%v (persist finalization failure: %w)", cause, err)
	}
	e.emit(core.EvFinalizationFailed, is.id, map[string]string{
		"stage": "merge-verification", "error": cause.Error(),
	})
	return cause
}

func (e *Engine) finalizeIntegration(
	ctx context.Context, is *issueState, verification marshal.Verification,
) (landed, preserveWorkspace bool, err error) {
	if e.cfg.Train != nil && is.branch != "" {
		e.emit(core.EvMergeStarted, is.id, map[string]string{"branch": is.branch})
		result, landErr := e.landWithEscalation(ctx, is, verification)
		if landErr != nil {
			var pending *marshal.PublishPendingError
			if errors.As(landErr, &pending) {
				integration := store.IssueIntegration{
					IssueID: is.id, State: store.IntegrationPublishPending,
					BaseBranch: result.BaseBranch, PreSHA: result.PreSHA,
					LandedSHA: result.LandedSHA, LastError: landErr.Error(),
					Worktree: is.wsPath, Branch: is.branch,
					Cleanup: cleanupOperations(is.wsPath, is.branch),
				}
				if storeErr := e.cfg.Store.SetIssueIntegration(integration); storeErr != nil {
					return false, true, fmt.Errorf("%v (persist publish pending: %w)", landErr, storeErr)
				}
				e.emit(core.EvPublishPending, is.id, map[string]string{
					"branch": result.BaseBranch, "commit": result.LandedSHA,
					"error": landErr.Error(),
				})
				return false, true, landErr
			}
			if errors.Is(landErr, errConflictHeld) {
				e.emit(core.EvIssueCompleted, is.id, map[string]string{
					"merge": "left-unmerged", "branch": is.branch})
				if e.cfg.Marshal != nil {
					e.cfg.Marshal.Merged(is.id)
				}
				return false, false, nil
			}
			return false, true, e.recordFinalizationFailure(is, landErr)
		}
		landed = true
		if e.cfg.Train.Push {
			e.emit(core.EvPublishSucceeded, is.id, map[string]string{
				"branch": result.BaseBranch, "commit": result.LandedSHA})
		}
		e.emit(core.EvIssueMerged, is.id, map[string]string{"branch": is.branch})
		e.wakeDependents(context.Background(), is.id)
		integration := store.IssueIntegration{
			IssueID: is.id, State: store.IntegrationMerged,
			BaseBranch: result.BaseBranch, PreSHA: result.PreSHA,
			LandedSHA: result.LandedSHA,
			Worktree:  is.wsPath, Branch: is.branch,
		}
		if cleanupErr := e.finishLandingCleanup(
			is.id, integration, is.wsPath, is.branch, is.wsRelease,
		); cleanupErr != nil {
			return true, false, cleanupErr
		}
	}
	if e.cfg.Marshal != nil {
		e.cfg.Marshal.Merged(is.id)
	}
	e.emit(core.EvIssueCompleted, is.id, nil)
	return landed, false, nil
}

func (e *Engine) restoreVerifiedWorkspace(
	is *issueState, integration store.IssueIntegration,
) error {
	if integration.Worktree == "" || integration.Branch == "" || integration.PreSHA == "" {
		return fmt.Errorf("verification checkpoint is missing workspace identity")
	}
	if _, err := os.Stat(integration.Worktree); err != nil {
		return fmt.Errorf("verified worktree %s: %w", integration.Worktree, err)
	}
	branch, err := gitCommandOutput(integration.Worktree, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if branch != integration.Branch {
		return fmt.Errorf("verified branch changed: current %s, checkpoint %s", branch, integration.Branch)
	}
	var release func() error
	if releaser, ok := e.cfg.Workspace.(workspace.Releaser); ok {
		release = func() error { return releaser.ReleasePath(integration.Worktree) }
	}
	e.mu.Lock()
	is.wsPath, is.wsRelease = integration.Worktree, release
	is.branch, is.baseRef = integration.Branch, integration.PreSHA
	e.mu.Unlock()
	return nil
}

func (e *Engine) retryVerifiedFinalization(
	ctx context.Context, is *issueState, integration store.IssueIntegration,
) error {
	if err := e.restoreVerifiedWorkspace(is, integration); err != nil {
		return e.recordFinalizationFailure(is, err)
	}
	prepared, err := e.prepareFinalization(is)
	if err != nil {
		return e.recordFinalizationFailure(is, err)
	}
	_, _, err = e.finalizeIntegration(ctx, is, prepared.Verification)
	return err
}
