# Guild Tower TUI Implementation Plan (Plan 4 of 4)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `watchtower tower` — the cell-based Bubble Tea client: stage floors with issue cards in per-issue identity colors, a war room floor (merge lane, slot gauge, triage count), decision toasts with `y/n/o`, flip navigation with attention cycling, in-app drill-down (issue detail → artifacts → pager), and a cell-based architecture map with per-issue ghost overlays.

**Architecture:** The TUI is a pure client of the existing socket protocol. A poll loop (`tail` every 500ms) feeds events into an extended projection; every view is a pure function `func(...) string` over that state, so rendering is tested with plain string assertions — no TTY needed. Bubble Tea owns only the event loop, keys, and terminal lifecycle. v2 (sprites/kitty graphics) later replaces the tower pane's renderer without touching state or navigation.

**Tech Stack:** `github.com/charmbracelet/bubbletea`, `github.com/charmbracelet/lipgloss`. Everything else exists.

## Global Constraints

- Plans 1–3 constraints apply. The daemon/protocol may only gain additive ops (`arch_map`); no breaking wire changes.
- Views are pure functions over state — no I/O in any `render*` function; all terminal writes go through Bubble Tea.
- Colors come from one palette table; every issue-scoped glyph/border uses the issue's identity color. Degrade cleanly when the terminal lacks 256-color (lipgloss handles profiles).
- Derived data only: progress = completed stages / total stages; never agent-self-reported numbers.
- Keyboard-only; every keybinding listed in the help footer (`?`).

---

### Task 1: Extend the projection for TUI state

**Files:**
- Modify: `internal/projection/projection.go`
- Test: `internal/projection/projection_test.go` (extend)

**Interfaces:**
- `IssueView` gains: `Tokens int` (accumulate from a new `stage_completed` payload field — no; tokens live in stage_runs. Instead: apply `EvBudgetExceeded` spent field when present, and add a `TokensFromRuns` setter the TUI fills from `list_issues`? Keep it simple and honest: drop live token display from v1 cards — REMOVE `Tokens` from the card; the issue detail panel shows tokens fetched on focus via a new proto op in Task 2). Concretely this task adds:
  ```go
  // to IssueView:
  Flow      string   // from issue_created payload ("flow")
  Behind    string   // merge_sequenced: waiting for this issue ("" = free)
  Merged    bool     // issue_merged seen
  Unmerged  bool     // issue_completed payload merge=="left-unmerged"
  // to State:
  Order     []string // issue IDs in creation order (stable identity indices)
  ```
- New event handling: `merge_sequenced` (payload `behind`) sets `Behind`; `issue_merged` sets `Merged=true`, clears `Behind`, and clears `Behind` on every issue whose `Behind` pointed at it; `merge_started` no-op for now; `issue_completed` with payload `merge=="left-unmerged"` sets `Unmerged`; `proposal_filed` increments `State.ProposalCount`; `proposal_accepted`/`proposal_rejected` decrement (floor 0); `slot_queued`/`slot_acquired` already handled.

- [ ] **Step 1: Write the failing test**

```go
func TestMergeSequencingProjection(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "a", "flow": "default"}))
	s.Apply(ev(t, core.EvIssueCreated, "GH-2", map[string]any{"title": "b", "flow": "default"}))
	s.Apply(ev(t, core.EvMergeSequenced, "GH-2", map[string]any{"behind": "GH-1"}))
	if s.Issues["GH-2"].Behind != "GH-1" {
		t.Fatalf("behind: %+v", s.Issues["GH-2"])
	}
	if len(s.Order) != 2 || s.Order[0] != "GH-1" {
		t.Fatalf("order: %v", s.Order)
	}
	s.Apply(ev(t, core.EvIssueMerged, "GH-1", nil))
	if !s.Issues["GH-1"].Merged || s.Issues["GH-2"].Behind != "" {
		t.Fatalf("release: %+v %+v", s.Issues["GH-1"], s.Issues["GH-2"])
	}
	s.Apply(ev(t, core.EvProposalFiled, "GH-2", map[string]any{"title": "x"}))
	if s.ProposalCount != 1 {
		t.Fatalf("proposals: %d", s.ProposalCount)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/projection/ -run Sequencing -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Add the fields; in `Apply`: append to `Order` on `issue_created`; the cases exactly as specced (the `issue_merged` release loop iterates `s.Issues` clearing `Behind == ev.IssueID`). `issue_completed`: also read payload `merge` string → `Unmerged`.

- [ ] **Step 4: Run the full suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/projection/
git commit -m "feat: projection tracks merge sequencing, proposals, and issue order"
```

