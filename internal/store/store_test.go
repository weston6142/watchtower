package store

import (
	"sync"
	"testing"
	"time"

	"github.com/wbushyeager/guildhall/internal/core"
)

func TestAppendAssignsSeqAndReplays(t *testing.T) {
	s, err := Open("file:t1?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	e1, _ := core.NewEvent(core.EvIssueCreated, "GH-1", map[string]string{"title": "a"})
	e2, _ := core.NewEvent(core.EvStageStarted, "GH-1", map[string]string{"stage": "brainstorm"})
	e1, err = s.Append(e1)
	if err != nil {
		t.Fatal(err)
	}
	e2, _ = s.Append(e2)
	if e2.Seq != e1.Seq+1 {
		t.Fatalf("seq not monotonic: %d then %d", e1.Seq, e2.Seq)
	}
	got, err := s.EventsSince(e1.Seq) // strictly after e1
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Type != core.EvStageStarted {
		t.Fatalf("replay wrong: %+v", got)
	}
}

func TestDecisionOrderingByBlockingCost(t *testing.T) {
	s, _ := Open("file:t3?mode=memory&cache=shared")
	defer s.Close()
	old := time.Now().Add(-time.Hour)
	id1, _ := s.InsertDecision(DecisionRow{IssueID: "GH-1", Question: "small", BlockingCost: 1, CreatedAt: time.Now()})
	id2, _ := s.InsertDecision(DecisionRow{IssueID: "GH-2", Question: "big", BlockingCost: 3, CreatedAt: time.Now()})
	id3, _ := s.InsertDecision(DecisionRow{IssueID: "GH-3", Question: "old-small", BlockingCost: 1, CreatedAt: old})
	rows, err := s.PendingDecisionRows()
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].ID != id2 || rows[1].ID != id3 || rows[2].ID != id1 {
		t.Fatalf("order wrong: %v %v %v", rows[0].ID, rows[1].ID, rows[2].ID)
	}
	if err := s.AnswerDecision(id2, 0, "answered"); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.PendingDecisionRows()
	if len(rows) != 2 {
		t.Fatalf("answered row still pending: %v", rows)
	}
}

func TestProposalLifecycle(t *testing.T) {
	s, _ := Open("file:t4?mode=memory&cache=shared")
	defer s.Close()
	id, err := s.InsertProposal("GH-1", "New task", "details")
	if err != nil {
		t.Fatal(err)
	}
	ps, _ := s.PendingProposals()
	if len(ps) != 1 || ps[0].Title != "New task" {
		t.Fatalf("pending: %+v", ps)
	}
	s.SetProposalStatus(id, "accepted")
	if ps, _ = s.PendingProposals(); len(ps) != 0 {
		t.Fatalf("still pending: %+v", ps)
	}
}

func TestConcurrentDecisionAndEventWrites(t *testing.T) {
	s, _ := Open("file:t5?mode=memory&cache=shared")
	defer s.Close()
	_, _ = s.db.Exec("PRAGMA busy_timeout = 0")
	s.db.SetMaxOpenConns(8)
	var wg sync.WaitGroup
	errs := make(chan error, 512)
	start := make(chan struct{})
	for i := 0; i < 256; i++ {
		i := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.InsertDecision(DecisionRow{IssueID: "GH-1", Question: "q", BlockingCost: i})
			if err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			ev, err := core.NewEvent(core.EvStageStarted, "GH-1", map[string]int{"n": i})
			if err == nil {
				_, err = s.Append(ev)
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	rows, err := s.PendingDecisionRows()
	if err != nil || len(rows) != 256 {
		t.Fatalf("decisions=%d err=%v", len(rows), err)
	}
	events, err := s.EventsSince(0)
	if err != nil || len(events) != 256 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
}

func TestStageRunLifecycle(t *testing.T) {
	s, err := Open("file:t2?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, err := s.InsertStageRun(StageRun{IssueID: "GH-1", Stage: "execute", Agent: "executor", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStageRun(id, "succeeded", "sess-abc", 1234); err != nil {
		t.Fatal(err)
	}
	runs, err := s.StageRuns("GH-1")
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs: %v %v", runs, err)
	}
	r := runs[0]
	if r.Status != "succeeded" || r.SessionID != "sess-abc" || r.Tokens != 1234 {
		t.Fatalf("bad run: %+v", r)
	}
	tok, _ := s.IssueTokens("GH-1")
	if tok != 1234 {
		t.Fatalf("tokens: %d", tok)
	}
}

func TestIssueLeversPersistWithIssue(t *testing.T) {
	s, err := Open("file:t6?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.UpsertIssue(IssueRow{
		ID: "GH-1", Title: "lever test", Flow: "default", State: "running",
		Levers: map[string]string{"spec": "strict"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIssueLever("GH-1", "execute", "yolo"); err != nil {
		t.Fatal(err)
	}
	issues, err := s.Issues()
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Levers["spec"] != "strict" || issues[0].Levers["execute"] != "yolo" {
		t.Fatalf("levers were not persisted: %+v", issues)
	}
}
