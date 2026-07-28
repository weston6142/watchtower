# TUI Design Cohesion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restyle every control-room flow onto one shared design system (13-token theme, glyph language, chrome header/keybar, one overlay box) and build a self-serve visual verification harness (fixture snapshots → ANSI goldens + freeze PNGs, plus a tmux capture script).

**Architecture:** Extend `internal/tui` in place: `theme.go` grows to 13 tokens, a new `chrome.go` holds shared components (glyphs, header, keybar, key chips, cursor rows, boxed overlays), and each renderer migrates flow-by-flow. A hidden `watchtower snap` subcommand renders each flow from fixture state through the real `Model.View()`; golden ANSI files under `internal/tui/testdata/` are the regression net and freeze-generated PNGs are the visual check.

**Tech Stack:** Go, bubbletea/lipgloss, charmbracelet `freeze` CLI (dev-only), tmux (dev-only).

**Spec:** `docs/2026-07-28-tui-design-cohesion-design.md` — read it before starting any task.

## Global Constraints

- No behavior changes: key handling, daemon protocol, projection logic untouched. Renderers and styles only (plus the new `snap` subcommand and fixtures).
- Color contract: Accent = "enter does something here"; Warn = "human needed"; Ok/Err = outcomes only; Structure = identity/nouns, never state. Hues never trade jobs.
- Glyphs: `●` done, `◔` need-you, `◐` working (Structure blue), `○` waiting/idle, `✕` failed, `▸` cursor, `⇡` shipped, `⏸` parked. No other state glyphs anywhere.
- Every task ends with: `go test ./...` green, goldens regenerated intentionally (`go test ./internal/tui -run TestSnapshots -update`), PNGs regenerated and **visually inspected by Reading them**, then commit.
- PNGs are never committed. Add `/tmp-snaps/` (the default snap output dir) to `.gitignore` in Task 1.
- Module path is `github.com/weston6142/watchtower`.

---

### Task 1: Snapshot harness — fixtures, `snap` subcommand, golden tests

**Files:**
- Create: `internal/tui/fixtures.go`
- Create: `internal/tui/snapshot_test.go`
- Modify: `cmd/watchtower/main.go` (add hidden `snap` case to the command switch)
- Create: `scripts/snap.sh`
- Modify: `.gitignore` (add `/tmp-snaps/`)

**Interfaces:**
- Produces: `FixtureModel(flow string, width, height int) Model` and `FixtureFlows() []string` in package `tui` — every later task regenerates goldens through these.
- Produces: CLI `watchtower snap [--out DIR] [--width N] [--height N] [--flow NAME]` writing `DIR/<flow>.txt` ANSI dumps.
- Produces: `scripts/snap.sh [flow]` → builds, runs snap, converts each `.txt` to `.png` with freeze when installed.

- [ ] **Step 1: Install freeze (dev machine)**

Run: `brew install charmbracelet/tap/freeze || brew install freeze`
Verify: `freeze --version` prints a version. If brew fails, `go install github.com/charmbracelet/freeze@latest`.

- [ ] **Step 2: Write `internal/tui/fixtures.go`**

The fixture state exercises: a need-you decision, a working stage with tokens, a failed stage, shipped + parked shelf, 3 proposals, long/unicode titles. `FixtureModel` returns a fully-populated `Model` posed for a named flow so snapshots go through the real `View()`.

```go
package tui

import (
	"github.com/weston6142/watchtower/internal/archmap"
	"github.com/weston6142/watchtower/internal/projection"
	"github.com/weston6142/watchtower/internal/proto"
	"github.com/weston6142/watchtower/internal/store"
)

// FixtureFlows lists every posable flow, in spec order.
func FixtureFlows() []string {
	return []string{"floor", "rows", "decision", "decisions-door", "tray", "modal", "levers", "arch", "pager", "help"}
}

func fixtureState() *projection.State {
	st := projection.NewState()
	st.Order = []string{"ca-repo", "gh-importer", "fx-e2e"}
	st.Issues["ca-repo"] = &projection.IssueView{
		ID: "ca-repo", Title: "create a repo for GH-1", Flow: "default",
		CurrentStage: "brainstorm", State: "need-you", Tokens: 12000,
	}
	st.Issues["gh-importer"] = &projection.IssueView{
		ID: "gh-importer", Title: "issue importer — GitHub → watchtower", Flow: "default",
		CurrentStage: "execute", State: "running", Tokens: 96000,
		Completed: []string{"brainstorm", "spec", "plan"},
	}
	st.Issues["fx-e2e"] = &projection.IssueView{
		ID: "fx-e2e", Title: "flaky e2e fix — «unicode» + a deliberately very long title to test truncation", Flow: "default",
		CurrentStage: "execute", State: "failed", LastError: "tests failed: importer_webhook_test.go", Tokens: 41000,
		Completed: []string{"brainstorm", "spec", "plan"}, Attempt: 2, AttemptOf: 3,
	}
	st.Decisions[1] = projection.DecisionView{
		ID: 1, IssueID: "ca-repo", Stage: "brainstorm",
		Question: "Should creating the repo mean just local version control, or also a hosted remote (GitHub) pushed from day one?",
		Options: []string{
			"Local repo + GitHub remote, pushed — backed up and shareable now; needs credentials and an owner/name today.",
			"Local repo only, remote later — simplest setup, nothing published; work stays on this machine until a remote is added.",
		},
		Recommended: 0,
		Why:         "The issue is numbered GH-1, suggesting GitHub is the intended home; a remote gives backup and collaboration immediately.",
		Reversible:  "Cheap to change until others clone the remote or CI points at it.",
	}
	st.ShippedToday = []string{"ml-retry"}
	st.Parked = []string{"fx-dark"}
	st.Issues["ml-retry"] = &projection.IssueView{ID: "ml-retry", Title: "retry budget for marshal", Merged: true, State: "done"}
	st.Issues["fx-dark"] = &projection.IssueView{ID: "fx-dark", Title: "dark-mode audit", Paused: true}
	return st
}

func fixtureProposals() []store.ProposalRow {
	return []store.ProposalRow{
		{Title: "Add smoke test for the importer webhook", Body: "The last two importer regressions were webhook-shaped. A 30-second smoke test on PR would have caught both before review."},
		{Title: "Split marshal retries into their own lever", Body: "Retry budget currently rides on the global aggressiveness lever; tuning one shouldn't move the other."},
		{Title: "Park FX-9 until the token budget resets", Body: "Dark-mode audit is cosmetic and burning budget the failing e2e fix needs this week."},
	}
}

func fixtureArch() *archmap.Map {
	return &archmap.Map{Modules: []archmap.Module{
		{Name: "internal/tui"}, {Name: "internal/marshal"}, {Name: "internal/store"},
		{Name: "internal/engine"}, {Name: "internal/flow"}, {Name: "internal/levers"},
	}}
}

// FixtureModel returns a Model posed for the named flow at the given size.
func FixtureModel(flowName string, width, height int) Model {
	m := Model{
		State: fixtureState(),
		Overview: &proto.Overview{
			Building: 2, NeedYou: 1, Failing: 1, ShippedToday: 1,
			TokensTotal: 412000, DollarsTotal: 6.18,
		},
		Ids: map[string]Identity{
			"ca-repo":     {Tag: "CA", Color: "#bb9af7"},
			"gh-importer": {Tag: "GH", Color: "#7aa2f7"},
			"fx-e2e":      {Tag: "FX", Color: "#7dcfff"},
			"ml-retry":    {Tag: "ML", Color: "#9ece6a"},
			"fx-dark":     {Tag: "FX", Color: "#7dcfff"},
		},
		Focus:  Focus{Issue: "ca-repo"},
		Width:  width,
		Height: height,
	}
	m.stages = []string{"brainstorm", "spec", "plan", "execute", "review", "merge"}
	m.dismissed = map[int64]bool{}
	m.retired = map[string]bool{}
	m.evidenceOpened = map[int64]bool{}
	switch flowName {
	case "rows":
		m.rows = true
	case "decision":
		d := m.State.Decisions[1]
		m.Toast = &d
	case "decisions-door":
		m.modes = []string{"decisions"}
	case "tray":
		m.modes = []string{"tray"}
		m.proposals = fixtureProposals()
	case "modal":
		m.modal = &modalState{Title: "Wire importer smoke test into CI", Field: 0}
	case "levers":
		m.leverEditor = newLeverEditor("ca-repo", m.stages, map[string]string{
			"brainstorm": "regular", "spec": "regular", "plan": "regular",
			"execute": "regular", "review": "strict", "merge": "regular",
		})
	case "arch":
		m.archMode = "full"
		m.Arch = fixtureArch()
	case "pager":
		m.pager = pagerState{Mode: "pager", Title: "evidence · decision 1", Lines: []string{
			"docs/brainstorm/repo-scope.md",
			"@@ options considered @@",
			"  The issue tracker prefix (GH-) implies a GitHub home.",
			"+ Recommendation: create remote now; renaming later breaks",
			"+ clone URLs and CI triggers once anything points at it.",
			"- Alternative: defer remote until first collaborator.",
			"  Cost of reversal stays low until CI or clones exist.",
		}}
	case "help":
		m.help = true
	}
	return m
}

// SnapshotFlow renders one flow's full screen for snapshots and goldens.
func SnapshotFlow(flowName string, width, height int) string {
	m := FixtureModel(flowName, width, height)
	return m.View()
}
```

