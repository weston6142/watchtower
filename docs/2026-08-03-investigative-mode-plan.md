# Investigative Mode (GH-5) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pressing `i` in the watchtower TUI opens a picker (agent/model/effort), then spawns a real interactive claude or codex session in a new herdr pane with a structured exit protocol (discard / save draft / save + launch).

**Architecture:** A new `SpawnPane` call in `internal/herdr` splits a pane over the existing socket API and types the agent command into it. A new `internal/tui/investigate.go` owns the picker modal, command construction, the injected protocol prompt, per-repo last-used persistence, and the claude wrap-up skill installer. `app.go` routes the `i` key and Enter/Esc; `cmd/watchtower/main.go` wires the spawner and repo root.

**Tech Stack:** Go, Bubble Tea/lipgloss (existing), herdr NDJSON unix-socket API.

## Global Constraints

- Spec: `docs/2026-08-03-investigative-mode-design.md`.
- Fire-and-forget: no daemon involvement, no tracking of spawned panes.
- Spawn errors must surface in the TUI status line **including the manual command** so the operator can run it by hand.
- All herdr traffic goes through `internal/herdr` (no direct socket code in the TUI).
- Run `gofmt -l` clean; run the full suite `go test ./...` before finishing.
- Commit messages follow repo convention (`feat(tui): …`, `feat(herdr): …`).

---

### Task 1: herdr pane spawning

**Files:**
- Create: `internal/herdr/spawn.go`
- Test: `internal/herdr/spawn_test.go`
- Reference: `internal/herdr/herdr.go` (Reporter, `write`), `internal/herdr/herdr_test.go` (fake socket server pattern)

**Interfaces:**
- Produces: `func (r *Reporter) SpawnPane(cwd, command string) error` — splits a pane to the right of the TUI's pane (`focus:true`), then types `command + "\n"` into it. Returns an error (never swallows) when disabled, on dial/write failure, on an error response, or when no pane id can be parsed.

**Protocol facts (verified against `herdr api schema`):**
- Request: `{"id": "...", "method": "pane.split", "params": {"direction": "right", "cwd": "<cwd>", "focus": true, "target_pane_id": "<HERDR_PANE_ID>"}}`
- Success response carries a `result` object shaped like `PaneInfo` (`pane_id` at top level). Parse `result.pane_id`; fall back to `result.pane.pane_id` if empty.
- Then: `{"method": "pane.send_text", "params": {"pane_id": "<new>", "text": "<command>\n"}}`

- [ ] **Step 1: Write the failing test**

In `internal/herdr/spawn_test.go`, reuse the fake unix-socket server pattern from `herdr_test.go` (a goroutine accepting connections, reading one NDJSON line, recording it, replying). The spawn server replies to `pane.split` with `{"id":"x","result":{"pane_id":"w1:p9"}}` and to `pane.send_text` with `{"id":"x","result":{}}`.

```go
func TestSpawnPane(t *testing.T) {
	reqs, sock := startFakeSpawnServer(t) // returns recorded requests + socket path
	r := New(sock, "w1:p1")
	if err := r.SpawnPane("/repo", "exec claude"); err != nil {
		t.Fatalf("SpawnPane: %v", err)
	}
	got := drain(reqs, 2)
	if got[0]["method"] != "pane.split" {
		t.Fatalf("first request = %v, want pane.split", got[0]["method"])
	}
	p := got[0]["params"].(map[string]any)
	if p["cwd"] != "/repo" || p["direction"] != "right" || p["focus"] != true || p["target_pane_id"] != "w1:p1" {
		t.Fatalf("split params = %v", p)
	}
	if got[1]["method"] != "pane.send_text" {
		t.Fatalf("second request = %v, want pane.send_text", got[1]["method"])
	}
	sp := got[1]["params"].(map[string]any)
	if sp["pane_id"] != "w1:p9" || sp["text"] != "exec claude\n" {
		t.Fatalf("send_text params = %v", sp)
	}
}

func TestSpawnPaneDisabled(t *testing.T) {
	if err := New("", "").SpawnPane("/repo", "x"); err == nil {
		t.Fatal("disabled reporter must return an error, not nil")
	}
}

func TestSpawnPaneErrorResponse(t *testing.T) {
	// fake server replies {"id":"x","error":{"message":"no such workspace"}}
	sock := startFakeErrorServer(t)
	if err := New(sock, "w1:p1").SpawnPane("/repo", "x"); err == nil {
		t.Fatal("error response must surface")
	}
}
```

