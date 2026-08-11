package store

import (
	"bytes"
	"testing"
)

func TestLoadLatestPlannerArtifactBeforeSelectsNewestEligibleExactScope(t *testing.T) {
	s, err := Open(t.TempDir() + "/planner-artifact.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const (
		issueID  = "GH-77"
		stage    = "plan"
		worktree = "/worktrees/GH-77"
	)
	insert := func(issue, stage string, attempt int, worktree, status string, manifest, sections []byte) {
		t.Helper()
		if err := s.CreatePlannerArtifact(issue, stage, attempt, worktree, status, []byte("digest"), manifest, sections); err != nil {
			t.Fatal(err)
		}
	}

	manifest1, sections1 := []byte("manifest-attempt-1"), []byte("sections-attempt-1")
	manifest2, sections2 := []byte("manifest-attempt-2"), []byte("sections-attempt-2")
	manifest4, sections4 := []byte("manifest-attempt-4"), []byte("sections-attempt-4")
	insert(issueID, stage, 1, worktree, "active", manifest1, sections1)
	insert(issueID, stage, 2, worktree, "active", manifest2, sections2)
	insert(issueID, stage, 4, worktree, "active", manifest4, sections4)
	insert("GH-78", stage, 3, worktree, "active", []byte("other-issue"), []byte("other-issue"))
	insert(issueID, "execute", 3, worktree, "active", []byte("other-stage"), []byte("other-stage"))
	insert(issueID, stage, 3, "/worktrees/GH-78", "active", []byte("other-worktree"), []byte("other-worktree"))
	insert(issueID, stage, 3, worktree, "expired", []byte("expired"), []byte("expired"))

	assertLoad := func(before, wantAttempt int, wantManifest, wantSections []byte, wantFound bool) {
		t.Helper()
		attempt, status, _, manifest, sections, found, err := s.LoadLatestPlannerArtifactBefore(issueID, stage, before, worktree)
		if err != nil || found != wantFound {
			t.Fatalf("before %d = attempt=%d status=%q found=%v err=%v", before, attempt, status, found, err)
		}
		if !wantFound {
			return
		}
		if attempt != wantAttempt || status != "active" || !bytes.Equal(manifest, wantManifest) || !bytes.Equal(sections, wantSections) {
			t.Fatalf("before %d = attempt=%d status=%q manifest=%q sections=%q, want attempt=%d active %q %q", before, attempt, status, manifest, sections, wantAttempt, wantManifest, wantSections)
		}
	}

	assertLoad(4, 2, manifest2, sections2, true)
	assertLoad(2, 1, manifest1, sections1, true)
	assertLoad(1, 0, nil, nil, false)
}
