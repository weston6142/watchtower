package store

import (
	"testing"

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
