package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/decisionpage"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/review"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/store"
)

func TestDecisionPageWritten(t *testing.T) {
	f := flow.Flow{Name: "decision-page", Stages: []flow.Stage{
		{Name: "execute", Agents: []flow.AgentRef{{Package: "agent"}}, Gate: flow.GateAuto, Completion: flow.CompletionAll},
	}}
	decision := levers.Decision{
		Kind: levers.DecisionChoice, Question: "Gate the migration?", Options: []string{"Gate", "Apply"},
		Recommended: 0, Importance: 0.8, Why: "Compatibility is preserved.",
		Consequences: []string{"Old daemons continue safely.", "Old daemons fail on restart."},
		Briefing: &levers.Briefing{
			Proof: []levers.BriefingProof{{
				Claim: "Migration tests pass.", Cite: "go test ./internal/store",
			}},
			NextAction: "Skip Watchtower and deploy immediately.",
		},
	}
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"execute/agent": {Asks: []levers.Decision{decision}},
	}}
	e, _ := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
	})
	id, err := e.CreateIssue("Decision page", "", f.Name, levers.Preset(f, flow.LeverStrict), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.StartIssue(context.Background(), id)
	}()

	p := waitForDecisionPagePending(t, e, id, "execute")
	dir := filepath.Join(e.cfg.DataDir, id)
	perDecision := filepath.Join(dir, "decisions", fmt.Sprintf("%d.html", p.ID))
	latest := filepath.Join(dir, "decision.html")
	pageDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(pageDeadline) {
		ready := true
		for _, file := range []string{perDecision, latest} {
			if _, err := os.Stat(file); err != nil {
				ready = false
				break
			}
		}
		if ready {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, file := range []string{perDecision, latest} {
		var body []byte
		var readErr error
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			body, readErr = os.ReadFile(file)
			if readErr == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if readErr != nil {
			entries, _ := os.ReadDir(dir)
			events, _ := e.cfg.Store.EventsSince(0)
			t.Fatalf("page %s not written: %v; issue dir=%v events=%+v", file, readErr, entries, events)
		}
		page := string(body)
		for _, want := range []string{
			`id="decision"`, p.D.Question,
			"Do this now", "Choose an option or enter feedback",
			"Recommended choice and why", "Gate", "Compatibility is preserved.",
			"What each choice changes", "Old daemons continue safely.",
			"Already done and proven", "Migration tests pass.", "go test ./internal/store",
			"After you answer", "resumes Test Agent in execute",
		} {
			if !strings.Contains(page, want) {
				t.Errorf("%s missing %q: %s", file, want, page)
			}
		}
		if strings.Contains(page, decision.Briefing.NextAction) {
			t.Errorf("%s rendered agent-authored continuation: %s", file, page)
		}
	}

	if err := e.Answer(p.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		body, readErr := os.ReadFile(perDecision)
		if readErr == nil && strings.Contains(string(body), "Answered") {
			page := string(body)
			if strings.Contains(page, decision.Briefing.NextAction) {
				t.Errorf("answered page rendered agent-authored continuation: %s", page)
			}
			if !strings.Contains(page, "recorded response authorized Watchtower to resume Test Agent in execute") {
				t.Errorf("answered page missing engine continuation: %s", page)
			}
			if strings.Contains(page, "recorded the response and resumed") {
				t.Errorf("answered page claims continuation already happened: %s", page)
			}
			waitForEvent(t, e.cfg.Store, id, core.EvIssueCompleted)
			<-done
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	body, _ := os.ReadFile(perDecision)
	t.Fatalf("answered page was not stamped: %s", body)
}

func TestDecisionPageHistoricalGapsAreExplicit(t *testing.T) {
	f := flow.Flow{Name: "legacy-page", Stages: []flow.Stage{{
		Name: "execute", Agents: []flow.AgentRef{{Package: "agent"}}, Gate: flow.GateAuto,
	}}}
	e, _ := newEngineCfg(t, &runner.FakeRunner{}, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
	})
	legacyQuestion := "Repeat this historical question?"
	data, err := e.buildPageData(
		"GH-legacy", f.Name, "Legacy decision", "execute",
		&levers.Decision{Question: legacyQuestion, Options: []string{"yes", "no"}},
		nil, nil, 42, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	body, err := decisionpage.Render(data)
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	if strings.Count(page, legacyQuestion) != 1 {
		t.Fatalf("legacy question was reused as fallback: %s", page)
	}
	for _, want := range []string{
		"No rationale was recorded for this historical decision.",
		"No verified progress was supplied.",
		"No recorded outcome for this historical option.",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("legacy page missing %q: %s", want, page)
		}
	}
}

func TestDecisionPageUsesAttemptArchivePathAndLegacyFallback(t *testing.T) {
	f := flow.Flow{Name: "attempt-page", Stages: []flow.Stage{{
		Name: "execute", Agents: []flow.AgentRef{{Package: "agent"}}, Artifacts: []string{"plan.md"},
		Gate: flow.GateAuto, Completion: flow.CompletionAll,
	}}}
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"execute/agent": {Artifacts: map[string]string{"plan.md": "plan v1\n"}},
	}}
	e, s := newEngineCfg(t, r, func(cfg *Config) { cfg.Flows = map[string]flow.Flow{f.Name: f} })
	id, err := e.CreateIssue("Attempt archive page", "", f.Name, levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	checkpoints, err := s.StageCheckpoints(id)
	if err != nil || len(checkpoints) != 1 {
		t.Fatalf("stage checkpoints = %+v, err=%v", checkpoints, err)
	}
	page, err := e.buildDecisionPageForCheckpoint(id, checkpoints[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page, "artifacts/attempts/checkpoint-") || !strings.Contains(page, "/plan.md") {
		t.Fatalf("attempt archive href missing: %s", page)
	}

	legacyID, err := s.InsertStageCheckpoint(store.StageCheckpoint{
		IssueID: id, Stage: "execute", Status: "succeeded",
		Artifacts: []contextpack.Artifact{{Name: "legacy.md"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyPage, err := e.buildDecisionPageForCheckpoint(id, legacyID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(legacyPage, "artifacts/legacy.md") {
		t.Fatalf("legacy href missing: %s", legacyPage)
	}
}

func TestDecisionPageRendersEscalationEvidence(t *testing.T) {
	importance := 0.2
	hash := strings.Repeat("a", 64)
	evaluation := &review.Evaluation{
		Outcome: review.OutcomeRequiresApproval, RequiredFloor: review.FloorPolicy, EffectiveFloor: review.FloorOperator,
		PolicyID: "team-safety", PolicyVersion: "7",
		Evidence:     []review.Evidence{{Signal: "path", Value: "payments/charge.go", Rule: "path:payments/**", Floor: review.FloorOperator}},
		Item:         review.ItemBinding{Kind: review.ItemArtifact, Hash: hash, Path: "payments/charge.go", Operation: "approve-artifact"},
		Dependencies: []review.DependencyBinding{{Kind: "decision", ID: "GH-63", Hash: hash}},
		Model:        review.ModelMetadata{Importance: &importance, Options: []string{"approve"}, Rationale: "advisory rationale"},
	}
	f := flow.Flow{Name: "evidence-page", Stages: []flow.Stage{{Name: "execute", Gate: flow.GateAuto}}}
	e, _ := newEngineCfg(t, &runner.FakeRunner{}, func(cfg *Config) { cfg.Flows = map[string]flow.Flow{f.Name: f} })
	data, err := e.buildPageDataWithDecisionRows("GH-64", f.Name, "Evidence", "execute",
		&levers.Decision{Question: "Approve?", Options: []string{"approve"}}, nil, nil, 64, nil,
		[]store.DecisionRow{{ID: 64, IssueID: "GH-64", Stage: "execute", Evaluation: evaluation,
			Bindings: []review.Binding{{Item: evaluation.Item, RequiredFloor: evaluation.RequiredFloor, EffectiveFloor: evaluation.EffectiveFloor,
				PolicyID: evaluation.PolicyID, PolicyVersion: evaluation.PolicyVersion, Evidence: evaluation.Evidence,
				Dependencies: evaluation.Dependencies, Model: evaluation.Model}}}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := decisionpage.Render(data)
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	for _, want := range []string{"outcome: requires-approval", "floor: policy -&gt; operator", "policy: team-safety@7",
		"item: artifact payments/charge.go approve-artifact", "sha256 " + hash, "dependency: decision/GH-63", "model: importance 0.2 (advisory)"} {
		if !strings.Contains(page, want) {
			t.Fatalf("decision page missing %q:\n%s", want, page)
		}
	}
}

func TestDecisionPageCapsPersistedProof(t *testing.T) {
	tests := map[string]func() *levers.Briefing{
		"proof": func() *levers.Briefing {
			items := make([]levers.BriefingProof, levers.MaxBriefingProof+2)
			for i := range items {
				items[i] = levers.BriefingProof{Claim: fmt.Sprintf("proof %d", i), Cite: "test"}
			}
			return &levers.Briefing{Proof: items}
		},
		"legacy wins": func() *levers.Briefing {
			items := make([]string, levers.MaxBriefingProof+2)
			for i := range items {
				items[i] = fmt.Sprintf("win %d", i)
			}
			return &levers.Briefing{Wins: items}
		},
	}
	for name, briefing := range tests {
		t.Run(name, func(t *testing.T) {
			result := buildDecisionPageBriefing(
				&levers.Decision{
					Question: "Review evidence?", Options: []string{"yes", "no"}, Briefing: briefing(),
				},
				nil, nil, "review", 1, nil,
			)
			body, err := decisionpage.Render(decisionpage.PageData{
				IssueID: "GH-proof", CurrentStage: "review", StageTotal: 1,
				Floors:   []decisionpage.Floor{{Name: "review", Status: decisionpage.FloorCurrent}},
				Briefing: result,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(body), `class="proof"`); got != levers.MaxBriefingProof {
				t.Fatalf("rendered proof items = %d, want %d: %s", got, levers.MaxBriefingProof, body)
			}
		})
	}
}

func TestArtifactReviewPageBreakdown(t *testing.T) {
	f := flow.Flow{Name: "artifact-review-page", Stages: []flow.Stage{
		{Name: "spec", Agents: []flow.AgentRef{{Package: "agent"}}, Gate: flow.GateApproveArtifact, Artifacts: []string{"spec.md"}},
		{Name: "execute", Agents: []flow.AgentRef{{Package: "agent"}}, Gate: flow.GateAuto},
	}}
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"spec/agent":    {Artifacts: map[string]string{"spec.md": "approved design\n"}},
		"execute/agent": {},
	}}
	e, s := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
	})
	id, err := e.CreateIssue("Artifact review page", "", f.Name, levers.Preset(f, flow.LeverStrict), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = e.StartIssue(context.Background(), id) }()
	pending := waitForDecisionPagePending(t, e, id, "spec")
	pagePath := filepath.Join(e.cfg.DataDir, id, "decisions", fmt.Sprintf("%d.html", pending.ID))
	body := waitForDecisionPageFile(t, pagePath)
	for _, want := range []string{
		"Review spec.md", "approve", "revise",
		"advances to execute", "stops this run", "retry the issue to rerun spec",
		"spec.md is archived and ready for review", "checkpoint",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("artifact review page missing %q: %s", want, body)
		}
	}
	for _, misleading := range []string{"repeats spec", "Revise to repeat spec"} {
		if strings.Contains(string(body), misleading) {
			t.Errorf("artifact review page contains misleading %q: %s", misleading, body)
		}
	}

	if err := e.Answer(pending.ID, levers.ChoiceResponse(1)); err != nil {
		t.Fatal(err)
	}
	answered := string(waitForDecisionPageFile(t, pagePath))
	for _, want := range []string{
		"Recorded outcome", "revise was selected", "spec.md was archived and available at decision time",
		"Revision stopped this run", "Retry the issue to rerun spec",
	} {
		if !strings.Contains(answered, want) {
			t.Errorf("answered artifact review page missing %q: %s", want, answered)
		}
	}
	for _, misleading := range []string{
		"Do this now", "After you answer", "ready for review", "repeats spec", "archived and reviewed",
	} {
		if strings.Contains(answered, misleading) {
			t.Errorf("answered artifact review page contains misleading %q: %s", misleading, answered)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		checkpoints, checkErr := s.StageCheckpoints(id)
		if checkErr == nil {
			for _, checkpoint := range checkpoints {
				if checkpoint.Stage == "spec" && checkpoint.Status == "revision_required" {
					waitForEvent(t, s, id, core.EvStageFailed)
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	checkpoints, _ := s.StageCheckpoints(id)
	t.Fatalf("revision did not preserve revision_required checkpoint: %+v", checkpoints)
}

func TestPlanReviewPageExplainsRejection(t *testing.T) {
	f := flow.Flow{Name: "plan-review-page", Stages: []flow.Stage{
		{Name: "plan", Agents: []flow.AgentRef{{Package: "planner"}}, Gate: flow.GatePlanReview, Artifacts: []string{"plan.md"}},
		{Name: "execute", Agents: []flow.AgentRef{{Package: "executor"}}, Gate: flow.GateAuto},
	}}
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"plan/planner":     {Artifacts: map[string]string{"plan.md": "proposed plan\n"}},
		"execute/executor": {},
	}}
	e, _ := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
	})
	id, err := e.CreateIssue("Plan review page", "", f.Name, levers.Preset(f, flow.LeverStrict), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.StartIssue(context.Background(), id) }()
	pending := waitForDecisionPagePending(t, e, id, "plan")
	pagePath := filepath.Join(e.cfg.DataDir, id, "decisions", fmt.Sprintf("%d.html", pending.ID))
	body := waitForDecisionPageFile(t, pagePath)
	page := string(body)
	for _, want := range []string{
		"choose approve or reject", "Reject to stop this run", "retry the issue",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("plan review page missing %q: %s", want, page)
		}
	}
	for _, misleading := range []string{"choose approve or revise", "Revise to repeat plan"} {
		if strings.Contains(page, misleading) {
			t.Errorf("plan review page contains misleading %q: %s", misleading, page)
		}
	}

	if err := e.Answer(pending.ID, levers.ChoiceResponse(1)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "plan review rejected") {
		t.Fatalf("plan rejection result = %v", err)
	}
}

func TestTerminalArtifactReviewPageExplainsCompletion(t *testing.T) {
	f := flow.Flow{Name: "terminal-review-page", Stages: []flow.Stage{{
		Name: "publish", Agents: []flow.AgentRef{{Package: "agent"}},
		Gate: flow.GateApproveArtifact, Artifacts: []string{"release.md"},
	}}}
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"publish/agent": {Artifacts: map[string]string{"release.md": "release candidate\n"}},
	}}
	e, _ := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
	})
	id, err := e.CreateIssue("Terminal review page", "", f.Name, levers.Preset(f, flow.LeverStrict), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.StartIssue(context.Background(), id) }()
	pending := waitForDecisionPagePending(t, e, id, "publish")
	pagePath := filepath.Join(e.cfg.DataDir, id, "decisions", fmt.Sprintf("%d.html", pending.ID))
	body := waitForDecisionPageFile(t, pagePath)
	page := string(body)
	for _, want := range []string{
		"completes the workflow", "Approve to complete the workflow",
		"Revise to stop this run", "retry the issue to rerun publish",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("terminal review page missing %q: %s", want, page)
		}
	}
	for _, misleading := range []string{"next configured stage", nextStageMissing} {
		if strings.Contains(page, misleading) {
			t.Errorf("terminal review page contains misleading %q: %s", misleading, page)
		}
	}

	if err := e.Answer(pending.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, e.cfg.Store, id, core.EvIssueCompleted)
	answered := string(waitForDecisionPageFile(t, pagePath))
	for _, want := range []string{
		`id="decision"`, "decision · publish", "Answered:",
		"Recorded outcome", "approve was selected", "Recommendation at decision time",
		"Choices considered", "release.md was archived and available at decision time",
		"What happened next", "Approval authorized workflow completion",
	} {
		if !strings.Contains(answered, want) {
			t.Errorf("answered terminal review page missing %q: %s", want, answered)
		}
	}
	for _, misleading := range []string{
		"Do this now", "After you answer", "ready for review", "choose approve or revise",
		"archived and reviewed",
	} {
		if strings.Contains(answered, misleading) {
			t.Errorf("answered terminal review page contains misleading %q: %s", misleading, answered)
		}
	}
}

