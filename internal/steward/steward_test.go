package steward

import (
	"testing"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/store"
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

func TestStewardMarksAbandoned(t *testing.T) {
	s, _ := store.Open("file:st_abandon?mode=memory&cache=shared")
	defer s.Close()
	st := &Steward{Store: s}
	st.Observe(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "doomed", "flow": "default"}))
	st.Observe(ev(t, core.EvStageFailed, "GH-1", map[string]any{"stage": "spec"}))
	st.Observe(ev(t, core.EvIssueAbandoned, "GH-1", map[string]any{}))
	rows, _ := s.Issues()
	if len(rows) != 1 || rows[0].State != "abandoned" {
		t.Fatalf("rows: %+v", rows)
	}
}
