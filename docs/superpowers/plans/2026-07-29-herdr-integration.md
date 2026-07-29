# Herdr Integration Implementation Plan (GH-7)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The watchtower TUI reports itself to herdr as an agent named "watchtower" — `blocked` when anything needs the user, `working` when lanes are building, `idle` otherwise — so herdr's sidebar and native blocked notifications cover watchtower.

**Architecture:** A new `internal/herdr` package holds a `Reporter` that writes NDJSON requests (`pane.report_agent`, `pane.report_metadata`) over the Unix socket herdr injects via `HERDR_SOCKET_PATH`/`HERDR_PANE_ID`. The TUI feeds it every overview snapshot (level-triggered); the Reporter dedupes and swallows all errors. Spec: `docs/superpowers/specs/2026-07-29-herdr-integration-design.md`.

**Tech Stack:** Go, stdlib only (`net`, `encoding/json`). Tests use a fake Unix socket server in `t.TempDir()`.

## Global Constraints

- Never block or crash the TUI on herdr errors: 500ms socket timeout, all errors swallowed.
- With `HERDR_SOCKET_PATH` or `HERDR_PANE_ID` unset, every Reporter method is a no-op.
- Wire values exactly: `source: "custom:watchtower"`, `agent: "watchtower"`, states `idle|working|blocked`.
- Verified against herdr socket schema (protocol 16): `pane.report_agent` requires `pane_id, source, agent, state`; optional `message`, `seq` (uint64). `pane.report_metadata` takes `pane_id, source, display_agent, seq`.

---

### Task 1: `internal/herdr` Reporter

**Files:**
- Create: `internal/herdr/herdr.go`
- Test: `internal/herdr/herdr_test.go`

**Interfaces:**
- Produces: `herdr.New(socketPath, paneID string) *Reporter`, `herdr.NewFromEnv() *Reporter`, `(*Reporter).Report(needYou, failing, building int)`, `(*Reporter).Idle()`. All methods nil-safe and no-ops when disabled. Task 2 consumes these.

- [ ] **Step 1: Write the failing tests**

`internal/herdr/herdr_test.go`:

```go
package herdr

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeHerdr accepts connections on a Unix socket and records each NDJSON
// request line it receives.
type fakeHerdr struct {
	ln net.Listener

	mu   sync.Mutex
	reqs []map[string]any
}

func startFakeHerdr(t *testing.T) (*fakeHerdr, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "herdr.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeHerdr{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					var req map[string]any
					if json.Unmarshal(sc.Bytes(), &req) == nil {
						f.mu.Lock()
						f.reqs = append(f.reqs, req)
						f.mu.Unlock()
					}
					c.Write([]byte(`{"ok":true}` + "\n"))
				}
			}(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f, path
}

func (f *fakeHerdr) requests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.reqs...)
}

func params(req map[string]any) map[string]any {
	p, _ := req["params"].(map[string]any)
	return p
}

func TestDisabledWithoutEnv(t *testing.T) {
	// Must not panic or write anywhere.
	var nilR *Reporter
	nilR.Report(1, 0, 0)
	nilR.Idle()
	r := New("", "")
	r.Report(1, 0, 0)
	r.Idle()
}

func TestReportsBlockedWithDisplayName(t *testing.T) {
	f, path := startFakeHerdr(t)
	r := New(path, "w1:p1")
	r.Report(2, 1, 4)

	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("want 2 requests (metadata + report), got %d: %v", len(reqs), reqs)
	}
	if reqs[0]["method"] != "pane.report_metadata" {
		t.Fatalf("first request = %v, want pane.report_metadata", reqs[0]["method"])
	}
	if got := params(reqs[0])["display_agent"]; got != "watchtower" {
		t.Fatalf("display_agent = %v", got)
	}
	if reqs[1]["method"] != "pane.report_agent" {
		t.Fatalf("second request = %v, want pane.report_agent", reqs[1]["method"])
	}
	p := params(reqs[1])
	if p["pane_id"] != "w1:p1" || p["source"] != "custom:watchtower" || p["agent"] != "watchtower" {
		t.Fatalf("identity params = %v", p)
	}
	if p["state"] != "blocked" || p["message"] != "3 need you" {
		t.Fatalf("state/message = %v/%v, want blocked/3 need you", p["state"], p["message"])
	}
}

func TestStateMapping(t *testing.T) {
	cases := []struct {
		needYou, failing, building int
		state, message             string
	}{
		{1, 0, 0, "blocked", "1 need you"},
		{0, 2, 5, "blocked", "2 need you"},
		{0, 0, 2, "working", "2 building"},
		{0, 0, 0, "idle", ""},
	}
	for _, c := range cases {
		f, path := startFakeHerdr(t)
		r := New(path, "w1:p1")
		r.Report(c.needYou, c.failing, c.building)
		reqs := f.requests()
		p := params(reqs[len(reqs)-1])
		if p["state"] != c.state {
			t.Errorf("(%d,%d,%d) state = %v, want %s", c.needYou, c.failing, c.building, p["state"], c.state)
		}
		msg, _ := p["message"].(string)
		if msg != c.message {
			t.Errorf("(%d,%d,%d) message = %q, want %q", c.needYou, c.failing, c.building, msg, c.message)
		}
	}
}

func TestDedupesUnchangedState(t *testing.T) {
	f, path := startFakeHerdr(t)
	r := New(path, "w1:p1")
	r.Report(1, 0, 0)
	r.Report(1, 0, 0) // identical: no new write
	r.Report(2, 0, 0) // message changed: writes
	reqs := f.requests()
	// metadata + blocked(1) + blocked(2) = 3
	if len(reqs) != 3 {
		t.Fatalf("want 3 requests, got %d: %v", len(reqs), reqs)
	}
	if got := params(reqs[2])["message"]; got != "2 need you" {
		t.Fatalf("third request message = %v", got)
	}
}

func TestRetriesAfterFailedWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr.sock")
	r := New(path, "w1:p1")
	r.Report(1, 0, 0) // no server listening: swallowed

	// Server comes up; the same state must be re-sent because the failed
	// write cleared the dedupe memory.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeHerdr{ln: ln}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			var req map[string]any
			if json.Unmarshal(sc.Bytes(), &req) == nil {
				f.mu.Lock()
				f.reqs = append(f.reqs, req)
				f.mu.Unlock()
			}
			conn.Write([]byte(`{"ok":true}` + "\n"))
		}
	}()

	r.Report(1, 0, 0)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, req := range f.requests() {
			if req["method"] == "pane.report_agent" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("state was not re-sent after a failed write")
}

func TestIdleSendsIdle(t *testing.T) {
	f, path := startFakeHerdr(t)
	r := New(path, "w1:p1")
	r.Report(1, 0, 0)
	r.Idle()
	reqs := f.requests()
	p := params(reqs[len(reqs)-1])
	if p["state"] != "idle" {
		t.Fatalf("last state = %v, want idle", p["state"])
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/herdr/`
Expected: FAIL — package does not exist / `New` undefined.

