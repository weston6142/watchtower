package review

import (
	"strings"

	"github.com/weston6142/watchtower/internal/flow"
)

// PolicySettings is the persisted repository policy used when a run is
// created. Invalid settings are retained so resolution can fail closed.
type PolicySettings struct {
	ID                 string
	Version            string
	AutoApproveRegular bool
	Valid              bool
}

// ResolvedPolicy is the immutable plan-review requirement for one run.
type ResolvedPolicy struct {
	Mode               string `json:"mode"`
	HumanRequired      bool   `json:"human_required"`
	PolicyAutoApproval bool   `json:"policy_auto_approval"`
	PolicyID           string `json:"policy_id"`
	PolicyVersion      string `json:"policy_version"`
	Reason             string `json:"reason"`
}

type ApprovalKind string

const (
	ApprovalHuman  ApprovalKind = "human"
	ApprovalPolicy ApprovalKind = "policy"
)

type ApprovalProvenance struct {
	Kind          ApprovalKind `json:"approval_kind"`
	ActorID       string       `json:"actor_id,omitempty"`
	PolicyID      string       `json:"policy_id,omitempty"`
	PolicyVersion string       `json:"policy_version,omitempty"`
}

// ResolvePlanReviewPolicy resolves plan authorization once for a run.
func ResolvePlanReviewPolicy(mode flow.Lever, settings PolicySettings) ResolvedPolicy {
	policy := ResolvedPolicy{
		Mode:          string(mode),
		HumanRequired: true,
		PolicyID:      strings.TrimSpace(settings.ID),
		PolicyVersion: strings.TrimSpace(settings.Version),
	}
	if policy.PolicyID == "" {
		policy.PolicyID = "manual-default"
	}
	if policy.PolicyVersion == "" {
		policy.PolicyVersion = "1"
	}
	if mode == flow.LeverStrict {
		policy.Reason = "strict_mode"
		return policy
	}
	if mode == flow.LeverRegular && settings.Valid && settings.AutoApproveRegular &&
		settings.ID != "" && settings.Version != "" {
		policy.HumanRequired = false
		policy.PolicyAutoApproval = true
		policy.Reason = "policy_opt_in"
		return policy
	}
	if mode == flow.LeverRegular && settings.AutoApproveRegular &&
		(!settings.Valid || settings.ID == "" || settings.Version == "") {
		policy.Reason = "invalid_policy"
		return policy
	}
	policy.Reason = "manual_default"
	return policy
}

// ManualPlanReviewPolicy is the safe fallback for legacy or damaged state.
func ManualPlanReviewPolicy(mode flow.Lever) ResolvedPolicy {
	return ResolvedPolicy{
		Mode:          string(mode),
		HumanRequired: true,
		PolicyID:      "manual-default",
		PolicyVersion: "1",
		Reason:        "invalid_policy",
	}
}
