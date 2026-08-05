package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		Recommended: 0, Importance: 0.8,
		Briefing: &levers.Briefing{NextAction: "Press 1.", Wins: []string{"tests pass"}},
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
		body, readErr := os.ReadFile(file)
		if readErr != nil {
			entries, _ := os.ReadDir(dir)
			events, _ := e.cfg.Store.EventsSince(0)
			t.Fatalf("page %s not written: %v; issue dir=%v events=%+v", file, readErr, entries, events)
		}
		page := string(body)
		if !strings.Contains(page, `id="decision"`) || !strings.Contains(page, p.D.Question) {
			t.Errorf("%s missing briefing content: %s", file, page)
		}
	}

	if err := e.Answer(p.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		body, readErr := os.ReadFile(perDecision)
		if readErr == nil && strings.Contains(string(body), "Answered") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	body, _ := os.ReadFile(perDecision)
	t.Fatalf("answered page was not stamped: %s", body)
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
