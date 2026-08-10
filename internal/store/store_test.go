package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/decisionpage"
	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/review"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/stageusage"
	_ "modernc.org/sqlite"
)

func TestPlannerArtifactRegistryRoundTrip(t *testing.T) {
	database := t.TempDir() + "/planner-artifact.db"
	s, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	worktree := t.TempDir()
	digest := []byte("digest")
	manifest := []byte(`{"sections":[{"key":"goal","globs":["internal/goal/**"]}]}`)
	sections := []byte(`[{"key":"goal","markdown":"first","globs":["internal/goal/**"]}]`)
	if err := s.CreatePlannerArtifact("GH-62", "plan", 1, worktree, "active", digest, manifest, sections); err != nil {
		t.Fatal(err)
	}
	status, gotDigest, gotManifest, gotSections, found, err := s.LoadPlannerArtifact("GH-62", "plan", 1, worktree)
	if err != nil || !found {
		t.Fatalf("load = status=%q found=%v err=%v", status, found, err)
	}
	if status != "active" || string(gotDigest) != string(digest) || string(gotManifest) != string(manifest) || string(gotSections) != string(sections) {
		t.Fatalf("round trip = %q %q %q %q", status, gotDigest, gotManifest, gotSections)
	}
	for _, mismatch := range []struct {
		issue    string
		stage    string
		attempt  int
		worktree string
	}{
		{issue: "GH-61", stage: "plan", attempt: 1, worktree: worktree},
		{issue: "GH-62", stage: "execute", attempt: 1, worktree: worktree},
		{issue: "GH-62", stage: "plan", attempt: 2, worktree: worktree},
		{issue: "GH-62", stage: "plan", attempt: 1, worktree: t.TempDir()},
	} {
		_, _, _, _, found, err := s.LoadPlannerArtifact(mismatch.issue, mismatch.stage, mismatch.attempt, mismatch.worktree)
		if err != nil || found {
			t.Fatalf("mismatch unexpectedly loaded: %+v found=%v err=%v", mismatch, found, err)
		}
	}
}

