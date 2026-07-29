# Issue Backlog Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Issues can be captured as durable `backlog` drafts, then browsed, edited, launched, and deleted from the TUI and CLI.

**Architecture:** A draft is a normal row in the existing `issues` table with state `backlog`, flowing through the event log (`issue_drafted`, `issue_updated`). Launch emits the existing `issue_created` event with the draft's id and runs the normal CreateIssue start path. Delete reuses the existing abandon machinery. Spec: `docs/superpowers/specs/2026-07-28-issue-backlog-design.md`.

**Tech Stack:** Go, SQLite (modernc.org/sqlite), Bubble Tea + lipgloss TUI, Unix-socket JSONL proto.

## Global Constraints

- Module path is `github.com/weston6142/watchtower` (NOT guildhall) — check `go.mod` before writing imports.
- Default branch is `develop`. Binary/CLI name is `watchtower` (in `cmd/watchtower/`).
- The TUI is a pure client: renders projected state, sends ops, never owns state.
- Rehydrate must treat `backlog` as inert: never auto-started, never marked failed.
- Backlog issues count nowhere in overview failing/building/queued counts.
- No hard delete: removal is `abandon_issue`.
- Run `go test ./...` after each task; goldens live in `internal/tui/testdata/`.

---

### Task 1: Engine — DraftIssue, UpdateIssue, and backlog rehydrate

**Files:**
- Modify: `internal/core/event.go` (add two event consts)
- Modify: `internal/engine/engine.go`
- Test: `internal/engine/engine_backlog_test.go` (new; copy test harness setup from existing `internal/engine/engine_test.go` — use the same fake store/flows helpers it uses)

**Interfaces:**
- Produces: `core.EvIssueDrafted` (= `"issue_drafted"`), `core.EvIssueUpdated` (= `"issue_updated"`).
- Produces: `func (e *Engine) DraftIssue(title, body, flowName, preset string, m levers.Matrix, priority int) (string, error)`
- Produces: `func (e *Engine) UpdateIssue(id, title, body, flowName, preset string, m levers.Matrix, priority int) error`
- Produces: `issueState.draft bool` field.
- Event payloads (both events): `{"title", "body", "flow", "preset": string, "priority": float64, "levers": map[string]string}` — levers included so the steward can persist them without clobbering (UpsertIssue replaces the whole row).

- [ ] **Step 1: Add the event types**

In `internal/core/event.go`, extend the const block:

```go
	EvIssueDrafted EventType = "issue_drafted"
	EvIssueUpdated EventType = "issue_updated"
```

- [ ] **Step 2: Write failing engine tests**

In `internal/engine/engine_backlog_test.go` (mirror the setup pattern of the existing engine tests — construct `Engine` via `New(Config{Store: ..., Flows: ...})` with a real temp-dir store the way `engine_test.go` does):

```go
func TestDraftIssueStaysInBacklog(t *testing.T) {
	e, st := newTestEngine(t) // reuse/extract the helper the existing tests use
	id, err := e.DraftIssue("t", "b", "default", "regular", levers.Matrix{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	row := issueRow(t, st, id)
	if row.State != "backlog" {
		t.Fatalf("state = %q, want backlog", row.State)
	}
	if row.Priority != 2 || row.Title != "t" {
		t.Fatalf("row fields not persisted: %+v", row)
	}
	if !hasEvent(t, st, id, core.EvIssueDrafted) {
		t.Fatal("no issue_drafted event")
	}
	// No stage ran.
	runs, _ := st.StageRuns(id)
	if len(runs) != 0 {
		t.Fatalf("draft ran %d stages", len(runs))
	}
}

func TestUpdateIssueOnlyLegalFromBacklog(t *testing.T) {
	e, st := newTestEngine(t)
	id, _ := e.DraftIssue("t", "b", "default", "regular", levers.Matrix{}, 0)
	if err := e.UpdateIssue(id, "t2", "b2", "default", "strict", levers.Matrix{}, 5); err != nil {
		t.Fatal(err)
	}
	row := issueRow(t, st, id)
	if row.Title != "t2" || row.Priority != 5 || row.State != "backlog" {
		t.Fatalf("update not persisted: %+v", row)
	}
	if !hasEvent(t, st, id, core.EvIssueUpdated) {
		t.Fatal("no issue_updated event")
	}
	// A running (non-draft) issue refuses updates.
	rid, _ := e.CreateIssue("r", "", "default", levers.Matrix{}, 0)
	if err := e.UpdateIssue(rid, "x", "", "default", "regular", levers.Matrix{}, 0); err == nil {
		t.Fatal("update of non-draft succeeded")
	}
	if err := e.UpdateIssue("GH-999", "x", "", "default", "regular", levers.Matrix{}, 0); err == nil {
		t.Fatal("update of unknown issue succeeded")
	}
}

func TestAbandonDraft(t *testing.T) {
	e, st := newTestEngine(t)
	id, _ := e.DraftIssue("t", "b", "default", "regular", levers.Matrix{}, 0)
	if err := e.Abandon(id); err != nil {
		t.Fatal(err)
	}
	if issueRow(t, st, id).State != "abandoned" {
		t.Fatal("draft not abandoned")
	} // requires steward observing; if the engine test harness has no steward, assert the event instead:
	if !hasEvent(t, st, id, core.EvIssueAbandoned) {
		t.Fatal("no issue_abandoned event")
	}
}

func TestRehydrateKeepsDraftsInert(t *testing.T) {
	e, st := newTestEngine(t)
	id, _ := e.DraftIssue("t", "b", "default", "regular", levers.Matrix{"impl": "yolo"}, 3)
	// Simulate restart: fresh engine over the same store.
	e2 := newEngineOver(t, st)
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	// No stage_failed was emitted for the draft.
	if hasEvent(t, st, id, core.EvStageFailed) {
		t.Fatal("rehydrate marked draft failed")
	}
	// The draft is editable after restart (proves it was rebuilt in memory).
	if err := e2.UpdateIssue(id, "t2", "b", "default", "regular", levers.Matrix{}, 3); err != nil {
		t.Fatal(err)
	}
	// And the id counter accounts for it: next create must not collide.
	nid, _ := e2.CreateIssue("n", "", "default", levers.Matrix{}, 0)
	if nid == id {
		t.Fatal("id collision after rehydrate")
	}
}
```

