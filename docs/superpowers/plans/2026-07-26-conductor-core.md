# Conductor Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The Watchtower daemon: an event-sourced flow runner that takes issues through a YAML-defined pipeline with autonomy levers, a decision queue, and a slot pool — testable end-to-end with a fake runner and a CLI client.

**Architecture:** Single Go module. The Conductor owns SQLite state and an append-only event log; a flow engine walks stage definitions and spawns runners through a `Runner` interface (only `FakeRunner` in this plan); clients talk JSONL over a Unix socket. Every state change is an event; clients rebuild state by replay.

**Tech Stack:** Go 1.22+, `modernc.org/sqlite` (pure-Go, no cgo), `gopkg.in/yaml.v3`, stdlib `net` for the socket. Tests use the standard library only.

## Global Constraints

- Module path: `github.com/weston6142/watchtower` (rename later is fine; keep consistent).
- Pure Go — no cgo (SQLite via `modernc.org/sqlite`).
- Every state mutation MUST be expressed as an `Event` appended to the log before side effects propagate; SQLite tables are projections.
- Events and protocol messages are JSON with `snake_case` fields.
- Levers are exactly `yolo | regular | strict` (spec: "Autonomy levers").
- Gates are exactly `approve_artifact | decision_queue | auto` (spec: "Flows as data").
- Stage workspace values: `none | worktree | readonly`.
- Auto-resolved decisions are still recorded (spec: "Auto-resolved decisions are still logged as events").
- Multi-agent stages: one `stage_runs` row per agent; stage completes per `all`/`any` rule (spec: "Flows as data").
- No TUI in this plan. The only client is `watchtower` CLI subcommands.

---

### Task 1: Module scaffold + Event types

**Files:**
- Create: `go.mod`, `internal/core/event.go`
- Test: `internal/core/event_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Event struct { ID int64; Seq int64; Type EventType; IssueID string; Payload json.RawMessage; At time.Time }`, `type EventType string` with constants `EvIssueCreated, EvStageStarted, EvStageCompleted, EvStageFailed, EvDecisionRequired, EvDecisionAnswered, EvDecisionAutoResolved, EvSlotAcquired, EvSlotQueued, EvSlotReleased, EvProposalFiled, EvArtifactProduced`; `func NewEvent(t EventType, issueID string, payload any) (Event, error)`.

- [ ] **Step 1: Init module and write the failing test**

```bash
cd ~/watchtower && go mod init github.com/weston6142/watchtower
```

```go
// internal/core/event_test.go
package core

import (
	"encoding/json"
	"testing"
)

func TestNewEventMarshalsPayloadSnakeCase(t *testing.T) {
	ev, err := NewEvent(EvIssueCreated, "GH-1", map[string]string{"title": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != EvIssueCreated || ev.IssueID != "GH-1" {
		t.Fatalf("bad event: %+v", ev)
	}
	var p map[string]string
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p["title"] != "hello" {
		t.Fatalf("payload lost: %v", p)
	}
	if ev.At.IsZero() {
		t.Fatal("At not set")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/core/ -run TestNewEvent -v`
Expected: FAIL (compile error: undefined `NewEvent`, `EvIssueCreated`).

- [ ] **Step 3: Write minimal implementation**

```go
// internal/core/event.go
package core

import (
	"encoding/json"
	"time"
)

type EventType string

const (
	EvIssueCreated         EventType = "issue_created"
	EvStageStarted         EventType = "stage_started"
	EvStageCompleted       EventType = "stage_completed"
	EvStageFailed          EventType = "stage_failed"
	EvDecisionRequired     EventType = "decision_required"
	EvDecisionAnswered     EventType = "decision_answered"
	EvDecisionAutoResolved EventType = "decision_auto_resolved"
	EvSlotAcquired         EventType = "slot_acquired"
	EvSlotQueued           EventType = "slot_queued"
	EvSlotReleased         EventType = "slot_released"
	EvProposalFiled        EventType = "proposal_filed"
	EvArtifactProduced     EventType = "artifact_produced"
)

type Event struct {
	ID      int64           `json:"id"`
	Seq     int64           `json:"seq"`
	Type    EventType       `json:"type"`
	IssueID string          `json:"issue_id"`
	Payload json.RawMessage `json:"payload"`
	At      time.Time       `json:"at"`
}

func NewEvent(t EventType, issueID string, payload any) (Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	return Event{Type: t, IssueID: issueID, Payload: raw, At: time.Now().UTC()}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/core/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go.mod internal/core/
git commit -m "feat: module scaffold and core event types"
```

---

### Task 2: SQLite store with append-only event log

**Files:**
- Create: `internal/store/store.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: `core.Event`, `core.EventType` from Task 1.
- Produces: `type Store struct{...}`; `func Open(path string) (*Store, error)` (use `:memory:` in tests — pass path `"file:test?mode=memory&cache=shared"`); `func (s *Store) Append(ev core.Event) (core.Event, error)` (assigns `ID`, monotonically increasing `Seq`, persists); `func (s *Store) EventsSince(seq int64) ([]core.Event, error)`; `func (s *Store) Close() error`. Schema created on Open: tables `events(id INTEGER PRIMARY KEY AUTOINCREMENT, seq INTEGER UNIQUE, type TEXT, issue_id TEXT, payload TEXT, at TEXT)`, `issues(id TEXT PRIMARY KEY, title TEXT, body TEXT, state TEXT, flow TEXT, levers TEXT, priority INTEGER, links TEXT)`, `stage_runs(id INTEGER PRIMARY KEY AUTOINCREMENT, issue_id TEXT, stage TEXT, agent TEXT, session_id TEXT, worktree TEXT, artifacts TEXT, status TEXT, tokens INTEGER)`, `decisions(id INTEGER PRIMARY KEY AUTOINCREMENT, issue_id TEXT, question TEXT, options TEXT, recommended INTEGER, evidence TEXT, lever TEXT, status TEXT, answer TEXT, answered_by TEXT, blocking_cost INTEGER, created_at TEXT)`, `proposals(id INTEGER PRIMARY KEY AUTOINCREMENT, issue_id TEXT, title TEXT, body TEXT, status TEXT)`.

- [ ] **Step 1: Write the failing test**

```go
// internal/store/store_test.go
package store

import (
	"testing"

	"github.com/weston6142/watchtower/internal/core"
)

