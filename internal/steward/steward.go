package steward

import (
	"encoding/json"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/store"
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
	boolean := func(k string) bool {
		v, _ := p[k].(bool)
		return v
	}
	leverMap := func() map[string]string {
		out := map[string]string{}
		if raw, ok := p["levers"].(map[string]any); ok {
			for k, v := range raw {
				if s, ok := v.(string); ok {
					out[k] = s
				}
			}
		}
		return out
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
	case core.EvIssueDrafted, core.EvIssueUpdated:
		prio, _ := p["priority"].(float64)
		_ = st.Store.UpsertIssue(store.IssueRow{
			ID: ev.IssueID, Title: str("title"), Body: str("body"),
			Flow: str("flow"), State: "backlog", Priority: int(prio),
			Levers: leverMap()})
	case core.EvIssueClaimed:
		setState("claimed")
	case core.EvIssueReleased:
		setState("backlog")
	case core.EvIssueCreated:
		prio, _ := p["priority"].(float64)
		_ = st.Store.UpsertIssue(store.IssueRow{
			ID: ev.IssueID, Title: str("title"), Body: str("body"),
			Flow: str("flow"), State: "running", Priority: int(prio)})
	case core.EvIssueWaitingDependencies:
		setState("waiting_dependencies")
	case core.EvIssueDependenciesSatisfied:
		setState("running")
	case core.EvStageStarted:
		if boolean("merge_barrier") || str("stage") == "merge-verification" {
			setState("verifying")
		} else {
			setState("running:" + str("stage"))
		}
	case core.EvDecisionRequired:
		setState("waiting_decision")
	case core.EvDecisionAnswered:
		setState("running")
	case core.EvStageFailed:
		setState("failed")
	case core.EvVerificationReady:
		setState("waiting:integration")
	case core.EvMergeStarted:
		setState("integrating")
	case core.EvFinalizationFailed:
		setState("failed:finalize")
	case core.EvIssueCompleted:
		rows, _ := st.Store.Issues()
		for _, row := range rows {
			if row.ID == ev.IssueID && row.State == "cleanup_needed" {
				return
			}
		}
		if str("merge") == "left-unmerged" {
			setState("done (unmerged)")
		} else {
			setState("done")
		}
	case core.EvIssueMerged:
		setState("merged")
	case core.EvCleanupNeeded:
		setState("cleanup_needed")
	case core.EvCleanupCompleted:
		setState("done")
	case core.EvIssueAbandoned:
		setState("abandoned")
	}
}
