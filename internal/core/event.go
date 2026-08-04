package core

import (
	"encoding/json"
	"time"
)

type EventType string

const (
	EvIssueCreated               EventType = "issue_created"
	EvIssueDrafted               EventType = "issue_drafted"
	EvIssueUpdated               EventType = "issue_updated"
	EvIssueClaimed               EventType = "issue_claimed"
	EvIssueReleased              EventType = "issue_released"
	EvIssueWaitingDependencies   EventType = "issue_waiting_dependencies"
	EvIssueDependenciesSatisfied EventType = "issue_dependencies_satisfied"
	EvStageStarted               EventType = "stage_started"
	EvStageCompleted             EventType = "stage_completed"
	EvStageFailed                EventType = "stage_failed"
	EvDecisionRequired           EventType = "decision_required"
	EvDecisionAnswered           EventType = "decision_answered"
	EvDecisionAutoResolved       EventType = "decision_auto_resolved"
	EvPlanReviewRequested        EventType = "plan_review_requested"
	EvPlanReviewHumanApproved    EventType = "plan_review_human_approved"
	EvPlanReviewPolicyApproved   EventType = "plan_review_policy_approved"
	EvPlanReviewRejected         EventType = "plan_review_rejected"
	EvExecutionStarted           EventType = "execution_started"
	EvSlotAcquired               EventType = "slot_acquired"
	EvSlotQueued                 EventType = "slot_queued"
	EvSlotReleased               EventType = "slot_released"
	EvProposalFiled              EventType = "proposal_filed"
	EvProposalAccepted           EventType = "proposal_accepted"
	EvProposalRejected           EventType = "proposal_rejected"
	EvIssueMerged                EventType = "issue_merged"
	EvMergeSequenced             EventType = "merge_sequenced"
	EvMergeStarted               EventType = "merge_started"
	EvVerificationReady          EventType = "verification_ready"
	EvFinalizationFailed         EventType = "finalization_failed"
	EvBaseStale                  EventType = "base_stale"
	EvMergeConflict              EventType = "merge_conflict"
	EvPublishPending             EventType = "publish_pending"
	EvPublishRetry               EventType = "publish_retry"
	EvPublishSucceeded           EventType = "publish_succeeded"
	EvCleanupNeeded              EventType = "cleanup_needed"
	EvCleanupCompleted           EventType = "cleanup_completed"
	EvArtifactProduced           EventType = "artifact_produced"
	EvIssueCompleted             EventType = "issue_completed"
	EvBudgetExceeded             EventType = "budget_exceeded"
	EvIssuePaused                EventType = "issue_paused"
	EvIssueResumed               EventType = "issue_resumed"
	EvIssueAbandoned             EventType = "issue_abandoned"
	EvStageKilled                EventType = "stage_killed"
	EvLeverChanged               EventType = "lever_changed"
	EvRunnerAttempt              EventType = "runner_attempt"
)

type Event struct {
	ID      int64           `json:"id"`
	Seq     int64           `json:"seq"`
	Type    EventType       `json:"type"`
	IssueID string          `json:"issue_id"`
	Payload json.RawMessage `json:"payload"`
	At      time.Time       `json:"at"`
}

func NewEvent(t EventType, issueID string, payload any) (Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	return Event{Type: t, IssueID: issueID, Payload: raw, At: time.Now().UTC()}, nil
}