Write small local helpers `issueRow(t, st, id)` (linear scan of `st.Issues()`) and `hasEvent(t, st, issueID, typ)` (scan `st.EventsSince(0)`), plus `newTestEngine`/`newEngineOver` if the existing tests don't already export equivalents — copy their construction code rather than inventing new config.

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/engine/ -run 'Draft|UpdateIssue|AbandonDraft|RehydrateKeepsDrafts' -v`
Expected: FAIL — `e.DraftIssue undefined`, `e.UpdateIssue undefined`.

- [ ] **Step 4: Implement**

In `internal/engine/engine.go`:

Add to `issueState`:

```go
	draft bool
```

Add after `CreateIssue`:

```go
// DraftIssue records an issue in the backlog without starting anything: no
// flow run, no slot. The draft is durable and editable until launched.
func (e *Engine) DraftIssue(title, body, flowName, preset string, m levers.Matrix, priority int) (string, error) {
	if _, ok := e.cfg.Flows[flowName]; !ok {
		return "", fmt.Errorf("unknown flow %q", flowName)
	}
	e.mu.Lock()
	e.nextID++
	id := fmt.Sprintf("GH-%d", e.nextID)
	e.issues[id] = &issueState{id: id, title: title, body: body, flowName: flowName, matrix: m, priority: priority, draft: true}
	e.mu.Unlock()
	if err := e.cfg.Store.UpsertIssue(store.IssueRow{
		ID: id, Title: title, Body: body, Flow: flowName, State: "backlog", Levers: matrixStrings(m), Priority: priority,
	}); err != nil {
		return "", err
	}
	e.emit(core.EvIssueDrafted, id, map[string]any{
		"title": title, "body": body, "flow": flowName, "preset": preset,
		"priority": priority, "levers": matrixStrings(m)})
	return id, nil
}

// UpdateIssue rewrites a draft's fields. Only legal while the issue is a
// backlog draft; launched issues are immutable through this path.
func (e *Engine) UpdateIssue(id, title, body, flowName, preset string, m levers.Matrix, priority int) error {
	if _, ok := e.cfg.Flows[flowName]; !ok {
		return fmt.Errorf("unknown flow %q", flowName)
	}
	e.mu.Lock()
	is, ok := e.issues[id]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("unknown issue %s", id)
	}
	if !is.draft {
		e.mu.Unlock()
		return fmt.Errorf("issue %s is not in the backlog", id)
	}
	is.title, is.body, is.flowName, is.matrix, is.priority = title, body, flowName, m, priority
	e.mu.Unlock()
	if err := e.cfg.Store.UpsertIssue(store.IssueRow{
		ID: id, Title: title, Body: body, Flow: flowName, State: "backlog", Levers: matrixStrings(m), Priority: priority,
	}); err != nil {
		return err
	}
	e.emit(core.EvIssueUpdated, id, map[string]any{
		"title": title, "body": body, "flow": flowName, "preset": preset,
		"priority": priority, "levers": matrixStrings(m)})
	return nil
}
```

In `Rehydrate`, inside the `for _, row := range rows` loop, immediately after the terminal-state `continue` check, add a backlog branch (before the `LastStageEvents`/flow lookup logic):

```go
		if row.State == "backlog" {
			e.mu.Lock()
			if _, known := e.issues[row.ID]; !known {
				matrix := levers.Matrix{}
				for st, lv := range row.Levers {
					matrix[st] = flow.Lever(lv)
				}
				e.issues[row.ID] = &issueState{
					id: row.ID, title: row.Title, body: row.Body, flowName: row.Flow,
					matrix: matrix, priority: row.Priority, draft: true,
				}
			}
			e.mu.Unlock()
			continue
		}
```

`Abandon` already handles drafts correctly (`stageCancel` is nil, no pending decisions) — no change needed there.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/engine/ ./internal/core/ -v -run 'Draft|UpdateIssue|AbandonDraft|Rehydrate'`
Expected: PASS (including pre-existing Rehydrate tests — the new branch must not break them).

- [ ] **Step 6: Commit**

```bash
git add internal/core/event.go internal/engine/engine.go internal/engine/engine_backlog_test.go
git commit -m "feat: engine drafts issues into a durable backlog"
```

---

### Task 2: Engine — LaunchIssue

**Files:**
- Modify: `internal/engine/engine.go`
- Test: `internal/engine/engine_backlog_test.go`

**Interfaces:**
- Consumes: `issueState.draft` from Task 1.
- Produces: `func (e *Engine) LaunchIssue(id string) error` — validates synchronously, then runs the flow detached (like `Resume`), emitting the existing `core.EvIssueCreated` with the draft's fields so downstream consumers treat it like any created issue.

- [ ] **Step 1: Write failing tests**

Append to `internal/engine/engine_backlog_test.go`:

```go
func TestLaunchIssueRunsDraft(t *testing.T) {
	e, st := newTestEngine(t) // harness whose flow/runner completes (same as CreateIssue happy-path tests)
	id, _ := e.DraftIssue("t", "b", "default", "regular", levers.Matrix{}, 1)
	if err := e.LaunchIssue(id); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, st, id, core.EvIssueCompleted) // poll EventsSince(0) with timeout, like existing async engine tests
	if !hasEvent(t, st, id, core.EvIssueCreated) {
		t.Fatal("launch did not emit issue_created")
	}
	// A launched issue is no longer editable.
	if err := e.UpdateIssue(id, "x", "", "default", "regular", levers.Matrix{}, 0); err == nil {
		t.Fatal("update after launch succeeded")
	}
}

func TestLaunchIssueRejectsNonDrafts(t *testing.T) {
	e, _ := newTestEngine(t)
	if err := e.LaunchIssue("GH-999"); err == nil {
		t.Fatal("launched unknown issue")
	}
	rid, _ := e.CreateIssue("r", "", "default", levers.Matrix{}, 0)
	if err := e.LaunchIssue(rid); err == nil {
		t.Fatal("launched a non-draft issue")
	}
}
```

