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
// depends on an agent-authored verification.json.
func (e *Engine) writeVerificationReceipt(
	ctx context.Context, is *issueState, workdir string, lease *verificationcache.Lease,
) error {
	if e.cfg.Train == nil || len(e.cfg.Train.TestCmd) == 0 {
		return fmt.Errorf("engine verification requires test_cmd")
	}
	commands := [][]string{append([]string(nil), e.cfg.Train.TestCmd...)}
	if lease != nil {
		branchSHA, treeSHA, err := verificationIdentity(workdir)
		if err != nil {
			return rejectVerificationIdentity(lease, fmt.Errorf("verification pre-gate identity: %w", err), "verification pre-gate identity read failed")
		}
		if branchSHA != lease.BranchSHA() || treeSHA != lease.TreeSHA() {
			return rejectVerificationIdentity(lease, fmt.Errorf(
				"verification pre-gate identity %s/%s does not match lease %s/%s",
				branchSHA, treeSHA, lease.BranchSHA(), lease.TreeSHA()), "verification pre-gate identity mismatch")
		}
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
	branchSHA, treeSHA, err := verificationIdentity(workdir)
	if err != nil {
		return rejectVerificationIdentity(lease, fmt.Errorf("verification post-gate identity: %w", err), "verification post-gate identity read failed")
	}
	if lease != nil && (branchSHA != lease.BranchSHA() || treeSHA != lease.TreeSHA()) {
		return rejectVerificationIdentity(lease, fmt.Errorf(
			"verification post-gate identity %s/%s does not match lease %s/%s",
			branchSHA, treeSHA, lease.BranchSHA(), lease.TreeSHA()), "verification post-gate identity mismatch")
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
		receipt.CacheEvidence = marshal.NewCacheEvidence(evidence)
	}
	document, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("encode verification receipt: %w", err)
	}
	return writeFileAtomic(workdir, "verification.json", document)
}

func writeFileAtomic(dir, name string, document []byte) error {
	temporary, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", name, err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(document); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, filepath.Join(dir, name))
}

func verificationIdentity(workdir string) (branchSHA, treeSHA string, err error) {
	branchSHA, err = gitRevision(workdir, "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("verification branch commit: %w", err)
	}
	treeSHA, err = gitRevision(workdir, "HEAD^{tree}")
	if err != nil {
		return "", "", fmt.Errorf("verification tree: %w", err)
	}
	return branchSHA, treeSHA, nil
}

func rejectVerificationIdentity(lease *verificationcache.Lease, cause error, reason string) error {
	if lease == nil {
		return cause
	}
	if err := lease.Quarantine(reason); err != nil {
		return fmt.Errorf("%v (quarantine verification lease: %w)", cause, err)
	}
	return cause
}

func verificationReceiptBytes(workdir string) ([]byte, error) {
	return os.ReadFile(filepath.Join(workdir, "verification.json"))
}
