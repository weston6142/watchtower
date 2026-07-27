package projection

import (
	"encoding/json"

	"github.com/wbushyeager/guildhall/internal/core"
)

type IssueView struct {
	ID           string
	Title        string
	Flow         string
	CurrentStage string
	State        string
	Completed    []string
	Tokens       int
	Behind       string
	Merged       bool
	Unmerged     bool
}

type DecisionView struct {
	ID          int64
	IssueID     string
	Stage       string
	Question    string
	Options     []string
	Recommended int
}

type State struct {
	Issues        map[string]*IssueView
	Decisions     map[int64]DecisionView
	Order         []string
	ProposalCount int
}

func NewState() *State {
	return &State{Issues: map[string]*IssueView{}, Decisions: map[int64]DecisionView{}}
}

func (s *State) Apply(ev core.Event) {
	var p map[string]any
	// Best-effort decode: a malformed payload leaves p nil and the
	// accessors below return zero values.
	_ = json.Unmarshal(ev.Payload, &p)
	str := func(k string) string { v, _ := p[k].(string); return v }
	num := func(k string) float64 { v, _ := p[k].(float64); return v }

	iv := s.Issues[ev.IssueID]
	switch ev.Type {
	case core.EvIssueCreated:
		s.Issues[ev.IssueID] = &IssueView{ID: ev.IssueID, Title: str("title"), Flow: str("flow"), State: "running"}
		s.Order = append(s.Order, ev.IssueID)
	case core.EvStageStarted:
		if iv != nil {
			iv.CurrentStage = str("stage")
			iv.State = "running"
		}
	case core.EvSlotQueued:
		if iv != nil {
			iv.State = "queued_for_slot"
		}
	case core.EvSlotAcquired:
		if iv != nil {
			iv.State = "running"
		}
	case core.EvDecisionRequired:
		var opts []string
		if raw, ok := p["options"].([]any); ok {
			for _, o := range raw {
				if os, ok := o.(string); ok {
					opts = append(opts, os)
				}
			}
		}
		id := int64(num("decision_id"))
		s.Decisions[id] = DecisionView{ID: id, IssueID: ev.IssueID, Stage: str("stage"),
			Question: str("question"), Options: opts, Recommended: int(num("recommended"))}
		if iv != nil {
			iv.State = "waiting_decision"
		}
	case core.EvDecisionAnswered:
		delete(s.Decisions, int64(num("decision_id")))
		if iv != nil {
			iv.State = "running"
		}
	case core.EvStageCompleted:
		if iv != nil {
			iv.Completed = append(iv.Completed, str("stage"))
		}
	case core.EvIssueCompleted:
		if iv != nil {
			iv.State = "done"
			iv.Unmerged = str("merge") == "left-unmerged"
		}
	case core.EvStageFailed:
		if iv != nil {
			iv.CurrentStage = str("stage")
			iv.State = "failed"
		}
	case core.EvMergeSequenced:
		if iv != nil {
			iv.Behind = str("behind")
		}
	case core.EvMergeStarted:
		// Merge sequencing is the meaningful projection for now.
	case core.EvIssueMerged:
		if iv != nil {
			iv.Merged = true
			iv.Behind = ""
		}
		for _, other := range s.Issues {
			if other.Behind == ev.IssueID {
				other.Behind = ""
			}
		}
	case core.EvProposalFiled:
		s.ProposalCount++
	case core.EvProposalAccepted, core.EvProposalRejected:
		if s.ProposalCount > 0 {
			s.ProposalCount--
		}
	}
}
