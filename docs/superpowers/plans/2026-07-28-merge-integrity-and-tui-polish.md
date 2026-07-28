# Merge Integrity and TUI Polish Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the merge stage from reporting success without landing work (detached-HEAD treehouse worktrees), and fix seven TUI legibility issues.

**Architecture:** Merge integrity lands in three layers: `workspace.Treehouse` checks out a real branch after leasing; `marshal.Train.Land` rejects `HEAD`/empty branch names and verifies the branch tip is an ancestor of the default branch after merging (failures flow through the existing `landWithEscalation` path, so no engine changes). TUI fixes are localized to `internal/tui` render functions plus small plumbing: a `Model`/`Effort` pair on `proto.IssueDetail` resolved server-side from flow → agent package.

**Tech Stack:** Go, bubbletea/lipgloss, sqlite store, `claude` CLI subprocess.

Spec: `docs/superpowers/specs/2026-07-28-merge-integrity-and-tui-polish-design.md`

## Global Constraints

- Repo module path is `github.com/weston6142/watchtower`; the directory is `~/guildhall` and must NOT be renamed.
- Run `go test ./...` from the repo root before every commit; all tests must pass.
- TUI state glyphs and theme colors come from `internal/tui/chrome.go` / `theme.go`; do not invent new colors.
- Keep help copy in `render.go` `helpGroups` in sync with any key behavior change.

---

### Task 1: `Train.Land` rejects detached/empty branch and verifies the merge landed

**Files:**
- Modify: `internal/marshal/train.go`
- Test: `internal/marshal/train_test.go`

**Interfaces:**
- Consumes: existing `Train.Land(ctx, issueID, branch string) error`.
- Produces: same signature; new failure modes — `branch "HEAD"/""` → error `no branch to merge: worktree is detached (HEAD)`; post-merge ancestor check failure → error containing `merge did not land`. Task 2 relies on `Land` accepting `issue/<id>` branches unchanged.

- [ ] **Step 1: Write failing tests**

Append to `internal/marshal/train_test.go`:

