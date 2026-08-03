package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/weston6142/watchtower/internal/marshal"
)

// writeVerificationReceipt runs the configured verification command in the
// issue worktree and records the receipt itself, so merge verification never
// depends on an agent-authored verification.json. Without a configured
// test_cmd the agent's receipt (or the fake runner's) remains authoritative.
func (e *Engine) writeVerificationReceipt(
	ctx context.Context, is *issueState, workdir string,
) error {
	if e.cfg.Train == nil || len(e.cfg.Train.TestCmd) == 0 {
		return nil
	}
	commands := [][]string{e.cfg.Train.TestCmd}
	if err := marshal.Replay(ctx, workdir, commands); err != nil {
		return fmt.Errorf("verification: %w", err)
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
	document, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("encode verification receipt: %w", err)
	}
	return os.WriteFile(filepath.Join(workdir, "verification.json"), document, 0o644)
}
