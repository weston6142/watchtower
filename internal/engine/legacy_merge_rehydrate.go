package engine

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/weston6142/watchtower/internal/store"
)

func (e *Engine) reconcileLegacyMerges(rows []store.IssueRow) error {
	events, err := e.cfg.Store.EventsSince(0)
	if err != nil {
		return fmt.Errorf("load lifecycle evidence for legacy merges: %w", err)
	}
	for _, row := range rows {
		if row.State != "done" && row.State != "merged" {
			continue
		}
		if _, ok, err := e.cfg.Store.IssueIntegration(row.ID); err != nil {
			return fmt.Errorf("read integration checkpoint for %s: %w", row.ID, err)
		} else if ok {
			continue
		}

		if e.cfg.Train == nil || e.cfg.Train.Repo == "" {
			continue
		}
		baseBranch, err := e.cfg.Train.DefaultBranch()
		if err != nil || baseBranch == "" {
			continue
		}
		evidence, err := foldLegacyMergeEvidence(row, events, baseBranch)
		var rejection *legacyEvidenceError
		if errors.As(err, &rejection) {
			continue
		}
		if err != nil {
			return fmt.Errorf("fold legacy merge evidence for %s: %w", row.ID, err)
		}
		landedSHA := evidence.LandedSHA
		if evidence.Kind == legacyMergeBranchOnly {
			baseSHA, resolveErr := resolveLegacyBaseSHA(e.cfg.Train.Repo, evidence.BaseBranch)
			if resolveErr != nil {
				continue
			}
			landedSHA = matchCanonicalLegacyMerge(
				e.cfg.Train.Repo, baseSHA, evidence.BaseBranch, evidence.IssueBranch,
			)
			if landedSHA == "" {
				continue
			}
		} else if err := proveLegacyCommitReachable(
			e.cfg.Train.Repo, evidence.LandedSHA, evidence.BaseBranch,
		); err != nil {
			continue
		}
		if err := e.cfg.Store.SetIssueIntegration(store.IssueIntegration{
			IssueID: evidence.IssueID, State: store.IntegrationMerged,
			BaseBranch: evidence.BaseBranch, LandedSHA: landedSHA,
		}); err != nil {
			return fmt.Errorf("persist legacy merge integration for %s: %w", row.ID, err)
		}
	}
	return nil
}

func proveLegacyCommitReachable(repo, landedSHA, baseBranch string) error {
	resolve := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--end-of-options", landedSHA+"^{commit}")
	resolvedOutput, err := resolve.CombinedOutput()
	resolved := strings.TrimSpace(string(resolvedOutput))
	if err != nil {
		return fmt.Errorf("recorded landed commit %s is not a commit: %v: %s",
			landedSHA, err, resolved)
	}
	if !strings.EqualFold(resolved, landedSHA) {
		return fmt.Errorf("recorded landed commit %q is not a canonical commit ID", landedSHA)
	}

	cmd := exec.Command("git", "-C", repo, "merge-base", "--is-ancestor", landedSHA, baseBranch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("commit %s is not reachable from %s: %v: %s",
			landedSHA, baseBranch, err, strings.TrimSpace(string(out)))
	}
	return nil
}
