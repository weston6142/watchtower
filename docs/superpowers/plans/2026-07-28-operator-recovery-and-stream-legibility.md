# Operator Recovery and Stream Legibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a stopped lane always recoverable with one key, make the tower say *where* it is parked, and turn the transcript door into a legible live view of what the agent is doing.

**Architecture:** Four independent slices over an existing event-sourced Go TUI. The engine gains a restart path for lanes with no live goroutine. The paused event starts carrying the stage it parked at, so the projection can point the grid at one cell instead of painting every row. The Bubble Tea `Update` loop gains a modal-capture branch for the help overlay. The claude stream parser stops discarding `tool_use` blocks, and the transcript door renders through the same box chrome the help overlay uses.

**Tech Stack:** Go 1.x, Bubble Tea (`tea.Model` update/view loop), Lipgloss for styling, SQLite-backed event store, golden-file snapshot tests.

## Global Constraints

- Module path is `github.com/weston6142/watchtower`. All internal imports use it.
- No new third-party dependencies. Everything needed is already in `go.mod`.
- Full suite must pass: `go test ./...`
- Never regenerate golden snapshots blind. When a snapshot task says to run `-update`, read the resulting diff and confirm it matches the intended change before committing.
- Events already written to the store carry old payload shapes. Every projection change must leave pre-existing events rendering as they do today.
- Comments explain *why*, not *what*. Match the density of the surrounding file.
- Commit after every task.

---

### Task 1: `Resume` restarts a lane with no live waiter

`Engine.Resume` closes `is.pauseGate` and returns success whether or not a goroutine is selecting on it. The gate is only selected on inside `runFrom`'s stage loop, so after a kill, an abort, or a daemon restart the close is a silent no-op.

The discriminator for "should this restart?" is `is.terminal`, not merely `!is.running`. A lane that was created and paused but never started also has `running == false`; restarting that one would launch an issue the operator never asked to start. `Rehydrate` sets `terminal: true` on every lane it restores (`engine.go:189`), and `StartIssue`/`RetryStage` set it from `runFrom`'s error — so `terminal` is exactly "this lane ran and stopped".

**Files:**
- Modify: `internal/engine/engine.go:239-253` (`Resume`), `internal/engine/engine.go:805-843` (`StartIssue`, `RetryStage`)
- Test: `internal/engine/engine_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func (e *Engine) runAndRecord(ctx context.Context, is *issueState, startIdx int) error` — runs `runFrom` and records `is.terminal`. `Resume` keeps its existing signature `func (e *Engine) Resume(issueID string) error`.

- [ ] **Step 1: Write the failing test**

Add to `internal/engine/engine_test.go`:

```go
// A killed lane has no goroutine waiting at the gate. Resume must restart the
// stage that was killed rather than close a channel nobody is listening on.
func TestResumeRestartsKilledLane(t *testing.T) {
	sc := scripts()
	sc["brainstorm/brainstorm"] = runner.Script{Asks: []levers.Decision{
		{Question: "block forever?", Options: []string{"a"}, Recommended: 0, Importance: 1.0}}}
	fr := &runner.FakeRunner{Scripts: sc}
	e, s := newEngine(t, fr)
	id, _ := e.CreateIssue("k", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)

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
	id, _ := e.CreateIssue("u", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)
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
	id, _ := e.CreateIssue("n", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/engine -run 'TestResume' -v`
Expected: `TestResumeRestartsKilledLane` FAILS with "brainstorm never restarted (starts=1)". `TestResumeDoesNotStartUnstartedLane` PASSES already. `TestResumeRunningLaneWithNoGateErrors` PASSES already. The two passing tests are regression guards for the change below.

- [ ] **Step 3: Extract the shared run bookkeeping**

In `internal/engine/engine.go`, add below `runFrom`:

```go
// runAndRecord runs an issue from startIdx and records whether it ended
// terminally. StartIssue, RetryStage, and Resume all restart a lane through
// this one path so their bookkeeping cannot drift apart.
func (e *Engine) runAndRecord(ctx context.Context, is *issueState, startIdx int) error {
	err := e.runFrom(ctx, is, startIdx)
	e.mu.Lock()
	is.terminal = err != nil
	e.mu.Unlock()
	return err
}
```

Rewrite the tail of `StartIssue` to use it:

```go
	return e.runAndRecord(ctx, is, 0)
```

replacing the existing `err := e.runFrom(ctx, is, 0)` / lock / `is.terminal = err != nil` / unlock / `return err` block.

Rewrite the tail of `RetryStage` the same way — replace its `err := e.runFrom(ctx, is, startIdx)` / lock / assign / unlock / `return err` block with:

```go
	return e.runAndRecord(ctx, is, startIdx)
```

- [ ] **Step 4: Rewrite `Resume`**

Replace `Resume` in `internal/engine/engine.go` with:

```go
// Resume continues a stopped lane. A lane whose runFrom goroutine is alive is
// released from its between-stage gate. A lane whose goroutine has already
// exited — killed, aborted, or rehydrated after a daemon restart — has no
// waiter to release, so it is restarted at the stage it stopped on instead;
// closing the gate there would report success and do nothing.
func (e *Engine) Resume(issueID string) error {
	e.mu.Lock()
	is, ok := e.issues[issueID]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("unknown issue %s", issueID)
	}
	if is.running {
		if is.pauseGate == nil {
			e.mu.Unlock()
			return fmt.Errorf("issue %s is not paused", issueID)
		}
		close(is.pauseGate)
		is.pauseGate = nil
		e.mu.Unlock()
		return nil
	}
	// terminal, not merely stopped: a lane created and paused before it ever
	// started must stay unstarted, and only clear its gate.
	if !is.terminal {
		is.pauseGate = nil
		e.mu.Unlock()
		return nil
	}
	startIdx := is.stageIdx
	is.pauseGate = nil
	is.killRequested = false
	is.terminal = false
	e.mu.Unlock()
	// Detached, like start_issue and retry_stage: failures surface as
	// stage_failed events, not in the caller's response.
	go e.runAndRecord(context.Background(), is, startIdx)
	return nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/engine -run 'TestResume|TestPauseGates|TestKillStage|TestRetryStage|TestRehydrate' -v`
Expected: all PASS.

