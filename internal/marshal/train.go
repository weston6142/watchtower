package marshal

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// errMergeConflict tags failures that a conflict resolver may repair;
// its message is part of the error strings surfaced to humans.
var errMergeConflict = errors.New("merge conflict")

// maxTestOutputBytes caps failing test output embedded in a Land error.
const maxTestOutputBytes = 2000

// Train lands issue branches on the repo's default branch, serially.
type Train struct {
	Repo    string
	TestCmd []string
	Resolve func(ctx context.Context, issueID, branch string) error
	// Pull enables SyncBase fast-forwarding the default branch from origin
	// before an issue starts; Push publishes the default branch after a land.
	Pull bool
	Push bool
	mu   sync.Mutex
}

type LandResult struct {
	BaseBranch string
	PreSHA     string
	LandedSHA  string
	Published  bool
}

type PublishPendingError struct {
	Branch string
	Commit string
	Err    error
}

func (e *PublishPendingError) Error() string {
	return fmt.Sprintf("publish %s at %s pending: %v", e.Branch, e.Commit, e.Err)
}

func (e *PublishPendingError) Unwrap() error {
	return e.Err
}

// hasOrigin reports whether the repo has an origin remote to sync against.
func (tr *Train) hasOrigin() bool {
	_, err := tr.git("remote", "get-url", "origin")
	return err == nil
}

// SyncBase fast-forwards the default branch from origin so issues start from
// the latest shared code. Fast-forward only: local commits ahead of origin or
// a diverged branch return an error and leave the repo untouched.
func (tr *Train) SyncBase() error {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if !tr.Pull || !tr.hasOrigin() {
		return nil
	}
	def, err := tr.defaultBranch()
	if err != nil {
		return err
	}
	if out, err := tr.git("fetch", "-q", "origin", def); err != nil {
		return fmt.Errorf("fetch origin %s: %v: %s", def, err, out)
	}
	if out, err := tr.git("merge", "--ff-only", "origin/"+def); err != nil {
		return fmt.Errorf("fast-forward %s from origin: %v: %s", def, err, out)
	}
	return nil
}

func (tr *Train) git(args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", tr.Repo}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (tr *Train) defaultBranch() (string, error) {
	out, err := tr.git("symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("default branch: %v: %s", err, out)
	}
	return out, nil
}

func (tr *Train) Land(ctx context.Context, issueID, branch string) error {
	var commands [][]string
	if len(tr.TestCmd) > 0 {
		commands = [][]string{tr.TestCmd}
	}
	verification := Verification{Commands: commands}
	_, err := tr.LandVerified(ctx, issueID, branch, verification)
	return err
}

// LandVerified serializes integration for this repository across merge,
// verification, and publication. A failed merge or verification restores the
// exact pre-merge commit; a failed push intentionally preserves the verified
// local merge so Publish can retry without merging again.
func (tr *Train) LandVerified(
	ctx context.Context, issueID, branch string, verification Verification,
) (LandResult, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var result LandResult
	if branch == "" || branch == "HEAD" {
		return result, fmt.Errorf("no branch to merge: worktree is detached (HEAD); commits were not landed")
	}
	def, err := tr.defaultBranch()
	if err != nil {
		return result, err
	}
	result.BaseBranch = def
	if status, err := tr.git("status", "--porcelain", "--untracked-files=no"); err != nil {
		return result, fmt.Errorf("base status: %v: %s", err, status)
	} else if status != "" {
		return result, fmt.Errorf("base checkout is dirty before merge: %s", status)
	}
	pre, err := tr.git("rev-parse", def)
	if err != nil {
		return result, fmt.Errorf("pre ref: %v", err)
	}
	result.PreSHA = pre
	attempt := func() error {
		if out, err := tr.git("merge", "--no-ff", "--no-edit", branch); err != nil {
			return tr.rollback(pre, fmt.Errorf("%w: %s", errMergeConflict, out))
		}
		if _, err := tr.git("merge-base", "--is-ancestor", branch, def); err != nil {
			return tr.rollback(pre, fmt.Errorf(
				"merge did not land: %s is not reachable from %s after merge", branch, def))
		}
		landed, err := tr.git("rev-parse", def)
		if err != nil {
			return tr.rollback(pre, fmt.Errorf("landed ref: %v", err))
		}
		tree, err := tr.git("rev-parse", def+"^{tree}")
		if err != nil {
			return tr.rollback(pre, fmt.Errorf("integrated tree: %v", err))
		}
		commands := verification.Commands
		if len(commands) == 0 && len(tr.TestCmd) > 0 {
			commands = [][]string{tr.TestCmd}
		}
		if len(commands) > 0 && !verification.AppliesTo(tree) {
			if err := Replay(ctx, tr.Repo, commands); err != nil {
				return tr.rollback(pre, fmt.Errorf("combined verification failed: %w", err))
			}
		}
		result.LandedSHA = landed
		return nil
	}
	err = attempt()
	if err != nil && tr.Resolve != nil && errors.Is(err, errMergeConflict) {
		if rerr := tr.Resolve(ctx, issueID, branch); rerr != nil {
			return result, fmt.Errorf("%v (repair failed: %v)", err, rerr)
		}
		err = attempt()
	}
	if err != nil {
		return result, err
	}
	if !tr.Push {
		result.Published = true
		return result, nil
	}
	if err := tr.push(def); err != nil {
		return result, &PublishPendingError{Branch: def, Commit: result.LandedSHA, Err: err}
	}
	result.Published = true
	return result, nil
}

func (tr *Train) rollback(pre string, cause error) error {
	_, _ = tr.git("merge", "--abort")
	if out, err := tr.git("reset", "--hard", pre); err != nil {
		return fmt.Errorf("%v (rollback to %s failed: %v: %s)", cause, pre, err, out)
	}
	if status, err := tr.git("status", "--porcelain", "--untracked-files=no"); err != nil {
		return fmt.Errorf("%v (verify rollback failed: %v: %s)", cause, err, status)
	} else if status != "" {
		return fmt.Errorf("%v (base dirty after rollback: %s)", cause, status)
	}
	return cause
}

// Publish retries only publication of an already-landed commit.
func (tr *Train) Publish(commit, branch string) error {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	currentBranch, err := tr.defaultBranch()
	if err != nil {
		return err
	}
	if currentBranch != branch {
		return fmt.Errorf("publish branch changed: current %s, pending %s", currentBranch, branch)
	}
	head, err := tr.git("rev-parse", branch)
	if err != nil {
		return fmt.Errorf("publish ref: %v", err)
	}
	if head != commit {
		return fmt.Errorf("publish commit changed: current %s, pending %s", head, commit)
	}
	return tr.push(branch)
}

// push publishes the default branch after a land. The merge is already on
// disk, so a failed push surfaces as a Land error for the operator to retry
// without rolling anything back.
func (tr *Train) push(def string) error {
	if !tr.Push {
		return nil
	}
	if out, err := tr.git("push", "-q", "origin", def); err != nil {
		return fmt.Errorf("push %s to origin: %v: %s", def, err, out)
	}
	return nil
}

// DeleteBranch removes a landed issue branch. It uses git's safe delete, so
// a branch whose commits have not been merged is refused rather than lost.
func (tr *Train) DeleteBranch(branch string) error {
	if branch == "" || branch == "HEAD" {
		return nil
	}
	if out, err := tr.git("branch", "-d", branch); err != nil {
		return fmt.Errorf("delete branch %s: %v: %s", branch, err, out)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
