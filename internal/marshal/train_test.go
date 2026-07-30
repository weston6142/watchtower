package marshal

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

func verificationFor(t *testing.T, repo, branch string, commands [][]string) Verification {
	t.Helper()
	return Verification{
		BaseSHA:   strings.TrimSpace(git(t, repo, "merge-base", "main", branch)),
		BranchSHA: strings.TrimSpace(git(t, repo, "rev-parse", branch)),
		TreeSHA:   strings.TrimSpace(git(t, repo, "rev-parse", branch+"^{tree}")),
		Passed:    true,
		Commands:  commands,
	}
}

func addIndependentBranch(t *testing.T, repo, branch, file string) {
	t.Helper()
	git(t, repo, "checkout", "-qb", branch, "main")
	if err := os.WriteFile(filepath.Join(repo, file), []byte(branch+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", file)
	git(t, repo, "commit", "-qm", branch)
	git(t, repo, "checkout", "-q", "main")
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLandSerializesNonOverlappingBranches(t *testing.T) {
	repo, first := repoWithBranch(t, false)
	addIndependentBranch(t, repo, "issue/GH-2", "second.txt")
	entered := filepath.Join(t.TempDir(), "entered")
	release := filepath.Join(t.TempDir(), "release")
	blocker := filepath.Join(t.TempDir(), "block")
	if err := os.WriteFile(blocker, []byte(
		"#!/bin/sh\nset -eu\ntouch \"$1\"\nwhile [ ! -f \"$2\" ]; do sleep 0.01; done\n"),
		0o755); err != nil {
		t.Fatal(err)
	}
	firstReceipt := verificationFor(t, repo, first, [][]string{{blocker, entered, release}})
	firstReceipt.TreeSHA = "force-replay"
	secondReceipt := verificationFor(t, repo, "issue/GH-2", [][]string{{"true"}})
	tr := &Train{Repo: repo}
	errs := make(chan error, 2)
	var starts sync.WaitGroup
	starts.Add(2)
	go func() {
		starts.Done()
		_, err := tr.LandVerified(context.Background(), "GH-1", first, firstReceipt)
		errs <- err
	}()
	waitForFile(t, entered)
	go func() {
		starts.Done()
		_, err := tr.LandVerified(context.Background(), "GH-2", "issue/GH-2", secondReceipt)
		errs <- err
	}()
	starts.Wait()
	time.Sleep(50 * time.Millisecond)
	if _, err := exec.Command(
		"git", "-C", repo, "merge-base", "--is-ancestor", "issue/GH-2", "main",
	).CombinedOutput(); err == nil {
		t.Fatal("second branch entered integration while first verification was blocked")
	}
	if err := os.WriteFile(release, []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestMovedBaseReplaysVerification(t *testing.T) {
	repo, branch := repoWithBranch(t, false)
	recorded := filepath.Join(t.TempDir(), "verified-tree")
	recorder := filepath.Join(t.TempDir(), "record-tree")
	if err := os.WriteFile(recorder, []byte(
		"#!/bin/sh\nset -eu\ngit rev-parse 'HEAD^{tree}' > \"$1\"\n"),
		0o755); err != nil {
		t.Fatal(err)
	}
	receipt := verificationFor(t, repo, branch, [][]string{{recorder, recorded}})
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("advanced\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "base.txt")
	git(t, repo, "commit", "-qm", "advance base")

	tr := &Train{Repo: repo}
	result, err := tr.LandVerified(context.Background(), "GH-1", branch, receipt)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(recorded)
	if err != nil {
		t.Fatal("moved base did not replay verification")
	}
	if strings.TrimSpace(string(got)) != strings.TrimSpace(git(t, repo, "rev-parse", "HEAD^{tree}")) ||
		result.PreSHA == result.LandedSHA {
		t.Fatalf("result=%+v recorded=%q", result, got)
	}
}

func TestFailedCombinedVerificationRestoresBase(t *testing.T) {
	repo, branch := repoWithBranch(t, false)
	receipt := verificationFor(t, repo, branch, [][]string{{"false"}})
	receipt.TreeSHA = "force-replay"
	pre := strings.TrimSpace(git(t, repo, "rev-parse", "main"))
	tr := &Train{Repo: repo}
	if _, err := tr.LandVerified(context.Background(), "GH-1", branch, receipt); err == nil {
		t.Fatal("combined verification unexpectedly passed")
	}
	if post := strings.TrimSpace(git(t, repo, "rev-parse", "main")); post != pre {
		t.Fatalf("main moved despite failed replay: %s -> %s", pre, post)
	}
	if status := git(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("base checkout dirty after rollback: %q", status)
	}
}

func TestPushFailureCanPublishWithoutRemerge(t *testing.T) {
	repo, branch := repoWithBranch(t, false)
	remote := remoteFor(t, repo)
	receipt := verificationFor(t, repo, branch, [][]string{{"true"}})
	git(t, repo, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
	tr := &Train{Repo: repo, Push: true}
	result, err := tr.LandVerified(context.Background(), "GH-1", branch, receipt)
	var pending *PublishPendingError
	if !errors.As(err, &pending) || result.Published || result.LandedSHA == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	git(t, repo, "remote", "set-url", "origin", remote)
	if err := tr.Publish(result.LandedSHA, result.BaseBranch); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(git(t, repo, "log", "--format=%s", "main"), "Merge branch"); got != 1 {
		t.Fatalf("merge commits = %d, want 1", got)
	}
}
