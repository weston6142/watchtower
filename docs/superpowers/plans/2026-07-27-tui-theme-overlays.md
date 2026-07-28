# TUI Theme + Modal Overlays Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Theme tokens (Tokyo Night default, configurable via `.guildhall/config.yaml`) plus herdr-style bordered overlays for help, modals, confirm, and lever editor, composited over the dimmed main view.

**Architecture:** A six-token `Theme` struct with named presets lives in `internal/tui/theme.go` as a package-level `activeTheme`. A small ANSI-aware compositor (`internal/tui/overlay.go`) splices a bordered modal into the centre of the fully rendered, dimmed base view. `renderHelp` is rewritten as a grouped two-column key table; `renderModal`/`renderConfirm`/lever editor get the same box treatment and are composited instead of replacing the tower. The toast keeps its stacked position but adopts theme tokens.

**Tech Stack:** Go, bubbletea, lipgloss, `github.com/charmbracelet/x/ansi` (already a direct dependency).

**Spec:** `docs/superpowers/specs/2026-07-27-tui-theme-overlays-design.md`

## Global Constraints

- Default theme name is exactly `tokyo-night`; presets: `tokyo-night`, `terminal`, `catppuccin`, `gruvbox`. Unknown names silently fall back to `tokyo-night`.
- Never set background colors on text runs (terminal background shows through). The `Panel` token is used only as the foreground of the inverted `esc close`/`esc cancel` chip.
- Status colors `statusOk/Warn/Bad` in `internal/tui/render.go` stay unchanged.
- Key bindings and modal key routing are unchanged — this is presentation only.
- All tests: `go test ./...` must pass at every commit; run `go vet ./internal/tui/` before each commit.

---

### Task 1: Theme tokens and presets

**Files:**
- Create: `internal/tui/theme.go`
- Test: `internal/tui/theme_test.go`

**Interfaces:**
- Produces: `type Theme struct { Accent, Heading, Text, Dim, Bright, Panel lipgloss.Color }`; `func themeByName(name string) Theme`; package var `activeTheme Theme` (initialised to tokyo-night); `func SetTheme(name string)` (sets `activeTheme`). Later tasks read `activeTheme` directly.

- [ ] **Step 1: Write the failing test**

```go
// internal/tui/theme_test.go
package tui

import "testing"

func TestThemeByName(t *testing.T) {
	if got := themeByName("tokyo-night").Accent; got != "#bb9af7" {
		t.Fatalf("tokyo-night accent = %q", got)
	}
	if got := themeByName("gruvbox").Heading; got != "#fabd2f" {
		t.Fatalf("gruvbox heading = %q", got)
	}
	if got := themeByName("terminal").Accent; got != "13" {
		t.Fatalf("terminal accent = %q", got)
	}
	// unknown name falls back to tokyo-night
	if got := themeByName("does-not-exist"); got != themeByName("tokyo-night") {
		t.Fatalf("fallback = %+v", got)
	}
}

func TestSetTheme(t *testing.T) {
	defer SetTheme("tokyo-night")
	SetTheme("catppuccin")
	if activeTheme.Accent != "#cba6f7" {
		t.Fatalf("activeTheme.Accent = %q", activeTheme.Accent)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run 'TestThemeByName|TestSetTheme' -v`
Expected: FAIL — `undefined: themeByName`

- [ ] **Step 3: Write the implementation**

