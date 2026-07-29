package marshal

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
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
	if branch == "" || branch == "HEAD" {
		return fmt.Errorf("no branch to merge: worktree is detached (HEAD); commits were not landed")
	}
	def, err := tr.defaultBranch()
	if err != nil {
		return err
	}
	pre, err := tr.git("rev-parse", def)
	if err != nil {
		return fmt.Errorf("pre ref: %v", err)
	}
	attempt := func() error {
		if out, err := tr.git("merge", "--no-ff", "--no-edit", branch); err != nil {
			_, _ = tr.git("merge", "--abort")
			return fmt.Errorf("%w: %s", errMergeConflict, out)
		}
		if _, err := tr.git("merge-base", "--is-ancestor", branch, def); err != nil {
			_, _ = tr.git("reset", "--hard", pre)
			return fmt.Errorf("merge did not land: %s is not reachable from %s after merge", branch, def)
		}
		if len(tr.TestCmd) > 0 {
			cmd := exec.CommandContext(ctx, tr.TestCmd[0], tr.TestCmd[1:]...)
			cmd.Dir = tr.Repo
			if out, err := cmd.CombinedOutput(); err != nil {
				_, _ = tr.git("reset", "--hard", pre)
				return fmt.Errorf("tests failed after merge: %v: %s", err, truncate(string(out), maxTestOutputBytes))
			}
		}
		return nil
	}
	err = attempt()
	if err == nil {
		return tr.push(def)
	}
	if tr.Resolve == nil || !errors.Is(err, errMergeConflict) {
		return err
	}
	if rerr := tr.Resolve(ctx, issueID, branch); rerr != nil {
		return fmt.Errorf("%v (repair failed: %v)", err, rerr)
	}
	if err := attempt(); err != nil {
		return err
	}
	return tr.push(def)
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
