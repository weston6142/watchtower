package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/stagelifecycle"
	"github.com/weston6142/watchtower/internal/stageresult"
	"github.com/weston6142/watchtower/internal/store"
)

func lifecycleTestFlow() flow.Flow {
	return flow.Flow{Name: "lifecycle", Stages: []flow.Stage{{
		Name: "execute", Agents: []flow.AgentRef{{Package: "agent"}},
		Workspace: "none", Gate: flow.GateAuto, Completion: flow.CompletionAll,
	}}}
}

func TestStructuredStageResultRetryProjectsOnlyRemainingWork(t *testing.T) {
	first := executeStageEvidence(stageresult.OutcomeRetryable)
	first.RemainingWork = []stageresult.WorkItem{{Kind: stageresult.WorkPlanTask, Description: "finish task-0002", Paths: []string{"internal/engine/engine.go"}}}
	first.RemainingConcerns = []stageresult.Concern{{Explanation: "confirm retry persistence"}}
	first.Execute.PlanTasks = append(first.Execute.PlanTasks,
		stageresult.PlanTask{ID: "task-0002", Outcome: stageresult.TaskRemaining, Summary: "finish engine integration"})
	first.Execute.Skips = append(first.Execute.Skips,
		stageresult.Skip{Activity: "affected race check", Explanation: "owned by merge verification"})
	second := executeStageEvidence(stageresult.OutcomeCompleted)
	var starts atomic.Int32
	var retryBrief string
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"execute/executor": {StageEvidence: &first},
	}}
	r.OnStart = func(_, _, _, workdir string) error {
		if starts.Add(1) == 1 {
			r.Scripts["execute/executor"] = runner.Script{StageEvidence: &second}
			return nil
		}
		body, err := os.ReadFile(filepath.Join(workdir, "STAGE.md"))
		retryBrief = string(body)
		return err
	}
	e, s := structuredLifecycleEngine(t, r, []flow.Stage{structuredStage("execute", "executor", 1)})
	id := createStructuredIssue(t, e, "structured-retry", []string{"execute"})
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"finish task-0002", "owned by merge verification", "confirm retry persistence"} {
		if !strings.Contains(retryBrief, want) {
			t.Fatalf("retry brief missing %q:\n%s", want, retryBrief)
		}
	}
	for _, absent := range []string{"task-0001", "first-attempt-commit"} {
		if strings.Contains(retryBrief, absent) {
			t.Fatalf("retry brief repeated %q:\n%s", absent, retryBrief)
		}
	}
	attempts, err := s.StageResultAttempts(id, "execute", stageresult.KindExecute)
	if err != nil || len(attempts) != 2 || attempts[1].PredecessorAttemptID != attempts[0].AttemptID {
		t.Fatalf("attempts = %+v, %v", attempts, err)
	}
	events, _ := s.EventsSince(0)
	completed := 0
	for _, event := range events {
		if event.IssueID == id && event.Type == core.EvStageCompleted {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("stage completed events = %d", completed)
	}
}

func TestStageResultPersistenceFailurePreservesPredecessor(t *testing.T) {
	retryable := executeStageEvidence(stageresult.OutcomeRetryable)
	retryable.RemainingWork = []stageresult.WorkItem{{Kind: stageresult.WorkPlanTask, Description: "finish task-0002"}}
	completed := executeStageEvidence(stageresult.OutcomeCompleted)
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{"execute/executor": {StageEvidence: &retryable}}}
	e, s := structuredLifecycleEngine(t, r, []flow.Stage{structuredStage("execute", "executor", 0)})
	id := createStructuredIssue(t, e, "persistence-failure", []string{"execute"})
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("retryable first result unexpectedly completed")
	}
	r.Scripts["execute/executor"] = runner.Script{StageEvidence: &completed}
	s.FailNextStageResultPutForTest()
	if err := e.RetryStage(context.Background(), id); err == nil {
		t.Fatal("injected persistence failure unexpectedly completed")
	}
	latest, found, err := s.LatestValidStageResultAttempt(id, "execute", stageresult.KindExecute)
	if err != nil || !found || latest.StageResultOutcome != stageresult.OutcomeRetryable {
		t.Fatalf("latest after failure = %+v, %v, %v", latest, found, err)
	}
	if err := e.RetryStage(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	attempts, err := s.StageResultAttempts(id, "execute", stageresult.KindExecute)
	if err != nil || len(attempts) != 2 || attempts[1].PredecessorAttemptID != attempts[0].AttemptID {
		t.Fatalf("attempts = %+v, %v", attempts, err)
	}
}

