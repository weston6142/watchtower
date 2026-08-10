package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/verificationcache"
	"github.com/weston6142/watchtower/internal/workspace"
)

type preparedFinalization struct {
	Decision     marshal.MergeDecision
	Verification marshal.Verification
}

func (e *Engine) prepareFinalization(
	is *issueState, verificationLease *verificationcache.Lease,
) (preparedFinalization, error) {
	artifactDir := filepath.Join(e.issueDir(is.id), "artifacts")
	decision, err := marshal.LoadMergeDecision(filepath.Join(artifactDir, "merge-decision.json"))
	if err != nil {
		return preparedFinalization{}, fmt.Errorf("load merge decision: %w", err)
	}
	verification, err := marshal.LoadVerification(filepath.Join(artifactDir, "verification.json"))
	if err != nil {
		return preparedFinalization{}, fmt.Errorf("load verification receipt: %w", err)
	}
	if err := e.validateFinalIdentity(is, decision, verification, verificationLease); err != nil {
		return preparedFinalization{}, err
	}
	return preparedFinalization{Decision: decision, Verification: verification}, nil
}

func (e *Engine) validateFinalIdentity(
	is *issueState, decision marshal.MergeDecision, receipt marshal.Verification,
	verificationLease *verificationcache.Lease,
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
	if e.cfg.Train != nil && len(e.cfg.Train.TestCmd) > 0 {
		if receipt.CacheEvidence == nil {
			return fmt.Errorf("cache-managed verification receipt is missing cache evidence")
		}
		repository, err := verificationcache.CanonicalRepositoryIdentity(e.cfg.Train.Repo)
		if err != nil {
			return fmt.Errorf("resolve verification cache repository: %w", err)
		}
		current := marshal.CacheIdentity{
			LeaseID: receipt.CacheEvidence.LeaseID, Repository: repository,
			ManagedScope: receipt.CacheEvidence.ManagedScope, BaseSHA: is.baseRef,
			BranchSHA: branchSHA, TreeSHA: treeSHA,
			CommandDigest: verificationcache.CommandDigest(e.cfg.Train.TestCmd),
		}
		if verificationLease != nil {
			if verificationLease.State() != verificationcache.StateComplete {
				return fmt.Errorf("current verification cache lease is not complete")
			}
			current = marshal.CacheIdentity{
				LeaseID: verificationLease.ID(), Repository: repository,
				ManagedScope: verificationLease.ManagedScope(), BaseSHA: verificationLease.BaseSHA(),
				BranchSHA: verificationLease.BranchSHA(), TreeSHA: verificationLease.TreeSHA(),
				CommandDigest: verificationLease.CommandDigest(),
			}
		}
		if err := receipt.CacheEvidence.ValidateAgainst(current); err != nil {
			return fmt.Errorf("validate verification cache identity: %w", err)
		}
		cacheRoot := e.cfg.CacheRoot
		if cacheRoot == "" {
			cacheRoot = e.cfg.DataDir
		}
		runtime, err := verificationcache.New(verificationcache.Config{
			CacheRoot: cacheRoot, RepoDir: e.cfg.Train.Repo,
		})
		if err != nil {
			return fmt.Errorf("initialize verification cache validation: %w", err)
		}
		if err := runtime.ValidateEvidence(receipt.CacheEvidence.RuntimeEvidence()); err != nil {
			return fmt.Errorf("validate durable verification cache evidence: %w", err)
		}
	}
	return nil
}

func (e *Engine) checkpointVerificationReady(is *issueState) error {
	return e.cfg.Store.SetIssueIntegration(store.IssueIntegration{
		IssueID: is.id, State: store.IntegrationVerificationReady, PreSHA: is.baseRef,
		Worktree: is.wsPath, Branch: is.branch,
	})
}

func (e *Engine) integrationStageName(is *issueState) string {
	if stage, _, ok := e.cfg.Flows[is.flowName].IntegrationStage(); ok {
		return stage.Name
	}
	return ""
}

