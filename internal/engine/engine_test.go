package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wbushyeager/guildhall/internal/core"
	"github.com/wbushyeager/guildhall/internal/flow"
	"github.com/wbushyeager/guildhall/internal/levers"
	"github.com/wbushyeager/guildhall/internal/runner"
	"github.com/wbushyeager/guildhall/internal/slots"
	"github.com/wbushyeager/guildhall/internal/store"
	"github.com/wbushyeager/guildhall/internal/touchset"
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

func TestWorktreeAcquiredOnceAndReleased(t *testing.T) {
	ws := &fakeWS{dir: t.TempDir()}
	initGitRepo(t, ws.dir)
	sc := scripts()
	sc["execute/executor"] = runner.Script{Artifacts: map[string]string{"diff": "changed\n"}}
	e, s := newEngine(t, &runner.FakeRunner{Scripts: sc})
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
	id, _ := e.CreateIssue("p", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)
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
			if err := e.Answer(ds[0].ID, 0); err != nil {
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
	id, _ := e.CreateIssue("k", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)
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

func TestRetryStageResumesFromFailure(t *testing.T) {
	sc := scripts()
	sc["execute/executor"] = runner.Script{Fail: true}
	fr := &runner.FakeRunner{Scripts: sc}
	e, s := newEngine(t, fr)
	id, _ := e.CreateIssue("r", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)
	errC := make(chan error, 1)
	go func() { errC <- e.StartIssue(context.Background(), id) }()
	for {
		if ds := e.PendingDecisions(); len(ds) == 1 {
			if err := e.Answer(ds[0].ID, 0); err != nil {
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
	id, _ := e.CreateIssue("l", "", "default", levers.Preset(testFlow(), flow.LeverStrict), 0)
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

type recordingSequencer struct {
	planned int
	merged  int
}

func (s *recordingSequencer) BlockedBehind(string) int { return 0 }
func (s *recordingSequencer) PlanApproved(string, touchset.Set) {
	s.planned++
}
func (s *recordingSequencer) ReadyToMerge(context.Context, string) error { return nil }
func (s *recordingSequencer) Merged(string)                              { s.merged++ }
func (s *recordingSequencer) Aborted(string)                             {}

func TestMarshalReleasedAfterSuccessfulCompletionWithoutTrain(t *testing.T) {
	f := flow.Flow{Name: "default", Stages: []flow.Stage{
		{Name: "plan", Agents: []flow.AgentRef{{Package: "planner"}}, Gate: flow.GateApproveArtifact, Artifacts: []string{"touchset.json"}},
		{Name: "merge", Agents: []flow.AgentRef{{Package: "reviewer"}}, Gate: flow.GateAuto, MergeBarrier: true},
	}}
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seq := &recordingSequencer{}
	e := New(Config{
		Store: s, Runner: &runner.FakeRunner{Scripts: map[string]runner.Script{
			"plan/planner":   {Artifacts: map[string]string{"touchset.json": `{"globs":["src/**"]}`}},
			"merge/reviewer": {},
		}},
		Marshal: seq, Pool: slots.NewPool(1), Flows: map[string]flow.Flow{"default": f}, DataDir: t.TempDir(),
	})
	id, err := e.CreateIssue("marshal", "", "default", levers.Preset(f, flow.LeverYolo), 0)
	if err != nil {
		t.Fatal(err)
	}
	errC := make(chan error, 1)
	go func() { errC <- e.StartIssue(context.Background(), id) }()
	for {
		if ds := e.PendingDecisions(); len(ds) == 1 {
			if err := e.Answer(ds[0].ID, 0); err != nil {
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