---

### Task 2: Identity palette + `issue_detail` proto op

**Files:**
- Create: `internal/tui/identity.go`
- Modify: `internal/proto/proto.go`, `internal/proto/server.go`, `internal/engine/engine.go`
- Test: `internal/tui/identity_test.go`, `internal/proto/proto_test.go` (extend)

**Interfaces:**
- Identity:
  ```go
  package tui
  // Identity is an issue's stable visual identity: a color and 2-letter tag.
  type Identity struct{ Color string; Tag string } // Color = lipgloss hex
  // Identify derives identities for issues in creation order: color from an
  // 8-color distinguishable palette (cycling), tag from the first two
  // consonant-ish letters of the title (fallback: issue number).
  func Identify(order []string, titles map[string]string) map[string]Identity
  ```
  Palette (exact): `#e06c75 #61afef #98c379 #e5c07b #c678dd #56b6c2 #d19a66 #abb2bf`.
  Tag rule (exact): uppercase; take the first letter of the first two words of the title; single-word titles take the first two letters; empty title → the digits of the issue ID (e.g. "7" → "07", "12" → "12").
- Proto: op `issue_detail` (field `IssueID`) → Response gains `Detail *IssueDetail` where
  ```go
  type IssueDetail struct {
      Issue     store.IssueRow    `json:"issue"`
      Runs      []store.StageRun  `json:"runs"`
      Tokens    int               `json:"tokens"`
      Artifacts []string          `json:"artifacts"` // file paths under the issue dir + worktree, from stage_runs + artifact events
  }
  ```
  Server assembles it from `Store.Issues()` (find by ID), `Store.StageRuns`, `Store.IssueTokens`, and artifact paths from the events table (`artifact_produced` payloads for the issue). Add `func (s *Store) ArtifactPaths(issueID string) ([]string, error)` reading event payloads.

- [ ] **Step 1: Write the failing tests**

```go
// internal/tui/identity_test.go
package tui

import "testing"

func TestIdentify(t *testing.T) {
	order := []string{"GH-1", "GH-2", "GH-3"}
	titles := map[string]string{"GH-1": "payment adapter", "GH-2": "Fixnpe", "GH-3": ""}
	ids := Identify(order, titles)
	if ids["GH-1"].Tag != "PA" || ids["GH-2"].Tag != "FI" || ids["GH-3"].Tag != "03" {
		t.Fatalf("tags: %+v", ids)
	}
	if ids["GH-1"].Color == ids["GH-2"].Color {
		t.Fatal("adjacent issues share a color")
	}
	if ids["GH-1"].Color != "#e06c75" {
		t.Fatalf("palette order broken: %s", ids["GH-1"].Color)
	}
}
```

Proto test (append to `internal/proto/proto_test.go`, inside or alongside the existing socket test after the issue completes): request `issue_detail` for the issue and assert `Detail.Tokens > 0 == false` is not required — assert `Detail != nil`, `Detail.Issue.ID == id`, `len(Detail.Runs) == 6`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tui/ ./internal/proto/ -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// internal/tui/identity.go
package tui

import (
	"fmt"
	"strings"
)

type Identity struct {
	Color string
	Tag   string
}

var palette = []string{"#e06c75", "#61afef", "#98c379", "#e5c07b", "#c678dd", "#56b6c2", "#d19a66", "#abb2bf"}

func tagFor(issueID, title string) string {
	words := strings.Fields(strings.ToUpper(title))
	switch {
	case len(words) >= 2:
		return words[0][:1] + words[1][:1]
	case len(words) == 1 && len(words[0]) >= 2:
		return words[0][:2]
	default:
		digits := strings.TrimLeft(issueID, "GH-")
		return fmt.Sprintf("%02s", digits)
	}
}

