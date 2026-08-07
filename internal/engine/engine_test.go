package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/decisionpage"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/librarian"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/plannerbudget"
	"github.com/weston6142/watchtower/internal/review"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/slots"
	"github.com/weston6142/watchtower/internal/stageusage"
	"github.com/weston6142/watchtower/internal/steward"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/touchset"
	"github.com/weston6142/watchtower/internal/verificationcache"
	"github.com/weston6142/watchtower/internal/workspace"
)

func testFlow() flow.Flow {
	f, err := flow.Load("../flow/testdata/default.yaml")
	if err != nil {
		panic(err)
	}
	return f
}

func issueRowByID(t *testing.T, rows []store.IssueRow, id string) store.IssueRow {
	t.Helper()
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("issue row %s not found in %+v", id, rows)
	return store.IssueRow{}
}

func mustIssues(t *testing.T, s *store.Store) []store.IssueRow {
	t.Helper()
	rows, err := s.Issues()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestPauseBeforeStagePersistsBoundaryAndResumeUsesIt(t *testing.T) {
	f := flow.Flow{Name: "paused", Stages: []flow.Stage{
		{Name: "plan", Agents: []flow.AgentRef{{Package: "agent"}}, Workspace: "none", Gate: flow.GateAuto, Completion: flow.CompletionAll},
		{Name: "execute", Agents: []flow.AgentRef{{Package: "agent"}}, Workspace: "none", Gate: flow.GateAuto, Completion: flow.CompletionAll},
	}}
	s, err := store.Open(filepath.Join(t.TempDir(), "gh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	runner1 := &runner.FakeRunner{Scripts: map[string]runner.Script{"plan/agent": {}, "execute/agent": {}}}
	e1 := newEngineOnFileWithFlow(t, s, runner1, t.TempDir(), f)
	id, err := e1.CreateIssue("pause before stage", "", f.Name, levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e1.Pause(id); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e1.StartIssue(context.Background(), id)
	}()
	waitForEvent(t, s, id, core.EvIssuePaused)

	row := issueRowByID(t, mustIssues(t, s), id)
	if row.State != "paused" {
		t.Fatalf("stored state = %q, want paused", row.State)
	}
	paused, ok, err := s.LoadRunState(id)
	if err != nil || !ok || paused.Lifecycle != "paused" || paused.Stage != "plan" || paused.StageIndex != 0 || paused.Boundary != "before_stage" {
		t.Fatalf("paused snapshot = %+v, ok=%v, err=%v", paused, ok, err)
	}

	if err := e1.Resume(id); err != nil {
		t.Fatal(err)
	}
	if err := e1.Resume(id); err == nil {
		t.Fatal("duplicate resume unexpectedly started or accepted a second run")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runs, runErr := s.StageRuns(id)
		if runErr == nil && len(runs) == 2 {
			<-done
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	runs, _ := s.StageRuns(id)
	t.Fatalf("resume did not run plan and execute exactly once: %+v", runs)
}

func TestPausePersistenceFailureIsSurfaced(t *testing.T) {
	e, s := newEngineCfg(t, &runner.FakeRunner{Scripts: scripts()}, nil)
	id, err := e.CreateIssue("pause failure", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.FailNextPausePersistenceForTest()
	if err := e.Pause(id); err == nil {
		t.Fatal("pause persistence failure was not returned")
	}
	if row := issueRowByID(t, mustIssues(t, s), id); row.State != "running" {
		t.Fatalf("issue state after failed pause = %q, want running", row.State)
	}
	for _, event := range mustEvents(t, s, id) {
		if event.Type == core.EvIssuePaused {
			t.Fatal("failed pause emitted issue_paused")
		}
	}
}

func testEvent(t *testing.T, typ core.EventType, issueID string, payload any) core.Event {
	t.Helper()
	event, err := core.NewEvent(typ, issueID, payload)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func countEventType(t *testing.T, s *store.Store, issueID string, typ core.EventType) int {
	t.Helper()
	count := 0
	for _, event := range mustEvents(t, s, issueID) {
		if event.Type == typ {
			count++
		}
	}
	return count
}

func TestRehydrateKeepsPausedRunPausedAndIdempotent(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "gh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := flow.Flow{Name: "restart", Stages: []flow.Stage{{
		Name: "plan", Agents: []flow.AgentRef{{Package: "agent"}}, Workspace: "none", Gate: flow.GateAuto, Completion: flow.CompletionAll,
	}}}
	if err := s.UpsertIssue(store.IssueRow{ID: "GH-36", Title: "restart", Flow: f.Name, State: "paused"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(testEvent(t, core.EvIssueCreated, "GH-36", map[string]any{"title": "restart", "flow": f.Name})); err != nil {
		t.Fatal(err)
	}
	want := store.RunState{IssueID: "GH-36", Lifecycle: "paused", Stage: "plan", StageIndex: 0, Boundary: "before_stage", Artifacts: []string{"spec.md", "plan.md"}}
	if err := s.PersistPausedRun(want); err != nil {
		t.Fatal(err)
	}
	var starts atomic.Int32
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{"plan/agent": {}}, OnStart: func(_, _, _, _ string) error { starts.Add(1); return nil }}
	e := newEngineOnFileWithFlow(t, s, r, t.TempDir(), f)
	if err := e.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	before := countEventType(t, s, "GH-36", core.EvStageFailed)
	if err := e.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	after := countEventType(t, s, "GH-36", core.EvStageFailed)
	if before != 0 || after != 0 || starts.Load() != 0 {
		t.Fatalf("paused rehydrate changed failure events or started work: before=%d after=%d starts=%d", before, after, starts.Load())
	}
	if row := issueRow(t, s, "GH-36"); row.State != "paused" {
		t.Fatalf("paused issue state = %q, want paused", row.State)
	}
	got, ok, err := s.LoadRunState("GH-36")
	if err != nil || !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("paused snapshot after repeated rehydrate = %+v, ok=%v, err=%v", got, ok, err)
	}
	if err := e.Resume("GH-36"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for starts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if starts.Load() != 1 {
		t.Fatalf("resume starts = %d, want 1", starts.Load())
	}
	waitForEvent(t, s, "GH-36", core.EvIssueCompleted)
}

func TestRehydrateActiveWorkerLossRemainsRetryableFailure(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "gh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := flow.Flow{Name: "active", Stages: []flow.Stage{{
		Name: "execute", Agents: []flow.AgentRef{{Package: "agent"}}, Workspace: "none", Gate: flow.GateAuto, Completion: flow.CompletionAll,
	}}}
	if err := s.UpsertIssue(store.IssueRow{ID: "GH-37", Title: "active", Flow: f.Name, State: "running:execute"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(testEvent(t, core.EvStageStarted, "GH-37", map[string]any{"stage": "execute", "attempt": 1, "of": 1})); err != nil {
		t.Fatal(err)
	}
	e := newEngineOnFileWithFlow(t, s, &runner.FakeRunner{Scripts: map[string]runner.Script{"execute/agent": {}}}, t.TempDir(), f)
	projection := &steward.Steward{Store: s}
	e.cfg.Observers = []func(core.Event){projection.Observe}
	if err := e.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if countEventType(t, s, "GH-37", core.EvStageFailed) != 1 || issueRow(t, s, "GH-37").State != "failed" {
		t.Fatalf("active loss did not use existing retryable failure path")
	}
	for _, event := range mustEvents(t, s, "GH-37") {
		if event.Type == core.EvIssuePaused {
			t.Fatal("active worker loss was misclassified as operator pause")
		}
	}
}

func TestPlanReviewPolicyIsSnapshottedAtIssueCreation(t *testing.T) {
	e, s := newEngineCfg(t, artifactReviewRunner(), func(cfg *Config) {
		cfg.PlanReview = review.PolicySettings{ID: "manual-default", Version: "1", Valid: true}
	})
	id, err := e.CreateIssue("snapshot", "", "default", levers.Preset(testFlow(), flow.LeverRegular), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.Issues()
	if err != nil {
		t.Fatal(err)
	}
	got := issueRowByID(t, rows, id).PlanReviewPolicy
	if got.Mode != "regular" || !got.HumanRequired || got.PolicyAutoApproval || got.PolicyID != "manual-default" {
		t.Fatalf("snapshot = %+v", got)
	}

	e2 := newEngineOnFileWithFlow(t, s, artifactReviewRunner(), e.cfg.DataDir, testFlow())
	e2.cfg.PlanReview = review.PolicySettings{
		ID: "team-ci", Version: "2026-08-03", AutoApproveRegular: true, Valid: true,
	}
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	rows, err = s.Issues()
	if err != nil {
		t.Fatal(err)
	}
	if got := issueRowByID(t, rows, id).PlanReviewPolicy; got.PolicyID != "manual-default" || !got.HumanRequired {
		t.Fatalf("rehydration reread mutable config: %+v", got)
	}

	strictID, err := e2.CreateIssue("strict snapshot", "", "default", levers.Matrix{"plan": flow.LeverStrict}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err = s.Issues()
	if err != nil {
		t.Fatal(err)
	}
	strict := issueRowByID(t, rows, strictID).PlanReviewPolicy
	if strict.Mode != "strict" || !strict.HumanRequired || strict.PolicyAutoApproval {
		t.Fatalf("strict snapshot = %+v", strict)
	}
}

// newEngineCfg builds a test engine like newEngine but lets the caller adjust
// the config first: attachment tests need a Librarian and a workspace path.
func newEngineCfg(t *testing.T, r runner.Runner, adjust func(*Config)) (*Engine, *store.Store) {
	t.Helper()
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	cfg := Config{
		Store: s, Runner: r, Pool: slots.NewPool(2),
		Flows: map[string]flow.Flow{"default": testFlow()}, DataDir: t.TempDir(),
		DecisionIdentities: testDecisionIdentities(),
	}
	if adjust != nil {
		adjust(&cfg)
	}
	return New(cfg), s
}

func testDecisionIdentities() map[string]decision.AgentIdentity {
	return map[string]decision.AgentIdentity{
		"brainstorm":           {Name: "Brainstorm", Color: "cyan", Symbol: "✦"},
		"spec-writer":          {Name: "Spec Writer", Color: "violet", Symbol: "✎"},
		"planner":              {Name: "Planner", Color: "blue", Symbol: "⌘"},
		"executor":             {Name: "Executor", Color: "green", Symbol: "⚙"},
		"clean-code-reviewer":  {Name: "Clean Code Reviewer", Color: "teal", Symbol: "◆"},
		"correctness-reviewer": {Name: "Correctness Reviewer", Color: "yellow", Symbol: "✓"},
		"conflict-resolver":    {Name: "Conflict Resolver", Color: "red", Symbol: "⚔"},
		"librarian":            {Name: "Librarian", Color: "slate", Symbol: "▤"},
		"merge-verifier":       {Name: "Merge Verifier", Color: "orange", Symbol: "⛨"},
		"agent":                {Name: "Test Agent", Color: "gray", Symbol: "A"},
		"explorer":             {Name: "Explorer", Color: "gray", Symbol: "E"},
		"researcher":           {Name: "Researcher", Color: "gray", Symbol: "S"},
		"reviewer":             {Name: "Reviewer", Color: "gray", Symbol: "R"},
		"doc-writer":           {Name: "Doc Writer", Color: "gray", Symbol: "D"},
		"verifier":             {Name: "Verifier", Color: "gray", Symbol: "V"},
	}
}

func newEngine(t *testing.T, r runner.Runner) (*Engine, *store.Store) {
	t.Helper()
	return newEngineCfg(t, r, nil)
}

type typedStageRunner struct {
	result  runner.Result
	results map[string]runner.Result
	sink    runner.AttemptSink
}

func (r *typedStageRunner) Run(ctx context.Context, issueID, stage, agentPkg, workdir string,
	_ chan<- runner.Ask) <-chan runner.Result {
	done := make(chan runner.Result, 1)
	go func() {
		result := r.result
		if configured, ok := r.results[agentPkg]; ok {
			result = configured
		}
		operationID := runner.OperationID(ctx)
		for index := range result.Attempts {
			attempt := result.Attempts[index]
			attempt.OperationID, attempt.IssueID = operationID, issueID
			attempt.Stage, attempt.AgentPackage = stage, agentPkg
			if r.sink != nil {
				_ = r.sink.RecordAttempt(ctx, attempt)
			}
		}
		done <- result
	}()
	return done
}

func (r *typedStageRunner) SetAttemptSink(sink runner.AttemptSink) { r.sink = sink }

func TestTerminalStageMapsTypedRunnerFailureToFinalStageOutcome(t *testing.T) {
	f := flow.Flow{Name: "typed", Stages: []flow.Stage{{
		Name: "execute", Agents: []flow.AgentRef{{Package: "agent"}},
		Completion: flow.CompletionAll, Workspace: "none", Gate: flow.GateAuto,
	}}}
	result := runner.Result{
		FailureClass: runner.FailureLaunch,
		Attempt:      runner.Attempt{Kind: runner.AttemptPrimary, State: runner.AttemptTerminal, FailureClass: runner.FailureLaunch},
		Attempts:     []runner.Attempt{{Kind: runner.AttemptPrimary, State: runner.AttemptTerminal, FailureClass: runner.FailureLaunch, RedactedArgv: []string{"exec", "[redacted]"}}},
		NextAction:   "restore the Codex executable",
		Err:          errors.New("Codex primary launch failed"),
	}
	e, s := newEngineCfg(t, &typedStageRunner{result: result}, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{"typed": f}
	})
	id, err := e.CreateIssue("typed failure", "", "typed", levers.Matrix{"execute": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("typed runner failure unexpectedly succeeded")
	}
	rows, err := s.StageRuns(id)
	if err != nil || len(rows) != 1 || rows[0].Status != "failed" {
		t.Fatalf("stage runs = %+v, err = %v", rows, err)
	}
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var failed bool
	for _, event := range events {
		if event.Type != core.EvStageFailed {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["final"] == true {
			failed = true
			if payload["failure_class"] != string(runner.FailureLaunch) ||
				payload["attempt_kind"] != string(runner.AttemptPrimary) ||
				payload["next_action"] != "restore the Codex executable" {
				t.Fatalf("typed stage failure payload = %v", payload)
			}
		}
	}
	if !failed {
		t.Fatal("no final typed stage failure event")
	}
	attempts, err := s.LoadOperation(context.Background(), "1")
	if err != nil || len(attempts) != 1 {
		t.Fatalf("durable attempt history = %+v, err = %v", attempts, err)
	}
}

func TestFallbackLifecycleCompletesAllStage(t *testing.T) {
	f := flow.Flow{Name: "fallback", Stages: []flow.Stage{{
		Name: "execute", Agents: []flow.AgentRef{{Package: "agent"}},
		Completion: flow.CompletionAll, Workspace: "none", Gate: flow.GateAuto,
	}}}
	result := runner.Result{
		FallbackConsumed: true,
		Attempt:          runner.Attempt{Kind: runner.AttemptFallback, State: runner.AttemptSucceeded, FailureClass: runner.FailureLaunch},
		Attempts: []runner.Attempt{
			{Kind: runner.AttemptPrimary, State: runner.AttemptFailed, FailureClass: runner.FailureLaunch},
			{Kind: runner.AttemptFallback, State: runner.AttemptReserved, FailureClass: runner.FailureLaunch},
			{Kind: runner.AttemptFallback, State: runner.AttemptSucceeded, FailureClass: runner.FailureLaunch},
		},
	}
	e, s := newEngineCfg(t, &typedStageRunner{result: result}, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{"fallback": f}
	})
	id, err := e.CreateIssue("fallback success", "", "fallback", levers.Matrix{"execute": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	runs, err := s.StageRuns(id)
	if err != nil || len(runs) != 1 || runs[0].Status != "succeeded" {
		t.Fatalf("stage runs = %+v, err = %v", runs, err)
	}
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, event := range events {
		if event.Type != core.EvRunnerAttempt {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		seen[payload["attempt_kind"].(string)+":"+payload["state"].(string)] = true
	}
	for _, key := range []string{"primary:failed", "fallback:reserved", "fallback:succeeded"} {
		if !seen[key] {
			t.Errorf("missing lifecycle event %q: %v", key, seen)
		}
	}
}

func TestEngineCompletionAnyKeepsAgentFallbackAccountingIndependent(t *testing.T) {
	f := flow.Flow{Name: "any", Stages: []flow.Stage{{
		Name: "execute", Agents: []flow.AgentRef{{Package: "first"}, {Package: "second"}},
		Parallel: true, Completion: flow.CompletionAny, Workspace: "none", Gate: flow.GateAuto,
	}}}
	r := &typedStageRunner{results: map[string]runner.Result{
		"first": {FallbackConsumed: true, Attempt: runner.Attempt{Kind: runner.AttemptFallback, State: runner.AttemptSucceeded}, Attempts: []runner.Attempt{
			{Kind: runner.AttemptPrimary, State: runner.AttemptFailed, FailureClass: runner.FailureLaunch},
			{Kind: runner.AttemptFallback, State: runner.AttemptSucceeded},
		}},
		"second": {Err: errors.New("second agent failed"), FailureClass: runner.FailureExecution, Attempt: runner.Attempt{Kind: runner.AttemptPrimary, State: runner.AttemptTerminal}, Attempts: []runner.Attempt{
			{Kind: runner.AttemptPrimary, State: runner.AttemptTerminal, FailureClass: runner.FailureExecution},
		}},
	}}
	e, s := newEngineCfg(t, r, func(cfg *Config) { cfg.Flows = map[string]flow.Flow{"any": f} })
	id, err := e.CreateIssue("any completion", "", "any", levers.Matrix{"execute": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	runs, err := s.StageRuns(id)
	if err != nil || len(runs) != 2 {
		t.Fatalf("stage runs = %+v, err = %v", runs, err)
	}
	operationIDs, err := s.LoadOperation(context.Background(), "1")
	if err != nil || len(operationIDs) == 0 {
		t.Fatalf("first agent attempts = %+v, err = %v", operationIDs, err)
	}
	other, err := s.LoadOperation(context.Background(), "2")
	if err != nil || len(other) == 0 {
		t.Fatalf("second agent attempts = %+v, err = %v", other, err)
	}
	if operationIDs[0].IssueID != id || other[0].IssueID != id || operationIDs[0].OperationID == other[0].OperationID {
		t.Fatalf("agent operation identities crossed: first=%+v second=%+v", operationIDs, other)
	}
}

func plannerTestFlow() flow.Flow {
	return flow.Flow{Name: "planner-test", Stages: []flow.Stage{
		{Name: "plan", Agents: []flow.AgentRef{{Package: "planner"}}, Workspace: "none",
			Completion: flow.CompletionAll, Gate: flow.GatePlanReview,
			Artifacts: []string{"plan.md", "touchset.json"}},
	}}
}

func TestPlannerBudgetLimitStillArchivesArtifactsAndRequestsPlanReview(t *testing.T) {
	planFlow := plannerTestFlow()
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"plan/planner": {
			Tools: []runner.ToolCall{
				{Name: "read", SourceID: "ISSUE.md", Fingerprint: "v1", Reservation: 1},
				{Name: "read", SourceID: "STAGE.md", Fingerprint: "v1", Reservation: 1},
				{Name: "read", SourceID: "internal/engine/engine.go", Fingerprint: "v1", Reservation: 1},
			},
			PlannerRequests: plannerArtifactRequests(),
			Tokens:          3, TokensKnown: true,
		},
	}}
	e, st := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{planFlow.Name: planFlow}
		cfg.PlannerBudget = plannerbudget.Profile{
			Calls:   stageusage.DimensionLimit{Warning: 1, Hard: 8},
			Tokens:  stageusage.DimensionLimit{Warning: 1, Hard: 2},
			Elapsed: stageusage.ElapsedLimit{Warning: time.Minute, Hard: 2 * time.Minute},
		}
		cfg.PlanReview = review.PolicySettings{ID: "test", Version: "1", AutoApproveRegular: true, Valid: true}
	})
	id, err := e.CreateIssue("bounded planner", "", planFlow.Name, levers.Preset(planFlow, flow.LeverRegular), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	events, err := st.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var budgetEvent, reviewEvent bool
	for _, event := range events {
		budgetEvent = budgetEvent || event.Type == core.EvPlannerBudgetUpdated
		reviewEvent = reviewEvent || event.Type == core.EvPlanReviewRequested
	}
	if !budgetEvent || !reviewEvent {
		t.Fatalf("events missing planner budget/review: %+v", events)
	}
	runs, err := st.StageRuns(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Tokens != 2 || runs[0].Status != "succeeded" {
		t.Fatalf("stage runs = %+v", runs)
	}
	artifacts, err := st.ArtifactPaths(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 2 {
		t.Fatalf("archived artifacts = %v", artifacts)
	}
}

func scripts() map[string]runner.Script {
	return map[string]runner.Script{
		"brainstorm/brainstorm": {Asks: []levers.Decision{
			{Question: "Scope ok?", Options: []string{"yes", "no"}, Recommended: 0, Importance: 0.3}}},
		"spec/spec-writer":           {Artifacts: map[string]string{"spec.md": ""}},
		"execute/executor":           {Artifacts: map[string]string{"diff": ""}, Tokens: 100},
		"review/clean-code-reviewer": {Artifacts: map[string]string{"review.md": ""}},
		"review/reviewer":            {},
		"review/doc-writer":          {Artifacts: map[string]string{"docs": ""}},
	}
}

func artifactGateFlow() flow.Flow {
	return flow.Flow{Name: "artifact-gates", Stages: []flow.Stage{
		{Name: "spec", Agents: []flow.AgentRef{{Package: "spec-writer"}}, Workspace: "none", Completion: flow.CompletionAll,
			Gate: flow.GateApproveArtifact, Artifacts: []string{"spec.md"}},
		{Name: "plan", Agents: []flow.AgentRef{{Package: "planner"}}, Workspace: "none", Completion: flow.CompletionAll,
			Gate: flow.GateApproveArtifact, Artifacts: []string{"plan.md", "touchset.json"}},
		{Name: "implementation", Agents: []flow.AgentRef{{Package: "implementation"}}, Workspace: "none", Completion: flow.CompletionAll,
			Gate: flow.GateAuto},
	}}
}

func waitForPendingStage(t *testing.T, e *Engine, stage string) PendingDecision {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		for _, pending := range e.PendingDecisions() {
			if pending.Stage == stage {
				return pending
			}
		}
		select {
		case <-deadline:
			t.Fatalf("pending %s review never appeared", stage)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func planReviewFlow() flow.Flow {
	return flow.Flow{Name: "plan-review", Stages: []flow.Stage{
		{Name: "plan", Agents: []flow.AgentRef{{Package: "planner"}}, Workspace: "none",
			Completion: flow.CompletionAll, Gate: flow.GatePlanReview, Artifacts: []string{"plan.md"}},
		{Name: "execute", Agents: []flow.AgentRef{{Package: "executor"}}, Workspace: "none",
			Completion: flow.CompletionAll, Gate: flow.GateAuto},
	}}
}

func planReviewRunner() *runner.FakeRunner {
	return &runner.FakeRunner{Scripts: map[string]runner.Script{
		"plan/planner":     {Artifacts: map[string]string{"plan.md": "plan\n"}},
		"execute/executor": {},
	}}
}

func planReviewSettings(id, version string, auto bool) review.PolicySettings {
	return review.PolicySettings{ID: id, Version: version, AutoApproveRegular: auto, Valid: true}
}

func planReviewEvents(t *testing.T, s *store.Store, issueID string, typ core.EventType) []core.Event {
	t.Helper()
	events := mustEvents(t, s, issueID)
	var matching []core.Event
	for _, event := range events {
		if event.Type == typ {
			matching = append(matching, event)
		}
	}
	return matching
}

func waitForPlanReviewEvent(t *testing.T, s *store.Store, issueID string, typ core.EventType) core.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if events := planReviewEvents(t, s, issueID, typ); len(events) > 0 {
			return events[0]
		}
		select {
		case <-deadline:
			t.Fatalf("event %s for %s never appeared", typ, issueID)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func assertPlanApprovalEvent(t *testing.T, event core.Event, typ core.EventType, kind, actor, policyID, policyVersion string) {
	t.Helper()
	if event.Type != typ {
		t.Fatalf("event type = %s, want %s", event.Type, typ)
	}
	payload := map[string]any{}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	value := func(key string) string {
		if payload[key] == nil {
			return ""
		}
		return fmt.Sprint(payload[key])
	}
	if value("approval_kind") != kind || value("actor_id") != actor ||
		value("policy_id") != policyID || value("policy_version") != policyVersion {
		t.Fatalf("approval payload = %+v", payload)
	}
}

func TestPlanReviewAuthorizationMatrix(t *testing.T) {
	t.Run("regular default requires human approval", func(t *testing.T) {
		f := planReviewFlow()
		e, s := newEngineCfg(t, planReviewRunner(), func(cfg *Config) {
			cfg.Flows = map[string]flow.Flow{f.Name: f}
			cfg.PlanReview = planReviewSettings("manual-default", "1", false)
		})
		id, err := e.CreateIssue("manual review", "", f.Name, levers.Matrix{
			"plan": flow.LeverRegular, "execute": flow.LeverYolo,
		}, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- e.StartIssue(context.Background(), id) }()
		pending := waitForPendingStage(t, e, "plan")
		if pending.ReviewPolicy == nil || !pending.ReviewPolicy.HumanRequired || pending.ReviewPolicy.PolicyAutoApproval {
			t.Fatalf("pending review policy = %+v", pending.ReviewPolicy)
		}
		if len(planReviewEvents(t, s, id, core.EvPlanReviewPolicyApproved)) != 0 ||
			len(planReviewEvents(t, s, id, core.EvExecutionStarted)) != 0 {
			t.Fatal("manual review emitted automatic approval or execution before response")
		}
		if err := e.Answer(pending.ID, levers.ChoiceResponse(0)); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		assertPlanApprovalEvent(t, waitForPlanReviewEvent(t, s, id, core.EvPlanReviewHumanApproved),
			core.EvPlanReviewHumanApproved, "human", "operator", "manual-default", "1")
		if got := len(planReviewEvents(t, s, id, core.EvExecutionStarted)); got != 1 {
			t.Fatalf("execution_started events = %d, want 1", got)
		}
	})

	t.Run("regular explicit auto approval", func(t *testing.T) {
		f := planReviewFlow()
		e, s := newEngineCfg(t, planReviewRunner(), func(cfg *Config) {
			cfg.Flows = map[string]flow.Flow{f.Name: f}
			cfg.PlanReview = planReviewSettings("team-ci", "2026-08-03", true)
		})
		id, err := e.CreateIssue("automatic review", "", f.Name, levers.Matrix{
			"plan": flow.LeverRegular, "execute": flow.LeverYolo,
		}, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- e.StartIssue(context.Background(), id) }()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		assertPlanApprovalEvent(t, waitForPlanReviewEvent(t, s, id, core.EvPlanReviewPolicyApproved),
			core.EvPlanReviewPolicyApproved, "policy", "", "team-ci", "2026-08-03")
		if len(e.PendingDecisions()) != 0 || len(planReviewEvents(t, s, id, core.EvPlanReviewHumanApproved)) != 0 {
			t.Fatal("automatic approval exposed a pending or human approval")
		}
		if got := len(planReviewEvents(t, s, id, core.EvExecutionStarted)); got != 1 {
			t.Fatalf("execution_started events = %d, want 1", got)
		}
		rows, err := s.AllDecisionRows()
		if err != nil {
			t.Fatal(err)
		}
		var autoReview store.DecisionRow
		for _, row := range rows {
			if row.IssueID == id && row.Stage == "plan" {
				autoReview = row
				break
			}
		}
		pagePath := filepath.Join(e.issueDir(id), "decisions", fmt.Sprintf("%d.html", autoReview.ID))
		page, err := os.ReadFile(pagePath)
		if err != nil {
			t.Fatalf("policy approval page missing: %v", err)
		}
		for _, want := range []string{
			"Automatically approved by policy team-ci@2026-08-03", `id="decision"`, "decision · plan",
			"Recorded outcome", "policy team-ci@2026-08-03 automatically selected approve",
			"Recommendation at authorization time", "Configured choices",
			"Authorization evidence", "plan.md was archived and automatically authorized by policy team-ci@2026-08-03",
			"What happened next", "Policy approval authorized Watchtower to continue to execute",
		} {
			if !strings.Contains(string(page), want) {
				t.Errorf("policy approval page missing %q: %s", want, page)
			}
		}
		for _, misleading := range []string{
			"Answered:", "Do this now", "After you answer", "ready for review",
			"choose approve or reject", "Evidence reviewed", "was archived and reviewed",
			"The reviewed artifact version",
		} {
			if strings.Contains(string(page), misleading) {
				t.Errorf("policy approval page contains misleading %q: %s", misleading, page)
			}
		}
		if strings.Contains(string(page), "decision · execute") {
			t.Errorf("policy approval page placed plan decision under execute: %s", page)
		}
		if got := strings.Count(string(page), `id="decision"`); got != 1 {
			t.Errorf("policy approval page rendered %d decision briefings, want 1: %s", got, page)
		}
	})

	t.Run("strict overrides configured auto approval", func(t *testing.T) {
		f := planReviewFlow()
		e, s := newEngineCfg(t, planReviewRunner(), func(cfg *Config) {
			cfg.Flows = map[string]flow.Flow{f.Name: f}
			cfg.PlanReview = planReviewSettings("team-ci", "2026-08-03", true)
		})
		id, err := e.CreateIssue("strict review", "", f.Name, levers.Matrix{
			"plan": flow.LeverStrict, "execute": flow.LeverYolo,
		}, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- e.StartIssue(context.Background(), id) }()
		pending := waitForPendingStage(t, e, "plan")
		if pending.ReviewPolicy == nil || !pending.ReviewPolicy.HumanRequired || pending.ReviewPolicy.PolicyAutoApproval {
			t.Fatalf("strict pending review policy = %+v", pending.ReviewPolicy)
		}
		if _, err := s.ResolveArtifactReview(pending.ID, *pending.Review, levers.ChoiceResponse(0), &review.ApprovalProvenance{
			Kind: review.ApprovalPolicy, PolicyID: "team-ci", PolicyVersion: "2026-08-03",
		}); err == nil {
			t.Fatal("strict policy auto-approval authorized execution")
		}
		if len(planReviewEvents(t, s, id, core.EvExecutionStarted)) != 0 {
			t.Fatal("strict run started execution before human approval")
		}
		if err := e.Answer(pending.ID, levers.ChoiceResponse(0)); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestInvalidPlanReviewPolicyFailsClosedToHumanReview(t *testing.T) {
	f := planReviewFlow()
	e, _ := newEngineCfg(t, planReviewRunner(), func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.PlanReview = review.PolicySettings{
			AutoApproveRegular: true, Valid: true,
		}
	})
	id, err := e.CreateIssue("invalid policy", "", f.Name, levers.Matrix{
		"plan": flow.LeverRegular, "execute": flow.LeverYolo,
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.StartIssue(context.Background(), id) }()
	pending := waitForPendingStage(t, e, "plan")
	if pending.ReviewPolicy == nil || !pending.ReviewPolicy.HumanRequired || pending.ReviewPolicy.PolicyAutoApproval {
		t.Fatalf("invalid policy review = %+v", pending.ReviewPolicy)
	}
	if err := e.Answer(pending.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPlanReviewRejectsDuplicateResponse(t *testing.T) {
	f := planReviewFlow()
	e, s := newEngineCfg(t, planReviewRunner(), func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.PlanReview = planReviewSettings("manual-default", "1", false)
	})
	id, err := e.CreateIssue("reject review", "", f.Name, levers.Matrix{
		"plan": flow.LeverRegular, "execute": flow.LeverYolo,
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.StartIssue(context.Background(), id) }()
	pending := waitForPendingStage(t, e, "plan")
	if err := e.Answer(pending.ID, levers.ChoiceResponse(1)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("rejected plan review advanced to execute")
	}
	rejected := planReviewEvents(t, s, id, core.EvPlanReviewRejected)
	if len(rejected) != 1 {
		t.Fatalf("rejection events = %d, want 1", len(rejected))
	}
	assertPlanApprovalEvent(t, rejected[0], core.EvPlanReviewRejected, "human", "operator", "manual-default", "1")
	if err := e.Answer(pending.ID, levers.ChoiceResponse(0)); err == nil {
		t.Fatal("late response was accepted")
	}
	if got := len(planReviewEvents(t, s, id, core.EvPlanReviewHumanApproved)); got != 0 {
		t.Fatalf("late response created %d approval events", got)
	}
}

func TestPlanReviewAuditFailureBlocksExecution(t *testing.T) {
	f := planReviewFlow()
	e, s := newEngineCfg(t, planReviewRunner(), func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.PlanReview = planReviewSettings("manual-default", "1", false)
	})
	id, err := e.CreateIssue("audit retry", "", f.Name, levers.Matrix{
		"plan": flow.LeverRegular, "execute": flow.LeverYolo,
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.StartIssue(context.Background(), id) }()
	pending := waitForPendingStage(t, e, "plan")
	s.FailNextArtifactReviewResolutionForTest()
	if err := e.Answer(pending.ID, levers.ChoiceResponse(0)); err == nil {
		t.Fatal("injected approval persistence failure was not returned")
	}
	if len(e.PendingDecisions()) != 1 || len(planReviewEvents(t, s, id, core.EvExecutionStarted)) != 0 {
		t.Fatal("audit failure removed the pending review or started execution")
	}
	if err := e.Answer(pending.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := len(planReviewEvents(t, s, id, core.EvExecutionStarted)); got != 1 {
		t.Fatalf("execution_started events = %d, want 1", got)
	}
}

func TestPlanReviewPageFailureDoesNotPublishRequest(t *testing.T) {
	f := planReviewFlow()
	e, s := newEngineCfg(t, planReviewRunner(), func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.PlanReview = planReviewSettings("manual-default", "1", false)
	})
	id, err := e.CreateIssue("unpublished review", "", f.Name, levers.Matrix{
		"plan": flow.LeverRegular, "execute": flow.LeverYolo,
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.FailNextDecisionPageSnapshotForTest()
	if err := e.StartIssue(context.Background(), id); err == nil ||
		!strings.Contains(err.Error(), "write plan review decision page") {
		t.Fatalf("StartIssue error = %v, want plan review page failure", err)
	}
	if pending := e.PendingDecisions(); len(pending) != 0 {
		t.Fatalf("pending decisions = %#v", pending)
	}
	if rows, err := s.PendingDecisionRows(); err != nil || len(rows) != 0 {
		t.Fatalf("pending rows = %#v, err = %v", rows, err)
	}
	for _, event := range mustEvents(t, s, id) {
		if event.Type == core.EvPlanReviewRequested || event.Type == core.EvDecisionRequired {
			t.Fatalf("unpublished plan review emitted %s", event.Type)
		}
	}
}

func TestPlanReviewDecisionEventFailureDoesNotPublishRequest(t *testing.T) {
	f := planReviewFlow()
	e, s := newEngineCfg(t, planReviewRunner(), func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.PlanReview = planReviewSettings("manual-default", "1", false)
	})
	id, err := e.CreateIssue("unpublished decision", "", f.Name, levers.Matrix{
		"plan": flow.LeverRegular, "execute": flow.LeverYolo,
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.FailNextAppendForTest(core.EvDecisionRequired)
	if err := e.StartIssue(context.Background(), id); err == nil ||
		!strings.Contains(err.Error(), "append plan review decision") {
		t.Fatalf("StartIssue error = %v, want plan review decision event failure", err)
	}
	if pending := e.PendingDecisions(); len(pending) != 0 {
		t.Fatalf("pending decisions = %#v", pending)
	}
	if rows, err := s.PendingDecisionRows(); err != nil || len(rows) != 0 {
		t.Fatalf("pending rows = %#v, err = %v", rows, err)
	}
	for _, event := range mustEvents(t, s, id) {
		if event.Type == core.EvPlanReviewRequested || event.Type == core.EvDecisionRequired {
			t.Fatalf("unpublished plan review emitted %s", event.Type)
		}
	}
}

func eventStage(t *testing.T, event core.Event) string {
	t.Helper()
	var payload struct {
		Stage string `json:"stage"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("decode %s: %v", event.Type, err)
	}
	return payload.Stage
}

func TestArtifactGateBlocksBothHandoffsUntilAccepted(t *testing.T) {
	f := artifactGateFlow()
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"spec/spec-writer":              {Artifacts: map[string]string{"spec.md": "approved spec\n"}},
		"plan/planner":                  {PlannerRequests: plannerArtifactRequests()},
		"implementation/implementation": {},
	}}
	e, s := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{"artifact-gates": f}
	})
	id, err := e.CreateIssue("artifact gates", "test both handoffs", f.Name, levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.StartIssue(context.Background(), id) }()

	spec := waitForPendingStage(t, e, "spec")
	if spec.Review == nil || spec.Review.Stage != "spec" || spec.Review.NextStage != "plan" ||
		len(spec.Review.Artifacts) != 1 || spec.Review.Artifacts[0].Name != "spec.md" || spec.Review.ArtifactVersion == "" {
		t.Fatalf("spec review = %#v", spec.Review)
	}
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == core.EvStageCompleted && eventStage(t, event) == "spec" {
			t.Fatal("spec completed before explicit acceptance")
		}
		if event.Type == core.EvStageStarted && eventStage(t, event) == "plan" {
			t.Fatal("plan started before spec acceptance")
		}
	}
	if err := e.Answer(spec.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}

	plan := waitForPendingStage(t, e, "plan")
	if plan.Review == nil || plan.Review.Stage != "plan" || plan.Review.NextStage != "implementation" ||
		plan.Review.CheckpointID == spec.Review.CheckpointID || len(plan.Review.Artifacts) != 2 {
		t.Fatalf("plan review = %#v", plan.Review)
	}
	artifactNames := map[string]bool{}
	for _, artifact := range plan.Review.Artifacts {
		artifactNames[artifact.Name] = artifact.SHA256 != ""
	}
	if !artifactNames["plan.md"] || !artifactNames["touchset.json"] {
		t.Fatalf("plan review artifacts = %+v", plan.Review.Artifacts)
	}
	events, err = s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	planRequired := int64(0)
	touchsetProduced := int64(0)
	for _, event := range events {
		if event.Type == core.EvArtifactProduced {
			var payload struct {
				Stage    string `json:"stage"`
				Artifact string `json:"artifact"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Stage == "plan" && payload.Artifact == "touchset.json" {
				touchsetProduced = event.Seq
			}
		}
		if event.Type == core.EvDecisionRequired && eventStage(t, event) == "plan" {
			planRequired = event.Seq
		}
	}
	if touchsetProduced == 0 || planRequired == 0 || touchsetProduced >= planRequired {
		t.Fatalf("plan artifact/review order = produced %d, required %d", touchsetProduced, planRequired)
	}
	if err := e.Answer(plan.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	events, err = s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var specAnswered, specCompleted, planStarted, planAnswered, planCompleted, implementationStarted int64
	for _, event := range events {
		switch event.Type {
		case core.EvDecisionAnswered:
			if event.IssueID != id {
				continue
			}
			var payload struct {
				Response levers.Response `json:"response"`
			}
			_ = json.Unmarshal(event.Payload, &payload)
			if payload.Response.Option != nil {
				if specAnswered == 0 {
					specAnswered = event.Seq
				} else {
					planAnswered = event.Seq
				}
			}
		case core.EvStageStarted:
			switch eventStage(t, event) {
			case "plan":
				planStarted = event.Seq
			case "implementation":
				implementationStarted = event.Seq
			}
		case core.EvStageCompleted:
			switch eventStage(t, event) {
			case "spec":
				specCompleted = event.Seq
			case "plan":
				planCompleted = event.Seq
			}
		}
	}
	if specAnswered == 0 || specCompleted == 0 || planStarted == 0 || planAnswered == 0 || planCompleted == 0 || implementationStarted == 0 ||
		!(specAnswered < specCompleted && specCompleted < planStarted && planAnswered < planCompleted && planCompleted < implementationStarted) {
		t.Fatalf("lifecycle order = spec answered %d, spec completed %d, plan started %d, plan answered %d, plan completed %d, implementation started %d", specAnswered, specCompleted, planStarted, planAnswered, planCompleted, implementationStarted)
	}
}

func TestArtifactGateIgnoresYoloAndRecommendedAnswer(t *testing.T) {
	f := artifactGateFlow()
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"spec/spec-writer":              {Artifacts: map[string]string{"spec.md": "spec\n"}},
		"plan/planner":                  {PlannerRequests: plannerArtifactRequests()},
		"implementation/implementation": {},
	}}
	e, s := newEngineCfg(t, r, func(cfg *Config) { cfg.Flows = map[string]flow.Flow{"artifact-gates": f} })
	id, err := e.CreateIssue("explicit review", "", f.Name, levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.StartIssue(context.Background(), id) }()
	pending := waitForPendingStage(t, e, "spec")
	if pending.D.Recommended != 0 || pending.Review == nil {
		t.Fatalf("pending artifact review = %#v", pending)
	}
	time.Sleep(100 * time.Millisecond)
	rows, err := s.PendingDecisionRows()
	if err != nil || len(rows) != 1 || rows[0].ID != pending.ID {
		t.Fatalf("yolo resolved artifact review: rows=%+v err=%v", rows, err)
	}
	if err := e.Answer(pending.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	plan := waitForPendingStage(t, e, "plan")
	if err := e.Answer(plan.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAutoStageStillCompletesWithoutReview(t *testing.T) {
	f := flow.Flow{Name: "auto", Stages: []flow.Stage{{
		Name: "implementation", Agents: []flow.AgentRef{{Package: "implementation"}}, Workspace: "none",
		Completion: flow.CompletionAll, Gate: flow.GateAuto,
	}}}
	e, s := newEngineCfg(t, &runner.FakeRunner{Scripts: map[string]runner.Script{"implementation/implementation": {}}}, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{"auto": f}
	})
	id, err := e.CreateIssue("auto stage", "", f.Name, levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if len(e.PendingDecisions()) != 0 {
		t.Fatal("auto stage created an approval decision")
	}
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	completed := false
	for _, event := range events {
		if event.Type == core.EvStageCompleted && eventStage(t, event) == "implementation" {
			completed = true
		}
	}
	if !completed {
		t.Fatal("auto stage did not complete")
	}
}

func TestArtifactGateRejectsMissingArtifactBeforeReview(t *testing.T) {
	f := flow.Flow{Name: "missing", Stages: []flow.Stage{{
		Name: "spec", Agents: []flow.AgentRef{{Package: "spec-writer"}}, Workspace: "none",
		Completion: flow.CompletionAll, Gate: flow.GateApproveArtifact, Artifacts: []string{"spec.md"},
	}}}
	e, s := newEngineCfg(t, &runner.FakeRunner{Scripts: map[string]runner.Script{"spec/spec-writer": {}}}, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{"missing": f}
	})
	id, err := e.CreateIssue("missing artifact", "", f.Name, levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = e.StartIssue(context.Background(), id)
	if err == nil || !strings.Contains(err.Error(), "missing artifact spec.md") {
		t.Fatalf("StartIssue error = %v", err)
	}
	if len(e.PendingDecisions()) != 0 {
		t.Fatal("missing artifact created a pending review")
	}
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == core.EvDecisionRequired || event.Type == core.EvStageCompleted {
			t.Fatalf("missing artifact emitted %s", event.Type)
		}
	}
}

func TestDecisionContextIsFrozenAndTrustedAcrossAgentDecisions(t *testing.T) {
	f := flow.Flow{Name: "context", Stages: []flow.Stage{{
		Name: "ask", Completion: flow.CompletionAll, Workspace: "none", Gate: flow.GateAuto,
		Agents: []flow.AgentRef{{Package: "agent-a"}, {Package: "agent-b"}},
	}}}
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"ask/agent-a": {Asks: []levers.Decision{
			{Kind: levers.DecisionChoice, Question: "Choose?", Options: []string{"yes", "no"}, Recommended: 0, Importance: 0.1},
			{Kind: levers.DecisionFreeform, Question: "Explain?", RecommendedResponse: "Because.", Importance: 1.0},
		}},
		"ask/agent-b": {Asks: []levers.Decision{
			{Kind: levers.DecisionChoice, Question: "Again?", Options: []string{"yes", "no"}, Recommended: 0, Importance: 0.1},
		}},
	}}
	e, _ := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{"context": f}
		cfg.DecisionIdentities = map[string]decision.AgentIdentity{
			"agent-a": {Name: "First Agent", Color: "violet", Symbol: "α"},
			"agent-b": {Name: "Second Agent", Color: "orange", Symbol: "β"},
		}
	})
	var autoContexts []decision.DecisionContext
	e.cfg.Observers = append(e.cfg.Observers, func(event core.Event) {
		if event.Type != core.EvDecisionAutoResolved {
			return
		}
		var payload struct {
			Context decision.DecisionContext `json:"context"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Errorf("decode auto decision: %v", err)
			return
		}
		autoContexts = append(autoContexts, payload.Context)
	})
	id, err := e.CreateIssue("  Make   decisions self-identifying! Extra title.", "Keep the full identity visible. Extra body.", "context", levers.Matrix{"ask": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.StartIssue(context.Background(), id) }()
	var pending PendingDecision
	deadline := time.After(5 * time.Second)
	for {
		if decisions := e.PendingDecisions(); len(decisions) == 1 {
			pending = decisions[0]
			break
		}
		select {
		case <-deadline:
			t.Fatal("trusted context decision never appeared")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if pending.Context == nil {
		t.Fatal("pending decision has no context")
	}
	if pending.Context.TaskSummary != "Make decisions self-identifying: Keep the full identity visible." ||
		pending.Context.AgentName != "First Agent" || pending.Context.AgentColor != "violet" || pending.Context.AgentSymbol != "α" {
		t.Fatalf("pending context = %#v", pending.Context)
	}
	if err := e.Answer(pending.ID, levers.FreeformResponse("Because.")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(autoContexts) != 2 {
		t.Fatalf("auto decision contexts = %d, want 2", len(autoContexts))
	}
	wantSummary := "Make decisions self-identifying: Keep the full identity visible."
	for _, ctx := range autoContexts {
		if ctx.TaskSummary != wantSummary {
			t.Fatalf("auto summary = %q, want %q", ctx.TaskSummary, wantSummary)
		}
	}
	if autoContexts[0].AgentName != "First Agent" || autoContexts[0].AgentColor != "violet" || autoContexts[0].AgentSymbol != "α" ||
		autoContexts[1].AgentName != "Second Agent" || autoContexts[1].AgentColor != "orange" || autoContexts[1].AgentSymbol != "β" {
		t.Fatalf("auto identities = %#v, %#v", autoContexts[0], autoContexts[1])
	}
}

func TestDecisionContextFailureStopsBeforePresentation(t *testing.T) {
	f := flow.Flow{Name: "context", Stages: []flow.Stage{{
		Name: "ask", Completion: flow.CompletionAll, Workspace: "none", Gate: flow.GateAuto,
		Agents: []flow.AgentRef{{Package: "agent-a"}},
	}}}
	for _, tc := range []struct {
		name       string
		identities map[string]decision.AgentIdentity
		title      string
		want       string
	}{
		{name: "missing identity", identities: map[string]decision.AgentIdentity{}, title: "A task", want: "agent-a"},
		{name: "malformed identity", identities: map[string]decision.AgentIdentity{
			"agent-a": {Name: "Agent", Color: "#00ff00", Symbol: "A"},
		}, title: "A task", want: "agent_color"},
		{name: "over budget context", identities: map[string]decision.AgentIdentity{
			"agent-a": {Name: "Agent", Color: "green", Symbol: "A"},
		}, title: strings.Repeat("x", decision.MaxMessageBytes+1), want: "message budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &runner.FakeRunner{Scripts: map[string]runner.Script{
				"ask/agent-a": {Asks: []levers.Decision{{Question: "Proceed?", Options: []string{"yes"}, Recommended: 0, Importance: 0.1}}},
			}}
			e, s := newEngineCfg(t, r, func(cfg *Config) {
				cfg.Flows = map[string]flow.Flow{"context": f}
				cfg.DecisionIdentities = tc.identities
			})
			id, err := e.CreateIssue(tc.title, "", "context", levers.Matrix{"ask": flow.LeverYolo}, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := e.StartIssue(context.Background(), id); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("StartIssue error = %v, want %q", err, tc.want)
			}
			if got := e.PendingDecisions(); len(got) != 0 {
				t.Fatalf("pending decisions = %#v", got)
			}
			events, err := s.EventsSince(0)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Type == core.EvDecisionRequired {
					t.Fatal("invalid context was presented")
				}
			}
		})
	}
}

func TestDecisionRequiredObserverCanAnswerImmediately(t *testing.T) {
	f := flow.Flow{Name: "observer-answer", Stages: []flow.Stage{{
		Name: "ask", Completion: flow.CompletionAll, Workspace: "none", Gate: flow.GateAuto,
		Agents: []flow.AgentRef{{Package: "agent"}},
	}}}
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"ask/agent": {Asks: []levers.Decision{{
			Question: "Proceed?", Options: []string{"yes", "no"}, Recommended: 0, Importance: 1.0,
			Why: "The stage needs authorization.", Consequences: []string{"Continue.", "Stop."},
			Reversible: "The answer can be changed on retry.",
		}}},
	}}
	s, err := store.Open("file:decision-observer-answer?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	answerResult := make(chan error, 1)
	var e *Engine
	e = New(Config{
		Store: s, Runner: r, Pool: slots.NewPool(1), DataDir: t.TempDir(),
		Flows: map[string]flow.Flow{f.Name: f}, DecisionIdentities: testDecisionIdentities(),
		Observers: []func(core.Event){func(event core.Event) {
			if event.Type != core.EvDecisionRequired {
				return
			}
			var payload struct {
				DecisionID int64 `json:"decision_id"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				answerResult <- err
				return
			}
			answerResult <- e.Answer(payload.DecisionID, levers.ChoiceResponse(0))
		}},
	})
	id, err := e.CreateIssue("observer answer", "", f.Name, levers.Matrix{"ask": flow.LeverStrict}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.StartIssue(context.Background(), id) }()
	if err := <-answerResult; err != nil {
		if pending := e.PendingDecisions(); len(pending) == 1 {
			_ = e.Answer(pending[0].ID, levers.ChoiceResponse(0))
		}
		t.Fatalf("observer answer failed: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCancellationDuringDecisionPagePublicationDoesNotPublishDecision(t *testing.T) {
	tests := []struct {
		name    string
		flow    flow.Flow
		runner  runner.Runner
		matrix  levers.Matrix
		adjust  func(*Config)
		abandon bool
	}{
		{
			name: "decision",
			flow: flow.Flow{Name: "cancel-decision-publication", Stages: []flow.Stage{{
				Name: "ask", Completion: flow.CompletionAll, Workspace: "none", Gate: flow.GateAuto,
				Agents: []flow.AgentRef{{Package: "agent"}},
			}}},
			runner: &runner.FakeRunner{Scripts: map[string]runner.Script{
				"ask/agent": {Asks: []levers.Decision{{
					Question: "Proceed?", Options: []string{"yes", "no"}, Recommended: 0, Importance: 1.0,
					Why: "The stage needs authorization.", Consequences: []string{"Continue.", "Stop."},
					Reversible: "The answer can be changed on retry.",
				}}},
			}},
			matrix: levers.Matrix{"ask": flow.LeverStrict},
		},
		{
			name: "abandoned decision",
			flow: flow.Flow{Name: "abandon-decision-publication", Stages: []flow.Stage{{
				Name: "ask", Completion: flow.CompletionAll, Workspace: "none", Gate: flow.GateAuto,
				Agents: []flow.AgentRef{{Package: "agent"}},
			}}},
			runner: &runner.FakeRunner{Scripts: map[string]runner.Script{
				"ask/agent": {Asks: []levers.Decision{{
					Question: "Proceed?", Options: []string{"yes", "no"}, Recommended: 0, Importance: 1.0,
					Why: "The stage needs authorization.", Consequences: []string{"Continue.", "Stop."},
					Reversible: "The answer can be changed on retry.",
				}}},
			}},
			matrix:  levers.Matrix{"ask": flow.LeverStrict},
			abandon: true,
		},
		{
			name: "artifact review",
			flow: flow.Flow{Name: "cancel-artifact-review-publication", Stages: []flow.Stage{{
				Name: "review", Completion: flow.CompletionAll, Workspace: "none", Gate: flow.GateApproveArtifact,
				Agents: []flow.AgentRef{{Package: "agent"}}, Artifacts: []string{"spec.md"},
			}}},
			runner: &runner.FakeRunner{Scripts: map[string]runner.Script{
				"review/agent": {Artifacts: map[string]string{"spec.md": "spec\n"}},
			}},
			matrix: levers.Matrix{"review": flow.LeverRegular},
		},
		{
			name: "plan review",
			flow: flow.Flow{Name: "cancel-plan-review-publication", Stages: []flow.Stage{{
				Name: "review", Completion: flow.CompletionAll, Workspace: "none", Gate: flow.GatePlanReview,
				Agents: []flow.AgentRef{{Package: "agent"}}, Artifacts: []string{"plan.md"},
			}}},
			runner: &runner.FakeRunner{Scripts: map[string]runner.Script{
				"review/agent": {Artifacts: map[string]string{"plan.md": "plan\n"}},
			}},
			matrix: levers.Matrix{"review": flow.LeverRegular},
			adjust: func(cfg *Config) {
				cfg.PlanReview = planReviewSettings("manual-default", "1", false)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var e *Engine
			var issueID string
			var once sync.Once
			killResult := make(chan error, 1)
			e, s := newEngineCfg(t, test.runner, func(cfg *Config) {
				cfg.Flows = map[string]flow.Flow{test.flow.Name: test.flow}
				if test.adjust != nil {
					test.adjust(cfg)
				}
				cfg.Observers = []func(core.Event){func(event core.Event) {
					if event.Type != core.EvArtifactProduced {
						return
					}
					var payload struct {
						Artifact string `json:"artifact"`
					}
					if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.Artifact != decisionpage.FileName {
						return
					}
					once.Do(func() {
						if test.abandon {
							killResult <- e.Abandon(issueID)
							return
						}
						killResult <- e.KillStage(issueID)
					})
				}}
			})
			var err error
			issueID, err = e.CreateIssue("cancel publication", "", test.flow.Name, test.matrix, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- e.StartIssue(ctx, issueID) }()

			select {
			case err := <-killResult:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("decision page was not published")
			}

			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("StartIssue error = %v, want context canceled", err)
				}
			case <-time.After(5 * time.Second):
				cancel()
				<-done
				t.Fatal("canceled publication left the stage waiting on a decision")
			}
			if pending := e.PendingDecisions(); len(pending) != 0 {
				t.Fatalf("pending decisions = %#v", pending)
			}
			if rows, err := s.PendingDecisionRows(); err != nil || len(rows) != 0 {
				t.Fatalf("pending rows = %#v, err = %v", rows, err)
			}
			for _, event := range mustEvents(t, s, issueID) {
				if event.Type == core.EvDecisionRequired || event.Type == core.EvPlanReviewRequested {
					t.Fatalf("canceled publication emitted %s", event.Type)
				}
			}
			stable, err := os.ReadFile(filepath.Join(e.issueDir(issueID), decisionpage.FileName))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if strings.Contains(string(stable), "Do this now") {
				t.Fatalf("stable page advertises the canceled decision: %s", stable)
			}
		})
	}
}

func TestResolvedDecisionArchiveFailureDoesNotFailAnswer(t *testing.T) {
	tests := []struct {
		name   string
		flow   flow.Flow
		runner runner.Runner
		matrix levers.Matrix
	}{
		{
			name: "decision",
			flow: flow.Flow{Name: "answer-without-archive", Stages: []flow.Stage{{
				Name: "ask", Completion: flow.CompletionAll, Workspace: "none", Gate: flow.GateAuto,
				Agents: []flow.AgentRef{{Package: "agent"}},
			}}},
			runner: &runner.FakeRunner{Scripts: map[string]runner.Script{
				"ask/agent": {Asks: []levers.Decision{{
					Question: "Proceed?", Options: []string{"yes", "no"}, Recommended: 0, Importance: 1.0,
					Why: "The stage needs authorization.", Consequences: []string{"Continue.", "Stop."},
					Reversible: "The answer can be changed on retry.",
				}}},
			}},
			matrix: levers.Matrix{"ask": flow.LeverStrict},
		},
		{
			name: "artifact review",
			flow: flow.Flow{Name: "review-without-archive", Stages: []flow.Stage{{
				Name: "review", Completion: flow.CompletionAll, Workspace: "none", Gate: flow.GateApproveArtifact,
				Agents: []flow.AgentRef{{Package: "agent"}}, Artifacts: []string{"spec.md"},
			}}},
			runner: &runner.FakeRunner{Scripts: map[string]runner.Script{
				"review/agent": {Artifacts: map[string]string{"spec.md": "spec\n"}},
			}},
			matrix: levers.Matrix{"review": flow.LeverRegular},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e, s := newEngineCfg(t, test.runner, func(cfg *Config) {
				cfg.Flows = map[string]flow.Flow{test.flow.Name: test.flow}
			})
			issueID, err := e.CreateIssue("answer despite archive failure", "", test.flow.Name, test.matrix, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- e.StartIssue(context.Background(), issueID) }()
			pending := waitForPendingStage(t, e, test.flow.Stages[0].Name)
			decisionsDir := filepath.Join(e.issueDir(issueID), "decisions")
			if err := os.RemoveAll(decisionsDir); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(decisionsDir, []byte("block archive writes"), 0o644); err != nil {
				t.Fatal(err)
			}

			if err := e.Answer(pending.ID, levers.ChoiceResponse(0)); err != nil {
				t.Fatalf("Answer returned a post-commit archive failure: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			row, ok, err := s.DecisionByID(pending.ID)
			if err != nil || !ok || row.Status != "answered" {
				t.Fatalf("resolved row = %+v, ok = %v, err = %v", row, ok, err)
			}
			if got := len(planReviewEvents(t, s, issueID, core.EvDecisionAnswered)); got != 1 {
				t.Fatalf("decision_answered events = %d, want 1", got)
			}
		})
	}
}

func TestDecisionSnapshotFailureDoesNotPublishDecision(t *testing.T) {
	f := flow.Flow{Name: "snapshot-failure", Stages: []flow.Stage{{
		Name: "ask", Completion: flow.CompletionAll, Workspace: "none", Gate: flow.GateAuto,
		Agents: []flow.AgentRef{{Package: "agent"}},
	}}}
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"ask/agent": {Asks: []levers.Decision{{
			Question: "Proceed?", Options: []string{"yes", "no"}, Recommended: 0, Importance: 1.0,
			Why: "The stage needs authorization.", Consequences: []string{"Continue.", "Stop."},
			Reversible: "The answer can be changed on retry.",
		}}},
	}}
	e, s := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
	})
	id, err := e.CreateIssue("snapshot failure", "", f.Name, levers.Matrix{"ask": flow.LeverStrict}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.FailNextDecisionPageSnapshotForTest()
	done := make(chan error, 1)
	go func() { done <- e.StartIssue(context.Background(), id) }()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case runErr := <-done:
			if runErr == nil || !strings.Contains(runErr.Error(), "decision page snapshot") {
				t.Fatalf("StartIssue error = %v, want decision page snapshot failure", runErr)
			}
			assertDecisionWasNotPublished(t, e, s, id, "Proceed?")
			return
		case <-deadline.C:
			t.Fatal("StartIssue blocked after decision page snapshot failure")
		default:
			pending := e.PendingDecisions()
			if len(pending) == 0 {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			_ = e.Answer(pending[0].ID, levers.ChoiceResponse(0))
			t.Fatal("decision became answerable without a durable page snapshot")
		}
	}
}

func TestDecisionEventFailureRollsBackPublishedPages(t *testing.T) {
	f := flow.Flow{Name: "event-failure", Stages: []flow.Stage{{
		Name: "ask", Completion: flow.CompletionAll, Workspace: "none", Gate: flow.GateAuto,
		Agents: []flow.AgentRef{{Package: "agent"}},
	}}}
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"ask/agent": {Asks: []levers.Decision{{
			Question: "Proceed?", Options: []string{"yes", "no"}, Recommended: 0, Importance: 1.0,
			Why: "The stage needs authorization.", Consequences: []string{"Continue.", "Stop."},
			Reversible: "The answer can be changed on retry.",
		}}},
	}}
	e, s := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
	})
	id, err := e.CreateIssue("event failure", "", f.Name, levers.Matrix{"ask": flow.LeverStrict}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.FailNextAppendForTest(core.EvDecisionRequired)
	if err := e.StartIssue(context.Background(), id); err == nil || !strings.Contains(err.Error(), "append decision event") {
		t.Fatalf("StartIssue error = %v, want decision event failure", err)
	}
	assertDecisionWasNotPublished(t, e, s, id, "Proceed?")
}

func assertDecisionWasNotPublished(
	t *testing.T, e *Engine, s *store.Store, issueID, question string,
) {
	t.Helper()
	if pending := e.PendingDecisions(); len(pending) != 0 {
		t.Fatalf("pending decisions = %#v", pending)
	}
	if rows, err := s.PendingDecisionRows(); err != nil || len(rows) != 0 {
		t.Fatalf("pending rows = %#v, err = %v", rows, err)
	}
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == core.EvDecisionRequired {
			t.Fatalf("decision_required event remained: %+v", event)
		}
	}
	archives, err := filepath.Glob(filepath.Join(e.cfg.DataDir, issueID, "decisions", "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 0 {
		t.Fatalf("decision archives remained: %v", archives)
	}
	stable, err := os.ReadFile(filepath.Join(e.cfg.DataDir, issueID, decisionpage.FileName))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if strings.Contains(string(stable), question) || strings.Contains(string(stable), "Do this now") {
		t.Fatalf("stable page still advertises rolled-back decision: %s", stable)
	}
}

func TestContextSurvivesAutoAndPendingRows(t *testing.T) {
	f := flow.Flow{Name: "context", Stages: []flow.Stage{{
		Name: "ask", Completion: flow.CompletionAll, Workspace: "none", Gate: flow.GateAuto,
		Agents: []flow.AgentRef{{Package: "agent"}},
	}}}
	title := "A long but safe task " + strings.Repeat("detail ", 500)
	fr := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"ask/agent": {Asks: []levers.Decision{
			{Question: "Auto?", Options: []string{"yes"}, Recommended: 0, Importance: 0.1},
			{Question: "Human?", Options: []string{"yes"}, Recommended: 0, Importance: 1.0},
		}},
	}}
	e, s := newEngineCfg(t, fr, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{"context": f}
	})
	id, err := e.CreateIssue(title, "", "context", levers.Matrix{"ask": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantSummary, err := decision.BuildTaskSummary(title, "")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.StartIssue(context.Background(), id) }()
	var pending PendingDecision
	deadline := time.After(5 * time.Second)
	for {
		if decisions := e.PendingDecisions(); len(decisions) == 1 {
			pending = decisions[0]
			break
		}
		select {
		case <-deadline:
			t.Fatal("human decision never appeared")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if pending.Context == nil || pending.Context.TaskSummary != wantSummary {
		t.Fatalf("pending context = %#v", pending.Context)
	}
	if err := e.Answer(pending.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	rows, err := s.AllDecisionRows()
	if err != nil || len(rows) != 2 {
		t.Fatalf("decision rows = %#v, err = %v", rows, err)
	}
	for _, row := range rows {
		if row.Context == nil || row.Context.TaskSummary != wantSummary ||
			row.Context.AgentName != "Test Agent" || row.Context.AgentColor != "gray" || row.Context.AgentSymbol != "A" {
			t.Fatalf("stored context for %s = %#v", row.Question, row.Context)
		}
	}
}

func TestOverLimitContextFailsBeforePresentation(t *testing.T) {
	f := flow.Flow{Name: "context", Stages: []flow.Stage{{
		Name: "ask", Completion: flow.CompletionAll, Workspace: "none", Gate: flow.GateAuto,
		Agents: []flow.AgentRef{{Package: "agent"}},
	}}}
	e, s := newEngineCfg(t, &runner.FakeRunner{Scripts: map[string]runner.Script{
		"ask/agent": {Asks: []levers.Decision{{Question: "Proceed?", Options: []string{"yes"}, Recommended: 0, Importance: 0.1}}},
	}}, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{"context": f}
	})
	id, err := e.CreateIssue(strings.Repeat("x", decision.MaxMessageBytes+1), "", "context", levers.Matrix{"ask": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err == nil || !strings.Contains(err.Error(), "message budget") {
		t.Fatalf("StartIssue error = %v", err)
	}
	if rows, err := s.PendingDecisionRows(); err != nil || len(rows) != 0 {
		t.Fatalf("pending rows = %#v, err = %v", rows, err)
	}
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == core.EvDecisionRequired {
			t.Fatal("over-limit context emitted a required decision")
		}
	}
	failed := false
	for _, event := range events {
		if event.Type != core.EvStageFailed {
			continue
		}
		var payload map[string]any
		_ = json.Unmarshal(event.Payload, &payload)
		if strings.Contains(payload["error"].(string), "message budget") {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("no actionable stage failure in events: %+v", events)
	}
}

// YOLO everywhere: brainstorm ask auto-resolves, spec approve_artifact gate
// still escalates (importance 1.0 floor), so exactly one human decision.
func TestYoloRunEscalatesOnlyGate(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, err := e.CreateIssue("test", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	errc := make(chan error, 1)
	go func() { errc <- e.StartIssue(context.Background(), id) }()

	// wait for the spec gate decision to appear
	var pd PendingDecision
	deadline := time.After(5 * time.Second)
	for {
		if ds := e.PendingDecisions(); len(ds) == 1 {
			pd = ds[0]
			break
		}
		select {
		case <-deadline:
			t.Fatal("gate decision never appeared")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if pd.Stage != "spec" {
		t.Fatalf("expected spec gate, got %+v", pd)
	}
	if err := e.Answer(pd.ID, levers.ChoiceResponse(0)); err != nil { // approve
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}

	evs, _ := s.EventsSince(0)
	var auto, required, completed, issueDone int
	for _, ev := range evs {
		switch ev.Type {
		case core.EvDecisionAutoResolved:
			auto++
		case core.EvDecisionRequired:
			required++
		case core.EvStageCompleted:
			completed++
		case core.EvIssueCompleted:
			issueDone++
		}
	}
	if auto != 1 || required != 1 || completed != 4 || issueDone != 1 {
		t.Fatalf("auto=%d required=%d completed=%d issueDone=%d", auto, required, completed, issueDone)
	}
}

func TestInvalidResponseLeavesDecisionPending(t *testing.T) {
	e, _ := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, err := e.CreateIssue("test", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- e.StartIssue(context.Background(), id) }()

	var pending PendingDecision
	deadline := time.After(5 * time.Second)
	for pending.ID == 0 {
		if decisions := e.PendingDecisions(); len(decisions) == 1 {
			pending = decisions[0]
			break
		}
		select {
		case <-deadline:
			t.Fatal("decision never appeared")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := e.Answer(pending.ID, levers.FreeformResponse("")); err == nil {
		t.Fatal("empty freeform response was accepted for a choice decision")
	}
	if decisions := e.PendingDecisions(); len(decisions) != 1 || decisions[0].ID != pending.ID {
		t.Fatalf("invalid answer consumed pending decision: %#v", decisions)
	}
	if err := e.Answer(pending.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestChoiceNoteAnswerResolvesDecision(t *testing.T) {
	f := flow.Flow{Name: "default", Stages: []flow.Stage{{
		Name: "run", Agents: []flow.AgentRef{{Package: "agent"}},
		Gate: flow.GateAuto, Completion: flow.CompletionAll, Workspace: "none",
	}}}
	responses := make(chan levers.Response, 1)
	fr := &runner.FakeRunner{
		Scripts: map[string]runner.Script{
			"run/agent": {Asks: []levers.Decision{{
				Kind: levers.DecisionChoice, Question: "Proceed?",
				Options: []string{"approve", "hold"}, Recommended: 0,
				AllowFreeform: false, Importance: 1.0,
			}}},
		},
		OnResponse: func(_ string, _ string, response levers.Response) { responses <- response },
	}
	e, s := newEngineCfg(t, fr, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{"default": f}
	})
	id, err := e.CreateIssue("choice note", "", "default", levers.Preset(f, flow.LeverStrict), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	errC := make(chan error, 1)
	go func() { errC <- e.StartIssue(context.Background(), id) }()

	var pending PendingDecision
	deadline := time.After(5 * time.Second)
	for pending.ID == 0 {
		if decisions := e.PendingDecisions(); len(decisions) == 1 {
			pending = decisions[0]
			break
		}
		select {
		case <-deadline:
			t.Fatal("choice decision never appeared")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := e.Answer(pending.ID, levers.ChoiceResponse(len(pending.D.Options))); err == nil {
		t.Fatal("invalid option was accepted")
	}
	if err := e.Answer(pending.ID, levers.FreeformResponse("")); err == nil {
		t.Fatal("empty note was accepted")
	}
	if decisions := e.PendingDecisions(); len(decisions) != 1 || decisions[0].ID != pending.ID {
		t.Fatalf("invalid answers consumed pending decision: %#v", decisions)
	}

	const note = "Clarify the rollout before approval."
	if err := e.Answer(pending.ID, levers.FreeformResponse(note)); err != nil {
		t.Fatal(err)
	}
	if err := e.Answer(pending.ID, levers.FreeformResponse("second answer")); err == nil {
		t.Fatal("answered decision accepted a second response")
	}
	if err := <-errC; err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-responses:
		if got.Kind != levers.DecisionFreeform || got.Text != note {
			t.Fatalf("runner response = %#v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not receive note")
	}
	if decisions := e.PendingDecisions(); len(decisions) != 0 {
		t.Fatalf("decision remains pending: %#v", decisions)
	}
	rows, err := s.AllDecisionRows()
	if err != nil || len(rows) != 1 {
		t.Fatalf("decision rows = %#v, err = %v", rows, err)
	}
	if rows[0].Status != "answered" || rows[0].Response.Kind != levers.DecisionFreeform || rows[0].Response.Text != note {
		t.Fatalf("stored answer = %#v", rows[0])
	}
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	answered := 0
	for _, event := range events {
		if event.Type == core.EvDecisionAnswered {
			answered++
		}
	}
	if answered != 1 {
		t.Fatalf("decision_answered events = %d, want 1", answered)
	}
}

// The pause gate parks a lane before a stage; the event has to name it so the
// grid can mark one cell instead of the whole column.
func TestPausedEventNamesUpcomingStage(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, _ := e.CreateIssue("p", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
	if err := e.Pause(id); err != nil {
		t.Fatal(err)
	}
	go e.StartIssue(context.Background(), id)
	deadline := time.After(5 * time.Second)
	for {
		evs, _ := s.EventsSince(0)
		for _, event := range evs {
			if event.Type != core.EvIssuePaused {
				continue
			}
			var p map[string]any
			if err := json.Unmarshal(event.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p["stage"] != "brainstorm" {
				t.Fatalf("paused payload stage = %v, want brainstorm", p["stage"])
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("no issue_paused event")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestFailedAgentRetriesThenFails(t *testing.T) {
	sc := scripts()
	sc["execute/executor"] = runner.Script{Fail: true}
	e, s := newEngine(t, &runner.FakeRunner{Scripts: sc})
	id, _ := e.CreateIssue("boom", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)

	errc := make(chan error, 1)
	go func() { errc <- e.StartIssue(context.Background(), id) }()
	for {
		ds := e.PendingDecisions()
		if len(ds) == 1 {
			e.Answer(ds[0].ID, levers.ChoiceResponse(0))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-errc; err == nil {
		t.Fatal("expected issue failure")
	}
	evs, _ := s.EventsSince(0)
	var started, finalFailed int
	var attempts []int
	for _, ev := range evs {
		if ev.Type == core.EvStageStarted {
			started++
			var p map[string]any
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p["stage"] == "execute" {
				attempts = append(attempts, int(p["attempt"].(float64)))
			}
		}
		if ev.Type == core.EvStageFailed {
			var p map[string]any
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p["final"] == true {
				finalFailed++
				if p["attempt"] != float64(2) || p["of"] != float64(2) {
					t.Fatalf("terminal failure payload: %v", p)
				}
			}
		}
	}
	// brainstorm + spec + execute attempt1 + execute retry = 4 starts, 1 terminal fail
	if started != 4 || finalFailed != 1 || len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
		t.Fatalf("started=%d finalFailed=%d attempts=%v", started, finalFailed, attempts)
	}
}

func TestEngineRecordsStageRuns(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, _ := e.CreateIssue("t", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
	errC := make(chan error, 1)
	go func() { errC <- e.StartIssue(context.Background(), id) }()
	for {
		if ds := e.PendingDecisions(); len(ds) == 1 {
			e.Answer(ds[0].ID, levers.ChoiceResponse(0))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-errC; err != nil {
		t.Fatal(err)
	}
	runs, err := s.StageRuns(id)
	if err != nil {
		t.Fatal(err)
	}
	// brainstorm(1) + spec(1) + execute(1) + review(3 agents) = 6 rows
	if len(runs) != 6 {
		t.Fatalf("want 6 stage runs, got %d: %+v", len(runs), runs)
	}
	for _, r := range runs {
		if r.Status != "succeeded" {
			t.Fatalf("unfinished run: %+v", r)
		}
	}
}

type fakeWS struct {
	dir      string
	acquired int
	released int
}

func (f *fakeWS) Acquire(issueID string) (string, func() error, error) {
	f.acquired++
	return f.dir, func() error { f.released++; return nil }, nil
}

func (f *fakeWS) Name() string { return "fake" }

type existingBranchWS struct {
	repo string
	root string
}

func (w *existingBranchWS) Acquire(issueID string) (string, func() error, error) {
	path := filepath.Join(w.root, issueID)
	out, err := exec.Command("git", "-C", w.repo, "worktree", "add", path, "issue/"+issueID).CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("worktree add: %v: %s", err, out)
	}
	release := func() error {
		out, err := exec.Command("git", "-C", w.repo, "worktree", "remove", "--force", path).CombinedOutput()
		if err != nil {
			return fmt.Errorf("worktree remove: %v: %s", err, out)
		}
		return nil
	}
	return path, release, nil
}

func (w *existingBranchWS) Name() string { return "existing branch" }

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	runGit := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "t@t")
	runGit("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "diff"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-qm", "base")
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return string(out)
}

func TestWorktreeAcquiredOnceAndReleased(t *testing.T) {
	ws := &fakeWS{dir: t.TempDir()}
	initGitRepo(t, ws.dir)
	sc := scripts()
	sc["execute/executor"] = runner.Script{Artifacts: map[string]string{"diff": "changed\n"}}
	e, s := newEngine(t, &runner.FakeRunner{Scripts: sc})
	e.cfg.Workspace = ws
	id, _ := e.CreateIssue("w", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
	errC := make(chan error, 1)
	go func() { errC <- e.StartIssue(context.Background(), id) }()
	for {
		if ds := e.PendingDecisions(); len(ds) == 1 {
			e.Answer(ds[0].ID, levers.ChoiceResponse(0))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-errC; err != nil {
		t.Fatal(err)
	}
	// The worktree is acquired once across both stages. Its produced artifacts
	// are changed work, so a zero-barrier flow preserves rather than releases it.
	if ws.acquired != 1 || ws.released != 0 {
		t.Fatalf("acquired=%d released=%d", ws.acquired, ws.released)
	}
	evs, _ := s.EventsSince(0)
	artifacts := map[string]bool{}
	for _, ev := range evs {
		if ev.Type != core.EvArtifactProduced {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if artifact, ok := payload["artifact"].(string); ok {
			artifacts[artifact] = true
		}
	}
	if !artifacts["evidence.json"] || !artifacts["diff.patch"] {
		t.Fatalf("evidence artifacts missing: %v", artifacts)
	}
}

func TestPauseGatesBetweenStages(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, _ := e.CreateIssue("p", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
	if err := e.Pause(id); err != nil {
		t.Fatal(err)
	}
	errC := make(chan error, 1)
	go func() { errC <- e.StartIssue(context.Background(), id) }()
	// paused before the first stage: no stage_started should appear
	time.Sleep(50 * time.Millisecond)
	evs, _ := s.EventsSince(0)
	for _, ev := range evs {
		if ev.Type == core.EvStageStarted {
			t.Fatal("stage started while paused")
		}
	}
	if err := e.Resume(id); err != nil {
		t.Fatal(err)
	}
	for {
		if ds := e.PendingDecisions(); len(ds) == 1 {
			if err := e.Answer(ds[0].ID, levers.ChoiceResponse(0)); err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-errC; err != nil {
		t.Fatal(err)
	}
	evs, _ = s.EventsSince(0)
	var paused, resumed int
	for _, ev := range evs {
		if ev.Type == core.EvIssuePaused {
			paused++
		}
		if ev.Type == core.EvIssueResumed {
			resumed++
		}
	}
	if paused != 1 || resumed != 1 {
		t.Fatalf("paused=%d resumed=%d", paused, resumed)
	}
}

func TestKillStageEmitsKilledAndPauses(t *testing.T) {
	sc := scripts()
	sc["brainstorm/brainstorm"] = runner.Script{Asks: []levers.Decision{
		{Question: "block forever?", Options: []string{"a"}, Recommended: 0, Importance: 1.0}}}
	e, s := newEngine(t, &runner.FakeRunner{Scripts: sc})
	id, _ := e.CreateIssue("k", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
	errC := make(chan error, 1)
	go func() { errC <- e.StartIssue(context.Background(), id) }()
	// wait until the stage is genuinely running (blocked on its ask)
	deadline := time.After(5 * time.Second)
	for len(e.PendingDecisions()) == 0 {
		select {
		case <-deadline:
			t.Fatal("decision never appeared")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := e.KillStage(id); err != nil {
		t.Fatal(err)
	}
	if err := <-errC; err == nil {
		t.Fatal("expected killed issue run to return an error")
	}
	evs, _ := s.EventsSince(0)
	killed := false
	for _, ev := range evs {
		if ev.Type == core.EvStageKilled {
			killed = true
		}
	}
	if !killed {
		t.Fatal("no stage_killed event")
	}
	if len(e.PendingDecisions()) != 0 {
		t.Fatal("killed stage left a pending decision")
	}
}

// A killed lane has no goroutine waiting at the gate. Resume must restart the
// stage that was killed rather than close a channel nobody is listening on.
func TestResumeRestartsKilledLane(t *testing.T) {
	sc := scripts()
	sc["brainstorm/brainstorm"] = runner.Script{Asks: []levers.Decision{
		{Question: "block forever?", Options: []string{"a"}, Recommended: 0, Importance: 1.0}}}
	fr := &runner.FakeRunner{Scripts: sc}
	e, s := newEngine(t, fr)
	id, _ := e.CreateIssue("k", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)

	errC := make(chan error, 1)
	go func() { errC <- e.StartIssue(context.Background(), id) }()
	deadline := time.After(5 * time.Second)
	for len(e.PendingDecisions()) == 0 {
		select {
		case <-deadline:
			t.Fatal("decision never appeared")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := e.KillStage(id); err != nil {
		t.Fatal(err)
	}
	if err := <-errC; err == nil {
		t.Fatal("expected killed run to return an error")
	}

	// Unblock the stage, then resume. brainstorm must run a second time.
	fr.Scripts["brainstorm/brainstorm"] = runner.Script{}
	if err := e.Resume(id); err != nil {
		t.Fatal(err)
	}
	deadline = time.After(5 * time.Second)
	for {
		evs, _ := s.EventsSince(0)
		starts := 0
		for _, ev := range evs {
			if ev.Type != core.EvStageStarted {
				continue
			}
			var p map[string]any
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p["stage"] == "brainstorm" {
				starts++
			}
		}
		if starts >= 2 {
			if err := e.KillStage(id); err == nil {
				waitForEvent(t, s, id, core.EvStageKilled)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("brainstorm never restarted (starts=%d)", starts)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A lane created and paused but never started must not be launched by Resume;
// clearing the gate is all that is asked for.
func TestResumeDoesNotStartUnstartedLane(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, _ := e.CreateIssue("u", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
	if err := e.Pause(id); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(id); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	evs, _ := s.EventsSince(0)
	for _, ev := range evs {
		if ev.Type == core.EvStageStarted {
			t.Fatal("resume started a lane that was never started")
		}
	}
}

// Resuming a lane that is running normally, with no gate, is an error.
func TestResumeRunningLaneWithNoGateErrors(t *testing.T) {
	sc := scripts()
	sc["brainstorm/brainstorm"] = runner.Script{Asks: []levers.Decision{
		{Question: "hold", Options: []string{"a"}, Recommended: 0, Importance: 1.0}}}
	e, _ := newEngine(t, &runner.FakeRunner{Scripts: sc})
	id, _ := e.CreateIssue("n", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
	go e.StartIssue(context.Background(), id)
	deadline := time.After(5 * time.Second)
	for len(e.PendingDecisions()) == 0 {
		select {
		case <-deadline:
			t.Fatal("decision never appeared")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := e.Resume(id); err == nil {
		t.Fatal("expected an error resuming a lane with no gate")
	}
	_ = e.KillStage(id)
}

func TestRetryStageResumesFromFailure(t *testing.T) {
	sc := scripts()
	sc["execute/executor"] = runner.Script{Fail: true}
	fr := &runner.FakeRunner{Scripts: sc}
	e, s := newEngine(t, fr)
	id, _ := e.CreateIssue("r", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
	errC := make(chan error, 1)
	go func() { errC <- e.StartIssue(context.Background(), id) }()
	for {
		if ds := e.PendingDecisions(); len(ds) == 1 {
			if err := e.Answer(ds[0].ID, levers.ChoiceResponse(0)); err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-errC; err == nil {
		t.Fatal("expected failure")
	}
	// fix the world, then retry
	fr.Scripts["execute/executor"] = runner.Script{Artifacts: map[string]string{"diff": ""}}
	errC2 := make(chan error, 1)
	go func() { errC2 <- e.RetryStage(context.Background(), id) }()
	if err := <-errC2; err != nil {
		t.Fatal(err)
	}
	evs, _ := s.EventsSince(0)
	var completed, brainstormStarts int
	for _, ev := range evs {
		if ev.Type == core.EvStageCompleted {
			completed++
		}
		if ev.Type == core.EvStageStarted {
			var p map[string]any
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p["stage"] == "brainstorm" {
				brainstormStarts++
			}
		}
	}
	// retry must NOT re-run earlier stages
	if brainstormStarts != 1 {
		t.Fatalf("brainstorm re-ran: %d", brainstormStarts)
	}
	if completed != 4 {
		t.Fatalf("completed=%d", completed)
	}
}

func TestSetLeverEmitsEvent(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, _ := e.CreateIssue("l", "", "default", levers.Preset(testFlow(), flow.LeverStrict), 0, nil)
	if err := e.SetLever(id, "execute", flow.LeverYolo); err != nil {
		t.Fatal(err)
	}
	evs, _ := s.EventsSince(0)
	found := false
	for _, ev := range evs {
		if ev.Type == core.EvLeverChanged {
			found = true
		}
	}
	if !found {
		t.Fatal("no lever_changed event")
	}
	issues, err := s.Issues()
	if err != nil || len(issues) != 1 || issues[0].Levers["execute"] != string(flow.LeverYolo) {
		t.Fatalf("lever was not persisted: issues=%+v err=%v", issues, err)
	}
}

func TestTokenBudgetEscalates(t *testing.T) {
	sc := scripts()
	sc["brainstorm/brainstorm"] = runner.Script{Tokens: 5000}
	e, s := newEngine(t, &runner.FakeRunner{Scripts: sc})
	e.cfg.TokenBudget = 1000
	id, _ := e.CreateIssue("b", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
	errC := make(chan error, 1)
	go func() { errC <- e.StartIssue(context.Background(), id) }()

	// first escalation must be the budget question (before spec's gate)
	var pd PendingDecision
	deadline := time.After(5 * time.Second)
	for {
		if ds := e.PendingDecisions(); len(ds) == 1 {
			pd = ds[0]
			break
		}
		select {
		case <-deadline:
			t.Fatal("no decision")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !strings.Contains(pd.D.Question, "token budget") {
		t.Fatalf("expected budget question, got %q", pd.D.Question)
	}
	pagePath := filepath.Join(e.cfg.DataDir, id, "decisions", fmt.Sprintf("%d.html", pd.ID))
	page := string(waitForDecisionPageFile(t, pagePath))
	for _, want := range []string{
		"The durable token total is above the configured limit for this issue.",
		"Continues into spec and waives further token-budget checks for this issue until Watchtower restarts.",
		"Stops this run before spec starts; retrying spec asks for budget authorization again.",
		"The issue has consumed 5000 tokens against a configured budget of 1000.",
		"Continue to resume spec with the budget waived, or abort to stop before spec starts.",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("budget decision page missing %q: %s", want, page)
		}
	}
	for _, misleading := range []string{
		legacyWhyMissing, "No recorded outcome for this historical option.",
		"No verified progress was supplied.", "Add feedback", "enter feedback",
	} {
		if strings.Contains(page, misleading) {
			t.Errorf("budget decision page contains misleading %q: %s", misleading, page)
		}
	}
	if err := e.Answer(pd.ID, levers.FreeformResponse("Please continue cautiously.")); err == nil {
		t.Fatal("budget decision accepted feedback without a continue or abort choice")
	}
	if pending := e.PendingDecisions(); len(pending) != 1 || pending[0].ID != pd.ID {
		t.Fatalf("invalid budget feedback consumed the decision: %+v", pending)
	}
	if err := os.WriteFile(filepath.Join(e.issueDir(id), "touchset.json"), []byte(`{"globs":["late/**"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	lateEvidenceDir := filepath.Join(e.issueDir(id), "evidence", "spec")
	if err := os.MkdirAll(lateEvidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lateEvidenceDir, "evidence.json"), []byte(`{"files":[{"path":"late/change.go","added":1}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	e.Answer(pd.ID, levers.ChoiceResponse(1)) // abort
	if err := <-errC; err == nil {
		t.Fatal("expected abort error")
	}
	answeredPage := string(waitForDecisionPageFile(t, pagePath))
	for _, want := range []string{
		"Recorded outcome", "abort was selected",
		"Stops this run before spec starts; retrying spec asks for budget authorization again.",
	} {
		if !strings.Contains(answeredPage, want) {
			t.Errorf("answered budget page missing %q: %s", want, answeredPage)
		}
	}
	if strings.Contains(answeredPage, "resumed Test Agent in spec") {
		t.Errorf("answered budget page claims the aborted stage resumed: %s", answeredPage)
	}
	if strings.Contains(answeredPage, "late/**") || strings.Contains(answeredPage, "late/change.go") {
		t.Errorf("answered budget page replaced decision-time files with later state: %s", answeredPage)
	}
	if err := os.Remove(pagePath); err != nil {
		t.Fatal(err)
	}
	e2 := newEngineOnFileWithFlow(t, s, &runner.FakeRunner{Scripts: sc}, e.cfg.DataDir, testFlow())
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	recoveredPage := string(waitForDecisionPageFile(t, pagePath))
	if !strings.Contains(recoveredPage, "Stops this run before spec starts; retrying spec asks for budget authorization again.") {
		t.Errorf("recovered budget abort lost its engine continuation: %s", recoveredPage)
	}
	if strings.Contains(recoveredPage, "authorized Watchtower to resume") {
		t.Errorf("recovered budget abort claims the stage resumed: %s", recoveredPage)
	}
	evs, _ := s.EventsSince(0)
	found := false
	for _, ev := range evs {
		if ev.Type == core.EvBudgetExceeded {
			found = true
		}
	}
	if !found {
		t.Fatal("budget_exceeded event missing")
	}
}

func TestAutoResolvedDecisionsAreAudited(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, _ := e.CreateIssue("a", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
	errC := make(chan error, 1)
	go func() { errC <- e.StartIssue(context.Background(), id) }()
	pending := waitForPendingStage(t, e, "spec")
	if err := e.Answer(pending.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	if err := <-errC; err != nil {
		t.Fatal(err)
	}
	rows, err := s.AllDecisionRows()
	if err != nil {
		t.Fatal(err)
	}
	var auto, answered int
	var autoRow store.DecisionRow
	for _, r := range rows {
		switch r.Status {
		case "auto":
			auto++
			autoRow = r
		case "answered":
			answered++
		}
	}
	if auto != 1 || answered != 1 {
		t.Fatalf("auto=%d answered=%d rows=%+v", auto, answered, rows)
	}
	if autoRow.AnsweredAt.IsZero() {
		t.Fatalf("auto-resolved decision has no durable resolution time: %+v", autoRow)
	}
	archivePath := filepath.Join(e.cfg.DataDir, id, "decisions", fmt.Sprintf("%d.html", autoRow.ID))
	page := string(waitForDecisionPageFile(t, archivePath))
	if !strings.Contains(page, "Automatically resolved") || strings.Contains(page, "Answered:") {
		t.Fatalf("auto-resolved archive uses human resolution wording: %s", page)
	}
	if err := os.Remove(archivePath); err != nil {
		t.Fatal(err)
	}
	e2 := newEngineOnFileWithFlow(t, s, &runner.FakeRunner{Scripts: scripts()}, e.cfg.DataDir, testFlow())
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	e2.mu.Lock()
	continuationScheduled := len(e2.reviewContinuations) > 0
	e2.mu.Unlock()
	recovered := string(waitForDecisionPageFile(t, archivePath))
	wantStamp := "Automatically resolved: option 1 · " + autoRow.AnsweredAt.UTC().Format("2006-01-02 15:04")
	if !strings.Contains(recovered, wantStamp) || strings.Contains(recovered, "Answered:") {
		t.Fatalf("recovered auto archive lost automatic provenance: %s", recovered)
	}
	if continuationScheduled {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			e2.mu.Lock()
			is := e2.issues[id]
			idle := is != nil && !is.running && !is.terminal
			e2.mu.Unlock()
			if idle {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		e2.mu.Lock()
		is := e2.issues[id]
		idle := is != nil && !is.running && !is.terminal
		e2.mu.Unlock()
		if !idle {
			t.Fatal("rehydrated artifact continuation did not settle")
		}
	}
}

type recordingSequencer struct {
	planned  int
	merged   int
	plannedC chan touchset.Set
}

func (s *recordingSequencer) BlockedBehind(string) int { return 0 }
func (s *recordingSequencer) PlanApproved(issueID string, ts touchset.Set) {
	_ = issueID
	s.planned++
	if s.plannedC != nil {
		s.plannedC <- ts
	}
}
func (s *recordingSequencer) ReadyToMerge(context.Context, string) error { return nil }
func (s *recordingSequencer) Merged(string)                              { s.merged++ }
func (s *recordingSequencer) Aborted(string)                             {}

func TestMarshalReleasedAfterSuccessfulCompletionWithoutTrain(t *testing.T) {
	f := flow.Flow{Name: "default", Stages: []flow.Stage{
		{Name: "plan", Agents: []flow.AgentRef{{Package: "planner"}}, Workspace: "worktree", Gate: flow.GateApproveArtifact, Artifacts: []string{"touchset.json"}},
		{Name: "merge", Agents: []flow.AgentRef{{Package: "reviewer"}}, Workspace: "worktree", Gate: flow.GateAuto, MergeBarrier: true,
			Artifacts: append([]string(nil), flow.FinalizationArtifacts...)},
	}}
	repo := t.TempDir()
	initGitRepo(t, repo)
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seq := &recordingSequencer{}
	e := New(Config{
		Store: s, Runner: &runner.FakeRunner{Scripts: map[string]runner.Script{
			"plan/planner": {Artifacts: map[string]string{"touchset.json": `{"globs":["src/**"]}`}},
			"merge/reviewer": {Artifacts: map[string]string{
				"merge-report.md": "", "merge-decision.json": "", "verification.json": "",
			}},
		}},
		Marshal: seq, Pool: slots.NewPool(1), Flows: map[string]flow.Flow{"default": f},
		DataDir: t.TempDir(), Workspace: workspace.GitWorktree{Repo: repo}, DecisionIdentities: testDecisionIdentities(),
	})
	id, err := e.CreateIssue("marshal", "", "default", levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	errC := make(chan error, 1)
	go func() { errC <- e.StartIssue(context.Background(), id) }()
	for {
		if ds := e.PendingDecisions(); len(ds) == 1 {
			if err := e.Answer(ds[0].ID, levers.ChoiceResponse(0)); err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-errC; err != nil {
		t.Fatal(err)
	}
	if seq.planned != 1 || seq.merged != 1 {
		t.Fatalf("marshal lifecycle planned=%d merged=%d", seq.planned, seq.merged)
	}
}

func TestProposalAcceptCreatesIssue(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	e.FileProposal("GH-1", "Follow-up: retry queue", "discovered during execute")
	ps, _ := s.PendingProposals()
	if len(ps) != 1 {
		t.Fatalf("proposals: %+v", ps)
	}
	newID, err := e.ResolveProposal(ps[0].ID, true, "default", "regular")
	if err != nil || newID == "" {
		t.Fatalf("resolve: %v %q", err, newID)
	}
	evs, _ := s.EventsSince(0)
	var filed, accepted, created int
	for _, ev := range evs {
		switch ev.Type {
		case core.EvProposalFiled:
			filed++
		case core.EvProposalAccepted:
			accepted++
		case core.EvIssueCreated:
			created++
		}
	}
	if filed != 1 || accepted != 1 || created != 1 {
		t.Fatalf("filed=%d accepted=%d created=%d", filed, accepted, created)
	}
}

func TestProposalAcceptRetainsDependencies(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	parent, err := e.DraftIssue("parent", "", "default", "regular", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.FileProposalWithDependencies(
		"GH-source", "Follow-up", "needs parent first", []string{parent})
	ps, _ := s.PendingProposals()
	newID, err := e.ResolveProposal(ps[0].ID, true, "default", "regular")
	if err != nil {
		t.Fatal(err)
	}
	dependencies, err := s.Dependencies(newID)
	if err != nil || len(dependencies) != 1 || dependencies[0] != parent {
		t.Fatalf("dependencies: %v err=%v", dependencies, err)
	}
}

func TestProposalBatchAcceptResolvesLocalDependencyKeys(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	e.FileProposalBatch("GH-source", []runner.Proposal{
		{Key: "api", Title: "Add API"},
		{Key: "consumer", Title: "Use API", DependsOn: []string{"api"}},
	})
	ps, err := s.PendingProposals()
	if err != nil || len(ps) != 2 {
		t.Fatalf("proposals: %+v err=%v", ps, err)
	}
	firstID, err := e.ResolveProposal(ps[0].ID, true, "default", "regular")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.Issues()
	if err != nil || len(rows) != 2 {
		t.Fatalf("issues: %+v err=%v", rows, err)
	}
	idsByTitle := map[string]string{}
	for _, row := range rows {
		idsByTitle[row.Title] = row.ID
	}
	if firstID != idsByTitle["Add API"] {
		t.Fatalf("returned %s, API is %s", firstID, idsByTitle["Add API"])
	}
	consumerDeps, err := s.Dependencies(idsByTitle["Use API"])
	if err != nil || len(consumerDeps) != 1 || consumerDeps[0] != idsByTitle["Add API"] {
		t.Fatalf("consumer dependencies: %v err=%v", consumerDeps, err)
	}
	if pending, _ := s.PendingProposals(); len(pending) != 0 {
		t.Fatalf("batch left pending rows: %+v", pending)
	}
}

func TestProposalBatchRejectsUnknownDependencyWithoutPartialIssues(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	e.FileProposalBatch("GH-source", []runner.Proposal{
		{Key: "api", Title: "Add API"},
		{Key: "consumer", Title: "Use API", DependsOn: []string{"missing"}},
	})
	ps, _ := s.PendingProposals()
	if _, err := e.ResolveProposal(ps[0].ID, true, "default", "regular"); err == nil {
		t.Fatal("unknown batch dependency accepted")
	}
	if rows, _ := s.Issues(); len(rows) != 0 {
		t.Fatalf("partial issues persisted: %+v", rows)
	}
	if pending, _ := s.PendingProposals(); len(pending) != 2 {
		t.Fatalf("failed batch should remain reviewable: %+v", pending)
	}
}

func TestAcceptedDiscoveredDependencyReleasesRunAndWaitsForRestart(t *testing.T) {
	f := testFlow()
	fr := &runner.FakeRunner{Scripts: scripts()}
	fr.Scripts["brainstorm/brainstorm"] = runner.Script{DependsOn: []string{"GH-1"}}
	e, s := newEngine(t, fr)
	parent, err := e.DraftIssue("parent", "", "default", "regular", levers.Matrix{}, 0, nil)
	if err != nil || parent != "GH-1" {
		t.Fatalf("parent: %s %v", parent, err)
	}
	child, err := e.CreateIssue("child", "", "default", levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	dependencies, _ := s.Dependencies(child)
	if len(dependencies) != 1 || dependencies[0] != parent {
		t.Fatalf("dependencies: %v", dependencies)
	}
	events, _ := s.EventsSince(0)
	var waiting, completed bool
	for _, event := range events {
		if event.IssueID == child && event.Type == core.EvIssueWaitingDependencies {
			waiting = true
		}
		if event.IssueID == child && event.Type == core.EvIssueCompleted {
			completed = true
		}
	}
	if !waiting || completed {
		t.Fatalf("waiting=%v completed=%v", waiting, completed)
	}
}

func TestStageArtifactsAndCompactContextAreDurable(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	f := flow.Flow{Name: "default", Stages: []flow.Stage{
		{Name: "brainstorm", Agents: []flow.AgentRef{{Package: "brainstorm"}},
			Workspace: "worktree", Gate: flow.GateAuto, Completion: flow.CompletionAll,
			Artifacts: []string{"brainstorm.md"}},
		{Name: "spec", Agents: []flow.AgentRef{{Package: "spec-writer"}},
			Workspace: "worktree", Gate: flow.GateAuto, Completion: flow.CompletionAll},
	}}
	fr := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"brainstorm/brainstorm": {
			SessionID: "session-brainstorm",
			Asks: []levers.Decision{{
				Question: "Use the recommended shape?", Options: []string{"yes", "no"},
				Recommended: 0, Importance: 0.2, Why: "it is bounded",
				Consequences: []string{"bounded", "broader"},
			}},
			Artifacts: map[string]string{"brainstorm.md": "# approved\n"},
		},
		"spec/spec-writer": {SessionID: "session-spec"},
	}}
	var specBrief, specLedger string
	fr.OnStart = func(_, stage, _, workdir string) error {
		if stage != "spec" {
			return nil
		}
		brief, err := os.ReadFile(filepath.Join(workdir, "STAGE.md"))
		if err != nil {
			return err
		}
		ledger, err := os.ReadFile(filepath.Join(workdir, "decisions.md"))
		if err != nil {
			return err
		}
		specBrief, specLedger = string(brief), string(ledger)
		return nil
	}
	e, s := newEngineCfg(t, fr, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{"default": f}
		cfg.Workspace = &fakeWS{dir: repo}
	})
	id, err := e.CreateIssue(
		"durable context", "", "default", levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	durable := filepath.Join(e.cfg.DataDir, id, "artifacts", "brainstorm.md")
	if body, err := os.ReadFile(durable); err != nil || string(body) != "# approved\n" {
		t.Fatalf("durable brainstorm: %q err=%v", body, err)
	}
	if !strings.Contains(specBrief, "brainstorm.md") ||
		!strings.Contains(specLedger, "Use the recommended shape?") ||
		!strings.Contains(specLedger, "yes") {
		t.Fatalf("brief:\n%s\nledger:\n%s", specBrief, specLedger)
	}
	checkpoints, err := s.StageCheckpoints(id)
	if err != nil || len(checkpoints) != 2 {
		t.Fatalf("checkpoints: %+v err=%v", checkpoints, err)
	}
	if checkpoints[0].StartCommit == "" || checkpoints[0].EndCommit == "" ||
		len(checkpoints[0].Artifacts) != 1 ||
		checkpoints[0].Artifacts[0].Name != "brainstorm.md" {
		t.Fatalf("brainstorm checkpoint: %+v", checkpoints[0])
	}
	if checkpoints[0].SessionID == checkpoints[1].SessionID ||
		checkpoints[0].SessionID != "session-brainstorm" ||
		checkpoints[1].SessionID != "session-spec" {
		t.Fatalf("sessions not stage-local: %+v", checkpoints)
	}
}

func TestRetryBriefUsesCheckpointHeadDirtyStateAndFailure(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	f := flow.Flow{Name: "default", Stages: []flow.Stage{{
		Name: "execute", Agents: []flow.AgentRef{{Package: "executor"}},
		Workspace: "worktree", Gate: flow.GateAuto, Completion: flow.CompletionAll,
	}}}
	fr := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"execute/executor": {SessionID: "failed-session", Fail: true},
	}}
	var retryBrief string
	starts := 0
	fr.OnStart = func(_, _, _, workdir string) error {
		starts++
		if starts == 2 {
			body, err := os.ReadFile(filepath.Join(workdir, "STAGE.md"))
			retryBrief = string(body)
			return err
		}
		return nil
	}
	e, s := newEngineCfg(t, fr, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{"default": f}
		cfg.Workspace = &fakeWS{dir: repo}
	})
	id, err := e.CreateIssue("retry", "", "default", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("first run succeeded")
	}
	fr.Scripts["execute/executor"] = runner.Script{SessionID: "retry-session"}
	if err := e.RetryStage(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD"))
	for _, want := range []string{
		"Current HEAD: " + head, "dirty: no", "scripted failure execute/executor",
		"Last successful stage: unknown",
	} {
		if !strings.Contains(retryBrief, want) {
			t.Fatalf("retry brief missing %q:\n%s", want, retryBrief)
		}
	}
	checkpoints, err := s.StageCheckpoints(id)
	if err != nil || len(checkpoints) != 2 ||
		checkpoints[0].Status != "failed" || checkpoints[1].Status != "succeeded" {
		t.Fatalf("checkpoints: %+v err=%v", checkpoints, err)
	}
}

func verificationFlow() flow.Flow {
	return flow.Flow{Name: "default", Stages: []flow.Stage{{
		Name: "merge-verification", Agents: []flow.AgentRef{{Package: "merge-verifier"}},
		Workspace: "worktree", Gate: flow.GateAuto, Completion: flow.CompletionAll,
		MergeBarrier: true,
		Artifacts:    []string{"merge-report.md", "merge-decision.json", "verification.json"},
	}}}
}

func verificationEngine(
	t *testing.T, decision string, commands [][]string, treeOverride string,
) (*Engine, *store.Store, string) {
	t.Helper()
	return verificationEngineForFlow(t, verificationFlow(), decision, commands, treeOverride)
}

func verificationEngineForFlow(
	t *testing.T, f flow.Flow, decision string, commands [][]string, treeOverride string,
) (*Engine, *store.Store, string) {
	t.Helper()
	repo := t.TempDir()
	initGitRepo(t, repo)
	base := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD"))
	tree := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD^{tree}"))
	if treeOverride != "" {
		tree = treeOverride
	}
	receipt, err := json.Marshal(marshal.Verification{
		BaseSHA: base, BranchSHA: base, TreeSHA: tree, Passed: true, Commands: commands,
	})
	if err != nil {
		t.Fatal(err)
	}
	decisionReceipt := marshal.MergeDecision{Decision: decision}
	if decision == "merge" {
		decisionReceipt.BranchCommit = base
		decisionReceipt.BaseCommit = base
	}
	decisionBody, err := json.Marshal(decisionReceipt)
	if err != nil {
		t.Fatal(err)
	}
	integrationStage, _, ok := f.IntegrationStage()
	if !ok {
		t.Fatal("verification test flow has no merge barrier")
	}
	scripts := map[string]runner.Script{}
	for _, stage := range f.Stages {
		for _, agent := range stage.Agents {
			scripts[stage.Name+"/"+agent.Package] = runner.Script{}
		}
	}
	finalKey := integrationStage.Name + "/" + integrationStage.Agents[0].Package
	scripts[finalKey] = runner.Script{Artifacts: map[string]string{
		"merge-report.md": "verified\n", "merge-decision.json": string(decisionBody),
		"verification.json": string(receipt),
	}}
	for _, stage := range f.Stages {
		if stage.DeclaresArtifact("touchset.json") {
			key := stage.Name + "/" + stage.Agents[0].Package
			script := scripts[key]
			script.Artifacts = map[string]string{"touchset.json": `{"globs":["feature.txt"]}`}
			scripts[key] = script
		}
	}
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e := New(Config{
		Store: s, Pool: slots.NewPool(1), Flows: map[string]flow.Flow{f.Name: f},
		DataDir: t.TempDir(), Workspace: workspace.GitWorktree{Repo: repo},
		Train:              &marshal.Train{Repo: repo, TestCmd: []string{"true"}},
		Runner:             &runner.FakeRunner{Scripts: scripts},
		DecisionIdentities: testDecisionIdentities(),
	})
	return e, s, repo
}

func TestVerificationCacheAgentAndDaemonReplayShareLeaseEnvironment(t *testing.T) {
	e, s, _ := verificationEngine(t, "merge", [][]string{{"true"}}, "")
	cacheRoot := t.TempDir()
	capture := filepath.Join(t.TempDir(), "daemon-cache")
	command := filepath.Join(t.TempDir(), "verify.sh")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nset -eu\nprintf '%s\\n' \"$GOCACHE\" > \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.cfg.CacheRoot = cacheRoot
	e.cfg.Train.CacheRoot = cacheRoot
	e.cfg.Train.TestCmd = []string{command, capture}
	fake := e.cfg.Runner.(*runner.FakeRunner)
	var agentEnvironment []string
	fake.OnEnvironment = func(_, _, _, _ string, env []string) {
		agentEnvironment = append([]string(nil), env...)
	}
	id, err := e.CreateIssue("cache parity", "", "default", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatalf("cache-managed merge barrier failed: %v", err)
	}
	if len(agentEnvironment) == 0 {
		t.Fatal("agent did not observe a managed lease environment")
	}
	observed, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("daemon replay did not observe the managed environment: %v", err)
	}
	agentValues := make(map[string]string)
	for _, entry := range agentEnvironment {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			agentValues[key] = value
		}
	}
	if got, want := strings.TrimSpace(string(observed)), agentValues["GOCACHE"]; got != want {
		t.Fatalf("daemon GOCACHE = %q, agent GOCACHE = %q", got, want)
	}
	archived, err := marshal.LoadVerification(filepath.Join(e.issueDir(id), "artifacts", "verification.json"))
	if err != nil {
		t.Fatal(err)
	}
	if archived.CacheEvidence == nil || archived.CacheEvidence.State != string(verificationcache.StateComplete) {
		t.Fatalf("archived cache evidence = %+v, want complete", archived.CacheEvidence)
	}
	if !hasEvent(t, s, id, core.EvVerificationReady) {
		t.Fatal("matching replay and receipt did not create verification_ready")
	}
}

func renamedVerificationFlow() flow.Flow {
	return flow.Flow{Name: "custom", Stages: []flow.Stage{
		{
			Name: "scope-files", Agents: []flow.AgentRef{{Package: "planner"}},
			Workspace: "worktree", Gate: flow.GateAuto,
			Artifacts: []string{"touchset.json"},
		},
		{
			Name: "integrate-safely", Agents: []flow.AgentRef{{Package: "verifier"}},
			Workspace: "worktree", Gate: flow.GateAuto, MergeBarrier: true,
			Artifacts: append([]string(nil), flow.FinalizationArtifacts...),
		},
	}}
}

func TestRenamedStagesSequenceTouchsetVerifyAndMergeByCapability(t *testing.T) {
	f := renamedVerificationFlow()
	e, s, repo := verificationEngineForFlow(t, f, "merge", [][]string{{"true"}}, "")
	id, err := e.CreateIssue("custom flow", "", f.Name, levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	runs, err := s.StageRuns(id)
	if err != nil || len(runs) != 2 ||
		runs[0].Stage != "scope-files" || runs[1].Stage != "integrate-safely" {
		t.Fatalf("stage runs = %+v err %v", runs, err)
	}
	if branch := gitOutput(t, repo, "branch", "--list", "issue/"+id); strings.TrimSpace(branch) != "" {
		t.Fatalf("merged issue branch remains: %q", branch)
	}
	if !hasEvent(t, s, id, core.EvVerificationReady) ||
		!hasEvent(t, s, id, core.EvIssueMerged) {
		t.Fatal("custom flow did not verify and merge")
	}
}

func TestMalformedFinalReceiptFailsBeforeStageCompletion(t *testing.T) {
	e, s, _ := verificationEngine(t, "merge", [][]string{{"true"}}, "")
	fake := e.cfg.Runner.(*runner.FakeRunner)
	script := fake.Scripts["merge-verification/merge-verifier"]
	script.Artifacts["merge-decision.json"] =
		`{"decision":"merge","branch_commit":"b","base_commit":"a","rationale":"rich"}`
	fake.Scripts["merge-verification/merge-verifier"] = script
	id, err := e.CreateIssue("bad receipt", "", "default", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("malformed receipt completed")
	}
	events, _ := s.EventsSince(0)
	var completed, failed bool
	for _, event := range events {
		if event.IssueID != id {
			continue
		}
		completed = completed || event.Type == core.EvStageCompleted
		failed = failed || event.Type == core.EvStageFailed
	}
	if completed || !failed {
		t.Fatalf("completed=%v failed=%v", completed, failed)
	}
}

func TestVerificationReadyPrecedesMerge(t *testing.T) {
	e, s, _ := verificationEngine(t, "merge", [][]string{{"true"}}, "")
	id, err := e.CreateIssue("ready", "", "default", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	events, _ := s.EventsSince(0)
	positions := map[core.EventType]int{}
	for index, event := range events {
		if event.IssueID == id {
			positions[event.Type] = index + 1
		}
	}
	if positions[core.EvStageCompleted] == 0 || positions[core.EvVerificationReady] == 0 ||
		positions[core.EvMergeStarted] == 0 ||
		positions[core.EvStageCompleted] > positions[core.EvVerificationReady] ||
		positions[core.EvVerificationReady] > positions[core.EvMergeStarted] {
		t.Fatalf("event positions=%v", positions)
	}
}

func TestMergeVerificationHoldPreservesBranchWithoutLanding(t *testing.T) {
	e, s, repo := verificationEngine(t, "hold", [][]string{{"true"}}, "")
	id, err := e.CreateIssue("hold", "", "default", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	events, _ := s.EventsSince(0)
	var mergeStarted, leftUnmerged bool
	for _, event := range events {
		if event.IssueID != id {
			continue
		}
		if event.Type == core.EvMergeStarted {
			mergeStarted = true
		}
		if event.Type == core.EvIssueCompleted && strings.Contains(string(event.Payload), "left-unmerged") {
			leftUnmerged = true
		}
	}
	if mergeStarted || !leftUnmerged {
		t.Fatalf("mergeStarted=%v leftUnmerged=%v", mergeStarted, leftUnmerged)
	}
	if branch := gitOutput(t, repo, "branch", "--list", "issue/"+id); strings.TrimSpace(branch) == "" {
		t.Fatal("held branch was deleted")
	}
}

func TestRetryStageReopensDoneUnmergedIssueAfterRestart(t *testing.T) {
	e, s, repo := verificationEngine(t, "hold", [][]string{{"true"}}, "")
	e.cfg.Observers = append(e.cfg.Observers, (&steward.Steward{Store: s}).Observe)
	f := e.cfg.Flows["default"]
	f.Stages = append([]flow.Stage{{
		Name: "execute", Agents: []flow.AgentRef{{Package: "executor"}},
		Workspace: "worktree", Gate: flow.GateAuto, Completion: flow.CompletionAll,
	}}, f.Stages...)
	e.cfg.Flows["default"] = f
	fake := e.cfg.Runner.(*runner.FakeRunner)
	fake.Scripts["execute/executor"] = runner.Script{}

	id, err := e.CreateIssue("retry held issue", "", "default", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	runsBefore, err := s.StageRuns(id)
	if err != nil {
		t.Fatal(err)
	}

	restarted := New(e.cfg)
	if err := restarted.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if _, reopened := restarted.issues[id]; reopened {
		t.Fatal("rehydration reopened a held issue without an explicit retry")
	}

	issueWorktree := filepath.Join(t.TempDir(), "issue")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", issueWorktree, "issue/"+id).CombinedOutput(); err != nil {
		t.Fatalf("restore issue worktree: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(issueWorktree, "feature"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "feature"}, {"commit", "-qm", "feature"}} {
		if out, err := exec.Command("git", append([]string{"-C", issueWorktree}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	branch := strings.TrimSpace(gitOutput(t, issueWorktree, "rev-parse", "HEAD"))
	tree := strings.TrimSpace(gitOutput(t, issueWorktree, "rev-parse", "HEAD^{tree}"))
	if out, err := exec.Command("git", "-C", repo, "worktree", "remove", issueWorktree).CombinedOutput(); err != nil {
		t.Fatalf("remove issue worktree: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "base-change"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "base-change"}, {"commit", "-qm", "advance base"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	base := strings.TrimSpace(gitOutput(t, repo, "merge-base", "HEAD", branch))
	restarted.cfg.Workspace = &existingBranchWS{repo: repo, root: t.TempDir()}

	script := fake.Scripts["merge-verification/merge-verifier"]
	script.Artifacts["merge-decision.json"] = fmt.Sprintf(
		`{"decision":"merge","branch_commit":%q,"base_commit":%q}`, branch, base)
	verification, err := json.Marshal(marshal.Verification{
		BaseSHA: base, BranchSHA: branch, TreeSHA: tree, Passed: true, Commands: [][]string{{"true"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	script.Artifacts["verification.json"] = string(verification)
	fake.Scripts["merge-verification/merge-verifier"] = script

	if err := restarted.RetryStage(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	runsAfter, err := s.StageRuns(id)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, run := range runsAfter {
		counts[run.Stage]++
	}
	if len(runsAfter) != len(runsBefore)+1 || counts["execute"] != 1 || counts["merge-verification"] != 2 {
		t.Fatalf("stage runs after retry = %+v, counts=%v", runsAfter, counts)
	}
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	merged := false
	for _, event := range events {
		merged = merged || event.IssueID == id && event.Type == core.EvIssueMerged
	}
	if !merged {
		t.Fatal("retried held issue did not merge")
	}
}

func TestMergeVerificationRequiresMachineDecision(t *testing.T) {
	tests := []struct {
		name     string
		decision string
		want     string
	}{
		{name: "unknown decision", decision: "maybe", want: "merge decision"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e, _, _ := verificationEngine(t, test.decision, [][]string{{"true"}}, "")
			id, err := e.CreateIssue(test.name, "", "default", levers.Matrix{}, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := e.StartIssue(context.Background(), id); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("StartIssue error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestMergeVerificationAcceptsMatchingPassingReceipt(t *testing.T) {
	e, s, _ := verificationEngine(t, "merge", [][]string{{"true"}}, "")
	id, err := e.CreateIssue("merge", "", "default", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	events, _ := s.EventsSince(0)
	for _, event := range events {
		if event.IssueID == id && event.Type == core.EvIssueMerged {
			return
		}
	}
	t.Fatal("valid receipt did not reach merge")
}

func TestPushFailurePersistsAndRetryPublishesWithoutRerunningStage(t *testing.T) {
	e, s, repo := verificationEngine(t, "merge", [][]string{{"true"}}, "")
	goodRemote := t.TempDir()
	if out, err := exec.Command("git", "-C", goodRemote, "init", "-q", "--bare").CombinedOutput(); err != nil {
		t.Fatalf("init remote: %v: %s", err, out)
	}
	missingRemote := filepath.Join(t.TempDir(), "missing.git")
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", missingRemote).CombinedOutput(); err != nil {
		t.Fatalf("add remote: %v: %s", err, out)
	}
	e.cfg.Train.Push = true
	id, err := e.CreateIssue("publish", "", "default", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = e.StartIssue(context.Background(), id)
	var pending *marshal.PublishPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("StartIssue error = %v, want publish pending", err)
	}
	integration, ok, err := s.IssueIntegration(id)
	if err != nil || !ok || integration.State != store.IntegrationPublishPending ||
		integration.LandedSHA == "" {
		t.Fatalf("integration = %+v ok %v err %v", integration, ok, err)
	}
	runs, err := s.StageRuns(id)
	if err != nil || len(runs) != 1 {
		t.Fatalf("stage runs before retry = %+v err %v", runs, err)
	}
	if out, err := exec.Command(
		"git", "-C", repo, "remote", "set-url", "origin", goodRemote,
	).CombinedOutput(); err != nil {
		t.Fatalf("repair remote: %v: %s", err, out)
	}
	restarted := New(e.cfg)
	if err := restarted.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.RetryStage(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	runs, err = s.StageRuns(id)
	if err != nil || len(runs) != 1 {
		t.Fatalf("publish retry reran stage: %+v err %v", runs, err)
	}
	integration, ok, err = s.IssueIntegration(id)
	if err != nil || !ok || integration.State != store.IntegrationMerged {
		t.Fatalf("integration after retry = %+v ok %v err %v", integration, ok, err)
	}
	remoteHead := strings.TrimSpace(gitOutput(t, goodRemote, "rev-parse", "main"))
	if remoteHead != integration.LandedSHA {
		t.Fatalf("remote main %s != landed %s", remoteHead, integration.LandedSHA)
	}
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[core.EventType]bool{}
	for _, event := range events {
		if event.IssueID == id {
			seen[event.Type] = true
		}
	}
	for _, eventType := range []core.EventType{
		core.EvPublishPending, core.EvPublishRetry, core.EvPublishSucceeded, core.EvIssueMerged,
	} {
		if !seen[eventType] {
			t.Fatalf("missing %s event: %+v", eventType, seen)
		}
	}
}

func TestFinalizationFailureRetriesIntegrationWithoutRerunningVerifier(t *testing.T) {
	e, s, repo := verificationEngine(t, "merge", [][]string{{"true"}}, "")
	fake := e.cfg.Runner.(*runner.FakeRunner)
	fake.OnStart = func(_, stage, _, _ string) error {
		if stage != "merge-verification" {
			return nil
		}
		return os.WriteFile(filepath.Join(repo, "diff"), []byte("dirty base\n"), 0o644)
	}
	id, err := e.CreateIssue("retry finalization", "", "default", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = e.StartIssue(context.Background(), id)
	if err == nil || !strings.Contains(err.Error(), "base checkout is dirty") {
		t.Fatalf("StartIssue error = %v", err)
	}
	integration, ok, err := s.IssueIntegration(id)
	if err != nil || !ok || integration.State != store.IntegrationVerificationReady ||
		!strings.Contains(integration.LastError, "base checkout is dirty") {
		t.Fatalf("integration = %+v ok %v err %v", integration, ok, err)
	}
	runs, err := s.StageRuns(id)
	if err != nil || len(runs) != 1 {
		t.Fatalf("stage runs before retry = %+v err %v", runs, err)
	}
	if out, err := exec.Command("git", "-C", repo, "checkout", "--", "diff").CombinedOutput(); err != nil {
		t.Fatalf("repair base: %v: %s", err, out)
	}
	fake.OnStart = nil
	if err := e.RetryStage(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	runs, err = s.StageRuns(id)
	if err != nil || len(runs) != 1 {
		t.Fatalf("finalization retry reran verifier: %+v err %v", runs, err)
	}
	integration, ok, err = s.IssueIntegration(id)
	if err != nil || !ok || integration.State != store.IntegrationMerged {
		t.Fatalf("integration after retry = %+v ok %v err %v", integration, ok, err)
	}
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var failed, merged bool
	for _, event := range events {
		if event.IssueID == id {
			failed = failed || event.Type == core.EvFinalizationFailed
			merged = merged || event.Type == core.EvIssueMerged
		}
	}
	if !failed || !merged {
		t.Fatalf("finalization_failed=%v merged=%v", failed, merged)
	}
}

func TestRehydrateAutomaticallyResumesVerifiedFinalization(t *testing.T) {
	e, s, repo := verificationEngineForFlow(
		t, renamedVerificationFlow(), "merge", [][]string{{"true"}}, "",
	)
	fake := e.cfg.Runner.(*runner.FakeRunner)
	fake.OnStart = func(_, stage, _, _ string) error {
		if stage != "integrate-safely" {
			return nil
		}
		return os.WriteFile(filepath.Join(repo, "diff"), []byte("dirty base\n"), 0o644)
	}
	id, err := e.CreateIssue("restart finalization", "", "custom", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("dirty base unexpectedly landed")
	}
	if out, err := exec.Command("git", "-C", repo, "checkout", "--", "diff").CombinedOutput(); err != nil {
		t.Fatalf("repair base: %v: %s", err, out)
	}
	fake.OnStart = nil
	restarted := New(e.cfg)
	if err := restarted.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		integration, ok, err := s.IssueIntegration(id)
		if err != nil {
			t.Fatal(err)
		}
		if ok && integration.State == store.IntegrationMerged {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("integration did not resume: %+v", integration)
		}
		time.Sleep(10 * time.Millisecond)
	}
	runs, err := s.StageRuns(id)
	if err != nil {
		t.Fatalf("rehydration reran verifier: %+v err %v", runs, err)
	}
	counts := map[string]int{}
	for _, run := range runs {
		counts[run.Stage]++
	}
	if counts["integrate-safely"] != 1 {
		t.Fatalf("verified finalization reran agent: %+v", runs)
	}
	_, _, _, lastErr, err := s.LastStageEvents(id)
	if err != nil || lastErr != "" {
		t.Fatalf("restart synthesized stage failure: lastErr=%q err=%v", lastErr, err)
	}
}

type countingGitWorktree struct {
	repo     string
	acquired int
	releases int
}

func (w *countingGitWorktree) Acquire(issueID string) (string, func() error, error) {
	w.acquired++
	path, release, err := (workspace.GitWorktree{Repo: w.repo}).Acquire(issueID)
	if err != nil {
		return "", nil, err
	}
	return path, func() error {
		w.releases++
		return release()
	}, nil
}

func (w *countingGitWorktree) Name() string { return "counting git worktree" }

func (w *countingGitWorktree) ReleasePath(path string) error {
	w.releases++
	return (workspace.GitWorktree{Repo: w.repo}).ReleasePath(path)
}

func engineForFlow(
	t *testing.T, ws workspace.Provider, f flow.Flow, r runner.Runner,
) (*Engine, *store.Store) {
	t.Helper()
	return newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.Workspace = ws
	})
}

func hasCompletionPayload(
	t *testing.T, s *store.Store, issueID, merge, worktree string,
) bool {
	t.Helper()
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.IssueID != issueID || event.Type != core.EvIssueCompleted {
			continue
		}
		var payload map[string]string
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		return payload["merge"] == merge && payload["worktree"] == worktree
	}
	return false
}

func TestFlowWithoutBarrierReleasesUnchangedWorkspaceWithoutMerging(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	base := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD"))
	ws := &countingGitWorktree{repo: repo}
	f := flow.Flow{Name: "research", Stages: []flow.Stage{{
		Name: "inspect", Agents: []flow.AgentRef{{Package: "explorer"}},
		Workspace: "worktree", Gate: flow.GateAuto,
	}}}
	e, s := engineForFlow(t, ws, f, &runner.FakeRunner{
		Scripts: map[string]runner.Script{"inspect/explorer": {}},
	})
	id, err := e.CreateIssue("inspect", "", "research", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if head := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD")); head != base {
		t.Fatalf("non-integrating flow moved default branch: %s -> %s", base, head)
	}
	if ws.releases != 1 {
		t.Fatalf("workspace releases = %d", ws.releases)
	}
	if _, ok, err := s.IssueIntegration(id); err != nil || ok {
		t.Fatalf("unchanged integration row exists: ok=%v err=%v", ok, err)
	}
}

func TestFlowWithoutBarrierPreservesChangedWorkspaceAndReportsIdentity(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%t", committed), func(t *testing.T) {
			repo := t.TempDir()
			initGitRepo(t, repo)
			base := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD"))
			ws := &countingGitWorktree{repo: repo}
			run := &runner.FakeRunner{Scripts: map[string]runner.Script{"explore/researcher": {}}}
			run.OnStart = func(_, _, _, workdir string) error {
				if err := os.WriteFile(filepath.Join(workdir, "finding.md"), []byte("finding\n"), 0o644); err != nil {
					return err
				}
				if !committed {
					return nil
				}
				for _, args := range [][]string{{"add", "finding.md"}, {"commit", "-qm", "record finding"}} {
					if output, err := exec.Command("git", append([]string{"-C", workdir}, args...)...).CombinedOutput(); err != nil {
						return fmt.Errorf("git %v: %v: %s", args, err, output)
					}
				}
				return nil
			}
			f := flow.Flow{Name: "research", Stages: []flow.Stage{{
				Name: "explore", Agents: []flow.AgentRef{{Package: "researcher"}},
				Workspace: "worktree", Gate: flow.GateAuto,
			}}}
			e, s := engineForFlow(t, ws, f, run)
			id, err := e.CreateIssue("research", "", "research", levers.Matrix{}, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := e.StartIssue(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			if head := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD")); head != base {
				t.Fatalf("non-integrating flow moved default branch: %s -> %s", base, head)
			}
			integration, ok, err := s.IssueIntegration(id)
			if err != nil || !ok || integration.State != store.IntegrationPreserved ||
				integration.Worktree == "" || integration.Branch != "issue/"+id {
				t.Fatalf("preserved integration = %+v ok %v err %v", integration, ok, err)
			}
			if _, err := os.Stat(integration.Worktree); err != nil {
				t.Fatalf("preserved worktree: %v", err)
			}
			if ws.releases != 0 {
				t.Fatalf("preserved workspace was released %d times", ws.releases)
			}
			if !hasCompletionPayload(t, s, id, "left-unmerged", integration.Worktree) {
				t.Fatal("completion did not expose preserved work")
			}
		})
	}
}

type conflictFlowRunner struct {
	repo                     string
	decision                 string
	interruptAfterResolution bool
	gate                     []string
	originalWorkdir          string
	conflictWorkdir          string
	conflictContext          string
	currentBase              string
}

func commandIn(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (r *conflictFlowRunner) Run(
	_ context.Context, _, stage, _ string, workdir string, _ chan<- runner.Ask,
) <-chan runner.Result {
	results := make(chan runner.Result, 1)
	var result runner.Result
	switch stage {
	case "execute":
		r.originalWorkdir = workdir
		if err := os.WriteFile(filepath.Join(workdir, "diff"), []byte("issue\n"), 0o644); err != nil {
			result.Err = err
			break
		}
		if output, err := commandIn(workdir, "commit", "-qam", "issue change"); err != nil {
			result.Err = fmt.Errorf("commit issue: %v: %s", err, output)
		}
	case "integrate-safely":
		base, _ := commandIn(workdir, "merge-base", "main", "HEAD")
		branch, _ := commandIn(workdir, "rev-parse", "HEAD")
		tree, _ := commandIn(workdir, "rev-parse", "HEAD^{tree}")
		receipt, _ := json.Marshal(marshal.Verification{
			BaseSHA: base, BranchSHA: branch, TreeSHA: tree, Passed: true,
			Commands: [][]string{r.gate},
		})
		decision, _ := json.Marshal(marshal.MergeDecision{
			Decision: "merge", BranchCommit: branch, BaseCommit: base,
		})
		for name, body := range map[string][]byte{
			"merge-report.md":     []byte("verified\n"),
			"merge-decision.json": decision,
			"verification.json":   receipt,
		} {
			if err := os.WriteFile(filepath.Join(workdir, name), body, 0o644); err != nil {
				result.Err = err
				break
			}
		}
		if result.Err == nil {
			if err := os.WriteFile(filepath.Join(r.repo, "diff"), []byte("base advanced\n"), 0o644); err != nil {
				result.Err = err
			} else if output, err := commandIn(r.repo, "commit", "-qam", "advance base"); err != nil {
				result.Err = fmt.Errorf("advance base: %v: %s", err, output)
			} else {
				r.currentBase, _ = commandIn(r.repo, "rev-parse", "HEAD")
			}
		}
	case "conflict-resolution":
		r.conflictWorkdir = workdir
		contextBody, err := os.ReadFile(filepath.Join(workdir, "CONFLICT.md"))
		if err != nil {
			result.Err = err
			break
		}
		r.conflictContext = string(contextBody)
		if r.decision == "resolved" {
			_, _ = commandIn(workdir, "rebase", "main")
			if err := os.WriteFile(filepath.Join(workdir, "diff"), []byte("resolved\n"), 0o644); err != nil {
				result.Err = err
				break
			}
			if output, err := commandIn(workdir, "add", "diff"); err != nil {
				result.Err = fmt.Errorf("add resolution: %v: %s", err, output)
				break
			}
			if output, err := commandIn(workdir, "-c", "core.editor=true", "rebase", "--continue"); err != nil {
				result.Err = fmt.Errorf("continue rebase: %v: %s", err, output)
				break
			}
		}
		if err := os.WriteFile(
			filepath.Join(workdir, "conflict-report.md"), []byte("conflict handled\n"), 0o644,
		); err != nil {
			result.Err = err
			break
		}
		if r.decision != "missing" {
			body, _ := json.Marshal(map[string]string{"decision": r.decision})
			result.Err = os.WriteFile(filepath.Join(workdir, "conflict-decision.json"), body, 0o644)
			if result.Err == nil && r.interruptAfterResolution {
				result.Err = errors.New("resolver exited after writing its decision")
			}
		}
	}
	results <- result
	close(results)
	return results
}

func TestRetryFinalizesResolvedConflictWithoutRerunningAgents(t *testing.T) {
	e, s, repo, run, _, _ := conflictEngine(t, "resolved")
	run.interruptAfterResolution = true
	id, err := e.CreateIssue("interrupted conflict resolution", "", "default", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err == nil ||
		!strings.Contains(err.Error(), "resolver exited") {
		t.Fatalf("StartIssue error = %v, want resolver interruption", err)
	}
	runsBefore, err := s.StageRuns(id)
	if err != nil {
		t.Fatal(err)
	}
	resolvedHead := strings.TrimSpace(gitOutput(t, run.conflictWorkdir, "rev-parse", "HEAD"))
	run.interruptAfterResolution = false
	if err := e.RetryStage(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	runsAfter, err := s.StageRuns(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(runsAfter) != len(runsBefore) {
		t.Fatalf("retry reran an agent: before=%d after=%d", len(runsBefore), len(runsAfter))
	}
	counts := map[string]int{}
	for _, stageRun := range runsAfter {
		counts[stageRun.Stage]++
	}
	if counts["integrate-safely"] != 1 || counts["conflict-resolution"] != 1 {
		t.Fatalf("stage counts = %+v", counts)
	}
	integration, ok, err := s.IssueIntegration(id)
	if err != nil || !ok || integration.State != store.IntegrationMerged {
		t.Fatalf("integration after retry = %+v ok %v err %v", integration, ok, err)
	}
	if _, err := os.Stat(run.gate[1]); err != nil {
		t.Fatalf("combined verification was not replayed: %v", err)
	}
	if out, err := exec.Command(
		"git", "-C", repo, "merge-base", "--is-ancestor", resolvedHead, "main",
	).CombinedOutput(); err != nil {
		t.Fatalf("resolved issue was not merged: %v: %s", err, out)
	}
	if !hasEvent(t, s, id, core.EvIssueMerged) {
		t.Fatal("resolved conflict did not merge")
	}
}

func TestLoadConflictDecisionAllowsNonAuthoritativeMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conflict-decision.json")
	body := []byte(`{"decision":"resolved","issue":"GH-27","branch_commit":"abc123"}`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	decision, err := loadConflictDecision(path)
	if err != nil {
		t.Fatal(err)
	}
	if decision != "resolved" {
		t.Fatalf("decision = %q, want resolved", decision)
	}
}

func conflictEngine(t *testing.T, decision string) (*Engine, *store.Store, string, *conflictFlowRunner, *countingGitWorktree, string) {
	t.Helper()
	repo := t.TempDir()
	initGitRepo(t, repo)
	originalBase := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD"))
	marker := filepath.Join(t.TempDir(), "gate-replayed")
	gate := filepath.Join(t.TempDir(), "gate")
	if err := os.WriteFile(gate, []byte("#!/bin/sh\nset -eu\ntouch \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := &conflictFlowRunner{repo: repo, decision: decision, gate: []string{gate, marker}}
	ws := &countingGitWorktree{repo: repo}
	f := flow.Flow{Name: "default", Stages: []flow.Stage{
		{Name: "execute", Agents: []flow.AgentRef{{Package: "executor"}},
			Workspace: "worktree", Gate: flow.GateAuto, Completion: flow.CompletionAll},
		{Name: "integrate-safely", Agents: []flow.AgentRef{{Package: "merge-verifier"}},
			Workspace: "worktree", Gate: flow.GateAuto, Completion: flow.CompletionAll,
			MergeBarrier: true,
			Artifacts:    []string{"merge-report.md", "merge-decision.json", "verification.json"}},
	}}
	s, err := store.Open("file:" + strings.ReplaceAll(t.Name(), "/", "-") + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e := New(Config{
		Store: s, Runner: run, Pool: slots.NewPool(1),
		Flows: map[string]flow.Flow{"default": f}, DataDir: t.TempDir(), Workspace: ws,
		Train:              &marshal.Train{Repo: repo, TestCmd: run.gate},
		DecisionIdentities: testDecisionIdentities(),
	})
	return e, s, repo, run, ws, originalBase
}

func TestConflictResolutionUsesOriginalIssueWorktree(t *testing.T) {
	tests := []struct {
		decision string
		wantErr  string
		merged   bool
		replayed bool
	}{
		{decision: "hold", replayed: true},
		{decision: "resolved", merged: true, replayed: true},
		{decision: "invalid", wantErr: "conflict decision", replayed: true},
		{decision: "missing", wantErr: "conflict-decision.json", replayed: true},
	}
	for _, test := range tests {
		t.Run(test.decision, func(t *testing.T) {
			e, s, repo, run, ws, originalBase := conflictEngine(t, test.decision)
			id, err := e.CreateIssue(test.decision, "", "default", levers.Matrix{}, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = e.StartIssue(context.Background(), id)
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("StartIssue error = %v, want %q", err, test.wantErr)
			}
			if ws.acquired != 1 || run.conflictWorkdir != run.originalWorkdir {
				t.Fatalf("acquired=%d original=%q conflict=%q",
					ws.acquired, run.originalWorkdir, run.conflictWorkdir)
			}
			runs, err := s.StageRuns(id)
			if err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{}
			for _, stageRun := range runs {
				counts[stageRun.Stage]++
			}
			if counts["integrate-safely"] != 1 || counts["conflict-resolution"] != 1 {
				t.Fatalf("stage counts = %+v", counts)
			}
			for _, want := range []string{
				"issue/" + id, originalBase, run.currentBase, "diff", "merge conflict",
			} {
				if !strings.Contains(run.conflictContext, want) {
					t.Fatalf("CONFLICT.md missing %q:\n%s", want, run.conflictContext)
				}
			}
			_, markerErr := os.Stat(run.gate[1])
			if (markerErr == nil) != test.replayed {
				t.Fatalf("gate replayed=%v, want %v", markerErr == nil, test.replayed)
			}
			events, _ := s.EventsSince(0)
			sawMerged := false
			for _, event := range events {
				if event.IssueID == id && event.Type == core.EvIssueMerged {
					sawMerged = true
				}
			}
			if sawMerged != test.merged {
				t.Fatalf("merged=%v, want %v", sawMerged, test.merged)
			}
			if !test.merged {
				if branch := gitOutput(t, repo, "branch", "--list", "issue/"+id); strings.TrimSpace(branch) == "" {
					t.Fatal("unmerged conflict branch was deleted")
				}
			}
		})
	}
}

// newEngineOnFile builds an engine on a file-backed store so a second engine
// can be constructed on the same durable state, simulating a daemon restart.
func newEngineOnFile(t *testing.T, s *store.Store, r runner.Runner, dataDir string) *Engine {
	t.Helper()
	return newEngineOnFileWithFlow(t, s, r, dataDir, testFlow())
}

func newEngineOnFileWithFlow(t *testing.T, s *store.Store, r runner.Runner, dataDir string, f flow.Flow) *Engine {
	t.Helper()
	return New(Config{
		Store: s, Runner: r, Pool: slots.NewPool(2),
		Flows:   map[string]flow.Flow{f.Name: f},
		DataDir: dataDir, DecisionIdentities: testDecisionIdentities(),
	})
}

func artifactReviewRunner() *runner.FakeRunner {
	return &runner.FakeRunner{Scripts: map[string]runner.Script{
		"spec/spec-writer":              {Artifacts: map[string]string{"spec.md": "spec v1\n"}},
		"plan/planner":                  {PlannerRequests: plannerArtifactRequests()},
		"implementation/implementation": {},
	}}
}

func TestArtifactReviewRevisionRequiresNewTarget(t *testing.T) {
	f := artifactGateFlow()
	r := artifactReviewRunner()
	e, s := newEngineCfg(t, r, func(cfg *Config) { cfg.Flows = map[string]flow.Flow{f.Name: f} })
	id, err := e.CreateIssue("revise artifact", "", f.Name, levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.StartIssue(context.Background(), id) }()
	spec := waitForPendingStage(t, e, "spec")
	if err := e.Answer(spec.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	plan := waitForPendingStage(t, e, "plan")
	oldVersion := plan.Review.ArtifactVersion
	if err := e.Answer(plan.ID, levers.ChoiceResponse(1)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("revision result = %v", err)
	}
	checkpoints, err := s.StageCheckpoints(id)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoints[len(checkpoints)-1].Status != "revision_required" {
		t.Fatalf("plan checkpoint after revise = %+v", checkpoints[len(checkpoints)-1])
	}
	rows, err := s.ArtifactReviewRows(id)
	if err != nil || len(rows) != 2 || rows[1].Status != "answered" {
		t.Fatalf("review history = %+v, err = %v", rows, err)
	}
	for _, event := range mustEvents(t, s, id) {
		if event.Type == core.EvStageStarted && eventStage(t, event) == "implementation" {
			t.Fatal("implementation started after revision")
		}
	}

	r.Scripts["plan/planner"] = runner.Script{PlannerRequests: plannerArtifactRequests()}
	retryDone := make(chan error, 1)
	go func() { retryDone <- e.RetryStage(context.Background(), id) }()
	newPlan := waitForPendingStage(t, e, "plan")
	if newPlan.Review.ArtifactVersion == oldVersion || newPlan.Review.CheckpointID == plan.Review.CheckpointID {
		t.Fatalf("retry reused review target: old=%+v new=%+v", plan.Review, newPlan.Review)
	}
	if err := e.Answer(newPlan.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	if err := <-retryDone; err != nil {
		t.Fatal(err)
	}
}

func TestArtifactReviewRejectsStaleAnswer(t *testing.T) {
	f := artifactGateFlow()
	e, s := newEngineCfg(t, artifactReviewRunner(), func(cfg *Config) { cfg.Flows = map[string]flow.Flow{f.Name: f} })
	id, err := e.CreateIssue("stale artifact", "", f.Name, levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = e.StartIssue(context.Background(), id) }()
	spec := waitForPendingStage(t, e, "spec")
	if err := e.Answer(spec.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	plan := waitForPendingStage(t, e, "plan")
	changed := append([]contextpack.Artifact(nil), plan.Review.Artifacts...)
	changed[0].SHA256 = strings.Repeat("f", 64)
	if err := s.FinishStageCheckpoint(plan.Review.CheckpointID, "awaiting_review", "", "", "", changed); err != nil {
		t.Fatal(err)
	}
	if err := e.Answer(plan.ID, levers.ChoiceResponse(0)); err == nil {
		t.Fatal("stale artifact answer was accepted")
	}
	if len(e.PendingDecisions()) != 1 {
		t.Fatal("stale answer removed the pending decision")
	}
	for _, event := range mustEvents(t, s, id) {
		if event.Type == core.EvStageCompleted && (eventStage(t, event) == "plan" || eventStage(t, event) == "implementation") ||
			(event.Type == core.EvStageStarted && eventStage(t, event) == "implementation") {
			t.Fatalf("stale answer advanced workflow: %s", event.Type)
		}
	}
}

func TestArtifactReviewPersistenceFailureIsSafeToRetry(t *testing.T) {
	f := artifactGateFlow()
	e, s := newEngineCfg(t, artifactReviewRunner(), func(cfg *Config) { cfg.Flows = map[string]flow.Flow{f.Name: f} })
	id, err := e.CreateIssue("retry answer", "", f.Name, levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.StartIssue(context.Background(), id) }()
	spec := waitForPendingStage(t, e, "spec")
	if err := e.Answer(spec.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	plan := waitForPendingStage(t, e, "plan")
	beforeEvents, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	lastBeforeAnswer := beforeEvents[len(beforeEvents)-1].Seq
	s.FailNextArtifactReviewResolutionForTest()
	if err := e.Answer(plan.ID, levers.ChoiceResponse(0)); err == nil {
		t.Fatal("injected persistence failure was not returned")
	}
	if len(e.PendingDecisions()) != 1 {
		t.Fatal("persistence failure removed the pending decision")
	}
	events, err := s.EventsSince(lastBeforeAnswer)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == core.EvDecisionAnswered || event.Type == core.EvStageCompleted ||
			(event.Type == core.EvStageStarted && eventStage(t, event) == "implementation") {
			t.Fatalf("persistence failure advanced workflow with %s", event.Type)
		}
	}
	if err := e.Answer(plan.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestArtifactReviewSurvivesRestart(t *testing.T) {
	f := artifactGateFlow()
	s, err := store.Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dataDir := t.TempDir()
	r := artifactReviewRunner()
	e1 := newEngineOnFileWithFlow(t, s, r, dataDir, f)
	id, err := e1.CreateIssue("restart review", "", f.Name, levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = e1.StartIssue(context.Background(), id) }()
	pending := waitForPendingStage(t, e1, "spec")
	if !pending.D.RequiresOption {
		t.Fatal("artifact review accepted freeform responses before restart")
	}
	if err := e1.Answer(pending.ID, levers.FreeformResponse("revise this")); err == nil {
		t.Fatal("artifact review accepted freeform response before restart")
	}
	want := *pending.Review
	e2 := newEngineOnFileWithFlow(t, s, artifactReviewRunner(), dataDir, f)
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if got := e2.PendingDecisions(); len(got) != 1 || got[0].Review == nil ||
		!got[0].Review.Matches(want) || !got[0].D.RequiresOption {
		t.Fatalf("rehydrated review = %+v, want %+v", got, want)
	}
	rows, err := s.AllDecisionRows()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Status == "orphaned" {
			t.Fatal("artifact review was orphaned on restart")
		}
	}
	beforeRuns, err := s.StageRuns(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := e2.Answer(pending.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	plan := waitForPendingStage(t, e2, "plan")
	afterRuns, err := s.StageRuns(id)
	if err != nil {
		t.Fatal(err)
	}
	specRuns := 0
	for _, run := range afterRuns {
		if run.Stage == "spec" {
			specRuns++
		}
	}
	if specRuns != 1 {
		t.Fatalf("spec reran after restart: before=%+v after=%+v", beforeRuns, afterRuns)
	}
	if err := e2.Answer(plan.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactReviewRemainsAnswerableWhenRecoveryPageWriteFails(t *testing.T) {
	f := artifactGateFlow()
	s, err := store.Open(filepath.Join(t.TempDir(), "review-page-recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dataDir := t.TempDir()
	e1 := newEngineOnFileWithFlow(t, s, artifactReviewRunner(), dataDir, f)
	id, err := e1.CreateIssue("recover answer route", "", f.Name, levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = e1.StartIssue(context.Background(), id) }()
	want := waitForPendingStage(t, e1, "spec")

	s.FailNextDecisionPageSnapshotForTest()
	restarted := newEngineOnFileWithFlow(t, s, artifactReviewRunner(), dataDir, f)
	if err := restarted.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	pending := restarted.PendingDecisions()
	if len(pending) != 1 || pending[0].ID != want.ID {
		t.Fatalf("rehydrated decisions = %#v, want decision %d", pending, want.ID)
	}
	if err := restarted.Answer(want.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatalf("answer recovered decision: %v", err)
	}
}

func TestAcceptedArtifactReviewCompletesExactlyOnceAfterRestart(t *testing.T) {
	f := artifactGateFlow()
	s, err := store.Open(filepath.Join(t.TempDir(), "accepted-review.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dataDir := t.TempDir()
	e1 := newEngineOnFileWithFlow(t, s, artifactReviewRunner(), dataDir, f)
	id, err := e1.CreateIssue("accepted restart", "", f.Name, levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = e1.StartIssue(context.Background(), id) }()
	pending := waitForPendingStage(t, e1, "spec")
	if _, err := s.ResolveArtifactReview(pending.ID, *pending.Review, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	e2 := newEngineOnFileWithFlow(t, s, artifactReviewRunner(), dataDir, f)
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		events, err := s.EventsSince(0)
		if err != nil {
			t.Fatal(err)
		}
		answered, completed, downstream := 0, 0, 0
		for _, event := range events {
			if event.IssueID != id {
				continue
			}
			switch event.Type {
			case core.EvDecisionAnswered:
				answered++
			case core.EvStageCompleted:
				if eventStage(t, event) == "spec" {
					completed++
				}
			case core.EvStageStarted:
				if eventStage(t, event) == "plan" {
					downstream++
				}
			}
		}
		if answered == 1 && completed == 1 && downstream == 1 {
			plan := waitForPendingStage(t, e2, "plan")
			if err := e2.Answer(plan.ID, levers.ChoiceResponse(0)); err != nil {
				t.Fatal(err)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("accepted review counts = answered %d, completed %d, downstream %d", answered, completed, downstream)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func recoveryDecisionSnapshot(row store.DecisionRow, title string) decisionpage.PageData {
	dec := decisionFromRow(row)
	return decisionpage.PageData{
		IssueID: row.IssueID, Title: title, CurrentStage: row.Stage, DecisionStage: row.Stage,
		StageIndex: 1, StageTotal: 1,
		Floors: []decisionpage.Floor{{Name: row.Stage, Status: decisionpage.FloorCurrent}},
		Briefing: buildDecisionPageBriefing(
			&dec, row.Context, row.Review, row.Stage, row.ID, nil,
		),
		TouchsetMissing: true, EvidenceMissing: true,
	}
}

func TestResolvedArtifactReviewPageRebuiltAfterRestart(t *testing.T) {
	f := artifactGateFlow()
	s, err := store.Open(filepath.Join(t.TempDir(), "resolved-review.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dataDir := t.TempDir()
	e1 := newEngineOnFileWithFlow(t, s, artifactReviewRunner(), dataDir, f)
	id, err := e1.CreateIssue("resolved review page", "", f.Name, levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = e1.StartIssue(context.Background(), id) }()
	pending := waitForPendingStage(t, e1, "spec")
	if _, err := s.ResolveArtifactReview(pending.ID, *pending.Review, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.AllDecisionRows()
	if err != nil {
		t.Fatal(err)
	}
	var resolved store.DecisionRow
	for _, row := range rows {
		if row.ID == pending.ID {
			resolved = row
			break
		}
	}
	if resolved.AnsweredAt.IsZero() {
		t.Fatal("resolved review has no durable answer time")
	}

	e2 := newEngineOnFileWithFlow(t, s, artifactReviewRunner(), dataDir, f)
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	pagePath := filepath.Join(dataDir, id, "decisions", fmt.Sprintf("%d.html", pending.ID))
	page := string(waitForDecisionPageFile(t, pagePath))
	for _, want := range []string{
		"Answered: option 1 · " + resolved.AnsweredAt.UTC().Format("2006-01-02 15:04"),
		"approve was selected", "spec.md was archived and available at decision time",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("rehydrated review page missing %q: %s", want, page)
		}
	}
	for _, misleading := range []string{
		"Do this now", "ready for review", "After you answer", "archived and reviewed",
	} {
		if strings.Contains(page, misleading) {
			t.Errorf("rehydrated review page contains misleading %q: %s", misleading, page)
		}
	}
}

func TestRehydratePreservesFinalizedDecisionArchive(t *testing.T) {
	f := flow.Flow{Name: "preserve-finalized-archive", Stages: []flow.Stage{{Name: "execute"}}}
	s, err := store.Open(filepath.Join(t.TempDir(), "preserve-finalized-archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const issueID = "GH-1"
	if err := s.UpsertIssue(store.IssueRow{
		ID: issueID, Title: "current title", State: "done", Flow: f.Name,
		Levers: map[string]string{"execute": string(flow.LeverStrict)},
	}); err != nil {
		t.Fatal(err)
	}
	decisionID, err := s.InsertDecision(store.DecisionRow{
		IssueID: issueID, Stage: "execute", Kind: levers.DecisionChoice,
		Question: "Continue?", Options: []string{"continue", "stop"}, Recommended: 0,
		Status: "answered", Response: levers.ChoiceResponse(0),
		CreatedAt: time.Now().Add(-time.Minute), AnsweredAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := decisionpage.PageData{IssueID: issueID, Title: "decision-time title"}
	if err := s.SaveDecisionPageSnapshot(decisionID, snapshot); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	archivePath := filepath.Join(dataDir, issueID, "decisions", fmt.Sprintf("%d.html", decisionID))
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte(`<!doctype html><body data-decision-state="resolved">decision-time sentinel</body>`)
	if err := os.WriteFile(archivePath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	e := newEngineOnFileWithFlow(t, s, &runner.FakeRunner{}, dataDir, f)
	if err := e.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("finalized archive was replaced:\n%s", got)
	}
}

func TestRehydrateReportsUnreadableDecisionArchive(t *testing.T) {
	f := flow.Flow{Name: "unreadable-archive", Stages: []flow.Stage{{Name: "execute"}}}
	s, err := store.Open(filepath.Join(t.TempDir(), "unreadable-archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const issueID = "GH-1"
	if err := s.UpsertIssue(store.IssueRow{
		ID: issueID, Title: "unreadable archive", State: "done", Flow: f.Name,
		Levers: map[string]string{"execute": string(flow.LeverStrict)},
	}); err != nil {
		t.Fatal(err)
	}
	answeredAt := time.Now().UTC()
	row := store.DecisionRow{
		IssueID: issueID, Stage: "execute", Kind: levers.DecisionChoice,
		Question: "Continue?", Options: []string{"continue", "stop"}, Recommended: 0,
		Status: "answered", Response: levers.ChoiceResponse(0),
		CreatedAt: answeredAt.Add(-time.Minute), AnsweredAt: answeredAt,
	}
	snapshot := recoveryDecisionSnapshot(row, "unreadable archive")
	row.PageSnapshot = &snapshot
	decisionID, err := s.InsertDecision(row)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	archivePath := filepath.Join(dataDir, issueID, "decisions", fmt.Sprintf("%d.html", decisionID))
	if err := os.MkdirAll(archivePath, 0o755); err != nil {
		t.Fatal(err)
	}

	e := newEngineOnFileWithFlow(t, s, &runner.FakeRunner{}, dataDir, f)
	if err := e.Rehydrate(); err == nil || !strings.Contains(err.Error(), "decision archive") {
		t.Fatalf("Rehydrate error = %v, want decision archive read error", err)
	}
}

func TestRehydrateResolvesArchiveFromSnapshot(t *testing.T) {
	f := flow.Flow{Name: "snapshot-archive", Stages: []flow.Stage{{Name: "renamed-stage"}}}
	s, err := store.Open(filepath.Join(t.TempDir(), "snapshot-archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const issueID = "GH-1"
	if err := s.UpsertIssue(store.IssueRow{
		ID: issueID, Title: "current title", State: "done", Flow: f.Name,
		Levers: map[string]string{"renamed-stage": string(flow.LeverStrict)},
	}); err != nil {
		t.Fatal(err)
	}
	decisionID, err := s.InsertDecision(store.DecisionRow{
		IssueID: issueID, Stage: "execute", Kind: levers.DecisionChoice,
		Question: "Continue?", Options: []string{"continue", "stop"}, Recommended: 0,
		Why: "Decision-time reason.", Consequences: []string{"Continue.", "Stop."},
		Reversible: "Stopping preserves the current result.", Status: "answered",
		Response: levers.ChoiceResponse(0), CreatedAt: time.Now().Add(-time.Minute), AnsweredAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := decisionpage.PageData{
		IssueID: issueID, Title: "decision-time title", CurrentStage: "execute",
		DecisionStage: "execute", StageIndex: 1, StageTotal: 1,
		Floors: []decisionpage.Floor{{Name: "execute", Status: decisionpage.FloorCurrent}},
		Briefing: &decisionpage.Briefing{
			Question: "Continue?", Reversible: "Stopping preserves the current result.",
			Action: "Choose an option.", Recommendation: "continue", RecommendationWhy: "Decision-time reason.",
			Options:      []decisionpage.Option{{Key: "1", Label: "continue", OneLiner: "Continue.", Recommended: true}},
			ProofMissing: true, AfterAnswer: "Continue or stop.",
		},
		TouchsetGlobs: []string{"decision-time/**"}, EvidenceMissing: true,
	}
	if err := s.SaveDecisionPageSnapshot(decisionID, snapshot); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	archivePath := filepath.Join(dataDir, issueID, "decisions", fmt.Sprintf("%d.html", decisionID))
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePath, []byte(`<!doctype html><body data-decision-state="pending">pending</body>`), 0o644); err != nil {
		t.Fatal(err)
	}

	e := newEngineOnFileWithFlow(t, s, &runner.FakeRunner{}, dataDir, f)
	if err := e.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	page := string(waitForDecisionPageFile(t, archivePath))
	for _, want := range []string{"Recorded outcome", "continue was selected", "decision-time/**"} {
		if !strings.Contains(page, want) {
			t.Errorf("resolved snapshot archive missing %q: %s", want, page)
		}
	}
	if strings.Contains(page, "renamed-stage") {
		t.Fatalf("resolved snapshot archive used current flow state: %s", page)
	}
}

func TestResolvedOrdinaryDecisionPageRebuiltAfterRestart(t *testing.T) {
	f := flow.Flow{Name: "ordinary-decision-recovery", Stages: []flow.Stage{{Name: "execute"}}}
	s, err := store.Open(filepath.Join(t.TempDir(), "resolved-ordinary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const issueID = "GH-1"
	if err := s.UpsertIssue(store.IssueRow{
		ID: issueID, Title: "resolved ordinary decision", State: "done", Flow: f.Name,
		Levers: map[string]string{"execute": string(flow.LeverStrict)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertStageCheckpoint(store.StageCheckpoint{
		IssueID: issueID, Stage: "execute", Status: "succeeded",
	}); err != nil {
		t.Fatal(err)
	}
	answeredAt := time.Date(2026, time.August, 6, 12, 34, 0, 0, time.UTC)
	decisionID, err := s.InsertDecision(store.DecisionRow{
		IssueID: issueID, Stage: "execute", Kind: levers.DecisionChoice,
		Question: "Continue the rollout?", Options: []string{"continue", "stop"}, Recommended: 0,
		Why: "The rollout is ready.", Consequences: []string{"Continue rollout.", "Stop rollout."},
		Reversible: "Stopping preserves completed work.", RequiresOption: true, Status: "answered",
		Response: levers.ChoiceResponse(0), CreatedAt: answeredAt.Add(-5 * time.Minute), AnsweredAt: answeredAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyDecisionID, err := s.InsertDecision(store.DecisionRow{
		IssueID: issueID, Stage: "execute", Kind: levers.DecisionChoice,
		Question: "Legacy answer?", Options: []string{"continue", "stop"}, Recommended: 0,
		Why: "Historical context.", Consequences: []string{"Continue.", "Stop."},
		Reversible: "The choice can be revisited.", Status: "answered",
		Response: levers.ChoiceResponse(1), CreatedAt: answeredAt.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	storedRows, err := s.AllDecisionRows()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range storedRows {
		if err := s.SaveDecisionPageSnapshot(row.ID, recoveryDecisionSnapshot(row, "resolved ordinary decision")); err != nil {
			t.Fatal(err)
		}
	}
	dataDir := t.TempDir()
	e := newEngineOnFileWithFlow(t, s, &runner.FakeRunner{}, dataDir, f)
	if err := e.Rehydrate(); err != nil {
		t.Fatal(err)
	}

	archive, err := os.ReadFile(filepath.Join(dataDir, issueID, "decisions", fmt.Sprintf("%d.html", decisionID)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Continue the rollout?", "Recorded outcome",
		"Answered: option 1 · " + answeredAt.Format("2006-01-02 15:04"),
	} {
		if !strings.Contains(string(archive), want) {
			t.Errorf("rebuilt ordinary decision archive missing %q: %s", want, archive)
		}
	}
	if strings.Contains(string(archive), "Add feedback") {
		t.Errorf("rebuilt option-required decision archive offers feedback: %s", archive)
	}
	legacyArchive, err := os.ReadFile(filepath.Join(dataDir, issueID, "decisions", fmt.Sprintf("%d.html", legacyDecisionID)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(legacyArchive), "Answered: option 2 · timestamp unavailable") {
		t.Fatalf("legacy archive invented an answer timestamp: %s", legacyArchive)
	}
	stable, err := os.ReadFile(filepath.Join(dataDir, issueID, decisionpage.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stable), "1 stages done ✓") || strings.Contains(string(stable), "Recorded outcome") {
		t.Fatalf("stable page does not show current progress: %s", stable)
	}
}

func TestRehydrateRebuildsEveryResolvedArtifactReviewPage(t *testing.T) {
	f := artifactGateFlow()
	s, err := store.Open(filepath.Join(t.TempDir(), "all-resolved-review-pages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const issueID = "GH-1"
	if err := s.UpsertIssue(store.IssueRow{
		ID: issueID, Title: "all resolved review pages", State: "done", Flow: f.Name,
		Levers: map[string]string{
			"spec": string(flow.LeverStrict), "plan": string(flow.LeverStrict),
			"implementation": string(flow.LeverStrict),
		},
	}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		stage        string
		nextStage    string
		artifactName string
		blockingCost int
	}{
		{stage: "spec", nextStage: "plan", artifactName: "spec.md", blockingCost: 1},
		{stage: "plan", nextStage: "implementation", artifactName: "plan.md", blockingCost: 2},
	}
	decisionIDs := make(map[string]int64, len(tests))
	for index, test := range tests {
		artifacts := []contextpack.Artifact{{
			Name: test.artifactName, SHA256: strings.Repeat(strconv.Itoa(index+1), 64),
		}}
		checkpointID, err := s.InsertStageCheckpoint(store.StageCheckpoint{
			IssueID: issueID, Stage: test.stage, Status: "awaiting_review", Artifacts: artifacts,
		})
		if err != nil {
			t.Fatal(err)
		}
		target, err := (review.Target{
			IssueID: issueID, Stage: test.stage, CheckpointID: checkpointID,
			Artifacts: artifacts, NextStage: test.nextStage,
		}).Canonical()
		if err != nil {
			t.Fatal(err)
		}
		decision := artifactReviewDecision(target, false)
		decisionID, err := s.RequestArtifactReview(target, store.DecisionRow{
			IssueID: issueID, Stage: test.stage, Question: decision.Question,
			Options: decision.Options, Recommended: decision.Recommended, Kind: decision.Kind,
			Importance: decision.Importance, Why: decision.Why, Consequences: decision.Consequences,
			Reversible: decision.Reversible, Briefing: decision.Briefing, BlockingCost: test.blockingCost,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SaveDecisionPageSnapshot(decisionID, recoveryDecisionSnapshot(store.DecisionRow{
			ID: decisionID, IssueID: issueID, Stage: test.stage, Kind: decision.Kind,
			Question: decision.Question, Options: decision.Options, Recommended: decision.Recommended,
			Importance: decision.Importance, Why: decision.Why, Consequences: decision.Consequences,
			Reversible: decision.Reversible, Briefing: decision.Briefing, Review: &target,
		}, "all resolved review pages")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ResolveArtifactReview(decisionID, target, levers.ChoiceResponse(0)); err != nil {
			t.Fatal(err)
		}
		if err := s.CompleteArtifactReview(checkpointID, target); err != nil {
			t.Fatal(err)
		}
		decisionIDs[test.stage] = decisionID
	}
	if _, err := s.InsertStageCheckpoint(store.StageCheckpoint{
		IssueID: issueID, Stage: "implementation", Status: "succeeded",
	}); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	started := make(chan string, 1)
	r := artifactReviewRunner()
	r.OnStart = func(_, stage, _, _ string) error {
		started <- stage
		return nil
	}
	e := newEngineOnFileWithFlow(t, s, r, dataDir, f)
	if err := e.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	select {
	case stage := <-started:
		t.Fatalf("rehydrate restarted completed workflow at %s", stage)
	case <-time.After(100 * time.Millisecond):
	}
	for _, event := range mustEvents(t, s, issueID) {
		if event.Type == core.EvDecisionAnswered || event.Type == core.EvStageCompleted {
			t.Fatalf("rehydrate continued completed workflow with %s", event.Type)
		}
	}
	for _, test := range tests {
		pagePath := filepath.Join(dataDir, issueID, "decisions", fmt.Sprintf("%d.html", decisionIDs[test.stage]))
		page, err := os.ReadFile(pagePath)
		if err != nil {
			t.Errorf("read %s review page: %v", test.stage, err)
			continue
		}
		if !strings.Contains(string(page), test.artifactName+" was archived and available at decision time") {
			t.Errorf("%s review page has the wrong archive: %s", test.stage, page)
		}
	}
	stable, err := os.ReadFile(filepath.Join(dataDir, issueID, decisionpage.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stable), "Stage <b>3 of 3</b> — implementation") ||
		!strings.Contains(string(stable), "3 stages done ✓") ||
		strings.Contains(string(stable), "Recorded outcome") {
		t.Fatalf("stable page was replaced by a historical decision archive: %s", stable)
	}
}

func TestPolicyApprovedReviewPageRebuiltAfterRestart(t *testing.T) {
	f := planReviewFlow()
	s, err := store.Open(filepath.Join(t.TempDir(), "policy-review.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dataDir := t.TempDir()
	e1 := New(Config{
		Store: s, Runner: planReviewRunner(), Pool: slots.NewPool(2),
		Flows: map[string]flow.Flow{f.Name: f}, DataDir: dataDir,
		DecisionIdentities: testDecisionIdentities(),
		PlanReview:         planReviewSettings("team-ci", "2026-08-03", true),
	})
	id, err := e1.CreateIssue("policy review page", "", f.Name, levers.Matrix{
		"plan": flow.LeverRegular, "execute": flow.LeverYolo,
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e1.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	rows, err := s.AllDecisionRows()
	if err != nil {
		t.Fatal(err)
	}
	var resolved store.DecisionRow
	for _, row := range rows {
		if row.IssueID == id && row.Status == "auto" {
			resolved = row
			break
		}
	}
	if resolved.ID == 0 || resolved.AnsweredAt.IsZero() {
		t.Fatalf("policy review row = %+v", resolved)
	}
	pagePath := filepath.Join(dataDir, id, "decisions", fmt.Sprintf("%d.html", resolved.ID))
	if err := os.Remove(pagePath); err != nil {
		t.Fatal(err)
	}

	e2 := newEngineOnFileWithFlow(t, s, planReviewRunner(), dataDir, f)
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	page := string(waitForDecisionPageFile(t, pagePath))
	for _, want := range []string{
		"Automatically approved by policy team-ci@2026-08-03: option 1 · " +
			resolved.AnsweredAt.UTC().Format("2006-01-02 15:04"),
		"policy team-ci@2026-08-03 automatically selected approve",
		"plan.md was archived and automatically authorized by policy team-ci@2026-08-03",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("rehydrated policy page missing %q: %s", want, page)
		}
	}
}

func TestAcceptedArtifactReviewRestartRestoresPlanTouchset(t *testing.T) {
	f := flow.Flow{Name: "restart-marshal", Stages: []flow.Stage{
		{Name: "plan", Agents: []flow.AgentRef{{Package: "planner"}}, Workspace: "worktree",
			Gate: flow.GateApproveArtifact, Artifacts: []string{"touchset.json"}},
		{Name: "merge", Agents: []flow.AgentRef{{Package: "reviewer"}}, Workspace: "worktree",
			Gate: flow.GateAuto, MergeBarrier: true, Artifacts: append([]string(nil), flow.FinalizationArtifacts...)},
	}}
	repo := t.TempDir()
	initGitRepo(t, repo)
	s, err := store.Open(filepath.Join(t.TempDir(), "restart-marshal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dataDir := t.TempDir()
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"plan/planner": {Artifacts: map[string]string{"touchset.json": `{"globs":["src/**"]}`}},
		"merge/reviewer": {Artifacts: map[string]string{
			"merge-report.md": "", "merge-decision.json": "", "verification.json": "",
		}},
	}}
	seq1 := &recordingSequencer{}
	e1 := New(Config{
		Store: s, Runner: r, Marshal: seq1, Pool: slots.NewPool(1),
		Flows: map[string]flow.Flow{f.Name: f}, DataDir: dataDir,
		Workspace: workspace.GitWorktree{Repo: repo}, DecisionIdentities: testDecisionIdentities(),
	})
	id, err := e1.CreateIssue("restart marshal", "", f.Name, levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = e1.StartIssue(context.Background(), id) }()
	pending := waitForPendingStage(t, e1, "plan")
	if _, err := s.ResolveArtifactReview(pending.ID, *pending.Review, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}

	planned := make(chan touchset.Set, 1)
	seq2 := &recordingSequencer{plannedC: planned}
	e2 := New(Config{
		Store: s, Runner: r, Marshal: seq2, Pool: slots.NewPool(1),
		Flows: map[string]flow.Flow{f.Name: f}, DataDir: dataDir,
		Workspace: workspace.GitWorktree{Repo: repo}, DecisionIdentities: testDecisionIdentities(),
	})
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-planned:
		if len(got.Globs) != 1 || got.Globs[0] != "src/**" {
			t.Fatalf("restored plan touchset = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("accepted plan restart did not register its touchset")
	}
}

func mustEvents(t *testing.T, s *store.Store, issueID string) []core.Event {
	t.Helper()
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	filtered := make([]core.Event, 0, len(events))
	for _, event := range events {
		if event.IssueID == issueID {
			filtered = append(filtered, event)
		}
	}
	return filtered
}

func TestRehydrateAfterDaemonRestart(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "gh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	dataDir := t.TempDir()
	legacyFlow := flow.Flow{Name: "legacy", Stages: []flow.Stage{{
		Name: "ask", Agents: []flow.AgentRef{{Package: "agent"}}, Workspace: "none",
		Completion: flow.CompletionAll, Gate: flow.GateAuto,
	}}}
	legacyRunner := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"ask/agent": {Asks: []levers.Decision{{
			Question: "Legacy approval?", Options: []string{"yes", "no"}, Recommended: 0, Importance: 1.0,
		}}},
	}}

	// Engine 1: run until a legacy non-artifact decision parks.
	e1 := newEngineOnFileWithFlow(t, s, legacyRunner, dataDir, legacyFlow)
	id, err := e1.CreateIssue("restart me", "", legacyFlow.Name, levers.Preset(legacyFlow, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = e1.StartIssue(context.Background(), id) }()
	deadline := time.After(5 * time.Second)
	for len(e1.PendingDecisions()) != 1 {
		select {
		case <-deadline:
			t.Fatal("gate decision never appeared")
		case <-time.After(10 * time.Millisecond):
		}
	}
	beforeRestart := e1.PendingDecisions()[0]
	if beforeRestart.Context == nil {
		t.Fatal("pre-restart decision has no context")
	}
	wantContext := *beforeRestart.Context
	wantQuestion := beforeRestart.D.Question

	// "Restart": a fresh engine on the same store knows nothing in memory.
	e2 := newEngineOnFileWithFlow(t, s, &runner.FakeRunner{Scripts: legacyRunner.Scripts}, dataDir, legacyFlow)
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if err := e2.Rehydrate(); err != nil { // idempotent
		t.Fatal(err)
	}

	// nextID advanced past existing issues: no GH-1 collision.
	id2, err := e2.CreateIssue("after restart", "", legacyFlow.Name, levers.Preset(legacyFlow, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id2 == id {
		t.Fatalf("issue ID collision after restart: %s", id2)
	}

	// Orphaned decision closed, not resurrected.
	if ds := e2.PendingDecisions(); len(ds) != 0 {
		t.Fatalf("expected no pending decisions after rehydrate, got %d", len(ds))
	}
	rows, err := s.AllDecisionRows()
	if err != nil {
		t.Fatal(err)
	}
	orphaned := 0
	for _, row := range rows {
		if row.IssueID == id && row.Status == "orphaned" {
			orphaned++
		}
	}
	if orphaned != 1 {
		t.Fatalf("expected 1 orphaned decision for %s, got %d (%+v)", id, orphaned, rows)
	}
	var historical *store.DecisionRow
	for i := range rows {
		if rows[i].IssueID == id && rows[i].Question == wantQuestion {
			historical = &rows[i]
			break
		}
	}
	if historical == nil || historical.Context == nil || *historical.Context != wantContext {
		t.Fatalf("historical decision context = %#v, want %#v", historical, wantContext)
	}

	// Events: decision answered (orphaned) then a final stage failure marker.
	evs, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var answeredSeq, failedSeq int64
	for _, ev := range evs {
		if ev.IssueID != id {
			continue
		}
		var p map[string]any
		_ = json.Unmarshal(ev.Payload, &p)
		if ev.Type == core.EvDecisionRequired {
			encoded, _ := json.Marshal(p["context"])
			var got decision.DecisionContext
			if err := json.Unmarshal(encoded, &got); err != nil || got != wantContext {
				t.Fatalf("replayed required context = %s, want %#v", encoded, wantContext)
			}
		}
		switch ev.Type {
		case core.EvDecisionAnswered:
			if orphanedFlag, _ := p["orphaned"].(bool); orphanedFlag {
				answeredSeq = ev.Seq
			}
		case core.EvStageFailed:
			if msg, _ := p["error"].(string); strings.Contains(msg, "daemon restarted") {
				failedSeq = ev.Seq
			}
		}
	}
	if answeredSeq == 0 || failedSeq == 0 || answeredSeq > failedSeq {
		t.Fatalf("expected orphaned answer before restart failure marker, got answered=%d failed=%d", answeredSeq, failedSeq)
	}

	// The issue is retryable: no "unknown issue", and with the re-raised gate
	// answered the flow completes.
	contextAfterRetry := make(chan decision.DecisionContext, 1)
	go func() {
		deadline := time.After(5 * time.Second)
		for {
			if ds := e2.PendingDecisions(); len(ds) == 1 {
				if ds[0].Context != nil {
					contextAfterRetry <- *ds[0].Context
				}
				_ = e2.Answer(ds[0].ID, levers.ChoiceResponse(0))
				return
			}
			select {
			case <-deadline:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	if err := e2.RetryStage(context.Background(), id); err != nil {
		t.Fatalf("retry after rehydrate: %v", err)
	}
	evs, _ = s.EventsSince(0)
	completed := false
	for _, ev := range evs {
		if ev.IssueID == id && ev.Type == core.EvIssueCompleted {
			completed = true
		}
	}
	if !completed {
		t.Fatal("issue did not complete after rehydrated retry")
	}
	select {
	case got := <-contextAfterRetry:
		if got != wantContext {
			t.Fatalf("retried decision context = %#v, want %#v", got, wantContext)
		}
	default:
		t.Fatal("retried decision did not expose context")
	}
}

func TestRehydrateRunnerAttempts(t *testing.T) {
	f := flow.Flow{Name: "restart-attempts", Stages: []flow.Stage{{
		Name: "execute", Agents: []flow.AgentRef{{Package: "executor"}},
		Workspace: "none", Completion: flow.CompletionAll, Gate: flow.GateAuto,
	}}}
	cases := []struct {
		name              string
		states            []runner.AttemptState
		wantFinalFailure  bool
		wantTerminalState runner.AttemptState
	}{
		{name: "primary failed before fallback", states: []runner.AttemptState{runner.AttemptFailed}, wantFinalFailure: true},
		{name: "fallback interrupted", states: []runner.AttemptState{runner.AttemptFailed, runner.AttemptReserved, runner.AttemptRunning}, wantFinalFailure: true, wantTerminalState: runner.AttemptTerminal},
		{name: "fallback already succeeded", states: []runner.AttemptState{runner.AttemptFailed, runner.AttemptReserved, runner.AttemptRunning, runner.AttemptSucceeded}, wantFinalFailure: true, wantTerminalState: runner.AttemptSucceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := store.Open(filepath.Join(t.TempDir(), "gh.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			starts := 0
			restartedRunner := &runner.FakeRunner{Scripts: map[string]runner.Script{"execute/executor": {SessionID: "retry-session"}}}
			restartedRunner.OnStart = func(_, _, _, _ string) error {
				starts++
				return nil
			}
			first := newEngineOnFileWithFlow(t, s, restartedRunner, t.TempDir(), f)
			id, err := first.DraftIssue("restart attempt", "", f.Name, "regular", levers.Preset(f, flow.LeverYolo), 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			row := issueRowByID(t, mustIssues(t, s), id)
			row.State = "running"
			if err := s.UpsertIssue(row); err != nil {
				t.Fatal(err)
			}
			runID, err := s.InsertStageRun(store.StageRun{IssueID: id, Stage: "execute", Agent: "executor", Status: "running"})
			if err != nil {
				t.Fatal(err)
			}
			for index, state := range tc.states {
				kind := runner.AttemptPrimary
				if index > 0 {
					kind = runner.AttemptFallback
				}
				if err := s.RecordAttempt(context.Background(), runner.Attempt{
					OperationID: strconv.FormatInt(runID, 10), IssueID: id, Stage: "execute", AgentPackage: "executor",
					Kind: kind, State: state, FailureClass: runner.FailureExecution,
					RedactedArgv: []string{"exec", "features.unified_exec=false", "[redacted-prompt]"},
				}); err != nil {
					t.Fatal(err)
				}
			}

			restarted := newEngineOnFileWithFlow(t, s, restartedRunner, t.TempDir(), f)
			if err := restarted.Rehydrate(); err != nil {
				t.Fatal(err)
			}
			if starts != 0 {
				t.Fatalf("rehydrate launched runner %d times", starts)
			}
			attempts, err := s.LoadOperation(context.Background(), strconv.FormatInt(runID, 10))
			if err != nil {
				t.Fatal(err)
			}
			wantAttempts := 1
			if len(tc.states) > 1 {
				wantAttempts = 2
			}
			if len(attempts) != wantAttempts {
				t.Fatalf("attempt history = %+v, want %d attempt records", attempts, wantAttempts)
			}
			if tc.wantTerminalState != "" && attempts[len(attempts)-1].State != tc.wantTerminalState {
				t.Fatalf("latest attempt = %+v, want %s", attempts[len(attempts)-1], tc.wantTerminalState)
			}
			if attempts[len(attempts)-1].RedactedArgv[2] != "[redacted-prompt]" {
				t.Fatalf("redacted attempt = %+v", attempts[len(attempts)-1])
			}
			events, err := s.EventsSince(0)
			if err != nil {
				t.Fatal(err)
			}
			finalFailures := 0
			for _, event := range events {
				if event.IssueID == id && event.Type == core.EvStageFailed {
					finalFailures++
				}
			}
			if (finalFailures > 0) != tc.wantFinalFailure {
				t.Fatalf("stage failure count = %d, want failure=%t", finalFailures, tc.wantFinalFailure)
			}

			if tc.name == "primary failed before fallback" {
				if err := restarted.RetryStage(context.Background(), id); err != nil {
					t.Fatal(err)
				}
				runs, err := s.StageRuns(id)
				if err != nil || len(runs) != 2 || strconv.FormatInt(runs[0].ID, 10) == strconv.FormatInt(runs[1].ID, 10) {
					t.Fatalf("explicit retry stage runs = %+v, err = %v", runs, err)
				}
				if starts != 1 {
					t.Fatalf("explicit retry launched runner %d times", starts)
				}
			}
		})
	}
}

func TestRehydrateDoesNotCompleteStageFromRunnerSuccessAlone(t *testing.T) {
	f := flow.Flow{Name: "restart-gates", Stages: []flow.Stage{{
		Name: "execute", Agents: []flow.AgentRef{{Package: "executor"}},
		Workspace: "none", Completion: flow.CompletionAll, Gate: flow.GateApproveArtifact,
		Artifacts: []string{"result.md"},
	}}}
	s, err := store.Open(filepath.Join(t.TempDir(), "gh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	id := "GH-38"
	if err := s.UpsertIssue(store.IssueRow{ID: id, Title: "interrupted after runner success", Flow: f.Name, State: "running"}); err != nil {
		t.Fatal(err)
	}
	runID, err := s.InsertStageRun(store.StageRun{IssueID: id, Stage: "execute", Agent: "executor", Status: "succeeded"})
	if err != nil {
		t.Fatal(err)
	}
	operationID := strconv.FormatInt(runID, 10)
	for _, attempt := range []runner.Attempt{
		{OperationID: operationID, IssueID: id, Stage: "execute", AgentPackage: "executor", Kind: runner.AttemptPrimary, State: runner.AttemptFailed, FailureClass: runner.FailureLaunch},
		{OperationID: operationID, IssueID: id, Stage: "execute", AgentPackage: "executor", Kind: runner.AttemptFallback, State: runner.AttemptSucceeded, FailureClass: runner.FailureLaunch},
	} {
		if err := s.RecordAttempt(context.Background(), attempt); err != nil {
			t.Fatal(err)
		}
	}
	restarted := newEngineOnFileWithFlow(t, s, &runner.FakeRunner{}, t.TempDir(), f)
	if err := restarted.Rehydrate(); err != nil {
		t.Fatal(err)
	}

	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var sawFailure, sawCompletion bool
	for _, event := range events {
		if event.IssueID != id {
			continue
		}
		switch event.Type {
		case core.EvStageFailed:
			var payload map[string]any
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			sawFailure = payload["final"] == true
		case core.EvStageCompleted:
			sawCompletion = true
		}
	}
	if sawCompletion {
		t.Fatal("rehydration marked a runner success as a completed stage before applying artifact gates")
	}
	if !sawFailure {
		t.Fatal("rehydration did not expose an interrupted stage as a terminal retryable failure")
	}
}

func TestDecisionWireEnvelopeFailsBeforePresentation(t *testing.T) {
	f := flow.Flow{Name: "context", Stages: []flow.Stage{{
		Name: "ask", Completion: flow.CompletionAll, Workspace: "none", Gate: flow.GateAuto,
		Agents: []flow.AgentRef{{Package: "agent"}},
	}}}
	decisionValue := levers.Decision{
		Question: strings.Repeat("q", 1000), Options: []string{"yes"}, Recommended: 0, Importance: 1.0,
	}
	title := strings.Repeat("x", decision.MaxMessageBytes-300)
	summary, err := decision.BuildTaskSummary(title, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := &decision.DecisionContext{
		TaskSummary: summary, AgentName: "Test Agent", AgentColor: "gray", AgentSymbol: "A",
	}
	if err := decision.ValidateDecisionContext(*ctx); err != nil {
		t.Fatalf("context should fit on its own: %v", err)
	}
	candidate, err := json.Marshal(PendingDecision{D: decisionValue, Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidate) <= decision.MaxMessageBytes {
		t.Fatalf("test candidate is only %d bytes; expected it to exceed %d", len(candidate), decision.MaxMessageBytes)
	}

	e, s := newEngineCfg(t, &runner.FakeRunner{Scripts: map[string]runner.Script{
		"ask/agent": {Asks: []levers.Decision{decisionValue}},
	}}, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{"context": f}
	})
	id, err := e.CreateIssue(title, "", "context", levers.Matrix{"ask": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.StartIssue(context.Background(), id) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "message budget") {
			t.Fatalf("StartIssue error = %v, want complete-envelope budget error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("StartIssue blocked instead of rejecting the oversized decision envelope")
	}
	if rows, err := s.PendingDecisionRows(); err != nil || len(rows) != 0 {
		t.Fatalf("pending rows = %#v, err = %v", rows, err)
	}
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == core.EvDecisionRequired {
			t.Fatal("oversized decision envelope emitted a required decision")
		}
	}
}

func TestRetryAfterRestartReusesRecordedIssueWorktree(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	if out, err := exec.Command("git", "-C", repo, "checkout", "-qb", "issue/GH-1").CombinedOutput(); err != nil {
		t.Fatalf("create issue branch: %v: %s", err, out)
	}
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.UpsertIssue(store.IssueRow{
		ID: "GH-1", Title: "interrupted", Flow: "default", State: "running:execute",
	}); err != nil {
		t.Fatal(err)
	}
	started, err := core.NewEvent(core.EvStageStarted, "GH-1", map[string]any{
		"stage": "execute", "attempt": 1, "of": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(started); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertStageRun(store.StageRun{
		IssueID: "GH-1", Stage: "execute", Agent: "executor",
		Worktree: repo, Status: "succeeded",
	}); err != nil {
		t.Fatal(err)
	}
	ws := &fakeWS{dir: repo}
	f := flow.Flow{Name: "default", Stages: []flow.Stage{{
		Name: "execute", Agents: []flow.AgentRef{{Package: "executor"}},
		Workspace: "worktree", Gate: flow.GateAuto, Completion: flow.CompletionAll,
	}}}
	e := New(Config{
		Store: s, Runner: &runner.FakeRunner{Scripts: map[string]runner.Script{
			"execute/executor": {},
		}}, Pool: slots.NewPool(1), Flows: map[string]flow.Flow{"default": f},
		DataDir: t.TempDir(), Workspace: ws, DecisionIdentities: testDecisionIdentities(),
	})
	if err := e.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if err := e.RetryStage(context.Background(), "GH-1"); err != nil {
		t.Fatal(err)
	}
	if ws.acquired != 0 {
		t.Fatalf("retry acquired a replacement worktree %d times", ws.acquired)
	}
	runs, err := s.StageRuns("GH-1")
	if err != nil || len(runs) != 2 || runs[1].Worktree != repo {
		t.Fatalf("runs = %+v err=%v", runs, err)
	}
}

func TestRehydrateSkipsUnknownFlow(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "gh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.UpsertIssue(store.IssueRow{ID: "GH-7", Title: "ghost", Flow: "gone", State: "running:plan"}); err != nil {
		t.Fatal(err)
	}
	e := newEngineOnFile(t, s, &runner.FakeRunner{Scripts: scripts()}, t.TempDir())
	if err := e.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	err = e.RetryStage(context.Background(), "GH-7")
	if err == nil || !strings.Contains(err.Error(), "unknown issue") {
		t.Fatalf("expected unknown issue for unloadable flow, got %v", err)
	}
	// nextID still advanced past GH-7.
	id, err := e.CreateIssue("new", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id != "GH-8" {
		t.Fatalf("expected GH-8 after GH-7, got %s", id)
	}
}

func TestAbandonRehydratedIssue(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "gh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	dataDir := t.TempDir()
	e1 := newEngineOnFile(t, s, &runner.FakeRunner{Scripts: scripts()}, dataDir)
	id, err := e1.CreateIssue("doomed", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = e1.StartIssue(context.Background(), id) }()
	deadline := time.After(5 * time.Second)
	for len(e1.PendingDecisions()) != 1 {
		select {
		case <-deadline:
			t.Fatal("gate decision never appeared")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Restart, rehydrate, abandon.
	e2 := newEngineOnFile(t, s, &runner.FakeRunner{Scripts: scripts()}, dataDir)
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if err := e2.Abandon(id); err != nil {
		t.Fatal(err)
	}
	if err := e2.RetryStage(context.Background(), id); err == nil || !strings.Contains(err.Error(), "unknown issue") {
		t.Fatalf("expected unknown issue after abandon, got %v", err)
	}
	evs, _ := s.EventsSince(0)
	found := false
	for _, ev := range evs {
		if ev.IssueID == id && ev.Type == core.EvIssueAbandoned {
			found = true
		}
	}
	if !found {
		t.Fatal("issue_abandoned event not emitted")
	}
	// The steward persists "abandoned" (wired separately); once it is stored,
	// a later rehydrate must not resurrect the lane.
	rows, _ := s.Issues()
	for _, r := range rows {
		if r.ID == id {
			r.State = "abandoned"
			_ = s.UpsertIssue(r)
		}
	}
	e3 := newEngineOnFile(t, s, &runner.FakeRunner{Scripts: scripts()}, dataDir)
	if err := e3.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if err := e3.RetryStage(context.Background(), id); err == nil || !strings.Contains(err.Error(), "unknown issue") {
		t.Fatalf("rehydrate resurrected abandoned issue: %v", err)
	}
}

func TestAbandonRunningIssueCancelsStage(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, err := e.CreateIssue("live", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- e.StartIssue(context.Background(), id) }()
	deadline := time.After(5 * time.Second)
	for len(e.PendingDecisions()) != 1 {
		select {
		case <-deadline:
			t.Fatal("gate decision never appeared")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := e.Abandon(id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("stage goroutine never unblocked after abandon")
	}
	if ds := e.PendingDecisions(); len(ds) != 0 {
		t.Fatalf("pending decisions survived abandon: %d", len(ds))
	}
	rows, _ := s.AllDecisionRows()
	for _, row := range rows {
		if row.IssueID == id && row.Status == "pending" {
			t.Fatal("decision row left pending after abandon")
		}
	}
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.IssueID == id && event.Type == core.EvStageFailed {
			t.Fatalf("abandoned issue emitted stage failure: %s", event.Payload)
		}
	}
}

func TestAbandonUnknownIssue(t *testing.T) {
	e, _ := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	if err := e.Abandon("GH-404"); err == nil {
		t.Fatal("expected error for unknown issue")
	}
}

// After a merge lands, the issue branch has served its purpose; leaving it
// behind blocks later re-use of the branch name and clutters the repo.
func TestMergedIssueBranchIsDeleted(t *testing.T) {
	repo := t.TempDir()
	gitc := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return string(out)
	}
	gitc("init", "-q", "-b", "main")
	gitc("config", "user.email", "t@t")
	gitc("config", "user.name", "t")
	gitc("commit", "-q", "--allow-empty", "-m", "base")

	f := flow.Flow{Name: "default", Stages: []flow.Stage{
		{Name: "execute", Agents: []flow.AgentRef{{Package: "executor"}},
			Gate: flow.GateAuto, Workspace: "worktree"},
	}}
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := New(Config{
		Store: s,
		Runner: &runner.FakeRunner{Scripts: map[string]runner.Script{
			"execute/executor": {},
		}},
		Pool: slots.NewPool(1), Flows: map[string]flow.Flow{"default": f},
		DataDir: t.TempDir(), DecisionIdentities: testDecisionIdentities(),
		Workspace: workspace.GitWorktree{Repo: repo},
		Train:     &marshal.Train{Repo: repo},
	})
	id, err := e.CreateIssue("branch cleanup", "", "default", levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	branch := "issue/" + id
	if out := gitc("branch", "--list", branch); strings.TrimSpace(out) != "" {
		t.Fatalf("issue branch survived the merge: %q", out)
	}
}

type failOnceReleaseWorkspace struct {
	delegate workspace.GitWorktree
	failed   bool
}

func (w *failOnceReleaseWorkspace) Acquire(issueID string) (string, func() error, error) {
	path, _, err := w.delegate.Acquire(issueID)
	if err != nil {
		return "", nil, err
	}
	return path, func() error {
		if !w.failed {
			w.failed = true
			return errors.New("injected worktree release failure")
		}
		return w.delegate.ReleasePath(path)
	}, nil
}

func (w *failOnceReleaseWorkspace) ReleasePath(path string) error {
	return w.delegate.ReleasePath(path)
}

func (w *failOnceReleaseWorkspace) Name() string { return "fail-once worktree" }

func TestMergedCleanupFailureIsDurableAndRetryDoesNotReland(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	f := verificationFlow()
	s, err := store.Open("file:cleanup-retry?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ws := &failOnceReleaseWorkspace{delegate: workspace.GitWorktree{Repo: repo}}
	e := New(Config{
		Store: s, Runner: &runner.FakeRunner{Scripts: map[string]runner.Script{
			"merge-verification/merge-verifier": {Artifacts: map[string]string{
				"merge-report.md": "", "merge-decision.json": "", "verification.json": "",
			}},
		}},
		Pool: slots.NewPool(1), Flows: map[string]flow.Flow{"default": f},
		DataDir: t.TempDir(), Workspace: ws, Train: &marshal.Train{Repo: repo}, DecisionIdentities: testDecisionIdentities(),
	})
	id, err := e.CreateIssue("cleanup", "", "default", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	integration, ok, err := s.IssueIntegration(id)
	if err != nil || !ok || integration.State != store.IntegrationCleanupNeeded ||
		len(integration.Cleanup) != 2 ||
		!strings.HasPrefix(integration.Cleanup[0], cleanupReleasePrefix) ||
		integration.Cleanup[1] != cleanupDeletePrefix+"issue/"+id {
		t.Fatalf("cleanup integration = %+v ok %v err %v", integration, ok, err)
	}
	runs, _ := s.StageRuns(id)
	if len(runs) != 1 {
		t.Fatalf("stage runs before cleanup retry = %d", len(runs))
	}
	if err := e.RetryStage(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	integration, ok, err = s.IssueIntegration(id)
	if err != nil || !ok || integration.State != store.IntegrationMerged ||
		len(integration.Cleanup) != 0 {
		t.Fatalf("cleanup after retry = %+v ok %v err %v", integration, ok, err)
	}
	runs, _ = s.StageRuns(id)
	if len(runs) != 1 {
		t.Fatalf("cleanup retry reran stage: %d runs", len(runs))
	}
	if branch := gitOutput(t, repo, "branch", "--list", "issue/"+id); strings.TrimSpace(branch) != "" {
		t.Fatalf("branch survived cleanup retry: %q", branch)
	}
	events, _ := s.EventsSince(0)
	var mergedAt, cleanupAt, cleanupDoneAt int
	for index, event := range events {
		if event.IssueID != id {
			continue
		}
		switch event.Type {
		case core.EvIssueMerged:
			mergedAt = index + 1
		case core.EvCleanupNeeded:
			cleanupAt = index + 1
		case core.EvCleanupCompleted:
			cleanupDoneAt = index + 1
		}
	}
	if mergedAt == 0 || cleanupAt <= mergedAt || cleanupDoneAt <= cleanupAt {
		t.Fatalf("event order merged=%d cleanup=%d completed=%d", mergedAt, cleanupAt, cleanupDoneAt)
	}
}

// Agents must build on the latest shared code: an issue started while the
// local default branch lags origin should fast-forward it first.
func TestIssueStartFastForwardsBaseFromOrigin(t *testing.T) {
	repo := t.TempDir()
	gitc := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return string(out)
	}
	gitc(repo, "init", "-q", "-b", "main")
	gitc(repo, "config", "user.email", "t@t")
	gitc(repo, "config", "user.name", "t")
	gitc(repo, "commit", "-q", "--allow-empty", "-m", "base")
	remote := t.TempDir()
	gitc(remote, "init", "-q", "--bare", "-b", "main")
	gitc(repo, "remote", "add", "origin", remote)
	gitc(repo, "push", "-q", "origin", "main")
	ahead := t.TempDir()
	gitc(ahead, "clone", "-q", remote, ".")
	gitc(ahead, "config", "user.email", "t@t")
	gitc(ahead, "config", "user.name", "t")
	gitc(ahead, "commit", "-q", "--allow-empty", "-m", "remote work")
	gitc(ahead, "push", "-q", "origin", "main")

	f := flow.Flow{Name: "default", Stages: []flow.Stage{
		{Name: "execute", Agents: []flow.AgentRef{{Package: "executor"}},
			Gate: flow.GateAuto, Workspace: "worktree"},
	}}
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := New(Config{
		Store: s,
		Runner: &runner.FakeRunner{Scripts: map[string]runner.Script{
			"execute/executor": {},
		}},
		Pool: slots.NewPool(1), Flows: map[string]flow.Flow{"default": f},
		DataDir: t.TempDir(), DecisionIdentities: testDecisionIdentities(),
		Workspace: workspace.GitWorktree{Repo: repo},
		Train:     &marshal.Train{Repo: repo, Pull: true},
	})
	id, err := e.CreateIssue("freshness", "", "default", levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if log := gitc(repo, "log", "--oneline", "main"); !strings.Contains(log, "remote work") {
		t.Fatalf("base not fast-forwarded before issue ran: %s", log)
	}
}

// tempAttachment writes a file and returns its absolute path.
func tempAttachment(t *testing.T, name string, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCreateIssueStoresAttachmentBytesAndRows(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	src := tempAttachment(t, "app.log", 9)
	id, err := e.CreateIssue("a", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0,
		[]string{src})
	if err != nil {
		t.Fatal(err)
	}
	stored := filepath.Join(e.cfg.DataDir, id, "attachments", "app.log")
	if b, err := os.ReadFile(stored); err != nil || len(b) != 9 {
		t.Fatalf("bytes not stored at %s: %v %d", stored, err, len(b))
	}
	rows, err := s.Attachments(id)
	if err != nil || len(rows) != 1 || rows[0].Name != "app.log" || rows[0].Size != 9 {
		t.Fatalf("rows = %+v err = %v", rows, err)
	}
	// The event carries the stored names so the projection and the log agree.
	evs, _ := s.EventsSince(0)
	found := false
	for _, ev := range evs {
		if ev.Type != core.EvIssueCreated {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		names, _ := p["attachments"].([]any)
		if len(names) == 1 && names[0] == "app.log" {
			found = true
		}
	}
	if !found {
		t.Fatal("issue_created carried no attachments")
	}
}

// A refusal must cost nothing: no ID consumed, no issue dir created.
func TestCreateIssueRefusesBadAttachment(t *testing.T) {
	e, _ := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	dir := t.TempDir()
	oversized := filepath.Join(dir, "huge.bin")
	if err := os.WriteFile(oversized, make([]byte, (10<<20)+1), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{filepath.Join(dir, "ghost.log"), dir, oversized} {
		if _, err := e.CreateIssue("bad", "", "default", levers.Matrix{}, 0, []string{entry}); err == nil {
			t.Fatalf("CreateIssue accepted %q", entry)
		}
	}
	if _, err := os.Stat(filepath.Join(e.cfg.DataDir, "GH-1")); !os.IsNotExist(err) {
		t.Fatalf("a refused create left an issue dir: %v", err)
	}
	// The next successful create must still be GH-1 — nothing was burned.
	id, err := e.CreateIssue("good", "", "default", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id != "GH-1" {
		t.Fatalf("refusals consumed IDs: next id = %s", id)
	}
}

func TestDraftIssueStoresAttachmentsWithoutAStageRun(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	src := tempAttachment(t, "app.log", 4)
	id, err := e.DraftIssue("d", "b", "default", "regular", levers.Matrix{}, 0, []string{src})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.DataDir, id, "attachments", "app.log")); err != nil {
		t.Fatalf("draft attach did not create the issue dir: %v", err)
	}
	if rows, _ := s.Attachments(id); len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
}

// noneStage and worktreeStage are minimal stages that exercise the two
// stageWorkdir branches without declaring artifacts.
func noneStage() flow.Stage {
	return flow.Stage{Name: "spec", Workspace: "none", Agents: []flow.AgentRef{{Package: "spec-writer"}}}
}

func worktreeStage() flow.Stage {
	return flow.Stage{Name: "spec", Workspace: "worktree", Agents: []flow.AgentRef{{Package: "spec-writer"}}}
}

func TestAttachmentsMaterializeForNoneWorkspace(t *testing.T) {
	e, _ := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, err := e.CreateIssue("n", "body", "default", levers.Matrix{}, 0,
		[]string{tempAttachment(t, "app.log", 3)})
	if err != nil {
		t.Fatal(err)
	}
	is := e.issues[id]
	if err := e.runStageOnce(context.Background(), is, noneStage(), 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.cfg.DataDir, id, "attachments")
	if b, err := os.ReadFile(filepath.Join(dir, "app.log")); err != nil || len(b) != 3 {
		t.Fatalf("attachment unreadable at the canonical path: %v %d", err, len(b))
	}
	// dst == src, so Materialize must not have written a marker: nothing was
	// copied, because nothing needed copying.
	if _, err := os.Stat(filepath.Join(dir, ".watchtower")); !os.IsNotExist(err) {
		t.Fatalf("self-copy happened: %v", err)
	}
	md, err := os.ReadFile(filepath.Join(e.cfg.DataDir, id, "ISSUE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "`attachments/app.log`") {
		t.Fatalf("ISSUE.md does not name the attachment:\n%s", md)
	}
}

// The regression that matters: a worktree stage must see the file too.
func TestAttachmentsMaterializeIntoWorktree(t *testing.T) {
	e, _ := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, err := e.CreateIssue("w", "body", "default", levers.Matrix{}, 0,
		[]string{tempAttachment(t, "app.log", 3)})
	if err != nil {
		t.Fatal(err)
	}
	is := e.issues[id]
	// stageWorkdir returns is.wsPath for a non-"none" stage; assigning it
	// directly exercises that branch without provisioning a git worktree.
	is.wsPath = t.TempDir()
	if err := e.runStageOnce(context.Background(), is, worktreeStage(), 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(is.wsPath, "attachments", "app.log")); err != nil || len(b) != 3 {
		t.Fatalf("worktree copy missing: %v %d", err, len(b))
	}
	if _, err := os.Stat(filepath.Join(e.cfg.DataDir, id, "attachments", "app.log")); err != nil {
		t.Fatalf("canonical copy disturbed: %v", err)
	}
	md, _ := os.ReadFile(filepath.Join(is.wsPath, "ISSUE.md"))
	if !strings.Contains(string(md), "`attachments/app.log`") {
		t.Fatalf("worktree ISSUE.md does not name the attachment:\n%s", md)
	}
}

// A repo with its own tracked attachments/ cannot use the feature, and it must
// learn that as a loud refusal, never a silent overwrite.
func TestMaterializeRefusesForeignAttachmentsDir(t *testing.T) {
	e, _ := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, err := e.CreateIssue("g", "body", "default", levers.Matrix{}, 0,
		[]string{tempAttachment(t, "app.log", 3)})
	if err != nil {
		t.Fatal(err)
	}
	is := e.issues[id]
	is.wsPath = t.TempDir()
	foreign := filepath.Join(is.wsPath, "attachments")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "tracked.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = e.runStageOnce(context.Background(), is, worktreeStage(), 1, 1, nil)
	if err == nil || !strings.Contains(err.Error(), "is not watchtower's") {
		t.Fatalf("stage did not refuse: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "stage spec: ") {
		t.Fatalf("refusal does not name the stage: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(foreign, "tracked.txt")); string(b) != "mine" {
		t.Fatal("refusal was destructive")
	}
}

// The attachment list stays next to the issue it belongs to: after the body,
// before the Librarian's memory block.
func TestIssueMDAttachmentSectionOrdering(t *testing.T) {
	memory := t.TempDir()
	if err := os.WriteFile(filepath.Join(memory, "conventions.md"), []byte("use tabs"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, _ := newEngineCfg(t, &runner.FakeRunner{Scripts: scripts()}, func(cfg *Config) {
		cfg.Librarian = &librarian.Librarian{MemoryDir: memory}
	})
	id, err := e.CreateIssue("o", "the body", "default", levers.Matrix{}, 0,
		[]string{tempAttachment(t, "app.log", 3)})
	if err != nil {
		t.Fatal(err)
	}
	is := e.issues[id]
	if err := e.runStageOnce(context.Background(), is, noneStage(), 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	md, err := os.ReadFile(filepath.Join(e.cfg.DataDir, id, "ISSUE.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(md)
	body := strings.Index(text, "the body")
	attachments := strings.Index(text, "# Attachments")
	memoryHeading := strings.Index(text, "# Project memory")
	if body < 0 || attachments < 0 || memoryHeading < 0 {
		t.Fatalf("missing section:\n%s", text)
	}
	if !(body < attachments && attachments < memoryHeading) {
		t.Fatalf("wrong order body=%d attachments=%d memory=%d:\n%s",
			body, attachments, memoryHeading, text)
	}
}

func TestIssueMDOmitsEmptyAttachmentSection(t *testing.T) {
	e, _ := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, err := e.CreateIssue("e", "body", "default", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	is := e.issues[id]
	if err := e.runStageOnce(context.Background(), is, noneStage(), 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(filepath.Join(e.cfg.DataDir, id, "ISSUE.md"))
	if strings.Contains(string(md), "# Attachments") {
		t.Fatalf("empty set produced a section:\n%s", md)
	}
}

// A never-started draft's bytes are reachable only through abandon, so cleanup
// is wired there explicitly. Abandon stays a state, not a purge: the issues
// row and stage artifacts survive so the lane stays inspectable.
func TestAbandonDeletesAttachmentBytes(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, err := e.CreateIssue("a", "body", "default", levers.Matrix{}, 0,
		[]string{tempAttachment(t, "app.log", 3)})
	if err != nil {
		t.Fatal(err)
	}
	is := e.issues[id]
	if err := e.runStageOnce(context.Background(), is, noneStage(), 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.Abandon(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.DataDir, id, "attachments")); !os.IsNotExist(err) {
		t.Fatalf("attachment bytes survived abandon: %v", err)
	}
	if rows, _ := s.Attachments(id); len(rows) != 0 {
		t.Fatalf("attachment rows survived abandon: %+v", rows)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.DataDir, id, "ISSUE.md")); err != nil {
		t.Fatalf("abandon deleted stage artifacts: %v", err)
	}
	issues, _ := s.Issues()
	found := false
	for _, row := range issues {
		if row.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("abandon deleted the issues row")
	}
}

// Attachments are never in-memory state, so Rehydrate needs no change at all.
func TestRehydrateIgnoresAttachments(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, err := e.DraftIssue("d", "b", "default", "regular", levers.Matrix{}, 0,
		[]string{tempAttachment(t, "app.log", 3)})
	if err != nil {
		t.Fatal(err)
	}
	e2 := New(Config{Store: s, Runner: e.cfg.Runner, Pool: slots.NewPool(2),
		Flows: map[string]flow.Flow{"default": testFlow()}, DataDir: e.cfg.DataDir,
		DecisionIdentities: testDecisionIdentities()})
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	is, ok := e2.issues[id]
	if !ok || !is.draft {
		t.Fatalf("draft did not rehydrate: %+v", is)
	}
	if rows, _ := s.Attachments(id); len(rows) != 1 || rows[0].Name != "app.log" {
		t.Fatalf("rehydrate disturbed attachments: %+v", rows)
	}
	// The bytes are still where the next stage will look for them.
	if _, err := os.Stat(filepath.Join(e.cfg.DataDir, id, "attachments", "app.log")); err != nil {
		t.Fatalf("bytes lost across restart: %v", err)
	}
}