- [ ] **Step 6: Run the full suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/engine/engine.go internal/engine/engine_test.go
git commit -m "fix: resume restarts a stopped lane instead of closing a dead gate"
```

---

### Task 2: Resuming clears the killed flag

`EvIssueResumed` sets `Paused = false` and `State = "running"` but leaves `Killed` set. The TUI's `R` retry guard (`app.go:520`) tests `iv.State == "failed" || iv.Killed`, so a resumed lane keeps offering retry on the strength of a kill it has already recovered from.

**Files:**
- Modify: `internal/projection/projection.go:154-158`
- Test: `internal/projection/projection_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: nothing new; behavior change only.

- [ ] **Step 1: Write the failing test**

Add to `internal/projection/projection_test.go`. That file already has an `ev(t, typ, issue, payload)` helper and applies events with `s.Apply(...)`:

```go
// A resumed lane has recovered from its kill; leaving Killed set keeps the
// TUI's retry affordance armed for a stage that is running again.
func TestResumeClearsKilled(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t"}))
	s.Apply(ev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "execute"}))
	s.Apply(ev(t, core.EvStageKilled, "GH-1", map[string]any{"stage": "execute"}))
	if !s.Issues["GH-1"].Killed {
		t.Fatal("expected Killed after stage_killed")
	}

	s.Apply(ev(t, core.EvIssueResumed, "GH-1", nil))
	iv := s.Issues["GH-1"]
	if iv.Killed {
		t.Fatal("resume left Killed set")
	}
	if iv.Paused || iv.State != "running" {
		t.Fatalf("resume state: paused=%v state=%q", iv.Paused, iv.State)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/projection -run TestResumeClearsKilled -v`
Expected: FAIL with "resume left Killed set".

- [ ] **Step 3: Clear the flag**

In `internal/projection/projection.go`, change the `EvIssueResumed` case to:

```go
	case core.EvIssueResumed:
		if iv != nil {
			iv.Paused = false
			// The lane recovered from whatever stopped it; a stale Killed
			// keeps the retry affordance armed for a running stage.
			iv.Killed = false
			iv.State = "running"
		}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/projection -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/projection/projection.go internal/projection/projection_test.go
git commit -m "fix: resuming a lane clears its killed flag"
```

---

### Task 3: The paused event carries the stage it parked at

The grid needs to know *where* a lane stopped. Today `EvIssuePaused` is emitted with a nil payload (`engine.go:742`) and `EvStageCompleted` appends to `Completed` without advancing `CurrentStage` — so a lane parked after `brainstorm` still reports `CurrentStage == "brainstorm"`, a stage that is simultaneously in `Completed`.

**Files:**
- Modify: `internal/engine/engine.go:740-747` (the gate block in `runFrom`)
- Modify: `internal/projection/projection.go:149-153` (`EvIssuePaused`)
- Test: `internal/projection/projection_test.go`, `internal/engine/engine_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `EvIssuePaused` payload shape `{"stage": "<upcoming stage name>"}`. Task 4 relies on `IssueView.CurrentStage` naming an uncompleted stage for a paused lane.

- [ ] **Step 1: Write the failing projection test**

Add to `internal/projection/projection_test.go`:

```go
// The paused marker has to land on the stage the lane will resume into, not
// the one that just finished — otherwise it collides with that stage's tick.
func TestPausedEventAdvancesCurrentStage(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t"}))
	s.Apply(ev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}))
	s.Apply(ev(t, core.EvStageCompleted, "GH-1", map[string]any{"stage": "brainstorm"}))
	s.Apply(ev(t, core.EvIssuePaused, "GH-1", map[string]any{"stage": "spec"}))
	if got := s.Issues["GH-1"].CurrentStage; got != "spec" {
		t.Fatalf("CurrentStage = %q, want spec", got)
	}
}

// Events already in the store carry no payload; they must not blank the stage.
func TestPausedEventWithoutStageLeavesCurrentStage(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t"}))
	s.Apply(ev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}))
	s.Apply(ev(t, core.EvIssuePaused, "GH-1", nil))
	iv := s.Issues["GH-1"]
	if iv.CurrentStage != "brainstorm" {
		t.Fatalf("CurrentStage = %q, want brainstorm", iv.CurrentStage)
	}
	if !iv.Paused || iv.State != "paused" {
		t.Fatalf("paused=%v state=%q", iv.Paused, iv.State)
	}
}
```

- [ ] **Step 2: Run to verify the first fails**

Run: `go test ./internal/projection -run TestPausedEvent -v`
Expected: `TestPausedEventAdvancesCurrentStage` FAILS with `CurrentStage = "brainstorm", want spec`. The second PASSES (regression guard).

- [ ] **Step 3: Read the stage from the payload**

In `internal/projection/projection.go`, change the `EvIssuePaused` case to:

```go
	case core.EvIssuePaused:
		if iv != nil {
			iv.Paused = true
			iv.State = "paused"
			// The gate sits before the upcoming stage, so that is where the
			// lane is parked. Older events carry no payload; leave those.
			if stage := str("stage"); stage != "" {
				iv.CurrentStage = stage
			}
		}
```

- [ ] **Step 4: Emit the stage from the engine**

In `internal/engine/engine.go`, inside `runFrom`'s gate block, change:

```go
			e.emit(core.EvIssuePaused, is.id, nil)
```

to:

```go
			e.emit(core.EvIssuePaused, is.id, map[string]string{"stage": st.Name})