- [ ] **Step 2: Run tests, verify they fail** — `go test ./internal/herdr/ -run TestSpawnPane -v` → FAIL: `SpawnPane` undefined.

- [ ] **Step 3: Implement `internal/herdr/spawn.go`**

Unlike `write` (fire-and-forget, response discarded), spawning needs the response. Add a private `call` that sends one request and decodes the reply line:

```go
package herdr

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// SpawnPane splits a new pane beside the TUI's pane, focused, rooted at cwd,
// and types command into it. Unlike agent-state reporting, failures surface:
// the caller shows the operator a manual fallback.
func (r *Reporter) SpawnPane(cwd, command string) error {
	if !r.enabled() {
		return fmt.Errorf("not running under herdr")
	}
	res, err := r.call("pane.split", map[string]any{
		"direction":      "right",
		"cwd":            cwd,
		"focus":          true,
		"target_pane_id": r.paneID,
	})
	if err != nil {
		return fmt.Errorf("pane.split: %w", err)
	}
	paneID := paneIDFrom(res)
	if paneID == "" {
		return fmt.Errorf("pane.split: no pane_id in response")
	}
	if _, err := r.call("pane.send_text", map[string]any{
		"pane_id": paneID,
		"text":    command + "\n",
	}); err != nil {
		return fmt.Errorf("pane.send_text: %w", err)
	}
	return nil
}

func paneIDFrom(result map[string]any) string {
	if id, ok := result["pane_id"].(string); ok && id != "" {
		return id
	}
	if pane, ok := result["pane"].(map[string]any); ok {
		if id, ok := pane["pane_id"].(string); ok {
			return id
		}
	}
	return ""
}

// call sends one NDJSON request and decodes the single response line.
func (r *Reporter) call(method string, params map[string]any) (map[string]any, error) {
	conn, err := net.DialTimeout("unix", r.socketPath, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	req := map[string]any{
		"id":     fmt.Sprintf("watchtower:%d", time.Now().UnixNano()),
		"method": method,
		"params": params,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%s: %s", method, resp.Error.Message)
	}
	if resp.Result == nil {
		resp.Result = map[string]any{}
	}
	return resp.Result, nil
}
```

Note the spawn timeout: pane creation can be slower than the 500 ms report timeout; if tests or manual runs show flakiness, use a local `const spawnTimeout = 2 * time.Second` in `call` instead of `timeout`.

- [ ] **Step 4: Run tests, verify pass** — `go test ./internal/herdr/ -v` → all PASS (including existing reporter tests).

- [ ] **Step 5: Commit**

```bash
git add internal/herdr/spawn.go internal/herdr/spawn_test.go
git commit -m "feat(herdr): spawn a focused pane and type a command into it"
```

---

### Task 2: investigate picker state, command building, prompt, prefs, skill installer

**Files:**
- Create: `internal/tui/investigate.go`
- Test: `internal/tui/investigate_test.go`
- Reference: `internal/tui/modal.go` (`modalChoiceField`, `renderBox`, `keyChip`), `internal/repocfg/repocfg.go` (`ConfigPath` shows the `.watchtower` dir convention)

