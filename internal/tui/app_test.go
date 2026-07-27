package tui

import (
	"testing"
	"time"

	"github.com/wbushyeager/guildhall/internal/core"
	"github.com/wbushyeager/guildhall/internal/projection"
)

func mkev(t *testing.T, typ core.EventType, issue string, payload any) core.Event {
	t.Helper()
	e, err := core.NewEvent(typ, issue, payload)
	if err != nil {
		t.Fatal(err)
	}
	seq++
	e.Seq = seq
	return e
}

var seq int64

func TestApplyEventsBuildsStateAndToast(t *testing.T) {
	m := NewModel(nil, []string{"brainstorm", "spec", "execute", "review", "merge"})
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "payment adapter", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "spec"}),
		mkev(t, core.EvDecisionRequired, "GH-1", map[string]any{
			"decision_id": float64(7), "stage": "spec", "question": "Approve?",
			"options": []any{"approve", "reject"}, "recommended": float64(0)}),
	})
	if m.State.Issues["GH-1"] == nil || m.Ids["GH-1"].Tag != "PA" {
		t.Fatalf("state/ids: %+v", m.Ids)
	}
	if m.Toast == nil || m.Toast.ID != 7 {
		t.Fatalf("toast not raised: %+v", m.Toast)
	}
	if m.lastSeq != 3 {
		t.Fatalf("lastSeq: %d", m.lastSeq)
	}
}

func TestShelfAutoRetiresAndUnretiresMergedIssue(t *testing.T) {
	m := NewModel(nil, []string{"spec", "merge"})
	m.State.Issues["GH-1"] = &projection.IssueView{ID: "GH-1", Title: "shipped", Merged: true, MergedAt: time.Now().Add(-2 * time.Minute)}
	m.State.ShippedToday = []string{"GH-1"}
	m.SetRetireAfter(time.Minute)
	m.autoRetire(time.Now())
	if items := m.shelfItems(); len(items) != 1 || items[0].ID != "GH-1" {
		t.Fatalf("merged issue was not retired: %+v", items)
	}
	delete(m.retired, "GH-1")
	if items := m.shelfItems(); len(items) != 0 {
		t.Fatalf("unretire did not remove shelf item: %+v", items)
	}
}