```

`st` is the loop's current `flow.Stage`, already in scope from `st := f.Stages[i]`.

- [ ] **Step 5: Add the engine assertion**

Add to `internal/engine/engine_test.go`:

```go
// The pause gate parks a lane before a stage; the event has to name it so the
// grid can mark one cell instead of the whole column.
func TestPausedEventNamesUpcomingStage(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, _ := e.CreateIssue("p", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)
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
```

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/projection ./internal/engine -v`
Expected: PASS.

- [ ] **Step 7: Run the full suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/engine/engine.go internal/engine/engine_test.go internal/projection/projection.go internal/projection/projection_test.go
git commit -m "feat: issue_paused names the stage the lane parked at"
```

---

### Task 4: The grid marks one paused cell, not the whole column

`cellContentForStage` (`render.go:177`) tests `iv.Paused` before anything else, so a paused lane paints `⏸ paused` in every stage row and the operator cannot see how far it got.

The branch must keep its position in the precedence order and gain a guard. It cannot simply be moved below the other state checks: those all compare `iv.State` against `"running"`, `"waiting_decision"`, or `"failed"`, and a paused lane's state is `"paused"` — none would match, and the marker would disappear from every cell instead of narrowing to one.

**Files:**
- Modify: `internal/tui/render.go:163-208` (`cellContentForStage`)
- Test: `internal/tui/render_test.go`

**Interfaces:**
- Consumes: `IssueView.CurrentStage` naming an uncompleted stage for paused lanes (Task 3).
- Produces: `func pausedAtStage(iv *projection.IssueView, stage string, stageIdx int) bool`.

- [ ] **Step 1: Write the failing test**

Add to `internal/tui/render_test.go`:

```go
// A parked lane must still show how far it got. Painting every row "paused"
// hides the resume point and reads as a hung tower.
func TestPausedLaneMarksOnlyItsCurrentStage(t *testing.T) {
	iv := &projection.IssueView{
		ID: "GH-1", Title: "t",
		Completed: []string{"brainstorm"}, CurrentStage: "spec",
		Paused: true, State: "paused",
	}
	stages := []string{"brainstorm", "spec", "execute"}
	var got []string
	for i, stage := range stages {
		got = append(got, ansi.Strip(cellContentForStage(iv, nil, stage, i, 0, false, true)))
	}
	if strings.Contains(got[0], "paused") {
		t.Fatalf("completed stage shows paused: %q", got[0])
	}
	if !strings.Contains(got[0], glyphDone) {
		t.Fatalf("completed stage lost its tick: %q", got[0])
	}
	if !strings.Contains(got[1], "paused") {
		t.Fatalf("current stage missing paused: %q", got[1])
	}
	if strings.Contains(got[2], "paused") {
		t.Fatalf("later stage shows paused: %q", got[2])
	}
}

// Rehydrated and pre-payload lanes have no usable CurrentStage; the marker
// falls back to the first stage that has not finished.
func TestPausedLaneWithStaleCurrentStageFallsBack(t *testing.T) {
	iv := &projection.IssueView{
		ID: "GH-1", Title: "t",
		Completed: []string{"brainstorm"}, CurrentStage: "brainstorm",
		Paused: true, State: "paused",
	}
	stages := []string{"brainstorm", "spec", "execute"}
	var got []string
	for i, stage := range stages {
		got = append(got, ansi.Strip(cellContentForStage(iv, nil, stage, i, 0, false, true)))
	}
	if !strings.Contains(got[0], glyphDone) {
		t.Fatalf("completed stage lost its tick: %q", got[0])
	}
	if !strings.Contains(got[1], "paused") {
		t.Fatalf("expected fallback marker on spec: %q", got[1])
	}
}
```

Add `"github.com/charmbracelet/x/ansi"`, `"strings"`, and `"github.com/weston6142/watchtower/internal/projection"` to that file's imports if they are not already there.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tui -run TestPausedLane -v`
Expected: both FAIL with "completed stage shows paused".

- [ ] **Step 3: Add the guard helper**

In `internal/tui/render.go`, next to `completedStage`, add:

```go
// pausedAtStage reports whether this cell is where a parked lane stopped.
// CurrentStage is authoritative when it names a stage that has not finished;
// rehydrated lanes and pre-payload pause events leave it pointing at a
// completed stage, so fall back to the first unfinished one.
func pausedAtStage(iv *projection.IssueView, stage string, stageIdx int) bool {
	if iv.CurrentStage != "" && !completedStage(iv, iv.CurrentStage) {
		return iv.CurrentStage == stage
	}
	return stageIdx == len(iv.Completed)
}
```

- [ ] **Step 4: Guard the paused branch**

In `cellContentForStage`, replace:

```go
	if iv.Paused || iv.Killed || iv.State == "paused" {
		return lipgloss.NewStyle().Foreground(activeTheme.Dim).Render(glyphParked + " paused")
	}
```

with:

```go
	if (iv.Paused || iv.Killed || iv.State == "paused") && pausedAtStage(iv, stage, stageIdx) {
		return lipgloss.NewStyle().Foreground(activeTheme.Dim).Render(glyphParked + " paused")
	}
```

Stages other than the parked one now fall through to the completed/waiting logic below, which is what gives finished stages their tick back.

Note the fallback's assumption: `stageIdx == len(iv.Completed)` treats `Completed` as a dense prefix of the flow. Task 1's restart re-runs `is.stageIdx`, which for a killed-then-resumed lane can be a stage already in `Completed`, and `EvStageCompleted` appends unconditionally — so `Completed` can hold a duplicate and the length overshoot would put the fallback marker one cell too far right. `completedStage` does a linear scan, so the ticks stay correct either way. This needs a resume-after-kill *and* a pause event with no stage payload to be reachable at all, so it is not worth blocking on; if it shows up, the fix is to make the projection skip appending a stage already in `Completed` rather than to complicate this renderer.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/tui -run TestPausedLane -v`
Expected: PASS.

- [ ] **Step 6: Update the goldens**

Run: `go test ./internal/tui -run TestSnapshots -update`
Then: `git diff internal/tui/testdata`

The fixture lane `fx-dark` is `Paused: true` with no `Completed` and no `CurrentStage`, so it should change from paused-in-every-row to paused on the first stage only. Read the diff and confirm that is what changed — nothing else should move. If other lanes shifted, stop and find out why before committing.

- [ ] **Step 7: Run the full suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/tui/render.go internal/tui/render_test.go internal/tui/testdata
git commit -m "fix: paused lanes mark one cell instead of the whole column"
```

---

### Task 5: The notice row names the parked lane

`renderNoticeRow` renders blank space whenever there are no notices, so a lane waiting on a human sits under a header reading `● all clear`. Notices are event-sourced and only produced by proposals, so the parked hint is derived at render time rather than pushed into the event log.

Killed lanes are deliberately excluded: `R` remains their idiomatic verb, and advertising `p` for them would compete with the existing kill/retry copy.

**Files:**
- Modify: `internal/tui/render.go:68-77` (`renderNoticeRow`)
- Modify: `internal/tui/app.go` (`writeHeaderRows`, the single caller)
- Test: `internal/tui/render_test.go`

**Interfaces:**
- Consumes: `IssueView.Paused`, `IssueView.CurrentStage` (Task 3).
- Produces: `func renderNoticeRow(st *projection.State, ids map[string]Identity, width int) string` — signature changed, one caller. `func parkedHint(st *projection.State, ids map[string]Identity) string`.

- [ ] **Step 1: Write the failing test**

Add to `internal/tui/render_test.go`:

```go
// A lane waiting on a human under a blank notice row reads as a hung tower.
func TestNoticeRowNamesParkedLane(t *testing.T) {
	st := projection.NewState()
	st.Order = []string{"GH-2"}
	st.Issues["GH-2"] = &projection.IssueView{
		ID: "GH-2", Title: "rewrite refs",
		Completed: []string{"brainstorm"}, CurrentStage: "spec",
		Paused: true, State: "paused",
	}
	ids := map[string]Identity{"GH-2": {Tag: "RG"}}
	got := ansi.Strip(renderNoticeRow(st, ids, 80))
	if !strings.Contains(got, "RG") || !strings.Contains(got, "spec") || !strings.Contains(got, "p resumes") {
		t.Fatalf("notice row = %q", got)
	}
}

// Real notices outrank the derived hint.
func TestNoticeRowPrefersRealNotices(t *testing.T) {
	st := projection.NewState()
	st.Order = []string{"GH-2"}
	st.Issues["GH-2"] = &projection.IssueView{ID: "GH-2", Paused: true, State: "paused"}
	st.Notices = []projection.Notice{{Text: "✉ new idea from GH-3: something", Seq: 1}}
	got := ansi.Strip(renderNoticeRow(st, nil, 80))
	if !strings.Contains(got, "new idea") {
		t.Fatalf("notice row = %q", got)
	}
}

// Nothing parked, nothing to say — the row stays blank at full width.
func TestNoticeRowBlankWhenNothingParked(t *testing.T) {
	st := projection.NewState()
	st.Order = []string{"GH-2"}
	st.Issues["GH-2"] = &projection.IssueView{ID: "GH-2", State: "running", CurrentStage: "spec"}
	if got := renderNoticeRow(st, nil, 20); strings.TrimSpace(got) != "" {
		t.Fatalf("notice row = %q, want blank", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tui -run TestNoticeRow -v`
Expected: compile error — `renderNoticeRow` takes two arguments, not three. That is the failure; proceed.

- [ ] **Step 3: Rewrite the notice row**

In `internal/tui/render.go`, replace `renderNoticeRow` with:

```go
func renderNoticeRow(st *projection.State, ids map[string]Identity, width int) string {
	text := ""
	switch {
	case st != nil && len(st.Notices) > 0:
		text = strings.ReplaceAll(st.Notices[len(st.Notices)-1].Text, "\n", " ")
	default:
		text = parkedHint(st, ids)
	}
	if text == "" {
		if width > 0 {
			return strings.Repeat(" ", width)
		}
		return " "
	}
	return truncate(text, width)
}

// parkedHint names the first parked lane and the key that continues it. A lane
// waiting on a human with nothing on screen saying so reads as a hung tower.
// Killed lanes are left out: R is their verb, and the kill copy already says so.
func parkedHint(st *projection.State, ids map[string]Identity) string {
	if st == nil {
		return ""
	}
	for _, id := range st.Order {
		iv := st.Issues[id]
		if iv == nil || iv.Killed || !(iv.Paused || iv.State == "paused") {
			continue
		}
		tag := id
		if identity, ok := ids[id]; ok && identity.Tag != "" {
			tag = identity.Tag
		}
		if iv.CurrentStage != "" {
			return glyphParked + " " + tag + " parked at " + iv.CurrentStage + " — p resumes"
		}
		return glyphParked + " " + tag + " parked — p resumes"
	}
	return ""
}
```

- [ ] **Step 4: Update the caller**

In `internal/tui/app.go`, in `writeHeaderRows`, change:

```go
	b.WriteString(renderNoticeRow(m.State, width))
```

to:

```go
	b.WriteString(renderNoticeRow(m.State, m.Ids, width))
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/tui -run TestNoticeRow -v`
Expected: PASS.

- [ ] **Step 6: Update the goldens**

Run: `go test ./internal/tui -run TestSnapshots -update`
Then: `git diff internal/tui/testdata`

The fixture's `fx-dark` lane is paused, so every flow's notice row should now carry the parked hint. Confirm the diff shows only that row changing.

- [ ] **Step 7: Run the full suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/tui/render.go internal/tui/render_test.go internal/tui/app.go internal/tui/testdata
git commit -m "feat: notice row names the parked lane and the key that resumes it"
```

---

### Task 6: The footer stops lying, and `T` explains itself

The footer legend says `p pause` while the help overlay says `p pause / resume`; the footer is wrong. `T` is missing from the footer entirely, and pressing it with no lane focused calls `fetchTranscript`, which returns nil for an empty focus — opening a blank door with no explanation, unlike every other issue-scoped key.

**Files:**
- Modify: `internal/tui/app.go:1220-1223` (`mainBindings`), `internal/tui/app.go:430-432` (the `T` case)
- Test: `internal/tui/app_test.go`

**Interfaces:**
- Consumes: `laneModel`, `pressKey`, `mkev` test helpers already in `app_test.go`.
- Produces: nothing new.

- [ ] **Step 1: Write the failing test**

Add to `internal/tui/app_test.go`:

```go
// T is issue-scoped like p, x, and R. Opening an empty door instead of saying
// why is the same silent no-op those keys were fixed for.
func TestTranscriptKeyHintsWhenNothingFocused(t *testing.T) {
	m := NewModel(nil, []string{"brainstorm", "spec", "execute", "review", "merge"})
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
	})
	m = pressKey(t, m, "T")
	if m.Err != "no lane focused — press j or 1-9 to focus" {
		t.Fatalf("expected no-focus hint, got %q", m.Err)
	}
	if len(m.modes) != 0 {
		t.Fatalf("transcript door opened without focus: %v", m.modes)
	}
}

