package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
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