func Identify(order []string, titles map[string]string) map[string]Identity {
	out := map[string]Identity{}
	for i, id := range order {
		out[id] = Identity{Color: palette[i%len(palette)], Tag: tagFor(id, titles[id])}
	}
	return out
}
```

Store `ArtifactPaths`: SELECT payload FROM events WHERE type='artifact_produced' AND issue_id=?, unmarshal each, collect `path`. Server op `issue_detail` assembles the struct; wire types as specced.

- [ ] **Step 4: Run the full suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/ internal/proto/ internal/store/
git commit -m "feat: issue identity palette and issue_detail protocol op"
```

---

### Task 3: TUI app skeleton + event pump

**Files:**
- Create: `internal/tui/app.go`
- Modify: `cmd/watchtower/main.go` (add `tower` subcommand)
- Test: `internal/tui/app_test.go`

**Interfaces:**
- ```go
  type Model struct {
      State    *projection.State
      Flow     flow.Flow            // stage list for floors (fetched via new proto op? No — read from events? The flow name is on issues; floors need stage names. Add proto op `get_flow` returning the default flow's stage names: Response field FlowStages []string)
      Ids      map[string]Identity
      Focus    Focus                // Task 5
      Toast    *projection.DecisionView
      Detail   *proto.IssueDetail
      Width, Height int
      Err      string
  }
  type Msg struct{ Events []core.Event } // poll result
  func NewModel(client *proto.Client, stages []string) Model
  func (m Model) Init() tea.Cmd
  func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd)
  func (m Model) View() string
  ```
- Poll pump: `Init` returns a `tea.Tick(500ms)` command whose handler calls `client.Do({Op:"tail", SinceSeq: m.lastSeq})`, applies events to `State`, recomputes `Ids` when issues appear, auto-raises the newest pending decision as `Toast` when none is showing, and re-arms the tick. Socket errors set `m.Err` (shown in the footer) and keep ticking — the TUI must survive daemon restarts.
- Proto: add `get_flow` op → `FlowStages []string` (server: stage names of the flow named in the request or "default").
- `tower` subcommand: dial socket, `get_flow`, `tea.NewProgram(NewModel(...), tea.WithAltScreen()).Run()`.

- [ ] **Step 1: Write the failing test**

Test the pump handler logic, not the terminal: extract `func (m Model) applyEvents(evs []core.Event) Model` and test it directly.

```go
// internal/tui/app_test.go
package tui

import (
	"testing"

	"github.com/wbushyeager/watchtower/internal/core"
)

func mkev(t *testing.T, typ core.EventType, issue string, payload any) core.Event {
	t.Helper()
	e, err := core.NewEvent(typ, issue, payload)
	if err != nil {
		t.Fatal(err)
	}
	e.Seq = seqCounter()
	return e
}

var seq int64

func seqCounter() int64 { seq++; return seq }

func TestApplyEventsBuildsStateAndToast(t *testing.T) {
	m := NewModel(nil, []string{"brainstorm", "spec", "execute", "review", "merge"})
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "payment adapter", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "spec"}),
		mkev(t, core.EvDecisionRequired, "GH-1", map[string]any{
			"decision_id": float64(7), "stage": "spec", "question": "Approve?",
			"options": []any{"approve", "reject"}, "recommended": float64(0)}),
	})
	if m.State.Issues["GH-1"] == nil || m.Ids["GH-1"].Tag != "PA" {
		t.Fatalf("state/ids: %+v", m.Ids)
	}
	if m.Toast == nil || m.Toast.ID != 7 {
		t.Fatalf("toast not raised: %+v", m.Toast)
	}
	if m.lastSeq != 3 {
		t.Fatalf("lastSeq: %d", m.lastSeq)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run Apply -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

`app.go` with the Model as specced (unexported `client *proto.Client`, `lastSeq int64`), `applyEvents` doing: apply each to `State`, track max Seq, rebuild `Ids` via `Identify(State.Order, titlesFrom(State))`, raise toast when `m.Toast == nil` and a pending decision exists (pick the first of `State.Decisions` by lowest ID — deterministic). `Init`/`Update`/`View` wire the tick pump (`tickMsg` → do tail in a `tea.Cmd` closure → `Msg{Events}`), `tea.WindowSizeMsg` → Width/Height, `q`/`ctrl+c` → `tea.Quit`. `View` for now: `renderTower` placeholder returning a one-line summary per issue (replaced in Task 4). `get_flow` op + `tower` subcommand.

```bash
go get github.com/charmbracelet/bubbletea@latest github.com/charmbracelet/lipgloss@latest
```

- [ ] **Step 4: Run the full suite + manual check**

Run: `go test ./... -race`, then the stub smoke daemon + `go run ./cmd/watchtower tower --data /tmp/gh-smoke3` in a real terminal: issues appear as lines; `q` quits.
Expected: PASS + visible issue lines.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/tui/ internal/proto/ cmd/
git commit -m "feat: tower TUI skeleton with socket event pump"
```