Note: check `pagerState`'s real field names in `internal/tui/pager.go` before writing this — pose it with whatever fields `renderPager` actually reads (adjust `Title`/`Lines` to match; if the pager stores content differently, mirror that). Same for `newLeverEditor`'s signature in `modal.go:~140`. Adjust the fixture, not the production types.

- [ ] **Step 3: Write the failing golden test `internal/tui/snapshot_test.go`**

```go
package tui

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden snapshot files")

func TestSnapshots(t *testing.T) {
	for _, flow := range FixtureFlows() {
		for _, size := range []struct {
			name          string
			width, height int
		}{{"wide", 200, 50}, {"narrow", 100, 40}} {
			t.Run(flow+"-"+size.name, func(t *testing.T) {
				got := SnapshotFlow(flow, size.width, size.height)
				golden := filepath.Join("testdata", flow+"-"+size.name+".golden")
				if *update {
					if err := os.MkdirAll("testdata", 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
					return
				}
				want, err := os.ReadFile(golden)
				if err != nil {
					t.Fatalf("missing golden %s — run: go test ./internal/tui -run TestSnapshots -update", golden)
				}
				if string(want) != got {
					t.Errorf("%s render drifted from golden; if intentional run with -update", flow)
				}
			})
		}
	}
}
```

- [ ] **Step 4: Run test to verify it fails**

Run: `go test ./internal/tui -run TestSnapshots`
Expected: FAIL — compile errors first (fix fixture field names against the real structs), then "missing golden".

- [ ] **Step 5: Generate goldens, verify pass**

Run: `go test ./internal/tui -run TestSnapshots -update && go test ./internal/tui -run TestSnapshots`
Expected: PASS. Inspect one golden (`head -20 internal/tui/testdata/floor-wide.golden`) — it should contain ANSI escapes and recognizable content.

Important: lipgloss degrades colors when it detects a dumb terminal. If goldens come out colorless, force the color profile at the top of `SnapshotFlow`:

```go
import "github.com/charmbracelet/lipgloss"
// in an init or at the top of SnapshotFlow:
lipgloss.SetColorProfile(termenv.TrueColor) // import "github.com/muesli/termenv"
```

Only add this if needed; keep it inside the fixtures file so production behavior is unchanged.

- [ ] **Step 6: Add the `snap` subcommand to `cmd/watchtower/main.go`**

In the command switch (alongside `case "tower":`), add — do NOT add it to any usage/help output:

```go
case "snap":
	fs := flag.NewFlagSet("snap", flag.ExitOnError)
	out := fs.String("out", "tmp-snaps", "output directory")
	width := fs.Int("width", 200, "render width")
	height := fs.Int("height", 50, "render height")
	only := fs.String("flow", "", "render a single flow")
	_ = fs.Parse(args)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}
	for _, f := range tui.FixtureFlows() {
		if *only != "" && f != *only {
			continue
		}
		path := filepath.Join(*out, f+".txt")
		if err := os.WriteFile(path, []byte(tui.SnapshotFlow(f, *width, *height)), 0o644); err != nil {
			fatal(err)
		}
		fmt.Println(path)
	}
```

Match the file's existing error-handling helper (check how other cases report errors — there may be no `fatal`; mirror whatever `case "tower"` does). Import `internal/tui` and `path/filepath` as needed. Note `SnapshotFlow` and `FixtureFlows` must be exported for this (they are, per Step 2).

- [ ] **Step 7: Write `scripts/snap.sh`**

```bash
#!/usr/bin/env bash
# Render every TUI flow from fixtures to ANSI + PNG for visual inspection.
# Usage: scripts/snap.sh [flow]
set -euo pipefail
cd "$(dirname "$0")/.."
out=tmp-snaps
go run ./cmd/watchtower snap --out "$out" ${1:+--flow "$1"}
if command -v freeze >/dev/null; then
  for f in "$out"/*.txt; do
    freeze "$f" --output "${f%.txt}.png"
    echo "${f%.txt}.png"
  done
else
  echo "freeze not installed — ANSI dumps only" >&2
fi
```

