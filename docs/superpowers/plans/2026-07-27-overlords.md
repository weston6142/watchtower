# Overlords Implementation Plan (Plan 3 of 4)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The three overlords — Merge Marshal (overlap prediction, merge sequencing, the real merge train), Librarian (context injection + doc reconciliation), Issue Steward (issue sync + proposal triage) — plus decision persistence with blocking-cost ordering and the reviewer spec-conformance fix from the live smoke.

**Architecture:** Overlords are engine-owned components woken by events, not always-on processes. The Marshal's prediction core is deterministic (planner now emits `touchset.json`; overlap = glob intersection) — LLM agents are dispatched only for conflict repair and doc reconciliation through the existing Runner. Proposals ride the same marker protocol as decisions (`watchtower_proposal`).

**Tech Stack:** unchanged (Go, existing internal packages, stub-script runner tests).

## Global Constraints

- All Plan 1/2 global constraints apply. Runner interface stays frozen.
- New events: `merge_sequenced`, `merge_started`, `issue_merged`, `merge_conflict`, `proposal_accepted`, `proposal_rejected`, `docs_reconciled`. (`proposal_filed` exists since Plan 1.)
- Decision marker protocol is frozen; the proposal marker mirrors it exactly: a line starting `{"watchtower_proposal":` with `{"title": string, "body": string}`.
- Deterministic overlord logic is unit-tested; LLM dispatches are tested with stub scripts only.
- The Marshal is the ONLY component that writes to the target repo's default branch.

---

### Task 1: Persist decisions + blocking-cost ordering

**Files:**
- Modify: `internal/store/store.go`, `internal/engine/engine.go`
- Test: `internal/store/store_test.go`, `internal/engine/engine_test.go` (extend)

**Interfaces:**
- Store gains:
  ```go
  type DecisionRow struct {
      ID int64; IssueID, Stage, Question string; Options []string // stored as JSON
      Recommended int; Status string // "pending"|"answered"|"auto"
      Answer int; BlockingCost int; CreatedAt time.Time
  }
  func (s *Store) InsertDecision(d DecisionRow) (int64, error)
  func (s *Store) AnswerDecision(id int64, answer int, status string) error
  func (s *Store) PendingDecisionRows() ([]DecisionRow, error) // ORDER BY blocking_cost DESC, created_at ASC
  ```
- Engine: `escalate` inserts the row (BlockingCost from a new `Engine.blockingCost(issueID)` — 1 + number of issues sequenced behind this issue, wired fully in Task 5; until then returns 1) and uses the DB id as the decision ID (replaces the in-memory `nextDec` counter). `Answer` updates the row. `handleAsk`'s auto-resolve path inserts with status "auto" and the recommended answer (audit trail). `PendingDecisions()` now reads pending rows from the store and joins them with the in-memory reply channels; ordering comes from `PendingDecisionRows`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/store_test.go`:

```go
func TestDecisionOrderingByBlockingCost(t *testing.T) {
	s, _ := Open("file:t3?mode=memory&cache=shared")
	defer s.Close()
	old := time.Now().Add(-time.Hour)
	id1, _ := s.InsertDecision(DecisionRow{IssueID: "GH-1", Question: "small", BlockingCost: 1, CreatedAt: time.Now()})
	id2, _ := s.InsertDecision(DecisionRow{IssueID: "GH-2", Question: "big", BlockingCost: 3, CreatedAt: time.Now()})
	id3, _ := s.InsertDecision(DecisionRow{IssueID: "GH-3", Question: "old-small", BlockingCost: 1, CreatedAt: old})
	rows, err := s.PendingDecisionRows()
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].ID != id2 || rows[1].ID != id3 || rows[2].ID != id1 {
		t.Fatalf("order wrong: %v %v %v", rows[0].ID, rows[1].ID, rows[2].ID)
	}
	if err := s.AnswerDecision(id2, 0, "answered"); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.PendingDecisionRows()
	if len(rows) != 2 {
		t.Fatalf("answered row still pending: %v", rows)
	}
}
```

Append to `internal/engine/engine_test.go`:

```go
func TestAutoResolvedDecisionsAreAudited(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, _ := e.CreateIssue("a", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)
	errc := make(chan error, 1)
	go func() { errc <- e.StartIssue(context.Background(), id) }()
	for {
		if ds := e.PendingDecisions(); len(ds) == 1 {
			e.Answer(ds[0].ID, 0)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	<-errc
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
```

(Also add `func (s *Store) AllDecisionRows() ([]DecisionRow, error)` — same SELECT without the status filter.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ ./internal/engine/ -run 'Decision' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Store (`internal/store/store.go`):

```go
type DecisionRow struct {
	ID           int64
	IssueID      string
	Stage        string
	Question     string
	Options      []string
	Recommended  int
	Status       string
	Answer       int
	BlockingCost int
	CreatedAt    time.Time
}

func (s *Store) InsertDecision(d DecisionRow) (int64, error) {
	opts, _ := json.Marshal(d.Options)
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	if d.Status == "" {
		d.Status = "pending"
	}
	res, err := s.db.Exec(
		`INSERT INTO decisions(issue_id,question,options,recommended,lever,status,answer,answered_by,blocking_cost,created_at,evidence)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		d.IssueID, d.Question, string(opts), d.Recommended, d.Stage, d.Status,
		d.Answer, "", d.BlockingCost, d.CreatedAt.Format(time.RFC3339Nano), "")
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) AnswerDecision(id int64, answer int, status string) error {
	_, err := s.db.Exec(`UPDATE decisions SET status=?, answer=? WHERE id=?`, status, answer, id)
	return err
}

