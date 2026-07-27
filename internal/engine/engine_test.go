package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wbushyeager/guildhall/internal/core"
	"github.com/wbushyeager/guildhall/internal/flow"
	"github.com/wbushyeager/guildhall/internal/levers"
	"github.com/wbushyeager/guildhall/internal/runner"
	"github.com/wbushyeager/guildhall/internal/slots"
	"github.com/wbushyeager/guildhall/internal/store"
)

func testFlow() flow.Flow {
	f, err := flow.Load("../flow/testdata/default.yaml")
	if err != nil {
		panic(err)
	}
	return f
}

func newEngine(t *testing.T, r runner.Runner) (*Engine, *store.Store) {
	t.Helper()
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return New(Config{
		Store: s, Runner: r, Pool: slots.NewPool(2),
		Flows:   map[string]flow.Flow{"default": testFlow()},
		DataDir: t.TempDir(),
	}), s
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

// YOLO everywhere: brainstorm ask auto-resolves, spec approve_artifact gate
// still escalates (importance 1.0 floor), so exactly one human decision.
func TestYoloRunEscalatesOnlyGate(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, err := e.CreateIssue("test", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)
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
	if err := e.Answer(pd.ID, 0); err != nil { // approve
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

func TestFailedAgentRetriesThenFails(t *testing.T) {
	sc := scripts()
	sc["execute/executor"] = runner.Script{Fail: true}
	e, s := newEngine(t, &runner.FakeRunner{Scripts: sc})
	id, _ := e.CreateIssue("boom", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)

	errc := make(chan error, 1)
	go func() { errc <- e.StartIssue(context.Background(), id) }()
	for {
		ds := e.PendingDecisions()
		if len(ds) == 1 {
			e.Answer(ds[0].ID, 0)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-errc; err == nil {
		t.Fatal("expected issue failure")
	}
	evs, _ := s.EventsSince(0)
	var started, failed int
	for _, ev := range evs {
		if ev.Type == core.EvStageStarted {
			started++
		}
		if ev.Type == core.EvStageFailed {
			failed++
		}
	}
	// brainstorm + spec + execute attempt1 + execute retry = 4 starts, 1 terminal fail
	if started != 4 || failed != 1 {
		t.Fatalf("started=%d failed=%d", started, failed)
	}
}

func TestEngineRecordsStageRuns(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, _ := e.CreateIssue("t", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)
	errC := make(chan error, 1)
	go func() { errC <- e.StartIssue(context.Background(), id) }()
	for {
		if ds := e.PendingDecisions(); len(ds) == 1 {
			e.Answer(ds[0].ID, 0)
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

func TestWorktreeAcquiredOnceAndReleased(t *testing.T) {
	ws := &fakeWS{dir: t.TempDir()}
	e, _ := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	e.cfg.Workspace = ws
	id, _ := e.CreateIssue("w", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)
	errC := make(chan error, 1)
	go func() { errC <- e.StartIssue(context.Background(), id) }()
	for {
		if ds := e.PendingDecisions(); len(ds) == 1 {
			e.Answer(ds[0].ID, 0)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-errC; err != nil {
		t.Fatal(err)
	}
	// default.yaml has two worktree stages (execute, review) — one acquire, one release
	if ws.acquired != 1 || ws.released != 1 {
		t.Fatalf("acquired=%d released=%d", ws.acquired, ws.released)
	}
}

func TestTokenBudgetEscalates(t *testing.T) {
	sc := scripts()
	sc["brainstorm/brainstorm"] = runner.Script{Tokens: 5000}
	e, s := newEngine(t, &runner.FakeRunner{Scripts: sc})
	e.cfg.TokenBudget = 1000
	id, _ := e.CreateIssue("b", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)
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
	e.Answer(pd.ID, 1) // abort
	if err := <-errC; err == nil {
		t.Fatal("expected abort error")
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
	id, _ := e.CreateIssue("a", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)
	errC := make(chan error, 1)
	go func() { errC <- e.StartIssue(context.Background(), id) }()
	for {
		if ds := e.PendingDecisions(); len(ds) == 1 {
			e.Answer(ds[0].ID, 0)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-errC; err != nil {
		t.Fatal(err)
	}
	rows, err := s.AllDecisionRows()
	if err != nil {
		t.Fatal(err)
	}
	var auto, answered int
	for _, r := range rows {
		switch r.Status {
		case "auto":
			auto++
		case "answered":
			answered++
		}
	}
	if auto != 1 || answered != 1 {
		t.Fatalf("auto=%d answered=%d rows=%+v", auto, answered, rows)
	}
}