Run: `chmod +x scripts/snap.sh && echo '/tmp-snaps/' >> .gitignore`

- [ ] **Step 8: Run the harness end-to-end and LOOK at the output**

Run: `scripts/snap.sh`
Expected: one PNG per flow in `tmp-snaps/`. **Read `tmp-snaps/floor.png` and `tmp-snaps/decision.png`** — they should show the current (pre-redesign) UI. This is the "before" baseline; it proves the loop works.

- [ ] **Step 9: Full test suite + commit**

Run: `go test ./...`
Expected: PASS.

```bash
git add internal/tui/fixtures.go internal/tui/snapshot_test.go internal/tui/testdata cmd/watchtower/main.go scripts/snap.sh .gitignore
git commit -m "feat: fixture snapshot harness for TUI flows (watchtower snap + goldens)"
```

---

### Task 2: tmux capture script (acceptance tool)

**Files:**
- Create: `scripts/tui-capture.sh`

**Interfaces:**
- Produces: `scripts/tui-capture.sh <flow>` where flow ∈ floor|decision|tray|modal|levers|arch|help — writes `tmp-snaps/live-<flow>.png`.

- [ ] **Step 1: Write the script**

```bash
#!/usr/bin/env bash
# Capture the REAL watchtower TUI in tmux against a seeded temp daemon.
# Usage: scripts/tui-capture.sh <floor|decision|tray|modal|levers|arch|help>
set -euo pipefail
cd "$(dirname "$0")/.."
flow="${1:-floor}"
sess="ghsnap-$$"
dir="$(mktemp -d)"
out=tmp-snaps
mkdir -p "$out"
bin="$dir/watchtower"
go build -o "$bin" ./cmd/watchtower

cleanup() { tmux kill-session -t "$sess" 2>/dev/null || true; }
trap cleanup EXIT

# Seed: init a workspace in the temp dir and create one issue.
(cd "$dir" && "$bin" init >/dev/null 2>&1 || true)
(cd "$dir" && "$bin" new --title "create a repo for GH-1" >/dev/null 2>&1 || true)

tmux new-session -d -s "$sess" -x 200 -y 50 "cd '$dir' && '$bin' tower"

# Wait for first paint.
for _ in $(seq 1 50); do
  tmux capture-pane -t "$sess" -p | grep -q . && break
  sleep 0.2
done

# Drive to the requested flow.
case "$flow" in
  floor) ;;
  decision) tmux send-keys -t "$sess" d ;;
  tray) tmux send-keys -t "$sess" t ;;
  modal) tmux send-keys -t "$sess" n ;;
  levers) tmux send-keys -t "$sess" L ;;
  arch) tmux send-keys -t "$sess" A ;;
  help) tmux send-keys -t "$sess" '?' ;;
  *) echo "unknown flow: $flow" >&2; exit 1 ;;
esac
sleep 0.5

tmux capture-pane -t "$sess" -p -e > "$out/live-$flow.txt"
if command -v freeze >/dev/null; then
  freeze "$out/live-$flow.txt" --output "$out/live-$flow.png"
  echo "$out/live-$flow.png"
else
  echo "$out/live-$flow.txt"
fi
```

Check the real `init`/`new` flag names in `cmd/watchtower/main.go` (Step 1 of implementing this: `go run ./cmd/watchtower new -h`) and fix the seeding lines to match. If `tower` needs a daemon flag or the init needs `--repo`, mirror how `cmd/watchtower/integration_test.go` boots things.

- [ ] **Step 2: Verify it captures**

Run: `chmod +x scripts/tui-capture.sh && scripts/tui-capture.sh floor`
Expected: prints `tmp-snaps/live-floor.png`. **Read the PNG** — real binary, current design.

- [ ] **Step 3: Commit**

```bash
git add scripts/tui-capture.sh
git commit -m "feat: tmux live-capture script for TUI acceptance checks"
```

---

### Task 3: Theme expansion to 13 tokens + glyph and spacing constants

**Files:**
- Modify: `internal/tui/theme.go` (full rewrite of the struct + presets)
- Modify: `internal/tui/render.go:14-28` (delete `statusBad/statusWarn/statusOk` vars and the three `styleStatus*` funcs; re-point callers)
- Create: `internal/tui/chrome.go` (glyph + spacing constants only, in this task)
- Modify: `internal/tui/theme_test.go`
- Test: `internal/tui/theme_test.go`

**Interfaces:**
- Produces: `Theme` struct with fields `Bg0, Bg1, Bg2, Bg3, Accent, Structure, Ok, Warn, Err, Text, Dim, Dimmer, Bright lipgloss.Color`.
- Produces: glyph constants `glyphDone, glyphNeedYou, glyphWorking, glyphWaiting, glyphFailed, glyphCursor, glyphShipped, glyphParked` (strings).
- Produces: spacing constants `padV = 1`, `padH = 2`, `gutter = 2` (ints).
- Consumers everywhere: `activeTheme.Ok` replaces `styleStatusOk()`, etc. `Heading` is renamed `Structure`; `Panel` is renamed `Bg2` (its only use is the chip foreground in `renderBox`, which becomes `Bg0` on accent for contrast — verify visually).

- [ ] **Step 1: Update `theme_test.go` with the new expectations (failing test)**

```go
package tui

import "testing"

func TestThemePresetsComplete(t *testing.T) {
	for name, th := range themes {
		for field, v := range map[string]string{
			"Bg0": string(th.Bg0), "Bg1": string(th.Bg1), "Bg2": string(th.Bg2), "Bg3": string(th.Bg3),
			"Accent": string(th.Accent), "Structure": string(th.Structure),
			"Ok": string(th.Ok), "Warn": string(th.Warn), "Err": string(th.Err),
			"Text": string(th.Text), "Dim": string(th.Dim), "Dimmer": string(th.Dimmer), "Bright": string(th.Bright),
		} {
			if v == "" {
				t.Errorf("theme %q: token %s is empty", name, field)
			}
		}
	}
}

func TestThemeByNameFallsBack(t *testing.T) {
	if themeByName("nope") != themes[defaultThemeName] {
		t.Error("unknown theme should fall back to default")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tui -run TestTheme`
Expected: FAIL (compile: unknown fields).

- [ ] **Step 3: Rewrite `theme.go`**