func TestStageRejectsMissingOrWrongStructuredResult(t *testing.T) {
	wrong := correctnessStageEvidence()
	for _, tt := range []struct {
		name   string
		script runner.Script
	}{{"missing", runner.Script{OmitStageEvidence: true}}, {"wrong", runner.Script{StageEvidence: &wrong}}} {
		t.Run(tt.name, func(t *testing.T) {
			r := &runner.FakeRunner{Scripts: map[string]runner.Script{"execute/executor": tt.script}}
			e, s := structuredLifecycleEngine(t, r, []flow.Stage{structuredStage("execute", "executor", 0)})
			id := createStructuredIssue(t, e, "reject-"+tt.name, []string{"execute"})
			if err := e.StartIssue(context.Background(), id); err == nil || !strings.Contains(err.Error(), "structured stage result") {
				t.Fatalf("StartIssue error = %v", err)
			}
			if _, found, err := s.LatestValidStageResultAttempt(id, "execute", stageresult.KindExecute); err != nil || found {
				t.Fatalf("invalid result became latest: found=%v err=%v", found, err)
			}
		})
	}
}

func TestStructuredResultsRunExecuteThroughLibrarian(t *testing.T) {
	execute, correctness, clean, librarian := executeStageEvidence(stageresult.OutcomeCompleted), correctnessStageEvidence(), cleanCodeStageEvidence(), librarianStageEvidence()
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"execute/executor":                 {StageEvidence: &execute},
		"correctness/correctness-reviewer": {StageEvidence: &correctness},
		"clean-code/clean-code-reviewer":   {StageEvidence: &clean},
		"librarian/librarian":              {StageEvidence: &librarian},
	}}
	stages := []flow.Stage{
		structuredStage("execute", "executor", 0),
		structuredStage("correctness", "correctness-reviewer", 0),
		structuredStage("clean-code", "clean-code-reviewer", 0),
		structuredStage("librarian", "librarian", 0),
	}
	e, s := structuredLifecycleEngine(t, r, stages)
	id := createStructuredIssue(t, e, "full-sequence", []string{"execute", "correctness", "clean-code", "librarian"})
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	for index, want := range []struct {
		stage string
		kind  stageresult.Kind
	}{{"execute", stageresult.KindExecute}, {"correctness", stageresult.KindCorrectnessReview}, {"clean-code", stageresult.KindCleanCodeReview}, {"librarian", stageresult.KindLibrarian}} {
		attempt, found, err := s.LatestValidStageResultAttempt(id, want.stage, want.kind)
		if err != nil || !found || attempt.StageResultOutcome != stageresult.OutcomeCompleted {
			t.Fatalf("stage %d result = %+v, %v, %v", index, attempt, found, err)
		}
		ref, found, err := s.StageLifecycleResult(attempt)
		if err != nil || !found {
			t.Fatalf("stage %s ref found=%v err=%v", want.stage, found, err)
		}
		loaded, err := contextpack.LoadAttemptResult(e.issueDir(id), ref)
		if err != nil || loaded.StageResult == nil || loaded.StageResult.StageKind != want.kind || loaded.StageResult.ValidationStatus != stageresult.ValidationValid {
			t.Fatalf("stage %s loaded = %+v, %v", want.stage, loaded.StageResult, err)
		}
	}
}

func TestCompletedStructuredResultResumesLifecycleWithoutRunner(t *testing.T) {
	completed := executeStageEvidence(stageresult.OutcomeCompleted)
	var starts atomic.Int32
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{"execute/executor": {StageEvidence: &completed}}, OnStart: func(_, _, _, _ string) error {
		starts.Add(1)
		return nil
	}}
	e, s := structuredLifecycleEngine(t, r, []flow.Stage{structuredStage("execute", "executor", 0)})
	id := createStructuredIssue(t, e, "completed-resume", []string{"execute"})
	s.FailNextStageLifecycleCommitForTest()
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("injected lifecycle failure unexpectedly completed")
	}
	if err := e.RetryStage(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 {
		t.Fatalf("runner starts = %d, want 1", starts.Load())
	}
}