`waitForEvent`: loop up to ~2s polling `st.EventsSince(0)` for the type, `t.Fatal` on timeout — copy the polling idiom already used by async engine tests if one exists; otherwise write it exactly so.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/engine/ -run Launch -v`
Expected: FAIL — `e.LaunchIssue undefined`.

- [ ] **Step 3: Implement**

In `internal/engine/engine.go`, after `UpdateIssue`:

```go
// LaunchIssue promotes a backlog draft into a running lane: the row flips to
// running, the standard issue_created event fires (projection and steward
// already treat it as the start of a lane), and the flow runs detached like
// Resume — failures surface as stage_failed events, not in this response.
func (e *Engine) LaunchIssue(id string) error {
	e.mu.Lock()
	is, ok := e.issues[id]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("unknown issue %s", id)
	}
	if !is.draft {
		e.mu.Unlock()
		return fmt.Errorf("issue %s is not in the backlog", id)
	}
	if _, ok := e.cfg.Flows[is.flowName]; !ok {
		e.mu.Unlock()
		return fmt.Errorf("unknown flow %q", is.flowName)
	}
	is.draft = false
	title, body, flowName, matrix, priority := is.title, is.body, is.flowName, is.matrix, is.priority
	e.mu.Unlock()
	if err := e.cfg.Store.UpsertIssue(store.IssueRow{
		ID: id, Title: title, Body: body, Flow: flowName, State: "running", Levers: matrixStrings(matrix), Priority: priority,
	}); err != nil {
		e.mu.Lock()
		is.draft = true
		e.mu.Unlock()
		return err
	}
	e.emit(core.EvIssueCreated, id, map[string]any{
		"title": title, "flow": flowName, "body": body, "priority": priority})
	go e.runAndRecord(context.Background(), is, 0)
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/engine/ -v`
Expected: PASS (all engine tests).

- [ ] **Step 5: Commit**

```bash
git add internal/engine/engine.go internal/engine/engine_backlog_test.go
git commit -m "feat: launch promotes a backlog draft into a running lane"
```

---

### Task 3: Steward — durable backlog state

**Files:**
- Modify: `internal/steward/steward.go`
- Test: `internal/steward/steward_test.go`

**Interfaces:**
- Consumes: `core.EvIssueDrafted`, `core.EvIssueUpdated` and their payload shape from Task 1.

- [ ] **Step 1: Write failing tests**

Append to `internal/steward/steward_test.go`, following its existing style (it builds a `store.Store` in a temp dir and calls `Observe` with hand-built events):

```go
func TestObserveDraftAndUpdate(t *testing.T) {
	st := newTestStore(t) // reuse the existing test helper/pattern in this file
	sw := &Steward{Store: st}
	ev, _ := core.NewEvent(core.EvIssueDrafted, "GH-1", map[string]any{
		"title": "t", "body": "b", "flow": "default", "preset": "regular",
		"priority": 2, "levers": map[string]string{"impl": "yolo"}})
	sw.Observe(ev)
	row := findRow(t, st, "GH-1")
	if row.State != "backlog" || row.Title != "t" || row.Priority != 2 || row.Levers["impl"] != "yolo" {
		t.Fatalf("drafted row wrong: %+v", row)
	}
	ev2, _ := core.NewEvent(core.EvIssueUpdated, "GH-1", map[string]any{
		"title": "t2", "body": "b2", "flow": "default", "preset": "strict",
		"priority": 7, "levers": map[string]string{"impl": "strict"}})
	sw.Observe(ev2)
	row = findRow(t, st, "GH-1")
	if row.State != "backlog" || row.Title != "t2" || row.Priority != 7 || row.Levers["impl"] != "strict" {
		t.Fatalf("updated row wrong: %+v", row)
	}
}
```

(`findRow` = scan `st.Issues()`; add tiny helpers only if the file doesn't have equivalents.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/steward/ -run Draft -v`
Expected: FAIL — row state empty/row missing (no case handles the events).

- [ ] **Step 3: Implement**

In `internal/steward/steward.go`, the payload decoder is `map[string]any`; add a levers extractor and two cases. Inside `Observe`, alongside the existing `str` helper add:

```go
	leverMap := func() map[string]string {
		out := map[string]string{}
		if raw, ok := p["levers"].(map[string]any); ok {
			for k, v := range raw {
				if s, ok := v.(string); ok {
					out[k] = s
				}
			}
		}
		return out
	}
```

And in the switch:

```go
	case core.EvIssueDrafted, core.EvIssueUpdated:
		prio, _ := p["priority"].(float64)
		_ = st.Store.UpsertIssue(store.IssueRow{
			ID: ev.IssueID, Title: str("title"), Body: str("body"),
			Flow: str("flow"), State: "backlog", Priority: int(prio),
			Levers: leverMap()})
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/steward/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/steward/steward.go internal/steward/steward_test.go
git commit -m "feat: steward persists backlog drafts and edits"
```

---

### Task 4: Projection — Backlog list and draft views

**Files:**
- Modify: `internal/projection/projection.go`
- Test: `internal/projection/projection_test.go`

**Interfaces:**
- Produces: `State.Backlog []string` (draft ids, insertion order — the TUI sorts for display).
- Produces: `IssueView.Priority int`, `IssueView.Body string`, `IssueView.Preset string` (needed to pre-fill the edit modal and sort the backlog list).
- Draft views have `State: "backlog"` and are NOT in `State.Order`.

- [ ] **Step 1: Write failing tests**

Append to `internal/projection/projection_test.go` (its existing style builds events via `core.NewEvent` and calls `s.Apply`):

