# Control Room TUI Implementation Plan (Plan 4c)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rebuild the tower TUI as the control room the UX review demanded: collision-free identity palette, titled lanes with words-in-cells, plain-English status header, responsible decision toasts with a real evidence panel, four doors (decisions/tray/timeline/transcript), control verbs, new-issue modal, shipped shelf, and defined overflow — all consuming the Plan 4b protocol.

**Architecture:** Pure renderer + key-handling work inside `internal/tui`, plus small additive server-side conveniences (Budget on IssueDetail). All views stay pure functions tested as plain strings under `termenv.Ascii`. The Bubble Tea model gains a mode stack (tower → door → pager) so `esc` always walks back one level.

**Tech Stack:** unchanged (bubbletea, lipgloss, existing packages).

## Global Constraints

- Plans 1–4b constraints apply; protocol changes additive only.
- **One channel per meaning:** identity palette is exactly `#61afef #c678dd #56b6c2 #e78ac8 #7d9bf0` (5 hues, cycling); red `#e06c75` gold `#f2c14e` green `#98c379` are status-only and may never color an identity element.
- **Words in cells** for rare states (`NEED-YOU`, `FAILED`, `queued`, `merging`, `paused`, `after <chip>`); glyphs only for: spinner (working), `✓` (cleared), `⇡` (shipped), `·` (untouched), focus `▸◂`.
- **Motion budget:** healthy spinners render dim and advance every 2nd tick; `FAILED` is the only blinking element (inverse-video toggle on tick); a `reduced_motion` config disables both.
- Grid geometry never changes from transient content: the notice row is always reserved (blank when empty).
- User-facing copy bans internal jargon: no "Marshal", "touchset", "worktree" in grid/toast/notice text (FOCUS panel may show paths — that is diagnostic territory).
- Every key must appear in the `?` help screen; footer shows the 6 most contextual keys.

---

### Task 1: Palette purge + tag dedupe + status styles

**Files:**
- Modify: `internal/tui/identity.go`, `internal/tui/render.go` (theme table)
- Test: `internal/tui/identity_test.go` (extend)

**Interfaces:**
- `palette` becomes the 5 safe hues above. New exported-in-package helpers: `styleStatusBad/Warn/Ok` in one theme table; identity styles built only from `palette`.
- `Identify` gains dedupe: when a computed tag collides with an earlier issue's tag, fall back to the issue's digits (`"07"` style). Signature unchanged.

- [ ] **Step 1: Write the failing tests**

```go
func TestPaletteExcludesStatusHues(t *testing.T) {
	banned := map[string]bool{"#e06c75": true, "#f2c14e": true, "#98c379": true, "#e5c07b": true}
	for _, c := range palette {
		if banned[c] {
			t.Fatalf("status hue %s in identity palette", c)
		}
	}
	if len(palette) != 5 {
		t.Fatalf("palette size %d", len(palette))
	}
}

func TestIdentifyDedupesTags(t *testing.T) {
	order := []string{"GH-1", "GH-2"}
	titles := map[string]string{"GH-1": "fix auth", "GH-2": "fix api"}
	ids := Identify(order, titles)
	if ids["GH-1"].Tag == ids["GH-2"].Tag {
		t.Fatalf("tags collide: %+v", ids)
	}
	if ids["GH-2"].Tag != "02" {
		t.Fatalf("fallback wrong: %q", ids["GH-2"].Tag)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tui/ -run 'Palette|Dedupe' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Palette swap; dedupe via a `seen map[string]bool` inside `Identify` (first claimant keeps the tag). Update any test that asserted the old first color (`#e06c75`).

- [ ] **Step 4: Run the full suite** — `go test ./... -race` — Expected: PASS (fix palette-dependent assertions).

- [ ] **Step 5: Commit**

```bash
git add internal/tui/
git commit -m "feat: collision-free identity palette and tag dedupe"
```

---

