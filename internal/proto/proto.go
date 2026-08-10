package proto

import (
	"encoding/json"

	"github.com/weston6142/watchtower/internal/archmap"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/engine"
	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/plannerbudget"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/scaffold"
	"github.com/weston6142/watchtower/internal/stageusage"
	"github.com/weston6142/watchtower/internal/store"
)

// maxMessageBytes bounds a single newline-delimited JSON message on the wire.
const maxMessageBytes = decision.MaxMessageBytes

type Command struct {
	Op       string `json:"op"`
	Title    string `json:"title,omitempty"`
	Body     string `json:"body,omitempty"`
	Flow     string `json:"flow,omitempty"`
	Preset   string `json:"preset,omitempty"`
	Priority int    `json:"priority,omitempty"`
	// Attach carries absolute paths, never bytes: maxMessageBytes bounds a wire
	// message well under the per-file cap, so the daemon reads the files off the
	// shared filesystem itself. A bare name retains an existing attachment.
	Attach        []string                `json:"attach,omitempty"`
	DependsOn     []string                `json:"depends_on,omitempty"`
	IssueID       string                  `json:"issue_id,omitempty"`
	Worktree      string                  `json:"worktree,omitempty"`
	AllowNoChange bool                    `json:"allow_no_change,omitempty"`
	DecisionID    int64                   `json:"decision_id,omitempty"`
	Option        *int                    `json:"option,omitempty"`
	Text          string                  `json:"text,omitempty"`
	Actor         string                  `json:"actor,omitempty"`
	ProposalID    int64                   `json:"proposal_id,omitempty"`
	Accept        bool                    `json:"accept,omitempty"`
	SinceSeq      int64                   `json:"since_seq,omitempty"`
	ThroughSeq    int64                   `json:"through_seq,omitempty"`
	Repo          string                  `json:"repo,omitempty"`
	Stage         string                  `json:"stage,omitempty"`
	Lever         string                  `json:"lever,omitempty"`
	Package       string                  `json:"package,omitempty"` // setup_prompt: which agent package
	N             int                     `json:"n,omitempty"`
	PlannerBudget *plannerbudget.Override `json:"planner_budget,omitempty"`
}

type Response struct {
	OK         bool                     `json:"ok"`
	Error      string                   `json:"error,omitempty"`
	IssueID    string                   `json:"issue_id,omitempty"`
	Decisions  []engine.PendingDecision `json:"decisions,omitempty"`
	Proposals  []store.ProposalRow      `json:"proposals,omitempty"`
	Issues     []store.IssueRow         `json:"issues,omitempty"`
	Backlog    []BacklogItem            `json:"backlog,omitempty"`
	Claims     []engine.Claim           `json:"claims,omitempty"`
	Claim      *engine.Claim            `json:"claim,omitempty"`
	Events     []core.Event             `json:"events,omitempty"`
	ThroughSeq int64                    `json:"through_seq,omitempty"`
	Lines      []string                 `json:"lines,omitempty"`
	Overview   *Overview                `json:"overview,omitempty"`
	Detail     *IssueDetail             `json:"detail,omitempty"`
	FlowStages []string                 `json:"flow_stages,omitempty"`
	Arch       *archmap.Map             `json:"arch,omitempty"`
	Setup      *SetupView               `json:"setup,omitempty"`
}

type AttachmentSummary struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type BacklogItem struct {
	Issue       store.IssueRow      `json:"issue"`
	Attachments []AttachmentSummary `json:"attachments,omitempty"`
	Claimable   bool                `json:"claimable"`
	BlockedBy   []string            `json:"blocked_by,omitempty"`
}

type Overview struct {
	Building            int                           `json:"building"`
	NeedYou             int                           `json:"need_you"`
	Failing             int                           `json:"failing"`
	Queued              int                           `json:"queued"`
	ShippedToday        int                           `json:"shipped_today"`
	TokensTotal         int                           `json:"tokens_total"`
	DollarsTotal        float64                       `json:"dollars_total"`
	ConfigurationHealth *scaffold.ConfigurationHealth `json:"configuration_health,omitempty"`
}

type IssueDetail struct {
	Issue            store.IssueRow       `json:"issue"`
	Runs             []store.StageRun     `json:"runs"`
	Attempts         []runner.Attempt     `json:"attempts,omitempty"`
	Tokens           int                  `json:"tokens"`
	Artifacts        []string             `json:"artifacts"`
	Model            string               `json:"model,omitempty"`
	Effort           string               `json:"effort,omitempty"`
	LastError        string               `json:"last_error,omitempty"`
	Attempt          int                  `json:"attempt,omitempty"`
	AttemptOf        int                  `json:"attempt_of,omitempty"`
	Budget           int                  `json:"budget,omitempty"`
	Levers           map[string]string    `json:"levers,omitempty"`
	Cleanup          []string             `json:"cleanup,omitempty"`
	Dollars          float64              `json:"dollars,omitempty"`
	IntegrationState string               `json:"integration_state,omitempty"`
	Worktree         string               `json:"worktree,omitempty"`
	Branch           string               `json:"branch,omitempty"`
	Planner          *stageusage.Snapshot `json:"planner,omitempty"`
	PlannerOutcome   string               `json:"planner_outcome,omitempty"`
	DecisionPage     string               `json:"decision_page,omitempty"`
	FailureHistory   FailureHistory       `json:"failure_history"`
}