```go
func TestBacklogLifecycle(t *testing.T) {
	s := NewState()
	drafted, _ := core.NewEvent(core.EvIssueDrafted, "GH-1", map[string]any{
		"title": "t", "body": "b", "flow": "default", "preset": "regular", "priority": 2})
	s.Apply(drafted)
	iv := s.Issues["GH-1"]
	if iv == nil || iv.State != "backlog" || iv.Priority != 2 || iv.Body != "b" || iv.Preset != "regular" {
		t.Fatalf("draft view wrong: %+v", iv)
	}
	if len(s.Order) != 0 {
		t.Fatal("draft leaked into grid Order")
	}
	if len(s.Backlog) != 1 || s.Backlog[0] != "GH-1" {
		t.Fatalf("Backlog = %v", s.Backlog)
	}

	updated, _ := core.NewEvent(core.EvIssueUpdated, "GH-1", map[string]any{
		"title": "t2", "body": "b2", "flow": "default", "preset": "strict", "priority": 9})
	s.Apply(updated)
	iv = s.Issues["GH-1"]
	if iv.Title != "t2" || iv.Priority != 9 || iv.Preset != "strict" || iv.Body != "b2" {
		t.Fatalf("update not applied: %+v", iv)
	}

	created, _ := core.NewEvent(core.EvIssueCreated, "GH-1", map[string]any{
		"title": "t2", "flow": "default", "body": "b2", "priority": 9})
	s.Apply(created)
	if len(s.Backlog) != 0 {
		t.Fatal("launched draft still in Backlog")
	}
	if len(s.Order) != 1 || s.Issues["GH-1"].State != "running" {
		t.Fatal("launched draft not on the grid")
	}
}

func TestAbandonRemovesDraftFromBacklog(t *testing.T) {
	s := NewState()
	drafted, _ := core.NewEvent(core.EvIssueDrafted, "GH-1", map[string]any{"title": "t"})
	s.Apply(drafted)
	abandoned, _ := core.NewEvent(core.EvIssueAbandoned, "GH-1", nil)
	s.Apply(abandoned)
	if len(s.Backlog) != 0 || s.Issues["GH-1"] != nil {
		t.Fatal("abandoned draft still visible")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/projection/ -run Backlog -v`
Expected: FAIL — `s.Backlog undefined`, `iv.Priority undefined`.

- [ ] **Step 3: Implement**

In `internal/projection/projection.go`:

Add to `IssueView` (after `Tokens int`):

```go
	Priority int
	Body     string
	Preset   string
```

Add to `State` (after `Parked []string`):

```go
	Backlog []string
```

In `Apply`'s switch:

```go
	case core.EvIssueDrafted, core.EvIssueUpdated:
		view := s.Issues[ev.IssueID]
		if view == nil {
			view = &IssueView{ID: ev.IssueID, AreaWeights: map[string]int{}}
			s.Issues[ev.IssueID] = view
			appendUnique(&s.Backlog, ev.IssueID)
		}
		view.Title = str("title")
		view.Flow = str("flow")
		view.Body = str("body")
		view.Preset = str("preset")
		view.Priority = int(num("priority"))
		view.State = "backlog"
```

In the existing `case core.EvIssueCreated:` branch, add one line after `s.Order = append(...)`:

```go
		removeString(&s.Backlog, ev.IssueID)
```

Note: `EvIssueCreated` replaces the whole `IssueView`, dropping the draft's Body/Preset/Priority from the view. That is correct — the launched lane's rendering never uses them.

In the `case core.EvIssueAbandoned:` branch, add alongside the other `removeString` calls:

```go
		removeString(&s.Backlog, ev.IssueID)
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/projection/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/projection/projection.go internal/projection/projection_test.go
git commit -m "feat: projection carries a backlog of drafts"
```

---

### Task 5: Proto server ops + CLI verbs

**Files:**
- Modify: `internal/proto/server.go`
- Modify: `cmd/watchtower/main.go`
- Test: `internal/proto/proto_test.go`

**Interfaces:**
- Consumes: `Engine.DraftIssue`, `Engine.UpdateIssue`, `Engine.LaunchIssue` (Tasks 1–2).
- Produces ops: `draft_issue` (fields like `create_issue`; returns `IssueID`), `update_issue` (same fields + `issue_id`), `launch_issue` (`issue_id`).
- Produces CLI: `watchtower new --draft`, `watchtower backlog`, `watchtower launch <id>`.
- `proto.Command` already has every needed field; no proto struct changes.

- [ ] **Step 1: Write failing proto tests**

Append to `internal/proto/proto_test.go`, following the existing `create_issue` round-trip test's harness (it spins a server over a socket and dials a client):

```go
func TestBacklogOps(t *testing.T) {
	c := newTestClient(t) // reuse the file's existing server+client setup helper/pattern
	r, err := c.Do(Command{Op: "draft_issue", Title: "t", Body: "b", Flow: "default", Preset: "regular", Priority: 2})
	if err != nil || !r.OK {
		t.Fatalf("draft_issue: %v %+v", err, r)
	}
	id := r.IssueID

	r, err = c.Do(Command{Op: "update_issue", IssueID: id, Title: "t2", Body: "b2", Flow: "default", Preset: "strict", Priority: 5})
	if err != nil || !r.OK {
		t.Fatalf("update_issue: %v %+v", err, r)
	}

	r, _ = c.Do(Command{Op: "list_issues"})
	found := false
	for _, row := range r.Issues {
		if row.ID == id {
			found = true
			if row.State != "backlog" || row.Title != "t2" || row.Priority != 5 {
				t.Fatalf("row wrong after update: %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("draft missing from list_issues")
	}

	r, err = c.Do(Command{Op: "launch_issue", IssueID: id})
	if err != nil || !r.OK {
		t.Fatalf("launch_issue: %v %+v", err, r)
	}
	// Launching twice fails: no longer a draft.
	r, _ = c.Do(Command{Op: "launch_issue", IssueID: id})
	if r.OK {
		t.Fatal("second launch succeeded")
	}
}

func TestDraftNotCountedInOverview(t *testing.T) {
	c := newTestClient(t)
	if _, err := c.Do(Command{Op: "draft_issue", Title: "t", Flow: "default", Preset: "regular"}); err != nil {
		t.Fatal(err)
	}
	r, err := c.Do(Command{Op: "overview"})
	if err != nil || !r.OK {
		t.Fatal(err)
	}
	if r.Overview.Building != 0 || r.Overview.Failing != 0 || r.Overview.Queued != 0 {
		t.Fatalf("draft counted in overview: %+v", r.Overview)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/proto/ -run 'BacklogOps|DraftNotCounted' -v`
