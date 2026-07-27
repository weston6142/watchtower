package marshal

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return string(out)
}

func repoWithBranch(t *testing.T, conflicting bool) (string, string) {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "config", "user.email", "t@t")
	git(t, repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	git(t, repo, "checkout", "-qb", "issue/GH-1")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("branch change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "-aqm", "branch work")
	git(t, repo, "checkout", "-q", "main")
	if conflicting {
		if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("main change\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "commit", "-aqm", "main work")
	}
	return repo, "issue/GH-1"
}

func TestLandCleanMerge(t *testing.T) {
	repo, branch := repoWithBranch(t, false)
	tr := &Train{Repo: repo}
	if err := tr.Land(context.Background(), "GH-1", branch); err != nil {
		t.Fatal(err)
	}
	log := git(t, repo, "log", "--oneline", "main")
	if !strings.Contains(log, "branch work") {
		t.Fatalf("merge missing: %s", log)
	}
}

func TestLandConflictWithoutResolverFails(t *testing.T) {
	repo, branch := repoWithBranch(t, true)
	tr := &Train{Repo: repo}
	if err := tr.Land(context.Background(), "GH-1", branch); err == nil {
		t.Fatal("expected conflict error")
	}
	if s := git(t, repo, "status", "--porcelain"); s != "" {
		t.Fatalf("dirty repo after abort: %q", s)
	}
}

func TestLandTestFailureRollsBack(t *testing.T) {
	repo, branch := repoWithBranch(t, false)
	tr := &Train{Repo: repo, TestCmd: []string{"false"}}
	pre := git(t, repo, "rev-parse", "main")
	if err := tr.Land(context.Background(), "GH-1", branch); err == nil {
		t.Fatal("expected test failure")
	}
	if post := git(t, repo, "rev-parse", "main"); post != pre {
		t.Fatalf("main moved despite failing tests: %s -> %s", pre, post)
	}
}
