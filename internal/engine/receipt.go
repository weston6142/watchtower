package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/verificationcache"
)

// writeVerificationReceipt runs the configured verification command in the
// issue worktree and records the receipt itself, so merge verification never
// depends on an agent-authored verification.json. Without a configured
// test_cmd the agent's receipt (or the fake runner's) remains authoritative.
func (e *Engine) writeVerificationReceipt(
	ctx context.Context, is *issueState, workdir string, leases ...*verificationcache.Lease,
) error {
	if e.cfg.Train == nil || len(e.cfg.Train.TestCmd) == 0 {
		return nil
	}
	commands := [][]string{e.cfg.Train.TestCmd}
	var lease *verificationcache.Lease
	if len(leases) > 0 {
		lease = leases[0]
	}
	var replayErr error
	if lease != nil {
		replayErr = marshal.ReplayWithEnvironment(ctx, workdir, commands, lease.ManagedEnvironment())
	} else {
		replayErr = marshal.Replay(ctx, workdir, commands)
	}
	if replayErr != nil {
		return fmt.Errorf("verification: %w", replayErr)
	}
	branchSHA, err := gitRevision(workdir, "HEAD")
	if err != nil {
		return fmt.Errorf("verification branch commit: %w", err)
	}
	treeSHA, err := gitRevision(workdir, "HEAD^{tree}")
	if err != nil {
		return fmt.Errorf("verification tree: %w", err)
	}
	receipt := marshal.Verification{
		BaseSHA: is.baseRef, BranchSHA: branchSHA, TreeSHA: treeSHA,
		Passed: true, Commands: commands,
	}
	if lease != nil {
		if err := lease.Seal(); err != nil {
			return fmt.Errorf("seal verification cache: %w", err)
		}
		evidence, err := lease.Evidence()
		if err != nil {
			return fmt.Errorf("read verification cache evidence: %w", err)
		}
		receipt.CacheEvidence = &marshal.CacheEvidence{
			LeaseID: evidence.LeaseID, State: string(evidence.State), Repository: evidence.Repository,
			ManagedScope: evidence.ManagedScope, BaseSHA: evidence.BaseSHA, BranchSHA: evidence.BranchSHA,
			TreeSHA: evidence.TreeSHA, CommandDigest: evidence.CommandDigest,
			SeedLeaseID: evidence.SeedLeaseID,
			Quarantines: append([]verificationcache.QuarantineDisposition(nil), evidence.Quarantines...),
		}
	}
	document, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("encode verification receipt: %w", err)
	}
	return os.WriteFile(filepath.Join(workdir, "verification.json"), document, 0o644)
}