```go
func TestLandRejectsDetachedHead(t *testing.T) {
	repo, _ := repoWithBranch(t, false)
	tr := &Train{Repo: repo}
	for _, branch := range []string{"HEAD", ""} {
		if err := tr.Land(context.Background(), "GH-1", branch); err == nil {
			t.Fatalf("Land(%q) succeeded; want detached-head error", branch)
		}
	}
	log := git(t, repo, "log", "--oneline", "main")
	if strings.Contains(log, "branch work") {
		t.Fatalf("main moved on rejected branch: %s", log)
	}
}

func TestLandVerifiesBranchIsAncestor(t *testing.T) {
	repo, branch := repoWithBranch(t, false)
	tr := &Train{Repo: repo}
	if err := tr.Land(context.Background(), "GH-1", branch); err != nil {
		t.Fatal(err)
	}
	// the branch tip must now be reachable from main
	if _, err := exec.Command("git", "-C", repo, "merge-base", "--is-ancestor", branch, "main").CombinedOutput(); err != nil {
		t.Fatalf("branch not ancestor of main after Land: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify the new one fails**

Run: `go test ./internal/marshal/ -run TestLand -v`
Expected: `TestLandRejectsDetachedHead` FAILS (Land currently merges `HEAD` "successfully"); others pass.

- [ ] **Step 3: Implement**

In `internal/marshal/train.go`, at the top of `Land`:

```go
func (tr *Train) Land(ctx context.Context, issueID, branch string) error {
	if branch == "" || branch == "HEAD" {
		return fmt.Errorf("no branch to merge: worktree is detached (HEAD); commits were not landed")
	}
	def, err := tr.defaultBranch()
	...
```

And inside `attempt()`, after the merge succeeds and before the test-command block, verify the branch actually landed:

```go
	attempt := func() error {
		if out, err := tr.git("merge", "--no-ff", "--no-edit", branch); err != nil {
			_, _ = tr.git("merge", "--abort")
			return fmt.Errorf("%w: %s", errMergeConflict, out)
		}
		if _, err := tr.git("merge-base", "--is-ancestor", branch, def); err != nil {
			_, _ = tr.git("reset", "--hard", pre)
			return fmt.Errorf("merge did not land: %s is not reachable from %s after merge", branch, def)
		}
		if len(tr.TestCmd) > 0 {
		...
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/marshal/ -v`
Expected: all PASS (including the pre-existing clean-merge, conflict, and rollback tests — the ancestor check must not break them).

- [ ] **Step 5: Commit**

```bash
git add internal/marshal/train.go internal/marshal/train_test.go
git commit -m "fix: Land rejects detached HEAD and verifies the branch actually landed"
```

---

### Task 2: `Treehouse.Acquire` puts the leased worktree on a real branch

**Files:**
- Modify: `internal/workspace/workspace.go:41-61`
- Test: `internal/workspace/workspace_test.go`

**Interfaces:**
- Consumes: `treehouse get --lease --lease-holder <id>` printing a worktree path.
- Produces: `Treehouse.Acquire(issueID)` returns a path whose worktree is checked out on branch `issue/<issueID>` (created or reset with `git checkout -B`). The engine's later `git rev-parse --abbrev-ref HEAD` then yields `issue/<id>`, never `HEAD`.

- [ ] **Step 1: Write the failing test**

Append to `internal/workspace/workspace_test.go`. The fake `treehouse` binary is a shell script on PATH that prints a pre-made detached-HEAD worktree path:

```go
func TestTreehouseAcquireChecksOutBranch(t *testing.T) {
	repo := initRepo(t)
	wt := filepath.Join(t.TempDir(), "leased")
	head, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", repo, "worktree", "add", "--detach", wt, strings.TrimSpace(string(head))).CombinedOutput()
	if err != nil {
		t.Fatalf("worktree add: %v %s", err, out)
	}

	binDir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = get ]; then echo " + wt + "; fi\n"
	if err := os.WriteFile(filepath.Join(binDir, "treehouse"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := Treehouse{Repo: repo}
	path, _, err := p.Acquire("GH-7")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := exec.Command("git", "-C", path, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(branch)); got != "issue/GH-7" {
		t.Fatalf("worktree on %q, want issue/GH-7", got)
	}
}
```

Add `"strings"` to the test file's imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/workspace/ -run Treehouse -v`
Expected: FAIL — worktree still on `HEAD` (detached).

- [ ] **Step 3: Implement**

In `Treehouse.Acquire`, after `path := strings.TrimSpace(string(out))`:

```go
	path := strings.TrimSpace(string(out))
	// Treehouse leases detached-HEAD worktrees; the merge train needs a real
	// branch, so pin the lease to issue/<id> (-B resets a leftover branch from
	// a prior lease of the same issue to the leased tip).
	if co, err := exec.Command("git", "-C", path, "checkout", "-q", "-B", "issue/"+issueID).CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("checkout issue branch: %v: %s", err, co)
	}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/workspace/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/workspace/workspace.go internal/workspace/workspace_test.go
git commit -m "fix: treehouse worktrees get a real issue/<id> branch so merges can land"
```

---

### Task 3: Artifacts view wears the app's box chrome

**Files:**
- Modify: `internal/tui/pager.go:43-60` (`renderArtifactList`)
- Test: `internal/tui/pager_test.go`

**Interfaces:**
- Consumes: `renderBox(title, sub, chipText, content string) string` (modal.go:69), `cursorRow(selected bool, content string, width int) string` (chrome.go:117), `themeDim`.
- Produces: `renderArtifactList(p pagerState, id Identity, width, height int) string` — same signature, now a bordered box titled `artifacts` with cursorRow selection and a key-hint footer.

- [ ] **Step 1: Write the failing test**

Append to `internal/tui/pager_test.go`:

```go
func TestArtifactListWearsBoxChrome(t *testing.T) {
	p := pagerState{Mode: "artifacts", Files: []string{"a.md", "b.md"}, Sel: 1, Title: "GH-1"}
	out := renderArtifactList(p, Identity{Tag: "◆"}, 80, 20)
	if !strings.Contains(out, "─") {
		t.Fatal("artifact list has no border")
	}
	if !strings.Contains(out, glyphCursor) {
		t.Fatal("selected row has no cursor glyph")
	}
	if !strings.Contains(out, "esc back") {
		t.Fatal("missing esc hint")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run ArtifactList -v`
Expected: FAIL (no border, no `▸`).

- [ ] **Step 3: Implement**

Replace `renderArtifactList`:

```go
func renderArtifactList(p pagerState, id Identity, width, height int) string {
	t := activeTheme
	dim := lipgloss.NewStyle().Foreground(t.Dim)
	inner := max(20, width-8)
	var body []string
	if len(p.Files) == 0 {
		body = append(body, themeDim.Render("no artifacts"))
	} else {
		for i, file := range p.Files {
			body = append(body, cursorRow(i == p.Sel, truncate(file, max(1, inner-2)), inner))
		}
	}
	if height > 4 && len(body) > height-4 {
		body = body[:height-4]
	}
	foot := keyChip("enter") + dim.Render(" open  ") + keyChip("esc") + dim.Render(" back to tower")
	sub := strings.TrimSpace(id.Tag + " " + p.Title)
	return renderBox("artifacts", sub, " esc back ", strings.Join(append(body, "", foot), "\n"))
}
```

- [ ] **Step 4: Run tests, update snapshots if any break**

Run: `go test ./internal/tui/ -v`
Expected: PASS. If snapshot tests under `internal/tui/testdata/` cover the artifacts surface and now differ, regenerate them the way `snapshot_test.go` documents (look for an `-update` flag in that file) and eyeball the diff.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/pager.go internal/tui/pager_test.go internal/tui/testdata
git commit -m "feat: artifacts list wears the shared box chrome with cursor rows"
```

---

### Task 4: `g` expands the war room in place

**Files:**
- Modify: `internal/tui/app.go` (Model struct + key handling around app.go:563), `internal/tui/render.go:306-310, 548-582`
- Test: `internal/tui/render_test.go`, `internal/tui/app_test.go`

**Interfaces:**
- Consumes: `warRoom(st *projection.State, ids map[string]Identity) string`, `renderTowerConfigured(...)`.
- Produces: `warRoomLines(st *projection.State, ids map[string]Identity, expanded bool) []string` — line 1 is the existing summary (highlighted when expanded), line 2 (only when expanded) breaks out the full shipping order and queue counts. `Model` gains field `warExpanded bool`. `renderTowerConfigured` gains a trailing `warExpanded bool` parameter.

- [ ] **Step 1: Write failing tests**

In `internal/tui/render_test.go`:

```go
func TestWarRoomExpanded(t *testing.T) {
	st := projection.NewState()
	collapsed := warRoomLines(st, nil, false)
	if len(collapsed) != 1 {
		t.Fatalf("collapsed war room = %d lines, want 1", len(collapsed))
	}
	expanded := warRoomLines(st, nil, true)
	if len(expanded) != 2 {
		t.Fatalf("expanded war room = %d lines, want 2", len(expanded))
	}
	if !strings.Contains(expanded[1], "shipping order") {
		t.Fatalf("breakout line missing detail: %q", expanded[1])
	}
}
```

In `internal/tui/app_test.go` (follow the file's existing pattern for driving keys through `Update` — reuse its model-construction helper):

```go
func TestWarRoomKeyToggles(t *testing.T) {
	m := newTestModel(t) // use the file's existing helper name; adapt if it differs
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	if !next.(Model).warExpanded {
		t.Fatal("g did not expand the war room")
	}
	next, _ = next.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	if next.(Model).warExpanded {
		t.Fatal("second g did not collapse the war room")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tui/ -run 'WarRoom' -v`
Expected: FAIL — `warRoomLines` and `warExpanded` undefined.

- [ ] **Step 3: Implement**

1. In `render.go`, keep `warRoom` as the summary-string builder and add:

```go
// warRoomLines renders the war-room strip: the summary line, plus a breakout
// line when expanded via the g key.
func warRoomLines(st *projection.State, ids map[string]Identity, expanded bool) []string {
	summary := warRoom(st, ids)
	if !expanded {
		return []string{summary}
	}
	t := activeTheme
	head := lipgloss.NewStyle().Foreground(t.Bright).Background(t.Bg2).Bold(true).Render(summary)
	var lane []string
	busy := 0
	if st != nil {
		for _, id := range st.Order {
			iv := st.Issues[id]
			if iv == nil {
				continue
			}
			if iv.Merged || iv.CurrentStage == "merge" || iv.Behind != "" || iv.State == "done" {
				tag := id
				if identity, ok := ids[id]; ok {
					tag = identity.Tag
				}
				lane = append(lane, tag)
			}
			if iv.State == "running" && iv.CurrentStage != "" {
				busy++
			}
		}
	}
	order := "—"
	if len(lane) > 0 {
		order = strings.Join(lane, " → ") // full order, no +n cap
	}
	detail := fmt.Sprintf("shipping order %s · %d building · g collapse", order, busy)
	return []string{head, themeDim.Render(detail)}
}
```

2. In `renderTowerConfigured` (render.go:306), change the signature to `..., tick, width int, warExpanded bool)` and replace line 310:

```go
	lines := append(warRoomLines(st, ids, warExpanded), "MAP · "+mapInsight(st.Issues)+" · a full map")
```

Update `renderTower` (render.go:302) and every other caller (grep `renderTowerConfigured(` — app.go:1172 and any tests) to pass the new argument (`false` where no model is available, `m.warExpanded` in `View`).

3. In `app.go`: add `warExpanded bool` to `Model`; in the key switch, before the `if key == "a"` block near app.go:563:

```go
		if key == "g" {
			m.warExpanded = !m.warExpanded
			return m, nil
		}
```

Any focus move collapses it: inside the `if moved != m.Focus` branch (app.go:572), add `m.warExpanded = false`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/tui/ -v`
Expected: PASS; regenerate snapshots only if a snapshot legitimately includes the war-room row.

- [ ] **Step 5: Commit**

```bash
git add internal/tui
git commit -m "feat: g toggles an in-place war room breakout instead of dead-ending"
```

---

### Task 5: Timeline refreshes live and guards no-focus

**Files:**
- Modify: `internal/tui/app.go:194-203` (tick/Msg refresh), `app.go:438-441` (open handler)
- Test: `internal/tui/app_test.go`

**Interfaces:**
- Consumes: `humanizeEvents(evs []core.Event, issueID string) []string` (doors.go:96), `msgNoLaneFocused` (existing const), `m.currentMode()`.
- Produces: opening timeline without a focused lane sets `m.Err = msgNoLaneFocused` and does not push the mode; while the timeline door is open, `m.doorLines` is recomputed on every `Msg`/`tickMsg`.

- [ ] **Step 1: Write failing tests**

```go
func TestTimelineRequiresFocus(t *testing.T) {
	m := newTestModel(t) // adapt to the file's helper; ensure Focus.Issue == ""
	m.Focus = Focus{}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	nm := next.(Model)
	if nm.currentMode() == "timeline" {
		t.Fatal("timeline opened with no lane focused")
	}
	if nm.Err != msgNoLaneFocused {
		t.Fatalf("Err = %q, want no-lane message", nm.Err)
	}
}

func TestTimelineRefreshesOnEvents(t *testing.T) {
	m := newTestModel(t)
	m.Focus.Issue = "GH-1"
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	nm := next.(Model)
	ev := core.Event{IssueID: "GH-1", Type: core.EvStageStarted, At: time.Now(),
		Payload: json.RawMessage(`{"stage":"plan"}`)}
	next, _ = nm.Update(Msg{Events: []core.Event{ev}})
	nm = next.(Model)
	if len(nm.doorLines) == 0 || !strings.Contains(nm.doorLines[len(nm.doorLines)-1], "plan started") {
		t.Fatalf("timeline did not pick up new event: %v", nm.doorLines)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tui/ -run Timeline -v`
Expected: both FAIL.

- [ ] **Step 3: Implement**

Open handler (app.go:438):

```go
		case "e":
			if m.Focus.Issue == "" {
				m.Err = msgNoLaneFocused
				return m, nil
			}
			m.modes = append(m.modes, "timeline")
			m.doorLines = humanizeEvents(m.events, m.Focus.Issue)
			return m, nil
```

Refresh: in the `case Msg:` branch (app.go:198-203), after `m = m.applyEvents(msg.Events)`:

```go
		if m.currentMode() == "timeline" {
			m.doorLines = humanizeEvents(m.events, m.Focus.Issue)
		}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/tui/ -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/app.go internal/tui/app_test.go
git commit -m "fix: timeline door refreshes on events and refuses to open unfocused"
```

---

### Task 6: Tool-call lines read as activity, stream door counts them

**Files:**
- Modify: `internal/tui/doors.go:161-202` (`renderStreamDoor`), `internal/tui/app.go:1141-1154` (`streamSubtitle`)
- Test: `internal/tui/doors_test.go`

**Interfaces:**
- Consumes: transcript lines shaped `stage │ text`, tool lines prefixed `↳ `.
- Produces: `renderStreamDoor(subtitle string, lines []string, width int) string` — tool lines styled with the tool name in `t.Structure` (not Dim); subtitle passed in already carries counts. `streamSubtitle` returns `<tag> · <stage> · N lines · M tool calls`. Empty-buffer placeholder becomes "nothing here yet — either the stage just started or the transcript was lost to a daemon restart".

- [ ] **Step 1: Write failing tests**

```go
func TestStreamDoorCountsAndBrightensToolCalls(t *testing.T) {
	lines := []string{"plan │ ↳ Bash(go test ./...)", "plan │ thinking about tests"}
	out := renderStreamDoor("GH-1 · plan · 2 lines · 1 tool call", lines, 100)
	if !strings.Contains(out, "Bash(go test ./...)") {
		t.Fatal("tool line missing")
	}
	if !strings.Contains(out, "1 tool call") {
		t.Fatal("subtitle counts missing from box band")
	}
}

func TestStreamSubtitleCounts(t *testing.T) {
	m := Model{Focus: Focus{Issue: "GH-1"},
		doorLines: []string{"plan │ ↳ Read(a.go)", "plan │ ↳ Read(b.go)", "plan │ ok"}}
	got := m.streamSubtitle()
	if !strings.Contains(got, "3 lines") || !strings.Contains(got, "2 tool calls") {
		t.Fatalf("subtitle = %q", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tui/ -run 'StreamDoor|StreamSubtitle' -v`
Expected: FAIL on the counts assertions.

- [ ] **Step 3: Implement**

In `renderStreamDoor`, replace the dim tool styling (doors.go:182-185) with a brighter treatment:

```go
		style := prose
		if rest, ok := strings.CutPrefix(text, "↳ "); ok {
			name, args, _ := strings.Cut(rest, "(")
			tool := lipgloss.NewStyle().Foreground(t.Structure).Render(name)
			if args != "" {
				tool += prose.Render("(" + args)
			}
			lead := ""
			if stage != "" {
				lead = stage + " │ "
			}
			body = append(body, gutter.Render(lead)+dim.Render("↳ ")+tool)
			continue
		}
```

(Keep the existing wrap path for prose lines untouched.)

Replace the empty placeholder (doors.go:170):

```go
		body = append(body, gutter.Render("nothing here yet — either the stage just started or the transcript was lost to a daemon restart"))
```

In `streamSubtitle` (app.go), append counts before returning:

```go
	tools := 0
	for _, line := range m.doorLines {
		if _, text, found := strings.Cut(line, " │ "); found && strings.HasPrefix(text, "↳ ") {
			tools++
		} else if strings.HasPrefix(line, "↳ ") {
			tools++
		}
	}
	counts := fmt.Sprintf("%d line%s · %d tool call%s", len(m.doorLines), pluralSuffix(len(m.doorLines)), tools, pluralSuffix(tools))
```

Return `tag + " · " + stage + " · " + counts` in the found case and `tag + " · " + counts` in the fallback.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/tui/ -v` → PASS (regenerate stream-door snapshots if present; verify tool lines are no longer Dim in the diff).

- [ ] **Step 5: Commit**

```bash
git add internal/tui
git commit -m "feat: stream door counts tool calls and renders them as visible activity"
```

---

### Task 7: Effort field in packages, wired to the runner

**Files:**
- Modify: `internal/pkgs/pkgs.go:13-19`, `internal/claude/runner.go:48-63`
- Modify: all nine `.watchtower/packages/*/package.yaml`
- Test: `internal/claude/runner_test.go` (create if absent), `internal/pkgs/pkgs_test.go` if present

**Interfaces:**
- Consumes: `pkgs.Package` loaded from YAML.
- Produces: `Package.Effort string` (`yaml:"effort"`, values `low|medium|high`, empty = CLI default). New exported helper in `internal/claude`: `EffortEnv(effort string) string` returning `""` for empty/unknown, else `MAX_THINKING_TOKENS=<n>` with low=1024, medium=8192, high=32768. Task 8 reads both `Model` and `Effort` off `pkgs.Package`.

- [ ] **Step 1: Write the failing test**

Create `internal/claude/runner_env_test.go`:

```go
package claude

import "testing"

func TestEffortEnv(t *testing.T) {
	cases := map[string]string{
		"low":    "MAX_THINKING_TOKENS=1024",
		"medium": "MAX_THINKING_TOKENS=8192",
		"high":   "MAX_THINKING_TOKENS=32768",
		"":       "",
		"weird":  "",
	}
	for in, want := range cases {
		if got := EffortEnv(in); got != want {
			t.Errorf("EffortEnv(%q) = %q, want %q", in, got, want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/claude/ -run EffortEnv -v`
Expected: FAIL — `EffortEnv` undefined.

- [ ] **Step 3: Implement**

`internal/pkgs/pkgs.go` — add to `Package`:

```go
	Effort       string   `yaml:"effort"` // low|medium|high; empty = CLI default
```

`internal/claude/runner.go` — add:

```go
// EffortEnv maps a package effort level to the CLI's thinking-budget env var.
// Empty or unknown levels return "" (CLI default).
func EffortEnv(effort string) string {
	switch effort {
	case "low":
		return "MAX_THINKING_TOKENS=1024"
	case "medium":
		return "MAX_THINKING_TOKENS=8192"
	case "high":
		return "MAX_THINKING_TOKENS=32768"
	}
	return ""
}
```

And in `run`, after `cmd.Env = append(os.Environ(), c.ExtraEnv...)`:

```go
	if env := EffortEnv(pkg.Effort); env != "" {
		cmd.Env = append(cmd.Env, env)
	}
```

Update every `.watchtower/packages/*/package.yaml` to deliberate values (sonnet keeps stage cost visible and predictable; adjust later per package):

```yaml
model: "sonnet"
effort: "medium"
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/claude/ ./internal/pkgs/ -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pkgs internal/claude .watchtower/packages
git commit -m "feat: packages declare model and effort; runner passes the thinking budget"
```

---

### Task 8: Surface model + effort in the focus rail and stream door

**Files:**
- Modify: `internal/proto/proto.go:57-68`, `internal/proto/server.go` (Server struct + `issue_detail`), `cmd/watchtower/main.go` (~line 473, after `srv.SetFlows`), `internal/tui/rail.go:60-95`, `internal/tui/app.go:1141-1154`
- Test: `internal/tui/rail_test.go`, `internal/proto/server_test.go` if present (else the rail test carries the display contract)

**Interfaces:**
- Consumes: `pkgs.Package{Model, Effort}` (Task 7), `flow.Stage.Agents[].{Package,Model}`, `store.LastStageEvents` (server.go:200) for the current stage name.
- Produces: `proto.IssueDetail` gains `Model string \`json:"model,omitempty"\`` and `Effort string \`json:"effort,omitempty"\``; `Server.SetPackages(map[string]pkgs.Package)`; the rail shows a `model <m> · effort <e>` line, with `cli default` for empty values.

- [ ] **Step 1: Write the failing test**

Append to `internal/tui/rail_test.go` (follow the file's existing rail-rendering test pattern for constructing `proto.IssueDetail`):

```go
func TestRailShowsModelAndEffort(t *testing.T) {
	det := &proto.IssueDetail{Issue: store.IssueRow{ID: "GH-1", Title: "t", Flow: "default", State: "running"},
		Model: "sonnet", Effort: "medium"}
	out := renderRail(nil, map[string]Identity{}, det, 60)
	if !strings.Contains(out, "model sonnet · effort medium") {
		t.Fatalf("rail missing model line:\n%s", out)
	}
	det.Model, det.Effort = "", ""
	out = renderRail(nil, map[string]Identity{}, det, 60)
	if !strings.Contains(out, "model cli default") {
		t.Fatalf("rail missing cli-default line:\n%s", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run RailShowsModel -v`
Expected: FAIL — `proto.IssueDetail` has no `Model` field (compile error is the failure).

- [ ] **Step 3: Implement**

1. `proto.go` — add to `IssueDetail`:

```go
	Model     string            `json:"model,omitempty"`
	Effort    string            `json:"effort,omitempty"`
```

2. `server.go` — add `packages map[string]pkgs.Package` to `Server`, plus:

```go
// SetPackages lets the daemon share loaded agent packages so issue_detail can
// report which model/effort a stage runs with.
func (sv *Server) SetPackages(p map[string]pkgs.Package) { sv.packages = p }
```

In the `issue_detail` case, after `LastStageEvents` (server.go:200), resolve model/effort from the current stage's first agent (AgentRef.Model overrides the package's):

```go
		model, effort := "", ""
		if f, ok := sv.flows[issue.Flow]; ok {
			for _, stg := range f.Stages {
				if stg.Name != stage || len(stg.Agents) == 0 {
					continue
				}
				ref := stg.Agents[0]
				if p, ok := sv.packages[ref.Package]; ok {
					model, effort = p.Model, p.Effort
				}
				if ref.Model != "" {
					model = ref.Model
				}
				break
			}
		}
```

and set `Model: model, Effort: effort` on the returned `IssueDetail`. Import `internal/pkgs` in server.go.

3. `cmd/watchtower/main.go` — next to `srv.SetFlows(flows)` (line 473), add `srv.SetPackages(packages)`. Note `packages` is only in scope on the real-runner path (main.go:398); hoist its declaration above the fake/real branch (load it unconditionally, ignore for the fake runner) so it can be passed here — or pass `nil` when running with fixtures.

4. `rail.go` — in `renderRail`, after the flow/status line (rail.go:67):

```go
		model, effort := det.Model, det.Effort
		if model == "" {
			model = "cli default"
		}
		if effort == "" {
			effort = "default"
		}
		lines = append(lines, "model "+model+" · effort "+effort)
```

5. `app.go` `streamSubtitle` — when `m.Detail != nil && m.Detail.Model != ""`, append `" · " + m.Detail.Model` after the counts segment from Task 6.

- [ ] **Step 4: Run the full suite**

Run: `go test ./...`
Expected: PASS everywhere (proto JSON additions are omitempty, so goldens should hold; fix any that assert exact structs).

- [ ] **Step 5: Commit**

```bash
git add internal/proto internal/tui cmd/watchtower
git commit -m "feat: issue detail reports stage model and effort; rail and stream show it"
```

---

### Task 9: Focus rail wears a border

**Files:**
- Modify: `internal/tui/rail.go:60-116` (`renderRail`), `internal/tui/app.go:1170` (width budget)
- Test: `internal/tui/rail_test.go`

**Interfaces:**
- Consumes: existing `renderRail` content lines; `lipgloss` border styles; `activeTheme`.
- Produces: `renderRail(st, ids, det, width)` — same signature; output is a `FOCUS`-banded, `NormalBorder` box in `t.Dimmer` (quieter than the accent doors so it never competes with a door or toast), content width = `width-4`.

This lands after Task 8 so the border wraps the final rail content (model line included).

- [ ] **Step 1: Write the failing test**

```go
func TestRailIsBoxed(t *testing.T) {
	out := renderRail(nil, map[string]Identity{}, nil, 40)
	if !strings.Contains(out, "─") || !strings.Contains(out, "│") {
		t.Fatalf("rail has no border:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if w := lipgloss.Width(line); w > 40 {
			t.Fatalf("rail line %d cells wide, budget 40: %q", w, line)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run RailIsBoxed -v` → FAIL.

- [ ] **Step 3: Implement**

In `renderRail`, drop the leading `"FOCUS"` line (it becomes the band) and wrap the result:

```go
	inner := max(16, width-4) // border + padding
	// ... build lines exactly as before, but bounded to inner ...
	t := activeTheme
	band := lipgloss.NewStyle().Foreground(t.Bright).Background(t.Bg2).Bold(true).Render(" FOCUS ")
	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(t.Dimmer).
		Padding(0, 1).
		Width(width - 2).
		Render(band + "\n" + boundedLines(lines, inner))
```

(`boundedLines(lines, inner)` replaces the old `boundedLines(lines, width)` return; keep the `DECISION QUEUE` section inside the same box.)

- [ ] **Step 4: Run tests + eyeball**

Run: `go test ./internal/tui/ -v` (regen snapshots that include the rail; check no line exceeds the rail column). Then a manual sanity run if convenient: `go run ./cmd/watchtower tower` against fixtures.

- [ ] **Step 5: Commit**

```bash
git add internal/tui
git commit -m "feat: focus rail wears a bordered FOCUS box on the grid layer"
```

---

### Task 10: Full-suite verification

- [ ] **Step 1:** Run `go test ./...` from the repo root. Expected: all packages PASS.
- [ ] **Step 2:** Run `go vet ./...`. Expected: clean.
- [ ] **Step 3:** Manual smoke: rebuild and run the tower (see the `rebuilding-guildhall` skill), then verify: `enter` → boxed artifacts → `esc` back; `g` toggles the war-room breakout; `e` on a focused lane shows a live timeline; the rail is boxed and shows `model … · effort …`; `T` shows tool-call lines and counts.
- [ ] **Step 4:** Commit any snapshot regenerations left over: `git add -A && git commit -m "test: refresh snapshots"` (only if needed).
