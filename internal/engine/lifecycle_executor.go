package engine

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/verificationcache"
)

// lifecycleExecutor is the private boundary for repository lifecycle
// authority. It deliberately is not exposed through runner requests, provider
// contexts, or capability operations.
type lifecycleExecutor struct {
	engine *Engine
}

func (x lifecycleExecutor) verify(
	ctx context.Context, is *issueState, stage, capabilityAttemptID, workdir string,
) (runErr error) {
	if x.engine.cfg.Train == nil || len(x.engine.cfg.Train.TestCmd) == 0 {
		return fmt.Errorf("integrating stage %s requires engine test_cmd", stage)
	}
	if err := x.engine.injectFailure(ctx, failure.SiteVerification, is.id, stage); err != nil {
		return err
	}
	record, found, err := x.engine.cfg.Store.CapabilityAttempt(is.id, stage, capabilityAttemptID)
	if err != nil {
		return fmt.Errorf("load capability validation: %w", err)
	}
	if !found || !record.Validation.Passed || record.ImmutableResultID == "" ||
		record.Validation.ResultDigest != record.ImmutableResultID {
		return fmt.Errorf("final verification requires a bound successful capability result")
	}

	current, currentFound, err := x.engine.cfg.Store.CurrentVerificationAttempt(is.id)
	if err != nil {
		return fmt.Errorf("load verification attempt: %w", err)
	}
	if currentFound && current.Status == store.VerificationAttemptPassed {
		receipt, loadErr := loadVerificationBytes(current.ReceiptJSON)
		if loadErr == nil {
			decision, decisionErr := marshal.LoadMergeDecision(filepath.Join(workdir, "merge-decision.json"))
			if decisionErr == nil && x.engine.validateFinalIdentityForAttempt(is, decision, receipt, nil, current.ID) == nil {
				if err := writeFileAtomic(workdir, "verification.json", current.ReceiptJSON); err != nil {
					return err
				}
				_, err = contextpack.Archive(workdir, x.engine.issueDir(is.id), []string{"verification.json"})
				return err
			}
		}
	}

	branchSHA, treeSHA, err := verificationIdentity(workdir)
	if err != nil {
		return fmt.Errorf("verification identity: %w", err)
	}
	cacheRoot := x.engine.cfg.CacheRoot
	if cacheRoot == "" {
		cacheRoot = x.engine.cfg.DataDir
	}
	if err := x.engine.injectFailure(ctx, failure.SiteCache, is.id, stage); err != nil {
		return err
	}
	runtime, err := verificationcache.New(verificationcache.Config{
		CacheRoot: cacheRoot, RepoDir: x.engine.cfg.Train.Repo,
	})
	if err != nil {
		return fmt.Errorf("initialize verification cache: %w", err)
	}
	lease, err := runtime.Acquire(ctx, verificationcache.Config{
		RepoDir: x.engine.cfg.Train.Repo, BaseSHA: is.baseRef, BranchSHA: branchSHA,
		TreeSHA: treeSHA, Argv: append([]string(nil), x.engine.cfg.Train.TestCmd...),
	})
	if err != nil {
		return fmt.Errorf("acquire verification cache lease: %w", err)
	}
	sealed := false
	defer func() {
		if !sealed {
			reason := "engine verification did not complete"
			if runErr != nil {
				reason = runErr.Error()
			}
			_ = lease.Quarantine(reason)
		}
		_ = lease.Close()
	}()
	if err := x.engine.writeVerificationReceipt(ctx, is, workdir, lease); err != nil {
		return err
	}
	sealed = true
	receiptJSON, err := verificationReceiptBytes(workdir)
	if err != nil {
		return fmt.Errorf("read verification receipt: %w", err)
	}
	receipt, err := loadVerificationBytes(receiptJSON)
	if err != nil {
		return fmt.Errorf("validate verification receipt: %w", err)
	}
	decision, err := marshal.LoadMergeDecision(filepath.Join(workdir, "merge-decision.json"))
	if err != nil {
		return err
	}
	if err := x.engine.validateFinalIdentityForAttempt(is, decision, receipt, lease, 0); err != nil {
		return err
	}
	if currentFound && current.Status == store.VerificationAttemptPending {
		if _, err := x.engine.cfg.Store.FinishVerificationAttempt(
			is.id, current.ID, store.VerificationAttemptPassed, receiptJSON, "",
		); err != nil {
			return fmt.Errorf("finish verification attempt: %w", err)
		}
	} else if currentFound {
		if _, err := x.engine.cfg.Store.RecordVerificationAttempt(store.VerificationAttempt{
			IssueID: is.id, Stage: stage, ParentID: current.ID,
			Status: store.VerificationAttemptPassed, Reason: "engine verification rerun",
			ReceiptJSON: receiptJSON,
		}); err != nil {
			return fmt.Errorf("record verification attempt: %w", err)
		}
	} else if _, err := x.engine.cfg.Store.RecordVerificationAttempt(store.VerificationAttempt{
		IssueID: is.id, Stage: stage, Status: store.VerificationAttemptPassed,
		ReceiptJSON: receiptJSON,
	}); err != nil {
		return fmt.Errorf("record verification attempt: %w", err)
	}
	if _, err := contextpack.Archive(workdir, x.engine.issueDir(is.id), []string{"verification.json"}); err != nil {
		return fmt.Errorf("archive engine verification receipt: %w", err)
	}
	return nil
}

func agentOwnedArtifacts(names []string) []string {
	result := make([]string, 0, len(names))
	for _, name := range names {
		if name != "verification.json" {
			result = append(result, name)
		}
	}
	return result
}
