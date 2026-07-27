package proto

import (
	"github.com/wbushyeager/guildhall/internal/core"
	"github.com/wbushyeager/guildhall/internal/engine"
	"github.com/wbushyeager/guildhall/internal/store"
)

// maxMessageBytes bounds a single newline-delimited JSON message on the wire.
const maxMessageBytes = 1 << 20

type Command struct {
	Op         string `json:"op"`
	Title      string `json:"title,omitempty"`
	Body       string `json:"body,omitempty"`
	Flow       string `json:"flow,omitempty"`
	Preset     string `json:"preset,omitempty"`
	Priority   int    `json:"priority,omitempty"`
	IssueID    string `json:"issue_id,omitempty"`
	DecisionID int64  `json:"decision_id,omitempty"`
	Option     int    `json:"option,omitempty"`
	ProposalID int64  `json:"proposal_id,omitempty"`
	Accept     bool   `json:"accept,omitempty"`
	SinceSeq   int64  `json:"since_seq,omitempty"`
}

type Response struct {
	OK        bool                     `json:"ok"`
	Error     string                   `json:"error,omitempty"`
	IssueID   string                   `json:"issue_id,omitempty"`
	Decisions []engine.PendingDecision `json:"decisions,omitempty"`
	Proposals []store.ProposalRow      `json:"proposals,omitempty"`
	Events    []core.Event             `json:"events,omitempty"`
}
