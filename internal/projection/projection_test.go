package projection

import (
	"testing"

	"github.com/weston6142/watchtower/internal/core"
)

func ev(t *testing.T, typ core.EventType, issue string, payload any) core.Event {
	t.Helper()
	e, err := core.NewEvent(typ, issue, payload)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestReplayBuildsIssueView(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "hello"}))
	s.Apply(ev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "spec"}))
	s.Apply(ev(t, core.EvDecisionRequired, "GH-1", map[string]any{
		"decision_id": float64(1), "stage": "spec", "question": "Approve spec artifacts?",
		"options": []any{"approve", "reject"}, "recommended": float64(0)}))

	iv := s.Issues["GH-1"]
	if iv == nil || iv.Title != "hello" || iv.State != "waiting_decision" || iv.CurrentStage != "spec" {
		t.Fatalf("bad view: %+v", iv)
	}
	if len(s.Decisions) != 1 || s.Decisions[1].Question != "Approve spec artifacts?" {
		t.Fatalf("bad decisions: %+v", s.Decisions)
	}

	s.Apply(ev(t, core.EvDecisionAnswered, "GH-1", map[string]any{"decision_id": float64(1), "option": float64(0)}))
	s.Apply(ev(t, core.EvStageCompleted, "GH-1", map[string]any{"stage": "spec"}))
	if len(s.Decisions) != 0 || iv.State != "running" || len(iv.Completed) != 1 {
		t.Fatalf("after answer: %+v decisions=%v", iv, s.Decisions)
	}
}

func TestDecisionProjectionPreservesFreeformFields(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvDecisionRequired, "GH-1", map[string]any{
		"decision_id": float64(2), "stage": "spec",
		"kind": "freeform", "question": "Review spec.md",
		"recommended_response": "Approve spec.md as written.",
		"allow_freeform":       true,
	}))
	got := s.Decisions[2]
	if got.Kind != "freeform" || got.RecommendedResponse != "Approve spec.md as written." ||
		!got.AllowFreeform {
		t.Fatalf("decision = %#v", got)
	}
}

func TestIssueCompletedSetsDone(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-2", map[string]any{"title": "x"}))
	s.Apply(ev(t, core.EvStageCompleted, "GH-2", map[string]any{"stage": "review"}))
	if s.Issues["GH-2"].State == "done" {
		t.Fatal("stage name must no longer imply done")
	}
	s.Apply(ev(t, core.EvIssueCompleted, "GH-2", nil))
	if s.Issues["GH-2"].State != "done" {
		t.Fatalf("want done, got %q", s.Issues["GH-2"].State)
	}
}

func TestClaimAndReleasePreserveDraftMetadata(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueDrafted, "GH-41", map[string]any{
		"title": "explore", "body": "details", "flow": "default",
		"preset": "regular", "priority": 3,
		"attachments": []string{"trace.log"}, "depends_on": []string{"GH-40"},
	}))
	s.Apply(ev(t, core.EvIssueClaimed, "GH-41", map[string]any{
		"worktree": "/tmp/GH-41", "branch": "issue/GH-41", "base_sha": "abc",
	}))
	issue := s.Issues["GH-41"]
	if issue.State != "claimed" || issue.Title != "explore" || issue.Body != "details" ||
		issue.Priority != 3 || len(issue.Attachments) != 1 || issue.Attachments[0] != "trace.log" ||
		len(issue.DependsOn) != 1 || issue.DependsOn[0] != "GH-40" {
		t.Fatalf("claimed issue = %+v", issue)
	}
	if len(s.Backlog) != 0 || len(s.Order) != 1 || s.Order[0] != "GH-41" {
		t.Fatalf("claimed membership: backlog=%v order=%v", s.Backlog, s.Order)
	}

	s.Apply(ev(t, core.EvIssueReleased, "GH-41", nil))
	if issue.State != "backlog" || len(s.Order) != 0 ||
		len(s.Backlog) != 1 || s.Backlog[0] != "GH-41" {
		t.Fatalf("released issue=%+v backlog=%v order=%v", issue, s.Backlog, s.Order)
	}
}