- [ ] **Step 3: Implement the Reporter**

`internal/herdr/herdr.go`:

```go
// Package herdr reports watchtower's agent state to the herdr terminal
// workspace manager over its local socket API, so the TUI's pane shows up
// in herdr's sidebar as an agent named "watchtower" and herdr's native
// notifications fire when watchtower is blocked on the user.
package herdr

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

const (
	source  = "custom:watchtower"
	agent   = "watchtower"
	timeout = 500 * time.Millisecond
)

// Reporter sends pane.report_agent requests for the herdr pane hosting the
// TUI. A Reporter without a socket path and pane id (i.e. not running under
// herdr) is disabled: every method is a no-op. All errors are swallowed —
// herdr reporting must never block or crash the TUI.
type Reporter struct {
	socketPath string
	paneID     string

	mu        sync.Mutex
	last      string // state+message of the last successful report
	announced bool   // display_agent metadata already sent
}

// NewFromEnv builds a Reporter from the HERDR_SOCKET_PATH and HERDR_PANE_ID
// variables herdr injects into panes.
func NewFromEnv() *Reporter {
	return New(os.Getenv("HERDR_SOCKET_PATH"), os.Getenv("HERDR_PANE_ID"))
}

func New(socketPath, paneID string) *Reporter {
	return &Reporter{socketPath: socketPath, paneID: paneID}
}

func (r *Reporter) enabled() bool {
	return r != nil && r.socketPath != "" && r.paneID != ""
}

// Report maps an overview snapshot to a herdr agent state and sends it when
// it differs from the last successful report. Level-triggered: every
// overview recomputes the state from scratch, so a missed transition is
// corrected on the next poll.
func (r *Reporter) Report(needYou, failing, building int) {
	state, message := stateFor(needYou, failing, building)
	r.send(state, message)
}

// Idle reports the idle state unconditionally; called on TUI shutdown so a
// closed TUI doesn't leave a stale blocked pane in the sidebar.
func (r *Reporter) Idle() {
	r.send("idle", "")
}

func stateFor(needYou, failing, building int) (state, message string) {
	if n := needYou + failing; n > 0 {
		return "blocked", fmt.Sprintf("%d need you", n)
	}
	if building > 0 {
		return "working", fmt.Sprintf("%d building", building)
	}
	return "idle", ""
}

func (r *Reporter) send(state, message string) {
	if !r.enabled() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := state + "\x00" + message
	if key == r.last {
		return
	}
	if !r.announced {
		if r.write("pane.report_metadata", map[string]any{
			"pane_id":       r.paneID,
			"source":        source,
			"display_agent": agent,
			"seq":           time.Now().UnixNano(),
		}) != nil {
			r.last = ""
			return
		}
		r.announced = true
	}
	params := map[string]any{
		"pane_id": r.paneID,
		"source":  source,
		"agent":   agent,
		"state":   state,
		"seq":     time.Now().UnixNano(),
	}
	if message != "" {
		params["message"] = message
	}
	if r.write("pane.report_agent", params) != nil {
		// Clear the dedupe memory so the next overview retries.
		r.last = ""
		return
	}
	r.last = key
}

// write sends one NDJSON request and waits briefly for the response line.
func (r *Reporter) write(method string, params map[string]any) error {
	conn, err := net.DialTimeout("unix", r.socketPath, timeout)
	if err != nil {
		return err
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
		return err
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return err
	}
	// Best-effort read so the server isn't left with an unread response;
	// the reply content is ignored.
	bufio.NewReader(conn).ReadString('\n')
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/herdr/`
Expected: PASS (all 6 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/herdr/
git commit -m "feat: herdr Reporter reports watchtower agent state over the socket API"
```

---

### Task 2: Wire the Reporter into the TUI

**Files:**
- Modify: `internal/tui/app.go` (Model struct ~line 173, `overviewMsg` case ~line 240, add setter near `SetReducedMotion` ~line 189)
- Modify: `cmd/watchtower/main.go` (`case "tower"` ~lines 94–115)
- Test: `internal/tui/app_test.go`

**Interfaces:**
- Consumes: `herdr.NewFromEnv() *Reporter`, `(*Reporter).Report(needYou, failing, building int)`, `(*Reporter).Idle()` from Task 1.
- Produces: `(*Model).SetHerdrReporter(overviewReporter)` where `overviewReporter` is a one-method interface defined in `internal/tui/app.go` (lets tests use a spy without a socket).

- [ ] **Step 1: Write the failing test**

Add to `internal/tui/app_test.go` (match the file's existing test style and helpers for constructing a `Model`):

```go
type spyReporter struct {
	calls [][3]int
}

func (s *spyReporter) Report(needYou, failing, building int) {
	s.calls = append(s.calls, [3]int{needYou, failing, building})
}

func TestOverviewUpdateFeedsHerdrReporter(t *testing.T) {
	m := NewModel(nil, []string{"spec", "execute"})
	spy := &spyReporter{}
	m.SetHerdrReporter(spy)

	next, _ := m.Update(overviewMsg{overview: &proto.Overview{NeedYou: 2, Failing: 1, Building: 4}})
	m = next.(Model)

	if len(spy.calls) != 1 || spy.calls[0] != [3]int{2, 1, 4} {
		t.Fatalf("reporter calls = %v, want [[2 1 4]]", spy.calls)
	}

	// An errored overview poll must not report.
	m.Update(overviewMsg{err: errors.New("boom")})
	if len(spy.calls) != 1 {
		t.Fatalf("reporter called on overview error: %v", spy.calls)
	}
}
```

(Add `"errors"` and `proto` imports if the test file doesn't already have them.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run TestOverviewUpdateFeedsHerdrReporter -v`
Expected: FAIL — `SetHerdrReporter` undefined.

- [ ] **Step 3: Implement the TUI wiring**

In `internal/tui/app.go`:

Add to the `Model` struct (alongside the other unexported fields like `client`, `aliases`):

```go
	herdrReporter overviewReporter
```

Add near `SetReducedMotion` (~line 189):

```go
// overviewReporter receives every overview snapshot; satisfied by
// *herdr.Reporter. An interface so tests can substitute a spy.
type overviewReporter interface {
	Report(needYou, failing, building int)
}

func (m *Model) SetHerdrReporter(r overviewReporter) { m.herdrReporter = r }
```

In the `case overviewMsg:` branch (~line 240), after `m.Overview = msg.overview`:

```go
		if m.herdrReporter != nil && msg.overview != nil {
			m.herdrReporter.Report(msg.overview.NeedYou, msg.overview.Failing, msg.overview.Building)
		}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tui/ -run TestOverviewUpdateFeedsHerdrReporter -v`
Expected: PASS

- [ ] **Step 5: Wire the tower command and shutdown**

In `cmd/watchtower/main.go`, `case "tower"` (~line 105), after `model := tui.NewModel(c, r.FlowStages)`:

```go
		reporter := herdr.NewFromEnv()
		model.SetHerdrReporter(reporter)
```

And replace the program run (~line 113) so idle is reported after the TUI exits:

```go
		_, runErr := tea.NewProgram(model, tea.WithAltScreen()).Run()
		reporter.Idle()
		if runErr != nil {
			fatal(runErr)
		}
```

Add `"github.com/weston6142/watchtower/internal/herdr"` to the imports.

- [ ] **Step 6: Run the full test suite and build**

Run: `go build ./... && go test ./...`
Expected: everything passes.

- [ ] **Step 7: Commit**

```bash
git add internal/tui/app.go internal/tui/app_test.go cmd/watchtower/main.go
git commit -m "feat: TUI reports watchtower agent state to herdr (GH-7)"
```

---

## Manual Verification (after both tasks)

1. Rebuild/install watchtower (see the `rebuilding-watchtower` skill).
2. Inside a herdr pane: `watchtower tower` — the herdr sidebar should show an agent named "watchtower".
3. Launch a lane that hits a decision — the sidebar should flip to blocked ("N need you") and herdr should notify.
4. Answer the decision — state returns to working/idle.
5. Quit the TUI — the pane's agent state returns to idle.
