# TUI Design Cohesion — Design

**Date:** 2026-07-28
**Status:** Approved
**Reference:** design-direction artifact (https://claude.ai/code/artifact/57ffbac8-8a80-4d24-970a-247a5420aa40)

## Problem

The control room's flows (tower, decisions, tray, overlays, arch map, pager, help) each
invent their own visual rules on a single flat background: hierarchy is carried entirely
by text color, glyphs are inconsistent enough to need a legend line, keybindings are
styled ad hoc per surface, and status colors are hard-coded hexes outside the theme.
The result reads as floating text rather than one instrument panel.

A second problem is process: there is no way for an agent (or CI) to check visual work
on the TUI without a human screenshotting and pasting output.

## Goals

1. Every flow shares one anatomy: chrome header, content on layered surfaces, keybar footer.
2. One color contract, one glyph language, one overlay box — enforced by shared helpers,
   not convention.
3. Agent-checkable visuals: render any flow to ANSI/PNG from fixtures in one command,
   plus a tmux script that captures the real binary.

## Non-goals

- No behavior changes: key handling, daemon protocol, projection logic stay untouched.
- No new flows or features; this restyles what exists.
- No light theme.

## Design system

### Tokens (`internal/tui/theme.go`)

`Theme` grows from 6 to 13 tokens:

| Token | Role |
|---|---|
| `Bg0` | canvas — content ground |
| `Bg1` | chrome — header/footer bars, cards, rail surfaces |
| `Bg2` | selection — cursor rows, card header bands |
| `Bg3` | key chips |
| `Accent` | focus & agency: cursor rows, focused lane rule, selected option, overlay borders, keys |
| `Structure` | nouns & identity: help group names, active arch modules, working state (was `Heading`) |
| `Ok` | outcomes: done, shipped, healthy, diff additions |
| `Warn` | attention: need-you cells, question badges, decision numbers, ★ |
| `Err` | outcomes: failed, killed, diff deletions, footer errors |
| `Text` | body text |
| `Dim` | secondary text |
| `Dimmer` | waiting/idle glyphs, ghost content |
| `Bright` | titles, emphasized text |

The hard-coded `statusBad/statusWarn/statusOk` hexes in `render.go` fold into
`Err/Warn/Ok`. Presets: tokyo-night (default), catppuccin, and gruvbox map all 13 slots
with truecolor values. The `terminal` (ANSI-16) preset approximates: `Bg0`/`Bg1` share
the terminal background, `Bg2`/`Bg3` use the ANSI bright-black ground; degradation is
deliberate — the glyph language carries state without backgrounds.

Color contract (the rule reviewers enforce): each hue has exactly one job and hues never
trade. Violet on screen means "pressing enter does something there." Amber means "a human
is needed." Green/red are outcomes only, never chrome. Blue identifies, never signals
state. Per-issue identity colors remain a separate axis on top (existing `Identity`).

### Glyphs (`internal/tui/chrome.go`)

One named constant set used by every surface: `●` done/healthy, `◔` need-you,
`◐` working (rendered in `Structure` blue — an agent doing its job needs nothing from
you; this is a semantic change from today's amber), `○` waiting/idle, `✕` failed,
`▸` selected cursor, `⇡` shipped, `⏸` parked. The footer legend line
("○ working · ✓ done · × FAILED …") is removed; the help overlay is the only key/glyph
reference.

### Shared chrome (`internal/tui/chrome.go`, new file)

- `renderChromeHeader(width, badges, right)` — full-width `Bg1` bar: attention badges
  left (amber question pill, red failing pill, green all-clear), dim detail text right
  (building/shipped/tokens/$).
- `renderKeybar(width, bindings, right)` — full-width `Bg1` footer of key chips; the
  right slot docks transient errors in `Err` (errors stop floating below the footer).
- `keyChip(key)` — `Bg3` ground, `Bright` text; used identically by the global footer,
  card footers, and overlays.
- `cursorRow(selected, content)` — `▸` in `Accent` + `Bg2` ground when selected,
  two-space gutter when not; every list (decisions door, tray, arch, levers) uses it.
- `renderBox` (existing) gains a header-band variant: `Bg2` band holding tag/title/meta
  with an `Accent`-bordered body — the one overlay box for decision card, new-issue,
  levers, and help.

### Spacing constants

Next to the theme: panel padding (1 line vertical, 2 cells horizontal), 2-cell gutters,
exactly one blank row between sections. Renderers use the constants, never literal
spacing.

## Flow-by-flow

1. **Floor / tower** — chrome header; war-room order line directly beneath; tower becomes
   a bordered grid: lane header cells show identity tag + title + dim sub-line (branch ·
   runner), the focused lane gets an `Accent` top rule on a `Bg2` ground; stage-label
   column in dim smallcaps; cells are glyph + state word + dim meta (elapsed, tokens).
   Shipped/parked shelf docks above the persistent keybar. `z` rows layout gets the same
   treatment transposed.
2. **Decision card + door** — card on the header-band box: tag chip, title, reversibility
   verdict right-aligned (`↺ …` in `Ok`/`Warn` by cost). Options become bordered rows
   with `◉/○` radios; the ★ recommendation is pre-selected (`Accent` border, faint accent
   fill). Card footer is a keybar. The decisions door lists via `cursorRow`: number in
   `Warn`, identity tag, stage, question, age.
3. **Triage tray** — `cursorRow` items: title in `Text` (`Bright` when selected), body
   wrapped in `Dim` beneath; keybar `enter accept → new issue · r reject · esc back`.
4. **New-issue + lever overlays** — shared box over the floor dimmed via `Dimmer`
   re-rendering (no true alpha in terminals). New-issue fields are bordered `Bg0` inputs;
   active field gets `Accent` border + block caret. Levers render name left, value right
   in `Structure`/cyan; selected lever wraps its value in `◂ value ▸` with `Accent`
   arrows.
5. **Arch map** — active modules: `▣` in `Structure`, name, identity-colored
   builder/brusher marks; quiet modules collapse to one dim `▢ N quiet areas` row with a
   dim name sample; selected module's detail moves into the keybar's right slot.
6. **Evidence / diff pager** — reading mode: no accent except the position indicator;
   filenames `Bright`, hunks in cyan (`Structure`-adjacent), additions/deletions in
   `Ok`/`Err` foregrounds with faint matching background fills where truecolor allows.
7. **Help** — shared box; group names in `Structure`, keys in `Accent`, two columns
   collapsing to one under width pressure (existing behavior kept).

## Visual verification harness

### Snapshot command (iteration loop)

`guildhall snap [--out DIR] [--width N] [--flow NAME]` — hidden subcommand (excluded
from help text). Renders every flow from canned fixture state to `DIR/<flow>.txt`
(ANSI, via the real render functions at a fixed width, default 200) and, when the
`freeze` CLI is on PATH, also `DIR/<flow>.png`. Fixtures live in
`internal/tui/fixtures.go` (test-adjacent, not shipped logic): a `projection.State`
exercising the interesting cases — a need-you decision, a working stage with token meta,
a failed stage, shipped + parked shelf items, 3 tray proposals, long/unicode titles, and
a narrow-width variant.

Golden tests: `internal/tui/snapshot_test.go` compares each flow's ANSI render against
`internal/tui/testdata/<flow>.golden`, with `-update` to regenerate. This is the
regression net; PNGs are for eyes only and are never committed.

### tmux capture (acceptance pass)

`scripts/tui-capture.sh <flow>`: starts a daemon against a temp dir, seeds it through the
existing CLI (`guildhall new`, `proposals`, etc.), runs `tower` in a detached tmux
session at a pinned size, sends the flow's key sequence, polls `capture-pane -e` until
painted, writes ANSI + freeze PNG, kills the session. Used at the end of implementation
and for any real-daemon-only doubt; not part of CI.

## Testing

- Existing `internal/tui` unit tests keep passing with updated expectations.
- Golden snapshot tests cover every flow at default and narrow widths.
- `go test ./...` green after every task.
- Final acceptance: tmux captures of the real binary reviewed against the artifact.

## Implementation order

Harness first (so all subsequent work is self-checkable), then foundations (tokens,
glyphs, chrome helpers), then flows in the order above, each an independently
verifiable task ending with regenerated goldens and a PNG check.
