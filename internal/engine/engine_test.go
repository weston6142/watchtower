package engine

import (
	"context"
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
		Flows: map[string]flow.Flow{"default": testFlow()},
		DataDir: t.TempDir(),
	}), s
}

func scripts() map[string]runner.Script {
	return map[string]runner.Script{
		"brainstorm/brainstorm": {Asks: []levers.Decision{
			{Question: "Scope ok?", Options: []string{"yes", "no"}, Recommended: 0, Importance: 0.3}}},
		"spec/spec-writer":            {Artifacts: map[string]string{"spec.md": ""}},
		"execute/executor":            {Artifacts: map[string]string{"diff": ""}, Tokens: 100},
		"review/clean-code-reviewer":  {Artifacts: map[string]string{"review.md": ""}},
		"review/reviewer":             {},
		"review/doc-writer":           {Artifacts: map[string]string{"docs": ""}},
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