func structuredStage(name, agent string, retries int) flow.Stage {
	return flow.Stage{Name: name, Agents: []flow.AgentRef{{Package: agent}}, Workspace: "none", Gate: flow.GateAuto, Completion: flow.CompletionAll, Retries: retries, CapabilityProfile: flow.ProfileArtifact}
}

func structuredLifecycleEngine(t *testing.T, r runner.Runner, stages []flow.Stage) (*Engine, *store.Store) {
	t.Helper()
	f := flow.Flow{Name: "structured", Stages: stages}
	return newEngineCfg(t, r, func(cfg *Config) { cfg.Flows = map[string]flow.Flow{f.Name: f} })
}

func createStructuredIssue(t *testing.T, e *Engine, title string, stages []string) string {
	t.Helper()
	matrix := levers.Matrix{}
	for _, stage := range stages {
		matrix[stage] = flow.LeverYolo
	}
	id, err := e.CreateIssue(title, "", "structured", matrix, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func executeStageEvidence(outcome stageresult.Outcome) stageresult.Evidence {
	return stageresult.Evidence{
		SchemaVersion: stageresult.SchemaVersion, StageKind: stageresult.KindExecute, Outcome: outcome,
		Execute: &stageresult.ExecutePayload{
			PlanTasks: []stageresult.PlanTask{{ID: "task-0001", Outcome: stageresult.TaskCompleted, Summary: "completed task-0001"}},
			Commits:   []stageresult.Commit{{SHA: "first-attempt-commit", Message: "feat: task", TaskIDs: []string{"task-0001"}}},
			Checks: []stageresult.Check{
				{Name: "red", Command: "go test ./internal/engine", Result: stageresult.CheckRed, Affected: true},
				{Name: "green", Command: "go test ./internal/engine", Result: stageresult.CheckGreen, Affected: true},
			},
		},
	}
}

func correctnessStageEvidence() stageresult.Evidence {
	return stageresult.Evidence{
		SchemaVersion: stageresult.SchemaVersion, StageKind: stageresult.KindCorrectnessReview, Outcome: stageresult.OutcomeCompleted,
		CorrectnessReview: &stageresult.CorrectnessReviewPayload{
			Findings:      []stageresult.Finding{{ID: "F-1", Summary: "invalid result advanced", Status: stageresult.FindingFixed, Paths: []string{"internal/engine/engine.go"}}},
			Fixes:         []stageresult.Fix{{Summary: "reject invalid result", FindingIDs: []string{"F-1"}, Paths: []string{"internal/engine/engine.go"}, Commit: "fix123"}},
			Checks:        []stageresult.Check{{Name: "regression", Command: "go test ./internal/engine", Result: stageresult.CheckGreen, Affected: true}},
			ReviewedPaths: []string{"internal/engine/engine.go"},
		},
	}
}

func cleanCodeStageEvidence() stageresult.Evidence {
	return stageresult.Evidence{
		SchemaVersion: stageresult.SchemaVersion, StageKind: stageresult.KindCleanCodeReview, Outcome: stageresult.OutcomeCompleted,
		CleanCodeReview: &stageresult.CleanCodeReviewPayload{
			ReviewedPaths: []string{"internal/engine/engine.go"},
			Checks:        []stageresult.Check{{Name: "changed code", Command: "go test ./internal/engine", Result: stageresult.CheckGreen, Affected: true}},
			NoChange:      &stageresult.NoChangeConclusion{Explanation: "changed code follows repository conventions"},
		},
	}
}

func librarianStageEvidence() stageresult.Evidence {
	return stageresult.Evidence{
		SchemaVersion: stageresult.SchemaVersion, StageKind: stageresult.KindLibrarian, Outcome: stageresult.OutcomeCompleted,
		Librarian: &stageresult.LibrarianPayload{
			ReviewedPaths:        []string{"internal/engine/engine.go"},
			DocumentationUpdates: []stageresult.DocumentationUpdate{{Path: "docs/guildhall/lane-ops-and-issue-states.md", Summary: "document retry semantics"}},
		},
	}
}

func lifecycleTestRunner(starts *atomic.Int32) *runner.FakeRunner {
	return &runner.FakeRunner{
		Scripts: map[string]runner.Script{"execute/agent": {}},
		OnStart: func(_, _, _, _ string) error {
			starts.Add(1)
			return nil
		},
	}
}

func lifecycleTestEngine(t *testing.T, r runner.Runner) (*Engine, *store.Store) {
	t.Helper()
	f := lifecycleTestFlow()
	return newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
	})
}

