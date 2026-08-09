package engine

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/review"
	"github.com/weston6142/watchtower/internal/runner"
)

func TestGH54PlannerRecovery(t *testing.T) {
	planFlow := flow.Flow{
		Name: "gh54-recovery-fixture",
		Stages: []flow.Stage{
			{Name: "plan", Agents: []flow.AgentRef{{Package: "planner"}}, Workspace: "none", Completion: flow.CompletionAll,
				Gate: flow.GatePlanReview, Retries: 0, Artifacts: []string{"plan.md", "touchset.json"}},
			{Name: "execute", Agents: []flow.AgentRef{{Package: "executor"}}, Workspace: "none", Completion: flow.CompletionAll},
		},
	}
	var executionStarted atomic.Bool
	fake := &runner.FakeRunner{
		Scripts: map[string]runner.Script{
			"plan/planner":     {PlannerRequests: plannerArtifactRequests()},
			"execute/executor": {},
		},
		OnStart: func(_, stage, _, _ string) error {
			if stage == "execute" {
				executionStarted.Store(true)
			}
			return nil
		},
	}
	e, _ := newEngineCfg(t, fake, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{planFlow.Name: planFlow}
		cfg.PlanReview = review.PolicySettings{ID: "gh54-recovery", Version: "1", Valid: true, AutoApproveRegular: false}
	})
	id, err := e.CreateIssue("GH-54 recovery fixture", "", planFlow.Name, levers.Matrix{"plan": flow.LeverRegular, "execute": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- e.StartIssue(ctx, id) }()
	pending := waitForPendingStage(t, e, "plan")
	if pending.Review == nil || pending.Review.Stage != "plan" {
		t.Fatalf("pending recovery review = %+v", pending.Review)
	}
	if executionStarted.Load() {
		t.Fatal("GH-54 recovery started implementation before plan review")
	}
	if err := e.Answer(pending.ID, levers.ChoiceResponse(1)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("recovery unexpectedly advanced after plan rejection")
	}
}