// With a lane focused the door still opens.
func TestTranscriptKeyOpensDoorWhenFocused(t *testing.T) {
	m := laneModel(t,
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
	)
	m = pressKey(t, m, "T")
	if m.currentMode() != "transcript" {
		t.Fatalf("mode = %q, want transcript", m.currentMode())
	}
}
```

- [ ] **Step 2: Run the tests to verify the first fails**

Run: `go test ./internal/tui -run TestTranscriptKey -v`
Expected: `TestTranscriptKeyHintsWhenNothingFocused` FAILS — `m.Err` is empty and `m.modes` contains `transcript`.

- [ ] **Step 3: Guard the `T` case**

In `internal/tui/app.go`, replace:

```go
		case "T":
			m.modes = append(m.modes, "transcript")
			return m, m.fetchTranscript()
```

with:

```go
		case "T":
			// Issue-scoped like p and R: without focus fetchTranscript returns
			// nil and the door opens empty, which reads as broken.
			if m.Focus.Issue == "" {
				m.Err = "no lane focused — press j or 1-9 to focus"
				return m, nil
			}
			m.modes = append(m.modes, "transcript")
			return m, m.fetchTranscript()
```

- [ ] **Step 4: Fix the footer legend**

In `internal/tui/app.go`, replace `mainBindings` with:

```go
	mainBindings := [][2]string{
		{"j/k", "floors"}, {"tab", "attention"}, {"p", "pause/resume"}, {"x", "kill"},
		{"R", "retry"}, {"T", "stream"}, {"L", "levers"}, {"?", "help"}, {"q", "quit"},
	}
