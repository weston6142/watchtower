package steward

import (
	"encoding/json"

	"github.com/wbushyeager/guildhall/internal/core"
	"github.com/wbushyeager/guildhall/internal/store"
)

type Steward struct {
	Store *store.Store
}

// Observe applies one event to the durable issue projection.
func (st *Steward) Observe(ev core.Event) {
	var p map[string]any
	_ = json.Unmarshal(ev.Payload, &p)
	str := func(k string) string {
		v, _ := p[k].(string)
		return v
	}
	setState := func(state string) {
		rows, err := st.Store.Issues()
		if err != nil {
			return
		}
		for _, r := range rows {
			if r.ID == ev.IssueID {
				r.State = state
				_ = st.Store.UpsertIssue(r)
				return
			}
		}
	}
	switch ev.Type {
	case core.EvIssueCreated:
		prio, _ := p["priority"].(float64)
		_ = st.Store.UpsertIssue(store.IssueRow{
			ID: ev.IssueID, Title: str("title"), Body: str("body"),
			Flow: str("flow"), State: "running", Priority: int(prio)})
	case core.EvStageStarted:
		setState("running:" + str("stage"))
	case core.EvDecisionRequired:
		setState("waiting_decision")
	case core.EvDecisionAnswered:
		setState("running")
	case core.EvStageFailed:
		setState("failed")
	case core.EvIssueCompleted:
		setState("done")
	case core.EvIssueMerged:
		setState("merged")
	}
}
