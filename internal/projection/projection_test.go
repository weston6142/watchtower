package projection

import (
	"reflect"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/review"
	"github.com/weston6142/watchtower/internal/stageusage"
)

func TestActiveBlockersUsesProjectedStatusAndRestoresOnReversion(t *testing.T) {
	s := NewState()
	merged := "GH-1"
	cleanup := "GH-2"
	preserved := "GH-3"
	unknown := "GH-404"
	child := "GH-5"
	s.Apply(ev(t, core.EvIssueCreated, merged, map[string]any{"title": "merged"}))
	s.Apply(ev(t, core.EvIssueCompleted, merged, nil))
	s.Apply(ev(t, core.EvIssueCreated, cleanup, map[string]any{"title": "cleanup"}))
	s.Apply(ev(t, core.EvCleanupNeeded, cleanup, map[string]any{"error": "cleanup"}))
	s.Apply(ev(t, core.EvIssueCompleted, preserved, map[string]any{"merge": "left-unmerged"}))
	s.Apply(ev(t, core.EvIssueDrafted, child, map[string]any{
		"title": "child", "flow": "default", "preset": "regular", "priority": 1,
		"depends_on": []string{merged, cleanup, preserved, unknown},
	}))

	issue := s.Issues[child]
	if got, want := s.ActiveBlockers(child), []string{preserved, unknown}; !reflect.DeepEqual(got, want) {
		t.Fatalf("active blockers = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(issue.DependsOn, []string{merged, cleanup, preserved, unknown}) {
		t.Fatalf("raw dependencies = %v", issue.DependsOn)
	}

	s.Apply(ev(t, core.EvIssueCompleted, merged, map[string]any{"merge": "left-unmerged"}))
	if got, want := s.ActiveBlockers(child), []string{merged, preserved, unknown}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reverted active blockers = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(issue.DependsOn, []string{merged, cleanup, preserved, unknown}) {
		t.Fatalf("raw dependencies changed after status reversion = %v", issue.DependsOn)
	}
}

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

func TestPlannerSnapshotProjectionUsesImmutableMetadata(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-39", map[string]any{"title": "bounded"}))
	snapshot := stageusage.Snapshot{Stage: "plan", Attempt: 1, CallsUsed: 3, ChargedTokens: 99,
		Status: stageusage.StatusBudgetLimited, LastSource: "spec.md"}
	s.Apply(ev(t, core.EvPlannerBudgetUpdated, "GH-39", map[string]any{
		"stage": "plan", "outcome": "budget_limited", "snapshot": snapshot,
	}))
	view := s.Issues["GH-39"]
	if view == nil || view.Planner == nil || view.Planner.ChargedTokens != 99 || view.PlannerOutcome != "budget_limited" {
		t.Fatalf("planner projection = %+v", view)
	}
	snapshot.CallsUsed = 100
	if view.Planner.CallsUsed == 100 {
		t.Fatal("projection retained mutable snapshot data")
	}
}

func TestStageStartedMergeBarrierShowsVerifyingForAnyStageName(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "x", "flow": "custom"}))
	s.Apply(ev(t, core.EvStageStarted, "GH-1", map[string]any{
		"stage": "ship-it", "merge_barrier": true,
	}))
	if got := s.Issues["GH-1"].State; got != "verifying" {
		t.Fatalf("state = %q", got)
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

func TestProjectionDecisionContext(t *testing.T) {
	s := NewState()
	want := decision.DecisionContext{
		TaskSummary: "Ship decision context.", AgentName: "Executor",
		AgentColor: "green", AgentSymbol: "⚙",
	}
	s.Apply(ev(t, core.EvDecisionRequired, "GH-31", map[string]any{
		"decision_id": float64(7), "stage": "execute", "question": "Proceed?", "context": want,
	}))
	got := s.Decisions[7].Context
	if got == nil || *got != want {
		t.Fatalf("projected context = %#v", got)
	}
}

func TestProjectionLegacyDecisionContext(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvDecisionRequired, "GH-31", map[string]any{
		"decision_id": float64(8), "stage": "execute", "question": "Legacy?",
	}))
	if got := s.Decisions[8].Context; got != nil {
		t.Fatalf("legacy projected context = %#v", got)
	}
}