```

Task 10 retitles that door from "transcript" to "stream", so align the help overlay's copy now rather than leaving two names for one door. In `internal/tui/render.go`, in the `DOORS` group of `helpGroups`, change `{"T", "transcript"}` to `{"T", "stream"}`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/tui -run TestTranscriptKey -v`
Expected: PASS.

- [ ] **Step 6: Update the goldens and check the narrow width**

Run: `go test ./internal/tui -run TestSnapshots -update`
Then: `git diff internal/tui/testdata`

The footer gained a chip. Check the `-narrow` goldens (100 columns) specifically: `chromeBar` collapses its gap to a single space rather than truncating, so if the bar no longer fits, the right-hand slot will be crowded out. If the narrow footer is unreadable, shorten `{"tab", "attention"}` to `{"tab", "next"}` and re-run `-update`.

- [ ] **Step 7: Run the full suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/tui/app.go internal/tui/app_test.go internal/tui/testdata
git commit -m "fix: footer says pause/resume and lists T; T hints without focus"
```

---

### Task 7: The help overlay consumes keys

`m.help` is read only in `View()` (`app.go:1167`, `app.go:1234`) and never in `Update()`. The overlay paints over the screen while every key still drives the app beneath it: `esc` falls through to the next handler in the chain, `p` pauses a lane, `x` arms a kill confirm behind the overlay. The overlay's own footer advertises `? / esc close` — a binding it never receives.

**Files:**
- Modify: `internal/tui/app.go:316-323` (top of the `tea.KeyMsg` branch)
- Test: `internal/tui/app_test.go`

**Interfaces:**
- Consumes: `laneModel`, `pressKey` from `app_test.go`.
- Produces: nothing new.

- [ ] **Step 1: Write the failing test**

Add to `internal/tui/app_test.go`:

```go
// The overlay paints over the grid, so it has to swallow the grid's keys.
// Driving a lane you cannot see is worse than the key doing nothing.
func TestHelpOverlaySwallowsIssueKeys(t *testing.T) {
	m := laneModel(t,
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
	)
	m = pressKey(t, m, "?")
	if !m.help {
		t.Fatal("? did not open help")
	}
	for _, key := range []string{"p", "x", "X", "L", "d", "T", "n"} {
		m = pressKey(t, m, key)
		if !m.help {
			t.Fatalf("key %q closed the help overlay", key)
		}
		if m.confirm != nil || m.modal != nil || m.leverEditor != nil || len(m.modes) != 0 {
			t.Fatalf("key %q drove the screen beneath the overlay", key)
		}
	}
}

