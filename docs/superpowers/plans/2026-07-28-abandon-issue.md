# Abandon Issue + Kill Guard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A durable "abandon issue" op that removes a lane everywhere (engine, store state, projection, overview, TUI) and survives restarts, plus a TUI guard so `x` (kill) never round-trips to the daemon when nothing is running.

**Architecture:** New `EvIssueAbandoned` core event flows through the existing pipeline: engine emits it (after cancelling any running stage via the KillStage mechanics), steward persists state `abandoned`, projection deletes the lane, Rehydrate treats `abandoned` as terminal. Proto gains op `abandon_issue`, the CLI a positional `abandon` verb, and the TUI an `X` key using the existing confirm box (generalized to carry an op).

**Tech Stack:** Go; existing test patterns (engine FakeRunner tests, projection/tui table tests, golden snapshots).

**Spec:** `docs/2026-07-28-abandon-issue-design.md`.

## Global Constraints

- Worktree: `/Users/weston.bushyeager/.treehouse/guildhall-2253b2/1/guildhall`. First merge `weston/daemon-rehydration` → `develop` (tests green already), delete it, then branch `weston/abandon-issue` off `develop`.
- Every task ends with `go test ./...` green; golden snapshots (`go test ./internal/tui -run TestSnapshots`) must pass WITHOUT `-update` unless a fixture visibly changes (only Task 5's help overlay changes goldens — regenerate intentionally there).
- Abandon is a state, not a purge: no DB row deletion, no artifact/worktree cleanup.
- Commit at the end of each task with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.

---

### Task 0: Branch setup

- [ ] From the main repo (`/Users/weston.bushyeager/guildhall`): `git checkout develop && git merge weston/daemon-rehydration --no-edit && go test ./... && git branch -d weston/daemon-rehydration`
- [ ] In the worktree: `git checkout -b weston/abandon-issue develop`

---

### Task 1: Core event + engine Abandon (TDD)

**Files:**
- Modify: `internal/core/event.go` (event type list, ~line 30)
- Modify: `internal/engine/engine.go`
- Test: `internal/engine/engine_test.go`

**Interfaces:**
- Produces: `core.EvIssueAbandoned EventType = "issue_abandoned"`.
- Produces: `func (e *Engine) Abandon(issueID string) error` — later tasks (proto, TUI) call this via op `abandon_issue`.

- [ ] **Step 1: Write failing tests** (append to `engine_test.go`; reuse `newEngineOnFile`, `scripts()`, `testFlow()` helpers):

```go
func TestAbandonRehydratedIssue(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "gh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	dataDir := t.TempDir()
	e1 := newEngineOnFile(t, s, &runner.FakeRunner{Scripts: scripts()}, dataDir)
	id, err := e1.CreateIssue("doomed", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)
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
	// Gone from memory: issue ops now fail.
	if err := e2.RetryStage(context.Background(), id); err == nil || !strings.Contains(err.Error(), "unknown issue") {
		t.Fatalf("expected unknown issue after abandon, got %v", err)
	}
	// Event emitted.
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
	// Steward is wired in Task 2; here assert only engine behavior:
	// a third engine's Rehydrate must not resurrect the lane once the
	// stored state is "abandoned". Simulate steward until Task 2:
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
	id, err := e.CreateIssue("live", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)
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
	<-errc // stage goroutine unblocks (closed reply channel / cancelled ctx)
	if ds := e.PendingDecisions(); len(ds) != 0 {
		t.Fatalf("pending decisions survived abandon: %d", len(ds))
	}
	rows, _ := s.AllDecisionRows()
	for _, row := range rows {
		if row.IssueID == id && row.Status == "pending" {
			t.Fatal("decision row left pending after abandon")
		}
	}
}

func TestAbandonUnknownIssue(t *testing.T) {
	e, _ := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	if err := e.Abandon("GH-404"); err == nil {
		t.Fatal("expected error for unknown issue")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/engine -run TestAbandon`
Expected: FAIL (compile: `core.EvIssueAbandoned` and `e.Abandon` undefined).

- [ ] **Step 3: Add the event type**

In `internal/core/event.go`, alongside `EvIssuePaused`/`EvIssueResumed`:

```go
EvIssueAbandoned EventType = "issue_abandoned"
```

- [ ] **Step 4: Implement `Abandon` in `engine.go`** (place after `KillStage`; mirror its lock/cancel discipline):

```go
// Abandon removes an issue for good: any running stage is cancelled, its
// pending decisions are closed, and the lane disappears from every surface
// via EvIssueAbandoned. Abandon is a state, not a purge — rows and
// artifacts stay in the store.
func (e *Engine) Abandon(issueID string) error {
	e.mu.Lock()
	is, ok := e.issues[issueID]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("unknown issue %s", issueID)
	}
	is.killRequested = true
	cancel := is.stageCancel
	var killed []int64
	for id, p := range e.pend {
		if p.IssueID != issueID {
			continue
		}
		delete(e.pend, id)
		close(p.reply)
		killed = append(killed, id)
	}
	delete(e.issues, issueID)
	e.mu.Unlock()
	for _, id := range killed {
		_ = e.cfg.Store.AnswerDecision(id, -1, "killed")
	}
	if cancel != nil {
		cancel()
	}
	e.emit(core.EvIssueAbandoned, issueID, map[string]any{})
	return nil
}
```

- [ ] **Step 5: Make Rehydrate treat `abandoned` as terminal** — in the state filter inside `Rehydrate` (engine.go:~152):

```go
if row.State == "done" || row.State == "done (unmerged)" || row.State == "merged" || row.State == "abandoned" {
```

- [ ] **Step 6: Run tests**

Run: `go test ./internal/engine`
Expected: PASS (all, including existing rehydrate tests).

- [ ] **Step 7: Commit**

```bash
git add internal/core internal/engine
git commit -m "feat: engine Abandon — cancel, close decisions, emit issue_abandoned"
```

---

### Task 2: Steward + overview

**Files:**
- Modify: `internal/steward/steward.go` (Observe switch)
- Modify: `internal/proto/server.go` (`overview()`, ~line 256 latest-event switch)
- Test: `internal/steward/steward_test.go` (follow existing pattern if present; otherwise add)

**Interfaces:**
- Consumes: `core.EvIssueAbandoned`.
- Produces: durable issue state string `"abandoned"`.

- [ ] **Step 1: Failing test** — in the steward test file, apply an `issue_created` then `issue_abandoned` event via `Observe` and assert `store.Issues()` shows state `abandoned`. If no steward test file exists, create `internal/steward/steward_test.go` with an in-memory store (`store.Open("file:steward_test?mode=memory&cache=shared")`).

- [ ] **Step 2: Implement** — in the `Observe` switch:

```go
case core.EvIssueAbandoned:
	setState("abandoned")
```

- [ ] **Step 3: Overview counts** — in `overview()`'s latest-event switch add `core.EvIssueAbandoned` to the skip cases:

```go
case core.EvDecisionRequired, core.EvIssueCompleted, core.EvIssueMerged, core.EvIssueAbandoned:
	continue
```

(The state-prefix switch below already ignores `"abandoned"` — it matches no prefix case.)

- [ ] **Step 4: Tests + commit**

Run: `go test ./internal/steward ./internal/proto`
Expected: PASS.

```bash
git add internal/steward internal/proto
git commit -m "feat: steward persists abandoned state; overview skips abandoned lanes"
```

---

### Task 3: Projection removal (TDD)

**Files:**
- Modify: `internal/projection/projection.go` (Apply switch)
- Test: `internal/projection/projection_test.go`

**Interfaces:**
- Consumes: `core.EvIssueAbandoned`. Produces: lane fully removed from `Issues`, `Order`, `ShippedToday`, `Parked`, `Decisions`.

- [ ] **Step 1: Failing test** (follow existing projection test style):

```go
func TestAbandonRemovesLaneEverywhere(t *testing.T) {
	s := NewState()
	apply := func(typ core.EventType, payload any) {
		ev, err := core.NewEvent(typ, "GH-1", payload)
		if err != nil {
			t.Fatal(err)
		}
		s.Apply(ev)
	}
	apply(core.EvIssueCreated, map[string]any{"title": "doomed", "flow": "default"})
	apply(core.EvStageStarted, map[string]any{"stage": "spec"})
	apply(core.EvDecisionRequired, map[string]any{
		"decision_id": float64(3), "stage": "spec", "question": "q",
		"options": []any{"a"}, "recommended": float64(0)})
	apply(core.EvStageFailed, map[string]any{"stage": "spec", "error": "boom", "final": true})
	apply(core.EvIssueAbandoned, map[string]any{})
	if s.Issues["GH-1"] != nil {
		t.Fatal("issue survived abandon")
	}
	if len(s.Order) != 0 || len(s.Parked) != 0 || len(s.ShippedToday) != 0 {
		t.Fatalf("lane lists not cleaned: order=%v parked=%v shipped=%v", s.Order, s.Parked, s.ShippedToday)
	}
	if len(s.Decisions) != 0 {
		t.Fatalf("decisions survived abandon: %v", s.Decisions)
	}
}
```

- [ ] **Step 2: Verify failure** — `go test ./internal/projection -run TestAbandon` → FAIL (compile or assertion).

- [ ] **Step 3: Implement** in `Apply`'s switch:

```go
case core.EvIssueAbandoned:
	delete(s.Issues, ev.IssueID)
	s.Order = removeString(s.Order, ev.IssueID)
	s.ShippedToday = removeString(s.ShippedToday, ev.IssueID)
	s.Parked = removeString(s.Parked, ev.IssueID)
	for id, d := range s.Decisions {
		if d.IssueID == ev.IssueID {
			delete(s.Decisions, id)
		}
	}
```

with a small helper next to `appendUnique`:

```go
func removeString(list []string, v string) []string {
	out := list[:0]
	for _, s := range list {
		if s != v {
			out = append(out, s)
		}
	}
	return out
}
```

- [ ] **Step 4: Tests + commit**

Run: `go test ./internal/projection` → PASS.

```bash
git add internal/projection
git commit -m "feat: projection drops abandoned lanes from every surface"
```

---

### Task 4: Proto op + CLI verb

**Files:**
- Modify: `internal/proto/server.go` (op switch, after `"retry_stage"` ~line 141)
- Modify: `cmd/guildhall/main.go` (the `case "pause", "resume", "kill", "retry":` block, ~line 220, and the usage string ~line 44)

**Interfaces:**
- Produces: op `"abandon_issue"` (synchronous, errors surfaced) and CLI `guildhall abandon <issue-id>`.

- [ ] **Step 1: Server op** (synchronous like kill, not fire-and-forget like retry):

```go
case "abandon_issue":
	if err := sv.eng.Abandon(cmd.IssueID); err != nil {
		return Response{Error: err.Error()}
	}
	return Response{OK: true, IssueID: cmd.IssueID}
```

- [ ] **Step 2: CLI verb** — extend the shared case to `case "pause", "resume", "kill", "retry", "abandon":` and the ops map with `"abandon": "abandon_issue"`; add `abandon` to the top-level usage string (main.go:~44).

- [ ] **Step 3: Verify + commit**

Run: `go test ./... ` → PASS. Manual sanity: `go build ./cmd/guildhall`.

```bash
git add internal/proto cmd/guildhall
git commit -m "feat: abandon_issue op and guildhall abandon verb"
```

---

### Task 5: TUI — kill guard + X abandon (TDD)

**Files:**
- Modify: `internal/tui/app.go` (confirmState ~line 116, confirm `y` handler ~line 356-365, `x` handler ~line 497-505, new `X` handler beside it, help groups in `internal/tui/render.go` CONTROL list)
- Test: `internal/tui/app_test.go`
- Goldens: help overlay fixtures change (new `X abandon lane` row) — regenerate intentionally.

**Interfaces:**
- Consumes: op `"abandon_issue"` via the existing `m.issueCommand(issueID, op)`.
- Produces: `confirmState{IssueID, Prompt, Op}` — Op replaces the hardcoded `"kill_stage"`.

- [ ] **Step 1: Failing tests** (append to `app_test.go`; reuse `pressKey` and `mkev`):

```go
func TestKillGuardOnIdleLane(t *testing.T) {
	m := NewModel(nil, []string{"brainstorm", "spec", "execute", "review", "merge"})
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "dead lane", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
		mkev(t, core.EvStageFailed, "GH-1", map[string]any{"stage": "brainstorm", "error": "boom", "final": true}),
	})
	m = pressKey(t, m, "j") // focus the lane
	if m.Focus.Issue != "GH-1" {
		t.Fatalf("focus: %q", m.Focus.Issue)
	}
	m = pressKey(t, m, "x")
	if m.confirm != nil {
		t.Fatal("kill confirm opened for idle lane")
	}
	if m.Err != "nothing running — R retries · X abandons" {
		t.Fatalf("expected kill hint, got %q", m.Err)
	}
}

func TestKillStillConfirmsOnRunningLane(t *testing.T) {
	m := NewModel(nil, []string{"brainstorm", "spec", "execute", "review", "merge"})
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "busy lane", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
	})
	m = pressKey(t, m, "j")
	m = pressKey(t, m, "x")
	if m.confirm == nil || m.confirm.Op != "kill_stage" {
		t.Fatalf("expected kill confirm, got %+v", m.confirm)
	}
}

func TestAbandonConfirmSendsOp(t *testing.T) {
	m := NewModel(nil, []string{"brainstorm", "spec", "execute", "review", "merge"})
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "dead lane", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
		mkev(t, core.EvStageFailed, "GH-1", map[string]any{"stage": "brainstorm", "error": "boom", "final": true}),
	})
	m = pressKey(t, m, "j")
	m = pressKey(t, m, "X")
	if m.confirm == nil || m.confirm.Op != "abandon_issue" {
		t.Fatalf("expected abandon confirm, got %+v", m.confirm)
	}
	if !strings.Contains(m.confirm.Prompt, "abandon") {
		t.Fatalf("prompt: %q", m.confirm.Prompt)
	}
	// y sends the op (issueCommand returns a non-nil cmd; client is nil so
	// just assert the confirm cleared and no panic).
	m = pressKey(t, m, "y")
	if m.confirm != nil {
		t.Fatal("confirm not cleared after y")
	}
}
```

(`strings` may need importing in app_test.go.)

- [ ] **Step 2: Verify failure** — `go test ./internal/tui -run 'TestKillGuard|TestKillStill|TestAbandonConfirm'` → FAIL.

- [ ] **Step 3: Implement**

1. `confirmState` gains `Op string`; the `y` handler uses `m.issueCommand(issueID, op)` with the stored op (default nothing hardcoded).
2. `x` handler becomes:

```go
case "x":
	if iv := m.State.Issues[m.Focus.Issue]; iv != nil {
		if iv.State != "running" && iv.State != "waiting_decision" {
			m.Err = "nothing running — R retries · X abandons"
			return m, nil
		}
		stage := iv.CurrentStage
		if stage == "" {
			stage = "current"
		}
		m.confirm = &confirmState{IssueID: iv.ID, Op: "kill_stage",
			Prompt: fmt.Sprintf("kill the running %s stage of %s? y/n", stage, iv.Title)}
		return m, nil
	}
```

3. New `case "X":` beside it:

```go
case "X":
	if iv := m.State.Issues[m.Focus.Issue]; iv != nil {
		m.confirm = &confirmState{IssueID: iv.ID, Op: "abandon_issue",
			Prompt: fmt.Sprintf("abandon %s? the lane is removed for good. y/n", iv.Title)}
		return m, nil
	}
```

4. Add `X` to the no-focus hint key list (`"p", "x", "X", "R", "L", "c", "o", "enter"`).
5. Help overlay: in `helpGroups` CONTROL rows (render.go), add `{"X", "abandon lane"}` after the kill row; keybar footer stays as-is (already crowded — help documents it).

- [ ] **Step 4: Tests + goldens**

Run: `go test ./internal/tui` — only help-overlay snapshot tests should drift; regenerate: `go test ./internal/tui -run TestSnapshots -update`, re-run full package, then `scripts/snap.sh help` and **Read the PNG** to check the new row.

- [ ] **Step 5: Full suite + commit**

Run: `go test ./...` → PASS.

```bash
git add internal/tui
git commit -m "feat: kill guard on idle lanes; X abandons a lane with confirm"
```

---

### Task 6: Live verification + install

- [ ] `go install ./cmd/guildhall`; `kill $(cat ~/.local/share/guildhall/repos/*/daemon.pid 2>/dev/null)`; from the main repo run `guildhall issues` (respawns daemon).
- [ ] `guildhall issues` shows GH-1 `failed`. Run `guildhall abandon GH-1`; then `guildhall issues` no longer lists it as active (state `abandoned`) and the overview drops the failing count (`guildhall status`).
- [ ] `scripts/tui-capture.sh floor` → **Read the PNG**: no GH-1 lane, no parked entry. (Do this only if the user confirms abandoning the real GH-1 is desired — it was created as a test issue; ask first if unclear.)
- [ ] Report results to the user; then superpowers:finishing-a-development-branch for `weston/abandon-issue`.

## Verification (whole plan)

1. `go test ./...` green; goldens updated only for the help overlay.
2. Engine: abandon works on rehydrated, running, and unknown issues per tests.
3. Live: abandoned lane vanishes from tower/parked/overview and stays gone after another daemon restart (`kill` pidfile + `guildhall issues` twice).
