package engine

import (
	"testing"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/slots"
	"github.com/weston6142/watchtower/internal/steward"
	"github.com/weston6142/watchtower/internal/store"
)

func newTestEngine(t *testing.T) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sw := &steward.Steward{Store: st}
	e := New(Config{
		Store:     st,
		Runner:    &runner.FakeRunner{Scripts: scripts()},
		Pool:      slots.NewPool(2),
		Flows:     map[string]flow.Flow{"default": testFlow()},
		DataDir:   t.TempDir(),
		Observers: []func(core.Event){sw.Observe},
	})
	return e, st
}

func newEngineOver(t *testing.T, st *store.Store) *Engine {
	t.Helper()
	sw := &steward.Steward{Store: st}
	return New(Config{
		Store:     st,
		Runner:    &runner.FakeRunner{Scripts: scripts()},
		Pool:      slots.NewPool(2),
		Flows:     map[string]flow.Flow{"default": testFlow()},
		DataDir:   t.TempDir(),
		Observers: []func(core.Event){sw.Observe},
	})
}

func issueRow(t *testing.T, st *store.Store, id string) store.IssueRow {
	t.Helper()
	rows, err := st.Issues()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("issue %s not found", id)
	return store.IssueRow{}
}

func hasEvent(t *testing.T, st *store.Store, issueID string, typ core.EventType) bool {
	t.Helper()
	events, err := st.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.IssueID == issueID && event.Type == typ {
			return true
		}
	}
	return false
}

func TestDraftIssueStaysInBacklog(t *testing.T) {
	e, st := newTestEngine(t)
	id, err := e.DraftIssue("t", "b", "default", "regular", levers.Matrix{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	row := issueRow(t, st, id)
	if row.State != "backlog" {
		t.Fatalf("state = %q, want backlog", row.State)
	}
	if row.Priority != 2 || row.Title != "t" {
		t.Fatalf("row fields not persisted: %+v", row)
	}
	if !hasEvent(t, st, id, core.EvIssueDrafted) {
		t.Fatal("no issue_drafted event")
	}
	runs, err := st.StageRuns(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("draft ran %d stages", len(runs))
	}
}

func TestUpdateIssueOnlyLegalFromBacklog(t *testing.T) {
	e, st := newTestEngine(t)
	id, _ := e.DraftIssue("t", "b", "default", "regular", levers.Matrix{}, 0)
	if err := e.UpdateIssue(id, "t2", "b2", "default", "strict", levers.Matrix{}, 5); err != nil {
		t.Fatal(err)
	}
	row := issueRow(t, st, id)
	if row.Title != "t2" || row.Priority != 5 || row.State != "backlog" {
		t.Fatalf("update not persisted: %+v", row)
	}
	if !hasEvent(t, st, id, core.EvIssueUpdated) {
		t.Fatal("no issue_updated event")
	}
	rid, _ := e.CreateIssue("r", "", "default", levers.Matrix{}, 0)
	if err := e.UpdateIssue(rid, "x", "", "default", "regular", levers.Matrix{}, 0); err == nil {
		t.Fatal("update of non-draft succeeded")
	}
	if err := e.UpdateIssue("GH-999", "x", "", "default", "regular", levers.Matrix{}, 0); err == nil {
		t.Fatal("update of unknown issue succeeded")
	}
}

func TestAbandonDraft(t *testing.T) {
	e, st := newTestEngine(t)
	id, _ := e.DraftIssue("t", "b", "default", "regular", levers.Matrix{}, 0)
	if err := e.Abandon(id); err != nil {
		t.Fatal(err)
	}
	if issueRow(t, st, id).State != "abandoned" {
		t.Fatal("draft not abandoned")
	}
	if !hasEvent(t, st, id, core.EvIssueAbandoned) {
		t.Fatal("no issue_abandoned event")
	}
}

func TestRehydrateKeepsDraftsInert(t *testing.T) {
	e, st := newTestEngine(t)
	id, _ := e.DraftIssue("t", "b", "default", "regular", levers.Matrix{"impl": "yolo"}, 3)
	e2 := newEngineOver(t, st)
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if hasEvent(t, st, id, core.EvStageFailed) {
		t.Fatal("rehydrate marked draft failed")
	}
	if err := e2.UpdateIssue(id, "t2", "b", "default", "regular", levers.Matrix{}, 3); err != nil {
		t.Fatal(err)
	}
	nid, _ := e2.CreateIssue("n", "", "default", levers.Matrix{}, 0)
	if nid == id {
		t.Fatal("id collision after rehydrate")
	}
}
