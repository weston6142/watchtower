# Claude Runner & Default Flow Implementation Plan (Plan 2 of 4)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the FakeRunner with a real `ClaudeCodeRunner` that drives headless Claude Code sessions, ship the default flow's agent packages, acquire real worktrees for execution stages, and close Plan 1's deferred gaps (`issue_completed` event, `stage_runs` persistence, token budgets).

**Architecture:** The runner spawns `claude -p` with `--input-format stream-json --output-format stream-json` so the session stays interactive over stdio. Agents request human decisions by emitting a `watchtower_decision` JSON marker in their assistant text; the runner converts markers to engine `Ask`s and feeds the chosen option back as the next user message. Agent packages are directories (prompt + config). Workspaces come from a `Provider` interface: treehouse CLI if available, `git worktree` fallback.

**Tech Stack:** Go 1.22+ (same module), `os/exec` for the Claude CLI, existing internal packages from Plan 1. Tests stub the Claude binary with shell scripts in `testdata/` — no real API calls in the suite.

## Global Constraints

- All Plan 1 global constraints still apply (pure Go, events before side effects, snake_case JSON, module `github.com/weston6142/watchtower`).
- Runner contract from Plan 1 is frozen: `Run(ctx, issueID, stage, agentPkg, workdir string, asks chan<- Ask) <-chan Result` — `ClaudeCodeRunner` implements it unchanged.
- The Claude CLI is invoked as: `claude -p --input-format stream-json --output-format stream-json --verbose --dangerously-skip-permissions=false` plus per-package `--allowedTools`, `--model`, and `--append-system-prompt`; never hardcode a model default (empty = CLI default).
- Decision marker (exact): an assistant text turn whose trimmed content contains a line starting with `{"watchtower_decision":` parseable as `{"watchtower_decision": {"question": string, "options": []string, "recommended": int, "importance": float, "paths": []string}}`.
- Tests never execute the real `claude` binary: `ClaudeCodeRunner.Bin` is a field, tests point it at `testdata/*.sh` stubs.
- New events introduced here: `issue_completed`, `budget_exceeded`.

---

### Task 1: `issue_completed` event + projection fix

**Files:**
- Modify: `internal/core/event.go` (add constants), `internal/engine/engine.go` (`StartIssue`), `internal/projection/projection.go`
- Test: `internal/projection/projection_test.go` (extend), `internal/engine/engine_test.go` (extend)

**Interfaces:**
- Consumes: everything existing.
- Produces: `core.EvIssueCompleted EventType = "issue_completed"`, `core.EvBudgetExceeded EventType = "budget_exceeded"` (constant added now, emitted in Task 7). Engine emits `issue_completed` after the last stage completes. Projection: `issue_completed` sets `IssueView.State = "done"`; delete the Plan 1 shortcut that inferred done from `stage == "review"`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/projection/projection_test.go`:

```go
func TestIssueCompletedSetsDone(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-2", map[string]any{"title": "x"}))
	s.Apply(ev(t, core.EvStageCompleted, "GH-2", map[string]any{"stage": "review"}))
	if s.Issues["GH-2"].State == "done" {
		t.Fatal("stage name must no longer imply done")
	}
	s.Apply(ev(t, core.EvIssueCompleted, "GH-2", nil))
	if s.Issues["GH-2"].State != "done" {
		t.Fatalf("want done, got %q", s.Issues["GH-2"].State)
	}
}
```

Append to `internal/engine/engine_test.go` (inside `TestYoloRunEscalatesOnlyGate`, after the existing event-count loop, extend the switch and assertions):

```go
	// add to the switch in the event scan:
	//   case core.EvIssueCompleted: issueDone++
	// and assert issueDone == 1 alongside the existing counts.
```

Concretely: declare `var issueDone int` before the loop, add `case core.EvIssueCompleted: issueDone++` to the switch, and change the final assertion to:

```go
	if auto != 1 || required != 1 || completed != 4 || issueDone != 1 {
		t.Fatalf("auto=%d required=%d completed=%d issueDone=%d", auto, required, completed, issueDone)
	}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/projection/ ./internal/engine/ -run 'TestIssueCompleted|TestYolo' -v`
Expected: FAIL (undefined `EvIssueCompleted`, then wrong counts).

- [ ] **Step 3: Implement**

In `internal/core/event.go`, add to the const block:

```go
	EvIssueCompleted  EventType = "issue_completed"
	EvBudgetExceeded  EventType = "budget_exceeded"
```

In `internal/engine/engine.go` `StartIssue`, after the stage loop succeeds:

```go
	e.emit(core.EvIssueCompleted, id, nil)
	return nil
```

In `internal/projection/projection.go` `Apply`: remove the `if str("stage") == "review" { iv.State = "done" }` block from `EvStageCompleted`, and add:

```go
	case core.EvIssueCompleted:
		if iv != nil {
			iv.State = "done"
		}
```

- [ ] **Step 4: Run the full suite**

Run: `go test ./... -race`
Expected: PASS (the old projection test asserting done-on-review must be updated if it asserted that; the test added in Step 1 replaces that behavior).

- [ ] **Step 5: Commit**

```bash
git add internal/core/ internal/engine/ internal/projection/
git commit -m "feat: issue_completed event replaces review-stage done heuristic"
```

---

### Task 2: Persist `stage_runs` rows

**Files:**
- Modify: `internal/store/store.go`, `internal/engine/engine.go`
- Test: `internal/store/store_test.go` (extend), `internal/engine/engine_test.go` (extend)

**Interfaces:**
- Consumes: existing `stage_runs` table (schema already created in Plan 1).
- Produces on `Store`:
  ```go
  type StageRun struct {
      ID        int64
      IssueID   string
      Stage     string
      Agent     string
      SessionID string
      Worktree  string
      Status    string // "running"|"succeeded"|"failed"
      Tokens    int
  }
  func (s *Store) InsertStageRun(r StageRun) (int64, error)
  func (s *Store) FinishStageRun(id int64, status, sessionID string, tokens int) error
  func (s *Store) StageRuns(issueID string) ([]StageRun, error)
  func (s *Store) IssueTokens(issueID string) (int, error) // SUM(tokens)
  ```
- Engine: in `runStageOnce`'s `runAgent`, insert a `StageRun` (status "running") before calling `Runner.Run`, and finish it from the `Result` (status per `res.Err`, tokens from `res.Tokens`, session ID from `res.SessionID` — add `SessionID string` to `runner.Result` now; FakeRunner leaves it empty).

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/store_test.go`:

```go
func TestStageRunLifecycle(t *testing.T) {
	s, err := Open("file:t2?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, err := s.InsertStageRun(StageRun{IssueID: "GH-1", Stage: "execute", Agent: "executor", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStageRun(id, "succeeded", "sess-abc", 1234); err != nil {
		t.Fatal(err)
	}
	runs, err := s.StageRuns("GH-1")
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs: %v %v", runs, err)
	}
	r := runs[0]
	if r.Status != "succeeded" || r.SessionID != "sess-abc" || r.Tokens != 1234 {
		t.Fatalf("bad run: %+v", r)
	}
	tok, _ := s.IssueTokens("GH-1")
	if tok != 1234 {
		t.Fatalf("tokens: %d", tok)
	}
}
```

Append to `internal/engine/engine_test.go`:

```go
func TestEngineRecordsStageRuns(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, _ := e.CreateIssue("t", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)
	errc := make(chan error, 1)
	go func() { errc <- e.StartIssue(context.Background(), id) }()
	for {
		if ds := e.PendingDecisions(); len(ds) == 1 {
			e.Answer(ds[0].ID, 0)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-errc; err != nil {
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ ./internal/engine/ -run 'StageRun' -v`
Expected: FAIL (undefined `StageRun` methods).

- [ ] **Step 3: Implement**

Add to `internal/store/store.go`:

```go
type StageRun struct {
	ID        int64
	IssueID   string
	Stage     string
	Agent     string
	SessionID string
	Worktree  string
	Status    string
	Tokens    int
}

func (s *Store) InsertStageRun(r StageRun) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO stage_runs(issue_id,stage,agent,session_id,worktree,status,tokens)
		 VALUES(?,?,?,?,?,?,?)`,
		r.IssueID, r.Stage, r.Agent, r.SessionID, r.Worktree, r.Status, r.Tokens)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishStageRun(id int64, status, sessionID string, tokens int) error {
	_, err := s.db.Exec(
		`UPDATE stage_runs SET status=?, session_id=?, tokens=? WHERE id=?`,
		status, sessionID, tokens, id)
	return err
}