```go
// internal/tui/theme.go
package tui

import "github.com/charmbracelet/lipgloss"

// Theme is the six-token palette driving overlays and the toast. Text runs
// never set a background color; Panel is only the foreground of the inverted
// close chip so it reads against the Accent background.
type Theme struct {
	Accent  lipgloss.Color // keys, modal border, close chip background
	Heading lipgloss.Color // section labels, footer key-labels
	Text    lipgloss.Color // descriptions, body text
	Dim     lipgloss.Color // subtitles, secondary text
	Bright  lipgloss.Color // modal title
	Panel   lipgloss.Color // chip text on accent background
}

var themes = map[string]Theme{
	"tokyo-night": {Accent: "#bb9af7", Heading: "#7aa2f7", Text: "#c0caf5", Dim: "#565f89", Bright: "#e4ecff", Panel: "#1f2335"},
	"terminal":    {Accent: "13", Heading: "12", Text: "7", Dim: "8", Bright: "15", Panel: "0"},
	"catppuccin":  {Accent: "#cba6f7", Heading: "#89b4fa", Text: "#cdd6f4", Dim: "#6c7086", Bright: "#f0f4ff", Panel: "#181825"},
	"gruvbox":     {Accent: "#d3869b", Heading: "#fabd2f", Text: "#ebdbb2", Dim: "#928374", Bright: "#fbf1c7", Panel: "#32302f"},
}

func themeByName(name string) Theme {
	if t, ok := themes[name]; ok {
		return t
	}
	return themes["tokyo-night"]
}

var activeTheme = themeByName("tokyo-night")

// SetTheme selects the active preset by name; unknown names keep the
// tokyo-night default.
func SetTheme(name string) { activeTheme = themeByName(name) }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tui/ -run 'TestThemeByName|TestSetTheme' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tui/theme.go internal/tui/theme_test.go
git commit -m "feat: theme tokens with tokyo-night default and named presets"
```

---

### Task 2: Config key and CLI wiring

**Files:**
- Modify: `internal/repocfg/repocfg.go` (Config struct, ~line 15)
- Modify: `cmd/guildhall/main.go` (tower case, ~line 91-108)
- Test: `internal/repocfg/repocfg_test.go`

**Interfaces:**
- Consumes: `tui.SetTheme(name string)` from Task 1; existing `resolveRepo(repoFlag string) string` in `cmd/guildhall/spawn.go`.
- Produces: `repocfg.Config.Theme string` (yaml key `theme`, empty means default).

- [ ] **Step 1: Write the failing test** (follow the existing test style in `internal/repocfg/repocfg_test.go`)

```go
func TestLoadTheme(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".guildhall"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".guildhall", "config.yaml"), []byte("theme: gruvbox\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Theme != "gruvbox" {
		t.Fatalf("Theme = %q", cfg.Theme)
	}
	// missing file: Theme stays empty (tui applies its own default)
	cfg, err = Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Theme != "" {
		t.Fatalf("default Theme = %q, want empty", cfg.Theme)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/repocfg/ -run TestLoadTheme -v`
Expected: FAIL — `cfg.Theme undefined`

- [ ] **Step 3: Implement**

Add to the `Config` struct in `internal/repocfg/repocfg.go`:

```go
	Theme        string  `yaml:"theme"`
```

No `fillGaps` entry — empty string means "use the tui default", so the tui layer owns the fallback.

In `cmd/guildhall/main.go`, in the `case "tower":` block after `model.SetRetireAfter(*retireAfter)` (~line 105):

```go
		if cfg, err := repocfg.Load(resolveRepo(*repo)); err == nil {
			tui.SetTheme(cfg.Theme) // empty or unknown falls back to tokyo-night
		}
```

Add `"github.com/wbushyeager/guildhall/internal/repocfg"` to main.go imports if not present (it is already imported for the daemon path — check first).

Note: `SetTheme("")` must fall back to tokyo-night — Task 1's `themeByName` already handles any unknown key, including empty.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/repocfg/ ./internal/tui/ -v -run 'TestLoadTheme|TestSetTheme'` then `go build ./...`
Expected: PASS, build clean

- [ ] **Step 5: Commit**

```bash
git add internal/repocfg/repocfg.go internal/repocfg/repocfg_test.go cmd/guildhall/main.go
git commit -m "feat: theme config key wired from .guildhall/config.yaml to the TUI"
```

---

### Task 3: Overlay compositor

**Files:**
- Create: `internal/tui/overlay.go`
- Test: `internal/tui/overlay_test.go`