Expected: FAIL — `unknown op draft_issue`.

- [ ] **Step 3: Implement server ops**

In `internal/proto/server.go`'s `exec` switch, after the `create_issue` case:

```go
	case "draft_issue":
		fl, ok := sv.flowFor(cmd.Flow)
		if !ok {
			return Response{Error: "unknown flow " + cmd.Flow}
		}
		lever := flow.Lever(cmd.Preset)
		if lever != flow.LeverYolo && lever != flow.LeverRegular && lever != flow.LeverStrict {
			lever = flow.LeverRegular
		}
		id, err := sv.eng.DraftIssue(cmd.Title, cmd.Body, cmd.Flow, string(lever), levers.Preset(fl, lever), cmd.Priority)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: id}
	case "update_issue":
		fl, ok := sv.flowFor(cmd.Flow)
		if !ok {
			return Response{Error: "unknown flow " + cmd.Flow}
		}
		lever := flow.Lever(cmd.Preset)
		if lever != flow.LeverYolo && lever != flow.LeverRegular && lever != flow.LeverStrict {
			lever = flow.LeverRegular
		}
		if err := sv.eng.UpdateIssue(cmd.IssueID, cmd.Title, cmd.Body, cmd.Flow, string(lever), levers.Preset(fl, lever), cmd.Priority); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: cmd.IssueID}
	case "launch_issue":
		if err := sv.eng.LaunchIssue(cmd.IssueID); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: cmd.IssueID}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/proto/ -v`
Expected: PASS.

- [ ] **Step 5: Add the CLI verbs**

In `cmd/watchtower/main.go`:

In the `case "new":` block, add a flag and branch on it:

```go
		draft := fs.Bool("draft", false, "save to the backlog instead of starting")
```

and replace the two `mustDo` calls with:

```go
		if *draft {
			r := mustDo(c, proto.Command{Op: "draft_issue", Title: *title, Flow: *flowName, Preset: *preset, Priority: *prio})
			fmt.Println(r.IssueID)
			break
		}
		r := mustDo(c, proto.Command{Op: "create_issue", Title: *title, Flow: *flowName, Preset: *preset, Priority: *prio})
		mustDo(c, proto.Command{Op: "start_issue", IssueID: r.IssueID})
		fmt.Println(r.IssueID)
```

(Note: `fs.Parse(args)` and the dial stay as they are; only the tail of the case changes. If the case is inside a `switch`, `break` exits the case as written.)

Add a `backlog` case next to `issues`:

```go
	case "backlog":
		fs := flag.NewFlagSet("backlog", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
		fs.Parse(args)
		c := mustDial(*data, *repoF)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "list_issues"})
		for _, issue := range r.Issues {
			if issue.State != "backlog" {
				continue
			}
			fmt.Printf("%s  p%d  %s  %s\n", issue.ID, issue.Priority, issue.Flow, issue.Title)
		}
```

Add `"launch"` to the shared verb case and its op map:

```go
	case "pause", "resume", "kill", "retry", "abandon", "launch":
```

```go
			"abandon": "abandon_issue", "launch": "launch_issue",
```

Also add the three verbs to the CLI's usage/help text if `main.go` prints one (search for the usage string listing `pause`/`resume` and extend it the same way).

- [ ] **Step 6: Build and smoke-check**

Run: `go build ./... && go test ./...`
Expected: builds; all tests PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/proto/server.go internal/proto/proto_test.go cmd/watchtower/main.go
git commit -m "feat: draft/update/launch ops and backlog CLI verbs"
```

---

### Task 6: TUI — modal saves to backlog, gains priority field and edit mode

**Files:**
- Modify: `internal/tui/modal.go`
- Modify: `internal/tui/app.go`
- Test: `internal/tui/modal_test.go`, `internal/tui/app_test.go`

**Interfaces:**
- Produces: `modalState.EditID string` (non-empty = editing an existing draft), `modalState.Priority string` (text field 5).
- Produces: `func (m Model) draftIssue(modal modalState) tea.Cmd`, `func (m Model) updateIssue(modal modalState) tea.Cmd` — both return `createIssueMsg` (reused; its handler already closes the modal on OK).
- Modal keys: create mode — `enter` create+start (unchanged), `ctrl+s` save to backlog; edit mode — `enter` and `ctrl+s` both save via `update_issue`.
- Priority parses with `strconv.Atoi`; empty string means 0; non-numeric sets `m.Err = "priority must be a number"` and keeps the modal open.

- [ ] **Step 1: Write failing tests**

In `internal/tui/modal_test.go`, following its existing table/render style:

```go
func TestModalPriorityField(t *testing.T) {
	m := modalState{}
	for i := 0; i < 4; i++ {
		m = m.input("tab")
	}
	if m.Field != 4 {
		t.Fatalf("Field = %d, want 4 (priority)", m.Field)
	}
	m = m.input("7")
	if m.Priority != "7" {
		t.Fatalf("Priority = %q", m.Priority)
	}
	m = m.input("tab")
	if m.Field != 0 {
		t.Fatalf("tab wrap: Field = %d, want 0", m.Field)
	}
}