func (s *Store) decisionRows(where string) ([]DecisionRow, error) {
	rows, err := s.db.Query(
		`SELECT id,issue_id,lever,question,options,recommended,status,answer,blocking_cost,created_at
		 FROM decisions ` + where + ` ORDER BY blocking_cost DESC, created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DecisionRow
	for rows.Next() {
		var d DecisionRow
		var opts, created string
		if err := rows.Scan(&d.ID, &d.IssueID, &d.Stage, &d.Question, &opts,
			&d.Recommended, &d.Status, &d.Answer, &d.BlockingCost, &created); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(opts), &d.Options)
		d.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) PendingDecisionRows() ([]DecisionRow, error) {
	return s.decisionRows(`WHERE status='pending'`)
}

func (s *Store) AllDecisionRows() ([]DecisionRow, error) {
	return s.decisionRows(``)
}
```

(Note: the `lever` column double-duties as the stage name — it already exists in the Plan 1 schema; renaming a column would need a migration, so document the reuse with a comment at the SELECT.)

Engine (`internal/engine/engine.go`):

- Delete `nextDec`; `escalate` becomes:

```go
func (e *Engine) escalate(issueID, stage string, d levers.Decision) int {
	rowID, err := e.cfg.Store.InsertDecision(store.DecisionRow{
		IssueID: issueID, Stage: stage, Question: d.Question,
		Options: d.Options, Recommended: d.Recommended,
		BlockingCost: e.blockingCost(issueID),
	})
	if err != nil {
		// Without a persisted row the decision cannot be answered; fail loud.
		panic(fmt.Sprintf("insert decision: %v", err))
	}
	p := &pending{
		PendingDecision: PendingDecision{ID: rowID, IssueID: issueID, Stage: stage, D: d},
		reply:           make(chan int, 1),
	}
	e.mu.Lock()
	e.pend[rowID] = p
	e.mu.Unlock()
	e.emit(core.EvDecisionRequired, issueID, map[string]any{
		"decision_id": rowID, "stage": stage, "question": d.Question,
		"options": d.Options, "recommended": d.Recommended})
	return <-p.reply
}

// blockingCost is 1 (the issue itself) plus every issue sequenced behind it
// by the Merge Marshal. Wired fully in the marshal task.
func (e *Engine) blockingCost(issueID string) int {
	if e.cfg.Marshal == nil {
		return 1
	}
	return 1 + e.cfg.Marshal.BlockedBehind(issueID)
}
```

(Add `Marshal *marshal.Marshal` to `Config` in Task 5; until then declare the field as an interface `interface{ BlockedBehind(string) int }` named `Sequencer` in the engine package so this task compiles standalone: `Marshal Sequencer`.)

- `Answer`: after the reply send, call `e.cfg.Store.AnswerDecision(decisionID, option, "answered")`.
- `handleAsk` auto path: insert with `Status: "auto", Answer: a.Decision.Recommended` before emitting the event.
- `PendingDecisions()`: read `PendingDecisionRows()`, return them in that order, joining `e.pend` for validity (skip rows with no live channel — they belong to a previous daemon run).

- [ ] **Step 4: Run the full suite**

Run: `go test ./... -race`
Expected: PASS (proto/CLI unaffected — `PendingDecision` shape unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/store/ internal/engine/
git commit -m "feat: persist decisions with blocking-cost ordering and auto-resolve audit"
```

---

### Task 2: Proposal marker → triage tray → CLI

**Files:**
- Modify: `internal/claude/stream.go`, `internal/claude/runner.go`, `internal/runner/runner.go`, `internal/runner/fake.go`, `internal/engine/engine.go`, `internal/store/store.go`, `internal/proto/proto.go`, `internal/proto/server.go`, `cmd/watchtower/main.go`
- Test: `internal/claude/stream_test.go`, `internal/engine/engine_test.go`, `internal/store/store_test.go` (extend each)

**Interfaces:**
- Codec: `type Proposal struct { Title, Body string }`; `func ExtractProposal(text string) (Proposal, bool)` — mirrors `ExtractDecision` for lines starting `{"watchtower_proposal":`.
- Runner contract: `runner.Ask` gains sibling — `Run`'s `asks` channel stays decisions-only; proposals are fire-and-forget, so `runner.Result` is unchanged and instead the `Run` signature does NOT change: the ClaudeCodeRunner surfaces proposals through a new optional callback field `OnProposal func(issueID string, p Proposal)` on `CodeRunner` (set by the daemon at construction). FakeRunner gains the same field plus `Proposals []claude.Proposal` per Script — wait, that would import claude from runner; instead define `Proposal` in `internal/runner/runner.go` (`type Proposal struct{ Title, Body string }`) and have the claude codec return `runner.Proposal`. FakeRunner Script gains `Proposals []runner.Proposal`, emitted via the callback before artifacts.
- Store: `func (s *Store) InsertProposal(issueID, title, body string) (int64, error)`, `func (s *Store) SetProposalStatus(id int64, status string) error`, `func (s *Store) PendingProposals() ([]ProposalRow, error)` with `type ProposalRow struct{ ID int64; IssueID, Title, Body, Status string }`.
- Engine: `func (e *Engine) FileProposal(issueID, title, body string)` — inserts + emits `proposal_filed`; `func (e *Engine) ResolveProposal(id int64, accept bool, flowName, preset string) (string, error)` — accept: creates a real issue via `CreateIssue` (regular-preset matrix from the named flow), sets status "accepted", emits `proposal_accepted`, returns new issue ID (caller starts it explicitly); reject: status "rejected" + `proposal_rejected`.
- Proto: ops `list_proposals` (→ `Proposals []store.ProposalRow` on Response) and `resolve_proposal` (fields `ProposalID int64`, `Accept bool`; returns `IssueID` when accepted).
- CLI: `watchtower proposals`, `watchtower accept-proposal <id>`, `watchtower reject-proposal <id>`.

- [ ] **Step 1: Write the failing tests**

Codec test (append to `internal/claude/stream_test.go`):

```go
func TestExtractProposal(t *testing.T) {
	text := "found something\n{\"watchtower_proposal\": {\"title\": \"Refactor refunds\", \"body\": \"3 call sites entangled\"}}"
	p, ok := ExtractProposal(text)
	if !ok || p.Title != "Refactor refunds" || p.Body != "3 call sites entangled" {
		t.Fatalf("proposal: %+v ok=%v", p, ok)
	}
	if _, ok := ExtractProposal("nothing"); ok {
		t.Fatal("false positive")
	}
}
```

Store test (append):

```go
func TestProposalLifecycle(t *testing.T) {
	s, _ := Open("file:t4?mode=memory&cache=shared")
	defer s.Close()
	id, err := s.InsertProposal("GH-1", "New task", "details")
	if err != nil {
		t.Fatal(err)
	}
	ps, _ := s.PendingProposals()
	if len(ps) != 1 || ps[0].Title != "New task" {
		t.Fatalf("pending: %+v", ps)
	}
	s.SetProposalStatus(id, "accepted")
	if ps, _ = s.PendingProposals(); len(ps) != 0 {
		t.Fatalf("still pending: %+v", ps)
	}
}
```

Engine test (append):

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/claude/ ./internal/store/ ./internal/engine/ -run 'Proposal' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

`internal/runner/runner.go`:

```go
// Proposal is a suggested new issue discovered by an agent mid-flow.
type Proposal struct {
	Title string
	Body  string
}
```

Codec (`internal/claude/stream.go`):

```go
type proposalMarker struct {
	P struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	} `json:"watchtower_proposal"`
}

// ExtractProposal scans assistant text for the watchtower_proposal marker.
func ExtractProposal(text string) (runner.Proposal, bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `{"watchtower_proposal":`) {
			continue
		}
		var m proposalMarker
		if err := json.Unmarshal([]byte(line), &m); err != nil || m.P.Title == "" {
			continue
		}
		return runner.Proposal{Title: m.P.Title, Body: m.P.Body}, true
	}
	return runner.Proposal{}, false
}
```

(Import the runner package; codec keeps its own `Proposal` out — reuse `runner.Proposal`.)

`internal/claude/runner.go`: add field `OnProposal func(issueID string, p runner.Proposal)` to `CodeRunner`; in the `KindAssistantText` case, after the decision check:

```go
			if p, found := ExtractProposal(ev.Text); found && c.OnProposal != nil {
				c.OnProposal(issueID, p)
			}
```

`internal/runner/fake.go`: `Script` gains `Proposals []Proposal`; `FakeRunner` gains `OnProposal func(string, Proposal)`; emit each before artifact writing:

```go
		for _, p := range sc.Proposals {
			if f.OnProposal != nil {
				f.OnProposal(issueID, p)
			}
		}
```

Store: add `ProposalRow`, `InsertProposal`, `SetProposalStatus`, `PendingProposals` (SELECT ... WHERE status='pending' ORDER BY id; InsertProposal sets status 'pending').

Engine:

```go
func (e *Engine) FileProposal(issueID, title, body string) {
	if _, err := e.cfg.Store.InsertProposal(issueID, title, body); err != nil {
		return
	}
	e.emit(core.EvProposalFiled, issueID, map[string]string{"title": title})
}

func (e *Engine) ResolveProposal(id int64, accept bool, flowName, preset string) (string, error) {
	if !accept {
		if err := e.cfg.Store.SetProposalStatus(id, "rejected"); err != nil {
			return "", err
		}
		e.emit(core.EvProposalRejected, "", map[string]any{"proposal_id": id})
		return "", nil
	}
	ps, err := e.cfg.Store.PendingProposals()
	if err != nil {
		return "", err
	}
	var row *store.ProposalRow
	for i := range ps {
		if ps[i].ID == id {
			row = &ps[i]
		}
	}
	if row == nil {
		return "", fmt.Errorf("no pending proposal %d", id)
	}
	f, ok := e.cfg.Flows[flowName]
	if !ok {
		return "", fmt.Errorf("unknown flow %q", flowName)
	}
	lever := flow.Lever(preset)
	if lever != flow.LeverYolo && lever != flow.LeverRegular && lever != flow.LeverStrict {
		lever = flow.LeverRegular
	}
	newID, err := e.CreateIssue(row.Title, row.Body, flowName, levers.Preset(f, lever), 0)
	if err != nil {
		return "", err
	}
	if err := e.cfg.Store.SetProposalStatus(id, "accepted"); err != nil {
		return "", err
	}
	e.emit(core.EvProposalAccepted, newID, map[string]any{"proposal_id": id})
	return newID, nil
}
```

Add event constants `EvProposalAccepted EventType = "proposal_accepted"`, `EvProposalRejected EventType = "proposal_rejected"` to `internal/core/event.go`.

Proto + CLI: `list_proposals` / `resolve_proposal` ops in `server.go` `exec` switch (calling the engine methods), `Proposals []store.ProposalRow` + `ProposalID`/`Accept` fields on the wire types, and the three CLI subcommands printing `[id] (from ISSUE) title — body`.

Daemon (`cmd/watchtower/main.go`): construct runners with `OnProposal: eng.FileProposal` — note engine is created after the runner today; reorder so the engine is constructed first with a nil runner, then set — simplest: give `CodeRunner`/`FakeRunner` the callback after `engine.New` (`run` is stored by pointer in both cases; set the field post-construction before `Serve`).

- [ ] **Step 4: Run the full suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ cmd/
git commit -m "feat: proposal marker, triage tray, and accept/reject CLI"
```

---

### Task 3: Issue Steward — issues table sync

**Files:**
- Create: `internal/steward/steward.go`
- Modify: `internal/store/store.go`, `internal/engine/engine.go` (emit hook), `cmd/watchtower/main.go`, `internal/proto/proto.go`, `internal/proto/server.go`
- Test: `internal/steward/steward_test.go`

**Interfaces:**
- Store: `type IssueRow struct{ ID, Title, Body, State, Flow string; Priority int }`, `func (s *Store) UpsertIssue(r IssueRow) error`, `func (s *Store) Issues() ([]IssueRow, error)`.
- Steward:
  ```go
  type Steward struct{ Store *store.Store }
  // Observe applies one event to the issues table. Deterministic projection —
  // the durable sibling of internal/projection (which is per-client, in-memory).
  func (st *Steward) Observe(ev core.Event)
  ```
  Semantics: `issue_created` → upsert (state "running", title/flow from payload — extend `CreateIssue`'s emit payload to include `body` and `priority`); `stage_started` → state "running:<stage>"; `decision_required` → "waiting_decision"; `decision_answered` → "running"; `stage_failed` → "failed"; `issue_completed` → "done"; `issue_merged` (Task 6) → "merged".
- Engine: `Config` gains `Observers []func(core.Event)`; `emit` calls each observer after appending to the store. The daemon registers `steward.Observe`.
- Proto/CLI: op `list_issues` → `Issues []store.IssueRow`; CLI `watchtower issues` printing `ID  STATE  TITLE`.

- [ ] **Step 1: Write the failing test**

```go
// internal/steward/steward_test.go
package steward

import (
	"testing"

	"github.com/wbushyeager/watchtower/internal/core"
	"github.com/wbushyeager/watchtower/internal/store"
)

func ev(t *testing.T, typ core.EventType, issue string, payload any) core.Event {
	t.Helper()
	e, err := core.NewEvent(typ, issue, payload)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestStewardProjectsIssueTable(t *testing.T) {
	s, _ := store.Open("file:st1?mode=memory&cache=shared")
	defer s.Close()
	st := &Steward{Store: s}
	st.Observe(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "hello", "flow": "default", "body": "b", "priority": float64(2)}))
	st.Observe(ev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "spec"}))
	rows, _ := s.Issues()
	if len(rows) != 1 || rows[0].State != "running:spec" || rows[0].Title != "hello" || rows[0].Priority != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	st.Observe(ev(t, core.EvStageFailed, "GH-1", map[string]any{"stage": "spec"}))
	rows, _ = s.Issues()
	if rows[0].State != "failed" {
		t.Fatalf("state: %s", rows[0].State)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/steward/ -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Store: `UpsertIssue` (INSERT ... ON CONFLICT(id) DO UPDATE SET title/body/state/flow/priority), `Issues()` (SELECT ordered by id). Steward:

```go
// internal/steward/steward.go
package steward

import (
	"encoding/json"

	"github.com/wbushyeager/watchtower/internal/core"
	"github.com/wbushyeager/watchtower/internal/store"
)

type Steward struct {
	Store *store.Store
}

func (st *Steward) Observe(ev core.Event) {
	var p map[string]any
	json.Unmarshal(ev.Payload, &p)
	str := func(k string) string { v, _ := p[k].(string); return v }
	setState := func(state string) {
		rows, err := st.Store.Issues()
		if err != nil {
			return
		}
		for _, r := range rows {
			if r.ID == ev.IssueID {
				r.State = state
				st.Store.UpsertIssue(r)
				return
			}
		}
	}
	switch ev.Type {
	case core.EvIssueCreated:
		prio, _ := p["priority"].(float64)
		st.Store.UpsertIssue(store.IssueRow{
			ID: ev.IssueID, Title: str("title"), Body: str("body"),
			Flow: str("flow"), State: "running", Priority: int(prio)})
	case core.EvStageStarted:
		setState("running:" + str("stage"))
	case core.EvDecisionRequired:
		setState("waiting_decision")
	case core.EvDecisionAnswered:
		setState("running")
	case core.EvStageFailed:
		setState("failed")
	case core.EvIssueCompleted:
		setState("done")
	case core.EvIssueMerged:
		setState("merged")
	}
}
```

(Declare `EvIssueMerged EventType = "issue_merged"` in core now — Task 6 emits it. Also extend `CreateIssue`'s emit payload with `"body": body, "priority": priority`.)

Engine `emit` change:

```go
func (e *Engine) emit(t core.EventType, issueID string, payload any) {
	ev, err := core.NewEvent(t, issueID, payload)
	if err != nil {
		return
	}
	ev, err = e.cfg.Store.Append(ev)
	if err != nil {
		return
	}
	for _, o := range e.cfg.Observers {
		o(ev)
	}
}
```

Daemon: `Observers: []func(core.Event){(&steward.Steward{Store: st}).Observe}` in the engine config; add `list_issues` op + `watchtower issues` CLI.

- [ ] **Step 4: Run the full suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ cmd/
git commit -m "feat: issue steward projects events into the issues table"
```

---

### Task 4: Touchset artifact from the planner

**Files:**
- Create: `internal/touchset/touchset.go`
- Modify: `dist/flows/default.yaml` (plan stage artifacts), `dist/packages/planner/prompt.md`
- Test: `internal/touchset/touchset_test.go`

**Interfaces:**
- ```go
  type Set struct { Globs []string `json:"globs"` }
  func Load(path string) (Set, error)              // reads touchset.json
  func Overlap(a, b Set) bool                       // any glob pair may match the same file
  ```
- Overlap rule (deterministic, conservative): two globs overlap if either matches the other's literal prefix (strip everything from the first `*`); e.g. `internal/pay/**` vs `internal/pay/refund.go` → true; `internal/pay/**` vs `docs/**` → false. Exact rule: `prefix(a)` and `prefix(b)` — overlap iff one prefix is a prefix of the other (string-wise, after cleaning trailing `/`).
- Planner prompt gains: "Also write touchset.json: {\"globs\": [...]} listing every file or directory glob this plan will create or modify. Be complete — the merge scheduler uses it." Flow's plan stage artifacts become `[plan.md, touchset.json]`.

- [ ] **Step 1: Write the failing test**

```go
// internal/touchset/touchset_test.go
package touchset

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndOverlap(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "touchset.json")
	os.WriteFile(p, []byte(`{"globs":["internal/pay/**","go.mod"]}`), 0o644)
	a, err := Load(p)
	if err != nil || len(a.Globs) != 2 {
		t.Fatalf("load: %+v %v", a, err)
	}
	cases := []struct {
		b    []string
		want bool
	}{
		{[]string{"internal/pay/refund.go"}, true},
		{[]string{"internal/payments/**"}, false},
		{[]string{"docs/**"}, false},
		{[]string{"go.mod"}, true},
		{[]string{"internal/**"}, true},
	}
	for _, c := range cases {
		if got := Overlap(a, Set{Globs: c.b}); got != c.want {
			t.Errorf("overlap(%v)=%v want %v", c.b, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/touchset/ -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// internal/touchset/touchset.go
package touchset

import (
	"encoding/json"
	"os"
	"strings"
)

// Set is the list of file globs a plan expects to create or modify.
type Set struct {
	Globs []string `json:"globs"`
}

func Load(path string) (Set, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Set{}, err
	}
	var s Set
	if err := json.Unmarshal(b, &s); err != nil {
		return Set{}, err
	}
	return s, nil
}

// prefix strips a glob to its literal leading path (everything before the
// first wildcard), without a trailing slash.
func prefix(glob string) string {
	if i := strings.IndexAny(glob, "*?["); i >= 0 {
		glob = glob[:i]
	}
	return strings.TrimSuffix(glob, "/")
}

// Overlap is conservative: two globs may touch the same file iff one literal
// prefix is a path-prefix of the other.
func Overlap(a, b Set) bool {
	for _, ga := range a.Globs {
		pa := prefix(ga)
		for _, gb := range b.Globs {
			pb := prefix(gb)
			if pathPrefix(pa, pb) || pathPrefix(pb, pa) {
				return true
			}
		}
	}
	return false
}

func pathPrefix(p, of string) bool {
	if p == of {
		return true
	}
	return strings.HasPrefix(of, p+"/") || p == ""
}
```

(Watch the `p == ""` case: an empty prefix — glob starting with a wildcard like `**` — overlaps everything, which is the conservative right answer.)

Note `internal/payments/**` vs `internal/pay/**`: prefixes `internal/payments` / `internal/pay` — `HasPrefix("internal/payments", "internal/pay"+"/")` is false, so no overlap. Correct.

Update `dist/flows/default.yaml` plan stage: `artifacts: [plan.md, touchset.json]`. Append the touchset instruction paragraph to `dist/packages/planner/prompt.md`.

- [ ] **Step 4: Run tests**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/touchset/ dist/
git commit -m "feat: planner emits touchset.json; conservative glob-overlap predicate"
```

---

### Task 5: Merge Marshal A — overlap prediction & sequencing

**Files:**
- Create: `internal/marshal/marshal.go`
- Modify: `internal/engine/engine.go` (hooks), `internal/core/event.go`
- Test: `internal/marshal/marshal_test.go`

**Interfaces:**
- ```go
  type Emit func(t core.EventType, issueID string, payload any)
  type Marshal struct{ ... }
  func New(emit Emit) *Marshal
  // PlanApproved registers an issue's touchset; if it overlaps any in-flight
  // earlier issue, the new issue is sequenced behind the earliest such issue
  // and a merge_sequenced event is emitted.
  func (m *Marshal) PlanApproved(issueID string, ts touchset.Set)
  // ReadyToMerge blocks until every issue this one is sequenced behind has
  // merged (or failed/aborted), then returns.
  func (m *Marshal) ReadyToMerge(ctx context.Context, issueID string) error
  // Merged/Aborted clear the issue from the sequencing graph and wake waiters.
  func (m *Marshal) Merged(issueID string)
  func (m *Marshal) Aborted(issueID string)
  // BlockedBehind reports how many issues are currently sequenced behind issueID
  // (transitively) — feeds decision blocking-cost.
  func (m *Marshal) BlockedBehind(issueID string) int
  ```
- Engine hooks: `Config.Marshal *marshal.Marshal` (satisfies the `Sequencer` interface from Task 1). After the plan stage's gate passes (detect: stage just completed AND a file `touchset.json` exists in the stage workdir), call `Marshal.PlanApproved` with the loaded set. Before the merge stage runs, call `Marshal.ReadyToMerge(ctx, id)` (emit `merge_sequenced`-wait visibility comes from Marshal itself). On `issue_completed` → `Marshal.Merged` happens in Task 6 (after real merge); on `StartIssue` error return → `Marshal.Aborted`.
- Event: `EvMergeSequenced EventType = "merge_sequenced"`.

- [ ] **Step 1: Write the failing test**

```go
// internal/marshal/marshal_test.go
package marshal

import (
	"context"
	"testing"
	"time"

	"github.com/wbushyeager/watchtower/internal/core"
	"github.com/wbushyeager/watchtower/internal/touchset"
)

func TestSequencingAndRelease(t *testing.T) {
	var events []string
	m := New(func(typ core.EventType, issue string, payload any) {
		events = append(events, string(typ)+":"+issue)
	})
	m.PlanApproved("GH-1", touchset.Set{Globs: []string{"internal/pay/**"}})
	m.PlanApproved("GH-2", touchset.Set{Globs: []string{"internal/pay/refund.go"}}) // overlaps GH-1
	m.PlanApproved("GH-3", touchset.Set{Globs: []string{"docs/**"}})                // independent

	if m.BlockedBehind("GH-1") != 1 || m.BlockedBehind("GH-3") != 0 {
		t.Fatalf("blocked: GH-1=%d GH-3=%d", m.BlockedBehind("GH-1"), m.BlockedBehind("GH-3"))
	}
	found := false
	for _, e := range events {
		if e == "merge_sequenced:GH-2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no merge_sequenced event: %v", events)
	}

	// GH-3 merges immediately; GH-2 waits for GH-1.
	if err := m.ReadyToMerge(context.Background(), "GH-3"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.ReadyToMerge(context.Background(), "GH-2") }()
	select {
	case <-done:
		t.Fatal("GH-2 should wait for GH-1")
	case <-time.After(50 * time.Millisecond):
	}
	m.Merged("GH-1")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("GH-2 never released")
	}
}