func TestProjectionDecisionEscalationEvidence(t *testing.T) {
	importance := 0.2
	hash := strings.Repeat("a", 64)
	evaluation := review.Evaluation{
		Outcome: review.OutcomeRequiresApproval, RequiredFloor: review.FloorPolicy, EffectiveFloor: review.FloorOperator,
		PolicyID: "team-safety", PolicyVersion: "7",
		Evidence:     []review.Evidence{{Signal: "path", Value: "payments/charge.go", Rule: "path:payments/**", Floor: review.FloorOperator}},
		Item:         review.ItemBinding{Kind: review.ItemArtifact, Hash: hash, Path: "payments/charge.go", Operation: "approve-artifact"},
		Dependencies: []review.DependencyBinding{{Kind: "decision", ID: "GH-63", Hash: hash}},
		Model:        review.ModelMetadata{Importance: &importance, Options: []string{"approve", "revise"}, Rationale: "model metadata"},
	}
	binding := review.Binding{Item: evaluation.Item, RequiredFloor: evaluation.RequiredFloor, EffectiveFloor: evaluation.EffectiveFloor,
		PolicyID: evaluation.PolicyID, PolicyVersion: evaluation.PolicyVersion, Evidence: evaluation.Evidence,
		Dependencies: evaluation.Dependencies, Model: evaluation.Model}
	s := NewState()
	s.Apply(ev(t, core.EvDecisionRequired, "GH-64", map[string]any{
		"decision_id": float64(64), "stage": "execute", "question": "Approve?",
		"evaluation": evaluation, "bindings": []review.Binding{binding},
	}))
	got := s.Decisions[64]
	if got.Evaluation == nil || got.Evaluation.Outcome != evaluation.Outcome ||
		got.Evaluation.PolicyID != evaluation.PolicyID || got.Evaluation.Item != evaluation.Item ||
		len(got.Evaluation.Evidence) != 1 || len(got.Bindings) != 1 ||
		got.Bindings[0].Dependencies[0] != binding.Dependencies[0] || got.Bindings[0].Model.Rationale != "model metadata" {
		t.Fatalf("projected escalation evidence = %#v", got)
	}
}

func TestProjectionDecisionReviewTarget(t *testing.T) {
	s := NewState()
	want := review.Target{
		IssueID: "GH-26", Stage: "plan", CheckpointID: 17,
		Artifacts: []contextpack.Artifact{
			{Name: "plan.md", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			{Name: "touchset.json", SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		}, ArtifactVersion: "17|plan.md=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa|touchset.json=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		NextStage: "execute",
	}
	s.Apply(ev(t, core.EvDecisionRequired, "GH-26", map[string]any{
		"decision_id": float64(26), "stage": "plan", "question": "Approve plan artifacts?", "review": want,
	}))
	got := s.Decisions[26].Review
	if got == nil || got.IssueID != want.IssueID || got.Stage != want.Stage ||
		got.CheckpointID != want.CheckpointID || got.ArtifactVersion != want.ArtifactVersion ||
		got.NextStage != want.NextStage || len(got.Artifacts) != len(want.Artifacts) {
		t.Fatalf("projected review = %#v", got)
	}
	if !s.Decisions[26].RequiresOption {
		t.Fatal("legacy artifact-review event permits freeform feedback")
	}
	for i, artifact := range want.Artifacts {
		if got.Artifacts[i] != artifact {
			t.Fatalf("projected artifact %d = %#v, want %#v", i, got.Artifacts[i], artifact)
		}
	}
}

func TestPlanReviewProjectionTracksHumanAndPolicyOutcomes(t *testing.T) {
	policy := review.ResolvedPolicy{
		Mode: "regular", HumanRequired: true, PolicyID: "manual-default", PolicyVersion: "1", Reason: "manual_default",
	}
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-35", map[string]any{"title": "review"}))
	s.Apply(ev(t, core.EvPlanReviewRequested, "GH-35", map[string]any{
		"decision_id": float64(11), "stage": "plan", "mode": policy.Mode,
		"human_required": policy.HumanRequired, "policy_id": policy.PolicyID,
		"policy_version": policy.PolicyVersion, "reason": policy.Reason,
	}))
	s.Apply(ev(t, core.EvDecisionRequired, "GH-35", map[string]any{
		"decision_id": float64(11), "stage": "plan", "question": "Approve?",
		"options": []any{"approve", "reject"}, "recommended": float64(0),
		"review_policy": policy,
	}))
	if got := s.Decisions[11].ReviewPolicy; got == nil || *got != policy || s.Decisions[11].ReviewStatus != "pending" {
		t.Fatalf("projected review policy = %#v, want %#v", got, policy)
	}
	s.Apply(ev(t, core.EvPlanReviewHumanApproved, "GH-35", map[string]any{
		"decision_id": float64(11), "stage": "plan", "mode": policy.Mode,
		"human_required": true, "policy_id": policy.PolicyID, "policy_version": policy.PolicyVersion,
		"reason": policy.Reason, "approval_kind": "human", "actor_id": "alice",
	}))
	if len(s.Decisions) != 0 || s.Issues["GH-35"].ReviewStatus != "approved" ||
		s.Issues["GH-35"].Approval == nil || s.Issues["GH-35"].Approval.ActorID != "alice" {
		t.Fatalf("human outcome = issue=%#v decisions=%#v", s.Issues["GH-35"], s.Decisions)
	}

	policy = review.ResolvedPolicy{
		Mode: "regular", PolicyAutoApproval: true, PolicyID: "team-ci", PolicyVersion: "2026-08-03", Reason: "policy_opt_in",
	}
	s.Apply(ev(t, core.EvPlanReviewRequested, "GH-35", map[string]any{
		"decision_id": float64(12), "stage": "plan", "mode": policy.Mode,
		"human_required": false, "policy_auto_approval": true, "policy_id": policy.PolicyID,
		"policy_version": policy.PolicyVersion, "reason": policy.Reason,
	}))
	s.Apply(ev(t, core.EvPlanReviewPolicyApproved, "GH-35", map[string]any{
		"decision_id": float64(12), "stage": "plan", "mode": policy.Mode,
		"human_required": false, "policy_auto_approval": true, "policy_id": policy.PolicyID,
		"policy_version": policy.PolicyVersion, "reason": policy.Reason,
		"approval_kind": "policy",
	}))
	iv := s.Issues["GH-35"]
	if iv.ReviewPolicy.PolicyID != "team-ci" || iv.ReviewStatus != "approved_automatically" ||
		iv.Approval == nil || iv.Approval.Kind != review.ApprovalPolicy || iv.Approval.PolicyID != "team-ci" ||
		len(s.Decisions) != 0 {
		t.Fatalf("policy outcome = issue=%#v decisions=%#v", iv, s.Decisions)
	}
	s.Apply(ev(t, core.EvExecutionStarted, "GH-35", map[string]any{"stage": "execute"}))
	if iv.CurrentStage != "execute" || iv.State != "running" {
		t.Fatalf("execution outcome = issue=%#v", iv)
	}

	policy = review.ResolvedPolicy{
		Mode: "regular", HumanRequired: true, PolicyID: "manual-default", PolicyVersion: "1", Reason: "manual_default",
	}
	s.Apply(ev(t, core.EvPlanReviewRequested, "GH-35", map[string]any{
		"decision_id": float64(13), "stage": "plan", "mode": policy.Mode,
		"human_required": true, "policy_id": policy.PolicyID, "policy_version": policy.PolicyVersion,
		"reason": policy.Reason,
	}))
	s.Apply(ev(t, core.EvDecisionRequired, "GH-35", map[string]any{
		"decision_id": float64(13), "stage": "plan", "question": "Approve?",
		"options": []any{"approve", "reject"}, "recommended": float64(0), "review_policy": policy,
	}))
	s.Apply(ev(t, core.EvPlanReviewRejected, "GH-35", map[string]any{
		"decision_id": float64(13), "stage": "plan", "mode": policy.Mode,
		"human_required": true, "policy_id": policy.PolicyID, "policy_version": policy.PolicyVersion,
		"reason": policy.Reason, "approval_kind": "human", "actor_id": "alice",
	}))
	s.Apply(ev(t, core.EvDecisionAnswered, "GH-35", map[string]any{"decision_id": float64(13)}))
	if iv.ReviewStatus != "rejected" || iv.State != "review_rejected" || len(s.Decisions) != 0 {
		t.Fatalf("rejection outcome = issue=%#v decisions=%#v", iv, s.Decisions)
	}
}

func TestProjectionLegacyDecisionReviewIsNil(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvDecisionRequired, "GH-26", map[string]any{
		"decision_id": float64(27), "stage": "spec", "question": "Legacy?",
	}))
	if got := s.Decisions[27].Review; got != nil {
		t.Fatalf("legacy projected review = %#v", got)
	}
}

