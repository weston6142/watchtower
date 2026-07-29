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

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/librarian"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/slots"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/touchset"
	"github.com/weston6142/watchtower/internal/workspace"
)

func testFlow() flow.Flow {
	f, err := flow.Load("../flow/testdata/default.yaml")
	if err != nil {
		panic(err)
	}
	return f
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
		Flows:   map[string]flow.Flow{"default": testFlow()},
		DataDir: t.TempDir(),
	}
	if adjust != nil {
		adjust(&cfg)
	}
	return New(cfg), s
}

func newEngine(t *testing.T, r runner.Runner) (*Engine, *store.Store) {
	t.Helper()
	return newEngineCfg(t, r, nil)
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
	id, _ := e.CreateIssue("t", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
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
	id, _ := e.CreateIssue("w", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
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
	id, _ := e.CreateIssue("a", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
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
	id, err := e.CreateIssue("marshal", "", "default", levers.Preset(f, flow.LeverYolo), 0, nil)
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

// newEngineOnFile builds an engine on a file-backed store so a second engine
// can be constructed on the same durable state, simulating a daemon restart.
func newEngineOnFile(t *testing.T, s *store.Store, r runner.Runner, dataDir string) *Engine {
	t.Helper()
	return New(Config{
		Store: s, Runner: r, Pool: slots.NewPool(2),
		Flows:   map[string]flow.Flow{"default": testFlow()},
		DataDir: dataDir,
	})
}

func TestRehydrateAfterDaemonRestart(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "gh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	dataDir := t.TempDir()

	// Engine 1: run until the spec approve_artifact gate parks a decision.
	e1 := newEngineOnFile(t, s, &runner.FakeRunner{Scripts: scripts()}, dataDir)
	id, err := e1.CreateIssue("restart me", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
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

	// "Restart": a fresh engine on the same store knows nothing in memory.
	e2 := newEngineOnFile(t, s, &runner.FakeRunner{Scripts: scripts()}, dataDir)
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if err := e2.Rehydrate(); err != nil { // idempotent
		t.Fatal(err)
	}

	// nextID advanced past existing issues: no GH-1 collision.
	id2, err := e2.CreateIssue("after restart", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0, nil)
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
	go func() {
		deadline := time.After(5 * time.Second)
		for {
			if ds := e2.PendingDecisions(); len(ds) == 1 {
				_ = e2.Answer(ds[0].ID, 0)
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
		DataDir:   t.TempDir(),
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
	gitc(remote, "init", "-q", "--bare")
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
		DataDir:   t.TempDir(),
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
	if err := e.runStageOnce(context.Background(), is, noneStage(), 1, 1); err != nil {
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
	if err := e.runStageOnce(context.Background(), is, worktreeStage(), 1, 1); err != nil {
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
	err = e.runStageOnce(context.Background(), is, worktreeStage(), 1, 1)
	if err == nil || !strings.Contains(err.Error(), "is not Guildhall's") {
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
	if err := e.runStageOnce(context.Background(), is, noneStage(), 1, 1); err != nil {
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
	if err := e.runStageOnce(context.Background(), is, noneStage(), 1, 1); err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(filepath.Join(e.cfg.DataDir, id, "ISSUE.md"))
	if strings.Contains(string(md), "# Attachments") {
		t.Fatalf("empty set produced a section:\n%s", md)
	}
}