**Interfaces:**
- Produces (all in package `tui`):
  - `type investigateState struct { Agent, Model, Effort int; Field int }` (indices into the option slices)
  - `var investigateAgents = []string{"claude", "codex"}`
  - `var investigateModels = map[string][]string{"claude": {"fable-5", "opus-5", "sonnet-5"}, "codex": {"gpt-5.6-luna", "gpt-5.4"}}`
  - `var investigateEfforts = []string{"low", "medium", "high", "xhigh"}`
  - `func (s investigateState) agent() string`, `func (s investigateState) model() string`, `func (s investigateState) effort() string` — current selections (model index clamped to the active agent's list)
  - `func (s investigateState) cycle(delta int) investigateState` — adjust the focused field; changing Agent resets Model to 0
  - `func buildInvestigateCommand(agent, model, effort string) string`
  - `func renderInvestigate(s investigateState, width int) string`
  - `func loadInvestigatePrefs(repo string) investigateState` / `func saveInvestigatePrefs(repo string, s investigateState)` — JSON at `<repo>/.watchtower/investigate.json`; load returns zero state on any error, save is best-effort (errors ignored)
  - `func ensureWrapUpSkill(repo string) error` — writes `<repo>/.claude/skills/watchtower-wrap-up/SKILL.md` if absent
  - `const investigationPrompt` (the protocol text below)

**Command shapes (flags verified against installed CLIs):**
- claude: `exec claude --model <m> --effort <e> --append-system-prompt '<investigationPrompt>'`
- codex: `exec codex -m <m> -c model_reasoning_effort=<e> '<investigationPrompt>'` (codex has no system-prompt flag; the protocol goes in as the initial prompt)
- `exec` replaces the pane's shell so quitting the agent closes the pane.
- Single-quote the prompt with `'` → `'\''` escaping (write a tiny `shellQuote(s string) string` helper).

- [ ] **Step 1: Write the failing tests**

```go
func TestInvestigateCycleAgentResetsModel(t *testing.T) {
	s := investigateState{Field: 0, Model: 2}
	s = s.cycle(1) // claude -> codex
	if s.agent() != "codex" || s.Model != 0 {
		t.Fatalf("agent=%s model=%d, want codex/0", s.agent(), s.Model)
	}
}

func TestBuildInvestigateCommandClaude(t *testing.T) {
	got := buildInvestigateCommand("claude", "opus-5", "low")
	want := "exec claude --model opus-5 --effort low --append-system-prompt " + shellQuote(investigationPrompt)
	if got != want {
		t.Fatalf("got %q", got)
	}
}

func TestBuildInvestigateCommandCodex(t *testing.T) {
	got := buildInvestigateCommand("codex", "gpt-5.6-luna", "xhigh")
	want := "exec codex -m gpt-5.6-luna -c model_reasoning_effort=xhigh " + shellQuote(investigationPrompt)
	if got != want {
		t.Fatalf("got %q", got)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Fatalf("got %q", got)
	}
}

func TestInvestigatePrefsRoundTrip(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".watchtower"), 0o755); err != nil {
		t.Fatal(err)
	}
	saveInvestigatePrefs(repo, investigateState{Agent: 1, Model: 1, Effort: 3})
	s := loadInvestigatePrefs(repo)
	if s.Agent != 1 || s.Model != 1 || s.Effort != 3 {
		t.Fatalf("round trip = %+v", s)
	}
}

func TestLoadInvestigatePrefsMissing(t *testing.T) {
	s := loadInvestigatePrefs(t.TempDir())
	if s != (investigateState{}) {
		t.Fatalf("missing prefs should zero, got %+v", s)
	}
}

func TestEnsureWrapUpSkill(t *testing.T) {
	repo := t.TempDir()
	if err := ensureWrapUpSkill(repo); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, ".claude", "skills", "watchtower-wrap-up", "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "watchtower new") {
		t.Fatal("skill must reference watchtower new")
	}
	// Second call must not error and must not truncate.
	if err := ensureWrapUpSkill(repo); err != nil {
		t.Fatal(err)
	}
}

func TestRenderInvestigate(t *testing.T) {
	out := renderInvestigate(investigateState{}, 80)
	for _, want := range []string{"investigate", "AGENT", "MODEL", "EFFORT", "claude", "fable-5", "low"} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing %q:\n%s", want, out)
		}
	}
}
```

- [ ] **Step 2: Run, verify fail** — `go test ./internal/tui/ -run 'Investigate|ShellQuote|WrapUpSkill' -v` → FAIL: undefined symbols.

- [ ] **Step 3: Implement `internal/tui/investigate.go`**

State/cycling/prefs/quote:

```go
package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	investigateAgents = []string{"claude", "codex"}
	investigateModels = map[string][]string{
		"claude": {"fable-5", "opus-5", "sonnet-5"},
		"codex":  {"gpt-5.6-luna", "gpt-5.4"},
	}
	investigateEfforts = []string{"low", "medium", "high", "xhigh"}
)

// investigateState is a pure chooser: three fixed-choice fields, no text.
type investigateState struct {
	Agent  int `json:"agent"`
	Model  int `json:"model"`
	Effort int `json:"effort"`
	Field  int `json:"-"`
}

const investigateFieldCount = 3

func (s investigateState) agent() string { return investigateAgents[clampIdx(s.Agent, len(investigateAgents))] }

func (s investigateState) model() string {
	models := investigateModels[s.agent()]
	return models[clampIdx(s.Model, len(models))]
}

func (s investigateState) effort() string {
	return investigateEfforts[clampIdx(s.Effort, len(investigateEfforts))]
}

func clampIdx(i, n int) int {
	if i < 0 || i >= n {
		return 0
	}
	return i
}

func cycleIdx(i, delta, n int) int { return ((clampIdx(i, n) + delta) % n + n) % n }

func (s investigateState) cycle(delta int) investigateState {
	switch s.Field {
	case 0:
		s.Agent = cycleIdx(s.Agent, delta, len(investigateAgents))
		s.Model = 0
	case 1:
		s.Model = cycleIdx(s.Model, delta, len(investigateModels[s.agent()]))
	case 2:
		s.Effort = cycleIdx(s.Effort, delta, len(investigateEfforts))
	}
	return s
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func buildInvestigateCommand(agent, model, effort string) string {
	if agent == "codex" {
		return fmt.Sprintf("exec codex -m %s -c model_reasoning_effort=%s %s",
			model, effort, shellQuote(investigationPrompt))
	}
	return fmt.Sprintf("exec claude --model %s --effort %s --append-system-prompt %s",
		model, effort, shellQuote(investigationPrompt))
}

func investigatePrefsPath(repo string) string {
	return filepath.Join(repo, ".watchtower", "investigate.json")
}

func loadInvestigatePrefs(repo string) investigateState {
	var s investigateState
	data, err := os.ReadFile(investigatePrefsPath(repo))
	if err != nil {
		return investigateState{}
	}
	if json.Unmarshal(data, &s) != nil {
		return investigateState{}
	}
	s.Field = 0
	return s
}

// saveInvestigatePrefs is best-effort: a repo without .watchtower (or a
// read-only disk) must not break spawning.
func saveInvestigatePrefs(repo string, s investigateState) {
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = os.WriteFile(investigatePrefsPath(repo), data, 0o644)
}
```

Render (reuses the fixed-choice control and overlay chrome from `modal.go`):

```go
func renderInvestigate(s investigateState, width int) string {
	dim := lipgloss.NewStyle().Foreground(activeTheme.Dim)
	lines := []string{
		modalChoiceField(s.Field == 0, "agent", s.agent()),
		modalChoiceField(s.Field == 1, "model", s.model()),
		modalChoiceField(s.Field == 2, "effort", s.effort()),
		"",
		keyChip("tab") + dim.Render(" next field  ") + keyChip("h/l") + dim.Render(" adjust  ") + keyChip("enter") + dim.Render(" open session"),
	}
	return renderBox("investigate", "chat before the formal flow", " esc cancel ", boundedLines(lines, max(1, width-6)))
}
```

Protocol prompt and skill installer:

```go
// investigationPrompt is injected into the spawned session (claude: system
// prompt; codex: initial prompt). Agent-neutral by design.
const investigationPrompt = `You are in a watchtower INVESTIGATION session for this repository.
The operator wants to explore an idea, bug, or question in chat before deciding
whether it becomes tracked work. Investigate collaboratively: read code, answer
questions, prototype reasoning. Do not start a formal development workflow.

When the investigation winds down (or the operator types /wrap-up or says
"wrap up"), offer exactly three exits:
1. discard - just quit; nothing is saved.
2. save draft - file the findings as a backlog issue.
3. save + launch - file the issue and start the workflow on it immediately.

For exits 2 and 3: distill the investigation into a one-line title and a body
that captures the findings, open questions, and suggested approach. Show the
title and body to the operator for approval first, then run:
  save draft:   watchtower new --title "<title>" --body "<body>" --draft
  save+launch:  watchtower new --title "<title>" --body "<body>"
Then close this pane with: herdr pane close "$HERDR_PANE_ID"
If watchtower new fails, show the error and stay open; nothing is lost.`

const wrapUpSkill = `---
name: watchtower-wrap-up
description: Use when a watchtower investigation session is wrapping up - distill findings into an issue and exit via discard, save draft, or save + launch.
---

# Watchtower Wrap-Up

1. Distill this investigation into an issue: a one-line title and a body
   capturing findings, open questions, and a suggested approach. Suggest a
   priority (0 = default).
2. Show the title and body, then ask the operator to choose:
   **save draft** / **save + launch** / **discard**.
3. Run the exit:
   - save draft: watchtower new --title "<title>" --body "<body>" --draft
   - save + launch: watchtower new --title "<title>" --body "<body>"
   - discard: skip the command.
4. On success, close the pane: herdr pane close "$HERDR_PANE_ID"
   On failure, show the error and stay open.
`

// ensureWrapUpSkill installs the /wrap-up trigger for claude sessions. It
// never overwrites: the operator may have customized the skill.
func ensureWrapUpSkill(repo string) error {
	dir := filepath.Join(repo, ".claude", "skills", "watchtower-wrap-up")
	path := filepath.Join(dir, "SKILL.md")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(wrapUpSkill), 0o644)
}
```

- [ ] **Step 4: Run, verify pass** — `go test ./internal/tui/ -run 'Investigate|ShellQuote|WrapUpSkill' -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/investigate.go internal/tui/investigate_test.go
git commit -m "feat(tui): investigate picker state, spawn command, protocol prompt"
```

---

### Task 3: TUI key routing and spawn wiring

**Files:**
- Modify: `internal/tui/app.go` (Model struct ~line 80s; key router — `if key == "n"` block ~line 784; overlay render ~line 1797)
- Test: `internal/tui/app_test.go`

**Interfaces:**
- Consumes: everything Task 2 produces; `(*herdr.Reporter).SpawnPane` from Task 1.
- Produces:
  - Model fields: `investigate *investigateState`, `spawner paneSpawner`
  - `type paneSpawner interface{ SpawnPane(cwd, command string) error }` (satisfied by `*herdr.Reporter`; tests use a spy)
  - `func (m *Model) SetPaneSpawner(s paneSpawner)` (mirrors `SetHerdrReporter`)

**Behavior:**
- `i` (grid context, same guard position as the `n` handler): `m.investigate = &s` where `s = loadInvestigatePrefs(m.Repo)`.
- While `m.investigate != nil`, keys route before other handlers (same pattern as `m.modal`): `tab` next field (wrap at 3), `h`/`l` cycle value, `esc` close, `enter` submit.
- Submit: build the command; if claude, `ensureWrapUpSkill(m.Repo)` first (failure → `m.Err`, stay open); `saveInvestigatePrefs`; `m.spawner.SpawnPane(m.Repo, cmd)`. On nil spawner or spawn error: `m.Err = "investigate: <reason> — run manually: cd <repo> && <cmd without exec >"` and keep the modal open so the operator can copy/adjust. On success close the modal.
- Render: `if m.investigate != nil { overlayBox = renderInvestigate(*m.investigate, layoutWidth) }` beside the existing modal branch.

- [ ] **Step 1: Write the failing tests** (in `app_test.go`, following its existing `Update`-driven key-press test style — find a test that sends key msgs and copy its harness)

```go
type spySpawner struct {
	cwd, cmd string
	err      error
}

func (s *spySpawner) SpawnPane(cwd, cmd string) error { s.cwd, s.cmd = cwd, cmd; return s.err }

func TestInvestigateKeyOpensPicker(t *testing.T) {
	m := NewModel(nil, []string{"spec"})
	m2 := pressKey(m, "i") // use the file's existing key-dispatch helper
	if m2.investigate == nil {
		t.Fatal("i must open the investigate picker")
	}
}

func TestInvestigateEnterSpawns(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, ".watchtower"), 0o755)
	spy := &spySpawner{}
	m := NewModel(nil, []string{"spec"})
	m.Repo = repo
	m.SetPaneSpawner(spy)
	m = pressKey(m, "i")
	m = pressKey(m, "enter")
	if spy.cwd != repo {
		t.Fatalf("spawn cwd = %q, want %q", spy.cwd, repo)
	}
	if !strings.HasPrefix(spy.cmd, "exec claude --model fable-5 --effort low") {
		t.Fatalf("cmd = %q", spy.cmd)
	}
	if m.investigate != nil {
		t.Fatal("picker must close on success")
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude", "skills", "watchtower-wrap-up", "SKILL.md")); err != nil {
		t.Fatal("wrap-up skill must be installed before a claude spawn")
	}
}

func TestInvestigateSpawnFailureShowsManualCommand(t *testing.T) {
	repo := t.TempDir()
	m := NewModel(nil, []string{"spec"})
	m.Repo = repo
	m.SetPaneSpawner(&spySpawner{err: fmt.Errorf("not running under herdr")})
	m = pressKey(m, "i")
	m = pressKey(m, "enter")
	if m.investigate == nil {
		t.Fatal("picker must stay open on failure")
	}
	if !strings.Contains(m.Err, "claude --model") || !strings.Contains(m.Err, repo) {
		t.Fatalf("Err must carry the manual command, got %q", m.Err)
	}
}

func TestInvestigateFieldCycling(t *testing.T) {
	m := NewModel(nil, []string{"spec"})
	m = pressKey(m, "i")
	m = pressKey(m, "l") // agent claude -> codex
	if m.investigate.agent() != "codex" {
		t.Fatalf("agent = %s", m.investigate.agent())
	}
	m = pressKey(m, "tab")
	m = pressKey(m, "tab")
	m = pressKey(m, "l") // effort low -> medium
	if m.investigate.effort() != "medium" {
		t.Fatalf("effort = %s", m.investigate.effort())
	}
	m = pressKey(m, "esc")
	if m.investigate != nil {
		t.Fatal("esc must close")
	}
}
```

(If `app_test.go` has no reusable key helper, write `pressKey(m Model, k string) Model` locally the same way its neighbors dispatch `tea.KeyMsg`.)

- [ ] **Step 2: Run, verify fail** — `go test ./internal/tui/ -run TestInvestigate -v` → FAIL.

- [ ] **Step 3: Implement in `app.go`**

Model additions + setter (near `SetHerdrReporter`):

```go
investigate *investigateState
spawner     paneSpawner
```

```go
// paneSpawner opens an interactive agent pane; satisfied by *herdr.Reporter.
// An interface so tests can substitute a spy.
type paneSpawner interface {
	SpawnPane(cwd, command string) error
}

func (m *Model) SetPaneSpawner(s paneSpawner) { m.spawner = s }
```

Key routing — insert a block where the other overlays route (before the `if key == "n"` handler, alongside the `m.modal != nil` handling):

```go
if m.investigate != nil {
	s := *m.investigate
	switch key {
	case "esc":
		m.investigate = nil
	case "tab":
		s.Field = (s.Field + 1) % investigateFieldCount
		m.investigate = &s
	case "h":
		s = s.cycle(-1)
		m.investigate = &s
	case "l":
		s = s.cycle(1)
		m.investigate = &s
	case "enter":
		cmd := buildInvestigateCommand(s.agent(), s.model(), s.effort())
		if s.agent() == "claude" {
			if err := ensureWrapUpSkill(m.Repo); err != nil {
				m.Err = "investigate: install wrap-up skill: " + err.Error()
				return m, nil
			}
		}
		saveInvestigatePrefs(m.Repo, s)
		manual := "cd " + m.Repo + " && " + strings.TrimPrefix(cmd, "exec ")
		if m.spawner == nil {
			m.Err = "investigate: not running under herdr — run manually: " + manual
			return m, nil
		}
		if err := m.spawner.SpawnPane(m.Repo, cmd); err != nil {
			m.Err = "investigate: " + err.Error() + " — run manually: " + manual
			return m, nil
		}
		m.investigate = nil
	}
	return m, nil
}
```

Open handler (next to `if key == "n"`):

```go
if key == "i" {
	m.Err = ""
	s := loadInvestigatePrefs(m.Repo)
	m.investigate = &s
	return m, nil
}
```

Render branch (next to the `m.modal != nil` branch at ~1797):

```go
if m.investigate != nil {
	overlayBox = renderInvestigate(*m.investigate, layoutWidth)
}
```

- [ ] **Step 4: Run, verify pass** — `go test ./internal/tui/ -v` → all PASS (watch for golden/snapshot tests; if a help/keys golden changes because of new copy, regenerate per the repo's snapshot flow in `snapshot_test.go`).

- [ ] **Step 5: Commit**

```bash
git add internal/tui/app.go internal/tui/app_test.go
git commit -m "feat(tui): i opens investigate picker and spawns an agent pane"
```

---

### Task 4: wire spawner and repo root in `cmd/watchtower`

**Files:**
- Modify: `cmd/watchtower/main.go` (the `case "tower":` block, ~lines 105–130)

**Interfaces:**
- Consumes: `model.SetPaneSpawner` (Task 3), `herdr.Reporter.SpawnPane` (Task 1).

The tower block currently sets `model.Repo = *repo`, which is empty when the repo comes from CWD-walking — investigate needs the resolved root. `resolveRepo(*repo)` is already called for the theme; hoist it.

- [ ] **Step 1: Modify the tower block**

```go
repoRoot := resolveRepo(*repo)
model := tui.NewModel(c, r.FlowStages)
reporter := herdr.NewFromEnv()
model.SetHerdrReporter(reporter)
model.SetPaneSpawner(reporter)
model.Repo = repoRoot
model.SetStageAliases(tui.ParseStageAliases(*stageAliases))
model.SetReducedMotion(*reducedMotion)
model.SetRetireAfter(*retireAfter)
if cfg, err := repocfg.Load(repoRoot); err == nil {
	tui.SetTheme(cfg.Theme)
}
```

Note: a disabled reporter (not under herdr) still satisfies `paneSpawner` and returns the "not running under herdr" error from Task 1, which Task 3 turns into the manual-command message — so wire it unconditionally.

- [ ] **Step 2: Build and run the full suite**

Run: `go build ./... && go test ./...`
Expected: PASS. Also run `gofmt -l cmd internal` → no output.

- [ ] **Step 3: Manual smoke test (under herdr)**

From a herdr pane: `watchtower tower` in a registered repo → press `i` → picker shows agent/model/effort → `enter` → new focused pane opens running claude with the investigation system prompt. Outside herdr: `enter` shows the manual command in the status line.

- [ ] **Step 4: Commit**

```bash
git add cmd/watchtower/main.go
git commit -m "feat(watchtower): wire investigate pane spawner into tower"
```

---

### Task 5: docs

**Files:**
- Create: `docs/guildhall/investigative-mode.md`
- Reference: `docs/guildhall/lane-ops-and-issue-states.md` (tone/format), spec `docs/2026-08-03-investigative-mode-design.md`

- [ ] **Step 1: Write the doc** — one page covering: what `i` does, the picker fields and where last-used values persist (`.watchtower/investigate.json`), the exact spawn commands for both agents, the three-exit protocol and `/wrap-up`, pane-close mechanics (`exec` + `herdr pane close`), the no-herdr fallback, and the fire-and-forget scope boundary. Distill from the spec; do not copy it verbatim.

- [ ] **Step 2: Commit**

```bash
git add docs/guildhall/investigative-mode.md
git commit -m "docs: investigative mode operator guide"
```