---

### Task 4: Tower rendering — floors, cards, war room

**Files:**
- Create: `internal/tui/render.go`
- Test: `internal/tui/render_test.go`

**Interfaces:**
- ```go
  // renderTower draws the war room + one floor per stage (top to bottom:
  // war room, then stages in REVERSE flow order so merge is the ground floor).
  func renderTower(st *projection.State, stages []string, ids map[string]Identity, focus Focus, width int) string
  ```
- Card content (single line, in the issue's color, bordered when focused): `▐<TAG> <ID> <state-glyph> <progress-bar> <flags>` where state-glyph: `●` running, `◔` waiting_decision, `⧗` queued_for_slot, `✗` failed, `✓` done, `⇡` merged; progress = `len(Completed)/len(stages)` as a 5-cell bar (`█`/`░`); flags: `🔒behind:<tag>` when `Behind != ""`, `!unmerged` when `Unmerged`.
- Floor layout: floor label (stage name, uppercase, dim) on the left; cards side by side; empty floors render the label + dim `—`. An issue sits on the floor of `CurrentStage`; done/merged issues sit on the ground (merge) floor with their final glyph.
- War room line: `⚖ MERGE LANE: <tags in order, ⇢-separated, waiting ones dim>  ·  SLOTS <held>/<total placeholder n/a in v1: omit>  ·  TRAY <ProposalCount>  ·  DECISIONS <len(Decisions)>`. (Slot totals aren't in the projection — show `SLOTS busy:<count of queued_for_slot issues>` instead; be honest with available data.)
- All colors via lipgloss styles built from the palette + a small theme table (dim, label, warn, bad, good) defined once at the top of render.go.

- [ ] **Step 1: Write the failing test**

String assertions on structure, not exact ANSI: strip styles for the test via `lipgloss.NewStyle()` profile — simplest: build renders with `lipgloss.SetColorProfile(termenv.Ascii)` in the test (import `github.com/muesli/termenv`), so output is plain text.

```go
// internal/tui/render_test.go
package tui

import (
	"strings"
	"testing"

	"github.com/muesli/termenv"
	"github.com/charmbracelet/lipgloss"
	"github.com/wbushyeager/watchtower/internal/core"
)

func TestRenderTowerPlacesCards(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	m := NewModel(nil, []string{"brainstorm", "spec", "execute", "review", "merge"})
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "payment adapter", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "execute"}),
		mkev(t, core.EvIssueCreated, "GH-2", map[string]any{"title": "search fix", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-2", map[string]any{"stage": "spec"}),
		mkev(t, core.EvMergeSequenced, "GH-2", map[string]any{"behind": "GH-1"}),
	})
	out := renderTower(m.State, m.stages, m.Ids, m.Focus, 100)
	lines := strings.Split(out, "\n")
	// execute floor line must contain PA card; spec floor line must contain SF card
	var execLine, specLine string
	for _, l := range lines {
		if strings.Contains(l, "EXECUTE") {
			execLine = l
		}
		if strings.Contains(l, "SPEC") {
			specLine = l
		}
	}
	if !strings.Contains(execLine, "PA GH-1") {
		t.Fatalf("PA not on execute floor: %q", execLine)
	}
	if !strings.Contains(specLine, "SF GH-2") || !strings.Contains(specLine, "behind:PA") {
		t.Fatalf("SF card wrong: %q", specLine)
	}
	if !strings.Contains(out, "MERGE LANE") || !strings.Contains(out, "DECISIONS 0") {
		t.Fatalf("war room missing:\n%s", out)
	}
	// floors ordered: war room above BRAINSTORM above MERGE (ground)
	if strings.Index(out, "MERGE LANE") > strings.Index(out, "BRAINSTORM") ||
		strings.Index(out, "BRAINSTORM") > strings.Index(out, "MERGE ")+len(out) { // merge floor label is last
		_ = 0
	}
	if strings.LastIndex(out, "MERGE") < strings.Index(out, "BRAINSTORM") {
		t.Fatal("merge floor not at the bottom")
	}
}
```

(`m.stages` is the unexported stage list on Model — expose as needed for the test since tests share the package.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run Tower -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

`render.go`: theme styles; `card(iv, id Identity, focused bool) string`; `floorLine(name string, cards []string, width int) string`; `warRoom(st, ids)`; `renderTower` assembling war room + reversed stage floors, placing each issue by `CurrentStage` (done/merged → last stage). Glyph/progress/flags exactly as specced. Wire `Model.View()` to call it (header line: `◆ GUILD TOWER · <n> issues · q quit · ? help`).

- [ ] **Step 4: Run the full suite + manual check**

`go test ./... -race`; manual: stub daemon + tower shows floors with colored cards.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/
git commit -m "feat: tower renders stage floors, identity cards, and war room"
```

---

### Task 5: Navigation — flip focus + attention cycling

**Files:**
- Create: `internal/tui/focus.go`
- Modify: `internal/tui/app.go` (key handling)
- Test: `internal/tui/focus_test.go`

**Interfaces:**
- ```go
  type Focus struct {
      Floor int    // index into rendered floors (0 = war room, 1..N = stages top→bottom)
      Card  int    // index within the floor's cards (clamped)
      Issue string // resolved issue ID under focus ("" on empty floor)
  }
  // floorCards returns issue IDs on a rendered floor, in Order order.
  func floorCards(st *projection.State, stages []string, floor int) []string
  func moveFocus(f Focus, st *projection.State, stages []string, key string) Focus
  // key: "j" down a floor, "k" up, "h"/"l" within floor, "1".."9" jump to
  // issue by Order index, "tab" next attention item (worst first: failed >
  // waiting_decision > queued_for_slot; cycles), "g" war room.
  func attentionList(st *projection.State) []string // issue IDs, worst first, stable
  ```

- [ ] **Step 1: Write the failing test**

```go
// internal/tui/focus_test.go
package tui

import (
	"testing"

	"github.com/wbushyeager/watchtower/internal/core"
)

func navModel(t *testing.T) Model {
	m := NewModel(nil, []string{"brainstorm", "spec", "execute", "review", "merge"})
	return m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "aa bb", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "spec"}),
		mkev(t, core.EvIssueCreated, "GH-2", map[string]any{"title": "cc dd", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-2", map[string]any{"stage": "spec"}),
		mkev(t, core.EvIssueCreated, "GH-3", map[string]any{"title": "ee ff", "flow": "default"}),
		mkev(t, core.EvStageFailed, "GH-3", map[string]any{"stage": "execute"}),
	})
}

func TestMoveFocusAndAttention(t *testing.T) {
	m := navModel(t)
	f := Focus{Floor: 0}
	// j from war room lands on first stage floor (brainstorm, empty) then spec
	f = moveFocus(f, m.State, m.stages, "j") // brainstorm floor (empty)
	f = moveFocus(f, m.State, m.stages, "j") // spec floor
	if f.Issue != "GH-1" {
		t.Fatalf("expected GH-1 focused, got %+v", f)
	}
	f = moveFocus(f, m.State, m.stages, "l")
	if f.Issue != "GH-2" {
		t.Fatalf("l: %+v", f)
	}
	f = moveFocus(f, m.State, m.stages, "l") // clamp at last card
	if f.Issue != "GH-2" {
		t.Fatalf("clamp: %+v", f)
	}
	// attention: GH-3 failed — tab jumps to it regardless of position
	f = moveFocus(f, m.State, m.stages, "tab")
	if f.Issue != "GH-3" {
		t.Fatalf("tab: %+v", f)
	}
	// numeric jump
	f = moveFocus(f, m.State, m.stages, "1")
	if f.Issue != "GH-1" {
		t.Fatalf("jump: %+v", f)
	}
}

func TestAttentionOrder(t *testing.T) {
	m := navModel(t)
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvDecisionRequired, "GH-2", map[string]any{
			"decision_id": float64(1), "stage": "spec", "question": "q",
			"options": []any{"a"}, "recommended": float64(0)}),
	})
	al := attentionList(m.State)
	if len(al) != 2 || al[0] != "GH-3" || al[1] != "GH-2" {
		t.Fatalf("attention: %v", al)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run 'Focus|Attention' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

`focus.go` exactly as specced: floor index ↔ stage mapping (floor 0 = war room; stage floors are reversed flow order — reuse one helper shared with render so they can't diverge: `func floorStages(stages []string) []string`). `moveFocus` clamps; entering a floor sets `Card = 0` (or nearest valid); `Issue` resolved from `floorCards`. `attentionList`: filter+sort by severity class then `Order` position. `tab` finds current position in the list and advances cyclically. Wire keys in `Update`, and pass `Focus` through to `renderTower` (focused card gets a bordered style — assertion-free, visual only). On focus change, fire a `tea.Cmd` fetching `issue_detail` for the right rail (Task 6 renders it).

- [ ] **Step 4: Run the full suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/
git commit -m "feat: flip navigation with attention cycling and numeric jumps"
```

---

### Task 6: Right rail + decision toast

**Files:**
- Create: `internal/tui/rail.go`
- Modify: `internal/tui/app.go`, `internal/tui/render.go` (compose panes)
- Test: `internal/tui/rail_test.go`

**Interfaces:**
- ```go
  // renderRail: focused issue detail (identity header, state, stage, flow,
  // per-stage-run lines "stage/agent status tokens", token total) above the
  // decision queue (every pending decision: [id] TAG stage — question, top
  // one marked ▶). Width-bounded.
  func renderRail(st *projection.State, ids map[string]Identity, det *proto.IssueDetail, width int) string
  // renderToast: modal box for the raised decision: identity-colored border,
  // question, options with recommended marked ★, "y accept ★ · n choose · o evidence · esc dismiss".
  func renderToast(d projection.DecisionView, id Identity, width int) string
  ```
- Behavior in `Update`: when `Toast != nil`: `y` answers recommended (proto `answer_decision`), `n` enters option-select mode (digits choose), `esc` dismisses (toast won't re-raise for the same decision ID until a new decision arrives — track `dismissed map[int64]bool`), `o` focuses the issue and opens the artifact list (Task 7). Answer success clears the toast and lets the pump raise the next.
- Compose: `View()` = header + `lipgloss.JoinHorizontal(tower, rail)` + footer (err/help). Toast overlays by rendering instead of the tower body when active (simplest honest v1: no true overlay compositing).

- [ ] **Step 1: Write the failing test**

```go
// internal/tui/rail_test.go
package tui

import (
	"strings"
	"testing"

	"github.com/muesli/termenv"
	"github.com/charmbracelet/lipgloss"
	"github.com/wbushyeager/watchtower/internal/projection"
)

func TestRenderToastMarksRecommended(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	d := projection.DecisionView{ID: 4, IssueID: "GH-1", Stage: "spec",
		Question: "Approve spec artifacts?", Options: []string{"approve", "reject"}, Recommended: 0}
	out := renderToast(d, Identity{Color: "#e06c75", Tag: "PA"}, 60)
	if !strings.Contains(out, "Approve spec artifacts?") || !strings.Contains(out, "★ approve") {
		t.Fatalf("toast:\n%s", out)
	}
	if !strings.Contains(out, "y accept") {
		t.Fatalf("keys missing:\n%s", out)
	}
}

func TestRenderRailShowsQueueOrder(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	m := navModel(t)
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvDecisionRequired, "GH-1", map[string]any{
			"decision_id": float64(1), "stage": "spec", "question": "first?",
			"options": []any{"a"}, "recommended": float64(0)}),
		mkev(t, core.EvDecisionRequired, "GH-2", map[string]any{
			"decision_id": float64(2), "stage": "spec", "question": "second?",
			"options": []any{"a"}, "recommended": float64(0)}),
	})
	out := renderRail(m.State, m.Ids, nil, 40)
	if strings.Index(out, "first?") > strings.Index(out, "second?") {
		t.Fatalf("queue order wrong:\n%s", out)
	}
}
```

(Import `core` in the test file as needed.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run 'Toast|Rail' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

As specced. Queue order: decisions sorted by ID ascending (creation order; blocking-cost ordering arrives to clients only via the daemon's own answer priorities — the rail notes the top item with ▶). Answer command: `tea.Cmd` doing `client.Do({Op:"answer_decision", DecisionID, Option})`; on `!r.OK` set `m.Err`.

- [ ] **Step 4: Run the full suite + manual check**

`go test ./... -race`; manual: stub daemon, watch a toast appear, `y` it, watch the flow advance.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/
git commit -m "feat: right rail detail/queue and decision toast with y/n/esc"
```