func TestRenderModalHints(t *testing.T) {
	create := renderModal(modalState{}, 80)
	if !strings.Contains(create, "ctrl+s") || !strings.Contains(create, "create") {
		t.Fatalf("create-mode hints missing:\n%s", create)
	}
	edit := renderModal(modalState{EditID: "GH-1", Title: "t"}, 80)
	if !strings.Contains(edit, "save") || strings.Contains(edit, "create") {
		t.Fatalf("edit-mode hints wrong:\n%s", edit)
	}
	if !strings.Contains(edit, "edit issue") {
		t.Fatalf("edit-mode title wrong:\n%s", edit)
	}
}
```

In `internal/tui/app_test.go`, following its existing key-driving pattern (construct `Model`, send `tea.KeyMsg`, assert on the returned model — copy how existing modal tests there drive keys):

```go
func TestModalCtrlSValidatesTitle(t *testing.T) {
	m := Model{State: projection.NewState()}
	m2, _ := pressKey(m, "n") // reuse/extract the file's key-press helper
	m2, _ = pressKey(m2, "ctrl+s")
	if m2.(Model).Err != "title is required" {
		t.Fatalf("Err = %q", m2.(Model).Err)
	}
}

func TestModalBadPriorityKeepsModalOpen(t *testing.T) {
	m := Model{State: projection.NewState()}
	m2, _ := pressKey(m, "n")
	mm := m2.(Model)
	mm.modal.Title = "t"
	mm.modal.Priority = "abc"
	m3, _ := pressKey(mm, "ctrl+s")
	if m3.(Model).modal == nil || m3.(Model).Err != "priority must be a number" {
		t.Fatalf("bad priority: modal=%v err=%q", m3.(Model).modal, m3.(Model).Err)
	}
}
```

(If `app_test.go` has no `pressKey` helper, write one: `func pressKey(m Model, key string) (tea.Model, tea.Cmd)` building `tea.KeyMsg` the way the existing tests do — check how they construct key messages, e.g. `tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}` vs typed keys like ctrl+s which need `tea.KeyMsg{Type: tea.KeyCtrlS}`.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/tui/ -run 'ModalPriority|RenderModalHints|ModalCtrlS|ModalBadPriority' -v`
Expected: FAIL — `m.Priority undefined`, `EditID undefined`.

- [ ] **Step 3: Implement modal state and render**

In `internal/tui/modal.go`:

```go
type modalState struct {
	Title    string
	Body     string
	Field    int
	FlowName string
	Preset   string
	Priority string
	EditID   string // non-empty: editing this backlog draft instead of creating
}
```

In `input`, change `m.Field = (m.Field + 1) % 4` to `% 5`, and extend `setFieldValue`/`fieldValue` with `case 4:` for `Priority`.

In `renderModal`:

```go
func renderModal(m modalState, width int) string {
	flowName := m.FlowName
	if flowName == "" {
		flowName = "default"
	}
	preset := m.Preset
	if preset == "" {
		preset = string(flow.LeverRegular)
	}
	dim := lipgloss.NewStyle().Foreground(activeTheme.Dim)
	submit := keyChip("enter") + dim.Render(" create  ") + keyChip("ctrl+s") + dim.Render(" backlog")
	boxTitle := "new issue"
	if m.EditID != "" {
		submit = keyChip("enter") + dim.Render(" save")
		boxTitle = "edit issue"
	}
	lines := []string{
		modalField(m.Field == 0, "title", m.Title, true),
		modalField(m.Field == 1, "body", m.Body, false),
		modalField(m.Field == 2, "flow", flowName, false),
		modalField(m.Field == 3, "preset", preset, false),
		modalField(m.Field == 4, "priority", m.Priority, false),
		"",
		keyChip("tab") + dim.Render(" next field  ") + submit,
	}
	return renderBox(boxTitle, "", " esc cancel ", boundedLines(lines, max(1, width-6)))
}
```

- [ ] **Step 4: Implement app key handling and commands**

In `internal/tui/app.go`, replace the modal key block:

```go
		if m.modal != nil {
			switch key {
			case "esc":
				m.modal = nil
			case "enter", "ctrl+s":
				if strings.TrimSpace(m.modal.Title) == "" {
					m.Err = "title is required"
					return m, nil
				}
				if _, err := modalPriority(*m.modal); err != nil {
					m.Err = "priority must be a number"
					return m, nil
				}
				if m.client == nil {
					m.modal = nil
					return m, nil
				}
				switch {
				case m.modal.EditID != "":
					return m, m.updateIssue(*m.modal)
				case key == "ctrl+s":
					return m, m.draftIssue(*m.modal)
				default:
					return m, m.createIssue(*m.modal)
				}
			default:
				updated := m.modal.input(key)
				m.modal = &updated
			}
			return m, nil
		}
```

Add near `createIssue`:

```go
// modalPriority parses the modal's priority text; empty means 0.
func modalPriority(modal modalState) (int, error) {
	value := strings.TrimSpace(modal.Priority)
	if value == "" {
		return 0, nil
	}
	return strconv.Atoi(value)
}

func (m Model) draftIssue(modal modalState) tea.Cmd {
	return m.modalCommand(modal, "draft_issue", "")
}

func (m Model) updateIssue(modal modalState) tea.Cmd {
	return m.modalCommand(modal, "update_issue", modal.EditID)
}

// modalCommand sends one modal-backed op; createIssue keeps its own start step.
func (m Model) modalCommand(modal modalState, op, issueID string) tea.Cmd {
	if m.client == nil {
		return nil
	}
	flowName := strings.TrimSpace(modal.FlowName)
	if flowName == "" {
		flowName = "default"
	}
	preset := strings.TrimSpace(modal.Preset)
	if preset == "" {
		preset = "regular"
	}
	priority, _ := modalPriority(modal)
	client := m.client
	return func() tea.Msg {
		r, err := client.Do(proto.Command{Op: op, IssueID: issueID, Title: modal.Title,
			Body: modal.Body, Flow: flowName, Preset: preset, Priority: priority})
		return createIssueMsg{response: r, err: err}
	}
}
```

Also thread priority into `createIssue` (it currently drops it): add `Priority: priority` to its `proto.Command` using the same `modalPriority` parse.

Add `"strconv"` to the imports of `app.go`.

- [ ] **Step 5: Run to verify pass**