func TestCleanupWarningSurvivesIssueCompletion(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "x"}))
	s.Apply(ev(t, core.EvIssueMerged, "GH-1", nil))
	s.Apply(ev(t, core.EvCleanupNeeded, "GH-1", map[string]any{
		"operations": []string{"delete_branch:issue/GH-1"}, "error": "branch busy",
	}))
	s.Apply(ev(t, core.EvIssueCompleted, "GH-1", nil))
	issue := s.Issues["GH-1"]
	if issue.State != "cleanup_needed" || !issue.Merged ||
		issue.LastError != "branch busy" || len(issue.Cleanup) != 1 {
		t.Fatalf("cleanup warning = %+v", issue)
	}
	s.Apply(ev(t, core.EvCleanupCompleted, "GH-1", nil))
	if issue.State != "done" || issue.LastError != "" || len(issue.Cleanup) != 0 {
		t.Fatalf("completed cleanup = %+v", issue)
	}
}

func TestAttemptAndErrorSurfacing(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "x", "flow": "default"}))
	s.Apply(ev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "execute", "attempt": float64(2), "of": float64(3)}))
	iv := s.Issues["GH-1"]
	if iv.Attempt != 2 || iv.AttemptOf != 3 {
		t.Fatalf("attempts: %+v", iv)
	}
	s.Apply(ev(t, core.EvStageFailed, "GH-1", map[string]any{"stage": "execute", "error": "2 tests failing", "attempt": float64(3), "of": float64(3), "final": true}))
	if iv.LastError != "2 tests failing" || iv.State != "failed" {
		t.Fatalf("error: %+v", iv)
	}
	s.Apply(ev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "execute", "attempt": float64(1), "of": float64(3)}))
	if iv.LastError != "" {
		t.Fatalf("new attempt did not clear error: %+v", iv)
	}
}

func TestFinalizationLifecycleStatesAreDistinct(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "x"}))
	s.Apply(ev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "merge-verification"}))
	issue := s.Issues["GH-1"]
	if issue.State != "verifying" {
		t.Fatalf("verification state = %q", issue.State)
	}
	s.Apply(ev(t, core.EvVerificationReady, "GH-1", nil))
	if issue.State != "waiting:integration" {
		t.Fatalf("ready state = %q", issue.State)
	}
	s.Apply(ev(t, core.EvMergeStarted, "GH-1", nil))
	if issue.State != "integrating" {
		t.Fatalf("merge state = %q", issue.State)
	}
	s.Apply(ev(t, core.EvFinalizationFailed, "GH-1", map[string]string{"error": "dirty base"}))
	if issue.State != "failed:finalize" || issue.LastError != "dirty base" {
		t.Fatalf("failure state = %+v", issue)
	}
}

func TestMergeSequencingProjection(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "a", "flow": "default"}))
	s.Apply(ev(t, core.EvIssueCreated, "GH-2", map[string]any{"title": "b", "flow": "default"}))
	s.Apply(ev(t, core.EvMergeSequenced, "GH-2", map[string]any{"behind": "GH-1"}))
	if s.Issues["GH-2"].Behind != "GH-1" {
		t.Fatalf("behind: %+v", s.Issues["GH-2"])
	}
	if len(s.Order) != 2 || s.Order[0] != "GH-1" {
		t.Fatalf("order: %v", s.Order)
	}
	s.Apply(ev(t, core.EvIssueMerged, "GH-1", nil))
	if !s.Issues["GH-1"].Merged || s.Issues["GH-2"].Behind != "" {
		t.Fatalf("release: %+v %+v", s.Issues["GH-1"], s.Issues["GH-2"])
	}
	s.Apply(ev(t, core.EvProposalFiled, "GH-2", map[string]any{"title": "x"}))
	if s.ProposalCount != 1 {
		t.Fatalf("proposals: %d", s.ProposalCount)
	}
}

