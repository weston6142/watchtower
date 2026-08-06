package engine

import (
	"fmt"
	"html/template"
	"strings"

	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/decisionpage"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/review"
)

const (
	legacyWhyMissing = "No rationale was recorded for this historical decision."
	nextStageMissing = "The next stage is unavailable in this archived record."
)

func buildDecisionPageBriefing(
	dec *levers.Decision,
	ctx *decision.DecisionContext,
	target *review.Target,
	currentStage string,
	decisionID int64,
) *decisionpage.Briefing {
	recommendation, why := decisionPageRecommendation(dec)
	proof, missing := decisionPageProof(dec.Briefing)
	result := &decisionpage.Briefing{
		Question: dec.Question, Importance: dec.Importance, Reversible: dec.Reversible,
		Action:         decisionPageAction(dec, target, decisionID),
		Recommendation: recommendation, RecommendationWhy: why,
		Options: decisionPageOptions(dec, target),
		Proof:   proof, ProofMissing: missing,
		AfterAnswer: decisionPageContinuation(dec, ctx, target, currentStage),
	}
	if ctx != nil {
		result.AgentLabel = ctx.AgentLabel()
	}
	if dec.Briefing == nil {
		return result
	}
	for _, excerpt := range dec.Briefing.Excerpts {
		if len(result.Excerpts) == levers.MaxBriefingExcerpts {
			break
		}
		result.Excerpts = append(result.Excerpts, decisionpage.Excerpt{
			Text: excerpt.Text, Cite: excerpt.Cite,
		})
	}
	result.OverrideNote = dec.Briefing.OverrideNote
	result.DiagramCaption = dec.Briefing.DiagramCaption
	if dec.Briefing.DiagramSVG != "" {
		if err := decisionpage.ValidateSVG(dec.Briefing.DiagramSVG); err == nil {
			result.DiagramSVG = template.HTML(dec.Briefing.DiagramSVG)
		} else {
			result.DiagramMissing = true
		}
	}
	return result
}

func decisionPageAction(dec *levers.Decision, target *review.Target, decisionID int64) string {
	if target != nil {
		names := make([]string, 0, len(target.Artifacts))
		for _, artifact := range target.Artifacts {
			names = append(names, artifact.Name)
		}
		artifacts := "the archived artifacts"
		if len(names) > 0 {
			artifacts = strings.Join(names, ", ")
		}
		choices := "approve or revise"
		if len(dec.Options) >= 2 {
			choices = fmt.Sprintf("%s or %s", dec.Options[0], dec.Options[1])
		}
		return fmt.Sprintf(
			"Review %s, then choose %s for decision %d in the TUI.",
			artifacts, choices, decisionID,
		)
	}
	if dec.Kind == levers.DecisionFreeform {
		return fmt.Sprintf(
			"Enter your review or use the recommended response for decision %d in the TUI.",
			decisionID,
		)
	}
	return fmt.Sprintf("Choose an option or enter feedback for decision %d in the TUI.", decisionID)
}

func decisionPageRecommendation(dec *levers.Decision) (string, string) {
	label := "No recommendation was recorded for this historical decision."
	if dec.Kind == levers.DecisionFreeform && strings.TrimSpace(dec.RecommendedResponse) != "" {
		label = dec.RecommendedResponse
	} else if dec.Recommended >= 0 && dec.Recommended < len(dec.Options) {
		label = dec.Options[dec.Recommended]
	}
	why := strings.TrimSpace(dec.Why)
	if why == "" {
		why = legacyWhyMissing
	}
	return label, why
}

func decisionPageOptions(dec *levers.Decision, target *review.Target) []decisionpage.Option {
	if dec.Kind == levers.DecisionFreeform {
		outcome := "No recorded outcome for this historical response."
		if len(dec.Consequences) > 0 && strings.TrimSpace(dec.Consequences[0]) != "" {
			outcome = dec.Consequences[0]
		}
		label := dec.RecommendedResponse
		if strings.TrimSpace(label) == "" {
			label = "Enter feedback"
		}
		return []decisionpage.Option{{
			Key: "f", Label: label, OneLiner: outcome, Recommended: true,
		}}
	}

	options := make([]decisionpage.Option, 0, len(dec.Options)+1)
	for index, label := range dec.Options {
		outcome := "No recorded outcome for this historical option."
		if index < len(dec.Consequences) && strings.TrimSpace(dec.Consequences[index]) != "" {
			outcome = dec.Consequences[index]
		} else if dec.Briefing != nil && index < len(dec.Briefing.OptionDetails) &&
			strings.TrimSpace(dec.Briefing.OptionDetails[index]) != "" {
			outcome = dec.Briefing.OptionDetails[index]
		}
		options = append(options, decisionpage.Option{
			Key: fmt.Sprintf("%d", index+1), Label: label,
			OneLiner: outcome, Recommended: index == dec.Recommended,
		})
	}
	if target == nil {
		options = append(options, decisionpage.Option{
			Key: "f", Label: "Add feedback",
			OneLiner: "The requesting agent receives your instruction instead.",
		})
	}
	return options
}

