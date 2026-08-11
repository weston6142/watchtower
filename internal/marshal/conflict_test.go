package marshal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConflictControllerOwnsExactRebaseOperations(t *testing.T) {
	repo, branch := repoWithBranch(t, true)
	worktree := filepath.Join(t.TempDir(), "issue")
	git(t, repo, "worktree", "add", "-q", worktree, branch)
	controller, err := NewConflictController(worktree)
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSpace(git(t, repo, "rev-parse", "main"))
	state, err := controller.Start(base)
	if err != nil {
		t.Fatal(err)
	}
	if state.Done || len(state.Paths) != 1 || state.Paths[0] != "f.txt" {
		t.Fatalf("initial conflict state = %+v", state)
	}
	if err := os.WriteFile(filepath.Join(worktree, "f.txt"), []byte("resolved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state, err = controller.Continue([]string{"f.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !state.Done {
		t.Fatalf("completed state = %+v", state)
	}
	if _, err := conflictGit(worktree, "merge-base", "--is-ancestor", base, "HEAD"); err != nil {
		t.Fatalf("rebased branch is not based on %s: %v", base, err)
	}
}

func TestConflictControllerRejectsMismatchedPathsAndAborts(t *testing.T) {
	repo, branch := repoWithBranch(t, true)
	worktree := filepath.Join(t.TempDir(), "issue")
	git(t, repo, "worktree", "add", "-q", worktree, branch)
	controller, err := NewConflictController(worktree)
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSpace(git(t, repo, "rev-parse", "main"))
	if _, err := controller.Start(base); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Continue([]string{"other.txt"}); err == nil {
		t.Fatal("controller accepted a path outside the current conflict set")
	}
	if err := controller.Abort(); err != nil {
		t.Fatal(err)
	}
	if active, err := rebaseActive(worktree); err != nil || active {
		t.Fatalf("rebase remains active after abort: active=%v err=%v", active, err)
	}
}