func TestAppendAssignsSeqAndReplays(t *testing.T) {
	s, err := Open("file:t1?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	e1, _ := core.NewEvent(core.EvIssueCreated, "GH-1", map[string]string{"title": "a"})
	e2, _ := core.NewEvent(core.EvStageStarted, "GH-1", map[string]string{"stage": "brainstorm"})
	e1, err = s.Append(e1)
	if err != nil {
		t.Fatal(err)
	}
	e2, _ = s.Append(e2)
	if e2.Seq != e1.Seq+1 {
		t.Fatalf("seq not monotonic: %d then %d", e1.Seq, e2.Seq)
	}
	got, err := s.EventsSince(e1.Seq) // strictly after e1
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Type != core.EvStageStarted {
		t.Fatalf("replay wrong: %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -v`
Expected: FAIL (undefined `Open`).

- [ ] **Step 3: Write minimal implementation**

```go
// internal/store/store.go
package store

import (
	"database/sql"
	"encoding/json"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/weston6142/watchtower/internal/core"
)

const schema = `
CREATE TABLE IF NOT EXISTS events(
  id INTEGER PRIMARY KEY AUTOINCREMENT, seq INTEGER UNIQUE,
  type TEXT, issue_id TEXT, payload TEXT, at TEXT);
CREATE TABLE IF NOT EXISTS issues(
  id TEXT PRIMARY KEY, title TEXT, body TEXT, state TEXT,
  flow TEXT, levers TEXT, priority INTEGER, links TEXT);
CREATE TABLE IF NOT EXISTS stage_runs(
  id INTEGER PRIMARY KEY AUTOINCREMENT, issue_id TEXT, stage TEXT, agent TEXT,
  session_id TEXT, worktree TEXT, artifacts TEXT, status TEXT, tokens INTEGER);
CREATE TABLE IF NOT EXISTS decisions(
  id INTEGER PRIMARY KEY AUTOINCREMENT, issue_id TEXT, question TEXT, options TEXT,
  recommended INTEGER, evidence TEXT, lever TEXT, status TEXT, answer TEXT,
  answered_by TEXT, blocking_cost INTEGER, created_at TEXT);
CREATE TABLE IF NOT EXISTS proposals(
  id INTEGER PRIMARY KEY AUTOINCREMENT, issue_id TEXT, title TEXT, body TEXT, status TEXT);
`

type Store struct {
	db  *sql.DB
	mu  sync.Mutex
	seq int64
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	var max sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&max); err != nil {
		return nil, err
	}
	return &Store{db: db, seq: max.Int64}, nil
}

func (s *Store) Append(ev core.Event) (core.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	ev.Seq = s.seq
	res, err := s.db.Exec(
		`INSERT INTO events(seq,type,issue_id,payload,at) VALUES(?,?,?,?,?)`,
		ev.Seq, string(ev.Type), ev.IssueID, string(ev.Payload), ev.At.Format(time.RFC3339Nano))
	if err != nil {
		return ev, err
	}
	ev.ID, _ = res.LastInsertId()
	return ev, nil
}

func (s *Store) EventsSince(seq int64) ([]core.Event, error) {
	rows, err := s.db.Query(
		`SELECT id,seq,type,issue_id,payload,at FROM events WHERE seq > ? ORDER BY seq`, seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Event
	for rows.Next() {
		var ev core.Event
		var typ, payload, at string
		if err := rows.Scan(&ev.ID, &ev.Seq, &typ, &ev.IssueID, &payload, &at); err != nil {
			return nil, err
		}
		ev.Type = core.EventType(typ)
		ev.Payload = json.RawMessage(payload)
		ev.At, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *Store) Close() error { return s.db.Close() }
```

```bash
go get modernc.org/sqlite@latest
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/store/
git commit -m "feat: sqlite store with append-only event log"
```

---

### Task 3: Flow definition loading (YAML)

**Files:**
- Create: `internal/flow/flow.go`, `internal/flow/testdata/default.yaml`
- Test: `internal/flow/flow_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  ```go
  type Lever string // "yolo" | "regular" | "strict"
  type Gate string  // "approve_artifact" | "decision_queue" | "auto"
  type AgentRef struct { Package string `yaml:"package"`; Model string `yaml:"model"` }
  type Stage struct {
      Name       string     `yaml:"name"`
      Agents     []AgentRef `yaml:"agents"`
      Parallel   bool       `yaml:"parallel"`
      Completion string     `yaml:"completion"` // "all" (default) | "any"
      Workspace  string     `yaml:"workspace"`  // none|worktree|readonly
      Gate       Gate       `yaml:"gate"`
      Artifacts  []string   `yaml:"artifacts"`
      Retries    int        `yaml:"retries"`
      HeavySlot  bool       `yaml:"heavy_slot"`
  }
  type Flow struct { Name string `yaml:"name"`; Stages []Stage `yaml:"stages"` }
  func Load(path string) (Flow, error) // parses + validates
  ```
- Validation rules: at least one stage; every stage has ≥1 agent; gate/workspace/completion values in their enums (empty completion → "all"; empty workspace → "none"); stage names unique.

- [ ] **Step 1: Write testdata and the failing test**

```yaml
# internal/flow/testdata/default.yaml
name: default
stages:
  - name: brainstorm
    agents: [{package: brainstorm}]
    gate: decision_queue
    artifacts: []
  - name: spec
    agents: [{package: spec-writer}]
    gate: approve_artifact
    artifacts: [spec.md]
  - name: execute
    agents: [{package: executor}]
    workspace: worktree
    gate: auto
    heavy_slot: true
    retries: 1
    artifacts: [diff]
  - name: review
    agents: [{package: clean-code-reviewer}, {package: reviewer}, {package: doc-writer}]
    parallel: true
    completion: all
    workspace: worktree
    gate: auto
    artifacts: [review.md, docs]
```

```go
// internal/flow/flow_test.go
package flow

import "testing"

func TestLoadValidFlow(t *testing.T) {
	f, err := Load("testdata/default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "default" || len(f.Stages) != 4 {
		t.Fatalf("bad flow: %+v", f)
	}
	rev := f.Stages[3]
	if len(rev.Agents) != 3 || !rev.Parallel || rev.Completion != "all" {
		t.Fatalf("multi-agent stage wrong: %+v", rev)
	}
	if f.Stages[0].Workspace != "none" {
		t.Fatalf("workspace default not applied: %q", f.Stages[0].Workspace)
	}
}

func TestLoadRejectsBadGate(t *testing.T) {
	if _, err := loadBytes([]byte("name: x\nstages:\n  - name: a\n    agents: [{package: p}]\n    gate: bogus\n")); err == nil {
		t.Fatal("expected error for bad gate")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/flow/ -v`
Expected: FAIL (undefined `Load`, `loadBytes`).

- [ ] **Step 3: Write minimal implementation**

```go
// internal/flow/flow.go
package flow

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Lever string
type Gate string

const (
	LeverYolo    Lever = "yolo"
	LeverRegular Lever = "regular"
	LeverStrict  Lever = "strict"

	GateApproveArtifact Gate = "approve_artifact"
	GateDecisionQueue   Gate = "decision_queue"
	GateAuto            Gate = "auto"
)

type AgentRef struct {
	Package string `yaml:"package"`
	Model   string `yaml:"model"`
}

type Stage struct {
	Name       string     `yaml:"name"`
	Agents     []AgentRef `yaml:"agents"`
	Parallel   bool       `yaml:"parallel"`
	Completion string     `yaml:"completion"`
	Workspace  string     `yaml:"workspace"`
	Gate       Gate       `yaml:"gate"`
	Artifacts  []string   `yaml:"artifacts"`
	Retries    int        `yaml:"retries"`
	HeavySlot  bool       `yaml:"heavy_slot"`
}

type Flow struct {
	Name   string  `yaml:"name"`
	Stages []Stage `yaml:"stages"`
}

func Load(path string) (Flow, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Flow{}, err
	}
	return loadBytes(b)
}

func loadBytes(b []byte) (Flow, error) {
	var f Flow
	if err := yaml.Unmarshal(b, &f); err != nil {
		return Flow{}, err
	}
	if len(f.Stages) == 0 {
		return Flow{}, fmt.Errorf("flow %q has no stages", f.Name)
	}
	seen := map[string]bool{}
	for i := range f.Stages {
		st := &f.Stages[i]
		if seen[st.Name] {
			return Flow{}, fmt.Errorf("duplicate stage %q", st.Name)
		}
		seen[st.Name] = true
		if len(st.Agents) == 0 {
			return Flow{}, fmt.Errorf("stage %q has no agents", st.Name)
		}
		if st.Completion == "" {
			st.Completion = "all"
		}
		if st.Completion != "all" && st.Completion != "any" {
			return Flow{}, fmt.Errorf("stage %q bad completion %q", st.Name, st.Completion)
		}
		if st.Workspace == "" {
			st.Workspace = "none"
		}
		switch st.Workspace {
		case "none", "worktree", "readonly":
		default:
			return Flow{}, fmt.Errorf("stage %q bad workspace %q", st.Name, st.Workspace)
		}
		switch st.Gate {
		case GateApproveArtifact, GateDecisionQueue, GateAuto:
		default:
			return Flow{}, fmt.Errorf("stage %q bad gate %q", st.Name, st.Gate)
		}
	}
	return f, nil
}
```

```bash
go get gopkg.in/yaml.v3@latest
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/flow/ -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/flow/
git commit -m "feat: flow definition loading and validation"
```

---

### Task 4: Lever matrix + escalation routing

**Files:**
- Create: `internal/levers/levers.go`
- Test: `internal/levers/levers_test.go`

**Interfaces:**
- Consumes: `flow.Lever` constants from Task 3.
- Produces:
  ```go
  type Matrix map[string]flow.Lever            // stage name -> lever
  func Preset(f flow.Flow, l flow.Lever) Matrix // fills all stages with l
  type Decision struct { Question string; Options []string; Recommended int; Importance float64; Paths []string }
  type Rules struct { AlwaysEscalate []string /* glob patterns */ }
  // Route returns true if the decision must go to the human queue.
  func Route(d Decision, lever flow.Lever, rules Rules) bool
  ```
- Routing semantics (spec "Autonomy levers"): built-in floor is expressed as `Importance >= 1.0` (runners tag floor categories with importance 1.0); user rules match `d.Paths` against `rules.AlwaysEscalate` globs (use `path.Match` per path segment-free pattern, e.g. `payments/**` matched via `strings.HasPrefix` when pattern ends in `/**`, else `path.Match`); otherwise thresholds: `strict` → always escalate; `regular` → escalate when `Importance >= 0.5`; `yolo` → escalate when `Importance >= 0.9`.

- [ ] **Step 1: Write the failing test**

```go
// internal/levers/levers_test.go
package levers

import (
	"testing"

	"github.com/weston6142/watchtower/internal/flow"
)

func TestRouteMatrix(t *testing.T) {
	cases := []struct {
		name  string
		d     Decision
		lever flow.Lever
		rules Rules
		want  bool
	}{
		{"strict always asks", Decision{Importance: 0.1}, flow.LeverStrict, Rules{}, true},
		{"yolo skips minor", Decision{Importance: 0.5}, flow.LeverYolo, Rules{}, false},
		{"yolo floor still asks", Decision{Importance: 1.0}, flow.LeverYolo, Rules{}, true},
		{"regular mid asks", Decision{Importance: 0.6}, flow.LeverRegular, Rules{}, true},
		{"regular minor skips", Decision{Importance: 0.2}, flow.LeverRegular, Rules{}, false},
		{"user rule overrides yolo", Decision{Importance: 0.1, Paths: []string{"payments/charge.go"}},
			flow.LeverYolo, Rules{AlwaysEscalate: []string{"payments/**"}}, true},
	}
	for _, c := range cases {
		if got := Route(c.d, c.lever, c.rules); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestPresetFillsAllStages(t *testing.T) {
	f := flow.Flow{Stages: []flow.Stage{{Name: "a"}, {Name: "b"}}}
	m := Preset(f, flow.LeverYolo)
	if m["a"] != flow.LeverYolo || m["b"] != flow.LeverYolo {
		t.Fatalf("preset wrong: %v", m)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/levers/ -v`
Expected: FAIL (undefined symbols).

- [ ] **Step 3: Write minimal implementation**

```go
// internal/levers/levers.go
package levers

import (
	"path"
	"strings"

	"github.com/weston6142/watchtower/internal/flow"
)

type Matrix map[string]flow.Lever

func Preset(f flow.Flow, l flow.Lever) Matrix {
	m := Matrix{}
	for _, st := range f.Stages {
		m[st.Name] = l
	}
	return m
}

type Decision struct {
	Question    string
	Options     []string
	Recommended int
	Importance  float64
	Paths       []string
}

type Rules struct {
	AlwaysEscalate []string
}

func matches(pattern, p string) bool {
	if strings.HasSuffix(pattern, "/**") {
		return strings.HasPrefix(p, strings.TrimSuffix(pattern, "**"))
	}
	ok, _ := path.Match(pattern, p)
	return ok
}

func Route(d Decision, lever flow.Lever, rules Rules) bool {
	if d.Importance >= 1.0 {
		return true // built-in floor
	}
	for _, pat := range rules.AlwaysEscalate {
		for _, p := range d.Paths {
			if matches(pat, p) {
				return true
			}
		}
	}
	switch lever {
	case flow.LeverStrict:
		return true
	case flow.LeverRegular:
		return d.Importance >= 0.5
	default: // yolo
		return d.Importance >= 0.9
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/levers/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/levers/
git commit -m "feat: lever matrix presets and escalation routing"
```

---

### Task 5: Runner interface + FakeRunner

**Files:**
- Create: `internal/runner/runner.go`, `internal/runner/fake.go`
- Test: `internal/runner/fake_test.go`

**Interfaces:**
- Consumes: `levers.Decision` from Task 4.
- Produces:
  ```go
  // runner.go
  type Ask struct { Decision levers.Decision; Reply chan int } // engine sends chosen option index
  type Result struct { Artifacts map[string]string; Tokens int; Err error } // artifact name -> disk path
  type Runner interface {
      // Run executes one agent for one stage. It may send zero or more Asks
      // on the asks channel and MUST close done exactly once with the Result.
      Run(ctx context.Context, issueID, stage, agentPkg, workdir string,
          asks chan<- Ask) <-chan Result
  }
  // fake.go
  type Script struct { Asks []levers.Decision; Artifacts map[string]string; Tokens int; Fail bool }
  type FakeRunner struct { Scripts map[string]Script } // key: stage+"/"+agentPkg
  ```
  `FakeRunner.Run` replays the script: emits each Ask (waits for reply), then produces artifacts (writes literal files named by the artifact into `workdir` with content `"fake"`; the Result maps artifact → path), or a failed Result if `Fail`.

- [ ] **Step 1: Write the failing test**

```go
// internal/runner/fake_test.go
package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/weston6142/watchtower/internal/levers"
)

func TestFakeRunnerAsksThenProduces(t *testing.T) {
	dir := t.TempDir()
	fr := &FakeRunner{Scripts: map[string]Script{
		"spec/spec-writer": {
			Asks:      []levers.Decision{{Question: "REST or GraphQL?", Options: []string{"REST", "GraphQL"}, Recommended: 0, Importance: 0.6}},
			Artifacts: map[string]string{"spec.md": ""},
			Tokens:    42,
		},
	}}
	asks := make(chan Ask, 1)
	done := fr.Run(context.Background(), "GH-1", "spec", "spec-writer", dir, asks)

	a := <-asks
	if a.Decision.Question != "REST or GraphQL?" {
		t.Fatalf("wrong ask: %+v", a.Decision)
	}
	a.Reply <- 0

	res := <-done
	if res.Err != nil || res.Tokens != 42 {
		t.Fatalf("bad result: %+v", res)
	}
	p := res.Artifacts["spec.md"]
	if p != filepath.Join(dir, "spec.md") {
		t.Fatalf("artifact path wrong: %q", p)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("artifact not written: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/runner/ -v`
Expected: FAIL (undefined symbols).

- [ ] **Step 3: Write minimal implementation**

```go
// internal/runner/runner.go
package runner

import (
	"context"

	"github.com/weston6142/watchtower/internal/levers"
)

type Ask struct {
	Decision levers.Decision
	Reply    chan int
}

type Result struct {
	Artifacts map[string]string
	Tokens    int
	Err       error
}

type Runner interface {
	Run(ctx context.Context, issueID, stage, agentPkg, workdir string,
		asks chan<- Ask) <-chan Result
}
```

```go
// internal/runner/fake.go
package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/weston6142/watchtower/internal/levers"
)

type Script struct {
	Asks      []levers.Decision
	Artifacts map[string]string
	Tokens    int
	Fail      bool
}

type FakeRunner struct {
	Scripts map[string]Script
}

func (f *FakeRunner) Run(ctx context.Context, issueID, stage, agentPkg, workdir string,
	asks chan<- Ask) <-chan Result {
	done := make(chan Result, 1)
	go func() {
		sc, ok := f.Scripts[stage+"/"+agentPkg]
		if !ok {
			done <- Result{Err: fmt.Errorf("no script for %s/%s", stage, agentPkg)}
			return
		}
		for _, d := range sc.Asks {
			reply := make(chan int, 1)
			select {
			case asks <- Ask{Decision: d, Reply: reply}:
			case <-ctx.Done():
				done <- Result{Err: ctx.Err()}
				return
			}
			select {
			case <-reply:
			case <-ctx.Done():
				done <- Result{Err: ctx.Err()}
				return
			}
		}
		if sc.Fail {
			done <- Result{Err: fmt.Errorf("scripted failure %s/%s", stage, agentPkg)}
			return
		}
		out := map[string]string{}
		for name := range sc.Artifacts {
			p := filepath.Join(workdir, name)
			if err := os.WriteFile(p, []byte("fake"), 0o644); err != nil {
				done <- Result{Err: err}
				return
			}
			out[name] = p
		}
		done <- Result{Artifacts: out, Tokens: sc.Tokens}
	}()
	return done
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/runner/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/runner/
git commit -m "feat: runner interface and scripted fake runner"
```

---

### Task 6: Slot pool

**Files:**
- Create: `internal/slots/slots.go`
- Test: `internal/slots/slots_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  ```go
  type Pool struct{...}
  func NewPool(n int) *Pool
  // Acquire blocks until a slot frees (or ctx cancels). Higher priority
  // waiters acquire first; ties by FIFO. Returns a release func.
  func (p *Pool) Acquire(ctx context.Context, issueID string, priority int) (release func(), err error)
  func (p *Pool) Snapshot() (held []string, queued []string) // issue IDs
  ```

- [ ] **Step 1: Write the failing test**

```go
// internal/slots/slots_test.go
package slots

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestPriorityOrderAndSnapshot(t *testing.T) {
	p := NewPool(1)
	rel1, err := p.Acquire(context.Background(), "GH-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	grab := func(id string, prio int) {
		defer wg.Done()
		rel, err := p.Acquire(context.Background(), id, prio)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		order = append(order, id)
		mu.Unlock()
		rel()
	}
	wg.Add(2)
	go grab("GH-low", 0)
	time.Sleep(20 * time.Millisecond) // low enqueues first
	go grab("GH-high", 5)
	time.Sleep(20 * time.Millisecond)

	held, queued := p.Snapshot()
	if len(held) != 1 || held[0] != "GH-1" || len(queued) != 2 {
		t.Fatalf("snapshot wrong: held=%v queued=%v", held, queued)
	}

	rel1()
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if order[0] != "GH-high" {
		t.Fatalf("priority ignored: %v", order)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/slots/ -v`
Expected: FAIL (undefined `NewPool`).

- [ ] **Step 3: Write minimal implementation**

```go
// internal/slots/slots.go
package slots

import (
	"context"
	"sort"
	"sync"
)

type waiter struct {
	issueID  string
	priority int
	order    int64
	ready    chan struct{}
}

type Pool struct {
	mu      sync.Mutex
	free    int
	held    map[string]int // issueID -> count held
	queue   []*waiter
	counter int64
}

func NewPool(n int) *Pool {
	return &Pool{free: n, held: map[string]int{}}
}

func (p *Pool) Acquire(ctx context.Context, issueID string, priority int) (func(), error) {
	p.mu.Lock()
	if p.free > 0 && len(p.queue) == 0 {
		p.free--
		p.held[issueID]++
		p.mu.Unlock()
		return p.releaseFunc(issueID), nil
	}
	w := &waiter{issueID: issueID, priority: priority, order: p.counter, ready: make(chan struct{})}
	p.counter++
	p.queue = append(p.queue, w)
	p.sortQueue()
	p.mu.Unlock()

	select {
	case <-w.ready:
		return p.releaseFunc(issueID), nil
	case <-ctx.Done():
		p.mu.Lock()
		for i, q := range p.queue {
			if q == w {
				p.queue = append(p.queue[:i], p.queue[i+1:]...)
				break
			}
		}
		p.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (p *Pool) releaseFunc(issueID string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.held[issueID]--
			if p.held[issueID] <= 0 {
				delete(p.held, issueID)
			}
			if len(p.queue) > 0 {
				w := p.queue[0]
				p.queue = p.queue[1:]
				p.held[w.issueID]++
				close(w.ready)
			} else {
				p.free++
			}
		})
	}
}

func (p *Pool) sortQueue() {
	sort.SliceStable(p.queue, func(i, j int) bool {
		if p.queue[i].priority != p.queue[j].priority {
			return p.queue[i].priority > p.queue[j].priority
		}
		return p.queue[i].order < p.queue[j].order
	})
}

func (p *Pool) Snapshot() (held []string, queued []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id := range p.held {
		held = append(held, id)
	}
	sort.Strings(held)
	for _, w := range p.queue {
		queued = append(queued, w.issueID)
	}
	return
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/slots/ -race -v`
Expected: PASS (run with `-race`; this package is concurrency-critical).

- [ ] **Step 5: Commit**

```bash
git add internal/slots/
git commit -m "feat: priority slot pool"
```

---

### Task 7: Engine — issue lifecycle through stages

**Files:**
- Create: `internal/engine/engine.go`
- Test: `internal/engine/engine_test.go`

**Interfaces:**
- Consumes: `store.Store` (Task 2), `flow.Flow` (Task 3), `levers.Matrix`, `levers.Route`, `levers.Rules` (Task 4), `runner.Runner`, `runner.Ask` (Task 5), `slots.Pool` (Task 6), `core` events (Task 1).
- Produces:
  ```go
  type Engine struct{...}
  type Config struct {
      Store   *store.Store
      Runner  runner.Runner
      Pool    *slots.Pool
      Flows   map[string]flow.Flow
      Rules   levers.Rules
      DataDir string // per-issue artifact dirs live under here
  }
  func New(cfg Config) *Engine
  func (e *Engine) CreateIssue(title, body, flowName string, m levers.Matrix, priority int) (string, error) // returns issue ID "GH-<n>", emits EvIssueCreated
  func (e *Engine) StartIssue(ctx context.Context, id string) error // runs all stages; blocks until done/failed
  // Human interaction:
  func (e *Engine) PendingDecisions() []PendingDecision
  func (e *Engine) Answer(decisionID int64, option int) error
  type PendingDecision struct { ID int64; IssueID string; Stage string; D levers.Decision }
  ```
- Behavior per stage: acquire slot if `HeavySlot` (emit `slot_queued` before blocking, `slot_acquired` after, `slot_released` on release); emit `stage_started`; run all stage agents (concurrently if `Parallel`, else in order); for each runner Ask, build `levers.Decision`, call `levers.Route` with the issue's matrix lever for this stage — if false, reply with `Recommended` and emit `decision_auto_resolved`; if true, persist a pending decision row, emit `decision_required`, and block that agent until `Answer` is called (which emits `decision_answered` and replies to the runner). On agent results: `all` completion requires every agent to succeed; validate required `Artifacts` exist on disk (emit `artifact_produced` per artifact); missing artifact or agent error → retry the stage up to `Retries`, then emit `stage_failed` and stop the issue. On success emit `stage_completed` and advance. Gates: this task treats `approve_artifact` as an automatic importance-1.0 decision ("Approve <stage> artifacts?" options `["approve","reject"]`) routed through the same decision path — rejection = stage failure (no re-run loop in v1 core; the failure escalates); `auto` = no gate; `decision_queue` = no extra gate (in-stage asks already routed).
- Workspace: this plan does not create real worktrees. `workdir` = `<DataDir>/<issueID>/<stage>/` (MkdirAll). The treehouse integration replaces this in Plan 2.

- [ ] **Step 1: Write the failing test**

```go
// internal/engine/engine_test.go
package engine

import (
	"context"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/slots"
	"github.com/weston6142/watchtower/internal/store"
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
	var auto, required, completed int
	for _, ev := range evs {
		switch ev.Type {
		case core.EvDecisionAutoResolved:
			auto++
		case core.EvDecisionRequired:
			required++
		case core.EvStageCompleted:
			completed++
		}
	}
	if auto != 1 || required != 1 || completed != 4 {
		t.Fatalf("auto=%d required=%d completed=%d", auto, required, completed)
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -v`
Expected: FAIL (undefined `Engine` etc.).

- [ ] **Step 3: Write minimal implementation**

```go
// internal/engine/engine.go
package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/slots"
	"github.com/weston6142/watchtower/internal/store"
)

type Config struct {
	Store   *store.Store
	Runner  runner.Runner
	Pool    *slots.Pool
	Flows   map[string]flow.Flow
	Rules   levers.Rules
	DataDir string
}

type PendingDecision struct {
	ID      int64
	IssueID string
	Stage   string
	D       levers.Decision
}

type pending struct {
	PendingDecision
	reply chan int
}

type issueState struct {
	id       string
	flowName string
	matrix   levers.Matrix
	priority int
}

type Engine struct {
	cfg     Config
	mu      sync.Mutex
	nextID  int
	nextDec int64
	issues  map[string]*issueState
	pend    map[int64]*pending
}

func New(cfg Config) *Engine {
	return &Engine{cfg: cfg, issues: map[string]*issueState{}, pend: map[int64]*pending{}}
}

func (e *Engine) emit(t core.EventType, issueID string, payload any) {
	ev, err := core.NewEvent(t, issueID, payload)
	if err == nil {
		e.cfg.Store.Append(ev)
	}
}

func (e *Engine) CreateIssue(title, body, flowName string, m levers.Matrix, priority int) (string, error) {
	if _, ok := e.cfg.Flows[flowName]; !ok {
		return "", fmt.Errorf("unknown flow %q", flowName)
	}
	e.mu.Lock()
	e.nextID++
	id := fmt.Sprintf("GH-%d", e.nextID)
	e.issues[id] = &issueState{id: id, flowName: flowName, matrix: m, priority: priority}
	e.mu.Unlock()
	e.emit(core.EvIssueCreated, id, map[string]string{"title": title, "flow": flowName})
	return id, nil
}

func (e *Engine) PendingDecisions() []PendingDecision {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []PendingDecision
	for _, p := range e.pend {
		out = append(out, p.PendingDecision)
	}
	return out
}

func (e *Engine) Answer(decisionID int64, option int) error {
	e.mu.Lock()
	p, ok := e.pend[decisionID]
	if ok {
		delete(e.pend, decisionID)
	}
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("no pending decision %d", decisionID)
	}
	e.emit(core.EvDecisionAnswered, p.IssueID, map[string]any{
		"decision_id": p.ID, "option": option})
	p.reply <- option
	return nil
}

// escalate blocks until the human answers; returns chosen option.
func (e *Engine) escalate(issueID, stage string, d levers.Decision) int {
	e.mu.Lock()
	e.nextDec++
	p := &pending{
		PendingDecision: PendingDecision{ID: e.nextDec, IssueID: issueID, Stage: stage, D: d},
		reply:           make(chan int, 1),
	}
	e.pend[p.ID] = p
	e.mu.Unlock()
	e.emit(core.EvDecisionRequired, issueID, map[string]any{
		"decision_id": p.ID, "stage": stage, "question": d.Question,
		"options": d.Options, "recommended": d.Recommended})
	return <-p.reply
}

func (e *Engine) handleAsk(is *issueState, stage string, a runner.Ask) {
	lever := is.matrix[stage]
	if levers.Route(a.Decision, lever, e.cfg.Rules) {
		a.Reply <- e.escalate(is.id, stage, a.Decision)
		return
	}
	e.emit(core.EvDecisionAutoResolved, is.id, map[string]any{
		"stage": stage, "question": a.Decision.Question, "option": a.Decision.Recommended})
	a.Reply <- a.Decision.Recommended
}

func (e *Engine) runStageOnce(ctx context.Context, is *issueState, st flow.Stage) error {
	workdir := filepath.Join(e.cfg.DataDir, is.id, st.Name)
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return err
	}
	e.emit(core.EvStageStarted, is.id, map[string]string{"stage": st.Name})

	type agentDone struct {
		pkg string
		res runner.Result
	}
	dones := make(chan agentDone, len(st.Agents))
	runAgent := func(a flow.AgentRef) {
		asks := make(chan runner.Ask)
		resc := e.cfg.Runner.Run(ctx, is.id, st.Name, a.Package, workdir, asks)
		for {
			select {
			case ask := <-asks:
				e.handleAsk(is, st.Name, ask)
			case res := <-resc:
				dones <- agentDone{a.Package, res}
				return
			}
		}
	}
	if st.Parallel {
		for _, a := range st.Agents {
			go runAgent(a)
		}
	} else {
		go func() {
			for _, a := range st.Agents {
				runAgent(a)
			}
		}()
	}

	need := len(st.Agents)
	var firstErr error
	succeeded := 0
	for i := 0; i < need; i++ {
		d := <-dones
		if d.res.Err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("agent %s: %w", d.pkg, d.res.Err)
			}
			continue
		}
		succeeded++
		if st.Completion == "any" {
			break
		}
	}
	if st.Completion == "all" && firstErr != nil {
		return firstErr
	}
	if st.Completion == "any" && succeeded == 0 {
		return firstErr
	}
	// validate artifacts
	for _, name := range st.Artifacts {
		p := filepath.Join(workdir, name)
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("stage %s missing artifact %s", st.Name, name)
		}
		e.emit(core.EvArtifactProduced, is.id, map[string]string{"stage": st.Name, "artifact": name, "path": p})
	}
	return nil
}

func (e *Engine) runStage(ctx context.Context, is *issueState, st flow.Stage) error {
	var release func()
	if st.HeavySlot {
		e.emit(core.EvSlotQueued, is.id, map[string]string{"stage": st.Name})
		rel, err := e.cfg.Pool.Acquire(ctx, is.id, is.priority)
		if err != nil {
			return err
		}
		release = rel
		e.emit(core.EvSlotAcquired, is.id, map[string]string{"stage": st.Name})
		defer func() {
			release()
			e.emit(core.EvSlotReleased, is.id, map[string]string{"stage": st.Name})
		}()
	}

	var err error
	for attempt := 0; attempt <= st.Retries; attempt++ {
		err = e.runStageOnce(ctx, is, st)
		if err == nil {
			break
		}
	}
	if err != nil {
		e.emit(core.EvStageFailed, is.id, map[string]string{"stage": st.Name, "error": err.Error()})
		return err
	}

	if st.Gate == flow.GateApproveArtifact {
		d := levers.Decision{
			Question:    fmt.Sprintf("Approve %s artifacts?", st.Name),
			Options:     []string{"approve", "reject"},
			Recommended: 0,
			Importance:  1.0,
		}
		if e.escalate(is.id, st.Name, d) != 0 {
			err := fmt.Errorf("stage %s artifacts rejected", st.Name)
			e.emit(core.EvStageFailed, is.id, map[string]string{"stage": st.Name, "error": err.Error()})
			return err
		}
	}
	e.emit(core.EvStageCompleted, is.id, map[string]string{"stage": st.Name})
	return nil
}

func (e *Engine) StartIssue(ctx context.Context, id string) error {
	e.mu.Lock()
	is, ok := e.issues[id]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown issue %s", id)
	}
	f := e.cfg.Flows[is.flowName]
	for _, st := range f.Stages {
		if err := e.runStage(ctx, is, st); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/engine/ -race -v`
Expected: PASS (both tests, with `-race`).

- [ ] **Step 5: Commit**

```bash
git add internal/engine/
git commit -m "feat: engine runs issues through staged flows with lever routing"
```

---

### Task 8: Unix socket protocol (server + client)

**Files:**
- Create: `internal/proto/proto.go`, `internal/proto/server.go`, `internal/proto/client.go`
- Test: `internal/proto/proto_test.go`

**Interfaces:**
- Consumes: `engine.Engine`, `engine.PendingDecision` (Task 7), `store.Store` (Task 2), `core.Event` (Task 1).
- Produces:
  ```go
  // proto.go — wire types (JSONL, one JSON object per line)
  type Command struct {
      Op         string `json:"op"` // "create_issue"|"start_issue"|"list_decisions"|"answer_decision"|"tail"
      Title      string `json:"title,omitempty"`
      Body       string `json:"body,omitempty"`
      Flow       string `json:"flow,omitempty"`
      Preset     string `json:"preset,omitempty"` // yolo|regular|strict
      Priority   int    `json:"priority,omitempty"`
      IssueID    string `json:"issue_id,omitempty"`
      DecisionID int64  `json:"decision_id,omitempty"`
      Option     int    `json:"option,omitempty"`
      SinceSeq   int64  `json:"since_seq,omitempty"`
  }
  type Response struct {
      OK        bool                     `json:"ok"`
      Error     string                   `json:"error,omitempty"`
      IssueID   string                   `json:"issue_id,omitempty"`
      Decisions []engine.PendingDecision `json:"decisions,omitempty"`
      Events    []core.Event             `json:"events,omitempty"`
  }
  // server.go
  type Server struct{...}
  func NewServer(e *engine.Engine, s *store.Store) *Server
  func (sv *Server) Serve(l net.Listener) error // one Command line in, one Response line out, per request; connection stays open for more
  // client.go
  type Client struct{...}
  func Dial(sockPath string) (*Client, error)
  func (c *Client) Do(cmd Command) (Response, error)
  func (c *Client) Close() error
  ```
- `start_issue` runs `engine.StartIssue` in a goroutine and returns immediately (`OK: true`); completion/failure is observable via events (`tail` with `since_seq`).

- [ ] **Step 1: Write the failing test**

```go
// internal/proto/proto_test.go
package proto

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/engine"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/slots"
	"github.com/weston6142/watchtower/internal/store"
)

func TestCreateAnswerAndTailOverSocket(t *testing.T) {
	f, err := flow.Load("../flow/testdata/default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := store.Open("file:proto?mode=memory&cache=shared")
	defer s.Close()
	fr := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"brainstorm/brainstorm":      {},
		"spec/spec-writer":           {Artifacts: map[string]string{"spec.md": ""}},
		"execute/executor":           {Artifacts: map[string]string{"diff": ""}},
		"review/clean-code-reviewer": {Artifacts: map[string]string{"review.md": ""}},
		"review/reviewer":            {},
		"review/doc-writer":          {Artifacts: map[string]string{"docs": ""}},
	}}
	e := engine.New(engine.Config{Store: s, Runner: fr, Pool: slots.NewPool(2),
		Flows: map[string]flow.Flow{"default": f}, DataDir: t.TempDir()})
	_ = levers.Rules{}

	sock := filepath.Join(t.TempDir(), "g.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go NewServer(e, s).Serve(l)

	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	r, err := c.Do(Command{Op: "create_issue", Title: "hi", Flow: "default", Preset: "yolo"})
	if err != nil || !r.OK || r.IssueID == "" {
		t.Fatalf("create failed: %+v %v", r, err)
	}
	id := r.IssueID
	if r, _ = c.Do(Command{Op: "start_issue", IssueID: id}); !r.OK {
		t.Fatalf("start failed: %+v", r)
	}

	// spec gate escalates even on yolo — answer it
	deadline := time.After(5 * time.Second)
	for {
		r, _ = c.Do(Command{Op: "list_decisions"})
		if len(r.Decisions) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no decision appeared")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if r, _ = c.Do(Command{Op: "answer_decision", DecisionID: r.Decisions[0].ID, Option: 0}); !r.OK {
		t.Fatalf("answer failed: %+v", r)
	}

	// tail until issue completes all 4 stages
	deadline = time.After(5 * time.Second)
	completed := 0
	var since int64
	for completed < 4 {
		r, _ = c.Do(Command{Op: "tail", SinceSeq: since})
		for _, ev := range r.Events {
			since = ev.Seq
			if ev.Type == core.EvStageCompleted {
				completed++
			}
		}
		select {
		case <-deadline:
			t.Fatalf("only %d stages completed", completed)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proto/ -v`
Expected: FAIL (undefined symbols).

- [ ] **Step 3: Write minimal implementation**

```go
// internal/proto/proto.go
package proto

import (
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/engine"
)

type Command struct {
	Op         string `json:"op"`
	Title      string `json:"title,omitempty"`
	Body       string `json:"body,omitempty"`
	Flow       string `json:"flow,omitempty"`
	Preset     string `json:"preset,omitempty"`
	Priority   int    `json:"priority,omitempty"`
	IssueID    string `json:"issue_id,omitempty"`
	DecisionID int64  `json:"decision_id,omitempty"`
	Option     int    `json:"option,omitempty"`
	SinceSeq   int64  `json:"since_seq,omitempty"`
}

type Response struct {
	OK        bool                     `json:"ok"`
	Error     string                   `json:"error,omitempty"`
	IssueID   string                   `json:"issue_id,omitempty"`
	Decisions []engine.PendingDecision `json:"decisions,omitempty"`
	Events    []core.Event             `json:"events,omitempty"`
}
```

```go
// internal/proto/server.go
package proto

import (
	"bufio"
	"context"
	"encoding/json"
	"net"

	"github.com/weston6142/watchtower/internal/engine"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/store"
)

type Server struct {
	eng   *engine.Engine
	st    *store.Store
	flows map[string]flow.Flow
}

func NewServer(e *engine.Engine, s *store.Store) *Server {
	return &Server{eng: e, st: s}
}

// SetFlows lets the daemon share loaded flows for preset expansion.
func (sv *Server) SetFlows(f map[string]flow.Flow) { sv.flows = f }

func (sv *Server) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go sv.handle(conn)
	}
}

func (sv *Server) handle(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	enc := json.NewEncoder(conn)
	for sc.Scan() {
		var cmd Command
		if err := json.Unmarshal(sc.Bytes(), &cmd); err != nil {
			enc.Encode(Response{Error: err.Error()})
			continue
		}
		enc.Encode(sv.exec(cmd))
	}
}

func (sv *Server) exec(cmd Command) Response {
	switch cmd.Op {
	case "create_issue":
		fl, ok := sv.flowFor(cmd.Flow)
		if !ok {
			return Response{Error: "unknown flow " + cmd.Flow}
		}
		lever := flow.Lever(cmd.Preset)
		if lever != flow.LeverYolo && lever != flow.LeverRegular && lever != flow.LeverStrict {
			lever = flow.LeverRegular
		}
		id, err := sv.eng.CreateIssue(cmd.Title, cmd.Body, cmd.Flow, levers.Preset(fl, lever), cmd.Priority)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: id}
	case "start_issue":
		go sv.eng.StartIssue(context.Background(), cmd.IssueID)
		return Response{OK: true, IssueID: cmd.IssueID}
	case "list_decisions":
		return Response{OK: true, Decisions: sv.eng.PendingDecisions()}
	case "answer_decision":
		if err := sv.eng.Answer(cmd.DecisionID, cmd.Option); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true}
	case "tail":
		evs, err := sv.st.EventsSince(cmd.SinceSeq)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, Events: evs}
	default:
		return Response{Error: "unknown op " + cmd.Op}
	}
}

func (sv *Server) flowFor(name string) (flow.Flow, bool) {
	if sv.flows != nil {
		f, ok := sv.flows[name]
		return f, ok
	}
	return flow.Flow{}, false
}
```

Note: the test constructs the engine with flows but the server needs them too for preset expansion — in the test, after `NewServer(e, s)` call `.SetFlows(map[string]flow.Flow{"default": f})`. Update the test's server line to:

```go
	srv := NewServer(e, s)
	srv.SetFlows(map[string]flow.Flow{"default": f})
	go srv.Serve(l)
```

```go
// internal/proto/client.go
package proto

import (
	"bufio"
	"encoding/json"
	"net"
)

type Client struct {
	conn net.Conn
	sc   *bufio.Scanner
	enc  *json.Encoder
}

func Dial(sockPath string) (*Client, error) {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	return &Client{conn: conn, sc: sc, enc: json.NewEncoder(conn)}, nil
}

func (c *Client) Do(cmd Command) (Response, error) {
	if err := c.enc.Encode(cmd); err != nil {
		return Response{}, err
	}
	if !c.sc.Scan() {
		return Response{}, c.sc.Err()
	}
	var r Response
	if err := json.Unmarshal(c.sc.Bytes(), &r); err != nil {
		return Response{}, err
	}
	return r, nil
}

func (c *Client) Close() error { return c.conn.Close() }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/proto/ -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/proto/
git commit -m "feat: unix socket JSONL protocol, server and client"
```

---

### Task 9: `watchtower` binary — daemon + CLI

**Files:**
- Create: `cmd/watchtower/main.go`
- Test: manual smoke (script below) — thin wiring layer; logic is covered by Tasks 1–8.

**Interfaces:**
- Consumes: everything above.
- Produces a single binary with subcommands:
  - `watchtower daemon --data DIR --flows DIR --slots N` — opens `DIR/watchtower.db`, loads every `*.yaml` in flows dir, listens on `DIR/watchtower.sock`, uses `FakeRunner` when env `WATCHTOWER_FAKE=1` is set (real runner arrives in Plan 2; until then the daemon refuses to start without `WATCHTOWER_FAKE=1` with error "no real runner available yet — set WATCHTOWER_FAKE=1").
  - `watchtower new --title T [--flow default] [--preset regular] [--priority 0]` — create + start an issue; prints issue ID.
  - `watchtower decisions` — list pending decisions (ID, issue, stage, question, options with recommended marked).
  - `watchtower answer <decision-id> <option-index>`.
  - `watchtower tail [--since N]` — prints events as they exist (single poll).
  - All client commands honor `--data DIR` (default `~/.local/share/watchtower`) to find the socket.

- [ ] **Step 1: Write main.go**

```go
// cmd/watchtower/main.go
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/weston6142/watchtower/internal/engine"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/proto"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/slots"
	"github.com/weston6142/watchtower/internal/store"
)

func defaultData() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "watchtower")
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: watchtower <daemon|new|decisions|answer|tail> [flags]")
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "daemon":
		runDaemon(args)
	case "new":
		fs := flag.NewFlagSet("new", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		title := fs.String("title", "", "issue title")
		flowName := fs.String("flow", "default", "flow name")
		preset := fs.String("preset", "regular", "yolo|regular|strict")
		prio := fs.Int("priority", 0, "priority")
		fs.Parse(args)
		c := mustDial(*data)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "create_issue", Title: *title, Flow: *flowName, Preset: *preset, Priority: *prio})
		mustDo(c, proto.Command{Op: "start_issue", IssueID: r.IssueID})
		fmt.Println(r.IssueID)
	case "decisions":
		fs := flag.NewFlagSet("decisions", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		fs.Parse(args)
		c := mustDial(*data)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "list_decisions"})
		for _, d := range r.Decisions {
			fmt.Printf("[%d] %s/%s: %s\n", d.ID, d.IssueID, d.Stage, d.D.Question)
			for i, o := range d.D.Options {
				mark := "  "
				if i == d.D.Recommended {
					mark = "* "
				}
				fmt.Printf("    %s%d) %s\n", mark, i, o)
			}
		}
	case "answer":
		fs := flag.NewFlagSet("answer", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		fs.Parse(args)
		rest := fs.Args()
		if len(rest) != 2 {
			fmt.Fprintln(os.Stderr, "usage: watchtower answer <decision-id> <option>")
			os.Exit(2)
		}
		id, _ := strconv.ParseInt(rest[0], 10, 64)
		opt, _ := strconv.Atoi(rest[1])
		c := mustDial(*data)
		defer c.Close()
		mustDo(c, proto.Command{Op: "answer_decision", DecisionID: id, Option: opt})
		fmt.Println("answered")
	case "tail":
		fs := flag.NewFlagSet("tail", flag.ExitOnError)
		data := fs.String("data", defaultData(), "data dir")
		since := fs.Int64("since", 0, "since seq")
		fs.Parse(args)
		c := mustDial(*data)
		defer c.Close()
		r := mustDo(c, proto.Command{Op: "tail", SinceSeq: *since})
		for _, ev := range r.Events {
			fmt.Printf("%d %s %s %s\n", ev.Seq, ev.At.Format("15:04:05"), ev.IssueID, ev.Type)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		os.Exit(2)
	}
}

func runDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	data := fs.String("data", defaultData(), "data dir")
	flowsDir := fs.String("flows", "", "flows dir (required)")
	slotN := fs.Int("slots", 4, "heavy slots")
	fs.Parse(args)
	if *flowsDir == "" {
		fmt.Fprintln(os.Stderr, "daemon: --flows is required")
		os.Exit(2)
	}
	if os.Getenv("WATCHTOWER_FAKE") != "1" {
		fmt.Fprintln(os.Stderr, "no real runner available yet — set WATCHTOWER_FAKE=1")
		os.Exit(1)
	}
	if err := os.MkdirAll(*data, 0o755); err != nil {
		fatal(err)
	}
	st, err := store.Open(filepath.Join(*data, "watchtower.db"))
	if err != nil {
		fatal(err)
	}
	flows := map[string]flow.Flow{}
	matches, _ := filepath.Glob(filepath.Join(*flowsDir, "*.yaml"))
	for _, m := range matches {
		f, err := flow.Load(m)
		if err != nil {
			fatal(err)
		}
		flows[f.Name] = f
	}
	if len(flows) == 0 {
		fatal(fmt.Errorf("no flows found in %s", *flowsDir))
	}
	eng := engine.New(engine.Config{
		Store: st, Runner: fakeForFlows(flows), Pool: slots.NewPool(*slotN),
		Flows: flows, DataDir: filepath.Join(*data, "issues"),
	})
	sock := filepath.Join(*data, "watchtower.sock")
	os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		fatal(err)
	}
	fmt.Println("watchtower daemon listening on", sock)
	srv := proto.NewServer(eng, st)
	srv.SetFlows(flows)
	fatal(srv.Serve(l))
}

// fakeForFlows builds a FakeRunner that succeeds every stage and writes
// every declared artifact — enough to exercise the pipeline end to end.
func fakeForFlows(flows map[string]flow.Flow) *runner.FakeRunner {
	scripts := map[string]runner.Script{}
	for _, f := range flows {
		for _, st := range f.Stages {
			arts := map[string]string{}
			for _, a := range st.Artifacts {
				arts[a] = ""
			}
			for _, ag := range st.Agents {
				scripts[st.Name+"/"+ag.Package] = runner.Script{Artifacts: arts, Tokens: 10}
			}
		}
	}
	return &runner.FakeRunner{Scripts: scripts}
}

func mustDial(data string) *proto.Client {
	c, err := proto.Dial(filepath.Join(data, "watchtower.sock"))
	if err != nil {
		fatal(err)
	}
	return c
}

func mustDo(c *proto.Client, cmd proto.Command) proto.Response {
	r, err := c.Do(cmd)
	if err != nil {
		fatal(err)
	}
	if !r.OK {
		fatal(fmt.Errorf("%s: %s", cmd.Op, r.Error))
	}
	return r
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "watchtower:", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: clean build.

- [ ] **Step 3: Smoke test end-to-end**

```bash
cd ~/watchtower
mkdir -p /tmp/gh-smoke/flows
cp internal/flow/testdata/default.yaml /tmp/gh-smoke/flows/
WATCHTOWER_FAKE=1 go run ./cmd/watchtower daemon --data /tmp/gh-smoke --flows /tmp/gh-smoke/flows &
sleep 1
go run ./cmd/watchtower new --data /tmp/gh-smoke --title "smoke test" --preset yolo
sleep 1
go run ./cmd/watchtower decisions --data /tmp/gh-smoke
# expect: [1] GH-1/spec: Approve spec artifacts?  (* 0) approve  1) reject
go run ./cmd/watchtower answer --data /tmp/gh-smoke 1 0
sleep 1
go run ./cmd/watchtower tail --data /tmp/gh-smoke
# expect: issue_created ... stage_completed ×4 including review's 3 agents
kill %1
```

Expected: four `stage_completed` events for GH-1; no errors.

- [ ] **Step 4: Run full test suite**

Run: `go test ./... -race`
Expected: all packages PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/
git commit -m "feat: watchtower binary with daemon and CLI subcommands"
```

---

### Task 10: Event-replay projection (client state rebuild)

**Files:**
- Create: `internal/projection/projection.go`
- Test: `internal/projection/projection_test.go`

**Interfaces:**
- Consumes: `core.Event` types (Task 1).
- Produces (this is the state the TUI in Plan 4 will render — building it now proves the event stream is sufficient):
  ```go
  type IssueView struct {
      ID           string
      Title        string
      CurrentStage string
      State        string // "running"|"waiting_decision"|"queued_for_slot"|"failed"|"done"
      Completed    []string // completed stage names
      Tokens       int
  }
  type State struct {
      Issues    map[string]*IssueView
      Decisions map[int64]DecisionView
  }
  type DecisionView struct { ID int64; IssueID string; Stage, Question string; Options []string; Recommended int }
  func NewState() *State
  func (s *State) Apply(ev core.Event) // idempotent per event; unknown types ignored
  ```
- Semantics: `issue_created` adds an IssueView (state "running", title from payload); `stage_started` sets CurrentStage + state "running"; `slot_queued` → state "queued_for_slot"; `slot_acquired` → "running"; `decision_required` adds a DecisionView and sets issue state "waiting_decision"; `decision_answered` removes it and restores "running"; `stage_completed` appends to Completed; when Completed includes the last known started stage and no stage follows, leave state as-is (the daemon has no "issue_done" event yet — derive "done" when `stage_completed` arrives for stage `review`; acceptable v1 shortcut, noted for Plan 2 to add an `issue_completed` event); `stage_failed` → "failed".

- [ ] **Step 1: Write the failing test**

```go
// internal/projection/projection_test.go
package projection

import (
	"testing"

	"github.com/weston6142/watchtower/internal/core"
)

func ev(t *testing.T, typ core.EventType, issue string, payload any) core.Event {
	t.Helper()
	e, err := core.NewEvent(typ, issue, payload)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestReplayBuildsIssueView(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "hello"}))
	s.Apply(ev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "spec"}))
	s.Apply(ev(t, core.EvDecisionRequired, "GH-1", map[string]any{
		"decision_id": float64(1), "stage": "spec", "question": "Approve spec artifacts?",
		"options": []any{"approve", "reject"}, "recommended": float64(0)}))

	iv := s.Issues["GH-1"]
	if iv == nil || iv.Title != "hello" || iv.State != "waiting_decision" || iv.CurrentStage != "spec" {
		t.Fatalf("bad view: %+v", iv)
	}
	if len(s.Decisions) != 1 || s.Decisions[1].Question != "Approve spec artifacts?" {
		t.Fatalf("bad decisions: %+v", s.Decisions)
	}

	s.Apply(ev(t, core.EvDecisionAnswered, "GH-1", map[string]any{"decision_id": float64(1), "option": float64(0)}))
	s.Apply(ev(t, core.EvStageCompleted, "GH-1", map[string]any{"stage": "spec"}))
	if len(s.Decisions) != 0 || iv.State != "running" || len(iv.Completed) != 1 {
		t.Fatalf("after answer: %+v decisions=%v", iv, s.Decisions)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/projection/ -v`
Expected: FAIL.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/projection/projection.go
package projection

import (
	"encoding/json"

	"github.com/weston6142/watchtower/internal/core"
)

type IssueView struct {
	ID           string
	Title        string
	CurrentStage string
	State        string
	Completed    []string
	Tokens       int
}

type DecisionView struct {
	ID          int64
	IssueID     string
	Stage       string
	Question    string
	Options     []string
	Recommended int
}

type State struct {
	Issues    map[string]*IssueView
	Decisions map[int64]DecisionView
}

func NewState() *State {
	return &State{Issues: map[string]*IssueView{}, Decisions: map[int64]DecisionView{}}
}

func (s *State) Apply(ev core.Event) {
	var p map[string]any
	json.Unmarshal(ev.Payload, &p)
	str := func(k string) string { v, _ := p[k].(string); return v }
	num := func(k string) float64 { v, _ := p[k].(float64); return v }

	iv := s.Issues[ev.IssueID]
	switch ev.Type {
	case core.EvIssueCreated:
		s.Issues[ev.IssueID] = &IssueView{ID: ev.IssueID, Title: str("title"), State: "running"}
	case core.EvStageStarted:
		if iv != nil {
			iv.CurrentStage = str("stage")
			iv.State = "running"
		}
	case core.EvSlotQueued:
		if iv != nil {
			iv.State = "queued_for_slot"
		}
	case core.EvSlotAcquired:
		if iv != nil {
			iv.State = "running"
		}
	case core.EvDecisionRequired:
		var opts []string
		if raw, ok := p["options"].([]any); ok {
			for _, o := range raw {
				if os, ok := o.(string); ok {
					opts = append(opts, os)
				}
			}
		}
		id := int64(num("decision_id"))
		s.Decisions[id] = DecisionView{ID: id, IssueID: ev.IssueID, Stage: str("stage"),
			Question: str("question"), Options: opts, Recommended: int(num("recommended"))}
		if iv != nil {
			iv.State = "waiting_decision"
		}
	case core.EvDecisionAnswered:
		delete(s.Decisions, int64(num("decision_id")))
		if iv != nil {
			iv.State = "running"
		}
	case core.EvStageCompleted:
		if iv != nil {
			iv.Completed = append(iv.Completed, str("stage"))
			if str("stage") == "review" {
				iv.State = "done"
			}
		}
	case core.EvStageFailed:
		if iv != nil {
			iv.State = "failed"
		}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/projection/ -v` then `go test ./... -race`
Expected: PASS everywhere.

- [ ] **Step 5: Commit**

```bash
git add internal/projection/
git commit -m "feat: event-replay projection for client state"
```

---

## Self-review notes

- **Spec coverage:** Events/log/replay (T1,T2,T10), flows-as-data with multi-agent stages + artifact validation (T3,T7), lever matrix/presets/escalation three layers — floor via importance 1.0, user glob rules, thresholds (T4), Runner interface reserved for Claude/Codex (T5), slot pool with visible queued state (T6), decision queue with auto-resolve logging (T7), socket protocol + CLI clients (T8,T9). Deferred to later plans, per spec cutline: real ClaudeCodeRunner/treehouse (Plan 2), overlords + proposals table usage + blocking-cost ordering + token budgets (Plan 3 — `decisions.blocking_cost` and `proposals` schema already exist), TUI/arch view (Plan 4).
- **Known v1-core shortcuts, carried forward explicitly:** no `issue_completed` event (projection derives from final stage; Plan 2 adds the event); decisions are held in memory + events only (the `decisions` table is written by Plan 3 when ordering matters); `stage_runs` rows are written starting Plan 2 when real sessions exist.
- **Type consistency check:** `runner.Ask.Reply chan int` used by engine `handleAsk`/`escalate` ✓; `flow.Lever` shared by levers + proto preset parsing ✓; `PendingDecision` shared engine→proto→CLI ✓.
