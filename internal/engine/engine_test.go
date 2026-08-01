package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if err := e.Answer(pending.ID, levers.FreeformResponse("not allowed")); err == nil {
		t.Fatal("invalid freeform response was accepted for a choice decision")
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
	e.Answer(pd.ID, levers.ChoiceResponse(1)) // abort
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
			e.Answer(ds[0].ID, levers.ChoiceResponse(0))
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
		Artifacts: []string{"merge-report.md", "merge-decision.json", "verification.json"},
	}}}
}

func verificationEngine(
	t *testing.T, decision string, commands [][]string, treeOverride string,
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
	f := verificationFlow()
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e := New(Config{
		Store: s, Pool: slots.NewPool(1), Flows: map[string]flow.Flow{"default": f},
		DataDir: t.TempDir(), Workspace: workspace.GitWorktree{Repo: repo},
		Train: &marshal.Train{Repo: repo, TestCmd: []string{"true"}},
		Runner: &runner.FakeRunner{Scripts: map[string]runner.Script{
			"merge-verification/merge-verifier": {Artifacts: map[string]string{
				"merge-report.md": "verified\n", "merge-decision.json": string(decisionBody),
				"verification.json": string(receipt),
			}},
		}},
	})
	return e, s, repo
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

func TestMergeVerificationRequiresMachineDecisionAndApplicableReceipt(t *testing.T) {
	tests := []struct {
		name     string
		decision string
		commands [][]string
		tree     string
		want     string
	}{
		{name: "unknown decision", decision: "maybe", commands: [][]string{{"true"}}, want: "merge decision"},
		{name: "missing configured gate", decision: "merge", commands: [][]string{{"go", "test"}}, want: "configured verification command"},
		{name: "different tree", decision: "merge", commands: [][]string{{"true"}}, tree: "different", want: "verified tree"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e, _, _ := verificationEngine(t, test.decision, test.commands, test.tree)
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
	e, s, repo := verificationEngine(t, "merge", [][]string{{"true"}}, "")
	fake := e.cfg.Runner.(*runner.FakeRunner)
	fake.OnStart = func(_, stage, _, _ string) error {
		if stage != "merge-verification" {
			return nil
		}
		return os.WriteFile(filepath.Join(repo, "diff"), []byte("dirty base\n"), 0o644)
	}
	id, err := e.CreateIssue("restart finalization", "", "default", levers.Matrix{}, 0, nil)
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
	if err != nil || len(runs) != 1 {
		t.Fatalf("rehydration reran verifier: %+v err %v", runs, err)
	}
	_, _, _, lastErr, err := s.LastStageEvents(id)
	if err != nil || lastErr != "" {
		t.Fatalf("restart synthesized stage failure: lastErr=%q err=%v", lastErr, err)
	}
}

type countingGitWorktree struct {
	repo     string
	acquired int
}

func (w *countingGitWorktree) Acquire(issueID string) (string, func() error, error) {
	w.acquired++
	return (workspace.GitWorktree{Repo: w.repo}).Acquire(issueID)
}

func (w *countingGitWorktree) Name() string { return "counting git worktree" }

func (w *countingGitWorktree) ReleasePath(path string) error {
	return (workspace.GitWorktree{Repo: w.repo}).ReleasePath(path)
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
	case "merge-verification":
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
		{Name: "merge-verification", Agents: []flow.AgentRef{{Package: "merge-verifier"}},
			Workspace: "worktree", Gate: flow.GateAuto, Completion: flow.CompletionAll,
			Artifacts: []string{"merge-report.md", "merge-decision.json", "verification.json"}},
	}}
	s, err := store.Open("file:" + strings.ReplaceAll(t.Name(), "/", "-") + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e := New(Config{
		Store: s, Runner: run, Pool: slots.NewPool(1),
		Flows: map[string]flow.Flow{"default": f}, DataDir: t.TempDir(), Workspace: ws,
		Train: &marshal.Train{Repo: repo, TestCmd: run.gate},
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
		{decision: "hold"},
		{decision: "resolved", merged: true, replayed: true},
		{decision: "invalid", wantErr: "conflict decision"},
		{decision: "missing", wantErr: "conflict-decision.json"},
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
		DataDir: t.TempDir(), Workspace: ws,
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
	f := flow.Flow{Name: "default", Stages: []flow.Stage{{
		Name: "execute", Agents: []flow.AgentRef{{Package: "executor"}},
		Gate: flow.GateAuto, Workspace: "worktree", Completion: flow.CompletionAll,
	}}}
	s, err := store.Open("file:cleanup-retry?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ws := &failOnceReleaseWorkspace{delegate: workspace.GitWorktree{Repo: repo}}
	e := New(Config{
		Store: s, Runner: &runner.FakeRunner{Scripts: map[string]runner.Script{
			"execute/executor": {},
		}},
		Pool: slots.NewPool(1), Flows: map[string]flow.Flow{"default": f},
		DataDir: t.TempDir(), Workspace: ws, Train: &marshal.Train{Repo: repo},
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
	if err := e.runStageOnce(context.Background(), is, noneStage(), 1, 1); err != nil {
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
		Flows: map[string]flow.Flow{"default": testFlow()}, DataDir: e.cfg.DataDir})
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