```go
package tui

import "github.com/charmbracelet/lipgloss"

// Theme is the 13-token palette. The color contract: Accent means "enter
// does something here"; Warn means "a human is needed"; Ok/Err are outcomes
// only; Structure identifies (nouns, groups, working agents) and never
// signals need. Hues never trade jobs.
type Theme struct {
	Bg0       lipgloss.Color // canvas — content ground
	Bg1       lipgloss.Color // chrome — header/footer bars, cards
	Bg2       lipgloss.Color // selection — cursor rows, card header bands
	Bg3       lipgloss.Color // key chips
	Accent    lipgloss.Color // focus & agency
	Structure lipgloss.Color // nouns & identity (help groups, arch modules, working)
	Ok        lipgloss.Color // outcomes: done, shipped, additions
	Warn      lipgloss.Color // attention: need-you, questions, ★
	Err       lipgloss.Color // outcomes: failed, deletions, errors
	Text      lipgloss.Color // body
	Dim       lipgloss.Color // secondary
	Dimmer    lipgloss.Color // idle glyphs, ghost content
	Bright    lipgloss.Color // titles
}

const defaultThemeName = "tokyo-night"

var themes = map[string]Theme{
	defaultThemeName: {
		Bg0: "#16161e", Bg1: "#1a1b26", Bg2: "#1f2335", Bg3: "#292e42",
		Accent: "#bb9af7", Structure: "#7aa2f7",
		Ok: "#9ece6a", Warn: "#e0af68", Err: "#f7768e",
		Text: "#c0caf5", Dim: "#565f89", Dimmer: "#3b4261", Bright: "#e4ecff",
	},
	"terminal": {
		// ANSI-16 approximation: two grounds only; glyphs carry state.
		Bg0: "0", Bg1: "0", Bg2: "8", Bg3: "8",
		Accent: "13", Structure: "12",
		Ok: "10", Warn: "11", Err: "9",
		Text: "7", Dim: "8", Dimmer: "8", Bright: "15",
	},
	"catppuccin": {
		Bg0: "#181825", Bg1: "#1e1e2e", Bg2: "#313244", Bg3: "#45475a",
		Accent: "#cba6f7", Structure: "#89b4fa",
		Ok: "#a6e3a1", Warn: "#f9e2af", Err: "#f38ba8",
		Text: "#cdd6f4", Dim: "#6c7086", Dimmer: "#45475a", Bright: "#f0f4ff",
	},
	"gruvbox": {
		Bg0: "#282828", Bg1: "#32302f", Bg2: "#3c3836", Bg3: "#504945",
		Accent: "#d3869b", Structure: "#83a598",
		Ok: "#b8bb26", Warn: "#fabd2f", Err: "#fb4934",
		Text: "#ebdbb2", Dim: "#928374", Dimmer: "#665c54", Bright: "#fbf1c7",
	},
}

func themeByName(name string) Theme {
	if t, ok := themes[name]; ok {
		return t
	}
	return themes[defaultThemeName]
}

var activeTheme = themeByName(defaultThemeName)

// SetTheme selects the active preset by name; unknown names keep the default.
func SetTheme(name string) { activeTheme = themeByName(name) }
```

- [ ] **Step 4: Create `chrome.go` with glyph + spacing constants**

```go
package tui

// State glyphs — the ONLY state vocabulary any surface may use.
const (
	glyphDone    = "●"
	glyphNeedYou = "◔"
	glyphWorking = "◐"
	glyphWaiting = "○"
	glyphFailed  = "✕"
	glyphCursor  = "▸"
	glyphShipped = "⇡"
	glyphParked  = "⏸"
)

// Spacing — renderers use these, never literal spacing.
const (
	padV   = 1 // blank lines inside a panel
	padH   = 2 // cells of horizontal panel padding
	gutter = 2 // cells between adjacent panels
)
```

- [ ] **Step 5: Fix all compile errors from the rename**

Run: `go build ./... 2>&1 | head -30` and iterate. Mechanical mapping:
- `t.Heading` → `t.Structure`
- `t.Panel` → `t.Bg0` (the `renderBox` chip foreground — dark text on accent)
- `styleStatusOk()` → `lipgloss.NewStyle().Foreground(activeTheme.Ok)` (and Warn/Bad→Err); then delete the three funcs and the `statusBad/statusWarn/statusOk` vars from `render.go`.

Grep to confirm nothing is left: `grep -rn "statusBad\|statusWarn\|statusOk\|\.Heading\|\.Panel" internal/tui/` → no hits (excluding this plan/spec).

- [ ] **Step 6: Tests, goldens, visual check**

Run: `go test ./... ` — fix any unit tests asserting old colors.
Run: `go test ./internal/tui -run TestSnapshots -update && scripts/snap.sh`
**Read `tmp-snaps/floor.png`** — colors shift slightly (status hexes now themed); layout unchanged.

- [ ] **Step 7: Commit**

```bash
git add internal/tui .gitignore
git commit -m "feat: expand Theme to 13 tokens; glyph and spacing constants"
```

---

### Task 4: Shared chrome components

**Files:**
- Modify: `internal/tui/chrome.go` (add components)
- Create: `internal/tui/chrome_test.go`
- Modify: `internal/tui/modal.go:69-84` (`renderBox` gains a header-band look)

**Interfaces:**
- Produces (all in package `tui`):
  - `renderChromeHeader(width int, badges []badge, right string) string`
  - `type badge struct { Text string; Kind badgeKind }` with `badgeKind` consts `badgeWarn, badgeErr, badgeOk`
  - `renderKeybar(width int, bindings [][2]string, right string) string` — bindings are (key, label) pairs; right renders in Err style when non-empty
  - `keyChip(key string) string`
  - `cursorRow(selected bool, content string, width int) string`
  - `renderBox(title, sub, chipText, content string) string` — same signature, new look: Bg2 header band + accent border.

- [ ] **Step 1: Write failing tests `internal/tui/chrome_test.go`**

```go
package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestRenderChromeHeaderContainsBadgesAndRight(t *testing.T) {
	got := ansi.Strip(renderChromeHeader(120, []badge{
		{Text: "1 question for you", Kind: badgeWarn},
		{Text: "1 build failing", Kind: badgeErr},
	}, "2 building · 412k tokens"))
	for _, want := range []string{"1 question for you", "1 build failing", "412k tokens"} {
		if !strings.Contains(got, want) {
			t.Errorf("header missing %q in %q", want, got)
		}
	}
}

func TestKeybarDocksErrorRight(t *testing.T) {
	got := ansi.Strip(renderKeybar(100, [][2]string{{"j/k", "floors"}, {"?", "help"}}, "no pending decision 1"))
	if !strings.Contains(got, "no pending decision 1") || !strings.Contains(got, "floors") {
		t.Errorf("keybar wrong: %q", got)
	}
}

func TestCursorRow(t *testing.T) {
	sel := ansi.Strip(cursorRow(true, "hello", 40))
	if !strings.HasPrefix(sel, glyphCursor) {
		t.Errorf("selected row must start with cursor glyph: %q", sel)
	}
	unsel := ansi.Strip(cursorRow(false, "hello", 40))
	if !strings.HasPrefix(unsel, "  ") {
		t.Errorf("unselected row must keep a 2-cell gutter: %q", unsel)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/tui -run 'TestRenderChrome|TestKeybar|TestCursorRow'`
