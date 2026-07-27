package marshal

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Train lands issue branches on the repo's default branch, serially.
type Train struct {
	Repo    string
	TestCmd []string
	Resolve func(ctx context.Context, issueID, branch string) error
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
			return fmt.Errorf("merge conflict: %s", out)
		}
		if len(tr.TestCmd) > 0 {
			cmd := exec.CommandContext(ctx, tr.TestCmd[0], tr.TestCmd[1:]...)
			cmd.Dir = tr.Repo
			if out, err := cmd.CombinedOutput(); err != nil {
				_, _ = tr.git("reset", "--hard", pre)
				return fmt.Errorf("tests failed after merge: %v: %s", err, truncate(string(out), 2000))
			}
		}
		return nil
	}
	err = attempt()
	if err == nil {
		return nil
	}
	if tr.Resolve == nil || !strings.Contains(err.Error(), "merge conflict") {
		return err
	}
	if rerr := tr.Resolve(ctx, issueID, branch); rerr != nil {
		return fmt.Errorf("%v (repair failed: %v)", err, rerr)
	}
	return attempt()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
