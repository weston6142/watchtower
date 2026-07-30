package projection

import (
	"encoding/json"
	"time"

	"github.com/weston6142/watchtower/internal/core"
)

type IssueView struct {
	ID           string
	Title        string
	Flow         string
	CurrentStage string
	State        string
	Attempt      int
	AttemptOf    int
	LastError    string
	Completed    []string
	Tokens       int
	Priority     int
	Body         string
	Preset       string
	// Attachments holds stored names in ord order. It is populated from the
	// drafted/updated events and read only by the edit modal's prefill:
	// EvIssueCreated rebuilds the view wholesale, so a launched issue drops
	// the list, which is fine because a launched issue is not editable.
	Attachments []string
	DependsOn   []string
	Behind      string
	Merged      bool
	Unmerged    bool
	Paused      bool
	Killed      bool
	AreaWeights map[string]int
	MergedAt    time.Time
}

type DecisionView struct {
	ID                  int64
	IssueID             string
	Stage               string
	Kind                string
	Question            string
	Options             []string
	Recommended         int
	RecommendedResponse string
	AllowFreeform       bool
	Why                 string
	Consequences        []string
	Reversible          string
	Paths               []string
}

type Notice struct {
	Text string
	Seq  int64
}

type State struct {
	Issues        map[string]*IssueView
	Decisions     map[int64]DecisionView
	Order         []string
	ProposalCount int
	Notices       []Notice
	ShippedToday  []string
	Parked        []string
	Backlog       []string
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
	case core.EvIssueDrafted, core.EvIssueUpdated:
		view := s.Issues[ev.IssueID]
		if view == nil {
			view = &IssueView{ID: ev.IssueID, AreaWeights: map[string]int{}}
			s.Issues[ev.IssueID] = view
			appendUnique(&s.Backlog, ev.IssueID)
		}
		view.Title = str("title")
		view.Flow = str("flow")
		view.Body = str("body")
		view.Preset = str("preset")
		view.Priority = int(num("priority"))
		view.Attachments = stringsFromPayload(p["attachments"])
		view.DependsOn = stringsFromPayload(p["depends_on"])
		view.State = "backlog"
	case core.EvIssueCreated:
		s.Issues[ev.IssueID] = &IssueView{ID: ev.IssueID, Title: str("title"), Flow: str("flow"), State: "running", AreaWeights: map[string]int{}}
		s.Order = append(s.Order, ev.IssueID)
		removeString(&s.Backlog, ev.IssueID)
	case core.EvStageStarted:
		if iv != nil {
			iv.CurrentStage = str("stage")
			iv.State = "running"
			iv.Paused = false
			iv.Killed = false
			iv.Attempt = int(num("attempt"))
			iv.AttemptOf = int(num("of"))
			iv.LastError = ""
		}
	case core.EvIssueWaitingDependencies:
		if iv != nil {
			iv.State = "waiting_dependencies"
		}
	case core.EvIssueDependenciesSatisfied:
		if iv != nil {
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
			Kind: str("kind"), Question: str("question"), Options: opts,
			Recommended: int(num("recommended")), RecommendedResponse: str("recommended_response"),
			AllowFreeform: p["allow_freeform"] == true,
			Why:           str("why"), Consequences: stringsFromPayload(p["consequences"]),
			Reversible: str("reversible"), Paths: stringsFromPayload(p["paths"])}
		if iv != nil {
			iv.State = "waiting_decision"
		}
	case core.EvDecisionAnswered:
		delete(s.Decisions, int64(num("decision_id")))
		if iv != nil {
			iv.State = "running"
		}
	case core.EvArtifactProduced:
		if iv != nil {
			if iv.AreaWeights == nil {
				iv.AreaWeights = map[string]int{}
			}
			if weights, ok := p["area_weight"].(map[string]any); ok {
				for area, value := range weights {
					if weight, ok := value.(float64); ok {
						iv.AreaWeights[area] = int(weight)
					}
				}
			}
		}
	case core.EvStageCompleted:
		if iv != nil {
			iv.Completed = append(iv.Completed, str("stage"))
			iv.LastError = ""
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
			iv.Attempt = int(num("attempt"))
			iv.AttemptOf = int(num("of"))
			iv.LastError = str("error")
			if final, _ := p["final"].(bool); final {
				appendUnique(&s.Parked, ev.IssueID)
			}
		}
	case core.EvIssuePaused:
		if iv != nil {
			iv.Paused = true
			iv.State = "paused"
			// The gate sits before the upcoming stage, so that is where the
			// lane is parked. Older events carry no payload; leave those.
			if stage := str("stage"); stage != "" {
				iv.CurrentStage = stage
			}
		}
	case core.EvIssueResumed:
		if iv != nil {
			iv.Paused = false
			// The lane recovered from whatever stopped it; a stale Killed
			// keeps the retry affordance armed for a running stage.
			iv.Killed = false
			iv.State = "running"
		}
	case core.EvIssueAbandoned:
		// An abandoned lane leaves every surface: grid, shelves, and queue.
		delete(s.Issues, ev.IssueID)
		removeString(&s.Order, ev.IssueID)
		removeString(&s.ShippedToday, ev.IssueID)
		removeString(&s.Parked, ev.IssueID)
		removeString(&s.Backlog, ev.IssueID)
		for id, d := range s.Decisions {
			if d.IssueID == ev.IssueID {
				delete(s.Decisions, id)
			}
		}
	case core.EvStageKilled:
		if iv != nil {
			iv.Killed = true
			iv.Paused = true
			iv.State = "paused"
			iv.CurrentStage = str("stage")
		}
		appendUnique(&s.Parked, ev.IssueID)
	case core.EvMergeSequenced:
		if iv != nil {
			iv.Behind = str("behind")
		}
	case core.EvMergeStarted:
		// Merge sequencing is the meaningful projection for now.
	case core.EvIssueMerged:
		if iv != nil {
			iv.Merged = true
			iv.MergedAt = ev.At
			iv.Behind = ""
		}
		appendUnique(&s.ShippedToday, ev.IssueID)
		for _, other := range s.Issues {
			if other.Behind == ev.IssueID {
				other.Behind = ""
			}
		}
	case core.EvProposalFiled:
		s.ProposalCount++
		s.Notices = append(s.Notices, Notice{Text: "✉ new idea from " + ev.IssueID + ": " + str("title"), Seq: ev.Seq})
		if len(s.Notices) > 5 {
			s.Notices = s.Notices[len(s.Notices)-5:]
		}
	case core.EvProposalAccepted, core.EvProposalRejected:
		if s.ProposalCount > 0 {
			s.ProposalCount--
		}
	}
}

func stringsFromPayload(raw any) []string {
	var out []string
	switch values := raw.(type) {
	case []any:
		for _, value := range values {
			if value, ok := value.(string); ok {
				out = append(out, value)
			}
		}
	case []string:
		out = append(out, values...)
	}
	return out
}

func appendUnique(items *[]string, value string) {
	for _, item := range *items {
		if item == value {
			return
		}
	}
	*items = append(*items, value)
}

func removeString(items *[]string, value string) {
	kept := (*items)[:0]
	for _, item := range *items {
		if item != value {
			kept = append(kept, item)
		}
	}
	*items = kept
}