**Interfaces:**
- Produces: `func overlayCenter(base, modal string, width, height int) string` — dims the base, splices the modal centered. Later tasks call it from `View()`.
- Consumes: `themeDim` (existing, render.go:15).

- [ ] **Step 1: Write the failing test**

```go
// internal/tui/overlay_test.go
package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestOverlayCenterSplicesModal(t *testing.T) {
	base := strings.TrimSuffix(strings.Repeat("aaaaaaaaaa\n", 5), "\n") // 10x5
	out := overlayCenter(base, "XX\nXX", 10, 5)
	lines := strings.Split(out, "\n")
	if len(lines) != 5 {
		t.Fatalf("height = %d", len(lines))
	}
	// modal is 2x2 centered in 10x5 → x=4, y=1
	for _, row := range []int{1, 2} {
		plain := ansi.Strip(lines[row])
		if plain != "aaaaXXaaaa" {
			t.Fatalf("row %d = %q", row, plain)
		}
	}
	if ansi.Strip(lines[0]) != "aaaaaaaaaa" {
		t.Fatalf("row 0 = %q", ansi.Strip(lines[0]))
	}
	// base rows outside the modal are dimmed (faint SGR present)
	if !strings.Contains(lines[0], "\x1b[2m") {
		t.Fatalf("row 0 not dimmed: %q", lines[0])
	}
}

func TestOverlayCenterShortBase(t *testing.T) {
	// base shorter than height gets padded before splicing
	out := overlayCenter("aaaa", "XX", 10, 5)
	if got := len(strings.Split(out, "\n")); got != 5 {
		t.Fatalf("height = %d", got)
	}
}

func TestOverlayCenterModalTooBig(t *testing.T) {
	// modal wider than the screen degrades to a plain centered render
	out := overlayCenter("aa", strings.Repeat("X", 30), 10, 3)
	if !strings.Contains(ansi.Strip(out), "XXXX") {
		t.Fatalf("modal content lost: %q", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run TestOverlayCenter -v`
Expected: FAIL — `undefined: overlayCenter`

- [ ] **Step 3: Implement**

```go
// internal/tui/overlay.go
package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// overlayCenter composites modal over base, centered in width x height. The
// base is flattened to faint (existing colors are stripped) so the modal
// reads as the only saturated element — the herdr overlay look.
func overlayCenter(base, modal string, width, height int) string {
	modalLines := strings.Split(modal, "\n")
	mw := lipgloss.Width(modal)
	mh := len(modalLines)
	if mw >= width || mh >= height {
		return lipgloss.Place(max(1, width), max(1, height), lipgloss.Center, lipgloss.Center, modal)
	}
	baseLines := strings.Split(base, "\n")
	if len(baseLines) > height {
		baseLines = baseLines[:height]
	}
	for len(baseLines) < height {
		baseLines = append(baseLines, "")
	}
	for i := range baseLines {
		baseLines[i] = themeDim.Render(ansi.Strip(baseLines[i]))
	}
	x := (width - mw) / 2
	y := (height - mh) / 2
	for i, ml := range modalLines {
		row := y + i
		bl := baseLines[row]
		left := ansi.Truncate(bl, x, "")
		if pad := x - lipgloss.Width(left); pad > 0 {
			left += strings.Repeat(" ", pad)
		}
		// pad short modal lines so the right slice of the base lines up
		if pad := mw - lipgloss.Width(ml); pad > 0 {
			ml += strings.Repeat(" ", pad)
		}
		baseLines[row] = left + ml + ansi.TruncateLeft(bl, x+mw, "")
	}
	return strings.Join(baseLines, "\n")
}
```

