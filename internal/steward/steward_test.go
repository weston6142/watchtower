package steward

import (
	"testing"

	"github.com/wbushyeager/guildhall/internal/core"
	"github.com/wbushyeager/guildhall/internal/store"
)

func ev(t *testing.T, typ core.EventType, issue string, payload any) core.Event {
	t.Helper()
	e, err := core.NewEvent(typ, issue, payload)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestStewardProjectsIssueTable(t *testing.T) {
	s, _ := store.Open("file:st1?mode=memory&cache=shared")
	defer s.Close()
	st := &Steward{Store: s}
	st.Observe(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "hello", "flow": "default", "body": "b", "priority": float64(2)}))
	st.Observe(ev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "spec"}))
	rows, _ := s.Issues()
	if len(rows) != 1 || rows[0].State != "running:spec" || rows[0].Title != "hello" || rows[0].Priority != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	st.Observe(ev(t, core.EvStageFailed, "GH-1", map[string]any{"stage": "spec"}))
	rows, _ = s.Issues()
	if rows[0].State != "failed" {
		t.Fatalf("state: %s", rows[0].State)
	}
}
