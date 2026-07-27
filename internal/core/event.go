package core

import (
	"encoding/json"
	"time"
)

type EventType string

const (
	EvIssueCreated         EventType = "issue_created"
	EvStageStarted         EventType = "stage_started"
	EvStageCompleted       EventType = "stage_completed"
	EvStageFailed          EventType = "stage_failed"
	EvDecisionRequired     EventType = "decision_required"
	EvDecisionAnswered     EventType = "decision_answered"
	EvDecisionAutoResolved EventType = "decision_auto_resolved"
	EvSlotAcquired         EventType = "slot_acquired"
	EvSlotQueued           EventType = "slot_queued"
	EvSlotReleased         EventType = "slot_released"
	EvProposalFiled        EventType = "proposal_filed"
	EvProposalAccepted     EventType = "proposal_accepted"
	EvProposalRejected     EventType = "proposal_rejected"
	EvArtifactProduced     EventType = "artifact_produced"
	EvIssueCompleted       EventType = "issue_completed"
	EvBudgetExceeded       EventType = "budget_exceeded"
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
