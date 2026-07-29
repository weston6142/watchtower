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