func (s *Store) StageRuns(issueID string) ([]StageRun, error) {
	rows, err := s.db.Query(
		`SELECT id,issue_id,stage,agent,session_id,worktree,status,tokens
		 FROM stage_runs WHERE issue_id=? ORDER BY id`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StageRun
	for rows.Next() {
		var r StageRun
		if err := rows.Scan(&r.ID, &r.IssueID, &r.Stage, &r.Agent,
			&r.SessionID, &r.Worktree, &r.Status, &r.Tokens); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) IssueTokens(issueID string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COALESCE(SUM(tokens),0) FROM stage_runs WHERE issue_id=?`, issueID).Scan(&n)
	return n, err
}
```

Add `SessionID string` to `runner.Result` in `internal/runner/runner.go`.

In `internal/engine/engine.go` `runStageOnce`, wrap `runAgent`:

```go
	runAgent := func(a flow.AgentRef) {
		runID, insErr := e.cfg.Store.InsertStageRun(store.StageRun{
			IssueID: is.id, Stage: st.Name, Agent: a.Package,
			Worktree: workdir, Status: "running"})
		asks := make(chan runner.Ask)
		resc := e.cfg.Runner.Run(ctx, is.id, st.Name, a.Package, workdir, asks)
		for {
			select {
			case ask := <-asks:
				e.handleAsk(is, st.Name, ask)
			case res := <-resc:
				if insErr == nil {
					status := "succeeded"
					if res.Err != nil {
						status = "failed"
					}
					e.cfg.Store.FinishStageRun(runID, status, res.SessionID, res.Tokens)
				}
				dones <- agentDone{a.Package, res}
				return
			}
		}
	}
```

(Import `"github.com/weston6142/watchtower/internal/store"` in engine.go.)

- [ ] **Step 4: Run the full suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/ internal/engine/ internal/runner/
git commit -m "feat: persist stage_runs with session ids and token counts"
```

---

### Task 3: Agent package loader

**Files:**
- Create: `internal/pkgs/pkgs.go`, `internal/pkgs/testdata/executor/package.yaml`, `internal/pkgs/testdata/executor/prompt.md`
- Test: `internal/pkgs/pkgs_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  ```go
  type Package struct {
      Name         string   // directory basename
      Prompt       string   // contents of prompt.md
      AllowedTools []string `yaml:"allowed_tools"`
      Model        string   `yaml:"model"`
      MaxTurns     int      `yaml:"max_turns"` // 0 = unlimited
  }
  func LoadDir(root string) (map[string]Package, error) // every subdir with package.yaml + prompt.md
  ```

- [ ] **Step 1: Write testdata and the failing test**

```yaml
# internal/pkgs/testdata/executor/package.yaml
allowed_tools: ["Bash", "Read", "Write", "Edit"]
model: ""
max_turns: 0
```

```markdown
<!-- internal/pkgs/testdata/executor/prompt.md -->
You are the Watchtower executor. Execute the implementation plan given to you.
When you need a human decision, output a single line:
{"watchtower_decision": {"question": "...", "options": ["..."], "recommended": 0, "importance": 0.5, "paths": []}}
and wait for the reply before continuing.
```

```go
// internal/pkgs/pkgs_test.go
package pkgs

import "testing"

func TestLoadDir(t *testing.T) {
	m, err := LoadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := m["executor"]
	if !ok {
		t.Fatalf("executor missing: %v", m)
	}
	if len(p.AllowedTools) != 4 || p.AllowedTools[0] != "Bash" {
		t.Fatalf("tools: %v", p.AllowedTools)
	}
	if p.Prompt == "" || p.Name != "executor" {
		t.Fatalf("bad package: %+v", p)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/pkgs/ -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// internal/pkgs/pkgs.go
package pkgs

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Package struct {
	Name         string   `yaml:"-"`
	Prompt       string   `yaml:"-"`
	AllowedTools []string `yaml:"allowed_tools"`
	Model        string   `yaml:"model"`
	MaxTurns     int      `yaml:"max_turns"`
}

func LoadDir(root string) (map[string]Package, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := map[string]Package{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		cfgPath := filepath.Join(dir, "package.yaml")
		promptPath := filepath.Join(dir, "prompt.md")
		cfg, err := os.ReadFile(cfgPath)
		if err != nil {
			continue // not a package dir
		}
		prompt, err := os.ReadFile(promptPath)
		if err != nil {
			return nil, fmt.Errorf("package %s has package.yaml but no prompt.md", e.Name())
		}
		var p Package
		if err := yaml.Unmarshal(cfg, &p); err != nil {
			return nil, fmt.Errorf("package %s: %w", e.Name(), err)
		}
		p.Name = e.Name()
		p.Prompt = string(prompt)
		out[p.Name] = p
	}
	return out, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/pkgs/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pkgs/
git commit -m "feat: agent package loader (prompt.md + package.yaml dirs)"
```

---

### Task 4: Claude stream-json codec

**Files:**
- Create: `internal/claude/stream.go`
- Test: `internal/claude/stream_test.go`

**Interfaces:**
- Consumes: `levers.Decision`.
- Produces:
  ```go
  // One parsed line of claude --output-format stream-json output.
  type StreamEvent struct {
      Kind      string // "init"|"assistant_text"|"result"|"other"
      SessionID string // set on init
      Text      string // assistant text content (concatenated text blocks)
      Tokens    int    // set on result: usage input+output tokens
      IsError   bool   // set on result
  }
  func ParseLine(line []byte) StreamEvent
  // ExtractDecision scans assistant text for the watchtower_decision marker.
  // Returns the decision and true if found.
  func ExtractDecision(text string) (levers.Decision, bool)
  // UserMessage encodes a stream-json stdin line carrying a user text turn.
  func UserMessage(text string) []byte
  ```
- Parsing rules: a line is JSON; `{"type":"system","subtype":"init","session_id":...}` → Kind "init"; `{"type":"assistant","message":{"content":[{"type":"text","text":...}, ...]}}` → Kind "assistant_text" with all text blocks joined by "\n"; `{"type":"result", "is_error":bool, "usage":{"input_tokens":N,"output_tokens":M}}` → Kind "result", Tokens=N+M (usage may also live at `message.usage` in older CLIs — check top level first, fall back). Unparseable/other lines → Kind "other".

- [ ] **Step 1: Write the failing test**

```go
// internal/claude/stream_test.go
package claude

import (
	"strings"
	"testing"
)

func TestParseInitAssistantResult(t *testing.T) {
	init := ParseLine([]byte(`{"type":"system","subtype":"init","session_id":"s-123"}`))
	if init.Kind != "init" || init.SessionID != "s-123" {
		t.Fatalf("init: %+v", init)
	}
	at := ParseLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"},{"type":"text","text":"world"}]}}`))
	if at.Kind != "assistant_text" || at.Text != "hello\nworld" {
		t.Fatalf("assistant: %+v", at)
	}
	res := ParseLine([]byte(`{"type":"result","is_error":false,"usage":{"input_tokens":100,"output_tokens":50}}`))
	if res.Kind != "result" || res.Tokens != 150 || res.IsError {
		t.Fatalf("result: %+v", res)
	}
	if ParseLine([]byte(`garbage`)).Kind != "other" {
		t.Fatal("garbage should be other")
	}
}

func TestExtractDecision(t *testing.T) {
	text := "I need input.\n{\"watchtower_decision\": {\"question\": \"REST or GraphQL?\", \"options\": [\"REST\", \"GraphQL\"], \"recommended\": 0, \"importance\": 0.6, \"paths\": [\"api/routes.go\"]}}\n"
	d, ok := ExtractDecision(text)
	if !ok || d.Question != "REST or GraphQL?" || len(d.Options) != 2 || d.Importance != 0.6 || d.Paths[0] != "api/routes.go" {
		t.Fatalf("decision: %+v ok=%v", d, ok)
	}
	if _, ok := ExtractDecision("no marker here"); ok {
		t.Fatal("false positive")
	}
}

func TestUserMessage(t *testing.T) {
	line := string(UserMessage("go on"))
	if !strings.Contains(line, `"type":"user"`) || !strings.Contains(line, "go on") || !strings.HasSuffix(line, "\n") {
		t.Fatalf("bad user line: %q", line)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/claude/ -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// internal/claude/stream.go
package claude

import (
	"encoding/json"
	"strings"

	"github.com/weston6142/watchtower/internal/levers"
)

type StreamEvent struct {
	Kind      string
	SessionID string
	Text      string
	Tokens    int
	IsError   bool
}

type rawLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
	Usage     *usage `json:"usage"`
	Message   *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage *usage `json:"usage"`
	} `json:"message"`
}

type usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func ParseLine(line []byte) StreamEvent {
	var r rawLine
	if err := json.Unmarshal(line, &r); err != nil {
		return StreamEvent{Kind: "other"}
	}
	switch {
	case r.Type == "system" && r.Subtype == "init":
		return StreamEvent{Kind: "init", SessionID: r.SessionID}
	case r.Type == "assistant" && r.Message != nil:
		var parts []string
		for _, c := range r.Message.Content {
			if c.Type == "text" && c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		return StreamEvent{Kind: "assistant_text", Text: strings.Join(parts, "\n")}
	case r.Type == "result":
		u := r.Usage
		if u == nil && r.Message != nil {
			u = r.Message.Usage
		}
		tok := 0
		if u != nil {
			tok = u.InputTokens + u.OutputTokens
		}
		return StreamEvent{Kind: "result", Tokens: tok, IsError: r.IsError}
	default:
		return StreamEvent{Kind: "other"}
	}
}

type decisionMarker struct {
	D struct {
		Question    string   `json:"question"`
		Options     []string `json:"options"`
		Recommended int      `json:"recommended"`
		Importance  float64  `json:"importance"`
		Paths       []string `json:"paths"`
	} `json:"watchtower_decision"`
}

func ExtractDecision(text string) (levers.Decision, bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `{"watchtower_decision":`) {
			continue
		}
		var m decisionMarker
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m.D.Question == "" || len(m.D.Options) == 0 {
			continue
		}
		return levers.Decision{
			Question: m.D.Question, Options: m.D.Options,
			Recommended: m.D.Recommended, Importance: m.D.Importance,
			Paths: m.D.Paths,
		}, true
	}
	return levers.Decision{}, false
}

func UserMessage(text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": text}},
		},
	})
	return append(b, '\n')
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/claude/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/claude/
git commit -m "feat: claude stream-json codec and decision marker extraction"
```

---

### Task 5: ClaudeCodeRunner

**Files:**
- Create: `internal/claude/runner.go`, `internal/claude/testdata/happy.sh`, `internal/claude/testdata/asker.sh`, `internal/claude/testdata/failer.sh`
- Test: `internal/claude/runner_test.go`

**Interfaces:**
- Consumes: `pkgs.Package` (Task 3), codec (Task 4), `runner.Ask`/`runner.Result`/`runner.Runner` (Plan 1 + Task 2).
- Produces:
  ```go
  type CodeRunner struct {
      Bin      string                  // "claude" in prod, a stub script in tests
      Packages map[string]pkgs.Package // loaded agent packages
      ExtraEnv []string                // appended to os.Environ()
  }
  // implements runner.Runner
  func (c *CodeRunner) Run(ctx context.Context, issueID, stage, agentPkg, workdir string, asks chan<- runner.Ask) <-chan runner.Result
  ```
- Behavior: look up the package (unknown → error Result). Build args: `-p`, `--input-format stream-json`, `--output-format stream-json`, `--verbose`, `--append-system-prompt <pkg.Prompt>`, `--allowedTools <join ",">` when non-empty, `--model <pkg.Model>` when non-empty, `--max-turns <n>` when >0. `cmd.Dir = workdir`. Write the initial task as the first stdin user message: `Task: <stage> for issue <issueID>. Work in the current directory.` Then read stdout line-by-line: on `init` capture session ID; on `assistant_text` run `ExtractDecision` — if found, send an `Ask`, await the reply, write `UserMessage("Human decision: " + chosenOptionText)` to stdin; on `result` capture tokens/is_error, close stdin, wait for exit. Result: `Err` non-nil if is_error or non-zero exit; `SessionID` and `Tokens` filled; `Artifacts` left nil (the engine validates declared artifacts on disk — runner doesn't track them).
- Stub scripts (committed executable, `chmod +x`):

- [ ] **Step 1: Write stubs and the failing test**

```bash
# internal/claude/testdata/happy.sh
#!/bin/sh
# Reads and discards stdin lines in background; emits a fixed session.
cat > /dev/null &
echo '{"type":"system","subtype":"init","session_id":"s-happy"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"working..."}]}}'
echo 'spec content' > spec.md
echo '{"type":"result","is_error":false,"usage":{"input_tokens":200,"output_tokens":100}}'
```

```bash
# internal/claude/testdata/asker.sh
#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"s-ask"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"watchtower_decision\": {\"question\": \"Pick one\", \"options\": [\"a\",\"b\"], \"recommended\": 1, \"importance\": 0.7, \"paths\": []}}"}]}}'
# Wait for the reply line on stdin (the initial task line arrives first).
read _first_line
read reply
case "$reply" in
  *"Human decision: b"*) echo '{"type":"assistant","message":{"content":[{"type":"text","text":"got b"}]}}' ;;
  *) echo '{"type":"assistant","message":{"content":[{"type":"text","text":"got other"}]}}' ;;
