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

func TestLandRejectsDetachedHead(t *testing.T) {
	repo, _ := repoWithBranch(t, false)
	tr := &Train{Repo: repo}
	for _, branch := range []string{"HEAD", ""} {
		if err := tr.Land(context.Background(), "GH-1", branch); err == nil {
			t.Fatalf("Land(%q) succeeded; want detached-head error", branch)
		}
	}
	log := git(t, repo, "log", "--oneline", "main")
	if strings.Contains(log, "branch work") {
		t.Fatalf("main moved on rejected branch: %s", log)
	}
}

func TestLandVerifiesBranchIsAncestor(t *testing.T) {
	repo, branch := repoWithBranch(t, false)
	tr := &Train{Repo: repo}
	if err := tr.Land(context.Background(), "GH-1", branch); err != nil {
		t.Fatal(err)
	}
	// the branch tip must now be reachable from main
	if _, err := exec.Command("git", "-C", repo, "merge-base", "--is-ancestor", branch, "main").CombinedOutput(); err != nil {
		t.Fatalf("branch not ancestor of main after Land: %v", err)
	}
}

func TestDeleteBranchRemovesMergedBranch(t *testing.T) {
	repo, branch := repoWithBranch(t, false)
	tr := &Train{Repo: repo}
	if err := tr.Land(context.Background(), "GH-1", branch); err != nil {
		t.Fatal(err)
	}
	if err := tr.DeleteBranch(branch); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", repo, "branch", "--list", branch).CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("branch still exists after DeleteBranch: %s", out)
	}
}

func TestDeleteBranchRefusesUnmergedBranch(t *testing.T) {
	repo, branch := repoWithBranch(t, false)
	tr := &Train{Repo: repo}
	if err := tr.DeleteBranch(branch); err == nil {
		t.Fatal("expected error deleting unmerged branch")
	}
	if out := git(t, repo, "branch", "--list", branch); strings.TrimSpace(out) == "" {
		t.Fatal("unmerged branch was deleted")
	}
}

// remoteFor turns repo into a clone of a fresh bare remote named origin.
func remoteFor(t *testing.T, repo string) string {
	t.Helper()
	remote := t.TempDir()
	git(t, remote, "init", "-q", "--bare")
	git(t, repo, "remote", "add", "origin", remote)
	git(t, repo, "push", "-q", "origin", "main")
	return remote
}

func TestSyncBaseFastForwardsFromOrigin(t *testing.T) {
	repo, _ := repoWithBranch(t, false)
	remote := remoteFor(t, repo)
	// advance the remote past the local main
	ahead := t.TempDir()
	git(t, ahead, "clone", "-q", remote, ".")
	git(t, ahead, "config", "user.email", "t@t")
	git(t, ahead, "config", "user.name", "t")
	git(t, ahead, "commit", "-q", "--allow-empty", "-m", "remote work")
	git(t, ahead, "push", "-q", "origin", "main")

	tr := &Train{Repo: repo, Pull: true}
	if err := tr.SyncBase(); err != nil {
		t.Fatal(err)
	}
	if log := git(t, repo, "log", "--oneline", "main"); !strings.Contains(log, "remote work") {
		t.Fatalf("main not fast-forwarded: %s", log)
	}
}

func TestSyncBaseWithoutRemoteIsNoop(t *testing.T) {
	repo, _ := repoWithBranch(t, false)
	tr := &Train{Repo: repo, Pull: true}
	if err := tr.SyncBase(); err != nil {
		t.Fatal(err)
	}
}

func TestSyncBaseDisabledIsNoop(t *testing.T) {
	repo, _ := repoWithBranch(t, false)
	remoteFor(t, repo)
	tr := &Train{Repo: repo}
	if err := tr.SyncBase(); err != nil {
		t.Fatal(err)
	}
}

func TestLandPushesWhenEnabled(t *testing.T) {
	repo, branch := repoWithBranch(t, false)
	remote := remoteFor(t, repo)
	tr := &Train{Repo: repo, Push: true}
	if err := tr.Land(context.Background(), "GH-1", branch); err != nil {
		t.Fatal(err)
	}
	local := strings.TrimSpace(git(t, repo, "rev-parse", "main"))
	pushed := strings.TrimSpace(git(t, remote, "rev-parse", "main"))
	if local != pushed {
		t.Fatalf("remote main %s != local main %s after Land", pushed, local)
	}
}

func TestLandDoesNotPushByDefault(t *testing.T) {
	repo, branch := repoWithBranch(t, false)
	remote := remoteFor(t, repo)
	pre := strings.TrimSpace(git(t, remote, "rev-parse", "main"))
	tr := &Train{Repo: repo}
	if err := tr.Land(context.Background(), "GH-1", branch); err != nil {
		t.Fatal(err)
	}
	if post := strings.TrimSpace(git(t, remote, "rev-parse", "main")); post != pre {
		t.Fatalf("remote main moved without push enabled: %s -> %s", pre, post)
	}
}
