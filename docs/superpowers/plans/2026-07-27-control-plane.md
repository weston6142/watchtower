# Control Plane Implementation Plan (Plan 4b)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The engine/protocol half of the UX-review fixes: a responsible decision schema (rationale + consequences, engine-enforced), evidence bundles for gates, stage attempt/error surfacing, pause/kill/retry/set-lever control verbs, live transcript tailing, and cost/overview data — everything Plan 4c's TUI will render.

**Architecture:** All changes are additive to the existing daemon: the decision marker grows required fields (runner coaches non-compliant agents instead of failing them), worktree stages emit an `evidence.json` + `diff.patch` artifact pair computed from git, the engine gains a per-issue control block (pause gate between stages, cancellable stage context, resumable stage index), the runner streams transcript lines into a bounded ring buffer, and five new proto ops expose it all.

**Tech Stack:** unchanged (Go, existing packages, stub-script runner tests).

## Global Constraints

- Plans 1–4 constraints apply. Wire changes additive only; existing ops keep their shapes.
- Decision marker v2 is backward-tolerant on READ (a v1 marker without new fields is coached, not crashed) but the shipped prompts are updated so agents emit v2.
- Control verbs must be safe: kill = cancel current stage only (never the daemon), pause = between-stages gate (never mid-write), retry = re-run from the failed stage with a fresh attempt counter.
- Cost is estimate-labeled: tokens × `--price-per-mtok` (default 0 = hide dollars). Never present as billing truth.
- New events: `issue_paused`, `issue_resumed`, `stage_killed`, `lever_changed`.

---

### Task 1: Decision schema v2 (rationale, consequences, reversibility)

**Files:**
- Modify: `internal/levers/levers.go`, `internal/claude/stream.go`, `internal/claude/runner.go`, `internal/engine/engine.go`, `internal/store/store.go`, `internal/proto/proto.go` (PendingDecision passthrough is automatic via engine type), all seven `dist/packages/*/prompt.md` decision-protocol blocks
- Test: `internal/claude/stream_test.go`, `internal/claude/runner_test.go`, `internal/levers/levers_test.go` (extend)

**Interfaces:**
- `levers.Decision` gains:
  ```go
  Why          string   // one-line agent rationale for the recommendation
  Consequences []string // one line per option, parallel to Options
  Reversible   string   // e.g. "changeable until execute" ("" = unknown)
  ```
- Marker v2 (exact):
  ```json
  {"watchtower_decision": {"question": "...", "options": ["a","b"], "recommended": 0,
   "importance": 0.6, "paths": [], "why": "...",
   "consequences": ["...","..."], "reversible": "..."}}
  ```