Note: `ansi.TruncateLeft(s, n, "")` drops the first n columns, keeping the tail — verify against the vendored `x/ansi` v0.10.1 signature before use; if the signature differs, cut the tail via `ansi.Cut(bl, x+mw, lipgloss.Width(bl))` instead.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tui/ -run TestOverlayCenter -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tui/overlay.go internal/tui/overlay_test.go
git commit -m "feat: ANSI-aware centered overlay compositor with dimmed base"
```

---

### Task 4: Help overlay

**Files:**
- Modify: `internal/tui/render.go` (replace `renderHelp`, lines 424-448)
- Modify: `internal/tui/app.go` (help branch in `View`, lines 1140-1146, and end of `View`)
- Test: `internal/tui/render_test.go` (existing help test at ~line 147)

**Interfaces:**
- Consumes: `activeTheme` (Task 1), `overlayCenter` (Task 3).
- Produces: `func renderHelpOverlay(width int) string` — the bordered box only (no compositing; `View` composites).

- [ ] **Step 1: Update the help test to the new shape (failing first)**

Replace the existing `renderHelp` test in `internal/tui/render_test.go` (~line 147):

```go
func TestRenderHelpOverlay(t *testing.T) {
	out := ansi.Strip(renderHelpOverlay(100))
	for _, want := range []string{
		"help", "esc close",
		"NAVIGATION", "CONTROL", "DOORS", "DECISIONS",
		"war room", "lever editor", "architecture pane / map",
		"close ? / esc", "quit q / ctrl+c",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "SYSTEM") {
		t.Fatal("SYSTEM section should be gone")
	}
	// bordered box
	if !strings.Contains(out, "┌") || !strings.Contains(out, "└") {
		t.Fatal("expected box border")
	}
}
```

(Add `"github.com/charmbracelet/x/ansi"` to the test file imports if missing.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tui/ -run TestRenderHelpOverlay -v`
Expected: FAIL — `undefined: renderHelpOverlay`

- [ ] **Step 3: Implement renderHelpOverlay**

Replace `renderHelp` in `internal/tui/render.go` (delete lines 424-448) with:

```go
type helpGroup struct {
	name string
	rows [][2]string // key, description
}

var helpGroups = [][]helpGroup{
	{ // left column
		{"NAVIGATION", [][2]string{
			{"j / k", "floors"},
			{"h / l", "cards"},
			{"1..9", "focus issue"},
			{"tab", "attention / next field"},
			{"g", "war room"},
			{"enter", "open artifacts"},
			{"esc", "back"},
			{"z", "rows / tower layout"},
		}},
		{"CONTROL", [][2]string{
			{"p", "pause / resume"},
			{"x", "kill stage"},
			{"R", "retry failed stage"},
			{"L", "lever editor"},
			{"n", "new issue"},
			{"c", "retire shipped lane"},
			{"u", "shipped shelf"},
		}},
	},
	{ // right column
		{"DOORS", [][2]string{
			{"d", "decisions"},
			{"t", "triage"},
			{"e", "timeline"},
			{"T", "transcript"},
			{"r", "reject tray item"},
			{"a / A", "architecture pane / map"},
		}},
		{"DECISIONS", [][2]string{
			{"y", "accept recommendation"},
			{"n", "choose option"},
			{"o", "show evidence"},
			{"1..9", "choose option by number"},
		}},
	},
}

func renderHelpOverlay(width int) string {
	t := activeTheme
	keyStyle := lipgloss.NewStyle().Foreground(t.Accent).Bold(true).Width(8)
	descStyle := lipgloss.NewStyle().Foreground(t.Text)
	headStyle := lipgloss.NewStyle().Foreground(t.Heading).Bold(true)

	var cols []string
	for _, col := range helpGroups {
		var blocks []string
		for _, g := range col {
			lines := []string{headStyle.Render(g.name)}
			for _, row := range g.rows {
				lines = append(lines, keyStyle.Render(row[0])+descStyle.Render(row[1]))
			}
			blocks = append(blocks, strings.Join(lines, "\n"))
		}
		cols = append(cols, strings.Join(blocks, "\n\n"))
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, cols[0], "    ", cols[1])
	inner := lipgloss.Width(body)

	title := lipgloss.NewStyle().Foreground(t.Bright).Bold(true).Render("help")
	sub := lipgloss.NewStyle().Foreground(t.Dim).Render(" every key in the control room")
	chip := lipgloss.NewStyle().Foreground(t.Panel).Background(t.Accent).Bold(true).Render(" esc close ")
	gap := max(1, inner-lipgloss.Width(title)-lipgloss.Width(sub)-lipgloss.Width(chip))
	header := title + sub + strings.Repeat(" ", gap) + chip

	foot := lipgloss.NewStyle().Foreground(t.Heading).Render("close ") + descStyle.Render("? / esc") +
		lipgloss.NewStyle().Foreground(t.Dim).Render("  ·  ") +
		lipgloss.NewStyle().Foreground(t.Heading).Render("quit ") + descStyle.Render("q / ctrl+c")
	rule := lipgloss.NewStyle().Foreground(t.Dim).Render(strings.Repeat("─", inner))

	content := strings.Join([]string{header, "", body, rule, foot}, "\n")
	box := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(t.Accent).
		Padding(0, 1).
		Render(content)
	if lipgloss.Width(box) >= width {
		// narrow terminal: stack the two columns
		body = cols[0] + "\n\n" + cols[1]
		content = strings.Join([]string{header, "", body, foot}, "\n")
		box = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(t.Accent).Padding(0, 1).Render(content)
	}
	return box
}
```

