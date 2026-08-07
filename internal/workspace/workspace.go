package workspace

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Provider hands out isolated working copies of a repo, one per issue.
// Acquire returns the workspace path and a release func that tears it down.
type Provider interface {
	Acquire(issueID string) (path string, release func() error, err error)
	// Name is the provider's operator-facing name, reported by the setup
	// inspector: workspace choice is invisible otherwise. On the interface
	// rather than a type switch in main.go so the compiler catches a future
	// provider that forgets to name itself.
	Name() string
}

// Releaser makes a workspace cleanup operation replayable after a daemon
// restart. Engine integration state persists the exact path to pass back.
type Releaser interface {
	ReleasePath(path string) error
}

// IssueDiscarder removes durable branch state for an explicitly abandoned
// issue before that issue is requeued as a fresh run.
type IssueDiscarder interface {
	DiscardIssue(issueID string) error
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
		return g.ReleasePath(path)
	}
	return path, release, nil
}

func (g GitWorktree) Name() string { return "git worktree" }

func (g GitWorktree) DiscardIssue(issueID string) error {
	return discardIssueBranch(g.Repo, issueID)
}

func (g GitWorktree) ReleasePath(path string) error {
	out, err := exec.Command("git", "-C", g.Repo, "worktree", "remove", "--force", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("worktree remove: %v: %s", err, out)
	}
	return nil
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
	// Treehouse leases detached-HEAD worktrees; the merge train needs a real
	// branch. Preserve an existing issue branch because it is durable task
	// state; only new branches start from the current default tip rather than a
	// potentially stale pre-warmed lease.
	branch := "issue/" + issueID
	branchRef := "refs/heads/" + branch
	exists := exec.Command("git", "-C", t.Repo, "show-ref", "--verify", "--quiet", branchRef)
	checkout := []string{"-C", path, "checkout", "-q"}
	if err := exists.Run(); err == nil {
		checkout = append(checkout, branch)
	} else if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		checkout = append(checkout, "-B", branch)
		if def, err := exec.Command("git", "-C", t.Repo, "symbolic-ref", "--short", "HEAD").Output(); err == nil {
			checkout = append(checkout, strings.TrimSpace(string(def)))
		}
	} else {
		detectErr := fmt.Errorf("detect issue branch: %w", err)
		if returnErr := t.ReleasePath(path); returnErr != nil {
			return "", nil, fmt.Errorf("%v (return failed lease: %w)", detectErr, returnErr)
		}
		return "", nil, detectErr
	}
	if out, err := exec.Command("git", checkout...).CombinedOutput(); err != nil {
		checkoutErr := fmt.Errorf("checkout issue branch: %v: %s", err, out)
		if returnErr := t.ReleasePath(path); returnErr != nil {
			return "", nil, fmt.Errorf("%v (return failed lease: %w)", checkoutErr, returnErr)
		}
		return "", nil, checkoutErr
	}
	release := func() error {
		return t.ReleasePath(path)
	}
	return path, release, nil
}

func (t Treehouse) Name() string { return "treehouse" }

func (t Treehouse) DiscardIssue(issueID string) error {
	return discardIssueBranch(t.Repo, issueID)
}

func (t Treehouse) ReleasePath(path string) error {
	cmd := exec.Command("treehouse", "return", "--force", path)
	cmd.Dir = t.Repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("treehouse return: %v: %s", err, out)
	}
	return nil
}

func discardIssueBranch(repo, issueID string) error {
	branch := "issue/" + issueID
	ref := "refs/heads/" + branch
	deadline := time.Now().Add(5 * time.Second)
	var lastOutput []byte
	var lastErr error
	for {
		check := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", ref)
		if err := check.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				return nil
			}
			return fmt.Errorf("detect abandoned issue branch: %w", err)
		}
		lastOutput, lastErr = exec.Command("git", "-C", repo, "branch", "-D", branch).CombinedOutput()
		if lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("delete abandoned issue branch: %v: %s", lastErr, lastOutput)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// Detect prefers treehouse when its binary is on PATH, falling back to
// plain git worktrees otherwise.
func Detect(repo string) Provider {
	if _, err := exec.LookPath("treehouse"); err == nil {
		return Treehouse{Repo: repo}
	}
	return GitWorktree{Repo: repo}
}