Run: `go test ./internal/tui/ -v`
Expected: PASS except possibly `modal` golden fixtures — if the golden differs (new priority field + hint line), regenerate per the repo's snapshot workflow (see how `internal/tui/snapshot_test.go` / `watchtower snap` regenerates `testdata/`), eyeball the new golden, and include it.

- [ ] **Step 6: Commit**

```bash
git add internal/tui/modal.go internal/tui/app.go internal/tui/modal_test.go internal/tui/app_test.go internal/tui/testdata/
git commit -m "feat: modal drafts to the backlog and edits drafts in place"
```

---

### Task 7: TUI — backlog view (b), launch, delete, edit round-trip

**Files:**
- Modify: `internal/tui/app.go` (backlog state + keys)
- Modify: `internal/tui/modal.go` (renderBacklog)
- Modify: `internal/tui/render.go` (help overlay entries)
- Modify: `internal/tui/fixtures.go` (backlog fixture flow)
- Test: `internal/tui/app_test.go`, goldens in `internal/tui/testdata/`

**Interfaces:**
- Consumes: `State.Backlog`, `IssueView.{Priority,Body,Preset}` (Task 4); `modalState.EditID` (Task 6); ops `launch_issue`, `abandon_issue`.
- Produces: `Model.backlog *backlogState`; `backlogState{Sel int}`; `func backlogEntries(s *projection.State) []*projection.IssueView` (priority descending, id ascending tiebreak); `func renderBacklog(entries []*projection.IssueView, sel, width int) string`.

- [ ] **Step 1: Write failing tests**

Append to `internal/tui/app_test.go`:

```go
func backlogFixtureState() *projection.State {
	s := projection.NewState()
	for _, spec := range []struct {
		id, title string
		prio      int
	}{{"GH-2", "low fix", 0}, {"GH-3", "hot fix", 5}} {
		ev, _ := core.NewEvent(core.EvIssueDrafted, spec.id, map[string]any{
			"title": spec.title, "body": "b", "flow": "default", "preset": "regular",
			"priority": spec.prio})
		s.Apply(ev)
	}
	return s
}

func TestBacklogEntriesSorted(t *testing.T) {
	entries := backlogEntries(backlogFixtureState())
	if len(entries) != 2 || entries[0].ID != "GH-3" || entries[1].ID != "GH-2" {
		t.Fatalf("order wrong: %v", entries)
	}
}

func TestBacklogViewKeys(t *testing.T) {
	m := Model{State: backlogFixtureState()}
	m2, _ := pressKey(m, "b")
	mm := m2.(Model)
	if mm.backlog == nil {
		t.Fatal("b did not open the backlog view")
	}
	// enter on the top entry opens the edit modal pre-filled
	m3, _ := pressKey(mm, "enter")
	mm = m3.(Model)
	if mm.modal == nil || mm.modal.EditID != "GH-3" || mm.modal.Title != "hot fix" || mm.modal.Priority != "5" {
		t.Fatalf("edit modal not prefilled: %+v", mm.modal)
	}
	// esc back out of the modal, l arms the launch confirm
	m4, _ := pressKey(mm, "esc")
	m5, _ := pressKey(m4.(Model), "l")
	mm = m5.(Model)
	if mm.confirm == nil || mm.confirm.Op != "launch_issue" || mm.confirm.IssueID != "GH-3" {
		t.Fatalf("launch confirm wrong: %+v", mm.confirm)
	}
	// n cancels; X arms the abandon confirm
	m6, _ := pressKey(mm, "n")
	m7, _ := pressKey(m6.(Model), "j")
	m8, _ := pressKey(m7.(Model), "X")
	mm = m8.(Model)
	if mm.confirm == nil || mm.confirm.Op != "abandon_issue" || mm.confirm.IssueID != "GH-2" {
		t.Fatalf("abandon confirm wrong: %+v", mm.confirm)
	}
	// esc closes the view
	m9, _ := pressKey(mm, "esc")   // clears confirm
	m10, _ := pressKey(m9.(Model), "esc") // closes backlog
	if m10.(Model).backlog != nil {
		t.Fatal("esc did not close the backlog view")
	}
}
```

Note on `esc` in the modal test: closing the edit modal must return to the backlog view (see Step 3).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/tui/ -run Backlog -v`
Expected: FAIL — `m.backlog undefined`, `backlogEntries undefined`.

- [ ] **Step 3: Implement**

In `internal/tui/app.go`:

Add to `Model` (near `modal`): `backlog *backlogState`. Add:

```go
type backlogState struct{ Sel int }

// backlogEntries returns drafts for display: highest priority first, id as
// the tiebreak so the order is stable.
func backlogEntries(s *projection.State) []*projection.IssueView {
	if s == nil {
		return nil
	}
	var entries []*projection.IssueView
	for _, id := range s.Backlog {
		if iv := s.Issues[id]; iv != nil {
			entries = append(entries, iv)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Priority != entries[j].Priority {
			return entries[i].Priority > entries[j].Priority
		}
		return entries[i].ID < entries[j].ID
	})
	return entries
}
```

(add `"sort"` to imports.)

Modal esc returns to the backlog when editing — in the modal key block from Task 6, change the esc case to:

```go
			case "esc":
				if m.modal.EditID != "" {
					m.backlog = &backlogState{}
				}
				m.modal = nil
```

And in the `createIssueMsg` handler, after `m.modal = nil` on success, reopen the backlog if the submit was an edit. The message doesn't carry mode, so capture it instead: change the handler to

```go
	case createIssueMsg:
		if msg.err != nil {
			m.Err = msg.err.Error()
			return m, nil
		}
		if !msg.response.OK {
			m.Err = msg.response.Error
			return m, nil
		}
		if m.modal != nil && m.modal.EditID != "" {
			m.backlog = &backlogState{}
		}
		m.modal = nil
		return m, nil
