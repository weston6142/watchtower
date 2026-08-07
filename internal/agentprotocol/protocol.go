// Package agentprotocol defines the provider-neutral messages and structured
// markers shared by agent runners.
package agentprotocol

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/weston6142/watchtower/internal/deps"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/runner"
)

// CoachMessage asks an agent to repair an incomplete decision marker without
// changing the decision itself.
const CoachMessage = `Your watchtower_decision is missing required context. Re-emit the SAME decision
as one JSON line with a nonblank "why" and one nonblank "consequences" entry
per choice, plus a nonblank "reversible" boundary. Freeform decisions need at
least one consequence. Every supplied briefing proof needs nonblank "claim" and
"cite" fields, and every excerpt needs nonblank "text" and "cite" fields.
Nothing else.`

// UnstructuredDecisionCoachMessage repairs an agent turn that asks the
// operator to manufacture the runner's private Human decision reply instead
// of emitting the public decision marker.
const UnstructuredDecisionCoachMessage = `You requested a Human decision reply in prose, so Watchtower cannot present
the decision to the operator. Re-emit that SAME decision as one valid
watchtower_decision JSON marker on its own line, following the shared decision
protocol. Nothing else.`

// TaskMessage is the first user message watchtower sends an agent.
func TaskMessage(stage, issueID string) string {
	return fmt.Sprintf("Task: run the %s stage for issue %s. Read ISSUE.md and STAGE.md in the current directory, then use only the materialized artifacts and decisions.md named there. Work in the current directory.", stage, issueID)
}

type decisionPayload struct {
	Kind                levers.DecisionKind `json:"kind"`
	Question            string              `json:"question"`
	Options             []string            `json:"options"`
	Recommended         int                 `json:"recommended"`
	RecommendedResponse string              `json:"recommended_response"`
	AllowFreeform       bool                `json:"allow_freeform"`
	Importance          float64             `json:"importance"`
	Paths               []string            `json:"paths"`
	Why                 string              `json:"why"`
	Consequences        []string            `json:"consequences"`
	Reversible          string              `json:"reversible"`
	Briefing            *levers.Briefing    `json:"briefing,omitempty"`
}

// Legacy accepts the pre-rename guildhall_* marker key. Removable once no
// in-flight agent session predates the rename.
type decisionMarker struct {
	D      decisionPayload `json:"watchtower_decision"`
	Legacy decisionPayload `json:"guildhall_decision"`
}

// ExtractDecision scans assistant text for a valid decision marker line.
func ExtractDecision(text string) (levers.Decision, bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `{"watchtower_decision":`) &&
			!strings.HasPrefix(line, `{"guildhall_decision":`) {
			continue
		}
		var marker decisionMarker
		if err := json.Unmarshal([]byte(line), &marker); err != nil {
			continue
		}
		d := marker.D
		legacy := false
		if d.Question == "" {
			d = marker.Legacy
			legacy = true
		}
		if d.Kind == "" {
			d.Kind = levers.DecisionChoice
		}
		if d.Question == "" {
			continue
		}
		switch d.Kind {
		case levers.DecisionChoice:
			if len(d.Options) == 0 || d.Recommended < 0 || d.Recommended >= len(d.Options) {
				continue
			}
		case levers.DecisionFreeform:
			if d.RecommendedResponse == "" {
				continue
			}
		default:
			continue
		}
		return levers.Decision{
			Kind: d.Kind, Question: d.Question, Options: d.Options,
			Recommended: d.Recommended, RecommendedResponse: d.RecommendedResponse,
			AllowFreeform: d.AllowFreeform, Importance: d.Importance,
			Paths: d.Paths, Why: d.Why,
			Consequences: d.Consequences, Reversible: d.Reversible,
			Briefing: normalizeBriefing(d.Briefing, len(d.Options), legacy),
		}, true
	}
	return levers.Decision{}, false
}

// UnstructuredDecisionRequestNeedsCoaching recognizes the explicit private
// reply syntax agents sometimes ask operators to type when they should emit a
// structured decision marker instead. It intentionally requires request
// language so accepted Human decision replies are not mistaken for new asks.
func UnstructuredDecisionRequestNeedsCoaching(text string) bool {
	if _, ok := ExtractDecision(text); ok {
		return false
	}
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "human decision:") {
		return false
	}
	return strings.Contains(lower, "reply with") || strings.Contains(lower, "respond with")
}

