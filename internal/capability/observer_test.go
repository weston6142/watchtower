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