Expected: FAIL (undefined symbols).

- [ ] **Step 3: Implement in `chrome.go`**

```go
import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type badgeKind int

const (
	badgeWarn badgeKind = iota
	badgeErr
	badgeOk
)

type badge struct {
	Text string
	Kind badgeKind
}

func badgeColor(k badgeKind) lipgloss.Color {
	switch k {
	case badgeErr:
		return activeTheme.Err
	case badgeOk:
		return activeTheme.Ok
	default:
		return activeTheme.Warn
	}
}

func renderBadge(b badge) string {
	return lipgloss.NewStyle().Foreground(badgeColor(b.Kind)).Bold(true).Render("● " + b.Text)
}

// renderChromeHeader is the full-width Bg1 bar: badges left, dim detail right.
func renderChromeHeader(width int, badges []badge, right string) string {
	t := activeTheme
	parts := make([]string, 0, len(badges))
	for _, b := range badges {
		parts = append(parts, renderBadge(b))
	}
	left := strings.Join(parts, lipgloss.NewStyle().Foreground(t.Dim).Render("  "))
	r := lipgloss.NewStyle().Foreground(t.Dim).Render(right)
	return chromeBar(width, left, r)
}

// renderKeybar is the full-width Bg1 footer: key chips left, error slot right.
func renderKeybar(width int, bindings [][2]string, right string) string {
	t := activeTheme
	parts := make([]string, 0, len(bindings))
	label := lipgloss.NewStyle().Foreground(t.Dim)
	for _, b := range bindings {
		parts = append(parts, keyChip(b[0])+label.Render(" "+b[1]))
	}
	left := strings.Join(parts, label.Render("  "))
	r := ""
	if right != "" {
		r = lipgloss.NewStyle().Foreground(t.Err).Render(glyphFailed + " " + right)
	}
	return chromeBar(width, left, r)
}

// chromeBar lays left and right on one Bg1 row spanning width.
func chromeBar(width int, left, right string) string {
	t := activeTheme
	pad := width - lipgloss.Width(left) - lipgloss.Width(right) - 2*padH
	if pad < 1 {
		pad = 1
	}
	row := strings.Repeat(" ", padH) + left + strings.Repeat(" ", pad) + right + strings.Repeat(" ", padH)
	return lipgloss.NewStyle().Background(t.Bg1).Width(max(1, width)).Render(row)
}

func keyChip(key string) string {
	t := activeTheme
	return lipgloss.NewStyle().Foreground(t.Bright).Background(t.Bg3).Padding(0, 1).Render(key)
}

// cursorRow renders a selectable list row: accent ▸ + Bg2 ground when
// selected, a 2-cell gutter otherwise. Every list uses this.
func cursorRow(selected bool, content string, width int) string {
	t := activeTheme
	if !selected {
		return "  " + content
	}
	row := lipgloss.NewStyle().Foreground(t.Accent).Render(glyphCursor) + " " + content
	return lipgloss.NewStyle().Background(t.Bg2).Width(max(1, width)).Render(row)
}
```

- [ ] **Step 4: Restyle `renderBox` (same signature)**

Replace the body in `modal.go`:

```go
// renderBox wraps content in the shared overlay chrome: a Bg2 header band
// holding the bright title, dim subtitle, and inverted accent chip, over an
// accent-bordered Bg1 body. Every overlay uses this box.
func renderBox(title, sub, chipText, content string) string {
	t := activeTheme
	head := lipgloss.NewStyle().Foreground(t.Bright).Bold(true).Render(title)
	if sub != "" {
		head += lipgloss.NewStyle().Foreground(t.Dim).Render(" " + sub)
	}
	chip := lipgloss.NewStyle().Foreground(t.Bg0).Background(t.Accent).Bold(true).Render(chipText)
	inner := max(lipgloss.Width(content), lipgloss.Width(head)+lipgloss.Width(chip)+2)
	gap := max(1, inner-lipgloss.Width(head)-lipgloss.Width(chip))
	band := lipgloss.NewStyle().Background(t.Bg2).Width(inner).
		Render(head + strings.Repeat(" ", gap) + chip)
	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(t.Accent).
		Background(t.Bg1).
		Padding(0, 1).
		Render(band + "\n\n" + content)
}
```

- [ ] **Step 5: Tests, goldens, LOOK**

Run: `go test ./internal/tui` then `-update`, then `scripts/snap.sh`.
**Read `tmp-snaps/modal.png` and `tmp-snaps/help.png`** — overlays now show the Bg2 header band. Check the band spans the full box width and the chip stays right-pinned.

- [ ] **Step 6: Commit**

```bash
git add internal/tui
git commit -m "feat: shared chrome — header bar, keybar, key chips, cursor rows, banded renderBox"
```

---

### Task 5: Floor / tower flow

**Files:**
- Modify: `internal/tui/render.go:30-95` (`renderHeader`, `renderNoticeRow`), `render.go:275-470` (tower/rows/shelf/legend)
- Modify: `internal/tui/app.go:1108-1200` (`View()` — chrome header, keybar, error docking, remove legend line)
- Test: goldens + existing `render_test.go`

**Interfaces:**
- Consumes: `renderChromeHeader`, `renderKeybar`, `keyChip`, glyph constants, `activeTheme.Bg*`.
- Produces: no new API — `View()` output shape changes (header row, footer row, no trailing `error:` line, no legend).

- [ ] **Step 1: Rewrite `renderHeader` to use `renderChromeHeader`**

Map `Overview` to badges exactly:

```go
func renderHeader(ov *proto.Overview, width int) string {
	if ov == nil {
		ov = &proto.Overview{}
	}
	var badges []badge
	if ov.NeedYou > 0 {
		badges = append(badges, badge{Text: fmt.Sprintf("%d question%s for you", ov.NeedYou, pluralSuffix(ov.NeedYou)), Kind: badgeWarn})
	}
	if ov.Failing > 0 {
		badges = append(badges, badge{Text: fmt.Sprintf("%d build%s failing", ov.Failing, pluralSuffix(ov.Failing)), Kind: badgeErr})
	}
	if len(badges) == 0 {
		badges = append(badges, badge{Text: "all clear", Kind: badgeOk})
	}
	var details []string
	if ov.Building > 0 {
		details = append(details, fmt.Sprintf("%d building", ov.Building))
	}
	if ov.ShippedToday > 0 {
		details = append(details, fmt.Sprintf("%d shipped today", ov.ShippedToday))
	}
	if ov.TokensTotal > 0 {
		details = append(details, fmt.Sprintf("%s tokens", compactTokens(ov.TokensTotal)))
	}
	if ov.DollarsTotal > 0 {
		details = append(details, fmt.Sprintf("~$%.2f", ov.DollarsTotal))
	}
	return renderChromeHeader(width, badges, strings.Join(details, " · "))
}
```

- [ ] **Step 2: Restyle the tower**

In `renderTowerConfigured`/`headerCell`/`cellContentForStage` (`render.go:179-345`), read the current implementation first, then apply:
- Lane header cells: identity tag rendered as an inverted chip (`Foreground(t.Bg0).Background(identity color)`, bold, padded 0,1) + title in `Bright`, dim sub-line (flow · attempt). Focused lane: whole header cell on `Bg2` with a `Structure`-free accent top rule — simplest faithful approximation in rows of text: an `Accent`-colored `▔▔▔…` line above the focused cell (or `Background(t.Bg2)` + accent-bold title if the extra row breaks height math; decide by looking at the PNG).
- Stage label column: `Dim` uppercase (already uppercase stage names — keep) at fixed width.
- Cells: glyph + state word + `Dim` meta. State mapping (this replaces any ad-hoc glyphs currently in `cellContentForStage`):
  - completed → `glyphDone` in `Ok`
  - current + need-you decision → `glyphNeedYou` + "need-you" in `Warn`
  - current + running → `glyphWorking` + "working" in `Structure` + dim meta (tokens via `compactTokens`)
  - current + failed → `glyphFailed` + "failed" in `Err` + dim `R retry`
  - not reached → `glyphWaiting` in `Dimmer`
- Shelf (`renderShelf`, `render.go:384-422`): section labels ("SHIPPED today", "PARKED") in `Dim` letter-spaced style, items keep identity chips, `glyphShipped` in `Ok` / `glyphParked` in `Dim`.

- [ ] **Step 3: Update `View()` in `app.go`**

- Replace the hardcoded footer string at `app.go:1188` with:

```go
b.WriteString("\n")
b.WriteString(renderKeybar(layoutWidth, [][2]string{
	{"j/k", "floors"}, {"tab", "attention"}, {"p", "pause"}, {"x", "kill"},
	{"R", "retry"}, {"L", "levers"}, {"?", "help"}, {"q", "quit"},
}, m.Err))
```