func putLifecycleAttemptResult(t *testing.T, s *store.Store, attempt store.StageLifecycleAttempt, result contextpack.AttemptResult) {
	t.Helper()
	contract, err := capability.Compile(capability.CompileInput{
		IssueID: attempt.IssueID, Stage: attempt.Stage, AttemptID: attempt.AttemptID,
		Profile: flow.ProfileArtifact, WorkspaceRoot: "/tmp/worktree",
		MaterializedInputs: []string{"ISSUE.md"},
		Repository:         capability.RepositoryIdentity{Branch: "issue/fixture", BaseCommit: "base", StartCommit: "head", Tree: "tree"},
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := capability.AttemptIdentity{IssueID: attempt.IssueID, Stage: attempt.Stage, AttemptID: attempt.AttemptID}
	if err := s.CreateCapabilityAttempt(capability.AttemptRecord{Identity: identity, SchemaVersion: capability.ContractVersion, Contract: contract}); err != nil {
		t.Fatal(err)
	}
	if err := s.BindCapabilityValidation(identity, result.ResultSHA256, capability.ValidationResult{
		Passed: true, ResultDigest: result.ResultSHA256, DeltaDigest: strings.Repeat("d", 64),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutStageLifecycleResult(attempt, result); err != nil {
		t.Fatal(err)
	}
}

func lifecycleCommittedSubstates(t *testing.T, s *store.Store, issueID string) []string {
	t.Helper()
	records, err := s.StageLifecycleRecords(issueID, "execute", "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, record := range records {
		if record.Committed {
			got = append(got, string(record.Substate))
		}
	}
	return got
}

func TestStageLifecycleCommitsSixSubstatesInOrder(t *testing.T) {
	var starts atomic.Int32
	e, s := lifecycleTestEngine(t, lifecycleTestRunner(&starts))
	f := lifecycleTestFlow()
	id, err := e.CreateIssue("lifecycle", "", f.Name, levers.Matrix{"execute": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	want := []string{"runner_succeeded", "artifacts_validated", "artifacts_archived", "gate_resolved", "verification_passed", "finalization_ready"}
	if got := lifecycleCommittedSubstates(t, s, id); !reflect.DeepEqual(got, want) {
		t.Fatalf("committed substates = %v, want %v", got, want)
	}
	if starts.Load() != 1 {
		t.Fatalf("runner starts = %d, want one", starts.Load())
	}
}

func TestCheckpointFinalizationFailureIsVisibleAndResumableWithoutSecondRunner(t *testing.T) {
	var starts atomic.Int32
	r := lifecycleTestRunner(&starts)
	e, s := lifecycleTestEngine(t, r)
	f := lifecycleTestFlow()
	id, err := e.CreateIssue("lifecycle failure", "", f.Name, levers.Matrix{"execute": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.FailNextStageLifecycleCommitForTest()
	if err := e.StartIssue(context.Background(), id); err == nil ||
		!strings.Contains(err.Error(), string(stagelifecycle.CodeCheckpointFinalization)) {
		t.Fatalf("StartIssue error = %v", err)
	}
	if starts.Load() != 1 {
		t.Fatalf("runner starts after failure = %d, want one", starts.Load())
	}
	if err := e.RetryStage(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if got := lifecycleCommittedSubstates(t, s, id); len(got) != 6 || got[0] != "runner_succeeded" || got[5] != "finalization_ready" {
		t.Fatalf("recovered substates = %v", got)
	}
	if starts.Load() != 1 {
		t.Fatalf("runner starts after deterministic retry = %d, want one", starts.Load())
	}
}

func TestCheckpointPreparationFailureResumesDurableResultWithoutSecondRunner(t *testing.T) {
	var starts atomic.Int32
	e, s := lifecycleTestEngine(t, lifecycleTestRunner(&starts))
	f := lifecycleTestFlow()
	id, err := e.CreateIssue("lifecycle prepare failure", "", f.Name, levers.Matrix{"execute": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.FailNextStageLifecyclePrepareForTest()
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("StartIssue unexpectedly succeeded")
	}
	if starts.Load() != 1 {
		t.Fatalf("runner starts after prepare failure = %d, want one", starts.Load())
	}
	if err := e.RetryStage(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 {
		t.Fatalf("runner starts after durable-result recovery = %d, want one", starts.Load())
	}
}

func TestLegacyCheckpointFailurePrecedesFinalizationReady(t *testing.T) {
	var starts atomic.Int32
	e, s := lifecycleTestEngine(t, lifecycleTestRunner(&starts))
	f := lifecycleTestFlow()
	id, err := e.CreateIssue("legacy checkpoint failure", "", f.Name, levers.Matrix{"execute": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.FailNextStageCheckpointFinishForTest()
	if err := e.StartIssue(context.Background(), id); err == nil ||
		!strings.Contains(err.Error(), string(stagelifecycle.CodeCheckpointFinalization)) {
		t.Fatalf("StartIssue error = %v, want checkpoint finalization failure", err)
	}
	if got := lifecycleCommittedSubstates(t, s, id); len(got) != 5 || got[4] != "verification_passed" {
		t.Fatalf("committed substates after legacy finalization failure = %v, want through verification_passed", got)
	}
	if starts.Load() != 1 {
		t.Fatalf("runner starts = %d, want one", starts.Load())
	}
}

func TestRehydratedArtifactReviewCommitsGateResolvedBeforeNextStage(t *testing.T) {
	f := artifactGateFlow()
	e1, s := newEngineCfg(t, artifactReviewRunner(), func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
	})
	id, err := e1.CreateIssue("rehydrated artifact review", "", f.Name, levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = e1.StartIssue(context.Background(), id) }()
	spec := waitForPendingStage(t, e1, "spec")
	e2 := newEngineOnFileWithFlow(t, s, artifactReviewRunner(), e1.cfg.DataDir, f)
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if err := e2.Answer(spec.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	_ = waitForPendingStage(t, e2, "plan")
	records, err := s.StageLifecycleRecords(id, "spec", fmt.Sprintf("checkpoint-%d", spec.Review.CheckpointID))
	if err != nil {
		t.Fatal(err)
	}
	var committed []string
	for _, record := range records {
		if record.Committed {
			committed = append(committed, string(record.Substate))
		}
	}
	want := []string{"runner_succeeded", "artifacts_validated", "artifacts_archived", "gate_resolved"}
	if len(committed) < len(want) || !reflect.DeepEqual(committed[:len(want)], want) {
		t.Fatalf("spec committed substates = %v, want prefix %v", committed, want)
	}
}

func TestRehydrateRecoversDurableLifecycleBeforeLegacyFallback(t *testing.T) {
	var starts atomic.Int32
	r := lifecycleTestRunner(&starts)
	e1, s := lifecycleTestEngine(t, r)
	f := lifecycleTestFlow()
	id, err := e1.CreateIssue("lifecycle restart", "", f.Name, levers.Matrix{"execute": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.FailNextStageLifecycleCommitForTest()
	if err := e1.StartIssue(context.Background(), id); err == nil {
		t.Fatal("StartIssue unexpectedly succeeded")
	}

	e2 := newEngineOnFileWithFlow(t, s, r, e1.cfg.DataDir, f)
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if err := e2.RetryStage(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 {
		t.Fatalf("runner starts after restart recovery = %d, want one", starts.Load())
	}
}

func TestRehydrateFinalizationReadyMakesNextStageRetryable(t *testing.T) {
	var starts atomic.Int32
	f := flow.Flow{Name: "lifecycle-next-stage", Stages: []flow.Stage{
		{Name: "execute", Agents: []flow.AgentRef{{Package: "agent"}}, Workspace: "none", Gate: flow.GateAuto, Completion: flow.CompletionAll},
		{Name: "verify", Agents: []flow.AgentRef{{Package: "agent"}}, Workspace: "none", Gate: flow.GateAuto, Completion: flow.CompletionAll},
	}}
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"execute/agent": {},
		"verify/agent":  {},
	}, OnStart: func(_, _, _, _ string) error {
		starts.Add(1)
		return nil
	}}
	e1, s := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
	})
	interrupted := errors.New("restart after completed stage")
	e1.cfg.BoundaryObserver = boundaryObserverFunc(func(_ context.Context, boundary DurableBoundary) error {
		if boundary.Kind == BoundaryStageLifecycle && boundary.Stage == "execute" && boundary.ID == string(stagelifecycle.FinalizationReady) {
			return InterruptAfterCommit(interrupted)
		}
		return nil
	})
	id, err := e1.CreateIssue("completed stage restart", "", f.Name, levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e1.StartIssue(context.Background(), id); !errors.Is(err, interrupted) {
		t.Fatalf("StartIssue error = %v, want interruption", err)
	}
	e2 := newEngineOnFileWithFlow(t, s, r, e1.cfg.DataDir, f)
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if err := e2.RetryStage(context.Background(), id); err != nil {
		t.Fatalf("retry next stage after restart: %v", err)
	}
	if starts.Load() != 2 {
		t.Fatalf("runner starts after next-stage recovery = %d, want two", starts.Load())
	}
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	foundRecovery := false
	for _, event := range events {
		if event.IssueID != id || event.Type != core.EvStageFailed {
			continue
		}
		var payload struct {
			Stage string `json:"stage"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Stage == "verify" && payload.Error == "lifecycle checkpoint recovered — press R to retry" {
			foundRecovery = true
		}
	}
	if !foundRecovery {
		t.Fatal("rehydration emitted no actionable next-stage recovery event")
	}
}

func TestLifecycleRecoveryRejectsMissingLeadingStage(t *testing.T) {
	var starts atomic.Int32
	e1, s := lifecycleTestEngine(t, lifecycleTestRunner(&starts))
	f := lifecycleTestFlow()
	id, err := e1.CreateIssue("lifecycle prefix", "", f.Name, levers.Matrix{"execute": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e1.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	changed := f
	changed.Stages = []flow.Stage{
		{Name: "leading", Agents: []flow.AgentRef{{Package: "agent"}}, Workspace: "none", Gate: flow.GateAuto, Completion: flow.CompletionAll},
		f.Stages[0],
		{Name: "trailing", Agents: []flow.AgentRef{{Package: "agent"}}, Workspace: "none", Gate: flow.GateAuto, Completion: flow.CompletionAll},
	}
	e2 := newEngineOnFileWithFlow(t, s, lifecycleTestRunner(new(atomic.Int32)), e1.cfg.DataDir, changed)
	rows, err := s.Issues()
	if err != nil {
		t.Fatal(err)
	}
	var row store.IssueRow
	for _, candidate := range rows {
		if candidate.ID == id {
			row = candidate
		}
	}
	recovered, found, err := e2.lifecycleRecoveryState(row)
	var diagnostic *stagelifecycle.DiagnosticError
	if recovered != nil || !found || !errors.As(err, &diagnostic) || diagnostic.Code != stagelifecycle.CodeInvalidState {
		t.Fatalf("lifecycle gap recovery = %+v, %t, %v", recovered, found, err)
	}
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if err := e2.RetryStage(context.Background(), id); err == nil || !strings.Contains(err.Error(), "unknown issue") {
		t.Fatalf("rehydration restored legacy state across lifecycle gap: %v", err)
	}
}

func TestLifecycleRecoveryRejectsRenamedCommittedPrefix(t *testing.T) {
	var starts atomic.Int32
	e1, s := lifecycleTestEngine(t, lifecycleTestRunner(&starts))
	interrupted := errors.New("simulated interruption")
	e1.cfg.BoundaryObserver = boundaryObserverFunc(func(_ context.Context, boundary DurableBoundary) error {
		if boundary.Kind == BoundaryStageLifecycle && boundary.ID == string(stagelifecycle.RunnerSucceeded) {
			return InterruptAfterCommit(interrupted)
		}
		return nil
	})
	f := lifecycleTestFlow()
	id, err := e1.CreateIssue("renamed lifecycle prefix", "", f.Name, levers.Matrix{"execute": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e1.StartIssue(context.Background(), id); !errors.Is(err, interrupted) {
		t.Fatalf("StartIssue error = %v, want interruption", err)
	}
	changed := f
	changed.Stages = []flow.Stage{
		{Name: "renamed-leading", Agents: []flow.AgentRef{{Package: "agent"}}, Workspace: "none", Gate: flow.GateAuto, Completion: flow.CompletionAll},
		{Name: "renamed-trailing", Agents: []flow.AgentRef{{Package: "agent"}}, Workspace: "none", Gate: flow.GateAuto, Completion: flow.CompletionAll},
	}
	e2 := newEngineOnFileWithFlow(t, s, lifecycleTestRunner(new(atomic.Int32)), e1.cfg.DataDir, changed)
	rows, err := s.Issues()
	if err != nil {
		t.Fatal(err)
	}
	var row store.IssueRow
	for _, candidate := range rows {
		if candidate.ID == id {
			row = candidate
		}
	}
	recovered, found, err := e2.lifecycleRecoveryState(row)
	var diagnostic *stagelifecycle.DiagnosticError
	if recovered != nil || !found || !errors.As(err, &diagnostic) || diagnostic.Code != stagelifecycle.CodeInvalidState {
		t.Fatalf("renamed lifecycle recovery = %+v, %t, %v", recovered, found, err)
	}
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if err := e2.RetryStage(context.Background(), id); err == nil || !strings.Contains(err.Error(), "unknown issue") {
		t.Fatalf("rehydration restored legacy state across renamed lifecycle gap: %v", err)
	}
}

func TestLifecycleAttemptForUsesNewestNumericAttempt(t *testing.T) {
	e, s := lifecycleTestEngine(t, lifecycleTestRunner(new(atomic.Int32)))
	for _, attemptID := range []string{"checkpoint-9", "checkpoint-10"} {
		attempt := store.BeginAttempt("GH-66", "execute", attemptID)
		if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
			t.Fatal(err)
		}
		putLifecycleAttemptResult(t, s, attempt, contextpack.AttemptResult{
			AttemptID: attemptID, ResultPath: "artifacts/attempts/" + attemptID + "/result/manifest.json", ResultSHA256: strings.Repeat("a", 64),
		})
		record := stagelifecycle.Record{
			SchemaVersion: 1, IssueID: attempt.IssueID, Stage: attempt.Stage, AttemptID: attempt.AttemptID,
			Version: 1, Substate: stagelifecycle.RunnerSucceeded, TransitionID: attemptID + ":runner_succeeded",
			ResultRef: "artifacts/attempts/" + attemptID + "/result/manifest.json", ResultDigest: strings.Repeat("a", 64), PayloadDigest: strings.Repeat("a", 64),
		}
		if err := s.PrepareStageLifecycle(record); err != nil {
			t.Fatal(err)
		}
		if err := s.CommitPreparedStageLifecycle(record); err != nil {
			t.Fatal(err)
		}
	}
	got, records, err := e.lifecycleAttemptFor(&issueState{id: "GH-66"}, flow.Stage{Name: "execute"}, 11)
	if err != nil {
		t.Fatal(err)
	}
	if got.AttemptID != "checkpoint-10" {
		t.Fatalf("selected attempt = %q, want checkpoint-10", got.AttemptID)
	}
	for _, record := range records {
		if record.AttemptID != got.AttemptID {
			t.Fatalf("recovery record from attempt %q was returned for selected attempt %q", record.AttemptID, got.AttemptID)
		}
	}
}

func TestLifecycleAttemptForReusesDurableResultWithoutLedgerRecord(t *testing.T) {
	e, s := lifecycleTestEngine(t, lifecycleTestRunner(new(atomic.Int32)))
	attempt := store.BeginAttempt("GH-66", "execute", "checkpoint-9")
	if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
		t.Fatal(err)
	}
	putLifecycleAttemptResult(t, s, attempt, contextpack.AttemptResult{
		AttemptID: attempt.AttemptID, ResultPath: "artifacts/attempts/" + attempt.AttemptID + "/result/manifest.json", ResultSHA256: strings.Repeat("a", 64),
	})
	got, _, err := e.lifecycleAttemptFor(&issueState{id: "GH-66"}, flow.Stage{Name: "execute"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got.AttemptID != attempt.AttemptID {
		t.Fatalf("selected attempt = %q, want %q", got.AttemptID, attempt.AttemptID)
	}
}

func TestMaterializeStageContextUsesNewestNumericAttempt(t *testing.T) {
	e, s := lifecycleTestEngine(t, lifecycleTestRunner(new(atomic.Int32)))
	issueID := "GH-66"
	for _, item := range []struct {
		attemptID string
		contents  string
	}{
		{attemptID: "checkpoint-9", contents: "older"},
		{attemptID: "checkpoint-10", contents: "newer"},
	} {
		attempt := store.BeginAttempt(issueID, "spec", item.attemptID)
		if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
			t.Fatal(err)
		}
		resultPath := "artifacts/attempts/" + item.attemptID + "/result/manifest.json"
		putLifecycleAttemptResult(t, s, attempt, contextpack.AttemptResult{
			AttemptID: item.attemptID, ResultPath: resultPath, ResultSHA256: strings.Repeat("a", 64),
		})
		archivePath := filepath.Join(e.issueDir(issueID), "artifacts", "attempts", item.attemptID, "plan.md")
		if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(archivePath, []byte(item.contents), 0o644); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(item.contents))
		archiveRef := contextpack.AttemptArtifact{
			Name: "plan.md", Path: filepath.ToSlash(filepath.Join("artifacts", "attempts", item.attemptID, "plan.md")),
			SHA256: hex.EncodeToString(digest[:]),
		}
		transitionID := item.attemptID + ":artifacts_archived"
		if err := s.PutStageArchiveManifest(attempt, transitionID, []contextpack.AttemptArtifact{archiveRef}); err != nil {
			t.Fatal(err)
		}
		for version, substate := range []stagelifecycle.Substate{
			stagelifecycle.RunnerSucceeded, stagelifecycle.ArtifactsValidated, stagelifecycle.ArtifactsArchived,
		} {
			record := stagelifecycle.Record{
				SchemaVersion: 1, IssueID: issueID, Stage: "spec", AttemptID: item.attemptID,
				Version: version + 1, Substate: substate, PredecessorVersion: version,
				TransitionID: item.attemptID + ":" + string(substate), PayloadDigest: strings.Repeat("b", 64),
				ResultRef: resultPath, ResultDigest: strings.Repeat("a", 64),
			}
			if substate == stagelifecycle.ArtifactsArchived {
				record.Artifacts = []stagelifecycle.ArtifactRef{{Name: archiveRef.Name, Path: archiveRef.Path, SHA256: archiveRef.SHA256}}
			}
			if err := s.PrepareStageLifecycle(record); err != nil {
				t.Fatal(err)
			}
			if err := s.CommitPreparedStageLifecycle(record); err != nil {
				t.Fatal(err)
			}
		}
		checkpointID, err := s.InsertStageCheckpoint(store.StageCheckpoint{IssueID: issueID, Stage: "spec", Status: "running"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishStageCheckpoint(checkpointID, "succeeded", "", "", "", nil); err != nil {
			t.Fatal(err)
		}
	}

	workdir := t.TempDir()
	if err := e.materializeStageContext(issueID, workdir, []string{"plan.md"}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(workdir, "plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "newer" {
		t.Fatalf("materialized plan = %q, want newer attempt bytes", contents)
	}
}

func TestRestoreLifecycleResultRejectsManifestIdentityMismatch(t *testing.T) {
	e, _ := lifecycleTestEngine(t, lifecycleTestRunner(new(atomic.Int32)))
	issueID, stage, attemptID := "GH-66", "execute", "checkpoint-1"
	resultPath := "artifacts/attempts/" + attemptID + "/result/manifest.json"
	manifest := []byte(`{"attempt_id":"checkpoint-2","issue_id":"GH-66","stage":"other","artifacts":[]}`)
	manifestPath := filepath.Join(e.issueDir(issueID), filepath.FromSlash(resultPath))
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(manifest)
	_, err := e.restoreLifecycleResult(e.issueDir(issueID), stagelifecycle.Record{
		SchemaVersion: 1, IssueID: issueID, Stage: stage, AttemptID: attemptID,
		Version: 1, Substate: stagelifecycle.RunnerSucceeded,
		TransitionID: attemptID + ":runner_succeeded", PayloadDigest: strings.Repeat("b", 64),
		ResultRef: resultPath, ResultDigest: hex.EncodeToString(digest[:]),
	}, t.TempDir())
	if err == nil {
		t.Fatal("restore accepted a result manifest with a conflicting identity")
	}
}
