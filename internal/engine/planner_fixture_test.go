package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/plannerartifact"
	"github.com/weston6142/watchtower/internal/plannerbudget"
	"github.com/weston6142/watchtower/internal/review"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/stageusage"
	"github.com/weston6142/watchtower/internal/store"
)

type smallPlannerFixture struct {
	t       *testing.T
	engine  *Engine
	store   *store.Store
	runner  *recordingPlannerRunner
	issueID string
}

type plannerFixtureResult struct {
	AdmittedCalls             int
	Snapshot                  stageusage.Snapshot
	Sources                   []string
	Outcome                   plannerbudget.Outcome
	PlanReviewRequested       bool
	Artifacts                 []string
	UnchangedSourceReused     bool
	ChangedFingerprintCharged bool
}

type recordingPlannerRunner struct {
	Tools                 []runner.ToolCall
	Admitted              []string
	UnchangedSourceReused bool
}

func (r *recordingPlannerRunner) Run(context.Context, string, string, string, string, chan<- runner.Ask) <-chan runner.Result {
	done := make(chan runner.Result, 1)
	done <- runner.Result{Err: fmt.Errorf("non-planner run requested")}
	return done
}

func (r *recordingPlannerRunner) RunPlanner(ctx context.Context, _ string, _ string, _ string, workdir string,
	_ chan<- runner.Ask, gate runner.ExplorationGate) <-chan runner.Result {
	done := make(chan runner.Result, 1)
	go func() {
		for _, tool := range r.Tools {
			decision, err := gate.Admit(ctx, tool)
			if err != nil {
				done <- runner.Result{Err: err}
				return
			}
			if !decision.Allowed {
				continue
			}
			r.Admitted = append(r.Admitted, tool.SourceID)
			actual := tool.Reservation
			if err := gate.Complete(ctx, decision, &actual, nil); err != nil {
				done <- runner.Result{Err: err}
				return
			}
		}
		admittedIssueReads := 0
		for _, sourceID := range r.Admitted {
			if sourceID == "ISSUE.md" {
				admittedIssueReads++
			}
		}
		r.UnchangedSourceReused = admittedIssueReads == 1
		session, err := plannerartifact.OpenFromEnvironment(workdir, runner.PlannerArtifactEnv(ctx))
		if err != nil {
			done <- runner.Result{Err: err}
			return
		}
		for _, request := range plannerArtifactRequests() {
			if err := session.Apply(request); err != nil {
				done <- runner.Result{Err: err}
				return
			}
		}
		done <- runner.Result{}
	}()
	return done
}

func NewSmallPlannerFixture(t *testing.T) *smallPlannerFixture {
	t.Helper()
	planFlow := plannerTestFlow()
	recording := &recordingPlannerRunner{
		Tools: []runner.ToolCall{
			{Name: "read", SourceID: "ISSUE.md", Fingerprint: "v1", Reservation: 50000, Priority: int(plannerbudget.IssueArtifact)},
			{Name: "read", SourceID: "ISSUE.md", Fingerprint: "v1", Reservation: 50000, Priority: int(plannerbudget.IssueArtifact)},
			{Name: "read", SourceID: "STAGE.md", Fingerprint: "v1", Reservation: 50000, Priority: int(plannerbudget.IssueArtifact)},
			{Name: "read", SourceID: "spec.md", Fingerprint: "v1", Reservation: 50000, Priority: int(plannerbudget.IssueArtifact)},
			{Name: "read", SourceID: "touchset:internal/**", Fingerprint: "v1", Reservation: 50000, Priority: int(plannerbudget.TouchsetCandidate)},
			{Name: "read", SourceID: "internal/engine/engine.go", Fingerprint: "v1", Reservation: 50000, Priority: int(plannerbudget.DirectCode)},
			{Name: "read", SourceID: "README.md", Fingerprint: "v1", Reservation: 1, Priority: int(plannerbudget.BroadSource)},
		},
	}
	e, s := newEngineCfg(t, recording, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{planFlow.Name: planFlow}
		cfg.PlannerBudget = plannerbudget.DefaultProfile()
		cfg.PlanReview = reviewPolicyForFixture()
	})
	id, err := e.CreateIssue("bounded planner", "", planFlow.Name, levers.Preset(planFlow, flow.LeverRegular), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &smallPlannerFixture{t: t, engine: e, store: s, runner: recording, issueID: id}
}