func normalizeBriefing(briefing *levers.Briefing, optionCount int, legacy bool) *levers.Briefing {
	if briefing == nil {
		return nil
	}

	result := *briefing
	if len(result.OptionDetails) != 0 && len(result.OptionDetails) != optionCount {
		result.OptionDetails = nil
	}
	if !legacy {
		result.Wins = nil
	} else if len(result.Wins) > levers.MaxBriefingWins {
		result.Wins = result.Wins[:levers.MaxBriefingWins]
	}
	if len(result.Proof) > levers.MaxBriefingProof {
		result.Proof = result.Proof[:levers.MaxBriefingProof]
	}
	if len(result.Excerpts) > levers.MaxBriefingExcerpts {
		result.Excerpts = result.Excerpts[:levers.MaxBriefingExcerpts]
	}
	return &result
}

// DecisionNeedsCoaching reports whether a newly emitted decision is missing
// the actionable context required by every operator-facing decision surface.
func DecisionNeedsCoaching(d levers.Decision) bool {
	if strings.TrimSpace(d.Why) == "" {
		return true
	}
	if strings.TrimSpace(d.Reversible) == "" {
		return true
	}
	if d.Kind == levers.DecisionChoice && len(d.Consequences) != len(d.Options) {
		return true
	}
	if d.Kind == levers.DecisionFreeform && len(d.Consequences) == 0 {
		return true
	}
	for _, consequence := range d.Consequences {
		if strings.TrimSpace(consequence) == "" {
			return true
		}
	}
	if d.Briefing == nil {
		return false
	}
	for _, proof := range d.Briefing.Proof {
		if strings.TrimSpace(proof.Claim) == "" || strings.TrimSpace(proof.Cite) == "" {
			return true
		}
	}
	for _, excerpt := range d.Briefing.Excerpts {
		if strings.TrimSpace(excerpt.Text) == "" || strings.TrimSpace(excerpt.Cite) == "" {
			return true
		}
	}
	return false
}

type proposalPayload struct {
	Key       string   `json:"key"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	DependsOn []string `json:"depends_on"`
}

type proposalMarker struct {
	P      proposalPayload `json:"watchtower_proposal"`
	Legacy proposalPayload `json:"guildhall_proposal"`
}

// ExtractProposal scans assistant text for a proposal marker.
func ExtractProposal(text string) (runner.Proposal, bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `{"watchtower_proposal":`) &&
			!strings.HasPrefix(line, `{"guildhall_proposal":`) {
			continue
		}
		var marker proposalMarker
		if err := json.Unmarshal([]byte(line), &marker); err != nil {
			continue
		}
		p := marker.P
		if p.Title == "" {
			p = marker.Legacy
		}
		if p.Title == "" {
			continue
		}
		return runner.Proposal{
			Key: p.Key, Title: p.Title, Body: p.Body,
			DependsOn: deps.Normalize(p.DependsOn),
		}, true
	}
	return runner.Proposal{}, false
}

type proposalBatchMarker struct {
	Batch struct {
		Tasks []proposalPayload `json:"tasks"`
	} `json:"watchtower_proposal_batch"`
}

// ExtractProposalBatch scans assistant text for a set of keyed proposals.
func ExtractProposalBatch(text string) ([]runner.Proposal, bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `{"watchtower_proposal_batch":`) {
			continue
		}
		var marker proposalBatchMarker
		if json.Unmarshal([]byte(line), &marker) != nil || len(marker.Batch.Tasks) == 0 {
			continue
		}
		out := make([]runner.Proposal, 0, len(marker.Batch.Tasks))
		valid := true
		for _, task := range marker.Batch.Tasks {
			if strings.TrimSpace(task.Key) == "" || strings.TrimSpace(task.Title) == "" {
				valid = false
				break
			}
			out = append(out, runner.Proposal{
				Key: strings.TrimSpace(task.Key), Title: task.Title, Body: task.Body,
				DependsOn: deps.Normalize(task.DependsOn),
			})
		}
		if valid {
			return out, true
		}
	}
	return nil, false
}

type dependencyMarker struct {
	Dependency struct {
		DependsOn []string `json:"depends_on"`
	} `json:"watchtower_dependency"`
}

// ExtractDependency scans assistant text for a dependency marker.
func ExtractDependency(text string) ([]string, bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `{"watchtower_dependency":`) {
			continue
		}
		var marker dependencyMarker
		if json.Unmarshal([]byte(line), &marker) != nil {
			continue
		}
		ids := deps.Normalize(marker.Dependency.DependsOn)
		if len(ids) > 0 {
			return ids, true
		}
	}
	return nil, false
}