func TestAbortReleasesWaiters(t *testing.T) {
	m := New(func(core.EventType, string, any) {})
	m.PlanApproved("GH-1", touchset.Set{Globs: []string{"a/**"}})
	m.PlanApproved("GH-2", touchset.Set{Globs: []string{"a/b.go"}})
	done := make(chan error, 1)
	go func() { done <- m.ReadyToMerge(context.Background(), "GH-2") }()
	m.Aborted("GH-1")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("abort did not release waiter")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/marshal/ -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// internal/marshal/marshal.go
package marshal

import (
	"context"
	"sync"

	"github.com/wbushyeager/watchtower/internal/core"
	"github.com/wbushyeager/watchtower/internal/touchset"
)

type Emit func(t core.EventType, issueID string, payload any)

type entry struct {
	ts     touchset.Set
	behind string // issue this one waits for ("" = free)
	gone   chan struct{}
}

// Marshal sequences merges of issues whose plans touch overlapping files.
type Marshal struct {
	mu    sync.Mutex
	emit  Emit
	inFly map[string]*entry
	order []string
}

func New(emit Emit) *Marshal {
	return &Marshal{emit: emit, inFly: map[string]*entry{}}
}

func (m *Marshal) PlanApproved(issueID string, ts touchset.Set) {
	m.mu.Lock()
	e := &entry{ts: ts, gone: make(chan struct{})}
	for _, prior := range m.order {
		pe, ok := m.inFly[prior]
		if !ok {
			continue
		}
		if touchset.Overlap(ts, pe.ts) {
			e.behind = prior
			break
		}
	}
	m.inFly[issueID] = e
	m.order = append(m.order, issueID)
	behind := e.behind
	m.mu.Unlock()
	if behind != "" {
		m.emit(core.EvMergeSequenced, issueID, map[string]string{"behind": behind})
	}
}

func (m *Marshal) ReadyToMerge(ctx context.Context, issueID string) error {
	for {
		m.mu.Lock()
		e, ok := m.inFly[issueID]
		if !ok || e.behind == "" {
			m.mu.Unlock()
			return nil
		}
		pe, ok := m.inFly[e.behind]
		if !ok { // predecessor already cleared
			e.behind = ""
			m.mu.Unlock()
			return nil
		}
		gone := pe.gone
		m.mu.Unlock()
		select {
		case <-gone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *Marshal) clear(issueID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.inFly[issueID]; ok {
		close(e.gone)
		delete(m.inFly, issueID)
	}
	for i, id := range m.order {
		if id == issueID {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
}

func (m *Marshal) Merged(issueID string)  { m.clear(issueID) }
func (m *Marshal) Aborted(issueID string) { m.clear(issueID) }

func (m *Marshal) BlockedBehind(issueID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	frontier := []string{issueID}
	for len(frontier) > 0 {
		next := []string{}
		for id, e := range m.inFly {
			for _, f := range frontier {
				if e.behind == f {
					n++
					next = append(next, id)
				}
			}
		}
		frontier = next
	}
	return n
}
```

Engine hooks (in `runStage`, after the gate block succeeds, before `stage_completed` emit):

```go
	if e.cfg.Marshal != nil {
		if ts, err := touchset.Load(filepath.Join(e.stageWorkdir(is, st), "touchset.json")); err == nil {
			e.cfg.Marshal.PlanApproved(is.id, ts)
		}
	}
```

(Extract the workdir computation from `runStageOnce` into `func (e *Engine) stageWorkdir(is *issueState, st flow.Stage) string` so both call sites share it. The load silently no-ops for stages without a touchset — only the plan stage produces one.)

And at the top of `runStage` for the stage named by a new flow marker — add optional stage field `merge_barrier: true` (flow.Stage gains `MergeBarrier bool \`yaml:"merge_barrier"\``; set it on the merge stage in `dist/flows/default.yaml`):

```go
	if st.MergeBarrier && e.cfg.Marshal != nil {
		if err := e.cfg.Marshal.ReadyToMerge(ctx, is.id); err != nil {
			return err
		}
	}
```

In `StartIssue`: on error return path call `e.cfg.Marshal.Aborted(id)` (nil-guarded); the `Merged` call lands in Task 6.

Update the engine `Sequencer` field from Task 1 to the concrete `*marshal.Marshal` type (or keep the interface and assert both methods — keep the interface, now `interface{ BlockedBehind(string) int; PlanApproved(string, touchset.Set); ReadyToMerge(context.Context, string) error; Merged(string); Aborted(string) }`, declared in engine as `type Sequencer interface{...}`).

- [ ] **Step 4: Run the full suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/marshal/ internal/engine/ internal/core/ internal/flow/ dist/
git commit -m "feat: merge marshal sequences overlapping plans and feeds blocking cost"
```

---

### Task 6: Merge Marshal B — the merge train

**Files:**
- Create: `internal/marshal/train.go`
- Modify: `internal/engine/engine.go`, `internal/core/event.go`, `cmd/watchtower/main.go`, `dist/packages/conflict-resolver/{package.yaml,prompt.md}` (new package)
- Test: `internal/marshal/train_test.go`

**Interfaces:**
- ```go
  // Train lands issue branches on the default branch, one at a time.
  type Train struct {
      Repo    string        // main checkout
      TestCmd []string      // e.g. ["go","test","./..."]; empty = skip
      Resolve func(ctx context.Context, issueID, worktree string) error // conflict-repair dispatch; nil = no repair
  }
  // Land merges branch into the default branch with --no-ff, runs TestCmd,
  // and returns nil on success. On merge conflict it aborts the merge, calls
  // Resolve once, and retries once. On test failure it resets --hard to the
  // pre-merge ref and returns an error.
  func (tr *Train) Land(ctx context.Context, issueID, branch string) error
  ```
- Engine: `Config.Train *marshal.Train`. After `issue_completed`'s prerequisites (all stages done) and when a Train is configured: determine the issue branch (`git -C <wsPath> rev-parse --abbrev-ref HEAD` captured at workspace acquire time into `issueState.branch`), emit `merge_started`, call `Land`; success → emit `issue_merged` + `Marshal.Merged(id)`; failure → emit `merge_conflict` with the error, escalate an importance-1.0 decision ("Merge of GH-n failed after repair attempt: <err>. Retry, or leave branch for manual merge?" options `["retry","leave branch"]`) — retry loops `Land` once more; leave → issue still emits `issue_completed` but state notes the unmerged branch (Steward shows "done (unmerged)": payload field).
- Events: `EvMergeStarted "merge_started"`, `EvMergeConflict "merge_conflict"` (EvIssueMerged exists since Task 3).
- Conflict-resolver package: tools Bash,Read,Edit,Glob,Grep; prompt: works in the issue worktree, task text carries the branch and instructs `git rebase origin-default`, resolve conflicts faithfully to both intents, run tests, leave the branch rebased.
- `Train.Resolve` default wiring in the daemon: dispatch the `conflict-resolver` package via the runner in the issue's worktree (needs runner + packages — the daemon builds a closure).

- [ ] **Step 1: Write the failing test**

```go
// internal/marshal/train_test.go
package marshal

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return string(out)
}

func repoWithBranch(t *testing.T, conflicting bool) (string, string) {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "config", "user.email", "t@t")
	git(t, repo, "config", "user.name", "t")
	os.WriteFile(filepath.Join(repo, "f.txt"), []byte("base\n"), 0o644)
	git(t, repo, "add", "."); git(t, repo, "commit", "-qm", "base")
	git(t, repo, "checkout", "-qb", "issue/GH-1")
	os.WriteFile(filepath.Join(repo, "f.txt"), []byte("branch change\n"), 0o644)
	git(t, repo, "commit", "-aqm", "branch work")
	git(t, repo, "checkout", "-q", "main")
	if conflicting {
		os.WriteFile(filepath.Join(repo, "f.txt"), []byte("main change\n"), 0o644)
		git(t, repo, "commit", "-aqm", "main work")
	}
	return repo, "issue/GH-1"
}

func TestLandCleanMerge(t *testing.T) {
	repo, branch := repoWithBranch(t, false)
	tr := &Train{Repo: repo}
	if err := tr.Land(context.Background(), "GH-1", branch); err != nil {
		t.Fatal(err)
	}
	log := git(t, repo, "log", "--oneline", "main")
	if !contains(log, "branch work") {
		t.Fatalf("merge missing: %s", log)
	}
}

func TestLandConflictWithoutResolverFails(t *testing.T) {
	repo, branch := repoWithBranch(t, true)
	tr := &Train{Repo: repo}
	if err := tr.Land(context.Background(), "GH-1", branch); err == nil {
		t.Fatal("expected conflict error")
	}
	// repo must be left clean (merge aborted)
	if s := git(t, repo, "status", "--porcelain"); s != "" {
		t.Fatalf("dirty repo after abort: %q", s)
	}
}

func TestLandTestFailureRollsBack(t *testing.T) {
	repo, branch := repoWithBranch(t, false)
	tr := &Train{Repo: repo, TestCmd: []string{"false"}}
	pre := git(t, repo, "rev-parse", "main")
	if err := tr.Land(context.Background(), "GH-1", branch); err == nil {
		t.Fatal("expected test failure")
	}
	if post := git(t, repo, "rev-parse", "main"); post != pre {
		t.Fatalf("main moved despite failing tests: %s -> %s", pre, post)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || len(s) > 0 && (stringIndex(s, sub) >= 0)) }
func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

(Use `strings.Contains` instead of the helper — write the test with `strings` imported; the helper above is illustrative only. Final test code MUST use `strings.Contains`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/marshal/ -run Land -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// internal/marshal/train.go
package marshal

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Train lands issue branches on the repo's default branch, serially.
type Train struct {
	Repo    string
	TestCmd []string
	Resolve func(ctx context.Context, issueID, branch string) error
}

func (tr *Train) git(args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", tr.Repo}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (tr *Train) defaultBranch() (string, error) {
	out, err := tr.git("symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("default branch: %v: %s", err, out)
	}
	return out, nil
}

func (tr *Train) Land(ctx context.Context, issueID, branch string) error {
	def, err := tr.defaultBranch()
	if err != nil {
		return err
	}
	pre, err := tr.git("rev-parse", def)
	if err != nil {
		return fmt.Errorf("pre ref: %v", err)
	}
	attempt := func() error {
		if out, err := tr.git("merge", "--no-ff", "--no-edit", branch); err != nil {
			tr.git("merge", "--abort")
			return fmt.Errorf("merge conflict: %s", out)
		}
		if len(tr.TestCmd) > 0 {
			cmd := exec.CommandContext(ctx, tr.TestCmd[0], tr.TestCmd[1:]...)
			cmd.Dir = tr.Repo
			if out, err := cmd.CombinedOutput(); err != nil {
				tr.git("reset", "--hard", pre)
				return fmt.Errorf("tests failed after merge: %v: %s", err, truncate(string(out), 2000))
			}
		}
		return nil
	}
	err = attempt()
	if err == nil {
		return nil
	}
	if tr.Resolve == nil || !strings.Contains(err.Error(), "merge conflict") {
		return err
	}
	if rerr := tr.Resolve(ctx, issueID, branch); rerr != nil {
		return fmt.Errorf("%v (repair failed: %v)", err, rerr)
	}
	return attempt()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
```

Engine: capture branch at acquire (`issueState.branch` via `git -C path rev-parse --abbrev-ref HEAD` right after `Acquire`); after the stage loop, before emitting `issue_completed`:

```go
	if e.cfg.Train != nil && is.branch != "" {
		e.emit(core.EvMergeStarted, id, map[string]string{"branch": is.branch})
		if err := e.landWithEscalation(ctx, is); err != nil {
			e.emit(core.EvIssueCompleted, id, map[string]string{"merge": "left-unmerged", "branch": is.branch})
			if e.cfg.Marshal != nil {
				e.cfg.Marshal.Merged(is.id) // clear the sequencing slot either way
			}
			return nil
		}
		e.emit(core.EvIssueMerged, id, map[string]string{"branch": is.branch})
		if e.cfg.Marshal != nil {
			e.cfg.Marshal.Merged(is.id)
		}
	}
	e.emit(core.EvIssueCompleted, id, nil)
```

with:

```go
func (e *Engine) landWithEscalation(ctx context.Context, is *issueState) error {
	err := e.cfg.Train.Land(ctx, is.id, is.branch)
	for err != nil {
		e.emit(core.EvMergeConflict, is.id, map[string]string{"error": err.Error()})
		d := levers.Decision{
			Question:    fmt.Sprintf("Merge of %s failed: %v. Retry, or leave the branch for manual merge?", is.id, err),
			Options:     []string{"retry", "leave branch"},
			Recommended: 1,
			Importance:  1.0,
		}
		if e.escalate(is.id, "merge", d) != 0 {
			return err
		}
		err = e.cfg.Train.Land(ctx, is.id, is.branch)
	}
	return nil
}
```

Important ordering note: the worktree release defer (Plan 2) runs AFTER this merge code returns from `StartIssue` — the branch still exists after release (worktree removal keeps branches), so `Land` merging by branch name is safe. But `Land` runs while the worktree still holds the branch checked out — merging a branch that is checked out in a worktree is allowed (merging INTO it isn't). No change needed; note it in a comment.

Conflict-resolver package + daemon `Resolve` closure: package files as specced above; daemon builds

```go
	resolve := func(ctx context.Context, issueID, branch string) error {
		// Repair in a fresh worktree of the issue branch.
		wt, release, err := ws.Acquire(issueID + "-repair")
		if err != nil {
			return err
		}
		defer release()
		out, err := exec.Command("git", "-C", wt, "checkout", branch).CombinedOutput()
		if err != nil {
			return fmt.Errorf("checkout: %v: %s", err, out)
		}
		asks := make(chan runner.Ask) // repair runs headless; drain asks with recommended
		go func() {
			for a := range asks {
				a.Reply <- a.Decision.Recommended
			}
		}()
		res := <-run.Run(ctx, issueID, "conflict-repair", "conflict-resolver", wt, asks)
		return res.Err
	}
```

and sets `Train{Repo: *repo, TestCmd: splitTestCmd(*testCmd), Resolve: resolve}` behind a new `--test-cmd "go test ./..."` daemon flag (`strings.Fields` split; empty flag = no TestCmd). Train + Marshal are only constructed for `--runner claude` (fake mode keeps DataDir sandboxes; no repo).

- [ ] **Step 4: Run the full suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ cmd/ dist/
git commit -m "feat: merge train lands issue branches with tests, repair, and escalation"
```

---

### Task 7: Librarian — context injection + doc reconciliation

**Files:**
- Create: `internal/librarian/librarian.go`, `dist/packages/librarian/{package.yaml,prompt.md}`
- Modify: `internal/engine/engine.go`, `cmd/watchtower/main.go`, `internal/core/event.go`
- Test: `internal/librarian/librarian_test.go`

**Interfaces:**
- ```go
  type Librarian struct {
      MemoryDir string // <repo>/docs/watchtower; may not exist
  }
  // Context returns the injection block for a new issue: the concatenated
  // contents of every *.md in MemoryDir (sorted by name), each preceded by
  // "## <filename>". Empty string when the dir is missing or empty.
  func (l *Librarian) Context() (string, error)
  ```
- Engine: `Config.Librarian *librarian.Librarian` (nil = skip). In `runStageOnce`, the ISSUE.md write becomes: issue header + body, then (if Librarian returns content) `\n\n# Project memory (curated by the Librarian)\n\n` + content.
- Doc reconciliation: after a successful `Land` (Task 6's success path, before `issue_merged` is emitted), if a `librarian` agent package exists, dispatch it in the main repo (`workdir = Train.Repo`) with the drain-asks pattern; it reconciles `docs/` and the MemoryDir and commits. Emit `docs_reconciled` (`EvDocsReconciled EventType = "docs_reconciled"`) on success; on error, emit nothing fatal — log-only (doc debt must not block merges). Wire as `Config.Reconcile func(ctx context.Context, issueID string) error` built in the daemon (same closure shape as Resolve).
- Librarian package: tools Read,Write,Edit,Glob,Grep,Bash; prompt: "You are the Watchtower librarian, sole owner of docs/ and docs/watchtower/ (project memory) on the default branch. An issue just merged. Reconcile documentation: fold any doc drafts from the merge into a single coherent voice, resolve contradictions, update stale statements, and maintain docs/watchtower/*.md as curated memory files (one topic per file) that future issues receive as context. Commit your changes with message 'docs: librarian reconcile after <issue>'. Do not modify non-documentation code."

- [ ] **Step 1: Write the failing test**

```go
// internal/librarian/librarian_test.go
package librarian

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContextConcatenatesMemory(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "b-arch.md"), []byte("services: pay, cart"), 0o644)
	os.WriteFile(filepath.Join(dir, "a-conventions.md"), []byte("tabs not spaces"), 0o644)
	os.WriteFile(filepath.Join(dir, "ignore.txt"), []byte("no"), 0o644)
	l := &Librarian{MemoryDir: dir}
	got, err := l.Context()
	if err != nil {
		t.Fatal(err)
	}
	ia, ib := strings.Index(got, "a-conventions.md"), strings.Index(got, "b-arch.md")
	if ia < 0 || ib < 0 || ia > ib {
		t.Fatalf("order/content wrong:\n%s", got)
	}
	if strings.Contains(got, "ignore.txt") {
		t.Fatal("non-md file leaked")
	}
}

func TestContextMissingDirIsEmpty(t *testing.T) {
	l := &Librarian{MemoryDir: "/nonexistent/xyz"}
	got, err := l.Context()
	if err != nil || got != "" {
		t.Fatalf("want empty, got %q err %v", got, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/librarian/ -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// internal/librarian/librarian.go
package librarian

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Librarian curates project memory: a directory of markdown files injected
// into every new issue's context.
type Librarian struct {
	MemoryDir string
}

func (l *Librarian) Context() (string, error) {
	entries, err := os.ReadDir(l.MemoryDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		content, err := os.ReadFile(filepath.Join(l.MemoryDir, n))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", n, strings.TrimSpace(string(content)))
	}
	return strings.TrimSpace(b.String()), nil
}
```

Engine ISSUE.md write in `runStageOnce`:

```go
	issueMD := fmt.Sprintf("# %s: %s\n\n%s\n", is.id, is.title, is.body)
	if e.cfg.Librarian != nil {
		if mem, err := e.cfg.Librarian.Context(); err == nil && mem != "" {
			issueMD += "\n# Project memory (curated by the Librarian)\n\n" + mem + "\n"
		}
	}
```

Success path of the merge block (Task 6) gains, before `issue_merged`:

```go
		if e.cfg.Reconcile != nil {
			if err := e.cfg.Reconcile(ctx, is.id); err == nil {
				e.emit(core.EvDocsReconciled, is.id, nil)
			}
		}
```

Daemon: `Librarian: &librarian.Librarian{MemoryDir: filepath.Join(*repo, "docs", "watchtower")}` (claude mode only) and the `Reconcile` closure dispatching the `librarian` package with `workdir = *repo` and the drain-asks pattern. Write the librarian package files.

- [ ] **Step 4: Run the full suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ cmd/ dist/
git commit -m "feat: librarian injects curated memory and reconciles docs post-merge"
```

---

### Task 8: Reviewer spec-conformance + two-issue smoke

**Files:**
- Modify: `dist/packages/reviewer/prompt.md`
- Test: stub smoke (scripted below) + optional real smoke.

**Interfaces:** none new.

- [ ] **Step 1: Fix the reviewer prompt**

Append to `dist/packages/reviewer/prompt.md`:

```markdown

Spec conformance is part of your review: read spec.md (in the issue
artifacts directory or worktree) and verify the diff actually implements
what it specifies — including decided requirements like argument handling.
List every deviation explicitly; fix in-scope deviations, and file
{"watchtower_proposal": {"title": "...", "body": "..."}} for out-of-scope
ones. A diff that silently narrows the spec is a defect, not a style issue.
```

- [ ] **Step 2: Stub smoke — two overlapping issues sequence correctly**

```bash
cd ~/watchtower && go build -o /tmp/gh-bin/watchtower ./cmd/watchtower
rm -rf /tmp/gh-smoke3 && mkdir -p /tmp/gh-smoke3/flows
cp dist/flows/default.yaml /tmp/gh-smoke3/flows/
/tmp/gh-bin/watchtower daemon --runner fake --data /tmp/gh-smoke3 --flows /tmp/gh-smoke3/flows &
sleep 1
/tmp/gh-bin/watchtower new --data /tmp/gh-smoke3 --title "issue A" --preset yolo
/tmp/gh-bin/watchtower new --data /tmp/gh-smoke3 --title "issue B" --preset yolo
# answer gates for both issues as they appear (watchtower decisions / answer <id> 0)
# until both reach issue_completed:
/tmp/gh-bin/watchtower tail --data /tmp/gh-smoke3 | grep -E 'issue_completed|merge_sequenced'
/tmp/gh-bin/watchtower issues --data /tmp/gh-smoke3
/tmp/gh-bin/watchtower proposals --data /tmp/gh-smoke3
kill %1
```

Expected: both issues complete; `watchtower issues` shows both `done`; since the fake runner writes identical touchsets only if the flow's plan stage declares touchset.json — `fakeForFlows` auto-writes every declared artifact, so both issues emit identical `touchset.json` ("fake" content — `touchset.Load` fails on non-JSON and the hook silently no-ops). To actually exercise sequencing in the smoke, either patch `fakeForFlows` to write `{"globs":["src/**"]}` for `touchset.json` specifically (do this — it is a 5-line change in `fakeForFlows`: if the artifact name is `touchset.json`, write that JSON literal instead of "fake"), then expect ONE `merge_sequenced` event for the second issue. Verify `watchtower decisions` during the run shows the second issue's gate decisions ordered ahead when it blocks others (blocking-cost ordering).

- [ ] **Step 3: Run the full suite once more**

Run: `go test ./... -race && go build ./...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add dist/ cmd/
git commit -m "feat: reviewer checks spec conformance; fake runner emits real touchsets"
```

---

## Self-review notes

- **Spec coverage:** Merge Marshal predict+sequence (T4, T5) and merge train with conflict-repair dispatch + human escalation (T6) — replaces the "predict + sequence" spec decision and the Plan 2 placeholder merge gate (the flow's merge stage keeps its `approve_artifact` gate as the human authority; the Train is the mechanism that executes after approval). Librarian owns docs+memory and injects context (T7). Issue Steward syncs the issues table and runs the proposal tray (T2, T3). Decision queue ordering by blocking cost + persisted audit trail of auto-resolutions (T1). Smoke lesson folded in: reviewer spec-conformance (T8).
- **Deliberate v1 simplifications, stated:** Marshal sequencing is in-memory (daemon restart forgets pending sequence constraints — acceptable while flows are also not restart-resumable; both land together later); overlap prediction is prefix-conservative (false positives sequence unnecessarily, never false negatives); Librarian context is deterministic concatenation, not LLM curation (the reconcile agent curates the files themselves); `EvDocsReconciled` failure is non-blocking by design.
- **Type consistency check:** `runner.Proposal` shared codec↔fake↔engine ✓; `Sequencer` interface satisfied by `*marshal.Marshal` ✓; `DecisionRow.Stage` stored in the legacy `lever` column (comment required) ✓; `flow.Stage.MergeBarrier` new yaml field defaults false — existing flows unaffected ✓; engine emit-with-observers keeps event ordering (observers called after Append assigns Seq) ✓.
