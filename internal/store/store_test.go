package store

import (
	"sync"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/levers"
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
	if err := s.AnswerDecision(id2, levers.ChoiceResponse(0), "answered"); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.PendingDecisionRows()
	if len(rows) != 2 {
		t.Fatalf("answered row still pending: %v", rows)
	}
}

func TestDecisionRoundTripsTypedFreeformResponse(t *testing.T) {
	s, err := Open("file:typed-decisions?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id, err := s.InsertDecision(DecisionRow{
		IssueID:             "GH-1",
		Stage:               "spec",
		Kind:                levers.DecisionFreeform,
		Question:            "Review spec.md",
		RecommendedResponse: "Approve spec.md as written.",
		Importance:          0.8,
		Paths:               []string{"spec.md"},
		Why:                 "It matches the approved design.",
		Consequences:        []string{"Planning begins."},
		Reversible:          "yes",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := levers.FreeformResponse("Clarify the rollout before approval.")
	if err := s.AnswerDecision(id, response, "answered"); err != nil {
		t.Fatal(err)
	}

	rows, err := s.AllDecisionRows()
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %#v, err = %v", rows, err)
	}
	got := rows[0]
	if got.Kind != levers.DecisionFreeform ||
		got.RecommendedResponse != "Approve spec.md as written." ||
		got.Importance != 0.8 || len(got.Paths) != 1 ||
		got.Response.Kind != levers.DecisionFreeform ||
		got.Response.Text != response.Text {
		t.Fatalf("decision = %#v", got)
	}
}

func TestProposalLifecycle(t *testing.T) {
	s, _ := Open("file:t4?mode=memory&cache=shared")
	defer s.Close()
	id, err := s.InsertProposal("GH-1", "New task", "details", []string{"GH-2", "GH-3"})
	if err != nil {
		t.Fatal(err)
	}
	ps, _ := s.PendingProposals()
	if len(ps) != 1 || ps[0].Title != "New task" ||
		len(ps[0].DependsOn) != 2 || ps[0].DependsOn[1] != "GH-3" {
		t.Fatalf("pending: %+v", ps)
	}
	s.SetProposalStatus(id, "accepted")
	if ps, _ = s.PendingProposals(); len(ps) != 0 {
		t.Fatalf("still pending: %+v", ps)
	}
}

func TestProposalBatchLifecycle(t *testing.T) {
	s, _ := Open("file:proposal-batch?mode=memory&cache=shared")
	defer s.Close()
	batchID, err := s.InsertProposalBatch("GH-1", []ProposalRow{
		{Key: "api", Title: "Add API"},
		{Key: "consumer", Title: "Use API", DependsOn: []string{"api"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ps, err := s.PendingProposals()
	if err != nil || len(ps) != 2 || ps[0].BatchID != batchID ||
		ps[1].BatchID != batchID || ps[1].Key != "consumer" ||
		len(ps[1].DependsOn) != 1 || ps[1].DependsOn[0] != "api" {
		t.Fatalf("pending batch: %+v err=%v", ps, err)
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

func TestDependencyReplacementAndReverseLookup(t *testing.T) {
	s, err := Open("file:dependencies?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"GH-1", "GH-2", "GH-3"} {
		if err := s.UpsertIssue(IssueRow{ID: id, Flow: "default", State: "backlog"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ReplaceDependencies("GH-2", []string{"GH-1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceDependencies("GH-3", []string{"GH-1", "GH-2"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Dependencies("GH-3"); len(got) != 2 || got[0] != "GH-1" || got[1] != "GH-2" {
		t.Fatalf("dependencies = %v", got)
	}
	if got, _ := s.Dependents("GH-1"); len(got) != 2 || got[0] != "GH-2" || got[1] != "GH-3" {
		t.Fatalf("dependents = %v", got)
	}
	if err := s.ReplaceDependencies("GH-3", []string{"GH-2"}); err != nil {
		t.Fatal(err)
	}
	issues, err := s.Issues()
	if err != nil {
		t.Fatal(err)
	}
	for _, issue := range issues {
		if issue.ID == "GH-3" && (len(issue.DependsOn) != 1 || issue.DependsOn[0] != "GH-2") {
			t.Fatalf("issue dependencies = %v", issue.DependsOn)
		}
	}
}

func attachmentFixture(issueID string) []AttachmentRow {
	now := time.Unix(1700000000, 0).UTC()
	return []AttachmentRow{
		{IssueID: issueID, Name: "app.log", Size: 2048, SourcePath: "/tmp/app.log", AddedAt: now, Ord: 0},
		{IssueID: issueID, Name: "shot.png", Size: 4096, SourcePath: "/tmp/shot.png", AddedAt: now, Ord: 1},
	}
}

func TestAttachmentsRoundTrip(t *testing.T) {
	s, err := Open("file:attach1?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := attachmentFixture("GH-1")
	if err := s.ReplaceAttachments("GH-1", want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Attachments("GH-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows", len(got))
	}
	for i, row := range got {
		if row.Name != want[i].Name || row.Size != want[i].Size ||
			row.SourcePath != want[i].SourcePath || row.Ord != i ||
			!row.AddedAt.Equal(want[i].AddedAt) {
			t.Fatalf("row %d = %+v, want %+v", i, row, want[i])
		}
	}
}

func TestReplaceAttachmentsIsFullReplacement(t *testing.T) {
	s, _ := Open("file:attach2?mode=memory&cache=shared")
	defer s.Close()
	if err := s.ReplaceAttachments("GH-1", attachmentFixture("GH-1")); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceAttachments("GH-1", attachmentFixture("GH-1")[:1]); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Attachments("GH-1")
	if len(got) != 1 || got[0].Name != "app.log" {
		t.Fatalf("stale rows survived replacement: %+v", got)
	}
}

// UpsertIssue overwrites every column of issues; attachments live in their own
// table precisely so they stay out of that blast radius.
func TestUpsertIssueLeavesAttachments(t *testing.T) {
	s, _ := Open("file:attach3?mode=memory&cache=shared")
	defer s.Close()
	if err := s.ReplaceAttachments("GH-1", attachmentFixture("GH-1")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertIssue(IssueRow{ID: "GH-1", Title: "t", State: "backlog", Flow: "default"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Attachments("GH-1"); len(got) != 2 {
		t.Fatalf("UpsertIssue disturbed attachments: %+v", got)
	}
}

func TestDeleteAttachments(t *testing.T) {
	s, _ := Open("file:attach4?mode=memory&cache=shared")
	defer s.Close()
	if err := s.ReplaceAttachments("GH-1", attachmentFixture("GH-1")); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceAttachments("GH-2", attachmentFixture("GH-2")); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAttachments("GH-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Attachments("GH-1"); len(got) != 0 {
		t.Fatalf("rows survived delete: %+v", got)
	}
	if got, _ := s.Attachments("GH-2"); len(got) != 2 {
		t.Fatalf("delete hit the wrong issue: %+v", got)
	}
}
