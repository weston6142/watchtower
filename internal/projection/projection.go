package projection

import (
	"encoding/json"
	"time"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/deps"
	"github.com/weston6142/watchtower/internal/review"
	"github.com/weston6142/watchtower/internal/stageusage"
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
	Attachments        []string
	DependsOn          []string
	Behind             string
	Merged             bool
	Unmerged           bool
	Paused             bool
	Killed             bool
	Cleanup            []string
	AreaWeights        map[string]int
	MergedAt           time.Time
	ReviewPolicy       review.ResolvedPolicy
	ReviewStatus       string
	Approval           *review.ApprovalProvenance
	DecisionEvaluation *review.Evaluation
	DecisionBindings   []review.Binding
	Planner            *stageusage.Snapshot
	PlannerOutcome     string
}

// DecisionView is the projected operator-facing decision. RequiresOption
// suppresses freeform response affordances for engine-owned decisions.
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
	RequiresOption      bool
	Why                 string
	Consequences        []string
	Reversible          string
	Paths               []string
	Context             *decision.DecisionContext
	Review              *review.Target
	ReviewPolicy        *review.ResolvedPolicy
	ReviewStatus        string
	Approval            *review.ApprovalProvenance
	Evaluation          *review.Evaluation
	Bindings            []review.Binding
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
	// Shipped accumulates every merged lane ID in merge order. It is all-time
	// and time-free: the projection is a deterministic fold over the event log
	// and never reads a clock. Consumers that want a day scope filter on
	// IssueView.MergedAt.
	Shipped []string
	Parked  []string
	Backlog []string
}

func NewState() *State {
	return &State{Issues: map[string]*IssueView{}, Decisions: map[int64]DecisionView{}}
}

// ActiveBlockers derives the current backlog projection without changing the
// raw dependency list stored on the issue view.
func (s *State) ActiveBlockers(issueID string) []string {
	if s == nil {
		return nil
	}
	issue := s.Issues[issueID]
	if issue == nil {
		return nil
	}
	active, err := deps.ActiveBlockers(issue.DependsOn, func(parentID string) (bool, error) {
		parent := s.Issues[parentID]
		return parent != nil && !parent.Unmerged &&
			(parent.State == "done" || parent.State == "cleanup_needed"), nil
	})
	if err != nil {
		return append([]string(nil), issue.DependsOn...)
	}
	return active
}