- [ ] **Step 4: Wire compositing in View**

In `internal/tui/app.go`, delete the early-return help branch (lines 1140-1146):

```go
	if m.help {
		var b strings.Builder
		m.writeHeaderRows(&b, layoutWidth)
		b.WriteString(renderHelp(layoutWidth))
		b.WriteString("\n\n? close help · q quit")
		return b.String()
	}
```

Then at the very end of `View`, where the final string is returned (after the shelf append, ~line 1189), capture the screen and composite:

```go
	screen := b.String()
	if m.help {
		return overlayCenter(screen, renderHelpOverlay(layoutWidth), layoutWidth, max(m.Height, lipgloss.Height(screen)))
	}
	return screen
```

Also update the door-mode condition at line 1131 from `m.currentMode() != "" && !m.help` — the door early-return path must also composite when help is open. Change that branch's `return b.String()` to route through the same overlay:

```go
	if m.currentMode() != "" {
		var b strings.Builder
		m.writeHeaderRows(&b, layoutWidth)
		b.WriteString(tower)
		b.WriteString("\n\n")
		b.WriteString("j/k select · enter open · esc back · q quit")
		screen := b.String()
		if m.help {
			return overlayCenter(screen, renderHelpOverlay(layoutWidth), layoutWidth, max(m.Height, lipgloss.Height(screen)))
		}
		return screen
	}
```

- [ ] **Step 5: Run the package tests, fix fallout**

Run: `go test ./internal/tui/ -v 2>&1 | tail -20`
Expected: PASS. Any existing `View`/help snapshot-style tests that asserted the old inline help text need updating to assert via `ansi.Strip` on the composited output (help content is still present, now boxed and dimmed-background).

- [ ] **Step 6: Commit**

```bash
git add internal/tui/render.go internal/tui/render_test.go internal/tui/app.go internal/tui/app_test.go
git commit -m "feat: herdr-style help overlay composited over the dimmed control room"
```

---

### Task 5: Modal, confirm, and lever editor treatment

**Files:**
- Modify: `internal/tui/modal.go` (`renderModal` ~line 67, `renderConfirm` ~line 101, lever editor renderer)
- Modify: `internal/tui/app.go` (`View` lines 1147-1152)
- Test: `internal/tui/modal_test.go` (or wherever renderModal is tested — check `grep -rn renderModal internal/tui/*_test.go`)

**Interfaces:**
- Consumes: `activeTheme`, `overlayCenter`, `renderBox` (produced below).
- Produces: `func renderBox(title, sub, chipText, content string) string` — shared themed box used by help (optional refactor), modal, confirm, lever editor.