esac
echo '{"type":"result","is_error":false,"usage":{"input_tokens":10,"output_tokens":5}}'
```

```bash
# internal/claude/testdata/failer.sh
#!/bin/sh
cat > /dev/null &
echo '{"type":"system","subtype":"init","session_id":"s-fail"}'
echo '{"type":"result","is_error":true,"usage":{"input_tokens":1,"output_tokens":1}}'
```

```bash
chmod +x internal/claude/testdata/*.sh
```

```go
// internal/claude/runner_test.go
package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/runner"
)

func testPkgs() map[string]pkgs.Package {
	return map[string]pkgs.Package{
		"spec-writer": {Name: "spec-writer", Prompt: "write specs"},
	}
}

func run(t *testing.T, bin string, dir string) (<-chan runner.Result, chan runner.Ask) {
	t.Helper()
	c := &CodeRunner{Bin: bin, Packages: testPkgs()}
	asks := make(chan runner.Ask, 1)
	return c.Run(context.Background(), "GH-1", "spec", "spec-writer", dir, asks), asks
}

func TestHappyPathProducesArtifactAndTokens(t *testing.T) {
	dir := t.TempDir()
	done, _ := run(t, abs(t, "testdata/happy.sh"), dir)
	res := <-done
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.SessionID != "s-happy" || res.Tokens != 300 {
		t.Fatalf("res: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "spec.md")); err != nil {
		t.Fatal("stub should have written spec.md in workdir")
	}
}

func TestDecisionRoundTrip(t *testing.T) {
	done, asks := run(t, abs(t, "testdata/asker.sh"), t.TempDir())
	a := <-asks
	if a.Decision.Question != "Pick one" || a.Decision.Recommended != 1 {
		t.Fatalf("ask: %+v", a.Decision)
	}
	a.Reply <- 1 // choose "b"
	res := <-done
	if res.Err != nil || res.SessionID != "s-ask" {
		t.Fatalf("res: %+v", res)
	}
}

func TestErrorResultFails(t *testing.T) {
	done, _ := run(t, abs(t, "testdata/failer.sh"), t.TempDir())
	if res := <-done; res.Err == nil {
		t.Fatal("expected error result")
	}
}

func abs(t *testing.T, rel string) string {
	t.Helper()
	p, err := filepath.Abs(rel)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/claude/ -run 'Happy|Decision|Error' -v`
Expected: FAIL (undefined `CodeRunner`).

- [ ] **Step 3: Implement**

```go
// internal/claude/runner.go
package claude

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/runner"
)

type CodeRunner struct {
	Bin      string
	Packages map[string]pkgs.Package
	ExtraEnv []string
}

func (c *CodeRunner) Run(ctx context.Context, issueID, stage, agentPkg, workdir string,
	asks chan<- runner.Ask) <-chan runner.Result {
	done := make(chan runner.Result, 1)
	go func() {
		done <- c.run(ctx, issueID, stage, agentPkg, workdir, asks)
	}()
	return done
}

func (c *CodeRunner) run(ctx context.Context, issueID, stage, agentPkg, workdir string,
	asks chan<- runner.Ask) runner.Result {
	pkg, ok := c.Packages[agentPkg]
	if !ok {
		return runner.Result{Err: fmt.Errorf("unknown agent package %q", agentPkg)}
	}
	args := []string{"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--append-system-prompt", pkg.Prompt,
	}
	if len(pkg.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(pkg.AllowedTools, ","))
	}
	if pkg.Model != "" {
		args = append(args, "--model", pkg.Model)
	}
	if pkg.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(pkg.MaxTurns))
	}

	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), c.ExtraEnv...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return runner.Result{Err: err}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runner.Result{Err: err}
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return runner.Result{Err: err}
	}

	task := fmt.Sprintf("Task: %s for issue %s. Work in the current directory.", stage, issueID)
	if _, err := stdin.Write(UserMessage(task)); err != nil {
		cmd.Process.Kill()
		return runner.Result{Err: err}
	}

	var res runner.Result
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	gotResult := false
	for sc.Scan() {
		ev := ParseLine(sc.Bytes())
		switch ev.Kind {
		case "init":
			res.SessionID = ev.SessionID
		case "assistant_text":
			if d, found := ExtractDecision(ev.Text); found {
				reply := make(chan int, 1)
				select {
				case asks <- runner.Ask{Decision: d, Reply: reply}:
				case <-ctx.Done():
					cmd.Process.Kill()
					res.Err = ctx.Err()
					return res
				}
				var choice int
				select {
				case choice = <-reply:
				case <-ctx.Done():
					cmd.Process.Kill()
					res.Err = ctx.Err()
					return res
				}
				opt := ""
				if choice >= 0 && choice < len(d.Options) {
					opt = d.Options[choice]
				}
				if _, err := stdin.Write(UserMessage("Human decision: " + opt)); err != nil {
					cmd.Process.Kill()
					res.Err = err
					return res
				}
			}
		case "result":
			res.Tokens = ev.Tokens
			gotResult = true
			if ev.IsError {
				res.Err = fmt.Errorf("claude session %s ended with error", res.SessionID)
			}
		}
	}
	stdin.Close()
	waitErr := cmd.Wait()
	if res.Err == nil && waitErr != nil {
		res.Err = fmt.Errorf("claude exited: %w", waitErr)
	}
	if res.Err == nil && !gotResult {
		res.Err = fmt.Errorf("claude session %s ended without result event", res.SessionID)
	}
	return res
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/claude/ -race -v`
Expected: PASS (all codec + runner tests).

- [ ] **Step 5: Commit**

```bash
git add internal/claude/
git commit -m "feat: ClaudeCodeRunner drives headless claude sessions over stream-json"
```

---

### Task 6: Workspace provider (treehouse + git-worktree fallback)

**Files:**
- Create: `internal/workspace/workspace.go`
- Test: `internal/workspace/workspace_test.go`

**Interfaces:**
- Consumes: nothing internal.
- Produces:
  ```go
  type Provider interface {
      Acquire(issueID string) (path string, release func() error, err error)
  }
  // GitWorktree creates <repo>/.worktrees/<issueID> on branch issue/<issueID>.
  type GitWorktree struct{ Repo string }
  // Treehouse shells out to the treehouse CLI; used when `treehouse` is on PATH.
  type Treehouse struct{ Repo string }
  func Detect(repo string) Provider // Treehouse if binary found, else GitWorktree
  ```
- `GitWorktree.Acquire`: `git -C <repo> worktree add <repo>/.worktrees/<id> -b issue/<id>` (if branch exists, `-B` is NOT used — return error; the engine treats it as stage failure). Release: `git -C <repo> worktree remove --force <path>` (branch is kept — merge handling is Plan 3's Marshal).
- `Treehouse.Acquire`: `treehouse claim --repo <repo> --name <id>` printing the path on stdout; release runs `treehouse return <path>`. Only the GitWorktree path is unit-tested; Treehouse is a thin exec wrapper verified in the smoke test if installed.

- [ ] **Step 1: Write the failing test**

```go
// internal/workspace/workspace_test.go
package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"},
		{"commit", "--allow-empty", "-q", "-m", "root"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func TestGitWorktreeAcquireRelease(t *testing.T) {
	repo := initRepo(t)
	p := GitWorktree{Repo: repo}
	path, release, err := p.Acquire("GH-9")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != filepath.Join(repo, ".worktrees") {
		t.Fatalf("unexpected path %s", path)
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		t.Fatal("not a worktree")
	}
	// second acquire of same issue must fail (branch exists)
	if _, _, err := p.Acquire("GH-9"); err == nil {
		t.Fatal("expected duplicate acquire to fail")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("worktree not removed")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/workspace/ -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// internal/workspace/workspace.go
package workspace

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

type Provider interface {
	Acquire(issueID string) (path string, release func() error, err error)
}

type GitWorktree struct{ Repo string }

func (g GitWorktree) Acquire(issueID string) (string, func() error, error) {
	path := filepath.Join(g.Repo, ".worktrees", issueID)
	branch := "issue/" + issueID
	out, err := exec.Command("git", "-C", g.Repo, "worktree", "add", path, "-b", branch).CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("worktree add: %v: %s", err, out)
	}
	release := func() error {
		out, err := exec.Command("git", "-C", g.Repo, "worktree", "remove", "--force", path).CombinedOutput()
		if err != nil {
			return fmt.Errorf("worktree remove: %v: %s", err, out)
		}
		return nil
	}
	return path, release, nil
}

type Treehouse struct{ Repo string }

func (t Treehouse) Acquire(issueID string) (string, func() error, error) {
	out, err := exec.Command("treehouse", "claim", "--repo", t.Repo, "--name", issueID).CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("treehouse claim: %v: %s", err, out)
	}
	path := strings.TrimSpace(string(out))
	release := func() error {
		out, err := exec.Command("treehouse", "return", path).CombinedOutput()
		if err != nil {
			return fmt.Errorf("treehouse return: %v: %s", err, out)
		}
		return nil
	}
	return path, release, nil
}

func Detect(repo string) Provider {
	if _, err := exec.LookPath("treehouse"); err == nil {
		return Treehouse{Repo: repo}
	}
	return GitWorktree{Repo: repo}
}
```

**Verify the treehouse CLI verbs before executing this task**: run `treehouse --help` — if claim/return differ (e.g. `lease`/`release`), adjust `Treehouse` accordingly and note it in the commit message. The `Provider` interface and `GitWorktree` are the tested contract.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/workspace/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/workspace/
git commit -m "feat: workspace provider with treehouse and git-worktree fallback"
```

---

### Task 7: Engine worktree integration + token budgets

**Files:**
- Modify: `internal/engine/engine.go` (Config + runStage/runStageOnce/StartIssue)
- Test: `internal/engine/engine_test.go` (extend)

**Interfaces:**
- Consumes: `workspace.Provider` (Task 6), `Store.IssueTokens` (Task 2), `core.EvBudgetExceeded` (Task 1).
- Produces: `engine.Config` gains `Workspace workspace.Provider` (nil = keep Plan 1's DataDir behavior) and `TokenBudget int` (0 = unlimited). Behavior:
  - `StartIssue` acquires the workspace once, lazily, at the first stage with `Workspace != "none"`, and releases it after the flow ends (success or failure). Stages with `workspace: worktree|readonly` get `workdir = <acquired path>`; `none` stages keep the DataDir path.
  - Before each stage, if `TokenBudget > 0` and `Store.IssueTokens(id) > TokenBudget`, emit `budget_exceeded` and escalate an importance-1.0 decision: question `"Issue <id> exceeded its token budget (<spent>/<budget>). Continue?"`, options `["continue","abort"]`, recommended 1. "abort" → issue fails with an error; "continue" → proceed (budget check suppressed for the rest of the flow via an in-memory flag on issueState).

- [ ] **Step 1: Write the failing test**

Append to `internal/engine/engine_test.go`:

```go
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
	errc := make(chan error, 1)
	go func() { errc <- e.StartIssue(context.Background(), id) }()
	for {
		if ds := e.PendingDecisions(); len(ds) == 1 {
			e.Answer(ds[0].ID, 0)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-errc; err != nil {
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
	errc := make(chan error, 1)
	go func() { errc <- e.StartIssue(context.Background(), id) }()

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
	if err := <-errc; err == nil {
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
```

(Add `"strings"` to the test imports. `e.cfg` is accessible within the package — tests live in package `engine`.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/engine/ -run 'Worktree|Budget' -v`
Expected: FAIL (no `Workspace`/`TokenBudget` fields).

- [ ] **Step 3: Implement**

In `internal/engine/engine.go`:

Add to `Config`:

```go
	Workspace   workspace.Provider // nil = DataDir sandbox only
	TokenBudget int                // per-issue; 0 = unlimited
```

Add to `issueState`:

```go
	wsPath        string
	wsRelease     func() error
	budgetWaived  bool
```

Change `runStageOnce`'s workdir selection:

```go
	workdir := filepath.Join(e.cfg.DataDir, is.id, st.Name)
	if st.Workspace != "none" && is.wsPath != "" {
		workdir = is.wsPath
	}
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return err
	}
```

In `StartIssue`, replace the stage loop:

```go
	defer func() {
		if is.wsRelease != nil {
			is.wsRelease()
			is.wsRelease = nil
		}
	}()
	for _, st := range f.Stages {
		if st.Workspace != "none" && e.cfg.Workspace != nil && is.wsPath == "" {
			path, release, err := e.cfg.Workspace.Acquire(is.id)
			if err != nil {
				e.emit(core.EvStageFailed, is.id, map[string]string{"stage": st.Name, "error": "workspace: " + err.Error()})
				return err
			}
			is.wsPath, is.wsRelease = path, release
		}
		if err := e.checkBudget(is, st.Name); err != nil {
			return err
		}
		if err := e.runStage(ctx, is, st); err != nil {
			return err
		}
	}
	e.emit(core.EvIssueCompleted, id, nil)
	return nil
```

Add:

```go
func (e *Engine) checkBudget(is *issueState, stage string) error {
	if e.cfg.TokenBudget <= 0 || is.budgetWaived {
		return nil
	}
	spent, err := e.cfg.Store.IssueTokens(is.id)
	if err != nil || spent <= e.cfg.TokenBudget {
		return nil
	}
	e.emit(core.EvBudgetExceeded, is.id, map[string]any{"spent": spent, "budget": e.cfg.TokenBudget})
	d := levers.Decision{
		Question:    fmt.Sprintf("Issue %s exceeded its token budget (%d/%d). Continue?", is.id, spent, e.cfg.TokenBudget),
		Options:     []string{"continue", "abort"},
		Recommended: 1,
		Importance:  1.0,
	}
	if e.escalate(is.id, stage, d) == 0 {
		is.budgetWaived = true
		return nil
	}
	err = fmt.Errorf("issue %s aborted: token budget exceeded", is.id)
	e.emit(core.EvStageFailed, is.id, map[string]string{"stage": stage, "error": err.Error()})
	return err
}
```

(Import `workspace` package. Note `EvIssueCompleted` emission moves inside this rewritten loop end — Task 1 added it at the same spot.)

- [ ] **Step 4: Run the full suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/
git commit -m "feat: engine acquires real worktrees and enforces token budgets"
```

---

### Task 8: Shipped flow + packages, daemon wiring, smoke test

**Files:**
- Create: `dist/flows/default.yaml`, `dist/packages/{brainstorm,spec-writer,planner,executor,clean-code-reviewer,reviewer,doc-writer}/{package.yaml,prompt.md}` (14 files)
- Modify: `cmd/watchtower/main.go`
- Test: manual smoke (stub binary + optional real Claude).

**Interfaces:**
- Consumes: everything.
- Produces: `watchtower daemon` gains `--runner claude|fake` (default `claude`), `--repo PATH` (target repo for worktree stages; required when `--runner claude`), `--packages DIR` (default `dist/packages` relative to the binary's working directory), `--budget N` (per-issue token budget, default 0 = off), `--claude-bin` (default `claude`). The `WATCHTOWER_FAKE=1` gate is removed (`--runner fake` replaces it; keep reading the env var as a fallback alias for compatibility with Plan 1 scripts).

- [ ] **Step 1: Write the shipped flow**

```yaml
# dist/flows/default.yaml
name: default
stages:
  - name: brainstorm
    agents: [{package: brainstorm}]
    gate: decision_queue
    artifacts: [brainstorm.md]
  - name: spec
    agents: [{package: spec-writer}]
    gate: approve_artifact
    artifacts: [spec.md]
  - name: plan
    agents: [{package: planner}]
    gate: approve_artifact
    artifacts: [plan.md]
  - name: execute
    agents: [{package: executor}]
    workspace: worktree
    gate: auto
    heavy_slot: true
    retries: 1
    artifacts: []
  - name: review
    agents: [{package: clean-code-reviewer}, {package: reviewer}, {package: doc-writer}]
    parallel: true
    completion: all
    workspace: worktree
    gate: auto
    heavy_slot: true
    artifacts: []
  - name: merge
    agents: [{package: reviewer}]
    workspace: readonly
    gate: approve_artifact
    artifacts: [merge-report.md]
```

(Note: the merge stage is a placeholder gate until Plan 3's Merge Marshal replaces it — the reviewer package summarizes the branch diff into `merge-report.md` and the `approve_artifact` gate makes the human the merge authority for now.)

- [ ] **Step 2: Write the seven agent packages**

Every `package.yaml` follows this shape (tools vary as listed):

```yaml
# dist/packages/brainstorm/package.yaml
allowed_tools: ["Read", "Glob", "Grep"]
model: ""
max_turns: 0
```

Tool lists per package: `brainstorm` Read,Glob,Grep · `spec-writer` Read,Glob,Grep,Write · `planner` Read,Glob,Grep,Write · `executor` Bash,Read,Write,Edit,Glob,Grep · `clean-code-reviewer` Bash,Read,Edit,Glob,Grep · `reviewer` Bash,Read,Edit,Glob,Grep,Write · `doc-writer` Read,Write,Edit,Glob,Grep.

The prompts (each `prompt.md`; write all seven, adapting the role sentence and output contract):

```markdown
<!-- dist/packages/brainstorm/prompt.md -->
You are the Watchtower brainstorm agent. Your job: refine the issue you are
given into validated requirements by asking sharp questions and settling
design choices.

Decision protocol: whenever a choice needs human judgment, output a single
line, alone in a message:
{"watchtower_decision": {"question": "<plain-English question>", "options": ["<opt-a>", "<opt-b>"], "recommended": 0, "importance": <0.0-1.0>, "paths": ["<files this affects>"]}}
Importance calibration: 1.0 = destructive/security/spend/public-API (always
escalates); 0.6-0.8 = design choices that shape the feature; 0.3-0.5 =
preferences with a sane default; <0.3 = trivia (avoid asking these).
Wait for the "Human decision: ..." reply before continuing. The reply may
be auto-chosen; treat it as final either way.

When requirements are settled, write brainstorm.md in the current directory:
a summary of the validated idea, the decisions made (with their answers),
and open risks. Then stop.
```

```markdown
<!-- dist/packages/spec-writer/prompt.md -->
You are the Watchtower spec writer. Read brainstorm.md in the current
directory and produce spec.md: goals, non-goals, architecture, data
model, error handling, testing strategy. Be concrete; no placeholders.
Use the same watchtower_decision protocol as other agents (single JSON line,
wait for reply) if a genuine ambiguity blocks the spec. Then stop.
```

```markdown
<!-- dist/packages/planner/prompt.md -->
You are the Watchtower planner. Read spec.md and produce plan.md: an ordered
list of bite-sized TDD tasks (write failing test, run to confirm fail,
implement, run to confirm pass, commit) with exact file paths and real code
in every step. No placeholders, no "TBD". Use the watchtower_decision
protocol for genuine blockers only. Then stop.
```

```markdown
<!-- dist/packages/executor/prompt.md -->
You are the Watchtower executor working in a dedicated git worktree. Execute
plan.md task by task, in order, following each TDD step exactly. Commit
after each task with the message the plan specifies. Run the full test
suite before finishing; if it fails, fix it before stopping. Use the
watchtower_decision protocol when the plan is wrong or a real choice
appears; importance 1.0 for anything destructive.
```

```markdown
<!-- dist/packages/clean-code-reviewer/prompt.md -->
You are the Watchtower clean-code reviewer in the issue's worktree. Review
the branch diff (git diff against the default branch). Apply safe
cleanliness fixes directly (naming, dead code, comments, small
simplifications) and commit them. Never change public interfaces or
behavior. Run the test suite after changes. Summarize fixed/skipped at the
end of your final message.
```

```markdown
<!-- dist/packages/reviewer/prompt.md -->
You are the Watchtower general reviewer in the issue's worktree. Hunt for
real defects in the branch diff: correctness, concurrency, error handling,
edge cases. Fix what you find and commit; use the watchtower_decision
protocol (importance 0.8+) when a fix requires a design call. When invoked
at the merge stage (read-only), instead write merge-report.md: diff
summary, test status, risks, and a merge/hold recommendation.
```

```markdown
<!-- dist/packages/doc-writer/prompt.md -->
You are the Watchtower documentation agent in the issue's worktree. Update
or draft the docs this change needs (README sections, ADRs, inline doc
comments) and commit them. Match the repository's existing documentation
style. Do not touch non-documentation code.
```

- [ ] **Step 3: Wire the daemon**

Modify `runDaemon` in `cmd/watchtower/main.go`: add flags and construct the runner/workspace:

```go
	runnerKind := fs.String("runner", "claude", "claude|fake")
	repo := fs.String("repo", "", "target repo (required for --runner claude)")
	pkgDir := fs.String("packages", "dist/packages", "agent packages dir")
	budget := fs.Int("budget", 0, "per-issue token budget (0=off)")
	claudeBin := fs.String("claude-bin", "claude", "claude binary")
```

After flag parsing (replacing the `WATCHTOWER_FAKE` block):

```go
	if os.Getenv("WATCHTOWER_FAKE") == "1" {
		*runnerKind = "fake"
	}
	var run runner.Runner
	var ws workspace.Provider
	switch *runnerKind {
	case "fake":
		run = fakeForFlows(flows) // ws stays nil: DataDir sandbox
	case "claude":
		if *repo == "" {
			fatal(fmt.Errorf("--repo is required with --runner claude"))
		}
		packages, err := pkgs.LoadDir(*pkgDir)
		if err != nil {
			fatal(err)
		}
		run = &claude.CodeRunner{Bin: *claudeBin, Packages: packages}
		ws = workspace.Detect(*repo)
	default:
		fatal(fmt.Errorf("unknown runner %q", *runnerKind))
	}
```

And pass into the engine config: `Runner: run, Workspace: ws, TokenBudget: *budget`.

- [ ] **Step 4: Build, full suite, and smoke tests**

```bash
go build ./... && go test ./... -race
```

Stub-binary smoke (no API cost) — same flow as Plan 1's smoke but through the fake runner path:

```bash
rm -rf /tmp/gh-smoke2 && mkdir -p /tmp/gh-smoke2/flows
cp dist/flows/default.yaml /tmp/gh-smoke2/flows/
go run ./cmd/watchtower daemon --runner fake --data /tmp/gh-smoke2 --flows /tmp/gh-smoke2/flows &
sleep 1
go run ./cmd/watchtower new --data /tmp/gh-smoke2 --title "smoke" --preset yolo
sleep 1
go run ./cmd/watchtower decisions --data /tmp/gh-smoke2   # spec gate, then answer; then plan gate; then merge gate
# answer each gate with option 0 until tail shows issue_completed (6 stage_completed events)
```

Real-Claude smoke (manual, costs tokens — run once, against a scratch repo):

```bash
mkdir -p /tmp/gh-target && cd /tmp/gh-target && git init -q && git commit --allow-empty -qm root && cd ~/watchtower
go run ./cmd/watchtower daemon --runner claude --repo /tmp/gh-target --data /tmp/gh-real --flows dist/flows --packages dist/packages --budget 200000 &
go run ./cmd/watchtower new --data /tmp/gh-real --title "Add a hello CLI in Go that prints a greeting" --preset regular
# Watch: watchtower decisions / answer / tail. Expect brainstorm questions to
# surface (regular preset), artifacts to appear under /tmp/gh-real/issues/GH-1/,
# and a worktree under /tmp/gh-target/.worktrees/GH-1 during execute.
```

Expected: stub smoke reaches `issue_completed` with 6 completed stages; real smoke produces brainstorm.md/spec.md/plan.md, an execute worktree with commits, and stops at each `approve_artifact` gate for you.

- [ ] **Step 5: Commit**

```bash
git add dist/ cmd/watchtower/
git commit -m "feat: shipped default flow + agent packages, claude runner wiring in daemon"
```

---

## Self-review notes

- **Spec coverage:** ClaudeCodeRunner behind unchanged Runner interface (T5), agent packages as prompt+config dirs wrapping the superpowers-style roles (T3, T8), treehouse-or-git worktrees for worktree stages (T6, T7), decision protocol connecting live agents to the lever/queue machinery (T4, T5), token budgets escalating to the queue (T7), deferred Plan 1 items closed: `issue_completed` (T1), `stage_runs` persistence with sessions + tokens (T2). Still deferred by design: overlords, proposals, blocking-cost ordering, `decisions` table persistence (Plan 3); TUI (Plan 4).
- **Verify-before-build flags:** two externally-owned surfaces must be checked at execution time against the installed versions — the treehouse CLI verbs (noted in T6) and the exact Claude CLI stream-json flag set (`--input-format stream-json` requires `-p` with `--output-format stream-json --verbose`; confirm with `claude --help` before T5's commit and adjust args in one place, `runner.go`). The codec tests are fixture-based and survive CLI drift.
- **Type consistency:** `runner.Result.SessionID` added in T2 and consumed in T5 ✓; `engine.Config.Workspace`/`TokenBudget` defined in T7, wired in T8 ✓; `pkgs.Package` consumed by `claude.CodeRunner` ✓; stage names in `dist/flows/default.yaml` match the FakeRunner script generator (it derives scripts from the flow, so new stage names work unchanged) ✓ — note `fakeForFlows` writes every declared artifact, so the stub smoke passes all six stages including `merge-report.md`.