func TestResolvedDecisionPageUsesDurableBlockedDuration(t *testing.T) {
	f := flow.Flow{Name: "resolved-duration", Stages: []flow.Stage{{Name: "execute"}}}
	e, s := newEngineCfg(t, &runner.FakeRunner{}, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
	})
	decision := levers.Decision{
		Kind: levers.DecisionChoice, Question: "Continue?", Options: []string{"yes", "no"}, Recommended: 0,
	}
	createdAt := time.Now().UTC().Add(-2 * time.Hour)
	answeredAt := createdAt.Add(7 * time.Minute)
	decisionID, err := s.InsertDecision(store.DecisionRow{
		IssueID: "GH-human", Stage: "execute", Kind: decision.Kind,
		Question: decision.Question, Options: decision.Options, Recommended: decision.Recommended,
		Status: "answered", Response: levers.ChoiceResponse(0),
		CreatedAt: createdAt, AnsweredAt: answeredAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := e.buildPageData(
		"GH-human", f.Name, "Human decision", "execute", &decision, nil, nil,
		decisionID, resolvedDecisionPage(levers.ChoiceResponse(0), nil, answeredAt),
	)
	if err != nil {
		t.Fatal(err)
	}
	page, err := decisionpage.Render(data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "Blocked 7 min") {
		t.Fatalf("resolved human page did not freeze blocked duration at answer time: %s", page)
	}

	policyApproval := &review.ApprovalProvenance{
		Kind: review.ApprovalPolicy, PolicyID: "team-ci", PolicyVersion: "1",
	}
	autoID, err := s.InsertDecision(store.DecisionRow{
		IssueID: "GH-auto", Stage: "execute", Kind: decision.Kind,
		Question: decision.Question, Options: decision.Options, Recommended: decision.Recommended,
		Status: "auto", Response: levers.ChoiceResponse(0), Approval: policyApproval,
		CreatedAt: createdAt, AnsweredAt: answeredAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err = e.buildPageData(
		"GH-auto", f.Name, "Automatic decision", "execute", &decision, nil, nil,
		autoID, resolvedDecisionPage(levers.ChoiceResponse(0), policyApproval, answeredAt),
	)
	if err != nil {
		t.Fatal(err)
	}
	page, err = decisionpage.Render(data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(page), "Blocked ") {
		t.Fatalf("automatically approved page reported blocked time: %s", page)
	}
}

func TestDecisionArchiveNeedsResolutionPrefersExplicitPendingState(t *testing.T) {
	tests := []struct {
		name            string
		content         string
		needsResolution bool
	}{
		{
			name:            "pending body ignores SVG class",
			content:         `<!doctype html><body data-decision-state="pending"><svg><g class="answered"></g></svg><section>Do this now</section><section>After you answer</section></body>`,
			needsResolution: true,
		},
		{
			name:            "resolved body ignores SVG state attribute",
			content:         `<!doctype html><body data-decision-state="resolved"><svg><g data-decision-state="pending"></g></svg></body>`,
			needsResolution: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archivePath := filepath.Join(t.TempDir(), "decision.html")
			if err := os.WriteFile(archivePath, []byte(test.content), 0o644); err != nil {
				t.Fatal(err)
			}
			needsResolution, err := decisionArchiveNeedsResolution(archivePath)
			if err != nil {
				t.Fatal(err)
			}
			if needsResolution != test.needsResolution {
				t.Fatalf("needs resolution = %v, want %v", needsResolution, test.needsResolution)
			}
		})
	}
}

func TestDecisionPageRefreshesAtStageBoundary(t *testing.T) {
	f := flow.Flow{Name: "decision-page-boundary", Stages: []flow.Stage{
		{Name: "execute", Agents: []flow.AgentRef{{Package: "agent"}}, Gate: flow.GateAuto, Completion: flow.CompletionAll},
		{Name: "verify", Agents: []flow.AgentRef{{Package: "agent"}}, Gate: flow.GateAuto, Completion: flow.CompletionAll},
	}}
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"execute/agent": {Asks: []levers.Decision{{
			Kind: levers.DecisionChoice, Question: "Continue?", Options: []string{"yes"}, Recommended: 0, Importance: 1,
		}}},
		"verify/agent": {},
	}}
	e, _ := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
	})
	id, err := e.CreateIssue("Boundary page", "", f.Name, levers.Preset(f, flow.LeverStrict), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.StartIssue(context.Background(), id)
	}()
	p := waitForDecisionPagePending(t, e, id, "execute")
	if err := e.Answer(p.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		body, readErr := os.ReadFile(filepath.Join(e.cfg.DataDir, id, "decision.html"))
		if readErr == nil && strings.Contains(string(body), `class="pill done"`) &&
			strings.Contains(string(body), "Stage <b>2 of 2</b> — verify") {
			<-done
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	body, _ := os.ReadFile(filepath.Join(e.cfg.DataDir, id, "decision.html"))
	t.Fatalf("stage-boundary page was not refreshed: %s", body)
}

func waitForDecisionPagePending(t *testing.T, e *Engine, issueID, stage string) PendingDecision {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, pending := range e.PendingDecisions() {
			if pending.IssueID == issueID && pending.Stage == stage {
				return pending
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	rows, _ := e.cfg.Store.Issues()
	events, _ := e.cfg.Store.EventsSince(0)
	t.Fatalf("pending %s review never appeared; issue rows=%+v events=%+v", stage, rows, events)
	return PendingDecision{}
}

func waitForDecisionPageFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil {
			return body
		}
		time.Sleep(10 * time.Millisecond)
	}
	body, err := os.ReadFile(path)
	t.Fatalf("decision page %s was not written: %v", path, err)
	return body
}
