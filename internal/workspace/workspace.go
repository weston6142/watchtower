package workspace

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Provider hands out isolated working copies of a repo, one per issue.
// Acquire returns the workspace path and a release func that tears it down.
type Provider interface {
	Acquire(issueID string) (path string, release func() error, err error)
}

// GitWorktree provisions workspaces with `git worktree` under .worktrees/,
// creating a branch named issue/<id> per workspace.
type GitWorktree struct{ Repo string }

func (g GitWorktree) Acquire(issueID string) (string, func() error, error) {
	path := filepath.Join(g.Repo, ".worktrees", issueID)
	branch := "issue/" + issueID
	out, err := exec.Command("git", "-C", g.Repo, "worktree", "add", path, "-b", branch).CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("worktree add: %v: %s", err, out)
	}
	release := func() error {
		out, err := exec.Command("git", "-C", g.Repo, "worktree", "remove", "--force", path).CombinedOutput()
		if err != nil {
			return fmt.Errorf("worktree remove: %v: %s", err, out)
		}
		return nil
	}
	return path, release, nil
}

// Treehouse provisions workspaces via the `treehouse` CLI's lease mechanism.
type Treehouse struct{ Repo string }

func (t Treehouse) Acquire(issueID string) (string, func() error, error) {
	cmd := exec.Command("treehouse", "get", "--lease", "--lease-holder", issueID)
	cmd.Dir = t.Repo
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", nil, fmt.Errorf("treehouse get: %v: %s", err, stderr.String())
	}
	path := strings.TrimSpace(string(out))
	release := func() error {
		cmd := exec.Command("treehouse", "return", path)
		cmd.Dir = t.Repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("treehouse return: %v: %s", err, out)
		}
		return nil
	}
	return path, release, nil
}

// Detect prefers treehouse when its binary is on PATH, falling back to
// plain git worktrees otherwise.
func Detect(repo string) Provider {
	if _, err := exec.LookPath("treehouse"); err == nil {
		return Treehouse{Repo: repo}
	}
	return GitWorktree{Repo: repo}
}