func TestReplayLegacyDecisionEventHasNoFabricatedContext(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-31", map[string]any{"title": "legacy issue"}))
	s.Apply(ev(t, core.EvDecisionRequired, "GH-31", map[string]any{
		"decision_id": float64(9), "stage": "spec", "question": "Legacy?",
	}))
	got := s.Decisions[9]
	if got.Context != nil || got.Question != "Legacy?" || got.IssueID != "GH-31" {
		t.Fatalf("replayed legacy decision = %#v", got)
	}
}

func TestDecisionProjectionPreservesChoiceCompatibilityField(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvDecisionRequired, "GH-1", map[string]any{
		"decision_id": float64(3), "stage": "spec", "kind": "choice",
		"question": "Legacy false?", "options": []any{"yes", "no"},
		"recommended": float64(0), "allow_freeform": false,
	}))
	s.Apply(ev(t, core.EvDecisionRequired, "GH-1", map[string]any{
		"decision_id": float64(4), "stage": "spec",
		"kind": "choice", "question": "Legacy omitted?", "options": []any{"yes", "no"},
		"recommended": float64(0),
	}))
	for id, question := range map[int64]string{3: "Legacy false?", 4: "Legacy omitted?"} {
		got, ok := s.Decisions[id]
		if !ok || got.Kind != "choice" || got.Question != question || len(got.Options) != 2 || got.AllowFreeform {
			t.Fatalf("decision %d = %#v", id, got)
		}
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
	if len(s.Parked) != 0 {
		t.Fatalf("resumed lane remained parked: %v", s.Parked)
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
