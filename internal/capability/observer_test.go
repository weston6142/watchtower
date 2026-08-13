package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestObserverDetectsCompleteMutationClasses(t *testing.T) {
	repo := observerRepo(t)
	writeObserverFile(t, filepath.Join(repo, "edit.txt"), "before")
	writeObserverFile(t, filepath.Join(repo, "delete.txt"), "delete")
	writeObserverFile(t, filepath.Join(repo, "rename.txt"), "rename")
	observerGit(t, repo, "add", ".")
	observerGit(t, repo, "commit", "-m", "baseline")

	observer := Observer{}
	baseline, err := observer.Capture(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeObserverFile(t, filepath.Join(repo, "create.txt"), "created")
	writeObserverFile(t, filepath.Join(repo, "edit.txt"), "after")
	if err := os.Remove(filepath.Join(repo, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(repo, "rename.txt"), filepath.Join(repo, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(repo, "edit.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("edit.txt", filepath.Join(repo, "linked.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(repo, "edit.txt"), filepath.Join(repo, "hard.txt")); err != nil {
		t.Fatal(err)
	}
	observerGit(t, repo, "add", "create.txt")

	delta, err := observer.Compare(baseline)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []MutationClass{MutationCreate, MutationModify, MutationDelete, MutationRename, MutationMetadata, MutationLink} {
		if !deltaHasMutation(delta, want) {
			t.Errorf("delta missing %q: %+v", want, delta.Entries)
		}
	}
	if !delta.Git.IndexChanged || delta.Digest == "" {
		t.Fatalf("git/digest delta = %+v", delta)
	}
	for _, entry := range delta.Entries {
		if entry.Content != "" {
			t.Fatalf("delta disclosed content: %+v", entry)
		}
	}
}

func TestObserverRequiresReapedDescendants(t *testing.T) {
	repo := observerRepo(t)
	observerGit(t, repo, "commit", "--allow-empty", "-m", "baseline")
	observer := Observer{DescendantsReaped: func() bool { return false }}
	baseline, err := observer.Capture(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := observer.Compare(baseline); err == nil {
		t.Fatal("comparison succeeded while descendants were live")
	}
}

func TestObserverNoGitWorkspaceKeepsControlIdentityStable(t *testing.T) {
	workspace := t.TempDir()
	writeObserverFile(t, filepath.Join(workspace, "artifact.md"), "baseline")
	observer := Observer{}
	baseline, err := observer.Capture(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := observer.Compare(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if delta.Git.CommonGitChanged {
		t.Fatalf("unchanged non-Git workspace reported Git control mutation: %+v", delta.Git)
	}
}

func TestObserverDetectsRefsOutsideHeadsAndTags(t *testing.T) {
	repo := observerRepo(t)
	observerGit(t, repo, "commit", "--allow-empty", "-m", "baseline")
	observer := Observer{}
	baseline, err := observer.Capture(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	observerGit(t, repo, "update-ref", "refs/watchtower/escape", "HEAD")
	delta, err := observer.Compare(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if !delta.Git.RefsChanged {
		t.Fatalf("hidden ref mutation was not observed: %+v", delta.Git)
	}
}

func TestObserverDetectsDirectoryMetadataMutation(t *testing.T) {
	repo := observerRepo(t)
	writeObserverFile(t, filepath.Join(repo, "locked", "file.txt"), "baseline")
	observerGit(t, repo, "add", ".")
	observerGit(t, repo, "commit", "-m", "baseline")
	observer := Observer{}
	baseline, err := observer.Capture(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(repo, "locked"), 0o700); err != nil {
		t.Fatal(err)
	}
	delta, err := observer.Compare(baseline)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range delta.Entries {
		if entry.Path == "locked" && entry.Mutation == MutationMetadata {
			return
		}
	}
	t.Fatalf("directory metadata mutation was not observed: %+v", delta.Entries)
}

func TestObserverDetectsHooksDirectoryReplacement(t *testing.T) {
	repo := observerRepo(t)
	observerGit(t, repo, "commit", "--allow-empty", "-m", "baseline")
	hooks := filepath.Join(repo, ".git", "hooks")
	if err := os.RemoveAll(hooks); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	observer := Observer{}
	baseline, err := observer.Capture(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(hooks); err != nil {
		t.Fatal(err)
	}
	externalHooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.Mkdir(externalHooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalHooks, hooks); err != nil {
		t.Fatal(err)
	}
	delta, err := observer.Compare(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if !delta.Git.CommonGitChanged {
		t.Fatalf("hooks directory replacement was not observed: %+v", delta.Git)
	}
}

func TestBaselineDigestIncludesGitControlIdentity(t *testing.T) {
	first := Baseline{Workspace: "/workspace", CommonGitDigest: digestText("first")}
	second := first
	second.CommonGitDigest = digestText("second")
	firstDigest, err := baselineDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := baselineDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest == secondDigest {
		t.Fatal("baseline digest did not bind Git control identity")
	}
}

func deltaHasMutation(delta Delta, want MutationClass) bool {
	for _, entry := range delta.Entries {
		if entry.Mutation == want {
			return true
		}
	}
	return false
}

func digestText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
