package herdr

import (
	"bufio"
	"encoding/json"
	"net"
	"sync"
	"testing"
)

type fakeSpawnServer struct {
	ln      net.Listener
	errResp bool

	mu   sync.Mutex
	reqs []map[string]any
}

func startFakeSpawnServer(t *testing.T) (*fakeSpawnServer, string) {
	t.Helper()
	path := sockPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSpawnServer{ln: ln}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f, path
}

func startFakeErrorServer(t *testing.T) string {
	t.Helper()
	path := sockPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSpawnServer{ln: ln, errResp: true}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return path
}

func (f *fakeSpawnServer) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			sc := bufio.NewScanner(c)
			if !sc.Scan() {
				return
			}
			var req map[string]any
			if json.Unmarshal(sc.Bytes(), &req) != nil {
				return
			}
			f.mu.Lock()
			f.reqs = append(f.reqs, req)
			f.mu.Unlock()

			if f.errResp {
				_, _ = c.Write([]byte(`{"id":"x","error":{"message":"no such workspace"}}` + "\n"))
				return
			}
			method, _ := req["method"].(string)
			response := `{"id":"x","result":{}}`
			if method == "pane.split" {
				response = `{"id":"x","result":{"pane_id":"w1:p9"}}`
			}
			_, _ = c.Write([]byte(response + "\n"))
		}(conn)
	}
}

func (f *fakeSpawnServer) requests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.reqs...)
}

func TestSpawnPane(t *testing.T) {
	f, sock := startFakeSpawnServer(t)
	r := New(sock, "w1:p1")
	if err := r.SpawnPane("/repo", "exec claude"); err != nil {
		t.Fatalf("SpawnPane: %v", err)
	}
	got := f.requests()
	if len(got) != 2 {
		t.Fatalf("requests = %d, want 2: %v", len(got), got)
	}
	if got[0]["method"] != "pane.split" {
		t.Fatalf("first request = %v, want pane.split", got[0]["method"])
	}
	p := got[0]["params"].(map[string]any)
	if p["cwd"] != "/repo" || p["direction"] != "right" || p["focus"] != true || p["target_pane_id"] != "w1:p1" {
		t.Fatalf("split params = %v", p)
	}
	if got[1]["method"] != "pane.send_text" {
		t.Fatalf("second request = %v, want pane.send_text", got[1]["method"])
	}
	sp := got[1]["params"].(map[string]any)
	if sp["pane_id"] != "w1:p9" || sp["text"] != "exec claude\n" {
		t.Fatalf("send_text params = %v", sp)
	}
}

func TestSpawnPaneDisabled(t *testing.T) {
	if err := New("", "").SpawnPane("/repo", "x"); err == nil {
		t.Fatal("disabled reporter must return an error, not nil")
	}
}

func TestSpawnPaneErrorResponse(t *testing.T) {
	sock := startFakeErrorServer(t)
	if err := New(sock, "w1:p1").SpawnPane("/repo", "x"); err == nil {
		t.Fatal("error response must surface")
	}
}

func TestSpawnPaneNilReporter(t *testing.T) {
	var r *Reporter
	if err := r.SpawnPane("/repo", "x"); err == nil {
		t.Fatal("nil reporter must return an error")
	}
}

func TestSpawnPaneUsesNestedPaneID(t *testing.T) {
	if got := paneIDFrom(map[string]any{"pane": map[string]any{"pane_id": "w1:p9"}}); got != "w1:p9" {
		t.Fatalf("nested pane id = %q, want w1:p9", got)
	}
}