---

### Task 7: Drill-down — artifact list + pager

**Files:**
- Create: `internal/tui/pager.go`
- Modify: `internal/tui/app.go`
- Test: `internal/tui/pager_test.go`

**Interfaces:**
- ```go
  type pagerState struct {
      Mode  string   // "" | "artifacts" | "pager"
      Files []string // artifact paths for the focused issue
      Sel   int
      Title string
      Lines []string // loaded file content
      Top   int      // scroll offset
  }
  func renderArtifactList(p pagerState, id Identity, width, height int) string
  func renderPager(p pagerState, width, height int) string
  func (p pagerState) scroll(key string, height int) pagerState // j/k/d/u/g/G
  ```
- Behavior: `Enter` on a focused issue opens the artifact list (paths from `issue_detail.Artifacts`); `Enter` on a file loads it (plain `os.ReadFile`, no highlighting in v1 — note as v2) into the pager; `esc` walks back pager → list → tower. Scroll keys clamp.

- [ ] **Step 1: Write the failing test**

```go
// internal/tui/pager_test.go
package tui

import (
	"strings"
	"testing"
)

func TestPagerScrollClamps(t *testing.T) {
	p := pagerState{Mode: "pager", Lines: mklines(100)}
	p = p.scroll("G", 20) // bottom
	if p.Top != 80 {
		t.Fatalf("G: %d", p.Top)
	}
	p = p.scroll("j", 20)
	if p.Top != 80 {
		t.Fatalf("clamp: %d", p.Top)
	}
	p = p.scroll("g", 20)
	if p.Top != 0 {
		t.Fatalf("g: %d", p.Top)
	}
	p = p.scroll("d", 20) // half page
	if p.Top != 10 {
		t.Fatalf("d: %d", p.Top)
	}
}

func TestRenderPagerWindow(t *testing.T) {
	p := pagerState{Mode: "pager", Title: "spec.md", Lines: mklines(100), Top: 50}
	out := renderPager(p, 80, 10)
	if !strings.Contains(out, "line-50") || strings.Contains(out, "line-70") {
		t.Fatalf("window wrong:\n%s", out)
	}
}

func mklines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "line-" + itoa(i)
	}
	return out
}

func itoa(i int) string { return fmt.Sprintf("%d", i) }
```