- [ ] **Step 1: Write the failing test**

```go
func TestRenderBoxChrome(t *testing.T) {
	out := ansi.Strip(renderBox("confirm", "", " n cancel ", "really?"))
	for _, want := range []string{"confirm", "n cancel", "really?", "┌", "└"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tui/ -run TestRenderBoxChrome -v`
Expected: FAIL — `undefined: renderBox`

- [ ] **Step 3: Implement renderBox and restyle the renderers**

Add to `internal/tui/modal.go`:

```go
// renderBox wraps content in the themed overlay chrome: accent border,
// bright title, dim subtitle, inverted chip pinned to the right.
func renderBox(title, sub, chipText, content string) string {
	t := activeTheme
	head := lipgloss.NewStyle().Foreground(t.Bright).Bold(true).Render(title)
	if sub != "" {
		head += lipgloss.NewStyle().Foreground(t.Dim).Render(" " + sub)
	}
	chip := lipgloss.NewStyle().Foreground(t.Panel).Background(t.Accent).Bold(true).Render(chipText)
	inner := max(lipgloss.Width(content), lipgloss.Width(head)+lipgloss.Width(chip)+2)
	gap := max(1, inner-lipgloss.Width(head)-lipgloss.Width(chip))
	header := head + strings.Repeat(" ", gap) + chip
	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(t.Accent).
		Padding(0, 1).
		Render(header + "\n\n" + content)
}
```

