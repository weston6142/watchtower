package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"},
		{"commit", "--allow-empty", "-q", "-m", "root"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func TestGitWorktreeAcquireRelease(t *testing.T) {
	repo := initRepo(t)
	p := GitWorktree{Repo: repo}
	path, release, err := p.Acquire("GH-9")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != filepath.Join(repo, ".worktrees") {
		t.Fatalf("unexpected path %s", path)
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		t.Fatal("not a worktree")
	}
	// second acquire of same issue must fail (branch exists)
	if _, _, err := p.Acquire("GH-9"); err == nil {
		t.Fatal("expected duplicate acquire to fail")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("worktree not removed")
	}
}

func TestTreehouseAcquireChecksOutBranch(t *testing.T) {
	repo := initRepo(t)
	wt := filepath.Join(t.TempDir(), "leased")
	head, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", repo, "worktree", "add", "--detach", wt, strings.TrimSpace(string(head))).CombinedOutput()
	if err != nil {
		t.Fatalf("worktree add: %v %s", err, out)
	}

	binDir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = get ]; then echo " + wt + "; fi\n"
	if err := os.WriteFile(filepath.Join(binDir, "treehouse"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := Treehouse{Repo: repo}
	path, _, err := p.Acquire("GH-7")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := exec.Command("git", "-C", path, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(branch)); got != "issue/GH-7" {
		t.Fatalf("worktree on %q, want issue/GH-7", got)
	}
}

// A pre-warmed lane can be stale: the pool synced it before the repo's
// default branch moved. The issue branch must start from the current tip.
func TestTreehouseAcquireStartsFromDefaultTip(t *testing.T) {
	repo := initRepo(t)
	wt := filepath.Join(t.TempDir(), "leased")
	head, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", repo, "worktree", "add", "--detach", wt, strings.TrimSpace(string(head))).CombinedOutput()
	if err != nil {
		t.Fatalf("worktree add: %v %s", err, out)
	}
	// the default branch moves on after the lane was warmed
	if out, err := exec.Command("git", "-C", repo, "commit", "--allow-empty", "-q", "-m", "newer").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v %s", err, out)
	}

	binDir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = get ]; then echo " + wt + "; fi\n"
	if err := os.WriteFile(filepath.Join(binDir, "treehouse"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	path, _, err := Treehouse{Repo: repo}.Acquire("GH-8")
	if err != nil {
		t.Fatal(err)
	}
	tip, _ := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	got, _ := exec.Command("git", "-C", path, "rev-parse", "HEAD").Output()
	if string(got) != string(tip) {
		t.Fatalf("issue branch at %s, want default tip %s", got, tip)
	}
}