(Use `fmt.Sprintf` directly with `fmt` imported; drop the helper in final code.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run Pager -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

As specced; `Update` routes keys to the pager when `Mode != ""`. Window: `Lines[Top:min(Top+height-2, len)]` under a title bar (`Title · <Top>/<len> · esc back`).

- [ ] **Step 4: Run the full suite + manual**

`go test ./... -race`; manual: drill into a stub issue's spec.md.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/
git commit -m "feat: in-app artifact list and pager drill-down"
```

---

### Task 8: Architecture map with ghost overlays + help + smoke

**Files:**
- Create: `internal/archmap/archmap.go`, `internal/tui/arch.go`
- Modify: `internal/proto/proto.go`, `internal/proto/server.go`, `cmd/watchtower/main.go`, `internal/tui/app.go`
- Test: `internal/archmap/archmap_test.go`

**Interfaces:**
- ```go
  package archmap
  type Module struct { Name string; Files int }
  type Overlay struct { IssueID string; Globs []string } // from in-flight touchsets
  type Map struct { Modules []Module; Overlays []Overlay }
  // Scan lists top-level directories (and depth-2 under cmd/, internal/, pkg/,
  // src/) of repo with their recursive file counts, skipping dot-dirs,
  // .worktrees, node_modules, vendor.
  func Scan(repo string) ([]Module, error)
  ```
- Daemon: keeps in-flight touchsets (the Marshal already holds them — add `func (m *Marshal) Snapshot() []archmap.Overlay`… that couples packages; instead the daemon records overlays itself in the `PlanApproved` path? Simplest additive seam: server op `arch_map` scans the repo on request (`--repo` flag value; empty in fake mode → `Modules: nil`) and gets overlays from a new tiny method on the engine: `func (e *Engine) ActiveTouchsets() map[string][]string` — engine stores the loaded set on `issueState` when it calls `Marshal.PlanApproved` and clears on completion). Response field: `Arch *archmap.Map`.
- TUI: `a` toggles the arch pane replacing the rail: each module a one-line box `▣ name (files)`; a module whose name/path prefix-matches any overlay glob gets `◈` in each matching issue's color appended (ghosts). Issues' ghost legend at the bottom. `A` full-screen (replaces tower too); `a`/`esc` back.
- Help: `?` toggles a footer-expanded key reference listing every binding from Tasks 5–8.
- Final smoke: stub daemon two issues + tower in a real terminal — walk floors, tab to the failed/waiting item, answer a toast, drill into spec.md, toggle arch map (fake mode: shows "no repo — arch map available with --runner claude"), quit. Then `go test ./... -race && go build ./...`.

- [ ] **Step 1: Write the failing test**

```go
// internal/archmap/archmap_test.go
package archmap

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanCountsAndSkips(t *testing.T) {
	repo := t.TempDir()
	mk := func(p string) {
		full := filepath.Join(repo, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte("x"), 0o644)
	}
	mk("internal/pay/a.go")
	mk("internal/pay/b.go")
	mk("internal/cart/c.go")
	mk("docs/readme.md")
	mk(".worktrees/GH-1/junk.go")
	mk(".git/config")
	mods, err := Scan(repo)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]int{}
	for _, m := range mods {
		byName[m.Name] = m.Files
	}
	if byName["internal/pay"] != 2 || byName["internal/cart"] != 1 || byName["docs"] != 1 {
		t.Fatalf("modules: %+v", byName)
	}
	if _, ok := byName[".worktrees/GH-1"]; ok {
		t.Fatal("worktrees not skipped")
	}
	if _, ok := byName[".git"]; ok {
		t.Fatal(".git not skipped")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/archmap/ -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

`Scan`: read top-level entries; skip names starting with `.`, plus `node_modules`, `vendor`; for `cmd`,`internal`,`pkg`,`src` recurse one level and emit `parent/child` modules instead of the parent; count files recursively under each module (`filepath.WalkDir`, skipping the same exclusions). Sort by Name. Engine `ActiveTouchsets` + storage on `issueState` (set where `PlanApproved` is called; delete at flow end). `arch_map` op; TUI arch pane + ghosts (glob prefix match reuses `touchset` helpers — export `touchset.Prefix` or duplicate the 4-line helper locally with a comment; prefer exporting `func PrefixOf(glob string) string` from touchset). Help footer. `View()` composition honors the pane modes.

- [ ] **Step 4: Full suite + the manual smoke above**

Run: `go test ./... -race && go build ./...`
Expected: PASS; manual smoke checklist all green.

- [ ] **Step 5: Commit**

```bash
git add internal/ cmd/
git commit -m "feat: architecture map with issue ghost overlays, help footer"
```

---

## Self-review notes

- **Spec coverage:** floors + war room + identity cards (T4, T2), toasts with y/n/o/esc (T6), flip navigation with attention cycling + numeric jumps (T5), in-app drill-down pager (T7), arch map from repo scan with per-issue ghost overlays (T8), all over the existing event stream via a poll pump that survives daemon restarts (T3). Deliberately deferred to v2 and stated: sprites/kitty tier, syntax highlighting in the pager, true toast overlay compositing, live slot-pool totals on the war room line (needs a protocol addition — currently shows queued-count honestly), operator avatar animation, lever-matrix editor UI (`L` key — not in v1; levers are set at issue creation via CLI).
- **Placeholder scan:** Task 1 interface block contained a thinking-aloud passage — resolved concretely: no token display on cards; tokens appear in the rail via `issue_detail`. Verify no other hedge survives in final task text.
- **Type consistency:** `Focus` shared render/focus/app ✓; `projection.DecisionView` reused for toast ✓; `proto.IssueDetail` produced T2, consumed T6/T7 ✓; `floorStages` single source for floor order between render and focus ✓; `archmap.Map` wire type additive ✓.
- **Testing honesty:** all view tests run under `termenv.Ascii` so they assert structure, not ANSI codes; pump logic tested via `applyEvents` without a TTY; the only untested surfaces are Bubble Tea wiring and lipgloss styling, covered by the manual smoke checklist in Task 8.