func (e *Engine) recordFinalizationFailure(is *issueState, cause error) error {
	_ = e.recordBoundaryFailure(context.Background(), is.id, e.integrationStageName(is), 0,
		failure.SiteFinalization, failure.ClassStateMismatch, failure.RetryAfterStateChange, failure.StateOperator, cause)
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
		"stage": e.integrationStageName(is), "error": cause.Error(),
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
					_ = e.recordBoundaryFailure(ctx, is.id, e.integrationStageName(is), 0,
						failure.SiteStore, failure.ClassUnavailable, failure.RetryAfterStateChange, failure.StateStore, storeErr)
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
		integration := store.IssueIntegration{
			IssueID: is.id, State: store.IntegrationMerged,
			BaseBranch: result.BaseBranch, PreSHA: result.PreSHA,
			LandedSHA: result.LandedSHA,
			Worktree:  is.wsPath, Branch: is.branch,
		}
		if cleanupErr := e.finishLandingCleanup(
			is.id, integration, is.wsPath, is.branch, is.wsRelease,
		); cleanupErr != nil {
			if ready, readinessErr := e.readiness().Ready(is.id); readinessErr == nil && ready {
				_ = e.wakeDependents(context.Background(), is.id)
			}
			_ = e.recordBoundaryFailure(ctx, is.id, e.integrationStageName(is), 0,
				failure.SiteFinalization, failure.ClassStateMismatch, failure.RetryAfterStateChange, failure.StateOperator, cleanupErr)
			return true, false, cleanupErr
		}
		ready, readinessErr := e.readiness().Ready(is.id)
		if readinessErr != nil {
			return true, false, e.recordFinalizationFailure(is, readinessErr)
		}
		if ready {
			if wakeErr := e.wakeDependents(context.Background(), is.id); wakeErr != nil {
				return true, false, e.recordFinalizationFailure(is, wakeErr)
			}
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

func (e *Engine) restorePersistedWorkspace(is *issueState, run store.RunState) (retErr error) {
	defer func() {
		if retErr != nil {
			retErr = e.recordBoundaryFailure(context.Background(), is.id, run.Stage, 0,
				failure.SiteWorkspace, failure.ClassUnavailable, failure.RetryAfterStateChange,
				failure.StateWorkspace, retErr)
		}
	}()
	if run.Worktree == "" {
		if run.Branch != "" || run.BaseRef != "" {
			return fmt.Errorf("persisted workspace identity is incomplete")
		}
		e.mu.Lock()
		is.wsPath = ""
		is.wsRelease = nil
		is.branch = ""
		is.baseRef = ""
		e.mu.Unlock()
		return nil
	}
	info, err := os.Stat(run.Worktree)
	if err != nil {
		return fmt.Errorf("persisted worktree %s: %w", run.Worktree, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("persisted worktree %s is not a directory", run.Worktree)
	}
	branch, err := gitCommandOutput(run.Worktree, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if branch != run.Branch {
		return fmt.Errorf("persisted branch changed: current %s, snapshot %s", branch, run.Branch)
	}
	var release func() error
	if releaser, ok := e.cfg.Workspace.(workspace.Releaser); ok {
		release = func() error { return releaser.ReleasePath(run.Worktree) }
	}
	e.mu.Lock()
	is.wsPath, is.wsRelease = run.Worktree, release
	is.branch, is.baseRef = run.Branch, run.BaseRef
	e.mu.Unlock()
	return nil
}

// restoreInterruptedWorkspace reconnects a restarted engine to the worktree
// recorded by the last stage run. It is deliberately best-effort: an absent or
// mismatched worktree falls back to the provider's normal Acquire path, while a
// valid issue branch is reused so providers do not collide with their own
// surviving lease.
func (e *Engine) restoreInterruptedWorkspace(is *issueState) {
	runs, err := e.cfg.Store.StageRuns(is.id)
	if err != nil {
		return
	}
	worktreePath := ""
	for index := len(runs) - 1; index >= 0; index-- {
		if runs[index].Worktree != "" {
			worktreePath = runs[index].Worktree
			break
		}
	}
	if worktreePath == "" {
		return
	}
	if info, err := os.Stat(worktreePath); err != nil || !info.IsDir() {
		return
	}
	branch, err := gitCommandOutput(worktreePath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || branch != "issue/"+is.id {
		return
	}
	baseRef, err := gitRevision(worktreePath, "HEAD")
	if err != nil {
		return
	}
	if e.cfg.Train != nil {
		if baseHead, headErr := gitRevision(e.cfg.Train.Repo, "HEAD"); headErr == nil {
			if mergeBase, mergeErr := gitCommandOutput(
				worktreePath, "merge-base", "HEAD", baseHead,
			); mergeErr == nil {
				baseRef = mergeBase
			}
		}
	}
	var release func() error
	if releaser, ok := e.cfg.Workspace.(workspace.Releaser); ok {
		release = func() error { return releaser.ReleasePath(worktreePath) }
	}
	is.wsPath, is.wsRelease = worktreePath, release
	is.branch, is.baseRef = branch, baseRef
}

func (e *Engine) retryVerifiedFinalization(
	ctx context.Context, is *issueState, integration store.IssueIntegration,
) error {
	if err := e.restoreVerifiedWorkspace(is, integration); err != nil {
		return e.recordFinalizationFailure(is, err)
	}
	prepared, err := e.prepareFinalization(is, nil)
	if err != nil {
		verification, recovered, recoveryErr := e.resolvedConflictVerification(is)
		if recoveryErr != nil {
			return e.recordFinalizationFailure(is, recoveryErr)
		}
		if !recovered {
			return e.recordFinalizationFailure(is, err)
		}
		prepared.Verification = verification
	}
	_, _, err = e.finalizeIntegration(ctx, is, prepared.Verification)
	return err
}

// resolvedConflictVerification recovers the narrow window where a conflict
// resolver durably records a resolved, rebased branch but exits before the
// engine can retry the land. The original receipt still supplies the exact
// command set, while clearing TreeSHA forces those commands to run against the
// newly integrated tree.
func (e *Engine) resolvedConflictVerification(
	is *issueState,
) (marshal.Verification, bool, error) {
	decisionPath := filepath.Join(is.wsPath, "conflict-decision.json")
	decision, err := loadConflictDecision(decisionPath)
	if errors.Is(err, os.ErrNotExist) {
		return marshal.Verification{}, false, nil
	}
	if err != nil {
		return marshal.Verification{}, true, err
	}
	if decision != "resolved" {
		return marshal.Verification{}, true, fmt.Errorf(
			"conflict decision %q cannot resume integration", decision)
	}
	if status, err := gitCommandOutput(
		is.wsPath, "status", "--porcelain", "--untracked-files=no",
	); err != nil {
		return marshal.Verification{}, true, err
	} else if status != "" {
		return marshal.Verification{}, true, fmt.Errorf(
			"resolved issue branch is dirty: %s", status)
	}
	baseSHA, err := gitRevision(e.cfg.Train.Repo, "HEAD")
	if err != nil {
		return marshal.Verification{}, true, fmt.Errorf("read current base: %w", err)
	}
	if output, err := exec.Command(
		"git", "-C", is.wsPath, "merge-base", "--is-ancestor", baseSHA, "HEAD",
	).CombinedOutput(); err != nil {
		return marshal.Verification{}, true, fmt.Errorf(
			"resolved issue branch is not rebased onto %s: %v: %s",
			baseSHA, err, strings.TrimSpace(string(output)))
	}
	artifactDir := filepath.Join(e.issueDir(is.id), "artifacts")
	mergeDecision, err := marshal.LoadMergeDecision(
		filepath.Join(artifactDir, "merge-decision.json"),
	)
	if err != nil {
		return marshal.Verification{}, true, fmt.Errorf("load merge decision: %w", err)
	}
	if mergeDecision.Decision != "merge" {
		return marshal.Verification{}, true, fmt.Errorf(
			"merge decision %q cannot resume integration", mergeDecision.Decision)
	}
	verification, err := marshal.LoadVerification(
		filepath.Join(artifactDir, "verification.json"),
	)
	if err != nil {
		return marshal.Verification{}, true, fmt.Errorf("load verification receipt: %w", err)
	}
	if e.cfg.Train != nil && len(e.cfg.Train.TestCmd) > 0 &&
		!verification.Includes(e.cfg.Train.TestCmd) {
		return marshal.Verification{}, true, fmt.Errorf(
			"verification receipt does not include configured verification command %q",
			e.cfg.Train.TestCmd)
	}
	verification.BaseSHA = baseSHA
	verification.BranchSHA, err = gitRevision(is.wsPath, "HEAD")
	if err != nil {
		return marshal.Verification{}, true, fmt.Errorf("read resolved branch: %w", err)
	}
	verification.TreeSHA = ""
	return verification, true, nil
}
