package engine

import (
	"fmt"
	"path/filepath"

	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/store"
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
	})
}