func reviewPolicyForFixture() review.PolicySettings {
	return review.PolicySettings{ID: "planner-fixture", Version: "1", AutoApproveRegular: true, Valid: true}
}

func (f *smallPlannerFixture) Run() plannerFixtureResult {
	f.t.Helper()
	if err := f.engine.StartIssue(context.Background(), f.issueID); err != nil {
		f.t.Fatal(err)
	}
	snapshot, outcome, err := f.store.LatestPlannerSnapshot(f.issueID)
	if err != nil || snapshot == nil {
		f.t.Fatalf("latest planner snapshot = %+v, %v, %v", snapshot, outcome, err)
	}
	events, err := f.store.EventsSince(0)
	if err != nil {
		f.t.Fatal(err)
	}
	planReviewRequested := false
	for _, event := range events {
		if event.Type == core.EvPlanReviewRequested && event.IssueID == f.issueID {
			planReviewRequested = true
		}
	}
	artifacts, err := f.store.ArtifactPaths(f.issueID)
	if err != nil {
		f.t.Fatal(err)
	}
	return plannerFixtureResult{
		AdmittedCalls: f.snapshotAdmittedCalls(), Snapshot: *snapshot,
		Sources: append([]string(nil), f.runner.Admitted...), Outcome: plannerbudget.Outcome(outcome),
		PlanReviewRequested: planReviewRequested, Artifacts: artifacts,
		UnchangedSourceReused:     f.runner.UnchangedSourceReused,
		ChangedFingerprintCharged: f.verifyChangedFingerprint(),
	}
}

func (f *smallPlannerFixture) snapshotAdmittedCalls() int {
	return len(f.runner.Admitted)
}

func (f *smallPlannerFixture) verifyChangedFingerprint() bool {
	now := time.Unix(0, 0)
	controller, err := plannerbudget.NewController("plan", 1, plannerbudget.Profile{
		Calls:   stageusage.DimensionLimit{Warning: 4, Hard: 8},
		Tokens:  stageusage.DimensionLimit{Warning: 4, Hard: 8},
		Elapsed: stageusage.ElapsedLimit{Warning: time.Minute, Hard: 2 * time.Minute},
	}, func() time.Time { return now })
	if err != nil {
		f.t.Fatal(err)
	}
	source := plannerbudget.Source{ID: "ISSUE.md", Fingerprint: "v1", Content: "issue", Reservation: 1}
	first := controller.Read(source)
	unchanged := controller.Read(source)
	changed := controller.Read(plannerbudget.Source{ID: source.ID, Fingerprint: "v2", Content: source.Content, Reservation: 1})
	return first.Charged && !unchanged.Charged && changed.Charged
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func TestRepresentativeNarrowPlannerRunIsBoundedAndAuditable(t *testing.T) {
	fixture := NewSmallPlannerFixture(t)
	result := fixture.Run()
	if result.AdmittedCalls > 32 || result.Snapshot.ChargedTokens > 250000 || result.Snapshot.ElapsedMillis > 600000 {
		t.Fatalf("unbounded result = %+v", result)
	}
	wantSources := []string{"ISSUE.md", "STAGE.md", "spec.md", "touchset:internal/**", "internal/engine/engine.go"}
	if !sameStrings(result.Sources, wantSources) {
		t.Fatalf("source order = %v", result.Sources)
	}
	if result.Outcome != plannerbudget.OutcomeBudgetLimited || !result.PlanReviewRequested {
		t.Fatalf("result = %+v", result)
	}
	if result.Snapshot.Status != stageusage.StatusBudgetLimited ||
		result.Snapshot.StopDimension != stageusage.DimensionTokens ||
		result.Snapshot.TokensEstimated || len(result.Artifacts) != 2 {
		t.Fatalf("audit snapshot/artifacts = %+v", result)
	}
	if !result.UnchangedSourceReused || !result.ChangedFingerprintCharged {
		t.Fatalf("source reuse accounting = %+v", result)
	}
}