and delete the `if m.Err != ""` block (the error now docks in the keybar's right slot). Do the same for the door-mode footer at `app.go:1137` with bindings `{{"j/k", "select"}, {"enter", "open"}, {"esc", "back"}, {"q", "quit"}}`.
- Find and remove the legend line — grep `working · .* done` in `render.go` (it's in `renderNoticeRow` or near the tower) and delete its emission.

- [ ] **Step 4: Fix unit tests, regenerate goldens, LOOK**

Run: `go test ./internal/tui` — update `render_test.go` expectations that assert old header/footer text (keep semantic assertions: counts, titles; drop exact-styling asserts where they fight the redesign).
Run: `go test ./internal/tui -run TestSnapshots -update && scripts/snap.sh`
**Read `tmp-snaps/floor.png`, `tmp-snaps/rows.png` at both widths** (`go run ./cmd/watchtower snap --width 100` for narrow). Check against the artifact: chrome bars span full width, glyph column aligns, focused lane reads, no legend, error docked right.

- [ ] **Step 5: Full suite + commit**

Run: `go test ./...`

```bash
git add internal/tui
git commit -m "feat: floor/tower on shared chrome — header badges, glyph cells, keybar footer"
```

---

### Task 6: Decision card + decisions door

**Files:**
- Modify: `internal/tui/rail.go:183-273` (`renderToast`)
- Modify: `internal/tui/doors.go:15-31` (`renderDecisionsDoor`)
- Test: goldens + `rail_test.go`

**Interfaces:**
- Consumes: `renderBox` (banded), `keyChip`, `cursorRow`, glyphs.
- Produces: no API change; `renderToast(d, id, sel, streak, width)` keeps its signature.

- [ ] **Step 1: Read `renderToast` fully (`rail.go:183`), then restyle**

Target anatomy (from the artifact/spec):
- Box via `renderBox("DECISION "+strconv.FormatInt(d.ID, 10), identityTag+" "+d.Stage+" — "+shortQuestion, " esc dismiss ", body)` — but the reversibility verdict must sit in the band: pass it via the `sub` argument, styled `Ok` if `d.Reversible` contains "cheap"/"easy", else `Warn`. Render as `"↺ "+d.Reversible` truncated to keep the band one line.
- Body: question in `Text` wrapped to width; `why · …` line in `Dim`; blank line; then options.
- Each option is a bordered row:

```go
func renderOption(text string, selected, recommended bool, width int) string {
	t := activeTheme
	radio := glyphWaiting
	border := t.Dimmer
	head := lipgloss.NewStyle().Foreground(t.Text)
	if selected {
		radio = "◉"
		border = t.Accent
		head = lipgloss.NewStyle().Foreground(t.Bright)
	}
	star := ""
	if recommended {
		star = " " + lipgloss.NewStyle().Foreground(t.Warn).Render("★")
	}
	radioStyled := lipgloss.NewStyle().Foreground(border).Render(radio)
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Padding(0, 1).Width(max(10, width-4)).
		Render(radioStyled + " " + head.Render(text) + star)
}
```

Options split their existing "headline → consequence" text if `d.Consequences` has entries (headline = option, consequence in `Dim` on a second line inside the border); otherwise render the option string alone.
- Card footer INSIDE the box (last body line): `keyChip("j/k")+" choose  "+keyChip("enter")+" select  "+keyChip("y")+" accept ★  "+keyChip("o")+" evidence  "+keyChip("1..9")+" by number"` with labels in `Dim`. Preserve the existing accept-streak affordance (`streak` param) wherever it renders today — restyle in `Ok`, don't drop it.

- [ ] **Step 2: Restyle `renderDecisionsDoor`**

```go
func renderDecisionsDoor(ds []projection.DecisionView, ids map[string]Identity, sel, width int) string {
	t := activeTheme
	title := lipgloss.NewStyle().Foreground(t.Bright).Bold(true).Render("Decisions")
	subtitle := lipgloss.NewStyle().Foreground(t.Dim).Render(fmt.Sprintf("  worst first · %d open", len(ds)))
	lines := []string{title + subtitle, ""}
	if len(ds) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(t.Dimmer).Render("  —"))
		return boundedLines(lines, width)
	}
	sel = min(max(sel, 0), len(ds)-1)
	for i, d := range ds {
		num := lipgloss.NewStyle().Foreground(t.Warn).Render(fmt.Sprintf("[%d]", d.ID))
		id := lipglossIdentity(ids[d.IssueID], ids[d.IssueID].Tag)
		row := fmt.Sprintf("%s %s %s · %s", num, id, d.Stage, d.Question)
		lines = append(lines, cursorRow(i == sel, truncate(row, max(1, width-4)), width))
	}
	return boundedLines(lines, width)
}
```

(The old version painted the whole row in identity color — identity color now stays on the tag only, per the color contract.)

- [ ] **Step 3: Tests, goldens, LOOK**

Run: `go test ./internal/tui` (fix `rail_test.go` expectations), `-update`, `scripts/snap.sh`.
**Read `tmp-snaps/decision.png` and `tmp-snaps/decisions-door.png`** — verify: band shows reversibility on the right, ★ option pre-selected with accent border, radios aligned, card keybar chips match the global footer's.

- [ ] **Step 4: Commit**

```bash
git add internal/tui
git commit -m "feat: decision card with option rows and banded chrome; decisions door on cursor rows"
```

---

### Task 7: Triage tray + text doors

**Files:**
- Modify: `internal/tui/doors.go:44-100` (`renderProposalsDoor`, `renderTextDoor`)
- Test: goldens + `doors_test.go`

**Interfaces:**
- Consumes: `cursorRow`, theme tokens. No API changes.

- [ ] **Step 1: Restyle `renderProposalsDoor`**

Same title/subtitle pattern as the decisions door ("Tray", "N proposals"). Each proposal: `cursorRow(i==sel, title, width)` with title in `Bright` when selected / `Text` otherwise, then its wrapped body lines each rendered `"    "+Dim(line)` (reuse the existing `wrapProposal`, width−6). One blank line between proposals (`padV`).

- [ ] **Step 2: Restyle `renderTextDoor` (timeline/transcript)**

Same header treatment (`"Timeline"`/`"Transcript"` in `Bright`, count subtitle in `Dim`); body lines unchanged in `Text`; no other decoration — these are reading surfaces.

- [ ] **Step 3: Tests, goldens, LOOK**

Run: `go test ./internal/tui`, `-update`, `scripts/snap.sh`.
**Read `tmp-snaps/tray.png`** — cursor row grammar identical to the decisions door.

- [ ] **Step 4: Commit**

```bash
git add internal/tui
git commit -m "feat: tray and text doors on shared list grammar"
```

---

### Task 8: New-issue, confirm, and lever overlays

**Files:**
- Modify: `internal/tui/modal.go:86-135` (`renderModal`, `modalField`, `renderConfirm`) and the lever editor render (`renderLeverEditor`, same file below `newLeverEditor`)
- Test: goldens + `modal_test.go`

**Interfaces:**
- Consumes: banded `renderBox`, `keyChip`, `cursorRow`, theme tokens. Signatures unchanged.

- [ ] **Step 1: Restyle `modalField` into bordered inputs**

```go
func modalField(selected bool, name, value string, required bool) string {
	t := activeTheme
	label := strings.ToUpper(name)
	if required {
		label += " *"
	}
	labelLine := lipgloss.NewStyle().Foreground(t.Dim).Render(label)
	border := t.Dimmer
	body := lipgloss.NewStyle().Foreground(t.Text).Render(value)
	if value == "" {
		body = lipgloss.NewStyle().Foreground(t.Dimmer).Render("…")
	}
	if selected {
		border = t.Accent
		body = lipgloss.NewStyle().Foreground(t.Bright).Render(value) +
			lipgloss.NewStyle().Foreground(t.Accent).Render("▏")
	}
	field := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Padding(0, 1).Width(44).Render(body)
	return labelLine + "\n" + field
}
```

`renderModal` keeps its structure but the footer line becomes `keyChip("tab")+dim(" next field  ")+keyChip("enter")+dim(" create")`.

- [ ] **Step 2: Restyle the lever editor**

Find `renderLeverEditor` (grep `func renderLeverEditor` in `modal.go`). Each row: stage name left, value right in `Structure`; selected row via `cursorRow` with the value wrapped `◂ value ▸`, arrows in `Accent`:

```go
value := lipgloss.NewStyle().Foreground(t.Structure).Render(matrix[stage])
if i == sel {
	arrow := lipgloss.NewStyle().Foreground(t.Accent)
	value = arrow.Render("◂ ") + lipgloss.NewStyle().Foreground(t.Bright).Render(matrix[stage]) + arrow.Render(" ▸")
}
row := padCell(stageName, 14) + value
lines = append(lines, cursorRow(i == sel, row, boxInnerWidth))
```

Footer: `keyChip("j/k")+" lever  "+keyChip("h/l")+" adjust  "+keyChip("enter")+" apply"` in the same dim-label pattern. `renderConfirm` just inherits the banded box — restyle its hint line with `keyChip("y")`.

- [ ] **Step 3: Tests, goldens, LOOK**

Run: `go test ./internal/tui`, `-update`, `scripts/snap.sh`.
**Read `tmp-snaps/modal.png` and `tmp-snaps/levers.png`** — active field border + caret visible; `◂ ▸` only on the selected lever.

- [ ] **Step 4: Commit**

```bash
git add internal/tui
git commit -m "feat: overlays — bordered inputs with caret, lever adjust affordance"
```

---

### Task 9: Architecture map

**Files:**
- Modify: `internal/tui/arch.go:25-105` (`renderArchWithState`)
- Test: goldens + `arch_test.go`

**Interfaces:**
- Consumes: `cursorRow`, glyphs, theme tokens. Signature unchanged.

- [ ] **Step 1: Restyle `renderArchWithState`**

- Title line: `"Architecture map"` in `Bright` bold + `Dim` subtitle carrying the existing `mapInsight` text.
- Active module rows via `cursorRow`: `▣` in `Structure`, module name in `Text` (`Bright` when selected) padded to a fixed 28-cell column, then the existing identity marks (unchanged — they already use identity colors).
- Quiet rows: keep the existing collapse but restyle as one row: `▢` in `Dimmer` + `fmt.Sprintf("%d quiet areas", quiet)` in `Dim` + a dim sample of up to 3 names.
- Move the trailing `selection: …` caption into the door footer instead: `renderArchWithState` returns the body only; the selected-module detail string moves to the keybar right-slot. Concretely: give the function a companion `archFooterDetail(am, st, selected, filter) string` and in `View()`'s `archMode == "full"` branch render `renderKeybar(layoutWidth, [][2]string{{"j/k","module"},{"/","filter"},{"a/esc","back"}}, "")` — with the detail placed as the `right` argument in `Dim` (not `Err`; add a small variant: if the right text should not be an error, pass it pre-styled and have `renderKeybar` accept it verbatim when it does not come from `m.Err`. Simplest: change `renderKeybar`'s right param to accept an already-styled string and style `m.Err` at the call sites).

Note on that last point — to keep one signature, define in `chrome.go`:

```go
func errText(s string) string {
	if s == "" {
		return ""
	}
	return lipgloss.NewStyle().Foreground(activeTheme.Err).Render(glyphFailed + " " + s)
}
```

and make `renderKeybar` render `right` verbatim; update Task 5's call sites to pass `errText(m.Err)`. (If Task 5 already shipped the styled-inside version, refactor here.)

- [ ] **Step 2: Tests, goldens, LOOK**

Run: `go test ./internal/tui`, `-update`, `scripts/snap.sh`.
**Read `tmp-snaps/arch.png`** — module column aligned, quiet row collapsed, detail in the footer.

- [ ] **Step 3: Commit**

```bash
git add internal/tui
git commit -m "feat: arch map on shared rows; selection detail docks in keybar"
```

---

### Task 10: Evidence / diff pager

**Files:**
- Modify: `internal/tui/pager.go` (`renderPager`) and `internal/tui/rail.go:274+` (`renderEvidenceDetails`/`renderEvidenceFallback` styling only)
- Test: goldens + `pager_test.go`

**Interfaces:**
- Consumes: theme tokens only. Reading mode: NO accent anywhere except the position indicator.

- [ ] **Step 1: Restyle `renderPager`**

Read `pager.go` first. Apply per-line classification before printing:

```go
func pagerLine(line string) string {
	t := activeTheme
	switch {
	case strings.HasPrefix(line, "+"):
		return lipgloss.NewStyle().Foreground(t.Ok).Render(line)
	case strings.HasPrefix(line, "-"):
		return lipgloss.NewStyle().Foreground(t.Err).Render(line)
	case strings.HasPrefix(line, "@@"):
		return lipgloss.NewStyle().Foreground(t.Structure).Render(line)
	case strings.HasPrefix(line, "diff ") || strings.HasSuffix(line, ".md") || strings.HasSuffix(line, ".go"):
		return lipgloss.NewStyle().Foreground(t.Bright).Bold(true).Render(line)
	default:
		return lipgloss.NewStyle().Foreground(t.Dim).Render(line)
	}
}
```

(Exact filename heuristic: reuse whatever the pager already uses to detect headers if it has one; the suffix check is the fallback.) Title in `Bright`; the percent-position indicator is the ONLY `Accent` text on the surface. Footer via `renderKeybar`: `{{"j/k","scroll"},{"space","page"},{"esc","back"}}` with position as pre-styled right text.

- [ ] **Step 2: Evidence surfaces**

`renderEvidenceDetails`/`renderEvidenceFallback`: headings in `Bright`, paths in `Structure`, body in `Text`/`Dim`. No accent.

- [ ] **Step 3: Tests, goldens, LOOK**

Run: `go test ./internal/tui`, `-update`, `scripts/snap.sh`.
**Read `tmp-snaps/pager.png`** — additions/deletions/hunks colored; nothing violet except the position.

- [ ] **Step 4: Commit**

```bash
git add internal/tui
git commit -m "feat: pager as accent-free reading mode with diff semantics"
```

---

### Task 11: Help overlay, legend retirement audit, and live acceptance

**Files:**
- Modify: `internal/tui/render.go:424-500` (`renderHelpOverlay`)
- Test: goldens; full suite; tmux captures

**Interfaces:**
- Consumes: banded `renderBox`, `keyChip`. Existing two-column collapse behavior preserved.

- [ ] **Step 1: Restyle `renderHelpOverlay`**

Group names (`NAVIGATION`, `CONTROL`, `DOORS`, `DECISIONS`): `Structure` bold (currently `Heading` — same slot, confirm). Keys: `Accent` bold at width 8 (unchanged). The footer rule/close line: replace with the standard chip pattern `keyChip("? / esc")+dim(" close  ")+keyChip("q")+dim(" quit")`. Box already comes from `renderBox` — it inherits the band from Task 4. If the artifact's glyph legend has no home yet, add a fifth help group `STATES` listing the six glyphs with their meanings — help is the one legitimate legend location.

- [ ] **Step 2: Legend + contract audit**

Run and fix every hit that violates the system:

```bash
grep -rn "working · \|✓ done\|× FAILED" internal/tui/   # legend remnants → delete
grep -rn '"●"\|"○"\|"◐"\|"◔"\|"✕"\|"▸"' internal/tui/*.go | grep -v chrome.go  # inline glyphs → replace with constants
grep -rn '#[0-9a-fA-F]\{6\}' internal/tui/*.go | grep -v theme.go | grep -v fixtures.go  # stray hexes → theme tokens
```

- [ ] **Step 3: Full suite + final goldens**

Run: `go test ./...` then `go test ./internal/tui -run TestSnapshots -update && go test ./...`
Expected: all PASS.

- [ ] **Step 4: Live acceptance via tmux**

Run: `for f in floor decision tray modal levers arch help; do scripts/tui-capture.sh $f; done`
**Read every `tmp-snaps/live-*.png`** and compare against the artifact and the fixture PNGs. Differences between live and fixture snapshots that are data-driven (fewer issues) are fine; structural differences (missing chrome, misaligned columns) are bugs — fix before proceeding.

- [ ] **Step 5: Commit**

```bash
git add internal/tui
git commit -m "feat: help overlay on shared chrome; glyph/hex audit; retire legend"
```

---

## Verification (whole plan)

1. `go test ./...` green.
2. `scripts/snap.sh` → Read all PNGs against the artifact (https://claude.ai/code/artifact/57ffbac8-8a80-4d24-970a-247a5420aa40).
3. `scripts/tui-capture.sh floor` (+ decision, help) → real binary matches fixtures structurally.
4. Contract audit greps from Task 11 Step 2 return no hits.
5. `watchtower tower` run by the user in their real workspace for final sign-off.