func decisionPageProof(briefing *levers.Briefing) ([]decisionpage.Proof, bool) {
	if briefing == nil {
		return nil, true
	}
	if len(briefing.Proof) > 0 {
		proof := make([]decisionpage.Proof, 0, len(briefing.Proof))
		for _, item := range briefing.Proof {
			proof = append(proof, decisionpage.Proof{Claim: item.Claim, Cite: item.Cite})
		}
		return proof, false
	}
	if len(briefing.Wins) > 0 {
		proof := make([]decisionpage.Proof, 0, len(briefing.Wins))
		for _, win := range briefing.Wins {
			proof = append(proof, decisionpage.Proof{
				Claim: win, Cite: "Source not recorded in this historical decision.",
			})
		}
		return proof, false
	}
	return nil, true
}

func decisionPageContinuation(
	dec *levers.Decision, ctx *decision.DecisionContext, target *review.Target, currentStage string,
) string {
	if target != nil {
		if target.Stage == "" {
			return nextStageMissing
		}
		_, continuation := artifactReviewOutcomeCopy(*target, isPlanReviewDecision(dec))
		return continuation
	}
	if currentStage == "" {
		return nextStageMissing
	}
	agent := "the requesting agent"
	if ctx != nil && strings.TrimSpace(ctx.AgentName) != "" {
		agent = ctx.AgentName
	}
	return fmt.Sprintf("Watchtower records the response and resumes %s in %s.", agent, currentStage)
}

func artifactReviewDecision(target review.Target, plan bool) levers.Decision {
	question := fmt.Sprintf("Approve %s artifacts?", target.Stage)
	options := []string{"approve", "revise"}
	if plan {
		question = "Approve plan for execution?"
		options[1] = "reject"
	}
	consequences, _ := artifactReviewOutcomeCopy(target, plan)
	proof := make([]levers.BriefingProof, 0, len(target.Artifacts))
	for _, artifact := range target.Artifacts {
		proof = append(proof, levers.BriefingProof{
			Claim: fmt.Sprintf("%s is archived and ready for review.", artifact.Name),
			Cite: fmt.Sprintf(
				"checkpoint %d · %s · sha256 %s",
				target.CheckpointID, artifact.Name, artifact.SHA256,
			),
		})
	}
	if len(proof) > levers.MaxBriefingProof {
		proof = proof[:levers.MaxBriefingProof]
	}
	return levers.Decision{
		Kind: levers.DecisionChoice, Question: question, Options: options,
		Recommended: 0, Importance: 1.0,
		Why:          "The reviewed artifact version must be authorized before Watchtower advances the workflow.",
		Consequences: consequences,
		Reversible:   "The artifact can be revised before approval; approval authorizes this exact archived version.",
		Briefing:     &levers.Briefing{Proof: proof},
	}
}

func isPlanReviewDecision(dec *levers.Decision) bool {
	return dec != nil && len(dec.Options) >= 2 && strings.EqualFold(dec.Options[1], "reject")
}

func artifactReviewOutcomeCopy(target review.Target, plan bool) ([]string, string) {
	approval := fmt.Sprintf("Authorizes this artifact version and advances to %s.", target.NextStage)
	approvalContinuation := fmt.Sprintf("Approve to continue to %s.", target.NextStage)
	if target.NextStage == "" {
		approval = "Authorizes this artifact version and completes the workflow."
		approvalContinuation = "Approve to complete the workflow."
	}
	alternative := fmt.Sprintf("Marks this artifact version for revision and repeats %s.", target.Stage)
	alternativeContinuation := fmt.Sprintf("Revise to repeat %s.", target.Stage)
	if plan {
		alternative = "Rejects this plan and stops this run; retry the issue to produce and review a new plan."
		alternativeContinuation = "Reject to stop this run; retry the issue to produce and review a new plan."
	}
	return []string{approval, alternative}, approvalContinuation + " " + alternativeContinuation
}