// esc closes it, as the overlay's own footer promises.
func TestHelpOverlayEscCloses(t *testing.T) {
	m := laneModel(t,
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
	)
	m = pressKey(t, m, "?")
	m = pressKey(t, m, "esc")
	if m.help {
		t.Fatal("esc did not close help")
	}
	// ? still toggles it closed too.
	m = pressKey(t, m, "?")
	m = pressKey(t, m, "?")
	if m.help {
		t.Fatal("? did not close help")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tui -run TestHelpOverlay -v`
Expected: `TestHelpOverlaySwallowsIssueKeys` FAILS on the first key that drives the screen. `TestHelpOverlayEscCloses` FAILS with "esc did not close help".

- [ ] **Step 3: Capture keys while help is up**

In `internal/tui/app.go`, in the `case tea.KeyMsg:` branch, replace:

```go
		key := msg.String()
		switch key {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "?":
			m.help = !m.help
			return m, nil
		}
```

with:

```go
		key := msg.String()
		if key == "q" || key == "ctrl+c" {
			return m, tea.Quit
		}
		// The help overlay is modal. It paints over the grid, so it has to
		// swallow the grid's keys — otherwise p pauses a lane and x arms a
		// kill confirm behind a screen the operator cannot see.
		if m.help {
			if key == "?" || key == "esc" {
				m.help = false
			}
			return m, nil
		}
		if key == "?" {
			m.help = true
			return m, nil
		}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/tui -run TestHelpOverlay -v`
Expected: PASS.

- [ ] **Step 5: Run the full suite**

Run: `go test ./...`
Expected: PASS. The `help` snapshot flow sets `m.help` directly and does not press keys, so its goldens should not move. If they do, stop and find out why.

- [ ] **Step 6: Commit**

```bash
git add internal/tui/app.go internal/tui/app_test.go
git commit -m "fix: help overlay captures keys instead of painting over a live grid"
```

---

### Task 8: `ParseLine` keeps tool calls

`rawLine.Message.Content` decodes only `Type` and `Text`. An assistant message consisting solely of `tool_use` blocks still matches the `r.Type == "assistant"` case, producing `KindAssistantText` with an empty `Text` — and `strings.Split("", "\n")` is `[""]`, so the runner writes a blank line to the transcript. During a tool-heavy stage the door fills with blanks while the agent is working hard.

**Files:**
- Modify: `internal/claude/stream.go:11-77`
- Test: `internal/claude/stream_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `const KindToolUse = "tool_use"`
  - `StreamEvent.Tools []string` — pre-formatted one-line summaries, each already prefixed with `↳ `.
  - `func toolSummary(name string, input json.RawMessage) string`

- [ ] **Step 1: Write the failing test**

Add to `internal/claude/stream_test.go`:

```go
func TestParseToolUse(t *testing.T) {
	ev := ParseLine([]byte(`{"type":"assistant","message":{"content":[` +
		`{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}},` +
		`{"type":"tool_use","name":"Read","input":{"file_path":"internal/engine/engine.go"}}]}}`))
	if ev.Kind != KindToolUse {
		t.Fatalf("kind = %q, want tool_use", ev.Kind)
	}
	if len(ev.Tools) != 2 {
		t.Fatalf("tools = %v", ev.Tools)
	}
	if ev.Tools[0] != "↳ Bash go test ./..." {
		t.Fatalf("tools[0] = %q", ev.Tools[0])
	}
	if ev.Tools[1] != "↳ Read internal/engine/engine.go" {
		t.Fatalf("tools[1] = %q", ev.Tools[1])
	}
}

// Text and tool blocks in one message must both survive, text first.
func TestParseMixedTextAndToolUse(t *testing.T) {
	ev := ParseLine([]byte(`{"type":"assistant","message":{"content":[` +
		`{"type":"text","text":"checking the tests"},` +
		`{"type":"tool_use","name":"Bash","input":{"command":"go vet ./..."}}]}}`))
	if ev.Kind != KindAssistantText {
		t.Fatalf("kind = %q, want assistant_text", ev.Kind)
	}
	if ev.Text != "checking the tests" {
		t.Fatalf("text = %q", ev.Text)
	}
	if len(ev.Tools) != 1 || ev.Tools[0] != "↳ Bash go vet ./..." {
		t.Fatalf("tools = %v", ev.Tools)
	}
}

// An unknown tool, or one whose input has no field we recognize, still names
// itself rather than dumping raw JSON at the operator.
func TestParseToolUseUnknownShape(t *testing.T) {
	ev := ParseLine([]byte(`{"type":"assistant","message":{"content":[` +
		`{"type":"tool_use","name":"mcp__thing__do","input":{"weird":{"nested":1}}}]}}`))
	if len(ev.Tools) != 1 || ev.Tools[0] != "↳ mcp__thing__do" {
		t.Fatalf("tools = %v", ev.Tools)
	}
}

// Malformed input must not panic or leak newlines into the transcript.
func TestParseToolUseMalformedInput(t *testing.T) {
	ev := ParseLine([]byte(`{"type":"assistant","message":{"content":[` +
		`{"type":"tool_use","name":"Bash","input":"not-an-object"}]}}`))
	if len(ev.Tools) != 1 || ev.Tools[0] != "↳ Bash" {
		t.Fatalf("tools = %v", ev.Tools)
	}
	long := ParseLine([]byte(`{"type":"assistant","message":{"content":[` +
		`{"type":"tool_use","name":"Bash","input":{"command":"echo ` + strings.Repeat("x", 400) + `"}}]}}`))
	if strings.Contains(long.Tools[0], "\n") {
		t.Fatal("summary leaked a newline")
	}
	if len([]rune(long.Tools[0])) > 120 {
		t.Fatalf("summary not truncated: %d runes", len([]rune(long.Tools[0])))
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/claude -run TestParseTool -v`
Expected: compile error — `KindToolUse` and `StreamEvent.Tools` are undefined.

- [ ] **Step 3: Widen the decoded content block**

In `internal/claude/stream.go`, add the constant to the `Kinds` block:

```go
	KindToolUse       = "tool_use"
```

Add the field to `StreamEvent`:

```go
	// Tools holds one-line summaries of the tool_use blocks in this message,
	// already prefixed. They are what an operator watching a stage sees while
	// the agent is working rather than talking.
	Tools []string
```

Widen the content struct inside `rawLine`:

```go
	Message   *struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage *usage `json:"usage"`
	} `json:"message"`
```

- [ ] **Step 4: Collect tool summaries in `ParseLine`**

Replace the assistant case in `ParseLine` with:

```go
	case r.Type == "assistant" && r.Message != nil:
		var parts []string
		var tools []string
		for _, c := range r.Message.Content {
			switch {
			case c.Type == "text" && c.Text != "":
				parts = append(parts, c.Text)
			case c.Type == "tool_use" && c.Name != "":
				tools = append(tools, "↳ "+toolSummary(c.Name, c.Input))
			}
		}
		text := strings.Join(parts, "\n")
		// A message with nothing but tool calls used to fall through as an
		// empty assistant_text, writing a blank line per tool call.
		if text == "" && len(tools) > 0 {
			return StreamEvent{Kind: KindToolUse, Tools: tools}
		}
		return StreamEvent{Kind: KindAssistantText, Text: text, Tools: tools}
```

- [ ] **Step 5: Add the summariser**

Below `ParseLine`, add:

```go
// toolSummary renders one tool_use block as a single transcript line: the tool
// name plus its most identifying argument. Unrecognized tools and unparseable
// inputs degrade to the bare name — raw JSON in the stream door is noise.
func toolSummary(name string, input json.RawMessage) string {
	const maxRunes = 120
	var fields map[string]json.RawMessage
	if len(input) == 0 || json.Unmarshal(input, &fields) != nil {
		return name
	}
	var arg string
	for _, key := range []string{"command", "file_path", "path", "pattern", "description", "query"} {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		if json.Unmarshal(raw, &arg) == nil && arg != "" {
			break
		}
		arg = ""
	}
	if arg == "" {
		return name
	}
	arg = strings.Join(strings.Fields(arg), " ")
	out := name + " " + arg
	if runes := []rune(out); len(runes) > maxRunes {
		out = string(runes[:maxRunes-1]) + "…"
	}
	return out
}
```

`strings.Fields` collapses every run of whitespace — including newlines and tabs — into single spaces, which is what keeps a multi-line heredoc from breaking the door's layout.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/claude -v`
Expected: PASS, including the pre-existing `TestParseInitAssistantResult`.

- [ ] **Step 7: Commit**

```bash
git add internal/claude/stream.go internal/claude/stream_test.go
git commit -m "feat: parse tool_use blocks into one-line stream summaries"
```

---

### Task 9: The runner writes tool lines to the transcript

`CodeRunner` forwards only `ev.Text` to `OnLine`, and does it unconditionally — so an empty text yields a blank transcript line. It must emit prose lines when there are any, then the tool summaries, in that order.

**Files:**
- Modify: `internal/claude/runner.go:96-145` (the scan loop)
- Create: `internal/claude/testdata/tools.sh`
- Test: `internal/claude/runner_test.go`

**Interfaces:**
- Consumes: `StreamEvent.Tools`, `KindToolUse` (Task 8).
- Produces: nothing new; `OnLine` keeps its `func(issueID, stage, line string)` signature.

The existing tests drive a real subprocess: `run(t, bin, dir)` builds a `CodeRunner` around a stub shell script in `testdata/` and returns its result channel. This task needs a runner with `OnLine` set, so it adds a sibling helper rather than changing `run`.

- [ ] **Step 1: Add the stub script**

Create `internal/claude/testdata/tools.sh`, modeled on `testdata/happy.sh`:

```sh
#!/bin/sh
# Prose, then a tool-only message, then a result. The tool-only message is the
# case that used to write a blank transcript line.
cat > /dev/null &
echo '{"type":"system","subtype":"init","session_id":"s-tools"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"looking at the engine"}]}}'
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}'
echo '{"type":"result","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}'
```

Make it executable — the other stubs are, and `exec` will fail otherwise:

```bash
chmod +x internal/claude/testdata/tools.sh
```

- [ ] **Step 2: Write the failing test**

Add to `internal/claude/runner_test.go`:

```go
// runLines is run() with OnLine wired, for tests that assert on transcript
// output rather than on the result.
func runLines(t *testing.T, bin, dir string) ([]string, runner.Result) {
	t.Helper()
	var lines []string
	c := &CodeRunner{Bin: bin, Packages: testPkgs(), OnLine: func(_, _, line string) {
		lines = append(lines, line)
	}}
	asks := make(chan runner.Ask, 1)
	res := <-c.Run(context.Background(), "GH-1", "spec", "spec-writer", dir, asks)
	return lines, res
}

// The operator watching a tool-heavy stage needs to see the tools. A tool-only
// message used to fall through as empty assistant text, writing a blank line —
// so the door filled with nothing while the agent worked.
func TestOnLineReceivesToolCalls(t *testing.T) {
	lines, res := runLines(t, abs(t, "testdata/tools.sh"), t.TempDir())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "looking at the engine") {
		t.Fatalf("prose missing: %q", joined)
	}
	if !strings.Contains(joined, "↳ Bash go test ./...") {
		t.Fatalf("tool line missing: %q", joined)
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			t.Fatalf("blank line written to transcript: %q", joined)
		}
	}
}
```

Add `"strings"` to the file's imports.

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/claude -run TestOnLineReceivesToolCalls -v`
Expected: FAIL with "tool line missing" (and likely also the blank-line assertion).

- [ ] **Step 4: Emit prose then tools**

In `internal/claude/runner.go`, replace the `KindAssistantText` block's `OnLine` call:

```go
		case KindAssistantText:
			if c.OnLine != nil {
				for _, line := range strings.Split(ev.Text, "\n") {
					c.OnLine(issueID, stage, line)
				}
			}
```

with a shared emit that also covers the new kind. Add above the `switch ev.Kind` statement:

```go
		// Prose first, then the tool calls it introduced: that is the order the
		// agent produced them, and a tool-only message must not write a blank.
		emit := func(ev StreamEvent) {
			if c.OnLine == nil {
				return
			}
			if ev.Text != "" {
				for _, line := range strings.Split(ev.Text, "\n") {
					c.OnLine(issueID, stage, line)
				}
			}
			for _, tool := range ev.Tools {
				c.OnLine(issueID, stage, tool)
			}
		}
```

and change the case to:

```go
		case KindAssistantText:
			emit(ev)
```

Add a new case immediately after it, before `case KindResult:`:

```go
		case KindToolUse:
			emit(ev)
```

Leave the decision- and proposal-extraction logic in the `KindAssistantText` case untouched — a tool-only message carries no markers.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/claude -v`
Expected: PASS.

- [ ] **Step 6: Run the full suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/claude/runner.go internal/claude/runner_test.go internal/claude/testdata/tools.sh
git commit -m "feat: transcript records tool calls instead of blank lines"
```

---

### Task 10: The stream door wears the room's chrome

`renderTextDoor` (`doors.go:145`) dumps raw lines under a bare `panelTitle` — no border, no footer, no typography. It is the only surface in the TUI that does not wear the chrome the rest of the room uses. The transcript gets its own renderer built on `renderBox`, the same helper behind the help overlay and the new-issue modal. The timeline keeps `renderTextDoor` unchanged.

Lines arrive from `transcript.Buffer.Tail` formatted as `stage + " │ " + line`, so the gutter can be split off and dimmed.

**Files:**
- Modify: `internal/tui/doors.go` (add `renderStreamDoor`)
- Modify: `internal/tui/app.go:1150-1151` (the `"transcript"` view case)
- Modify: `internal/tui/fixtures.go:18-19` (`FixtureFlows`), `internal/tui/fixtures.go:106-136` (`FixtureModel`)
- Test: `internal/tui/doors_test.go`, `internal/tui/snapshot_test.go` (via goldens)

**Interfaces:**
- Consumes: `renderBox(title, sub, chipText, content string) string` from `internal/tui/modal.go:69`.
- Produces: `func renderStreamDoor(subtitle string, lines []string, width int) string`.

- [ ] **Step 1: Write the failing test**

Add to `internal/tui/doors_test.go`:

The other tests in this file pin the color profile to `termenv.Ascii` so styling drops out and the assertions read plain text. Do the same; no `ansi.Strip` is needed. Note the box title is lowercase `stream`, matching `renderBox("help", ...)`.

```go
// The stream door is the one reading surface an operator stares at while a
// stage runs; it wears the same chrome as every other box in the room.
func TestStreamDoorWearsBoxChrome(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	lines := []string{
		"brainstorm │ Requirements settled.",
		"brainstorm │ ↳ Bash go test ./...",
		"brainstorm │ — turn complete (13560 tokens) —",
	}
	got := renderStreamDoor("GH-2 · brainstorm", lines, 100)
	if !strings.Contains(got, "─") {
		t.Fatalf("no border: %q", got)
	}
	if !strings.Contains(got, "stream") {
		t.Fatalf("no title: %q", got)
	}
	if !strings.Contains(got, "GH-2 · brainstorm") {
		t.Fatalf("no subtitle: %q", got)
	}
	if !strings.Contains(got, "esc close") {
		t.Fatalf("no close chip: %q", got)
	}
	if !strings.Contains(got, "Requirements settled.") {
		t.Fatalf("body missing: %q", got)
	}
	if !strings.Contains(got, "↳ Bash go test ./...") {
		t.Fatalf("tool line missing: %q", got)
	}
	if strings.Contains(got, "turn complete") {
		t.Fatalf("turn marker should render as a rule, not prose: %q", got)
	}
}

// An empty buffer says so rather than rendering a hollow box.
func TestStreamDoorEmpty(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	got := renderStreamDoor("GH-2", nil, 100)
	if !strings.Contains(got, "nothing yet") {
		t.Fatalf("empty door = %q", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tui -run TestStreamDoor -v`
Expected: compile error — `renderStreamDoor` is undefined.

- [ ] **Step 3: Write the renderer**

Add to `internal/tui/doors.go`:

```go
// renderStreamDoor is the live agent view. Unlike the timeline it is watched
// while a stage runs, so it wears the same box chrome as the help overlay and
// gives its content typography: dim stage gutter, prose in Text, turn markers
// promoted from a line of prose into a rule.
func renderStreamDoor(subtitle string, lines []string, width int) string {
	t := activeTheme
	gutter := lipgloss.NewStyle().Foreground(t.Dimmer)
	prose := lipgloss.NewStyle().Foreground(t.Text)
	tool := lipgloss.NewStyle().Foreground(t.Dim)
	inner := max(20, width-8) // border, padding, and the gutter's own width

	var body []string
	if len(lines) == 0 {
		body = append(body, gutter.Render("nothing yet — the agent has not spoken this stage"))
	}
	for _, line := range lines {
		stage, text, found := strings.Cut(line, " │ ")
		if !found {
			stage, text = "", line
		}
		// The turn marker is punctuation, not something to read.
		if strings.HasPrefix(strings.TrimSpace(text), "— turn complete") {
			body = append(body, gutter.Render(strings.Repeat("─", inner)))
			continue
		}
		style := prose
		if strings.HasPrefix(text, "↳ ") {
			style = tool
		}
		lead := ""
		if stage != "" {
			lead = stage + " │ "
		}
		wrapWidth := max(1, inner-lipgloss.Width(lead))
		pad := strings.Repeat(" ", lipgloss.Width(lead))
		for i, wrapped := range strings.Split(ansi.Wrap(text, wrapWidth, ""), "\n") {
			marker := pad
			if i == 0 {
				marker = lead
			}
			body = append(body, gutter.Render(marker)+style.Render(wrapped))
		}
	}
	foot := keyChip("esc") + lipgloss.NewStyle().Foreground(t.Dim).Render(" close  ") +
		keyChip("q") + lipgloss.NewStyle().Foreground(t.Dim).Render(" quit")
	return renderBox("stream", subtitle, " esc close ", strings.Join(append(body, "", foot), "\n"))
}
```

Wrapping is done here rather than through `wrapIndent` (`rail.go:41`) because the gutter and the body need different styles, and `wrapIndent` returns one already-joined string per line with no seam to style across. Add `"github.com/charmbracelet/x/ansi"` to `doors.go`'s imports — `rail.go` in the same package already uses it.

- [ ] **Step 4: Point the view at it**

In `internal/tui/app.go`, replace:

```go
	case "transcript":
		tower = renderTextDoor("TRANSCRIPT", m.doorLines, layoutWidth)
```

with:

```go
	case "transcript":
		tower = renderStreamDoor(m.streamSubtitle(), m.doorLines, layoutWidth)
```

and add near `fetchTranscript`:

```go
// streamSubtitle names the lane and the stage whose output is on screen. The
// stage comes from the newest line's gutter, since the buffer spans stages.
func (m Model) streamSubtitle() string {
	tag := m.Focus.Issue
	if identity, ok := m.Ids[m.Focus.Issue]; ok && identity.Tag != "" {
		tag = identity.Tag + " " + m.Focus.Issue
	}
	for i := len(m.doorLines) - 1; i >= 0; i-- {
		if stage, _, found := strings.Cut(m.doorLines[i], " │ "); found && stage != "" {
			return tag + " · " + stage
		}
	}
	return tag
}
```

- [ ] **Step 5: Add a snapshot fixture**

In `internal/tui/fixtures.go`, add `"stream"` to the slice returned by `FixtureFlows`, and add a case to `FixtureModel`'s switch:

```go
	case "stream":
		m.modes = []string{"transcript"}
		m.doorLines = []string{
			"brainstorm │ Requirements settled. brainstorm.md is written to the issue directory.",
			"brainstorm │ ↳ Read internal/engine/engine.go",
			"brainstorm │ ↳ Bash go test ./internal/engine",
			"brainstorm │ The rename target is real: the remote is weston6142/watchtower and go.mod already agrees.",
			"brainstorm │ — turn complete (13560 tokens) —",
		}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/tui -run TestStreamDoor -v`
Expected: PASS.

- [ ] **Step 7: Generate the new goldens**

Run: `go test ./internal/tui -run TestSnapshots -update`
Then: `git diff internal/tui/testdata` and read `internal/tui/testdata/stream-wide.golden` in full.

Confirm by eye: the box has a border, the title band reads `stream  GH-2 · brainstorm` (or the fixture's focused lane), the `esc close` chip sits at the right of the band, the gutter is visibly dimmer than the prose, the tool lines are dimmer than the prose, and the turn marker is a rule rather than a sentence. Confirm nothing else in `testdata` moved.

- [ ] **Step 8: Run the full suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 9: Rebuild and look at it**

Run:

```bash
go install ./cmd/watchtower
kill $(cat ~/.local/share/watchtower/repos/*/daemon.pid ~/.local/share/guildhall/repos/*/daemon.pid 2>/dev/null)
$(go env GOPATH)/bin/watchtower issues
```

The binary was renamed from `guildhall` to `watchtower` on 2026-07-28; the old state directory may still exist and be migrated on startup, so the kill covers both paths. `$(go env GOPATH)/bin` is not on a plain shell's `PATH` — call the binary by full path or add it.

Then open the TUI, focus a lane, and press `T`. The daemon must be restarted or clients keep talking to the old binary. Confirm the door matches the golden and that `esc` returns to the grid.

- [ ] **Step 10: Commit**

```bash
git add internal/tui/doors.go internal/tui/app.go internal/tui/fixtures.go internal/tui/doors_test.go internal/tui/testdata
git commit -m "feat: stream door wears box chrome and reads as prose plus activity"
```

---

## Verification

After Task 10, verify the whole spec end to end:

- [ ] `go test ./...` passes.
- [ ] `go vet ./...` is clean.
- [ ] Rebuild and restart per Task 10 Step 9.
- [ ] Kill a running stage with `x`, then press `p`. The stage restarts. (Part 1)
- [ ] With a lane parked at a gate, confirm the grid ticks its finished stages and marks only the parked one, and the notice row names it. (Parts 2, 3)
- [ ] Press `?`, then `p`. Nothing happens beneath the overlay. Press `esc`. It closes. (Part 4)
- [ ] Press `T` during an `execute` stage. Tool lines appear as the agent works. (Part 3 of the spec)