func TestFailureHistoryRetainsDuplicateFingerprintsInRecordOrder(t *testing.T) {
	s, err := Open("file:failure-history?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	input := failure.RecordInput{
		IssueID: "GH-63", Stage: "plan", StageAttempt: 2,
		FailureSite: failure.SitePlanner, FailureClass: failure.ClassUnavailable,
		RetryDisposition:    failure.RetryAfterStateChange,
		RequiredStateChange: failure.StatePlannerInput,
		Fingerprint:         "sha256:" + strings.Repeat("a", 64),
	}
	first, err := s.AppendFailure(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AppendFailure(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if first.RecordID <= 0 || second.RecordID <= first.RecordID {
		t.Fatalf("record IDs = %d, %d", first.RecordID, second.RecordID)
	}
	records, err := s.FailureHistory(context.Background(), "GH-63")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Fingerprint != records[1].Fingerprint ||
		records[0].RecordID != first.RecordID || records[1].RecordID != second.RecordID {
		t.Fatalf("failure history = %#v", records)
	}
}

func TestFailureHistoryIsEmptyAndInjectable(t *testing.T) {
	s, err := Open("file:failure-history-empty?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	records, err := s.FailureHistory(context.Background(), "GH-empty")
	if err != nil || records == nil || len(records) != 0 {
		t.Fatalf("empty failure history = %#v, err=%v", records, err)
	}
	s.FailNextFailureHistoryForTest()
	if _, err := s.FailureHistory(context.Background(), "GH-empty"); err == nil {
		t.Fatal("injected failure-history read unexpectedly succeeded")
	}
}

func TestRunnerAttemptsPersistSafeLifecycleAcrossReopen(t *testing.T) {
	database := t.TempDir() + "/runner-attempts.db"
	s, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	base := runner.Attempt{
		OperationID: "operation-1", IssueID: "GH-38", Stage: "execute", AgentPackage: "executor",
		Kind: runner.AttemptPrimary, RedactedArgv: []string{"exec", "--json", "features.unified_exec=false", "[redacted]"},
	}
	for _, update := range []runner.Attempt{
		{State: runner.AttemptRunning}, {State: runner.AttemptFailed, FailureClass: runner.FailureLaunch},
		{Kind: runner.AttemptFallback, State: runner.AttemptReserved, FailureClass: runner.FailureLaunch},
		{Kind: runner.AttemptFallback, State: runner.AttemptRunning, FailureClass: runner.FailureLaunch},
		{Kind: runner.AttemptFallback, State: runner.AttemptSucceeded, FailureClass: runner.FailureLaunch, SessionID: "session-1", Tokens: 42},
	} {
		attempt := base
		attempt.Kind = update.Kind
		if attempt.Kind == "" {
			attempt.Kind = runner.AttemptPrimary
		}
		attempt.State, attempt.FailureClass = update.State, update.FailureClass
		attempt.SessionID, attempt.Tokens = update.SessionID, update.Tokens
		if err := s.RecordAttempt(context.Background(), attempt); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	attempts, err := s.LoadOperation(context.Background(), "operation-1")
	if err != nil || len(attempts) != 2 {
		t.Fatalf("attempts = %+v, err = %v", attempts, err)
	}
	if attempts[0].Kind != runner.AttemptPrimary || attempts[0].State != runner.AttemptFailed ||
		attempts[1].Kind != runner.AttemptFallback || attempts[1].State != runner.AttemptSucceeded || attempts[1].Tokens != 42 {
		t.Fatalf("latest attempt lifecycle = %+v", attempts)
	}
	encoded, _ := json.Marshal(attempts)
	for _, secret := range []string{"prompt secret", "developer instruction", "thread-secret", "environment-secret", "stderr-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("attempt metadata leaked %q: %s", secret, encoded)
		}
	}
	if !strings.Contains(string(encoded), "features.unified_exec=false") {
		t.Fatalf("safe feature metadata missing: %s", encoded)
	}
	unused, err := s.LoadOperation(context.Background(), "unused-operation")
	if err != nil || len(unused) != 0 {
		t.Fatalf("unused operation = %+v, err = %v", unused, err)
	}
}

func pausedRunTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open("file:pause-run-" + strings.ReplaceAll(t.Name(), "/", "-") + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func pausedRunIssue(t *testing.T, s *Store, id string) IssueRow {
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
	return IssueRow{}
}

func TestPausedRunStateRoundTripsAndResumes(t *testing.T) {
	s := pausedRunTestStore(t)
	if err := s.UpsertIssue(IssueRow{
		ID: "GH-36", Title: "paused", State: "running:execute", Flow: "default",
	}); err != nil {
		t.Fatal(err)
	}
	want := RunState{
		IssueID: "GH-36", Lifecycle: "paused", Stage: "execute", StageIndex: 3,
		Boundary: "before_stage", Worktree: "/work/GH-36", Branch: "issue/GH-36",
		BaseRef: "base-sha", Artifacts: []string{"brainstorm.md", "spec.md", "plan.md"},
	}
	if err := s.PersistPausedRun(want); err != nil {
		t.Fatal(err)
	}
	row := pausedRunIssue(t, s, "GH-36")
	if row.State != "paused" {
		t.Fatalf("issue state = %q, want paused", row.State)
	}
	got, ok, err := s.LoadRunState("GH-36")
	if err != nil || !ok {
		t.Fatalf("load paused run = %+v, ok=%v, err=%v", got, ok, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paused run = %+v, want %+v", got, want)
	}
	if err := s.ResumeRun(want); err != nil {
		t.Fatal(err)
	}
	if row := pausedRunIssue(t, s, "GH-36"); row.State != "running" {
		t.Fatalf("resumed issue state = %q, want running", row.State)
	}
	got, ok, err = s.LoadRunState("GH-36")
	if err != nil || !ok || got.Lifecycle != "active" || got.Stage != want.Stage || got.Worktree != want.Worktree {
		t.Fatalf("resumed run = %+v, ok=%v, err=%v", got, ok, err)
	}
}

func TestPausePersistenceFailureLeavesPriorStateAuthoritative(t *testing.T) {
	s := pausedRunTestStore(t)
	if err := s.UpsertIssue(IssueRow{ID: "GH-36", State: "running", Flow: "default"}); err != nil {
		t.Fatal(err)
	}
	s.FailNextPausePersistenceForTest()
	err := s.PersistPausedRun(RunState{IssueID: "GH-36", Lifecycle: "paused", Stage: "plan", Boundary: "before_stage"})
	if err == nil {
		t.Fatal("pause persistence unexpectedly succeeded")
	}
	if row := pausedRunIssue(t, s, "GH-36"); row.State != "running" {
		t.Fatalf("failed pause changed issue state to %q", row.State)
	}
	if _, ok, loadErr := s.LoadRunState("GH-36"); loadErr != nil || ok {
		t.Fatalf("failed pause left a run record: ok=%v err=%v", ok, loadErr)
	}
}

func TestActiveSnapshotDoesNotOverwritePausedRun(t *testing.T) {
	s := pausedRunTestStore(t)
	if err := s.UpsertIssue(IssueRow{ID: "GH-36", State: "running", Flow: "default"}); err != nil {
		t.Fatal(err)
	}
	paused := RunState{
		IssueID: "GH-36", Lifecycle: "paused", Stage: "plan", StageIndex: 0,
		Boundary: "before_stage", Artifacts: []string{"plan.md"},
	}
	if err := s.PersistPausedRun(paused); err != nil {
		t.Fatal(err)
	}
	active := paused
	active.Lifecycle = "active"
	active.Boundary = "in_stage"
	if err := s.PersistActiveRun(active); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LoadRunState("GH-36")
	if err != nil || !ok {
		t.Fatalf("load run state = %+v, ok=%v, err=%v", got, ok, err)
	}
	if !reflect.DeepEqual(got, paused) {
		t.Fatalf("active snapshot overwrote paused run: got %+v, want %+v", got, paused)
	}
}

func TestLegacyIssueDefaultsToManualPlanReview(t *testing.T) {
	database := t.TempDir() + "/legacy.db"
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE issues(
  id TEXT PRIMARY KEY, title TEXT, body TEXT, state TEXT,
  flow TEXT, levers TEXT, priority INTEGER, links TEXT)`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO issues(id,title,body,state,flow,levers,priority) VALUES(?,?,?,?,?,?,?)`,
		"GH-legacy", "legacy", "", "running", "default", `{"plan":"regular"}`, 0); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.Issues()
	if err != nil || len(rows) != 1 {
		t.Fatalf("issues = %+v, err = %v", rows, err)
	}
	policy := rows[0].PlanReviewPolicy
	if policy.Mode != "regular" || !policy.HumanRequired || policy.PolicyAutoApproval ||
		policy.PolicyID == "" || policy.PolicyVersion == "" {
		t.Fatalf("legacy plan review policy = %+v", policy)
	}
}

func TestLegacyDecisionTableAddsPageSnapshot(t *testing.T) {
	database := filepath.Join(t.TempDir(), "legacy-decision.db")
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE decisions(
  id INTEGER PRIMARY KEY AUTOINCREMENT, issue_id TEXT, question TEXT, options TEXT,
  recommended INTEGER, evidence TEXT, lever TEXT, status TEXT, answer TEXT,
  answered_by TEXT, blocking_cost INTEGER, created_at TEXT, answered_at TEXT,
  briefing TEXT)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	createdAt := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO decisions(issue_id,question,options,recommended,evidence,lever,status,answer,blocking_cost,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?)`, "GH-1", "Continue?", `[]`, 0, `{}`, "execute", "pending", "", 1, createdAt); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.AllDecisionRows()
	if err != nil || len(rows) != 1 || rows[0].PageSnapshot != nil {
		t.Fatalf("legacy decisions = %#v, err = %v", rows, err)
	}
	snapshot := decisionpage.PageData{IssueID: "GH-1", Title: "Decision-time title"}
	if err := s.SaveDecisionPageSnapshot(rows[0].ID, snapshot); err != nil {
		t.Fatal(err)
	}
	rows, err = s.AllDecisionRows()
	if err != nil || len(rows) != 1 || rows[0].PageSnapshot == nil || rows[0].PageSnapshot.Title != snapshot.Title {
		t.Fatalf("migrated decisions = %#v, err = %v", rows, err)
	}
}

func TestPlanReviewPolicyRoundTrips(t *testing.T) {
	s, err := Open("file:plan-review-evidence?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	policy := &review.ResolvedPolicy{
		Mode: "regular", PolicyID: "team-ci", PolicyVersion: "2026-08-03",
		PolicyAutoApproval: true, Reason: "policy_opt_in",
	}
	approval := &review.ApprovalProvenance{
		Kind: review.ApprovalPolicy, PolicyID: "team-ci", PolicyVersion: "2026-08-03",
	}
	pendingID, err := s.InsertDecision(DecisionRow{
		IssueID: "GH-35", Stage: "plan", Question: "Review plan", Options: []string{"approve", "revise"},
		ReviewPolicy: policy, Approval: approval,
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.PendingDecisionRows()
	if err != nil || len(rows) != 1 || rows[0].ID != pendingID {
		t.Fatalf("pending rows = %+v, err = %v", rows, err)
	}
	if rows[0].ReviewPolicy == nil || rows[0].ReviewPolicy.PolicyID != "team-ci" ||
		rows[0].Approval == nil || rows[0].Approval.Kind != review.ApprovalPolicy {
		t.Fatalf("pending plan review evidence = %+v", rows[0])
	}
	all, err := s.AllDecisionRows()
	if err != nil || len(all) != 1 {
		t.Fatalf("all rows = %+v, err = %v", all, err)
	}
	if all[0].ReviewPolicy == nil || all[0].ReviewPolicy.PolicyVersion != "2026-08-03" ||
		all[0].Approval == nil || all[0].Approval.PolicyVersion != "2026-08-03" {
		t.Fatalf("all plan review evidence = %+v", all[0])
	}
}

func TestDecisionBriefingRoundTrip(t *testing.T) {
	s, err := Open("file:decision-briefing-round-trip?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	want := &levers.Briefing{
		Wins:       []string{"legacy win"},
		NextAction: "legacy next action",
		Proof: []levers.BriefingProof{{
			Claim: "Focused tests pass.", Cite: "go test ./internal/decisionpage",
		}},
		Excerpts: []levers.BriefingExcerpt{{Text: "approved requirement", Cite: "spec.md §1"}},
	}
	id, err := s.InsertDecision(DecisionRow{
		IssueID: "GH-1", Stage: "execute", Question: "Q", Options: []string{"a", "b"},
		Briefing: want,
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.PendingDecisionRows()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == id {
			if row.Briefing == nil || row.Briefing.NextAction != "legacy next action" ||
				len(row.Briefing.Wins) != 1 || len(row.Briefing.Proof) != 1 ||
				row.Briefing.Proof[0].Cite != "go test ./internal/decisionpage" ||
				len(row.Briefing.Excerpts) != 1 || row.Briefing.Excerpts[0].Cite != "spec.md §1" {
				t.Fatalf("briefing lost: %+v", row.Briefing)
			}
			return
		}
	}
	t.Fatal("row not found")
}

func testDecisionContext() *decision.DecisionContext {
	return &decision.DecisionContext{
		TaskSummary: "Ship decision context.", AgentName: "Executor",
		AgentColor: "green", AgentSymbol: "⚙",
	}
}

func TestStageCheckpointLifecyclePreservesSuccessfulHistory(t *testing.T) {
	s, err := Open("file:checkpoints?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	successID, err := s.InsertStageCheckpoint(StageCheckpoint{
		IssueID: "GH-1", Stage: "brainstorm", StartCommit: "abc", Status: "running",
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact := contextpack.Artifact{Name: "brainstorm.md", SHA256: "digest"}
	if err := s.FinishStageCheckpoint(
		successID, "succeeded", "def", "session-1", "", []contextpack.Artifact{artifact}); err != nil {
		t.Fatal(err)
	}
	failedID, err := s.InsertStageCheckpoint(StageCheckpoint{
		IssueID: "GH-1", Stage: "spec", StartCommit: "def", Status: "running",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStageCheckpoint(
		failedID, "failed", "def", "session-2", "spec failed", nil); err != nil {
		t.Fatal(err)
	}
	rows, err := s.StageCheckpoints("GH-1")
	if err != nil || len(rows) != 2 {
		t.Fatalf("checkpoints: %+v err=%v", rows, err)
	}
	if rows[0].Status != "succeeded" || rows[0].Artifacts[0] != artifact ||
		rows[0].StartCommit != "abc" || rows[0].EndCommit != "def" ||
		rows[0].SessionID != "session-1" {
		t.Fatalf("successful checkpoint changed: %+v", rows[0])
	}
	if rows[1].Status != "failed" || rows[1].Failure != "spec failed" {
		t.Fatalf("failed checkpoint: %+v", rows[1])
	}
}

func TestArtifactReviewPersistsExactTargetAndRejectsChangedDigest(t *testing.T) {
	database := t.TempDir() + "/store.db"
	s, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	checkpointID, err := s.InsertStageCheckpoint(StageCheckpoint{
		IssueID: "GH-26", Stage: "plan", Status: "running",
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	target := review.Target{
		IssueID: "GH-26", Stage: "plan", CheckpointID: checkpointID,
		Artifacts: []contextpack.Artifact{{Name: "plan.md", SHA256: digest}},
		NextStage: "execute",
	}
	decisionID, err := s.RequestArtifactReview(target, DecisionRow{
		IssueID: "GH-26", Stage: "plan", Question: "Review plan.md",
		Options: []string{"approve", "revise"}, Recommended: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	checkpoints, err := s.StageCheckpoints("GH-26")
	if err != nil || len(checkpoints) != 1 {
		t.Fatalf("checkpoints = %+v, err = %v", checkpoints, err)
	}
	if checkpoints[0].Status != "awaiting_review" || len(checkpoints[0].Artifacts) != 1 || checkpoints[0].Artifacts[0].SHA256 != digest {
		t.Fatalf("checkpoint = %+v", checkpoints[0])
	}
	rows, err := s.ArtifactReviewRows("GH-26")
	if err != nil || len(rows) != 1 {
		t.Fatalf("review rows = %+v, err = %v", rows, err)
	}
	got := rows[0]
	if got.ID != decisionID || got.Status != "pending" || got.Review == nil || got.Review.CheckpointID != checkpointID ||
		got.Review.NextStage != "execute" || got.Review.Artifacts[0] != target.Artifacts[0] ||
		got.Review.ArtifactVersion == "" {
		t.Fatalf("decision review = %+v", got)
	}

	changed := *got.Review
	changed.Artifacts = []contextpack.Artifact{{Name: "plan.md", SHA256: strings.Repeat("b", 64)}}
	if _, err := s.ResolveArtifactReview(decisionID, changed, levers.ChoiceResponse(0)); err == nil {
		t.Fatal("ResolveArtifactReview accepted a changed artifact digest")
	}
	pending, err := s.PendingDecisionRows()
	if err != nil || len(pending) != 1 || pending[0].ID != decisionID {
		t.Fatalf("pending after stale answer = %+v, err = %v", pending, err)
	}
}

func TestArtifactReviewResolutionUpdatesCheckpointAndDecisionHistory(t *testing.T) {
	s, err := Open("file:artifact-review-resolution?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	checkpointID, err := s.InsertStageCheckpoint(StageCheckpoint{IssueID: "GH-26", Stage: "spec", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	target := review.Target{
		IssueID: "GH-26", Stage: "spec", CheckpointID: checkpointID,
		Artifacts: []contextpack.Artifact{{Name: "spec.md", SHA256: strings.Repeat("c", 64)}},
		NextStage: "plan",
	}
	id, err := s.RequestArtifactReview(target, DecisionRow{
		IssueID: "GH-26", Stage: "spec", Question: "Approve spec.md?",
		Options: []string{"approve", "revise"}, Recommended: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := s.ResolveArtifactReview(id, target, levers.ChoiceResponse(0))
	if err != nil || outcome != review.OutcomeAccepted {
		t.Fatalf("outcome = %q, err = %v", outcome, err)
	}
	checkpoints, err := s.StageCheckpoints("GH-26")
	if err != nil || checkpoints[0].Status != "handoff_authorized" {
		t.Fatalf("checkpoint after acceptance = %+v, err = %v", checkpoints, err)
	}
	rows, err := s.ArtifactReviewRows("GH-26")
	if err != nil || len(rows) != 1 || rows[0].Status != "answered" || rows[0].AnsweredAt.IsZero() {
		t.Fatalf("answered review = %+v, err = %v", rows, err)
	}
	if err := s.CompleteArtifactReview(checkpointID, target); err != nil {
		t.Fatal(err)
	}
	checkpoints, err = s.StageCheckpoints("GH-26")
	if err != nil || checkpoints[0].Status != "succeeded" {
		t.Fatalf("completed checkpoint = %+v, err = %v", checkpoints, err)
	}
}

func TestIssueIntegrationLifecycle(t *testing.T) {
	s, err := Open("file:integration-lifecycle?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok, err := s.IssueIntegration("GH-1"); err != nil || ok {
		t.Fatalf("missing integration = ok %v err %v", ok, err)
	}
	ready := IssueIntegration{
		IssueID: "GH-1", State: IntegrationVerificationReady, PreSHA: "base",
		Worktree: "/tmp/GH-1", Branch: "issue/GH-1",
	}
	if err := s.SetIssueIntegration(ready); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.IssueIntegration("GH-1")
	if err != nil || !ok || got.State != IntegrationVerificationReady || got.PreSHA != "base" ||
		got.Worktree != "/tmp/GH-1" || got.Branch != "issue/GH-1" {
		t.Fatalf("ready integration = %+v ok %v err %v", got, ok, err)
	}
	preserved := IssueIntegration{
		IssueID: "GH-1", State: IntegrationPreserved, PreSHA: "preserved-base",
		Worktree: "/tmp/preserved-GH-1", Branch: "issue/GH-1",
	}
	if err := s.SetIssueIntegration(preserved); err != nil {
		t.Fatal(err)
	}
	got, ok, err = s.IssueIntegration("GH-1")
	if err != nil || !ok || got.State != IntegrationPreserved ||
		got.PreSHA != preserved.PreSHA || got.Worktree != preserved.Worktree ||
		got.Branch != preserved.Branch {
		t.Fatalf("preserved integration = %+v ok %v err %v", got, ok, err)
	}

	pending := IssueIntegration{
		IssueID: "GH-1", State: IntegrationPublishPending, BaseBranch: "main",
		PreSHA: "before", LandedSHA: "merged", LastError: "push failed",
		Cleanup: []string{"delete_branch:issue/GH-1"},
	}
	if err := s.SetIssueIntegration(pending); err != nil {
		t.Fatal(err)
	}
	got, ok, err = s.IssueIntegration("GH-1")
	if err != nil || !ok || got.IssueID != pending.IssueID ||
		got.State != pending.State || got.BaseBranch != pending.BaseBranch ||
		got.PreSHA != pending.PreSHA || got.LandedSHA != pending.LandedSHA ||
		got.LastError != pending.LastError || len(got.Cleanup) != 1 ||
		got.Cleanup[0] != pending.Cleanup[0] || got.UpdatedAt.IsZero() {
		t.Fatalf("integration = %+v ok %v err %v", got, ok, err)
	}
	pending.State = IntegrationMerged
	pending.LastError = ""
	if err := s.SetIssueIntegration(pending); err != nil {
		t.Fatal(err)
	}
	got, ok, err = s.IssueIntegration("GH-1")
	if err != nil || !ok || got.State != IntegrationMerged || got.LastError != "" {
		t.Fatalf("updated integration = %+v ok %v err %v", got, ok, err)
	}
}

func TestIssueIntegrationWriteFailureInjection(t *testing.T) {
	s, err := Open("file:integration-write-failure?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := IssueIntegration{
		IssueID: "GH-1", State: IntegrationMerged,
		BaseBranch: "main", LandedSHA: "landed-a",
	}
	s.FailIssueIntegrationWriteAfterForTest(0)
	if err := s.SetIssueIntegration(want); err == nil {
		t.Fatal("SetIssueIntegration succeeded with an injected failure")
	}
	if got, ok, err := s.IssueIntegration(want.IssueID); err != nil || ok {
		t.Fatalf("failed write left integration = %+v ok=%v err=%v", got, ok, err)
	}
	if err := s.SetIssueIntegration(want); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.IssueIntegration(want.IssueID); err != nil || !ok ||
		got.IssueID != want.IssueID || got.State != want.State ||
		got.BaseBranch != want.BaseBranch || got.LandedSHA != want.LandedSHA {
		t.Fatalf("retry integration = %+v ok=%v err=%v", got, ok, err)
	}

	other := IssueIntegration{
		IssueID: "GH-2", State: IntegrationMerged,
		BaseBranch: "main", LandedSHA: "landed-b",
	}
	s.FailIssueIntegrationWriteAfterForTest(1)
	if err := s.SetIssueIntegration(want); err != nil {
		t.Fatalf("first write with one allowed write failed: %v", err)
	}
	if err := s.SetIssueIntegration(other); err == nil {
		t.Fatal("second write with one allowed write succeeded")
	}
	if err := s.SetIssueIntegration(other); err != nil {
		t.Fatalf("third write after consumed failure failed: %v", err)
	}
}

func TestClaimedIssueIntegrationPreservesWorkspaceIdentity(t *testing.T) {
	s, err := Open("file:claimed-integration?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := IssueIntegration{
		IssueID: "GH-41", State: IntegrationClaimed, PreSHA: "base",
		Worktree: "/tmp/GH-41", Branch: "issue/GH-41",
	}
	if err := s.SetIssueIntegration(want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.IssueIntegration(want.IssueID)
	if err != nil || !ok {
		t.Fatalf("IssueIntegration() = %+v, %v, %v", got, ok, err)
	}
	if got.State != want.State || got.PreSHA != want.PreSHA ||
		got.Worktree != want.Worktree || got.Branch != want.Branch {
		t.Fatalf("claim identity = %+v, want %+v", got, want)
	}
}

func TestAppendAssignsSeqAndReplays(t *testing.T) {
	s, err := Open("file:t1?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	e1, _ := core.NewEvent(core.EvIssueCreated, "GH-1", map[string]string{"title": "a"})
	e2, _ := core.NewEvent(core.EvStageStarted, "GH-1", map[string]string{"stage": "brainstorm"})
	e1, err = s.Append(e1)
	if err != nil {
		t.Fatal(err)
	}
	e2, _ = s.Append(e2)
	if e2.Seq != e1.Seq+1 {
		t.Fatalf("seq not monotonic: %d then %d", e1.Seq, e2.Seq)
	}
	got, err := s.EventsSince(e1.Seq) // strictly after e1
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Type != core.EvStageStarted {
		t.Fatalf("replay wrong: %+v", got)
	}
}

func TestPlannerSnapshotEventContainsMetadataOnly(t *testing.T) {
	snapshot := stageusage.Snapshot{CallsUsed: 3, ChargedTokens: 99, LastSource: "spec.md"}
	ev, err := core.NewEvent(core.EvPlannerBudgetUpdated, "GH-39", map[string]any{
		"stage": "plan", "outcome": "budget_limited", "snapshot": snapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ev.Payload), "source contents") || strings.Contains(string(ev.Payload), "password") {
		t.Fatalf("payload leaked private content: %s", ev.Payload)
	}
}

func TestLatestPlannerSnapshotSurvivesStoreReopen(t *testing.T) {
	database := filepath.Join(t.TempDir(), "watchtower.db")
	store, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := stageusage.Snapshot{Stage: "plan", Attempt: 2, CallsUsed: 3, ChargedTokens: 99, Status: stageusage.StatusBudgetLimited}
	ev, err := core.NewEvent(core.EvPlannerBudgetUpdated, "GH-39", map[string]any{
		"stage": "plan", "outcome": "budget_limited", "snapshot": snapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ev); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, outcome, err := reopened.LatestPlannerSnapshot("GH-39")
	if err != nil || got == nil || outcome != "budget_limited" || got.ChargedTokens != 99 || got.Attempt != 2 {
		t.Fatalf("snapshot=%+v outcome=%q err=%v", got, outcome, err)
	}
}

func TestDecisionOrderingByBlockingCost(t *testing.T) {
	s, _ := Open("file:t3?mode=memory&cache=shared")
	defer s.Close()
	old := time.Now().Add(-time.Hour)
	id1, _ := s.InsertDecision(DecisionRow{IssueID: "GH-1", Question: "small", BlockingCost: 1, CreatedAt: time.Now()})
	id2, _ := s.InsertDecision(DecisionRow{IssueID: "GH-2", Question: "big", BlockingCost: 3, CreatedAt: time.Now()})
	id3, _ := s.InsertDecision(DecisionRow{IssueID: "GH-3", Question: "old-small", BlockingCost: 1, CreatedAt: old})
	rows, err := s.PendingDecisionRows()
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].ID != id2 || rows[1].ID != id3 || rows[2].ID != id1 {
		t.Fatalf("order wrong: %v %v %v", rows[0].ID, rows[1].ID, rows[2].ID)
	}
	if err := s.AnswerDecision(id2, levers.ChoiceResponse(0), "answered"); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.PendingDecisionRows()
	if len(rows) != 2 {
		t.Fatalf("answered row still pending: %v", rows)
	}
}

func TestDecisionRoundTripsTypedFreeformResponse(t *testing.T) {
	s, err := Open("file:typed-decisions?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id, err := s.InsertDecision(DecisionRow{
		IssueID:             "GH-1",
		Stage:               "spec",
		Kind:                levers.DecisionFreeform,
		Question:            "Review spec.md",
		RecommendedResponse: "Approve spec.md as written.",
		Importance:          0.8,
		Paths:               []string{"spec.md"},
		Why:                 "It matches the approved design.",
		Consequences:        []string{"Planning begins."},
		Reversible:          "yes",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := levers.FreeformResponse("Clarify the rollout before approval.")
	if err := s.AnswerDecision(id, response, "answered"); err != nil {
		t.Fatal(err)
	}

	rows, err := s.AllDecisionRows()
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %#v, err = %v", rows, err)
	}
	got := rows[0]
	if got.Kind != levers.DecisionFreeform ||
		got.RecommendedResponse != "Approve spec.md as written." ||
		got.Importance != 0.8 || len(got.Paths) != 1 ||
		got.Response.Kind != levers.DecisionFreeform ||
		got.Response.Text != response.Text {
		t.Fatalf("decision = %#v", got)
	}
}

func TestAnswerDecisionPersistsAnsweredAt(t *testing.T) {
	s, err := Open("file:decision-answer-time?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id, err := s.InsertDecision(DecisionRow{
		IssueID: "GH-1", Stage: "execute", Kind: levers.DecisionChoice,
		Question: "Continue?", Options: []string{"continue", "stop"}, Recommended: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC()
	if err := s.AnswerDecision(id, levers.ChoiceResponse(0), "answered"); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()

	rows, err := s.AllDecisionRows()
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %#v, err = %v", rows, err)
	}
	if rows[0].AnsweredAt.Before(before) || rows[0].AnsweredAt.After(after) {
		t.Fatalf("answered_at = %v, want between %v and %v", rows[0].AnsweredAt, before, after)
	}
}

func TestDecisionRequiresOptionRoundTrip(t *testing.T) {
	s, err := Open("file:decision-requires-option?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.InsertDecision(DecisionRow{
		IssueID: "GH-1", Stage: "execute", Kind: levers.DecisionChoice,
		Question: "Continue?", Options: []string{"continue", "stop"}, Recommended: 0,
		RequiresOption: true,
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.AllDecisionRows()
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %#v, err = %v", rows, err)
	}
	if !rows[0].RequiresOption {
		t.Fatalf("decision response requirement was lost: %#v", rows[0])
	}
}

func TestDecisionRecoveryMetadataRoundTrip(t *testing.T) {
	s, err := Open("file:decision-recovery-metadata?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	snapshot := decisionpage.PageData{
		IssueID: "GH-1", Title: "Decision-time title", CurrentStage: "execute",
		DecisionStage: "execute", StageIndex: 1, StageTotal: 1,
	}
	_, err = s.InsertDecision(DecisionRow{
		IssueID: "GH-1", Stage: "execute", Kind: levers.DecisionChoice,
		Question: "Continue?", Options: []string{"continue", "abort"}, Recommended: 0,
		EngineContinuation: "Continue to resume execute, or abort to stop before execute starts.",
		PageSnapshot:       &snapshot,
	})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := s.AllDecisionRows()
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %#v, err = %v", rows, err)
	}
	if rows[0].EngineContinuation != "Continue to resume execute, or abort to stop before execute starts." {
		t.Fatalf("engine continuation = %q", rows[0].EngineContinuation)
	}
	if rows[0].PageSnapshot == nil || !reflect.DeepEqual(*rows[0].PageSnapshot, snapshot) {
		t.Fatalf("page snapshot = %#v, want %#v", rows[0].PageSnapshot, snapshot)
	}
}

func TestDecisionPageSnapshotIsWriteOnce(t *testing.T) {
	s, err := Open("file:decision-page-snapshot-write-once?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id, err := s.InsertDecision(DecisionRow{IssueID: "GH-1", Question: "Continue?"})
	if err != nil {
		t.Fatal(err)
	}
	first := decisionpage.PageData{IssueID: "GH-1", Title: "Decision-time title"}
	if err := s.SaveDecisionPageSnapshot(id, first); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDecisionPageSnapshot(id, decisionpage.PageData{IssueID: "GH-1", Title: "Later title"}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.AllDecisionRows()
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %#v, err = %v", rows, err)
	}
	if rows[0].PageSnapshot == nil || rows[0].PageSnapshot.Title != first.Title {
		t.Fatalf("page snapshot = %#v, want first snapshot %#v", rows[0].PageSnapshot, first)
	}
}

func TestChoiceDecisionRoundTripsTypedFreeformResponse(t *testing.T) {
	s, err := Open("file:choice-typed-decisions?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id, err := s.InsertDecision(DecisionRow{
		IssueID: "GH-1", Stage: "spec", Kind: levers.DecisionChoice,
		Question: "Approve?", Options: []string{"approve", "hold"}, Recommended: 0,
		AllowFreeform: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := levers.FreeformResponse("Clarify the rollout before approval.")
	if err := s.AnswerDecision(id, response, "answered"); err != nil {
		t.Fatal(err)
	}

	rows, err := s.AllDecisionRows()
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %#v, err = %v", rows, err)
	}
	got := rows[0]
	if got.Kind != levers.DecisionChoice || got.AllowFreeform || got.Status != "answered" ||
		got.Response.Kind != levers.DecisionFreeform || got.Response.Text != response.Text {
		t.Fatalf("decision = %#v", got)
	}
}

func TestDecisionContextRoundTrip(t *testing.T) {
	s, err := Open("file:decision-context-roundtrip?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, kind := range []levers.DecisionKind{levers.DecisionChoice, levers.DecisionFreeform} {
		id, err := s.InsertDecision(DecisionRow{
			IssueID: "GH-31", Stage: "execute", Kind: kind, Question: "Proceed?",
			Options: []string{"yes", "no"}, Recommended: 0, Context: testDecisionContext(),
		})
		if err != nil {
			t.Fatal(err)
		}
		status := "answered"
		if kind == levers.DecisionFreeform {
			status = "auto"
		}
		if err := s.AnswerDecision(id, levers.FreeformResponse("Proceed."), status); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := s.AllDecisionRows()
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %#v, err = %v", rows, err)
	}
	for _, row := range rows {
		if row.Kind == levers.DecisionChoice && row.Status != "answered" {
			t.Fatalf("choice status = %q", row.Status)
		}
		if row.Kind == levers.DecisionFreeform && row.Status != "auto" {
			t.Fatalf("freeform status = %q", row.Status)
		}
		if row.Context == nil || *row.Context != *testDecisionContext() {
			t.Fatalf("decision context = %#v", row.Context)
		}
	}
}

func TestLegacyDecisionWithoutContext(t *testing.T) {
	s, err := Open("file:legacy-decision-context?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id, err := s.InsertDecision(DecisionRow{IssueID: "GH-31", Question: "Legacy?"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE decisions SET evidence=? WHERE id=?`,
		`{"kind":"choice","why":"stored before context envelopes","consequences":[],"reversible":""}`, id); err != nil {
		t.Fatal(err)
	}
	rows, err := s.AllDecisionRows()
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %#v, err = %v", rows, err)
	}
	if rows[0].Context != nil || rows[0].Why != "stored before context envelopes" {
		t.Fatalf("legacy decision unexpectedly has context: %#v", rows[0].Context)
	}
}

func TestMalformedDecisionContext(t *testing.T) {
	cases := []struct {
		name     string
		evidence string
		want     string
	}{
		{name: "null", evidence: `{"context":null}`, want: "context"},
		{name: "partial", evidence: `{"context":{"task_summary":"Only task"}}`, want: "agent_name"},
		{name: "wrong type", evidence: `{"context":"bad"}`, want: "context"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open("file:malformed-decision-context-" + strings.ReplaceAll(tc.name, " ", "-") + "?mode=memory&cache=shared")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			id, err := s.InsertDecision(DecisionRow{IssueID: "GH-31", Question: "Malformed?"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("UPDATE decisions SET evidence=? WHERE id=?", tc.evidence, id); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AllDecisionRows(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("AllDecisionRows error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDecisionEvidenceKeepsContextNested(t *testing.T) {
	s, err := Open("file:decision-context-evidence?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, err := s.InsertDecision(DecisionRow{IssueID: "GH-31", Question: "Nested?", Context: testDecisionContext()})
	if err != nil {
		t.Fatal(err)
	}
	var evidence string
	if err := s.db.QueryRow("SELECT evidence FROM decisions WHERE id=?", id).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(evidence), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["context"]; !ok {
		t.Fatalf("evidence has no nested context: %s", evidence)
	}
}

func TestDecisionRoundTripsEscalationEvidenceAndExactBindings(t *testing.T) {
	s, err := Open("file:decision-escalation-evidence?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	hash := strings.Repeat("a", 64)
	id, err := s.InsertDecision(DecisionRow{
		IssueID: "GH-64", Stage: "execute", Question: "Approve repair?",
		Evaluation: &review.Evaluation{
			Outcome: review.OutcomeRequiresApproval, RequiredFloor: review.FloorOperator,
			EffectiveFloor: review.FloorOperator, PolicyID: "team-safety", PolicyVersion: "7",
			Item: review.ItemBinding{Kind: review.ItemRepair, Hash: hash, Path: "payments/charge.go", Operation: "repair"},
		},
		Bindings: []review.Binding{{
			Item:          review.ItemBinding{Kind: review.ItemRepair, Hash: hash, Path: "payments/charge.go", Operation: "repair"},
			RequiredFloor: review.FloorOperator, EffectiveFloor: review.FloorOperator,
			PolicyID: "team-safety", PolicyVersion: "7",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.DecisionByID(id)
	if err != nil || !ok || got.Evaluation == nil || len(got.Bindings) != 1 {
		t.Fatalf("decision = %+v, ok=%v, err=%v", got, ok, err)
	}
	if got.Evaluation.RequiredFloor != review.FloorOperator || got.Evaluation.PolicyID != "team-safety" ||
		got.Bindings[0].Item.Hash != hash || got.Bindings[0].Item.Path != "payments/charge.go" ||
		got.Bindings[0].Item.Operation != "repair" {
		t.Fatalf("escalation evidence = %+v bindings=%+v", got.Evaluation, got.Bindings)
	}
}

func TestProposalLifecycle(t *testing.T) {
	s, _ := Open("file:t4?mode=memory&cache=shared")
	defer s.Close()
	id, err := s.InsertProposal("GH-1", "New task", "details", []string{"GH-2", "GH-3"})
	if err != nil {
		t.Fatal(err)
	}
	ps, _ := s.PendingProposals()
	if len(ps) != 1 || ps[0].Title != "New task" ||
		len(ps[0].DependsOn) != 2 || ps[0].DependsOn[1] != "GH-3" {
		t.Fatalf("pending: %+v", ps)
	}
	s.SetProposalStatus(id, "accepted")
	if ps, _ = s.PendingProposals(); len(ps) != 0 {
		t.Fatalf("still pending: %+v", ps)
	}
}

func TestProposalBatchLifecycle(t *testing.T) {
	s, _ := Open("file:proposal-batch?mode=memory&cache=shared")
	defer s.Close()
	batchID, err := s.InsertProposalBatch("GH-1", []ProposalRow{
		{Key: "api", Title: "Add API"},
		{Key: "consumer", Title: "Use API", DependsOn: []string{"api"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ps, err := s.PendingProposals()
	if err != nil || len(ps) != 2 || ps[0].BatchID != batchID ||
		ps[1].BatchID != batchID || ps[1].Key != "consumer" ||
		len(ps[1].DependsOn) != 1 || ps[1].DependsOn[0] != "api" {
		t.Fatalf("pending batch: %+v err=%v", ps, err)
	}
}

func TestConcurrentDecisionAndEventWrites(t *testing.T) {
	s, _ := Open("file:t5?mode=memory&cache=shared")
	defer s.Close()
	_, _ = s.db.Exec("PRAGMA busy_timeout = 0")
	s.db.SetMaxOpenConns(8)
	var wg sync.WaitGroup
	errs := make(chan error, 512)
	start := make(chan struct{})
	for i := 0; i < 256; i++ {
		i := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.InsertDecision(DecisionRow{IssueID: "GH-1", Question: "q", BlockingCost: i})
			if err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			ev, err := core.NewEvent(core.EvStageStarted, "GH-1", map[string]int{"n": i})
			if err == nil {
				_, err = s.Append(ev)
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	rows, err := s.PendingDecisionRows()
	if err != nil || len(rows) != 256 {
		t.Fatalf("decisions=%d err=%v", len(rows), err)
	}
	events, err := s.EventsSince(0)
	if err != nil || len(events) != 256 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
}

func TestStageRunLifecycle(t *testing.T) {
	s, err := Open("file:t2?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, err := s.InsertStageRun(StageRun{IssueID: "GH-1", Stage: "execute", Agent: "executor", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStageRun(id, "succeeded", "sess-abc", 1234); err != nil {
		t.Fatal(err)
	}
	runs, err := s.StageRuns("GH-1")
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs: %v %v", runs, err)
	}
	r := runs[0]
	if r.Status != "succeeded" || r.SessionID != "sess-abc" || r.Tokens != 1234 {
		t.Fatalf("bad run: %+v", r)
	}
	tok, _ := s.IssueTokens("GH-1")
	if tok != 1234 {
		t.Fatalf("tokens: %d", tok)
	}
}

func TestIssueLeversPersistWithIssue(t *testing.T) {
	s, err := Open("file:t6?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.UpsertIssue(IssueRow{
		ID: "GH-1", Title: "lever test", Flow: "default", State: "running",
		Levers: map[string]string{"spec": "strict"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIssueLever("GH-1", "execute", "yolo"); err != nil {
		t.Fatal(err)
	}
	issues, err := s.Issues()
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Levers["spec"] != "strict" || issues[0].Levers["execute"] != "yolo" {
		t.Fatalf("levers were not persisted: %+v", issues)
	}
}

func TestDependencyReplacementAndReverseLookup(t *testing.T) {
	s, err := Open("file:dependencies?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"GH-1", "GH-2", "GH-3"} {
		if err := s.UpsertIssue(IssueRow{ID: id, Flow: "default", State: "backlog"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ReplaceDependencies("GH-2", []string{"GH-1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceDependencies("GH-3", []string{"GH-1", "GH-2"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Dependencies("GH-3"); len(got) != 2 || got[0] != "GH-1" || got[1] != "GH-2" {
		t.Fatalf("dependencies = %v", got)
	}
	if got, _ := s.Dependents("GH-1"); len(got) != 2 || got[0] != "GH-2" || got[1] != "GH-3" {
		t.Fatalf("dependents = %v", got)
	}
	if err := s.ReplaceDependencies("GH-3", []string{"GH-2"}); err != nil {
		t.Fatal(err)
	}
	issues, err := s.Issues()
	if err != nil {
		t.Fatal(err)
	}
	for _, issue := range issues {
		if issue.ID == "GH-3" && (len(issue.DependsOn) != 1 || issue.DependsOn[0] != "GH-2") {
			t.Fatalf("issue dependencies = %v", issue.DependsOn)
		}
	}
}

func attachmentFixture(issueID string) []AttachmentRow {
	now := time.Unix(1700000000, 0).UTC()
	return []AttachmentRow{
		{IssueID: issueID, Name: "app.log", Size: 2048, SourcePath: "/tmp/app.log", AddedAt: now, Ord: 0},
		{IssueID: issueID, Name: "shot.png", Size: 4096, SourcePath: "/tmp/shot.png", AddedAt: now, Ord: 1},
	}
}

func TestAttachmentsRoundTrip(t *testing.T) {
	s, err := Open("file:attach1?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := attachmentFixture("GH-1")
	if err := s.ReplaceAttachments("GH-1", want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Attachments("GH-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows", len(got))
	}
	for i, row := range got {
		if row.Name != want[i].Name || row.Size != want[i].Size ||
			row.SourcePath != want[i].SourcePath || row.Ord != i ||
			!row.AddedAt.Equal(want[i].AddedAt) {
			t.Fatalf("row %d = %+v, want %+v", i, row, want[i])
		}
	}
}

func TestReplaceAttachmentsIsFullReplacement(t *testing.T) {
	s, _ := Open("file:attach2?mode=memory&cache=shared")
	defer s.Close()
	if err := s.ReplaceAttachments("GH-1", attachmentFixture("GH-1")); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceAttachments("GH-1", attachmentFixture("GH-1")[:1]); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Attachments("GH-1")
	if len(got) != 1 || got[0].Name != "app.log" {
		t.Fatalf("stale rows survived replacement: %+v", got)
	}
}

// UpsertIssue overwrites every column of issues; attachments live in their own
// table precisely so they stay out of that blast radius.
func TestUpsertIssueLeavesAttachments(t *testing.T) {
	s, _ := Open("file:attach3?mode=memory&cache=shared")
	defer s.Close()
	if err := s.ReplaceAttachments("GH-1", attachmentFixture("GH-1")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertIssue(IssueRow{ID: "GH-1", Title: "t", State: "backlog", Flow: "default"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Attachments("GH-1"); len(got) != 2 {
		t.Fatalf("UpsertIssue disturbed attachments: %+v", got)
	}
}

func TestDeleteAttachments(t *testing.T) {
	s, _ := Open("file:attach4?mode=memory&cache=shared")
	defer s.Close()
	if err := s.ReplaceAttachments("GH-1", attachmentFixture("GH-1")); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceAttachments("GH-2", attachmentFixture("GH-2")); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAttachments("GH-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Attachments("GH-1"); len(got) != 0 {
		t.Fatalf("rows survived delete: %+v", got)
	}
	if got, _ := s.Attachments("GH-2"); len(got) != 2 {
		t.Fatalf("delete hit the wrong issue: %+v", got)
	}
}
