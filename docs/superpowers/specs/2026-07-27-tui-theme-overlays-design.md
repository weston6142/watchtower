# TUI Theme + Modal Overlays Design

Date: 2026-07-27
Status: approved direction (Tokyo Night default), spec pending user review

## Problem

The TUI's help view and modals are plain unstyled text (see `renderHelp` in
`internal/tui/render.go`, `renderModal`/`renderConfirm` in `internal/tui/modal.go`).
Help renders inline as dot-separated prose lines, which is hard to scan. The
desired look is herdr-style: a bordered, accent-colored modal floating over the
dimmed main view, with keys aligned in a colored column.

Reference mockups (approved by user):
- Help overlay: https://claude.ai/code/artifact/826503b4-6576-492a-8e18-82a17d29fe37
- Theme options: https://claude.ai/code/artifact/f8ca33cd-3f23-4acb-8ff3-b63d0806a1a6 (option 2, Tokyo Night, chosen)

## Design

### 1. Theme tokens (`internal/tui/theme.go`, new file)

A theme is six tokens:

```go
type Theme struct {
    Accent  lipgloss.Color // keys, modal border, esc-close chip
    Heading lipgloss.Color // section labels, footer key-labels
    Text    lipgloss.Color // descriptions, body text
    Dim     lipgloss.Color // subtitles, secondary text
    Bright  lipgloss.Color // modal title
    Panel   lipgloss.Color // chip text on accent background (modal bg is left transparent)
}
```

Presets, selected by name:

| name | Accent | Heading | Text | Dim | Bright | Panel |
|---|---|---|---|---|---|---|
| `tokyo-night` (default) | `#bb9af7` | `#7aa2f7` | `#c0caf5` | `#565f89` | `#e4ecff` | `#1f2335` |
| `terminal` | ANSI `13` | ANSI `12` | default fg | ANSI `8` | ANSI `15` | ANSI `0` |
| `catppuccin` | `#cba6f7` | `#89b4fa` | `#cdd6f4` | `#6c7086` | `#f0f4ff` | `#181825` |
| `gruvbox` | `#d3869b` | `#fabd2f` | `#ebdbb2` | `#928374` | `#fbf1c7` | `#32302f` |

We do not set background colors on text runs (terminal bg shows through);
`Panel` exists only for the inverted `esc close` chip. The three status colors
(`statusOk/Warn/Bad` in render.go) stay as-is for now.

Unknown theme name → warn-free fallback to `tokyo-night`.

### 2. Config

Add `Theme string \`yaml:"theme"\`` to `repocfg.Config`
(`internal/repocfg/repocfg.go`). Empty → `tokyo-night`. The TUI already loads
repo config at startup; thread the resolved `Theme` into the tui model.

### 3. Overlay compositor (`internal/tui/overlay.go`, new file)

`overlayCenter(base, modal string, width, height int) string` — splits both
into lines, dims the base (re-render base lines through `Faint`; strip existing
ANSI first with `x/ansi.Strip` so colors collapse to faint), then splices the
modal's lines into the base at the centered x/y using ANSI-aware
truncation/padding (`x/ansi` is already an indirect dependency via lipgloss).
The base is the fully rendered normal view, so the control room stays visible
behind the box.

### 4. Help overlay (rewrite `renderHelp`)

Bordered box (`lipgloss.NormalBorder`, `BorderForeground(theme.Accent)`):

- Header row: `help` (Bright, bold) · `every key in the control room` (Dim) ·
  right-aligned `esc close` chip (Panel-on-Accent, bold).
- Body: two columns of groups — navigation + control left, doors + decisions
  right (stack to one column when the box would exceed the terminal width).
  Each binding is a row: key in a fixed-width Accent column, description in
  Text. Section labels uppercase in Heading.
- Footer strip under a Dim rule: `close ? / esc · quit q / ctrl+c` (labels in
  Heading, keys in Text). SYSTEM section is removed.
- The duplicated `? close help · q quit` status line (`app.go:1144`) is removed;
  the help branch composites the overlay over the normal view instead of
  replacing it.

Key content is unchanged from today's `renderHelp` — same bindings, regrouped.

### 5. Same treatment for other modals

- `renderModal` (new issue) and `renderConfirm`: Accent border, Bright title,
  `esc cancel` / `n cancel` chip in the header, hint line in Dim with keys in
  Accent; composited via `overlayCenter` instead of replacing the tower.
- Lever editor: same box treatment (border/title/chip); internal matrix
  rendering unchanged.
- Toast (`renderToast`): keep its current position (stacked under the tower —
  spatial-memory rule, it is not an overlay), but restyle: Accent border,
  Heading section labels, Accent keys in its hint line, `245` hardcode replaced
  with theme.Dim.

### 6. Testing

- `theme_test.go`: preset lookup, fallback on unknown name, config threading.
- `overlay_test.go`: modal splices at centered coordinates, base dimmed, ANSI
  sequences intact, modal larger than base degrades to plain centered render.
- Existing render tests updated: strip ANSI where they assert plain text
  (helpers exist), help test asserts grouped/aligned output.

## Out of scope

- Theming the grid/rail/header (only overlays + toast this pass).
- Custom user-defined palettes in config (presets only; struct makes it easy later).
- Light themes.
