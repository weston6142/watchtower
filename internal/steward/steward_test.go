package steward

import (
	"testing"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func findRow(t *testing.T, s *store.Store, id string) store.IssueRow {
	t.Helper()
	rows, err := s.Issues()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("issue %s not found", id)
	return store.IssueRow{}
}

func ev(t *testing.T, typ core.EventType, issue string, payload any) core.Event {
	t.Helper()
	e, err := core.NewEvent(typ, issue, payload)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestStewardProjectsIssueTable(t *testing.T) {
	s, _ := store.Open("file:st1?mode=memory&cache=shared")
	defer s.Close()
	st := &Steward{Store: s}
	st.Observe(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "hello", "flow": "default", "body": "b", "priority": float64(2)}))
	st.Observe(ev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "spec"}))
	rows, _ := s.Issues()
	if len(rows) != 1 || rows[0].State != "running:spec" || rows[0].Title != "hello" || rows[0].Priority != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	st.Observe(ev(t, core.EvStageFailed, "GH-1", map[string]any{"stage": "spec"}))
	rows, _ = s.Issues()
	if rows[0].State != "failed" {
		t.Fatalf("state: %s", rows[0].State)
	}
}

func TestStewardProjectsPauseAndResumeLifecycle(t *testing.T) {
	s := newTestStore(t)
	st := &Steward{Store: s}
	st.Observe(ev(t, core.EvIssueCreated, "GH-36", map[string]any{
		"title": "paused", "flow": "default",
	}))
	st.Observe(ev(t, core.EvIssuePaused, "GH-36", map[string]string{"stage": "plan"}))
	if row := findRow(t, s, "GH-36"); row.State != "paused" {
		t.Fatalf("paused state = %q, want paused", row.State)
	}
	st.Observe(ev(t, core.EvIssueResumed, "GH-36", nil))
	if row := findRow(t, s, "GH-36"); row.State != "running" {
		t.Fatalf("resumed state = %q, want running", row.State)
	}
}

func TestStewardMarksAbandoned(t *testing.T) {
	s, _ := store.Open("file:st_abandon?mode=memory&cache=shared")
	defer s.Close()
	st := &Steward{Store: s}
	st.Observe(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "doomed", "flow": "default"}))
	st.Observe(ev(t, core.EvStageFailed, "GH-1", map[string]any{"stage": "spec"}))
	st.Observe(ev(t, core.EvIssueAbandoned, "GH-1", map[string]any{}))
	rows, _ := s.Issues()
	if len(rows) != 1 || rows[0].State != "abandoned" {
		t.Fatalf("rows: %+v", rows)
	}
}

func TestStewardProjectsFinalizationLifecycle(t *testing.T) {
	s := newTestStore(t)
	st := &Steward{Store: s}
	st.Observe(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "x", "flow": "default"}))
	for _, step := range []struct {
		typ  core.EventType
		want string
	}{
		{core.EvStageStarted, "verifying"},
		{core.EvVerificationReady, "waiting:integration"},
		{core.EvMergeStarted, "integrating"},
		{core.EvFinalizationFailed, "failed:finalize"},
	} {
		payload := map[string]string{}
		if step.typ == core.EvStageStarted {
			payload["stage"] = "merge-verification"
		}
		st.Observe(ev(t, step.typ, "GH-1", payload))
		if row := findRow(t, s, "GH-1"); row.State != step.want {
			t.Fatalf("%s state = %q, want %q", step.typ, row.State, step.want)
		}
	}
}

func TestMergeBarrierMetadataPersistsVerifyingForRenamedStage(t *testing.T) {
	s := newTestStore(t)
	st := &Steward{Store: s}
	st.Observe(ev(t, core.EvIssueCreated, "GH-1", map[string]any{
		"title": "x", "flow": "custom",
	}))
	st.Observe(ev(t, core.EvStageStarted, "GH-1", map[string]any{
		"stage": "ship-it", "merge_barrier": true,
	}))
	rows, err := s.Issues()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].State != "verifying" {
		t.Fatalf("issues = %+v", rows)
	}
}

func TestStewardPreservesCleanupWarningUntilCompleted(t *testing.T) {
	s := newTestStore(t)
	st := &Steward{Store: s}
	st.Observe(ev(t, core.EvIssueCreated, "GH-1", map[string]any{
		"title": "cleanup", "flow": "default",
	}))
	st.Observe(ev(t, core.EvIssueMerged, "GH-1", nil))
	st.Observe(ev(t, core.EvCleanupNeeded, "GH-1", nil))
	st.Observe(ev(t, core.EvIssueCompleted, "GH-1", nil))
	if row := findRow(t, s, "GH-1"); row.State != "cleanup_needed" {
		t.Fatalf("completion hid cleanup warning: %+v", row)
	}
	st.Observe(ev(t, core.EvCleanupCompleted, "GH-1", nil))
	if row := findRow(t, s, "GH-1"); row.State != "done" {
		t.Fatalf("cleanup completion state: %+v", row)
	}
}

func TestObserveDraftAndUpdate(t *testing.T) {
	s := newTestStore(t)
	sw := &Steward{Store: s}
	evDraft := ev(t, core.EvIssueDrafted, "GH-1", map[string]any{
		"title": "t", "body": "b", "flow": "default", "preset": "regular",
		"priority": 2, "levers": map[string]string{"impl": "yolo"}})
	sw.Observe(evDraft)
	row := findRow(t, s, "GH-1")
	if row.State != "backlog" || row.Title != "t" || row.Priority != 2 || row.Levers["impl"] != "yolo" {
		t.Fatalf("drafted row wrong: %+v", row)
	}
	evUpdate := ev(t, core.EvIssueUpdated, "GH-1", map[string]any{
		"title": "t2", "body": "b2", "flow": "default", "preset": "strict",
		"priority": 7, "levers": map[string]string{"impl": "strict"}})
	sw.Observe(evUpdate)
	row = findRow(t, s, "GH-1")
	if row.State != "backlog" || row.Title != "t2" || row.Priority != 7 || row.Levers["impl"] != "strict" {
		t.Fatalf("updated row wrong: %+v", row)
	}
}

func TestStewardProjectsClaimAndRelease(t *testing.T) {
	s := newTestStore(t)
	st := &Steward{Store: s}
	st.Observe(ev(t, core.EvIssueDrafted, "GH-41", map[string]any{
		"title": "explore", "body": "details", "flow": "default", "priority": 3,
	}))
	st.Observe(ev(t, core.EvIssueClaimed, "GH-41", map[string]any{
		"worktree": "/tmp/GH-41", "branch": "issue/GH-41", "base_sha": "abc",
	}))
	if row := findRow(t, s, "GH-41"); row.State != "claimed" || row.Title != "explore" {
		t.Fatalf("claimed row = %+v", row)
	}
	st.Observe(ev(t, core.EvIssueReleased, "GH-41", nil))
	if row := findRow(t, s, "GH-41"); row.State != "backlog" || row.Body != "details" {
		t.Fatalf("released row = %+v", row)
	}
}