func TestProjectionV2Fields(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "x", "flow": "default"}))
	s.Apply(ev(t, core.EvIssuePaused, "GH-1", nil))
	if !s.Issues["GH-1"].Paused {
		t.Fatal("paused not tracked")
	}
	s.Apply(ev(t, core.EvIssueResumed, "GH-1", nil))
	s.Apply(ev(t, core.EvArtifactProduced, "GH-1", map[string]any{
		"stage": "execute", "artifact": "evidence.json", "path": "/x",
		"area_weight": map[string]any{"payments": float64(300), "api": float64(40)}}))
	if s.Issues["GH-1"].AreaWeights["payments"] != 300 {
		t.Fatalf("weights: %v", s.Issues["GH-1"].AreaWeights)
	}
	s.Apply(ev(t, core.EvDecisionRequired, "GH-1", map[string]any{
		"decision_id": float64(1), "stage": "spec", "question": "Q?",
		"options": []any{"a", "b"}, "recommended": float64(0),
		"why": "a is safe", "consequences": []any{"c1", "c2"}, "reversible": "anytime"}))
	d := s.Decisions[1]
	if d.Why != "a is safe" || len(d.Consequences) != 2 || d.Reversible != "anytime" {
		t.Fatalf("decision v2: %+v", d)
	}
	s.Apply(ev(t, core.EvProposalFiled, "GH-1", map[string]any{"title": "new idea"}))
	if len(s.Notices) != 1 || s.Notices[0].Text != "✉ new idea from GH-1: new idea" {
		t.Fatalf("notices: %v", s.Notices)
	}
}

func TestProjectionTracksShippedAndParked(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "x"}))
	s.Apply(ev(t, core.EvIssueMerged, "GH-1", nil))
	if len(s.Shipped) != 1 || s.Shipped[0] != "GH-1" || s.Issues["GH-1"].MergedAt.IsZero() {
		t.Fatalf("shipped: %+v", s)
	}
	s.Apply(ev(t, core.EvIssueCreated, "GH-2", map[string]any{"title": "y"}))
	s.Apply(ev(t, core.EvStageFailed, "GH-2", map[string]any{"stage": "execute", "final": true}))
	if len(s.Parked) != 1 || s.Parked[0] != "GH-2" {
		t.Fatalf("parked failure: %v", s.Parked)
	}
	s.Apply(ev(t, core.EvIssueCreated, "GH-3", map[string]any{"title": "z"}))
	s.Apply(ev(t, core.EvStageKilled, "GH-3", map[string]any{"stage": "execute"}))
	if !s.Issues["GH-3"].Killed || s.Issues["GH-3"].State != "paused" {
		t.Fatalf("killed: %+v", s.Issues["GH-3"])
	}
	if len(s.Parked) != 2 || s.Parked[1] != "GH-3" {
		t.Fatalf("parked kill: %v", s.Parked)
	}
}

func TestAbandonRemovesLaneEverywhere(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "doomed", "flow": "default"}))
	s.Apply(ev(t, core.EvIssueCreated, "GH-2", map[string]any{"title": "keeper", "flow": "default"}))
	s.Apply(ev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "spec"}))
	s.Apply(ev(t, core.EvDecisionRequired, "GH-1", map[string]any{
		"decision_id": float64(3), "stage": "spec", "question": "q",
		"options": []any{"a"}, "recommended": float64(0)}))
	s.Apply(ev(t, core.EvStageFailed, "GH-1", map[string]any{"stage": "spec", "error": "boom", "final": true}))
	s.Apply(ev(t, core.EvIssueAbandoned, "GH-1", map[string]any{}))

	if s.Issues["GH-1"] != nil {
		t.Fatal("issue survived abandon")
	}
	if len(s.Order) != 1 || s.Order[0] != "GH-2" {
		t.Fatalf("order not cleaned: %v", s.Order)
	}
	if len(s.Parked) != 0 || len(s.Shipped) != 0 {
		t.Fatalf("shelf not cleaned: parked=%v shipped=%v", s.Parked, s.Shipped)
	}
	if len(s.Decisions) != 0 {
		t.Fatalf("decisions survived abandon: %v", s.Decisions)
	}
}

// A resumed lane has recovered from its kill; leaving Killed set keeps the
// TUI's retry affordance armed for a stage that is running again.
func TestResumeClearsKilled(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t"}))
	s.Apply(ev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "execute"}))
	s.Apply(ev(t, core.EvStageKilled, "GH-1", map[string]any{"stage": "execute"}))
	if !s.Issues["GH-1"].Killed {
		t.Fatal("expected Killed after stage_killed")
	}

	s.Apply(ev(t, core.EvIssueResumed, "GH-1", nil))
	iv := s.Issues["GH-1"]
	if iv.Killed {
		t.Fatal("resume left Killed set")
	}
	if iv.Paused || iv.State != "running" {
		t.Fatalf("resume state: paused=%v state=%q", iv.Paused, iv.State)
	}
}