- `ExtractDecision` parses the new fields (absent → zero values; still returns ok if question+options present — validation is the runner's job).
- Runner enforcement: when a decision marker is found but `Why == ""` or `len(Consequences) != len(Options)`, the runner does NOT raise an Ask; it sends a coaching user message and lets the agent re-emit (max 2 coach attempts per session, then pass the decision through as-is — a weak decision beats a stuck session):
  ```
  Your watchtower_decision is missing required fields. Re-emit the SAME decision
  as one JSON line including: "why" (one line: why you recommend option N) and
  "consequences" (one line per option, same order as options). Nothing else.
  ```
- Store `DecisionRow` gains `Why string`, `Consequences []string` (JSON column reuse: store in the existing `evidence` TEXT column as JSON `{"why":...,"consequences":[...],"reversible":...}` — no schema migration), populated by `InsertDecision`, read back by `decisionRows`.
- Engine `escalate` copies the fields through; `PendingDecision.D` already carries them (same struct).
- Prompts: replace the decision-protocol paragraph in all seven packages with the v2 marker line and one sentence: "Always include why (your rationale), consequences (one per option), and reversible (when this choice stops being cheap to change). Plain language — no file paths or jargon in question/why/consequences unless the human typed them first."

- [ ] **Step 1: Write the failing tests**

Append to `internal/claude/stream_test.go`:

```go
func TestExtractDecisionV2Fields(t *testing.T) {
	text := `{"watchtower_decision": {"question": "Q?", "options": ["a","b"], "recommended": 1, "importance": 0.5, "paths": [], "why": "b is safer", "consequences": ["fast but risky", "slower, safe"], "reversible": "until execute"}}`
	d, ok := ExtractDecision(text)
	if !ok || d.Why != "b is safer" || len(d.Consequences) != 2 || d.Reversible != "until execute" {
		t.Fatalf("v2 fields: %+v ok=%v", d, ok)
	}
}
```

Add stub + test for coaching in `internal/claude/runner_test.go` — new stub `testdata/coached.sh`:

```bash
#!/bin/sh
# Emits a v1 (incomplete) decision, expects a coaching message, then emits v2.
echo '{"type":"system","subtype":"init","session_id":"s-coach"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"watchtower_decision\": {\"question\": \"Q?\", \"options\": [\"a\",\"b\"], \"recommended\": 0, \"importance\": 0.5, \"paths\": []}}"}]}}'
read _task
read coach
case "$coach" in
  *"missing required fields"*)
    echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"watchtower_decision\": {\"question\": \"Q?\", \"options\": [\"a\",\"b\"], \"recommended\": 0, \"importance\": 0.5, \"paths\": [], \"why\": \"a is standard\", \"consequences\": [\"done now\", \"more work\"], \"reversible\": \"anytime\"}}"}]}}' ;;
  *) echo '{"type":"assistant","message":{"content":[{"type":"text","text":"no coaching received"}]}}' ;;
esac
read reply
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"proceeding"}]}}'
echo '{"type":"result","is_error":false,"usage":{"input_tokens":5,"output_tokens":5}}'
```

```go
func TestRunnerCoachesIncompleteDecision(t *testing.T) {
	done, asks := run(t, abs(t, "testdata/coached.sh"), t.TempDir())
	a := <-asks // must be the COACHED (v2) decision, not the v1 one
	if a.Decision.Why == "" || len(a.Decision.Consequences) != 2 {
		t.Fatalf("ask not coached to v2: %+v", a.Decision)
	}
	a.Reply <- 0
	if res := <-done; res.Err != nil {
		t.Fatal(res.Err)
	}
}
```

(Note stub read order: the coach message arrives after the initial task line; the decision reply arrives third. `chmod +x` the stub.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/claude/ ./internal/levers/ -run 'V2|Coach' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

`levers.Decision` fields; `decisionMarker` struct gains `Why string`, `Consequences []string`, `Reversible string` (json tags) and `ExtractDecision` copies them. Runner: in the decision-found branch:

```go
			if d, found := ExtractDecision(ev.Text); found {
				if (d.Why == "" || len(d.Consequences) != len(d.Options)) && coachCount < 2 {
					coachCount++
					if _, err := stdin.Write(UserMessage(coachMsg)); err != nil {
						return abort(err)
					}
					repliedThisTurn = true // the coaching turn continues the session
					continue
				}
				// ... existing Ask flow
			}
```

(`coachCount` declared with the other loop state; `coachMsg` a package const with the exact text above.) Store: marshal `{why, consequences, reversible}` into the `evidence` column with a comment on the column reuse; unmarshal in `decisionRows`. Update the seven prompt files.

- [ ] **Step 4: Run the full suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ dist/
git commit -m "feat: decision schema v2 with rationale, consequences, and coaching"
```

---

### Task 2: Evidence bundles from worktree stages

**Files:**
- Create: `internal/evidence/evidence.go`
- Modify: `internal/engine/engine.go`
- Test: `internal/evidence/evidence_test.go`, `internal/engine/engine_test.go` (extend)

**Interfaces:**
- ```go
  package evidence
  type FileStat struct { Path string `json:"path"`; Added, Removed int `json:"added","removed"` }
  type Bundle struct {
      Files      []FileStat     `json:"files"`
      Added      int            `json:"added"`
      Removed    int            `json:"removed"`
      Biggest    string         `json:"biggest"`
      AreaWeight map[string]int `json:"area_weight"` // top-level area -> added+removed
  }
  // Collect diffs worktree against baseRef (git diff --numstat baseRef) and
  // writes evidence.json + diff.patch into outDir. Returns the bundle.
  func Collect(worktree, baseRef, outDir string) (Bundle, error)
  ```
- Area = first path segment (or first two when the first is `cmd|internal|pkg|src` — reuse the archmap convention; duplicate the 6-line helper with a comment, or export it from archmap — prefer exporting `archmap.AreaOf(path string) string` and using it in both places).
- Engine: after a stage with `Workspace == "worktree"` completes successfully (before the gate), if `is.branch != ""`: `evidence.Collect(is.wsPath, defaultBranchRef, filepath.Join(DataDir, is.id, "evidence", st.Name))` where `defaultBranchRef` is captured once at workspace acquire (`git -C ws merge-base HEAD <default>`... simplest honest v1: diff against the merge-base of the acquire moment, recorded on issueState as `baseRef`). Emit `artifact_produced` for both files. Store the bundle's `AreaWeight` in the event payload — this is the data the TUI's building/brushing marks consume.

- [ ] **Step 1: Write the failing test**

```go
// internal/evidence/evidence_test.go
package evidence

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func TestCollectBundlesDiff(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "config", "user.email", "t@t")
	git(t, repo, "config", "user.name", "t")
	os.MkdirAll(filepath.Join(repo, "payments"), 0o755)
	os.WriteFile(filepath.Join(repo, "payments", "a.go"), []byte("one\n"), 0o644)
	git(t, repo, "add", "."); git(t, repo, "commit", "-qm", "base")
	// changes: big edit in payments, small edit at root
	os.WriteFile(filepath.Join(repo, "payments", "a.go"), []byte("one\ntwo\nthree\nfour\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644)
	git(t, repo, "add", "."); git(t, repo, "commit", "-qm", "work")

	out := t.TempDir()
	b, err := Collect(repo, "HEAD~1", out)
	if err != nil {
		t.Fatal(err)
	}
	if b.Added != 4 || len(b.Files) != 2 || b.Biggest != "payments/a.go" {
		t.Fatalf("bundle: %+v", b)
	}
	if b.AreaWeight["payments"] != 3 || b.AreaWeight["go.mod"] != 1 {
		t.Fatalf("weights: %v", b.AreaWeight)
	}
	for _, f := range []string{"evidence.json", "diff.patch"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Fatalf("%s missing", f)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/evidence/ -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

`Collect`: `git -C wt diff --numstat baseRef` parsed into FileStats (skip binary `-` lines); `git -C wt diff baseRef` → `diff.patch`; areas via `archmap.AreaOf` (export it: move the depth-2 prefix logic into `func AreaOf(rel string) string`); biggest by added+removed; write `evidence.json` (indent 2). Engine hook + `baseRef` capture at acquire:

```go
	// at workspace acquire, alongside branch capture:
	if out, err := exec.Command("git", "-C", path, "rev-parse", "HEAD").Output(); err == nil {
		is.baseRef = strings.TrimSpace(string(out))
	}
```

and after successful worktree-stage run (in `runStage`, success path before gate):

```go
	if st.Workspace == "worktree" && is.baseRef != "" {
		evDir := filepath.Join(e.cfg.DataDir, is.id, "evidence", st.Name)
		if b, err := evidence.Collect(is.wsPath, is.baseRef, evDir); err == nil {
			e.emit(core.EvArtifactProduced, is.id, map[string]any{
				"stage": st.Name, "artifact": "evidence.json",
				"path": filepath.Join(evDir, "evidence.json"), "area_weight": b.AreaWeight})
			e.emit(core.EvArtifactProduced, is.id, map[string]any{
				"stage": st.Name, "artifact": "diff.patch", "path": filepath.Join(evDir, "diff.patch")})
		}
	}
```

Engine test: extend the worktree test (`TestWorktreeAcquiredOnceAndReleased`) — make `fakeWS.dir` a real git repo (reuse a tiny init helper), have the fake executor script write a file, and assert two `artifact_produced` events with `artifact` values `evidence.json`/`diff.patch` appear. (FakeRunner writes artifacts into the workdir already — a `git add -A && commit` isn't done by the fake, so Collect diffs working tree vs baseRef: ensure `Collect` uses plain `git diff baseRef` which includes uncommitted changes. It does.)

- [ ] **Step 4: Run the full suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/
git commit -m "feat: evidence bundles (diff stats, patch, area weights) from worktree stages"
```

---

### Task 3: Stage attempts and error surfacing

**Files:**
- Modify: `internal/engine/engine.go`, `internal/projection/projection.go`, `internal/proto/server.go` (IssueDetail)
- Test: `internal/projection/projection_test.go`, `internal/engine/engine_test.go` (extend)

**Interfaces:**
- Engine: `stage_started` payload gains `"attempt": n, "of": retries+1`; `stage_failed` keeps `error` and gains `"attempt"`, `"of"`, `"final": bool`.
- Projection `IssueView` gains `Attempt, AttemptOf int`, `LastError string` (set on `stage_failed`, cleared on `stage_started` of a new stage or successful completion).
- `proto.IssueDetail` gains `LastError string`, `Attempt, AttemptOf int` (server reads from projection? No — server has no projection; derive from the events table: last `stage_failed`/`stage_started` payloads for the issue. Add `func (s *Store) LastStageEvents(issueID string) (attempt, of int, lastErr string, err error)` reading the two most recent relevant events).

- [ ] **Step 1: Write the failing tests**

Projection:

```go
func TestAttemptAndErrorSurfacing(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "x", "flow": "default"}))
	s.Apply(ev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "execute", "attempt": float64(2), "of": float64(3)}))
	iv := s.Issues["GH-1"]
	if iv.Attempt != 2 || iv.AttemptOf != 3 {
		t.Fatalf("attempts: %+v", iv)
	}
	s.Apply(ev(t, core.EvStageFailed, "GH-1", map[string]any{"stage": "execute", "error": "2 tests failing", "attempt": float64(3), "of": float64(3), "final": true}))
	if iv.LastError != "2 tests failing" || iv.State != "failed" {
		t.Fatalf("error: %+v", iv)
	}
}
```

Engine: extend `TestFailedAgentRetriesThenFails` to scan events and assert the retried stage produced `stage_started` with attempts 1 and 2, and the terminal `stage_failed` has `"final": true`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/projection/ ./internal/engine/ -run 'Attempt|Retries' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Engine `runStage` retry loop: move `stage_started` emission into `runStageOnce`'s caller per attempt (currently emitted inside `runStageOnce` — add `attempt`/`of` params), and enrich failure payloads. Projection cases. Store `LastStageEvents` (query last 20 events for the issue, walk backwards). Server: populate the new IssueDetail fields.

- [ ] **Step 4: Run the full suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/
git commit -m "feat: stage attempt counters and last-error surfacing"
```

---

### Task 4: Pause/resume and kill-stage

**Files:**
- Modify: `internal/engine/engine.go`, `internal/core/event.go`, `internal/proto/proto.go`, `internal/proto/server.go`, `cmd/watchtower/main.go`
- Test: `internal/engine/engine_test.go` (extend)

**Interfaces:**
- Events: `EvIssuePaused "issue_paused"`, `EvIssueResumed "issue_resumed"`, `EvStageKilled "stage_killed"`.
- Engine:
  ```go
  func (e *Engine) Pause(issueID string) error  // takes effect BETWEEN stages
  func (e *Engine) Resume(issueID string) error
  func (e *Engine) KillStage(issueID string) error // cancels the running stage's context now
  ```
  Implementation: `issueState` gains `paused chan struct{}` (nil = not paused; non-nil = closed on resume), `stageCancel context.CancelFunc` (set for the duration of each `runStage`). The stage loop in `StartIssue` checks pause before each stage:
  ```go
  	e.mu.Lock()
  	gate := is.pauseGate
  	e.mu.Unlock()
  	if gate != nil {
  		e.emit(core.EvIssuePaused, id, nil)
  		select {
  		case <-gate:
  			e.emit(core.EvIssueResumed, id, nil)
  		case <-ctx.Done():
  			return ctx.Err()
  		}
  	}
  ```
  `Pause` sets `pauseGate = make(chan struct{})`; `Resume` closes it and nils it. `KillStage` calls `stageCancel()`; the killed stage's error is labeled: wrap `runStage`'s context with `context.WithCancel`, and when the stage returns a context.Canceled error AND a kill was requested (flag on issueState), emit `stage_killed` instead of `stage_failed`, do not consume a retry, leave the issue paused (killing implies "I want to intervene").
- Proto ops: `pause_issue`, `resume_issue`, `kill_stage` (field `IssueID`). CLI: `watchtower pause|resume|kill <issue-id>`.

- [ ] **Step 1: Write the failing test**

```go
func TestPauseGatesBetweenStages(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, _ := e.CreateIssue("p", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)
	if err := e.Pause(id); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- e.StartIssue(context.Background(), id) }()
	// paused before the first stage: no stage_started should appear
	time.Sleep(50 * time.Millisecond)
	evs, _ := s.EventsSince(0)
	for _, ev := range evs {
		if ev.Type == core.EvStageStarted {
			t.Fatal("stage started while paused")
		}
	}
	if err := e.Resume(id); err != nil {
		t.Fatal(err)
	}
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
	evs, _ = s.EventsSince(0)
	var paused, resumed int
	for _, ev := range evs {
		if ev.Type == core.EvIssuePaused {
			paused++
		}
		if ev.Type == core.EvIssueResumed {
			resumed++
		}
	}
	if paused != 1 || resumed != 1 {
		t.Fatalf("paused=%d resumed=%d", paused, resumed)
	}
}

func TestKillStageEmitsKilledAndPauses(t *testing.T) {
	sc := scripts()
	sc["brainstorm/brainstorm"] = runner.Script{Asks: []levers.Decision{
		{Question: "block forever?", Options: []string{"a"}, Recommended: 0, Importance: 1.0}}}
	e, s := newEngine(t, &runner.FakeRunner{Scripts: sc})
	id, _ := e.CreateIssue("k", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)
	errc := make(chan error, 1)
	go func() { errc <- e.StartIssue(context.Background(), id) }()
	// wait until the stage is genuinely running (blocked on its ask)
	for len(e.PendingDecisions()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if err := e.KillStage(id); err != nil {
		t.Fatal(err)
	}
	<-errc // issue run returns (killed)
	evs, _ := s.EventsSince(0)
	killed := false
	for _, ev := range evs {
		if ev.Type == core.EvStageKilled {
			killed = true
		}
	}
	if !killed {
		t.Fatal("no stage_killed event")
	}
}
```

(Note: killing while an Ask is pending must also unblock the escalate — the killed stage's pending decision should be removed from the queue: engine cleans `pend` entries for the issue on kill and replies into the void safely. Implement: on kill, for each pending decision of the issue, delete and `close(reply)`; `escalate` must handle a closed reply channel by treating it as option `-1` and returning promptly; runner treats a `-1`/closed as ctx-cancel path. Simplest: `escalate` selects on `p.reply` and a per-issue kill channel.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/engine/ -run 'Pause|Kill' -race -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

As specced. The kill/escalate interaction is the delicate part — the acceptance is exactly the two tests above passing under `-race`. Wire proto ops + CLI subcommands.

- [ ] **Step 4: Run the full suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ cmd/
git commit -m "feat: pause/resume issues and kill running stages"
```

---

### Task 5: Retry failed stage + set-lever

**Files:**
- Modify: `internal/engine/engine.go`, `internal/core/event.go`, `internal/proto/proto.go`, `internal/proto/server.go`, `cmd/watchtower/main.go`
- Test: `internal/engine/engine_test.go` (extend)

**Interfaces:**
- Engine:
  ```go
  // RetryStage restarts a failed (or killed) issue from its failed stage.
  // Only valid when the issue's StartIssue has returned with an error.
  func (e *Engine) RetryStage(ctx context.Context, issueID string) error
  func (e *Engine) SetLever(issueID, stage string, l flow.Lever) error // emits lever_changed
  ```
  Implementation: `issueState` gains `stageIdx int` (advanced by the loop) and `terminal bool` (set when StartIssue returns error). `StartIssue` refactors its loop body into `runFrom(ctx, is, startIdx)`; `RetryStage` validates `terminal`, clears it, and calls `runFrom(ctx, is, is.stageIdx)` (same goroutine semantics as start: server runs it in a goroutine).
- Event: `EvLeverChanged "lever_changed"` payload `{stage, lever}`.
- Proto ops: `retry_stage` (IssueID), `set_lever` (IssueID, Stage, Lever string — validated). CLI: `watchtower retry <id>`, `watchtower lever <id> <stage> <yolo|regular|strict>`.

- [ ] **Step 1: Write the failing test**

```go
func TestRetryStageResumesFromFailure(t *testing.T) {
	sc := scripts()
	sc["execute/executor"] = runner.Script{Fail: true}
	fr := &runner.FakeRunner{Scripts: sc}
	e, s := newEngine(t, fr)
	id, _ := e.CreateIssue("r", "", "default", levers.Preset(testFlow(), flow.LeverYolo), 0)
	errc := make(chan error, 1)
	go func() { errc <- e.StartIssue(context.Background(), id) }()
	for {
		if ds := e.PendingDecisions(); len(ds) == 1 {
			e.Answer(ds[0].ID, 0)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-errc; err == nil {
		t.Fatal("expected failure")
	}
	// fix the world, then retry
	fr.Scripts["execute/executor"] = runner.Script{Artifacts: map[string]string{"diff": ""}}
	errc2 := make(chan error, 1)
	go func() { errc2 <- e.RetryStage(context.Background(), id) }()
	if err := <-errc2; err != nil {
		t.Fatal(err)
	}
	evs, _ := s.EventsSince(0)
	var completed, brainstormStarts int
	for _, ev := range evs {
		if ev.Type == core.EvStageCompleted {
			completed++
		}
		if ev.Type == core.EvStageStarted {
			var p map[string]any
			json.Unmarshal(ev.Payload, &p)
			if p["stage"] == "brainstorm" {
				brainstormStarts++
			}
		}
	}
	// retry must NOT re-run earlier stages
	if brainstormStarts != 1 {
		t.Fatalf("brainstorm re-ran: %d", brainstormStarts)
	}
	if completed != 4 {
		t.Fatalf("completed=%d", completed)
	}
}

func TestSetLeverEmitsEvent(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, _ := e.CreateIssue("l", "", "default", levers.Preset(testFlow(), flow.LeverStrict), 0)
	if err := e.SetLever(id, "execute", flow.LeverYolo); err != nil {
		t.Fatal(err)
	}
	evs, _ := s.EventsSince(0)
	found := false
	for _, ev := range evs {
		if ev.Type == core.EvLeverChanged {
			found = true
		}
	}
	if !found {
		t.Fatal("no lever_changed event")
	}
}
```

(Import `encoding/json` in the test file if not present.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/engine/ -run 'Retry|SetLever' -race -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

The `runFrom` refactor (workspace acquire must be idempotent — already guarded by `is.wsPath == ""`; the release defer moves to whichever call is running the loop, guard with a `running` flag so retry re-installs it). `SetLever` validates stage exists in the issue's flow and the lever value; mutates the matrix under lock. Proto + CLI wiring.

- [ ] **Step 4: Run the full suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ cmd/
git commit -m "feat: retry failed stages and change levers mid-flight"
```

---

### Task 6: Transcript ring buffer + tail op

**Files:**
- Create: `internal/transcript/transcript.go`
- Modify: `internal/claude/runner.go`, `internal/runner/fake.go` (optional lines), `internal/engine/engine.go` (plumb sink), `internal/proto/proto.go`, `internal/proto/server.go`, `cmd/watchtower/main.go`
- Test: `internal/transcript/transcript_test.go`

**Interfaces:**
- ```go
  package transcript
  // Buffer keeps the last N lines per (issue, stage) key. Safe for concurrent use.
  type Buffer struct{ ... }
  func NewBuffer(perKey int) *Buffer               // e.g. 500 lines per key
  func (b *Buffer) Add(issueID, stage, line string)
  func (b *Buffer) Tail(issueID string, n int) []string // most recent n lines across the issue's stages, oldest first, each prefixed "stage │ "
  ```
- `claude.CodeRunner` gains `OnLine func(issueID, stage, line string)` — called with a human-readable line per stream event: assistant text lines verbatim; tool-ish/other lines skipped; result → `"— turn complete (N tokens) —"`. (Raw JSON is NOT stored — the tail is for humans.)
- `runner.FakeRunner` Script gains `Lines []string`, emitted via the same callback (for tests/demo).
- Daemon: constructs one Buffer, sets `OnLine` on the runner to `buf.Add`, server gains op `transcript_tail` (IssueID, N) → `Lines []string` on Response. CLI: `watchtower transcript <id> [-n 50]`.

- [ ] **Step 1: Write the failing test**

```go
// internal/transcript/transcript_test.go
package transcript

import (
	"strings"
	"testing"
)

func TestBufferTailBoundedAndOrdered(t *testing.T) {
	b := NewBuffer(3)
	for i := 1; i <= 5; i++ {
		b.Add("GH-1", "execute", strings.Repeat("x", i)) // lengths 1..5
	}
	b.Add("GH-1", "review", "rev line")
	b.Add("GH-2", "spec", "other issue")

	got := b.Tail("GH-1", 10)
	if len(got) != 4 { // 3 kept from execute + 1 review
		t.Fatalf("tail: %v", got)
	}
	if !strings.HasPrefix(got[0], "execute │ xxx") || !strings.HasPrefix(got[3], "review │ rev") {
		t.Fatalf("order/prefix wrong: %v", got)
	}
	if len(b.Tail("GH-1", 2)) != 2 {
		t.Fatal("n limit ignored")
	}
	if len(b.Tail("GH-3", 5)) != 0 {
		t.Fatal("unknown issue should be empty")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/transcript/ -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Ring per key (slice + head index) with a global monotonic sequence per line so cross-stage tails interleave in true order; mutex-guarded. Runner `OnLine` calls in the `KindAssistantText` (split text on newlines) and `KindResult` branches, nil-guarded. Fake emits `Lines` before artifacts. Proto op + CLI.

- [ ] **Step 4: Run the full suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ cmd/
git commit -m "feat: per-issue transcript ring buffer with tail op"
```

---

### Task 7: Overview op — the status sentence's data

**Files:**
- Modify: `internal/proto/proto.go`, `internal/proto/server.go`, `internal/store/store.go`, `cmd/watchtower/main.go`
- Test: `internal/proto/proto_test.go` (extend)

**Interfaces:**
- Daemon flag `--price-per-mtok FLOAT` (default 0). Server holds it.
- Proto op `overview` → Response gains:
  ```go
  type Overview struct {
      Building  int     `json:"building"`   // issues in state running*
      NeedYou   int     `json:"need_you"`   // pending decisions
      Failing   int     `json:"failing"`
      Queued    int     `json:"queued"`
      ShippedToday int  `json:"shipped_today"` // issue_merged events since local midnight
      TokensToday  int  `json:"tokens_today"`
      DollarsToday float64 `json:"dollars_today"` // 0 when price unset
  }
  ```
  Computed server-side from `Store.Issues()` states + `PendingDecisionRows()` + events since midnight (`func (s *Store) EventsSinceTime(t time.Time) ([]core.Event, error)` — add it) + `stage_runs` token sums for runs whose issues had events today (approximation: total tokens of all stage_runs is fine for v1 IF labeled "total"; choose: `TokensTotal`/`DollarsTotal` — honest and simpler. Rename fields Total, not Today, and compute ShippedToday from events since midnight which IS cheap and exact).
- Final shape (use this): `Building, NeedYou, Failing, Queued, ShippedToday int; TokensTotal int; DollarsTotal float64`.
- CLI: `watchtower status` printing the sentence: `● 1 failing, 1 question for you — 3 building, 2 shipped today · 41k tokens (~$0.35)` with ● red when Failing>0, gold when NeedYou>0, green otherwise.

- [ ] **Step 1: Write the failing test**

Extend the proto socket test: after driving the issue to completion, call `overview` and assert `ShippedToday == 0` (no merge in fake mode), `Building == 0`, `NeedYou == 0`, `TokensTotal > 0` (fake scripts carry tokens), and with a second decision pending on a fresh issue, `NeedYou == 1`.

```go
	r, _ = c.Do(Command{Op: "overview"})
	if !r.OK || r.Overview == nil {
		t.Fatalf("overview: %+v", r)
	}
	if r.Overview.TokensTotal <= 0 || r.Overview.NeedYou != 0 {
		t.Fatalf("overview values: %+v", r.Overview)
	}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proto/ -run Socket -v` (or the existing test name)
Expected: FAIL.

- [ ] **Step 3: Implement**

`Store.EventsSinceTime` (WHERE at >= ?), `Store.TotalTokens()` (SUM over stage_runs), state-count helper over `Issues()`. Server assembles; price multiplication `float64(tokens)/1e6 * price`. CLI `status` subcommand.

- [ ] **Step 4: Run the full suite + smoke**

Run: `go test ./... -race && go build ./...`; then the Plan 2 stub smoke plus `watchtower status`, `watchtower pause GH-1`/`resume`, `watchtower transcript GH-1` against a live fake daemon.
Expected: PASS; status sentence prints; pause visibly delays the next stage.

- [ ] **Step 5: Commit**

```bash
git add internal/ cmd/
git commit -m "feat: overview op with status counts and cost estimate"
```

---

## Self-review notes

- **Coverage vs the Plan 4b proposal (engine half):** decision schema + coaching (T1 → toast v2, rubber-stamp data comes to the TUI as decision history), evidence bundles + area weights (T2 → evidence panel, building/brushing marks), attempts/errors (T3 → failure block), pause/kill (T4), retry/set-lever (T5 → control verbs, lever display), transcript tail (T6 → `T` door), overview + price (T7 → status sentence, $ figures). TUI-side items (palette, words-in-cells, legend, motion, doors' rendering, `n` modal, shelf, overflow, MAP sentence, plain-language copy, stage aliases) are Plan 4c, which consumes exactly these interfaces.
- **Deliberate simplifications, stated:** tokens/dollars are totals, not daily (only ShippedToday is midnight-scoped); pause is between-stages (mid-stage interruption = kill); Marshal sequencing state remains non-persistent (unchanged from Plan 3); coached decisions cap at 2 attempts then pass through incomplete.
- **Risk flags for the executor:** T4's kill/escalate interaction is the delicate concurrency (acceptance = both tests under `-race`); T5's `runFrom` refactor must keep the workspace-release defer correct across retry re-entry; T2 diffs include uncommitted changes by design (fake runner never commits).
- **Type consistency:** `levers.Decision` v2 fields flow levers→codec→runner→engine→store→proto untouched by name changes ✓; `archmap.AreaOf` shared by evidence + archmap ✓; new proto ops all additive ✓.
