package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/decisionpage"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/runner"
)

func TestDecisionPageWritten(t *testing.T) {
	f := flow.Flow{Name: "decision-page", Stages: []flow.Stage{
		{Name: "execute", Agents: []flow.AgentRef{{Package: "agent"}}, Gate: flow.GateAuto, Completion: flow.CompletionAll},
	}}
	decision := levers.Decision{
		Kind: levers.DecisionChoice, Question: "Gate the migration?", Options: []string{"Gate", "Apply"},
		Recommended: 0, Importance: 0.8, Why: "Compatibility is preserved.",
		Consequences: []string{"Old daemons continue safely.", "Old daemons fail on restart."},
		Briefing: &levers.Briefing{Proof: []levers.BriefingProof{{
			Claim: "Migration tests pass.", Cite: "go test ./internal/store",
		}}},
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
	go func() { _ = e.StartIssue(context.Background(), id) }()

	p := waitForDecisionPagePending(t, e, id, "execute")
	dir := filepath.Join(e.cfg.DataDir, id)
	perDecision := filepath.Join(dir, "decisions", fmt.Sprintf("%d.html", p.ID))
	latest := filepath.Join(dir, "decision.html")
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
	}

	if err := e.Answer(p.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		body, readErr := os.ReadFile(perDecision)
		if readErr == nil && strings.Contains(string(body), "Answered") {
			waitForEvent(t, e.cfg.Store, id, core.EvIssueCompleted)
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
		nil, nil, 42, "",
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
	body, err := os.ReadFile(pagePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Review spec.md", "approve", "revise",
		"advances to execute", "repeats spec",
		"spec.md is archived and ready for review", "checkpoint",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("artifact review page missing %q: %s", want, body)
		}
	}

	if err := e.Answer(pending.ID, levers.ChoiceResponse(1)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		checkpoints, checkErr := s.StageCheckpoints(id)
		if checkErr == nil {
			for _, checkpoint := range checkpoints {
				if checkpoint.Stage == "spec" && checkpoint.Status == "revision_required" {
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
		"completes the workflow", "Approve to complete the workflow", "Revise to repeat publish",
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
	for _, want := range []string{`id="decision"`, "decision · publish", "Answered:"} {
		if !strings.Contains(answered, want) {
			t.Errorf("answered terminal review page missing %q: %s", want, answered)
		}
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
	go func() { _ = e.StartIssue(context.Background(), id) }()
	p := waitForDecisionPagePending(t, e, id, "execute")
	if err := e.Answer(p.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		body, readErr := os.ReadFile(filepath.Join(e.cfg.DataDir, id, "decision.html"))
		if readErr == nil && strings.Contains(string(body), `class="pill done"`) &&
			strings.Contains(string(body), "Stage <b>2 of 2</b> — verify") {
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