type FailureHistory struct {
	Status  string                  `json:"status"`
	Records []failure.FailureRecord `json:"records,omitempty"`
}

func (history FailureHistory) MarshalJSON() ([]byte, error) {
	if history.Status == "unavailable" {
		return json.Marshal(struct {
			Status string `json:"status"`
		}{Status: history.Status})
	}
	records := history.Records
	if records == nil {
		records = make([]failure.FailureRecord, 0)
	}
	return json.Marshal(struct {
		Status  string                  `json:"status"`
		Records []failure.FailureRecord `json:"records"`
	}{Status: history.Status, Records: records})
}

// SetupView is the read-only picture of what the daemon is running: repo-level
// resolved config plus the flow's stages, agents, and packages. It reports the
// daemon's cached config, never the files on disk.
type SetupView struct {
	Flow                string                        `json:"flow"`
	IssueID             string                        `json:"issue_id,omitempty"`
	IssueTitle          string                        `json:"issue_title,omitempty"`
	Repo                RepoSetup                     `json:"repo"`
	Stages              []StageSetup                  `json:"stages"`
	ConfigurationHealth *scaffold.ConfigurationHealth `json:"configuration_health,omitempty"`
}

// RepoSetup is the repo-level config after flag overrides — what the daemon
// holds, not what config.yaml says.
type RepoSetup struct {
	Runner        string             `json:"runner"` // codex|claude|fake
	Slots         int                `json:"slots"`
	Budget        int                `json:"budget"` // 0 = off
	PricePerMTok  float64            `json:"price_per_mtok"`
	ClaudeBin     string             `json:"claude_bin"`
	CodexBin      string             `json:"codex_bin,omitempty"`
	CodexModel    string             `json:"codex_model,omitempty"`
	CodexEffort   string             `json:"codex_effort,omitempty"`
	CodexPolicy   string             `json:"codex_policy,omitempty"`
	CodexPrimary  *CodexProfileSetup `json:"codex_primary,omitempty"`
	CodexFallback *CodexProfileSetup `json:"codex_fallback,omitempty"`
	TestCmd       string             `json:"test_cmd,omitempty"`
	Pull          bool               `json:"pull"`
	Push          bool               `json:"push"`
	// Workspace is the resolved provider name — "treehouse", "git worktree",
	// or "" when the runner provisions none.
	Workspace string `json:"workspace"`
	// LoadedAt is pre-formatted "15:04", stamped once at daemon startup.
	// A string, not a time.Time, so render paths never call time.Now() and
	// the golden snapshots stay deterministic.
	LoadedAt string `json:"loaded_at"`
}

type CodexProfileSetup struct {
	Label            string          `json:"label"`
	Bin              string          `json:"bin"`
	Model            string          `json:"model"`
	Effort           string          `json:"effort"`
	FeatureOverrides map[string]bool `json:"feature_overrides,omitempty"`
	InitialArgv      []string        `json:"initial_argv,omitempty"`
	ResumedArgv      []string        `json:"resumed_argv,omitempty"`
}

type StageSetup struct {
	Name         string       `json:"name"`
	Gate         string       `json:"gate"`
	Workspace    string       `json:"workspace"` // none|worktree|readonly
	Parallel     bool         `json:"parallel"`
	Completion   string       `json:"completion"` // all|any
	HeavySlot    bool         `json:"heavy_slot"`
	MergeBarrier bool         `json:"merge_barrier"`
	Retries      int          `json:"retries"`
	Artifacts    []string     `json:"artifacts,omitempty"` // declared, not produced
	Lever        string       `json:"lever,omitempty"`     // issue-scoped only
	Agents       []AgentSetup `json:"agents"`
}

type AgentSetup struct {
	Package string `json:"package"`
	// Missing: the flow names this package but it is absent from the daemon's
	// loaded set. Every effective field below is empty when true.
	Missing bool `json:"missing,omitempty"`

	// Effective — what the CLI actually receives.
	Model          string   `json:"model,omitempty"`
	Effort         string   `json:"effort,omitempty"`
	ThinkingTokens string   `json:"thinking_tokens,omitempty"` // claude.ThinkingTokens value
	AllowedTools   []string `json:"allowed_tools,omitempty"`
	// Codex uses its full configured tool environment. Package allowed_tools
	// remains visible as Claude-only declaration metadata.
	DeclaredAllowedTools []string `json:"declared_allowed_tools,omitempty"`
	ToolSource           string   `json:"tool_source,omitempty"`

	// Declared but not applied — parsed by watchtower, never passed to the CLI.
	DeclaredModel string `json:"declared_model,omitempty"` // flow.AgentRef.Model
	MaxTurns      int    `json:"max_turns,omitempty"`      // pkgs.Package.MaxTurns

	PromptPreview []string `json:"prompt_preview,omitempty"` // ≤3 non-blank lines, ≤120 runes each
	PromptLines   int      `json:"prompt_lines,omitempty"`
}