### Task 2: Projection v2 — everything the new screens read

**Files:**
- Modify: `internal/projection/projection.go`, `internal/proto/proto.go`, `internal/proto/server.go`
- Test: `internal/projection/projection_test.go` (extend)

**Interfaces:**
- `DecisionView` gains `Why string`, `Consequences []string`, `Reversible string` (from the enriched `decision_required` payload — engine already emits question/options/recommended; extend the engine's emit in `escalate` to include the three new fields; that one-line engine change is in scope here).
- `IssueView` gains: `Paused bool` (issue_paused/resumed), `Killed bool` (stage_killed → also State "paused"), `AreaWeights map[string]int` (accumulated from `artifact_produced` payloads carrying `area_weight`), `MergedAt`, plus existing Attempt/AttemptOf/LastError from 4b.
- `State` gains `Notices []Notice` (`type Notice struct{ Text string; Seq int64 }`, appended on `proposal_filed` — text `"✉ new idea from <issueID>: <title>"` — capped at last 5) and `ShippedToday []string`, `Parked []string` (issue IDs; parked = final stage_failed or killed).
- Server: `IssueDetail` gains `Budget int` (from a new `Server.SetBudget(int)` called by the daemon with the `--budget` flag value; 0 = unlimited).

- [ ] **Step 1: Write the failing test**

```go
func TestProjectionV2Fields(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "x", "flow": "default"}))
	s.Apply(ev(t, core.EvIssuePaused, "GH-1", nil))
	if !s.Issues["GH-1"].Paused {
		t.Fatal("paused not tracked")
	}
	s.Apply(ev(t, core.EvIssueResumed, "GH-1", nil))
	s.Apply(ev(t, core.EvArtifactProduced, "GH-1", map[string]any{
		"stage": "execute", "artifact": "evidence.json", "path": "/x",
		"area_weight": map[string]any{"payments": float64(300), "api": float64(40)}}))
	if s.Issues["GH-1"].AreaWeights["payments"] != 300 {
		t.Fatalf("weights: %v", s.Issues["GH-1"].AreaWeights)
	}
	s.Apply(ev(t, core.EvDecisionRequired, "GH-1", map[string]any{
		"decision_id": float64(1), "stage": "spec", "question": "Q?",
		"options": []any{"a", "b"}, "recommended": float64(0),
		"why": "a is safe", "consequences": []any{"c1", "c2"}, "reversible": "anytime"}))
	d := s.Decisions[1]
	if d.Why != "a is safe" || len(d.Consequences) != 2 {
		t.Fatalf("decision v2: %+v", d)
	}
	s.Apply(ev(t, core.EvProposalFiled, "GH-1", map[string]any{"title": "new idea"}))
	if len(s.Notices) != 1 {
		t.Fatalf("notices: %v", s.Notices)
	}
}
```

- [ ] **Step 2: Run test to verify it fails** — `go test ./internal/projection/ -run V2 -v` — FAIL.

- [ ] **Step 3: Implement**

Projection cases as specced (area weights ACCUMULATE with max — a later bundle replaces earlier per area: use assignment, not addition, since each bundle is a full recount). Engine `escalate` payload gains `why/consequences/reversible`. Server budget plumbing.

- [ ] **Step 4: Run the full suite** — PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/
git commit -m "feat: projection v2 with pause, area weights, decision rationale, notices"
```

---

### Task 3: Header, war room, reserved notice row

**Files:**
- Modify: `internal/tui/render.go`, `internal/tui/app.go`
- Test: `internal/tui/render_test.go` (extend)

**Interfaces:**
- ```go
  // renderHeader: the plain-English status sentence from Overview data.
  // ● red "N builds failing" > gold "N questions for you" > green "all clear".
  func renderHeader(ov *proto.Overview, width int) string
  // renderNoticeRow: newest notice or blank — ALWAYS one row.
  func renderNoticeRow(st *projection.State, width int) string
  ```
- Model polls `overview` alongside `tail` (same tick, second command in the batch) into `m.Overview`.
- War room line simplifies to outcomes wording: `shipping order: <chip>→<chip> · builders 3/4 busy · ideas 1 · questions 1` (chips = colored titles' short tags with title tooltip in FOCUS; keep chips ≤3 then `+n`).

- [ ] **Step 1: Write the failing test**

```go
func TestRenderHeaderSeverityOrder(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	h := renderHeader(&proto.Overview{Failing: 1, NeedYou: 2, Building: 3}, 100)
	if !strings.Contains(h, "1 build failing") || !strings.Contains(h, "2 questions for you") {
		t.Fatalf("header: %q", h)
	}
	h = renderHeader(&proto.Overview{Building: 2, ShippedToday: 1, TokensTotal: 41000, DollarsTotal: 0.35}, 100)
	if !strings.Contains(h, "all clear") || !strings.Contains(h, "~$0.35") {
		t.Fatalf("calm header: %q", h)
	}
	h = renderHeader(&proto.Overview{Building: 2}, 100)
	if strings.Contains(h, "$") {
		t.Fatalf("dollars shown when price unset: %q", h)
	}
}

func TestNoticeRowAlwaysReserved(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	st := projection.NewState()
	empty := renderNoticeRow(st, 80)
	if lipgloss.Height(empty) != 1 {
		t.Fatalf("empty notice row height %d", lipgloss.Height(empty))
	}
}
```

- [ ] **Step 2: Run to verify FAIL.** — `go test ./internal/tui/ -run 'Header|Notice' -v`

- [ ] **Step 3: Implement** as specced; sentence templates: `● %d build(s) failing, %d question(s) for you — %d building, %d shipped today` with graceful zero-elision; dollars appended only when `DollarsTotal > 0`.

- [ ] **Step 4: Full suite** — PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/
git commit -m "feat: status sentence header, outcome-worded war room, reserved notice row"
```

---

### Task 4: Grid v2 — titled lanes, words-in-cells, motion inversion, legend

**Files:**
- Modify: `internal/tui/render.go`, `internal/tui/app.go` (tick), `cmd/guildhall/main.go` (tower flags: `--stage-aliases`, `--reduced-motion`)
- Test: `internal/tui/render_test.go` (extend)

**Interfaces:**
- Lane width 14; header = chip row + two title lines (word-wrapped short title from the full title's first ~24 chars) + id line.
- Cell contract (14 chars):
  - working: dim spinner + optional `%` when evidence exists (`◌ 82%` — % = completed stages? NO: percent only where real progress exists; omit otherwise — just dim `◌`)
  - waiting decision: `NEED-YOU ◔` gold
  - failing: `FAILED ✗` red, inverse-video every other tick
  - queued: `queued ⧗` faint
  - merge row sequencing: `after ▐█▌` + blocker's chip in blocker's color
  - merging: `merging ▼`
  - paused: `paused ⏸` faint
  - cleared floor: dim `✓` in lane color; untouched `·`; merged cap `⇡` on merge row
- Multi-agent stages: spinner count = live agents (`◌◌◌`), dim.
- Legend: one faint line under the FOCUS panel: `◌ working · ✓ done · ✗ FAILED · ◔ your turn · ▼ merging · ⇡ shipped · ? help`.
- Stage aliases: `--stage-aliases "spec=AGREE,execute=BUILD,merge=SHIP"` remaps floor labels (display only).
- Tick: model tick counter; spinners advance on even ticks only; FAILED inverse on odd ticks; both disabled by `--reduced-motion`.

- [ ] **Step 1: Write the failing test**

```go
func TestCellWordsAndStates(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	m := NewModel(nil, []string{"brainstorm", "spec", "execute", "review", "merge"})
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "payment adapter", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "execute", "attempt": float64(1), "of": float64(2)}),
		mkev(t, core.EvIssueCreated, "GH-2", map[string]any{"title": "search fix", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-2", map[string]any{"stage": "spec", "attempt": float64(1), "of": float64(1)}),
		mkev(t, core.EvDecisionRequired, "GH-2", map[string]any{
			"decision_id": float64(1), "stage": "spec", "question": "q",
			"options": []any{"a"}, "recommended": float64(0)}),
		mkev(t, core.EvMergeSequenced, "GH-2", map[string]any{"behind": "GH-1"}),
		mkev(t, core.EvIssueCreated, "GH-3", map[string]any{"title": "auth patch", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-3", map[string]any{"stage": "execute", "attempt": float64(2), "of": float64(2)}),
		mkev(t, core.EvStageFailed, "GH-3", map[string]any{"stage": "execute", "error": "boom", "attempt": float64(2), "of": float64(2), "final": true}),
	})
	out := renderTower(m.State, m.stages, m.Ids, m.Focus, 0 /*tick*/, 120)
	for _, want := range []string{"NEED-YOU", "FAILED", "after", "payment", "search fix"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "behind:") || strings.Contains(out, "🔒") {
		t.Fatalf("old jargon rendering survived:\n%s", out)
	}
}

func TestStageAliases(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	m := NewModel(nil, []string{"spec", "execute"})
	m.aliases = map[string]string{"spec": "AGREE", "execute": "BUILD"}
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "spec"}),
	})
	out := renderTower(m.State, m.stages, m.Ids, m.Focus, 0, 100)
	if !strings.Contains(out, "AGREE") || strings.Contains(out, "SPEC") {
		t.Fatalf("aliases not applied:\n%s", out)
	}
}
```

(`renderTower` signature gains `tick int`; update existing tests.)

- [ ] **Step 2: FAIL check** — `go test ./internal/tui/ -run 'Cell|Alias' -v`

- [ ] **Step 3: Implement** the cell contract table as one `cellContent(iv, ids, stageIdx, tick, focused) string` function with a switch — single source of truth. Titled headers (wrap title into two 12-char lines, faint id line). Legend line. Alias map + flags. Motion rules via tick parity.

- [ ] **Step 4: Full suite** — PASS (update older render tests for new geometry).

- [ ] **Step 5: Commit**

```bash
git add internal/tui/ cmd/
git commit -m "feat: grid v2 with titled lanes, words-in-cells, motion inversion, legend"
```

---

### Task 5: Toast v2 + evidence panel + diff pager

**Files:**
- Modify: `internal/tui/rail.go`, `internal/tui/app.go`, `internal/tui/pager.go`
- Test: `internal/tui/rail_test.go` (extend)

**Interfaces:**
- `renderToast(d projection.DecisionView, id Identity, streak int, width int) string` — renders question, `why:` line, each option with its consequence (`★ approve → planning starts now…`), reversibility, keys line, and (when `streak >= 3`) the friction line `you've accepted N recommendations in a row without opening evidence`.
- Model tracks `acceptStreak int`: +1 on `y` without having opened evidence for that decision; reset on `o` or `n`.
- Evidence panel (`o` on a toast or focused decision): reads the issue's latest `evidence.json` (path from `issue_detail.Artifacts`) and renders: files count, +/−, biggest file, per-area weights (top 5), plus `LastError`/tests availability note. `Enter` → `diff.patch` in the existing pager. No evidence artifacts (pre-execute decisions) → panel shows the decision's `paths` list and available artifacts instead.
- `esc` on toast defers (decision remains in `d` queue — Task 6).

- [ ] **Step 1: Write the failing test**

```go
func TestToastV2RendersRationaleAndConsequences(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	d := projection.DecisionView{ID: 4, IssueID: "GH-1", Stage: "spec",
		Question: "Approve the spec?", Options: []string{"approve", "reject"}, Recommended: 0,
		Why: "scope is settled", Consequences: []string{"planning starts now", "agent revises (~10 min)"},
		Reversible: "changeable until build"}
	out := renderToast(d, Identity{Color: "#61afef", Tag: "PA"}, 4, 70)
	for _, want := range []string{"scope is settled", "planning starts now", "agent revises", "changeable until build", "4 recommendations in a row"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if out2 := renderToast(d, Identity{Color: "#61afef", Tag: "PA"}, 0, 70); strings.Contains(out2, "in a row") {
		t.Fatal("friction line shown with zero streak")
	}
}

func TestEvidencePanelFromBundle(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	b := evidence.Bundle{Added: 412, Removed: 88, Biggest: "payments/gateway/client.go",
		Files: make([]evidence.FileStat, 14),
		AreaWeight: map[string]int{"payments": 300, "api": 40}}
	out := renderEvidence(b, "GH-1 payment adapter", 76)
	for _, want := range []string{"14 files", "+412", "−88", "payments/gateway/client.go", "payments"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}
```

- [ ] **Step 2: FAIL check.** — `go test ./internal/tui/ -run 'ToastV2|Evidence' -v`

- [ ] **Step 3: Implement** `renderToast` v2, `renderEvidence(b evidence.Bundle, title string, width int) string`, model plumbing: `o` loads evidence.json via `os.ReadFile` of the newest `evidence.json` artifact path (client-side read is fine — same machine as daemon; note the assumption in a comment), Enter loads diff.patch into pager, streak bookkeeping.

- [ ] **Step 4: Full suite** — PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/
git commit -m "feat: toast v2 with rationale/consequences and evidence panel with diff pager"
```

---

### Task 6: The four doors — decisions, tray, timeline, transcript

**Files:**
- Create: `internal/tui/doors.go`
- Modify: `internal/tui/app.go`
- Test: `internal/tui/doors_test.go`

**Interfaces:**
- Mode stack on the model: `modes []string` (push/pop; esc pops). Doors render full-width replacing the grid; header/footer persist.
- `d` DECISIONS: `renderDecisionsDoor(ds []projection.DecisionView, ids, sel int, width int) string` — worst-first list (queue order comes from server ordering: fetch via `list_decisions` which is already blocking-cost ordered), `j/k` select, digits or `enter` open that decision as a toast (reusing toast v2), so every decision is reachable in ≤2 keys.
- `t` TRAY: fetch `list_proposals`; render full text (title + body, wrapped); `a` accept → `resolve_proposal{Accept:true}` then `start_issue` on the returned ID (new lane appears); `r` reject.
- `e` TIMELINE: client-side filter of received events for the focused issue, newest first, humanized lines (`12:31 merged to main`, `12:18 review started (3 agents)`); rendered in the pager.
- `T` TRANSCRIPT: `transcript_tail{IssueID, N:200}` into the pager, auto-refresh on poll tick while open.

- [ ] **Step 1: Write the failing test**

```go
// internal/tui/doors_test.go
package tui

import (
	"strings"
	"testing"

	"github.com/muesli/termenv"
	"github.com/charmbracelet/lipgloss"
	"github.com/wbushyeager/guildhall/internal/projection"
)

func TestDecisionsDoorSelectable(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	ds := []projection.DecisionView{
		{ID: 2, IssueID: "GH-2", Stage: "spec", Question: "big blocker?"},
		{ID: 1, IssueID: "GH-1", Stage: "merge", Question: "small one?"},
	}
	out := renderDecisionsDoor(ds, map[string]Identity{"GH-1": {Tag: "01"}, "GH-2": {Tag: "02"}}, 1, 80)
	if strings.Index(out, "big blocker?") > strings.Index(out, "small one?") {
		t.Fatal("server order not preserved")
	}
	if !strings.Contains(out, "▸") { // selection marker on index 1
		t.Fatalf("no selection marker:\n%s", out)
	}
}

func TestTimelineHumanizes(t *testing.T) {
	lines := humanizeEvents([]core.Event{
		mkev(t, core.EvIssueMerged, "GH-1", map[string]any{"branch": "issue/GH-1"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "review"}),
	}, "GH-1")
	if len(lines) != 2 || !strings.Contains(lines[0], "merged") || strings.Contains(lines[1], "stage_started") {
		t.Fatalf("humanize: %v", lines)
	}
}
```

(Import `core` in the test.)

- [ ] **Step 2: FAIL check.** — `go test ./internal/tui/ -run 'Door|Timeline' -v`

- [ ] **Step 3: Implement** doors.go: the two render functions + `humanizeEvents(evs []core.Event, issueID string) []string` (map of event type → template; unknown types → skip), tray render + accept/reject commands, transcript fetch command. Mode-stack plumbing in `Update` (push on d/t/e/T, `esc` pops; existing pager becomes a mode too).

- [ ] **Step 4: Full suite** — PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/
git commit -m "feat: decisions, tray, timeline, and transcript doors with mode stack"
```

---

### Task 7: Control verbs + new-issue modal

**Files:**
- Create: `internal/tui/modal.go`
- Modify: `internal/tui/app.go`
- Test: `internal/tui/modal_test.go`

**Interfaces:**
- Keys on a focused issue: `p` toggle pause/resume (`pause_issue`/`resume_issue`), `x` kill current stage with a typed confirm (`renderConfirm("kill the running build stage of <title>? y/n")`), `R` retry (`retry_stage`, enabled only when State==failed/paused-after-kill), `L` lever editor: `renderLeverEditor(stages []string, matrix map[string]string, sel int) string` — j/k row, h/l cycles yolo/regular/strict, enter applies via `set_lever`, esc cancels.
- `n` new-issue modal: `type modalState struct{ Title, Body string; Field int; FlowName, Preset string }`; single required field (title), tab cycles to optional body/flow/preset; enter → `create_issue` + `start_issue`; esc cancels. Text input: plain rune append + backspace (no textinput dependency; it's two fields).
- All new keys registered in help.

- [ ] **Step 1: Write the failing test**

```go
// internal/tui/modal_test.go
package tui

import (
	"strings"
	"testing"

	"github.com/muesli/termenv"
	"github.com/charmbracelet/lipgloss"
)

func TestModalTyping(t *testing.T) {
	m := modalState{Preset: "regular", FlowName: "default"}
	for _, r := range "add rate limiting" {
		m = m.input(string(r))
	}
	m = m.input("backspace")
	if m.Title != "add rate limitin" {
		t.Fatalf("title: %q", m.Title)
	}
	lipgloss.SetColorProfile(termenv.Ascii)
	out := renderModal(m, 70)
	if !strings.Contains(out, "add rate limitin") || !strings.Contains(out, "regular") {
		t.Fatalf("modal:\n%s", out)
	}
}

func TestLeverEditorCycles(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	out := renderLeverEditor([]string{"spec", "execute"}, map[string]string{"spec": "strict", "execute": "yolo"}, 1)
	if !strings.Contains(out, "spec") || !strings.Contains(out, "yolo") || !strings.Contains(out, "▸") {
		t.Fatalf("editor:\n%s", out)
	}
}
```

- [ ] **Step 2: FAIL check.** — `go test ./internal/tui/ -run 'Modal|Lever' -v`

- [ ] **Step 3: Implement** modal.go (input state machine, renderModal, renderConfirm, renderLeverEditor) + Update wiring + socket commands. Lever matrix source: `issue_detail` — add `Levers map[string]string` to `IssueDetail` server-side (read from the issues table `levers` column; the engine must persist matrix changes there in `SetLever` — add the store write; small, in scope).

- [ ] **Step 4: Full suite** — PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ cmd/
git commit -m "feat: control verbs, lever editor, and new-issue modal"
```

---

### Task 8: FOCUS panel v2 + MAP insight line + calm tree v2

**Files:**
- Modify: `internal/tui/rail.go`, `internal/tui/arch.go`
- Test: `internal/tui/rail_test.go`, existing arch tests (extend)

**Interfaces:**
- FOCUS panel rows: (1) chip + id + full title + stage + status phrase; (2) failure block when failing: `error: <LastError> · attempt N of M`; (3) `budget ▰▰▰▱ 74% ($2.10)` when Budget>0 (tokens/budget × price), `spent 41k tokens` otherwise · `levers B:auto S:you E:auto…` (aliased first letters, `you` = strict/gate, `auto` = yolo); (4) worktree path + session id, faint (diagnostic row).
- MAP line (replaces the arch strip): `MAP · <insight> · a full map` where insight = contention first (`api/ has 2 builders (sequenced)`), else span (`GH-6 spans 7 areas`), else `quiet`. Computed from `AreaWeights` across issues: builder = area weight ≥ 25% of the issue's total weight; contention = ≥2 builders on one area.
- Tree v2 (inside `a`): one mark column (◈ builders, ◌ brushers per the same 25% rule), activity-expanded, `▸ n quiet areas` collapsed line, selection bar with details, `1-9` filter (existing), and NO file counts/churn in rows.

- [ ] **Step 1: Write the failing test**

```go
func TestBuildersAndContention(t *testing.T) {
	issues := map[string]*projection.IssueView{
		"GH-1": {ID: "GH-1", AreaWeights: map[string]int{"payments": 300, "api": 40}},
		"GH-2": {ID: "GH-2", AreaWeights: map[string]int{"api": 200}},
	}
	b := buildersByArea(issues)
	if !b["payments"]["GH-1"] || b["api"]["GH-1"] { // 40/340 < 25% → brusher not builder
		t.Fatalf("builders wrong: %v", b)
	}
	if !b["api"]["GH-2"] {
		t.Fatalf("GH-2 should build api: %v", b)
	}
	insight := mapInsight(issues)
	if !strings.Contains(insight, "quiet") == false && insight == "" {
		t.Fatal("no insight")
	}
	// force contention
	issues["GH-1"].AreaWeights["api"] = 200
	if !strings.Contains(mapInsight(issues), "api") {
		t.Fatalf("contention not surfaced: %q", mapInsight(issues))
	}
}
```

- [ ] **Step 2: FAIL check.** — `go test ./internal/tui/ -run 'Builders' -v`

- [ ] **Step 3: Implement** `buildersByArea`, `mapInsight`, FOCUS rows (data from issue_detail + projection), tree v2 render rewrite honoring the one-mark-column rule.

- [ ] **Step 4: Full suite** — PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/
git commit -m "feat: focus panel v2, map insight line, calm tree with builder/brusher marks"
```

---

### Task 9: Overflow, shipped shelf, help, final smoke

**Files:**
- Modify: `internal/tui/render.go`, `internal/tui/app.go`
- Test: `internal/tui/render_test.go` (extend)

**Interfaces:**
- Overflow: `visibleLanes(order []string, focus int, width int) (full []string, compactLeft, compactRight []string)` — lanes needing width beyond terminal compact to 5-char chip columns at the edges with `‹n›` gutters; focused ±1 always full. `z` toggles `rows` mode: transposed full-width one-row-per-issue rendering (`renderRows`) reusing `cellContent` — same states, horizontal layout.
- Shelf: merged issues auto-retire after `--retire-after` (default 5m; model-side timer keyed on MergedAt), or `c` immediately; retired render as one `SHIPPED today ▓ <chip>title ⇡ …` line; killed/parked issues go to a `PARKED` segment (never auto-hidden without it). `u` opens a shelf door (list, `enter` un-retires the lane).
- Help (`?`): full key table grouped by area.
- Final manual smoke checklist (real terminal): two issues via `n` modal; watch titled lanes; force a failure (fake runner Fail script) → FAILED blink + Tab + FOCUS failure block + `R` retry; `p` pause/resume; toast v2 with why/consequences; `o` evidence (fake runner produces no diff — expect the fallback panel); `d` `t` `e` `T` doors; `L` lever editor; 8 issues to trigger overflow compaction and `z` rows mode; `c`/`u` shelf; `?` help; `q`.

- [ ] **Step 1: Write the failing test**

```go
func TestVisibleLanesCompaction(t *testing.T) {
	order := []string{"A", "B", "C", "D", "E", "F", "G", "H"}
	full, left, right := visibleLanes(order, 4 /*focused=E*/, 100) // ~6 full lanes fit
	if len(left)+len(full)+len(right) != 8 {
		t.Fatalf("lanes lost: %v %v %v", left, full, right)
	}
	found := false
	for _, f := range full {
		if f == "E" {
			found = true
		}
	}
	if !found {
		t.Fatal("focused lane not full-width")
	}
	if len(left) == 0 && len(right) == 0 {
		t.Fatal("no compaction at 8 lanes/100 cols")
	}
}

func TestShelfRendersShippedAndParked(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	out := renderShelf([]shelfItem{{ID: "GH-1", Title: "payment adapter", Parked: false},
		{ID: "GH-3", Title: "auth patch", Parked: true}},
		map[string]Identity{"GH-1": {Tag: "PA"}, "GH-3": {Tag: "AP"}}, 100)
	if !strings.Contains(out, "SHIPPED") || !strings.Contains(out, "PARKED") || !strings.Contains(out, "auth patch") {
		t.Fatalf("shelf:\n%s", out)
	}
}
```

- [ ] **Step 2: FAIL check.** — `go test ./internal/tui/ -run 'Compaction|Shelf' -v`

- [ ] **Step 3: Implement** `visibleLanes`, chip-column rendering, `z` rows mode, shelf state (retire timers from `MergedAt` + `c`/`u`), `renderShelf`, shelf door, help screen.

- [ ] **Step 4: Full suite + smoke** — `go test ./... -race && go build ./...`; run the manual checklist above with the fake daemon; list any items that need the human.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/ cmd/
git commit -m "feat: lane overflow compaction, rows mode, shipped shelf, help screen"
```

---

## Self-review notes

- **Coverage vs the Plan 4b proposal (TUI half):** palette purge + dedupe (T1 ← InfoDesign 2, tag collisions), words-in-cells + legend + motion inversion + plain copy + aliases (T4 ← Layman 1/2, InfoDesign 1/3/5), status sentence + outcome war room + reserved notice row (T3 ← Layman 3/4, InfoDesign 4), toast v2 + streak friction + evidence panel + diff pager (T5 ← Layman 5, Operator 2), four doors (T6 ← Operator 1/3/6/9), control verbs + lever editor + `n` modal (T7 ← Operator 4/10, lifecycle), FOCUS v2 failure/budget/levers/diagnostics (T8 ← Operator 5/8/10, Layman 9), MAP insight + calm tree builders/brushers (T8 ← Layman 8 + arch refinement), overflow + `z` rows + shelf/PARKED (T9 ← Operator 7, lifecycle).
- **Assumptions stated:** TUI reads evidence files directly from disk (same-machine client — noted in code; a future remote client needs a file-fetch op); retire timer is client-side presentation state (lost on TUI restart — acceptable, it re-derives from MergedAt); rows mode reuses cellContent so states can't diverge between orientations.
- **Update-not-break:** renderTower signature change (tick param) and palette swap will break existing tests — the plan explicitly amends them (T1 step 4, T4 step 4); teatest snapshots don't exist, string assertions are updated inline.
- **Type consistency:** `evidence.Bundle` consumed by renderEvidence ✓; `projection.DecisionView` v2 fields flow into toast/doors ✓; `IssueDetail.Levers`/`Budget` added once (T7/T2) and consumed in T8 ✓; `cellContent` single source for grid + rows ✓.