Rewrite `renderModal` (modal.go:67-87) to keep its field rendering but use the chrome, with keys in Accent in the hint line:

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
	t := activeTheme
	key := lipgloss.NewStyle().Foreground(t.Accent).Bold(true)
	dim := lipgloss.NewStyle().Foreground(t.Dim)
	lines := []string{
		modalField(m.Field == 0, "title", m.Title, true),
		modalField(m.Field == 1, "body", m.Body, false),
		modalField(m.Field == 2, "flow", flowName, false),
		modalField(m.Field == 3, "preset", preset, false),
		"",
		key.Render("tab") + dim.Render(" next field · ") + key.Render("enter") + dim.Render(" create"),
	}
	return renderBox("new issue", "", " esc cancel ", boundedLines(lines, max(1, width-6)))
}
```

Rewrite `renderConfirm` (modal.go:101-104):

```go
func renderConfirm(prompt string, width int) string {
	t := activeTheme
	key := lipgloss.NewStyle().Foreground(t.Accent).Bold(true)
	dim := lipgloss.NewStyle().Foreground(t.Dim)
	hint := key.Render("y") + dim.Render(" confirm")
	content := boundedLines([]string{prompt, "", hint}, max(1, width-6))
	return renderBox("confirm", "", " n cancel ", content)
}
```

Find the lever editor renderer (`grep -n renderLeverEditor internal/tui/*.go`) and wrap its existing matrix output the same way: `return renderBox("levers", "per-stage autonomy", " esc close ", existingMatrixContent)` — keep the matrix rendering itself unchanged.

Remove the old `"NEW ISSUE"` / `"CONFIRM"` heading lines and old plain hint text from those renderers (the chrome replaces them).

- [ ] **Step 4: Composite instead of replacing the tower**

In `internal/tui/app.go` `View` (lines 1147-1152), the modal/confirm/lever branches currently do `tower = renderModal(...)` etc. Change them to record the overlay and leave the tower alone:

```go
	var overlayBox string
	if m.modal != nil {
		overlayBox = renderModal(*m.modal, layoutWidth)
	} else if m.confirm != nil {
		overlayBox = renderConfirm(m.confirm.Prompt, layoutWidth)
	} else if m.leverEditor != nil {
		overlayBox = renderLeverEditor(m.leverEditor.Stages, m.leverEditor.Matrix, m.leverEditor.Sel)
	} else if m.pager.Mode == "artifacts" {
	...
```

Then at the end of `View`, extend the compositing added in Task 4:

```go
	screen := b.String()
	switch {
	case m.help:
		return overlayCenter(screen, renderHelpOverlay(layoutWidth), layoutWidth, max(m.Height, lipgloss.Height(screen)))
	case overlayBox != "":
		return overlayCenter(screen, overlayBox, layoutWidth, max(m.Height, lipgloss.Height(screen)))
	}
	return screen
```

- [ ] **Step 5: Run package tests, fix fallout**

Run: `go test ./internal/tui/ -v 2>&1 | tail -20`
Expected: PASS after updating any tests asserting the old `NEW ISSUE`/`CONFIRM` headings or tower-replacement behavior (assert via `ansi.Strip` on `View()` output — modal text AND grid text are now both present).

- [ ] **Step 6: Commit**

```bash
git add internal/tui/modal.go internal/tui/app.go internal/tui/*_test.go
git commit -m "feat: themed overlay chrome for new-issue, confirm, and lever editor"
```

---

### Task 6: Toast restyle

**Files:**
- Modify: `internal/tui/rail.go` (`renderToast`, ~line 183)
- Test: `internal/tui/rail_test.go`

**Interfaces:**
- Consumes: `activeTheme`.
- Produces: nothing new — `renderToast` signature unchanged.

- [ ] **Step 1: Write the failing test**

```go
func TestRenderToastUsesTheme(t *testing.T) {
	d := projection.DecisionView{ID: 3, Stage: "plan", Question: "Pick one", Options: []string{"a", "b"}, Recommended: 0}
	out := renderToast(d, Identity{Tag: "st-1"}, 0, 0, 60)
	// theme accent (tokyo-night #bb9af7 → truecolor SGR 187;154;247) on border/keys
	if !strings.Contains(out, "187;154;247") {
		t.Fatalf("no accent color in toast:\n%q", out)
	}
	if strings.Contains(out, "\x1b[38;5;245m") {
		t.Fatal("hardcoded color 245 still present")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tui/ -run TestRenderToastUsesTheme -v`
Expected: FAIL (no accent SGR yet)

- [ ] **Step 3: Implement**

In `renderToast` (rail.go:183):
- Replace `dim := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))` with `dim := lipgloss.NewStyle().Foreground(activeTheme.Dim)`.
- Render the `DECISION [n] tag stage` heading with `lipgloss.NewStyle().Foreground(activeTheme.Heading).Bold(true)`.
- Render the selection cursor `▸` and the hint-line keys (find the trailing hint line in the function — `y accept · j/k move · enter choose · o evidence · esc dismiss` or similar) with keys in `activeTheme.Accent`, separators/dim text in `activeTheme.Dim`.
- Set the toast's border style (the function or its caller applies a border — check the end of `renderToast` / `boundedLines` wrapping) to `BorderForeground(activeTheme.Accent)`. If the toast currently has no border, add `lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(activeTheme.Accent).Padding(0, 1)` around the joined lines.

Adjust the exact strings to what is currently in the function — keys/labels content unchanged, colors only.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/tui/ -v 2>&1 | tail -10`
Expected: PASS (update existing toast tests that compare raw strings — strip ANSI with `ansi.Strip` where they assert text).

- [ ] **Step 5: Commit**

```bash
git add internal/tui/rail.go internal/tui/rail_test.go
git commit -m "feat: decision toast adopts theme tokens"
```

---

### Task 7: Full verification and live check

**Files:** none new.

- [ ] **Step 1: Full suite and vet**

Run: `go test ./... && go vet ./...`
Expected: all pass.

- [ ] **Step 2: Rebuild and eyeball**

Use the `rebuilding-guildhall` skill to rebuild/install, then open the tower and press `?` — confirm: bordered violet help box centered over the dimmed control room, `esc close` chip top-right, aligned key columns, no duplicated `? close help · q quit` line. Press `n` for the new-issue modal and `L` for the lever editor to confirm the same chrome. Trigger nothing destructive.

- [ ] **Step 3: Commit any polish, done**
