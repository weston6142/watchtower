package proto

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/engine"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/slots"
	"github.com/weston6142/watchtower/internal/store"
)

func newTestClient(t *testing.T) *Client {
	t.Helper()
	f := flow.Flow{
		Name: "default",
		Stages: []flow.Stage{{
			Name:   "run",
			Agents: []flow.AgentRef{{Package: "agent"}},
			Gate:   flow.GateAuto,
		}},
	}
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e := engine.New(engine.Config{
		Store: s,
		Runner: &runner.FakeRunner{Scripts: map[string]runner.Script{
			"run/agent": {},
		}},
		Pool:    slots.NewPool(1),
		Flows:   map[string]flow.Flow{"default": f},
		DataDir: t.TempDir(),
	})
	sock := filepath.Join(t.TempDir(), "g.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(e, s)
	srv.SetFlows(map[string]flow.Flow{"default": f})
	go srv.Serve(l)
	t.Cleanup(func() { l.Close() })
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestCreateAnswerAndTailOverSocket(t *testing.T) {
	f, err := flow.Load("../flow/testdata/default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := store.Open("file:proto?mode=memory&cache=shared")
	defer s.Close()
	fr := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"brainstorm/brainstorm":      {},
		"spec/spec-writer":           {Artifacts: map[string]string{"spec.md": ""}},
		"execute/executor":           {Artifacts: map[string]string{"diff": ""}, Tokens: 10},
		"review/clean-code-reviewer": {Artifacts: map[string]string{"review.md": ""}},
		"review/reviewer":            {},
		"review/doc-writer":          {Artifacts: map[string]string{"docs": ""}},
	}}
	e := engine.New(engine.Config{Store: s, Runner: fr, Pool: slots.NewPool(2),
		Flows: map[string]flow.Flow{"default": f}, DataDir: t.TempDir()})
	_ = levers.Rules{}

	sock := filepath.Join(t.TempDir(), "g.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(e, s)
	srv.SetFlows(map[string]flow.Flow{"default": f})
	go srv.Serve(l)

	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	r, err := c.Do(Command{Op: "create_issue", Title: "hi", Flow: "default", Preset: "yolo"})
	if err != nil || !r.OK || r.IssueID == "" {
		t.Fatalf("create failed: %+v %v", r, err)
	}
	id := r.IssueID
	if r, _ = c.Do(Command{Op: "start_issue", IssueID: id}); !r.OK {
		t.Fatalf("start failed: %+v", r)
	}

	// spec gate escalates even on yolo — answer it
	deadline := time.After(5 * time.Second)
	for {
		r, _ = c.Do(Command{Op: "list_decisions"})
		if len(r.Decisions) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no decision appeared")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if r, _ = c.Do(Command{Op: "answer_decision", DecisionID: r.Decisions[0].ID, Option: 0}); !r.OK {
		t.Fatalf("answer failed: %+v", r)
	}

	// tail until issue completes all 4 stages
	deadline = time.After(5 * time.Second)
	completed := 0
	var since int64
	for completed < 4 {
		r, _ = c.Do(Command{Op: "tail", SinceSeq: since})
		for _, ev := range r.Events {
			since = ev.Seq
			if ev.Type == core.EvStageCompleted {
				completed++
			}
		}
		select {
		case <-deadline:
			t.Fatalf("only %d stages completed", completed)
		case <-time.After(10 * time.Millisecond):
		}
	}

	r, err = c.Do(Command{Op: "issue_detail", IssueID: id})
	if err != nil || !r.OK || r.Detail == nil || r.Detail.Issue.ID != id {
		t.Fatalf("issue detail failed: %+v %v", r, err)
	}
	if len(r.Detail.Runs) != 6 {
		t.Fatalf("want 6 stage runs, got %d", len(r.Detail.Runs))
	}
	r, _ = c.Do(Command{Op: "overview"})
	if !r.OK || r.Overview == nil {
		t.Fatalf("overview: %+v", r)
	}
	if r.Overview.ShippedToday != 0 || r.Overview.Building != 0 || r.Overview.NeedYou != 0 || r.Overview.TokensTotal <= 0 {
		t.Fatalf("overview values: %+v", r.Overview)
	}

	r, _ = c.Do(Command{Op: "create_issue", Title: "pending", Flow: "default", Preset: "yolo"})
	if !r.OK {
		t.Fatalf("second create failed: %+v", r)
	}
	second := r.IssueID
	if r, _ = c.Do(Command{Op: "start_issue", IssueID: second}); !r.OK {
		t.Fatalf("second start failed: %+v", r)
	}
	deadline = time.After(5 * time.Second)
	for {
		r, _ = c.Do(Command{Op: "overview"})
		if r.Overview != nil && r.Overview.NeedYou == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("second decision never appeared: %+v", r.Overview)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestBacklogOps(t *testing.T) {
	c := newTestClient(t)
	r, err := c.Do(Command{Op: "draft_issue", Title: "t", Body: "b", Flow: "default", Preset: "regular", Priority: 2})
	if err != nil || !r.OK {
		t.Fatalf("draft_issue: %v %+v", err, r)
	}
	id := r.IssueID

	r, err = c.Do(Command{Op: "update_issue", IssueID: id, Title: "t2", Body: "b2", Flow: "default", Preset: "strict", Priority: 5})
	if err != nil || !r.OK {
		t.Fatalf("update_issue: %v %+v", err, r)
	}

	r, _ = c.Do(Command{Op: "list_issues"})
	found := false
	for _, row := range r.Issues {
		if row.ID == id {
			found = true
			if row.State != "backlog" || row.Title != "t2" || row.Priority != 5 || row.Body != "b2" {
				t.Fatalf("row wrong after update: %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("draft missing from list_issues")
	}

	r, err = c.Do(Command{Op: "launch_issue", IssueID: id})
	if err != nil || !r.OK {
		t.Fatalf("launch_issue: %v %+v", err, r)
	}
	r, _ = c.Do(Command{Op: "launch_issue", IssueID: id})
	if r.OK {
		t.Fatal("second launch succeeded")
	}
}

func TestDraftNotCountedInOverview(t *testing.T) {
	c := newTestClient(t)
	if r, err := c.Do(Command{Op: "draft_issue", Title: "t", Flow: "default", Preset: "regular"}); err != nil || !r.OK {
		t.Fatalf("draft_issue: %v %+v", err, r)
	}
	r, err := c.Do(Command{Op: "overview"})
	if err != nil || !r.OK {
		t.Fatal(err)
	}
	if r.Overview.Building != 0 || r.Overview.Failing != 0 || r.Overview.Queued != 0 {
		t.Fatalf("draft counted in overview: %+v", r.Overview)
	}
}

func TestOverviewIgnoresTrailingFailureForAbandonedIssue(t *testing.T) {
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.UpsertIssue(store.IssueRow{
		ID: "GH-3", Title: "abandoned", State: "abandoned", Flow: "default",
	}); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []core.EventType{core.EvIssueAbandoned, core.EvStageFailed} {
		event, err := core.NewEvent(typ, "GH-3", map[string]string{"stage": "merge"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Append(event); err != nil {
			t.Fatal(err)
		}
	}

	socketDir, err := os.MkdirTemp("", "overview")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	sock := filepath.Join(socketDir, "watchtower.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go NewServer(nil, s).Serve(listener)

	client, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	response, err := client.Do(Command{Op: "overview"})
	if err != nil || !response.OK || response.Overview == nil {
		t.Fatalf("overview: %v %+v", err, response)
	}
	if response.Overview.Failing != 0 || response.Overview.Building != 0 || response.Overview.NeedYou != 0 {
		t.Fatalf("abandoned issue counted in overview: %+v", response.Overview)
	}
}
