package proto

import (
	"github.com/wbushyeager/guildhall/internal/archmap"
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
	Repo       string `json:"repo,omitempty"`
	Stage      string `json:"stage,omitempty"`
	Lever      string `json:"lever,omitempty"`
	N          int    `json:"n,omitempty"`
}

type Response struct {
	OK         bool                     `json:"ok"`
	Error      string                   `json:"error,omitempty"`
	IssueID    string                   `json:"issue_id,omitempty"`
	Decisions  []engine.PendingDecision `json:"decisions,omitempty"`
	Proposals  []store.ProposalRow      `json:"proposals,omitempty"`
	Issues     []store.IssueRow         `json:"issues,omitempty"`
	Events     []core.Event             `json:"events,omitempty"`
	Lines      []string                 `json:"lines,omitempty"`
	Overview   *Overview                `json:"overview,omitempty"`
	Detail     *IssueDetail             `json:"detail,omitempty"`
	FlowStages []string                 `json:"flow_stages,omitempty"`
	Arch       *archmap.Map             `json:"arch,omitempty"`
}

type Overview struct {
	Building     int     `json:"building"`
	NeedYou      int     `json:"need_you"`
	Failing      int     `json:"failing"`
	Queued       int     `json:"queued"`
	ShippedToday int     `json:"shipped_today"`
	TokensTotal  int     `json:"tokens_total"`
	DollarsTotal float64 `json:"dollars_total"`
}

type IssueDetail struct {
	Issue     store.IssueRow    `json:"issue"`
	Runs      []store.StageRun  `json:"runs"`
	Tokens    int               `json:"tokens"`
	Artifacts []string          `json:"artifacts"`
	LastError string            `json:"last_error,omitempty"`
	Attempt   int               `json:"attempt,omitempty"`
	AttemptOf int               `json:"attempt_of,omitempty"`
	Budget    int               `json:"budget,omitempty"`
	Levers    map[string]string `json:"levers,omitempty"`
	Dollars   float64           `json:"dollars,omitempty"`
}
