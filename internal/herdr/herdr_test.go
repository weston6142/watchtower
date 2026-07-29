package herdr

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
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

// sockPath returns a Unix socket path short enough for macOS's 104-byte
// sun_path limit; t.TempDir() embeds the test name and can exceed it.
func sockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "h.sock")
}

func startFakeHerdr(t *testing.T) (*fakeHerdr, string) {
	t.Helper()
	path := sockPath(t)
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
	path := sockPath(t)
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