```

Add the backlog key block AFTER the arrow-alias rewrite and AFTER the `m.confirm` block, right before the `m.leverEditor` block (confirm must win while a launch/abandon prompt is up; arrows-as-hjkl are fine here — `right` meaning launch is acceptable because `l` is the documented key, matching the lever editor's use of h/l):

```go
		if m.backlog != nil {
			entries := backlogEntries(m.State)
			switch key {
			case "esc", "b":
				m.backlog = nil
			case "j":
				m.backlog.Sel = min(m.backlog.Sel+1, max(0, len(entries)-1))
			case "k":
				m.backlog.Sel = max(m.backlog.Sel-1, 0)
			case "enter":
				if m.backlog.Sel < len(entries) {
					iv := entries[m.backlog.Sel]
					m.Err = ""
					m.modal = &modalState{EditID: iv.ID, Title: iv.Title, Body: iv.Body,
						FlowName: iv.Flow, Preset: iv.Preset, Priority: strconv.Itoa(iv.Priority)}
					m.backlog = nil
				}
			case "l":
				if m.backlog.Sel < len(entries) {
					iv := entries[m.backlog.Sel]
					m.confirm = &confirmState{IssueID: iv.ID, Op: "launch_issue",
						Prompt: fmt.Sprintf("launch %s? the lane starts now. y/n", iv.Title)}
				}
			case "X":
				if m.backlog.Sel < len(entries) {
					iv := entries[m.backlog.Sel]
					m.confirm = &confirmState{IssueID: iv.ID, Op: "abandon_issue",
						Prompt: fmt.Sprintf("delete draft %s? it is removed for good. y/n", iv.Title)}
				}
			}
			return m, nil
		}
```

One wrinkle: after `y` on a confirm opened from the backlog, the generic confirm handler runs `issueCommand` and clears `m.confirm`, and the still-open... — note the backlog block above `return`s on every key, so the confirm block must stay ABOVE it (it already is). After the command, the backlog view stays open and the draft disappears from `entries` on the next event poll; clamp `Sel` in the render path (below) so a deleted last row can't leave the cursor out of range.

Add the open key alongside the other tower keys (near `if key == "n"`):

```go
			if key == "b" {
				m.Err = ""
				m.backlog = &backlogState{}
				return m, nil
			}
```

In the `View`/render function, add the overlay branch after the `m.modal` branch:

```go
	} else if m.backlog != nil {
		entries := backlogEntries(m.State)
		m.backlog.Sel = min(m.backlog.Sel, max(0, len(entries)-1))
		overlayBox = renderBacklog(entries, m.backlog.Sel, layoutWidth)
	}
```

(If `View` has a value receiver preventing the clamp write, clamp into a local `sel` variable instead and pass that.)

In `internal/tui/modal.go`, add:

```go
// renderBacklog lists drafts in the same box chrome as the new-issue modal:
// the backlog is where issues wait, so it wears the issue modal's clothes.
func renderBacklog(entries []*projection.IssueView, sel, width int) string {
	t := activeTheme
	dim := lipgloss.NewStyle().Foreground(t.Dim)
	const rowWidth = 44
	var lines []string
	if len(entries) == 0 {
		lines = append(lines, dim.Render("backlog is empty — n then ctrl+s files a draft"))
	}
	for i, iv := range entries {
		prio := lipgloss.NewStyle().Foreground(t.Structure).Render(fmt.Sprintf("p%d", iv.Priority))
		row := padCell(iv.ID, 7) + padCell(prio, 5) + iv.Title
		lines = append(lines, cursorRow(i == sel, boundedLine(row, rowWidth), rowWidth+4))
	}
	lines = append(lines, "",
		keyChip("enter")+dim.Render(" edit  ")+keyChip("l")+dim.Render(" launch  ")+
			keyChip("X")+dim.Render(" delete  ")+keyChip("j/k")+dim.Render(" move"))
	return renderBox("backlog", "drafts waiting to launch", " esc close ", strings.Join(lines, "\n"))
}
```

(add `"fmt"` and the projection import to `modal.go`; check whether a `boundedLine` single-line helper exists — if only `boundedLines` exists, use `boundedLines([]string{row}, rowWidth)`. `padCell` and `cursorRow` already exist — see `renderLeverEditor`.)

In `internal/tui/render.go`, extend the help overlay: in the `{"CONTROL", ...}` group add `{"b", "backlog"}`, `{"l", "launch draft (in backlog)"}`, and in whichever group holds `n` add a `ctrl+s save draft` line adjacent to it (match the existing grouping/format exactly).

- [ ] **Step 4: Add the golden fixture**

In `internal/tui/fixtures.go`: add `"backlog"` to the flow list returned at line ~19, and a `case "backlog":` in the fixture switch that builds `backlogFixtureState()`-equivalent drafted events on the fixture state and sets `m.backlog = &backlogState{}` — mirror exactly how `case "modal":` constructs its Model. Then regenerate goldens via the repo's snapshot workflow (`watchtower snap` / snapshot test), eyeball `internal/tui/testdata/backlog.txt`, and commit it.

- [ ] **Step 5: Run the full TUI suite**

Run: `go test ./internal/tui/ -v`
Expected: PASS, including snapshot tests with the new `backlog.txt` golden and any legitimately-changed goldens (modal, help). Inspect every changed golden by eye before accepting.

- [ ] **Step 6: Commit**

```bash
git add internal/tui/
git commit -m "feat: b opens a backlog box to edit, launch, and delete drafts"
```

---

### Task 8: Full verification and live smoke

**Files:** none (verification only)

- [ ] **Step 1: Full test suite**

Run: `go test ./...`
Expected: all packages PASS.

- [ ] **Step 2: Vet and build**

Run: `go vet ./... && go build ./...`
Expected: clean.

- [ ] **Step 3: Live smoke (only if a daemon environment is available; otherwise skip and say so)**

Follow the `rebuilding-watchtower` skill to rebuild/install, then:

```bash
watchtower new --draft --title "backlog smoke test" --priority 3
watchtower backlog          # shows the draft
watchtower abandon <id>     # removes it
watchtower backlog          # empty again
```

- [ ] **Step 4: Commit any stragglers and report**

Report results honestly: what passed, what was skipped.