func (s *State) Apply(ev core.Event) {
	var p map[string]any
	// Best-effort decode: a malformed payload leaves p nil and the
	// accessors below return zero values.
	_ = json.Unmarshal(ev.Payload, &p)
	str := func(k string) string { v, _ := p[k].(string); return v }
	num := func(k string) float64 { v, _ := p[k].(float64); return v }
	boolean := func(k string) bool { v, _ := p[k].(bool); return v }
	policy := resolvedPolicyFromPayload(p, "review_policy")
	if policy == nil {
		policy = resolvedPolicyFromPayload(p, "")
	}

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
	case core.EvIssueClaimed:
		if iv != nil {
			iv.State = "claimed"
			iv.CurrentStage = ""
			removeString(&s.Backlog, ev.IssueID)
			appendUnique(&s.Order, ev.IssueID)
		}
	case core.EvIssueReleased:
		if iv != nil {
			iv.State = "backlog"
			removeString(&s.Order, ev.IssueID)
			appendUnique(&s.Backlog, ev.IssueID)
		}
	case core.EvIssueCreated:
		s.Issues[ev.IssueID] = &IssueView{ID: ev.IssueID, Title: str("title"), Flow: str("flow"), State: "running", AreaWeights: map[string]int{}}
		s.Order = append(s.Order, ev.IssueID)
		removeString(&s.Backlog, ev.IssueID)
	case core.EvPlanReviewRequested:
		if iv != nil {
			if policy != nil {
				iv.ReviewPolicy = *policy
			}
			iv.ReviewStatus = "pending"
			iv.State = "waiting_decision"
		}
	case core.EvStageStarted:
		if iv != nil {
			iv.CurrentStage = str("stage")
			if boolean("merge_barrier") || iv.CurrentStage == "merge-verification" {
				iv.State = "verifying"
			} else {
				iv.State = "running"
			}
			iv.Paused = false
			iv.Killed = false
			iv.Attempt = int(num("attempt"))
			iv.AttemptOf = int(num("of"))
			iv.LastError = ""
		}
	case core.EvPlannerBudgetUpdated:
		if iv != nil {
			if raw, ok := p["snapshot"]; ok {
				encoded, err := json.Marshal(raw)
				if err == nil {
					var decoded stageusage.Snapshot
					if json.Unmarshal(encoded, &decoded) == nil {
						clone := decoded
						clone.Warnings = append([]stageusage.Dimension(nil), decoded.Warnings...)
						iv.Planner = &clone
					}
				}
			}
			iv.PlannerOutcome = str("outcome")
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
		var context *decision.DecisionContext
		if raw, ok := p["context"]; ok {
			encoded, err := json.Marshal(raw)
			if err == nil && string(encoded) != "null" {
				var decoded decision.DecisionContext
				if json.Unmarshal(encoded, &decoded) == nil {
					context = &decoded
				}
			}
		}
		var reviewTarget *review.Target
		if raw, ok := p["review"]; ok {
			encoded, err := json.Marshal(raw)
			if err == nil && string(encoded) != "null" {
				var decoded review.Target
				if json.Unmarshal(encoded, &decoded) == nil {
					reviewTarget = &decoded
				}
			}
		}
		decisionPolicy := resolvedPolicyFromPayload(p, "review_policy")
		decisionReviewStatus := ""
		if decisionPolicy != nil {
			decisionReviewStatus = "pending"
		}
		evaluation := escalationEvaluationFromPayload(p)
		bindings := escalationBindingsFromPayload(p)
		id := int64(num("decision_id"))
		s.Decisions[id] = DecisionView{ID: id, IssueID: ev.IssueID, Stage: str("stage"),
			Kind: str("kind"), Question: str("question"), Options: opts,
			Recommended: int(num("recommended")), RecommendedResponse: str("recommended_response"),
			AllowFreeform:  p["allow_freeform"] == true,
			RequiresOption: p["requires_option"] == true || reviewTarget != nil,
			Why:            str("why"), Consequences: stringsFromPayload(p["consequences"]),
			Reversible: str("reversible"), Paths: stringsFromPayload(p["paths"]), Context: context,
			Review: reviewTarget, ReviewPolicy: decisionPolicy, ReviewStatus: decisionReviewStatus,
			Evaluation: evaluation, Bindings: bindings}
		if iv != nil {
			iv.State = "waiting_decision"
			iv.DecisionEvaluation = evaluation
			iv.DecisionBindings = bindings
			if decisionPolicy != nil {
				iv.ReviewPolicy = *decisionPolicy
				iv.ReviewStatus = "pending"
			}
		}
	case core.EvDecisionAutoResolved:
		if iv != nil {
			iv.DecisionEvaluation = escalationEvaluationFromPayload(p)
			iv.DecisionBindings = escalationBindingsFromPayload(p)
			iv.State = "running"
		}
		delete(s.Decisions, int64(num("decision_id")))
	case core.EvDecisionStale, core.EvDecisionPolicyError:
		id := int64(num("decision_id"))
		evaluation := escalationEvaluationFromPayload(p)
		if decision, exists := s.Decisions[id]; exists {
			decision.Evaluation = evaluation
			decision.Bindings = escalationBindingsFromPayload(p)
			decision.ReviewStatus = string(ev.Type)
			s.Decisions[id] = decision
		}
		if iv != nil {
			iv.DecisionEvaluation = evaluation
			iv.DecisionBindings = escalationBindingsFromPayload(p)
			iv.ReviewStatus = string(ev.Type)
		}
	case core.EvPlanReviewHumanApproved, core.EvPlanReviewPolicyApproved, core.EvPlanReviewRejected:
		if iv != nil {
			if policy != nil {
				iv.ReviewPolicy = *policy
			}
			iv.Approval = approvalFromPayload(p, policy)
			iv.DecisionEvaluation = escalationEvaluationFromPayload(p)
			iv.DecisionBindings = escalationBindingsFromPayload(p)
			switch ev.Type {
			case core.EvPlanReviewHumanApproved:
				iv.ReviewStatus = "approved"
				iv.State = "running"
			case core.EvPlanReviewPolicyApproved:
				iv.ReviewStatus = "approved_automatically"
				iv.State = "running"
			case core.EvPlanReviewRejected:
				iv.ReviewStatus = "rejected"
				iv.State = "review_rejected"
			}
		}
		delete(s.Decisions, int64(num("decision_id")))
	case core.EvExecutionStarted:
		if iv != nil {
			iv.CurrentStage = str("stage")
			iv.State = "running"
		}
	case core.EvDecisionAnswered:
		delete(s.Decisions, int64(num("decision_id")))
		if iv != nil && iv.ReviewStatus != "rejected" {
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
	case core.EvVerificationReady:
		if iv != nil {
			iv.State = "waiting:integration"
			iv.LastError = ""
		}
	case core.EvMergeStarted:
		if iv != nil {
			iv.State = "integrating"
			iv.LastError = ""
		}
	case core.EvFinalizationFailed:
		if iv != nil {
			iv.State = "failed:finalize"
			iv.LastError = str("error")
			appendUnique(&s.Parked, ev.IssueID)
		}
	case core.EvIssueCompleted:
		if iv != nil {
			if iv.State != "cleanup_needed" {
				iv.State = "done"
			}
			iv.Unmerged = str("merge") == "left-unmerged"
		}
	case core.EvCleanupNeeded:
		if iv != nil {
			iv.State = "cleanup_needed"
			iv.Merged = true
			iv.LastError = str("error")
			iv.Cleanup = stringsFromPayload(p["operations"])
		}
	case core.EvCleanupCompleted:
		if iv != nil {
			iv.State = "done"
			iv.LastError = ""
			iv.Cleanup = nil
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
		removeString(&s.Parked, ev.IssueID)
	case core.EvIssueAbandoned:
		// An abandoned lane leaves every surface: grid, shelves, and queue.
		delete(s.Issues, ev.IssueID)
		removeString(&s.Order, ev.IssueID)
		removeString(&s.Shipped, ev.IssueID)
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
	case core.EvIssueMerged:
		if iv != nil {
			iv.Merged = true
			iv.MergedAt = ev.At
			iv.Behind = ""
		}
		appendUnique(&s.Shipped, ev.IssueID)
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

func escalationEvaluationFromPayload(payload map[string]any) *review.Evaluation {
	raw, ok := payload["evaluation"]
	if !ok || raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var evaluation review.Evaluation
	if err := json.Unmarshal(encoded, &evaluation); err != nil {
		return nil
	}
	return &evaluation
}

func escalationBindingsFromPayload(payload map[string]any) []review.Binding {
	raw, ok := payload["bindings"]
	if !ok || raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var bindings []review.Binding
	if err := json.Unmarshal(encoded, &bindings); err != nil {
		return nil
	}
	return bindings
}

func resolvedPolicyFromPayload(payload map[string]any, key string) *review.ResolvedPolicy {
	var raw any
	if key != "" {
		raw = payload[key]
	} else {
		if _, hasPolicy := payload["policy_id"]; !hasPolicy {
			if _, hasMode := payload["mode"]; !hasMode {
				return nil
			}
		}
		raw = payload
	}
	if raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var policy review.ResolvedPolicy
	if err := json.Unmarshal(encoded, &policy); err != nil {
		return nil
	}
	return &policy
}

func approvalFromPayload(payload map[string]any, policy *review.ResolvedPolicy) *review.ApprovalProvenance {
	var approval review.ApprovalProvenance
	if raw, ok := payload["approval"]; ok {
		encoded, err := json.Marshal(raw)
		if err == nil {
			_ = json.Unmarshal(encoded, &approval)
		}
	}
	if approval.Kind == "" {
		kind, _ := payload["approval_kind"].(string)
		approval.Kind = review.ApprovalKind(kind)
	}
	if approval.ActorID == "" {
		approval.ActorID, _ = payload["actor_id"].(string)
	}
	if approval.PolicyID == "" {
		approval.PolicyID, _ = payload["policy_id"].(string)
	}
	if approval.PolicyVersion == "" {
		approval.PolicyVersion, _ = payload["policy_version"].(string)
	}
	if approval.Kind == "" {
		return nil
	}
	if approval.Kind == review.ApprovalPolicy && policy != nil {
		if approval.PolicyID == "" {
			approval.PolicyID = policy.PolicyID
		}
		if approval.PolicyVersion == "" {
			approval.PolicyVersion = policy.PolicyVersion
		}
	}
	return &approval
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