// The paused marker has to land on the stage the lane will resume into, not
// the one that just finished — otherwise it collides with that stage's tick.
func TestPausedEventAdvancesCurrentStage(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t"}))
	s.Apply(ev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}))
	s.Apply(ev(t, core.EvStageCompleted, "GH-1", map[string]any{"stage": "brainstorm"}))
	s.Apply(ev(t, core.EvIssuePaused, "GH-1", map[string]any{"stage": "spec"}))
	if got := s.Issues["GH-1"].CurrentStage; got != "spec" {
		t.Fatalf("CurrentStage = %q, want spec", got)
	}
}

// Events already in the store carry no payload; they must not blank the stage.
func TestPausedEventWithoutStageLeavesCurrentStage(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t"}))
	s.Apply(ev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}))
	s.Apply(ev(t, core.EvIssuePaused, "GH-1", nil))
	iv := s.Issues["GH-1"]
	if iv.CurrentStage != "brainstorm" {
		t.Fatalf("CurrentStage = %q, want brainstorm", iv.CurrentStage)
	}
	if !iv.Paused || iv.State != "paused" {
		t.Fatalf("paused=%v state=%q", iv.Paused, iv.State)
	}
}

func TestBacklogLifecycle(t *testing.T) {
	s := NewState()
	drafted := ev(t, core.EvIssueDrafted, "GH-1", map[string]any{
		"title": "t", "body": "b", "flow": "default", "preset": "regular", "priority": 2})
	s.Apply(drafted)
	iv := s.Issues["GH-1"]
	if iv == nil || iv.State != "backlog" || iv.Priority != 2 || iv.Body != "b" || iv.Preset != "regular" {
		t.Fatalf("draft view wrong: %+v", iv)
	}
	if len(s.Order) != 0 {
		t.Fatal("draft leaked into grid Order")
	}
	if len(s.Backlog) != 1 || s.Backlog[0] != "GH-1" {
		t.Fatalf("Backlog = %v", s.Backlog)
	}

	updated := ev(t, core.EvIssueUpdated, "GH-1", map[string]any{
		"title": "t2", "body": "b2", "flow": "default", "preset": "strict", "priority": 9})
	s.Apply(updated)
	iv = s.Issues["GH-1"]
	if iv.Title != "t2" || iv.Priority != 9 || iv.Preset != "strict" || iv.Body != "b2" {
		t.Fatalf("update not applied: %+v", iv)
	}

	created := ev(t, core.EvIssueCreated, "GH-1", map[string]any{
		"title": "t2", "flow": "default", "body": "b2", "priority": 9})
	s.Apply(created)
	if len(s.Backlog) != 0 {
		t.Fatal("launched draft still in Backlog")
	}
	if len(s.Order) != 1 || s.Issues["GH-1"].State != "running" {
		t.Fatal("launched draft not on the grid")
	}
}

func TestAbandonRemovesDraftFromBacklog(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueDrafted, "GH-1", map[string]any{"title": "t"}))
	s.Apply(ev(t, core.EvIssueAbandoned, "GH-1", nil))
	if len(s.Backlog) != 0 || s.Issues["GH-1"] != nil {
		t.Fatal("abandoned draft still visible")
	}
}

// The edit modal prefills from IssueView, so the drafted/updated events are
// what make retain-by-name work.
func TestDraftedAndUpdatedCarryAttachments(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueDrafted, "GH-1", map[string]any{
		"title": "t", "body": "b", "flow": "default", "preset": "regular",
		"attachments": []string{"app.log", "shot.png"}}))
	iv := s.Issues["GH-1"]
	if iv == nil || len(iv.Attachments) != 2 ||
		iv.Attachments[0] != "app.log" || iv.Attachments[1] != "shot.png" {
		t.Fatalf("drafted attachments = %+v", iv)
	}
	s.Apply(ev(t, core.EvIssueUpdated, "GH-1", map[string]any{
		"title": "t", "attachments": []string{"app.log"}}))
	if got := s.Issues["GH-1"].Attachments; len(got) != 1 || got[0] != "app.log" {
		t.Fatalf("updated attachments = %v", got)
	}
	// Dropping every attachment must clear the field, not keep a stale list.
	s.Apply(ev(t, core.EvIssueUpdated, "GH-1", map[string]any{"title": "t"}))
	if got := s.Issues["GH-1"].Attachments; len(got) != 0 {
		t.Fatalf("stale attachments = %v", got)
	}
}
