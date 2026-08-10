package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/weston6142/watchtower/internal/attach"
	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/decisionpage"
	"github.com/weston6142/watchtower/internal/deps"
	"github.com/weston6142/watchtower/internal/evidence"
	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/librarian"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/plannerartifact"
	"github.com/weston6142/watchtower/internal/plannerbudget"
	"github.com/weston6142/watchtower/internal/repocfg"
	"github.com/weston6142/watchtower/internal/review"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/slots"
	"github.com/weston6142/watchtower/internal/stageusage"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/touchset"
	"github.com/weston6142/watchtower/internal/verificationcache"
	"github.com/weston6142/watchtower/internal/workspace"
)

var errDependenciesDiscovered = errors.New("new dependencies discovered")
var errConflictHeld = errors.New("merge conflict held")

type Config struct {
	Store              *store.Store
	FailureRecorder    failure.Recorder
	Runner             runner.Runner
	Marshal            Sequencer
	Train              *marshal.Train
	Librarian          *librarian.Librarian
	Observers          []func(core.Event)
	Pool               *slots.Pool
	Flows              map[string]flow.Flow
	Rules              levers.Rules
	PlanReview         review.PolicySettings
	DecisionIdentities map[string]decision.AgentIdentity
	DataDir            string
	CacheRoot          string
	Workspace          workspace.Provider
	TokenBudget        int
	PlannerBudget      plannerbudget.Profile
	OnLine             func(issueID, stage, line string)
}

type plannerExplorationGate struct {
	mu          sync.Mutex
	controller  *plannerbudget.Controller
	leases      map[string]stageusage.Lease
	nextLeaseID uint64
	onSnapshot  func(stageusage.Snapshot)
}

func (g *plannerExplorationGate) Admit(_ context.Context, call runner.ToolCall) (runner.ToolDecision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	lease, result := g.controller.AdmitSource(plannerbudget.Source{
		ID: call.SourceID, Fingerprint: call.Fingerprint, Reservation: call.Reservation,
	})
	if result.Err != nil {
		if g.onSnapshot != nil {
			g.onSnapshot(result.Snapshot)
		}
		if errors.Is(result.Err, stageusage.ErrAdmissionClosed) {
			return runner.ToolDecision{}, nil
		}
		return runner.ToolDecision{}, result.Err
	}
	if !result.Charged {
		return runner.ToolDecision{CachedContent: result.Content}, nil
	}
	g.nextLeaseID++
	leaseID := fmt.Sprintf("planner-lease-%d", g.nextLeaseID)
	if g.leases == nil {
		g.leases = make(map[string]stageusage.Lease)
	}
	g.leases[leaseID] = lease
	if g.onSnapshot != nil {
		g.onSnapshot(result.Snapshot)
	}
	return runner.ToolDecision{Allowed: true, LeaseID: leaseID}, nil
}

func (g *plannerExplorationGate) Complete(_ context.Context, decision runner.ToolDecision, actual *int64, operationErr error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	lease, ok := g.leases[decision.LeaseID]
	if !ok {
		return stageusage.ErrUnknownLease
	}
	delete(g.leases, decision.LeaseID)
	snapshot, err := g.controller.Reconcile(lease, actual, operationErr)
	if g.onSnapshot != nil {
		g.onSnapshot(snapshot)
	}
	return err
}

type Sequencer interface {
	BlockedBehind(string) int
	PlanApproved(string, touchset.Set)
	ReadyToMerge(context.Context, string) error
	Merged(string)
	Aborted(string)
}

type PendingDecision struct {
	ID           int64
	IssueID      string
	Stage        string
	D            levers.Decision
	Context      *decision.DecisionContext `json:"context,omitempty"`
	Review       *review.Target            `json:"review,omitempty"`
	ReviewPolicy *review.ResolvedPolicy    `json:"review_policy,omitempty"`
}

type pending struct {
	PendingDecision
	reply     chan levers.Response
	published bool
}

type issueState struct {
	id                  string
	title               string
	body                string
	taskSummary         string
	flowName            string
	matrix              levers.Matrix
	priority            int
	wsPath              string
	branch              string
	baseRef             string
	wsRelease           func() error
	pauseGate           chan struct{}
	stageCancel         context.CancelFunc
	killRequested       bool
	stageIdx            int
	paused              bool
	pauseRequested      bool
	pauseStage          int
	terminal            bool
	running             bool
	draft               bool
	claiming            bool
	claimed             bool
	externalSession     bool
	budgetWaived        bool
	activeTouchset      *touchset.Set
	dependsOn           []string
	waitingDependencies bool
	planReview          review.ResolvedPolicy
}

func planReviewLever(f flow.Flow, matrix levers.Matrix) flow.Lever {
	for _, stage := range f.Stages {
		if stage.Gate == flow.GatePlanReview {
			if lever := matrix[stage.Name]; lever != "" {
				return lever
			}
		}
	}
	if lever := matrix["plan"]; lever != "" {
		return lever
	}
	return flow.LeverRegular
}

func (e *Engine) resolvePlanReviewPolicy(flowName string, matrix levers.Matrix) review.ResolvedPolicy {
	f := e.cfg.Flows[flowName]
	mode := planReviewLever(f, matrix)
	settings := e.cfg.PlanReview
	if !settings.Valid && !settings.AutoApproveRegular && settings.ID == "" && settings.Version == "" {
		return review.ManualPlanReviewPolicy(mode)
	}
	return review.ResolvePlanReviewPolicy(mode, settings)
}

type Claim struct {
	IssueID    string `json:"issue_id"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	Priority   int    `json:"priority"`
	Flow       string `json:"flow"`
	Repository string `json:"repository"`
	Worktree   string `json:"worktree"`
	Branch     string `json:"branch"`
	BaseSHA    string `json:"base_sha"`
}

type FinishClaimRequest struct {
	IssueID       string
	Worktree      string
	AllowNoChange bool
}

type Engine struct {
	cfg                 Config
	mu                  sync.Mutex
	nextID              int
	issues              map[string]*issueState
	pend                map[int64]*pending
	reviewContinuations map[int64]bool
}

func New(cfg Config) *Engine {
	if cfg.FailureRecorder == nil {
		cfg.FailureRecorder = cfg.Store
	}
	if cfg.PlannerBudget == (plannerbudget.Profile{}) {
		cfg.PlannerBudget = plannerbudget.DefaultProfile()
	}
	if cfg.Train != nil && cfg.Train.CacheRoot == "" {
		cfg.Train.CacheRoot = cfg.CacheRoot
		if cfg.Train.CacheRoot == "" {
			cfg.Train.CacheRoot = cfg.DataDir
		}
	}
	e := &Engine{
		cfg: cfg, issues: map[string]*issueState{}, pend: map[int64]*pending{},
		reviewContinuations: map[int64]bool{},
	}
	if cfg.OnLine != nil {
		if sink, ok := cfg.Runner.(runner.LineSink); ok {
			sink.SetOnLine(cfg.OnLine)
		}
	}
	if reporter, ok := cfg.Runner.(runner.AttemptReporter); ok {
		reporter.SetAttemptSink(e)
	}
	return e
}

func (e *Engine) RecordAttempt(ctx context.Context, attempt runner.Attempt) error {
	if err := e.cfg.Store.RecordAttempt(ctx, attempt); err != nil {
		_ = e.recordBoundaryFailure(ctx, attempt.IssueID, attempt.Stage, 0,
			failure.SiteStore, failure.ClassUnavailable, failure.RetryNow, failure.StateStore, err)
		return err
	}
	e.emit(core.EvRunnerAttempt, attempt.IssueID, map[string]any{
		"operation_id":      attempt.OperationID,
		"attempt_kind":      string(attempt.Kind),
		"state":             string(attempt.State),
		"failure_class":     string(attempt.FailureClass),
		"fallback_consumed": attempt.Kind == runner.AttemptFallback && attempt.State != runner.AttemptFailed,
		"redacted_argv":     attempt.RedactedArgv,
		"session_id":        attempt.SessionID,
		"tokens":            attempt.Tokens,
	})
	return nil
}

func (e *Engine) LoadOperation(ctx context.Context, operationID string) ([]runner.Attempt, error) {
	return e.cfg.Store.LoadOperation(ctx, operationID)
}

func (e *Engine) rehydrateArtifactReview(
	row store.DecisionRow, issueRows map[string]store.IssueRow,
) (active, handoff bool, err error) {
	if row.Review == nil {
		return false, false, nil
	}
	issue, ok := issueRows[row.IssueID]
	if !ok {
		return false, false, fmt.Errorf("issue %s is missing", row.IssueID)
	}
	if issue.State == "done" || issue.State == "done (unmerged)" ||
		issue.State == "merged" || issue.State == "abandoned" {
		return false, false, nil
	}
	checkpoints, err := e.cfg.Store.StageCheckpoints(row.IssueID)
	if err != nil {
		return false, false, err
	}
	var checkpoint *store.StageCheckpoint
	for i := range checkpoints {
		if checkpoints[i].ID == row.Review.CheckpointID {
			checkpoint = &checkpoints[i]
			break
		}
	}
	if checkpoint == nil {
		return false, false, fmt.Errorf("checkpoint %d is missing", row.Review.CheckpointID)
	}
	statusActive := (row.Status == "pending" && checkpoint.Status == "awaiting_review") ||
		((row.Status == "answered" || row.Status == "auto") &&
			(checkpoint.Status == "handoff_authorized" || checkpoint.Status == "revision_required" || checkpoint.Status == "succeeded"))
	if !statusActive {
		return false, false, nil
	}
	current, err := (review.Target{
		IssueID: row.IssueID, Stage: row.Stage, CheckpointID: checkpoint.ID,
		Artifacts: checkpoint.Artifacts, NextStage: row.Review.NextStage,
	}).Canonical()
	if err != nil || !row.Review.Matches(current) {
		if err != nil {
			return false, false, fmt.Errorf("checkpoint %d target: %w", checkpoint.ID, err)
		}
		return false, false, review.ErrStaleTarget
	}
	f, ok := e.cfg.Flows[issue.Flow]
	if !ok {
		return false, false, fmt.Errorf("flow %q is not configured", issue.Flow)
	}
	stageIdx := -1
	for i, configured := range f.Stages {
		if configured.Name == row.Stage {
			stageIdx = i
			break
		}
	}
	if stageIdx < 0 {
		return false, false, fmt.Errorf("stage %q is not configured in flow %q", row.Stage, issue.Flow)
	}
	is := &issueState{
		id: row.IssueID, title: issue.Title, body: issue.Body, flowName: issue.Flow,
		matrix: matrixFromStrings(issue.Levers), priority: issue.Priority,
		dependsOn: append([]string(nil), issue.DependsOn...), stageIdx: stageIdx, terminal: true,
		planReview: issue.PlanReviewPolicy,
	}
	e.restoreInterruptedWorkspace(is)
	d := decisionFromRow(row)
	e.mu.Lock()
	e.issues[row.IssueID] = is
	if row.Status == "pending" {
		e.pend[row.ID] = &pending{
			PendingDecision: PendingDecision{
				ID: row.ID, IssueID: row.IssueID, Stage: row.Stage, D: d,
				Context: row.Context, Review: row.Review, ReviewPolicy: row.ReviewPolicy,
			},
			published: true,
		}
	}
	e.mu.Unlock()
	if row.Status == "pending" {
		if err := e.writeDecisionPage(is, row.Stage, row.ID, d, row.Context, row.Review, nil); err != nil {
			return false, false, fmt.Errorf("rehydrate pending decision page: %w", err)
		}
	}
	return true, (row.Status == "answered" || row.Status == "auto") &&
		(checkpoint.Status == "handoff_authorized" || checkpoint.Status == "succeeded"), nil
}

func decisionFromRow(row store.DecisionRow) levers.Decision {
	return levers.Decision{
		Kind: row.Kind, Question: row.Question, Options: row.Options,
		Recommended: row.Recommended, RecommendedResponse: row.RecommendedResponse,
		AllowFreeform: row.AllowFreeform, Importance: row.Importance, Paths: row.Paths,
		Why: row.Why, Consequences: row.Consequences, Reversible: row.Reversible,
		Briefing: row.Briefing, RequiresOption: row.RequiresOption || row.Review != nil,
		EngineContinuation: row.EngineContinuation,
	}
}

func (e *Engine) rebuildResolvedDecisionPages(
	rows []store.DecisionRow, issueRows map[string]store.IssueRow,
) (map[string]struct{}, error) {
	affected := map[string]struct{}{}
	ordered := append([]store.DecisionRow(nil), rows...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for _, row := range ordered {
		if row.Status != "answered" && row.Status != "auto" {
			continue
		}
		_, ok := issueRows[row.IssueID]
		if !ok {
			continue
		}
		affected[row.IssueID] = struct{}{}
		archivePath := filepath.Join(e.issueDir(row.IssueID), "decisions", fmt.Sprintf("%d.html", row.ID))
		needsResolution, err := decisionArchiveNeedsResolution(archivePath)
		if err != nil {
			return nil, fmt.Errorf("read decision archive %d: %w", row.ID, err)
		}
		if !needsResolution {
			continue
		}
		data, ok := resolvedDecisionPageSnapshot(row)
		if !ok {
			continue
		}
		content, renderErr := decisionpage.Render(data)
		if renderErr != nil {
			return nil, fmt.Errorf("render decision archive %d: %w", row.ID, renderErr)
		}
		if err := e.writeDecisionArchiveContent(row.IssueID, row.ID, content); err != nil {
			return nil, fmt.Errorf("write decision archive %d: %w", row.ID, err)
		}
	}
	return affected, nil
}

// Rehydrate rebuilds in-memory state from the store after a daemon restart.
// Non-terminal issues become terminal (retryable via RetryStage) — stages are
// never auto-restarted, so a reboot cannot spend tokens on its own. Pending
// decisions whose stage goroutine died with the old process are closed as
// "orphaned"; retrying the stage re-raises them. Paused and killed issues are
// rehydrated the same way: a pause gate with no parked goroutine is
// meaningless. Resolved decision archives that are missing or still marked
// pending are repaired from their frozen page snapshots; already-resolved
// archives are preserved. Idempotent.
//
// TODO: worktrees acquired by the previous daemon are never released; a
// retried stage acquires a fresh one and the old lease leaks.
func (e *Engine) Rehydrate() error {
	rows, err := e.cfg.Store.Issues()
	if err != nil {
		return err
	}

	// Restore the ID counter across every row — including terminal issues —
	// so CreateIssue never mints a colliding GH-n.
	maxID := 0
	for _, row := range rows {
		if n, ok := issueNumber(row.ID); ok && n > maxID {
			maxID = n
		}
	}
	e.mu.Lock()
	if maxID > e.nextID {
		e.nextID = maxID
	}
	e.mu.Unlock()

	issueRows := make(map[string]store.IssueRow, len(rows))
	for _, row := range rows {
		issueRows[row.ID] = row
	}
	activeReviewIssues := map[string]bool{}
	reviewCheckpointIDs := map[string]int64{}
	var authorizedReviews []store.DecisionRow
	decisionRows, err := e.cfg.Store.AllDecisionRows()
	if err != nil {
		return err
	}
	decisionPageIssues, err := e.rebuildResolvedDecisionPages(decisionRows, issueRows)
	if err != nil {
		return err
	}
	decisionRowsByIssue := make(map[string][]store.DecisionRow)
	for _, row := range decisionRows {
		decisionRowsByIssue[row.IssueID] = append(decisionRowsByIssue[row.IssueID], row)
	}
	var reviewRows []store.DecisionRow
	for _, row := range decisionRows {
		if row.Review != nil {
			reviewRows = append(reviewRows, row)
		}
	}
	for _, row := range reviewRows {
		if row.Review == nil {
			continue
		}
		if issue, ok := issueRows[row.IssueID]; ok && issue.State == "paused" {
			continue
		}
		if previous := reviewCheckpointIDs[row.IssueID]; previous >= row.Review.CheckpointID {
			continue
		}
		active, handoff, restoreErr := e.rehydrateArtifactReview(row, issueRows)
		if restoreErr != nil {
			activeReviewIssues[row.IssueID] = true
			e.emit(core.EvStageFailed, row.IssueID, map[string]any{
				"stage": row.Stage, "error": fmt.Sprintf("artifact review recovery blocked: %v", restoreErr), "final": true})
			continue
		}
		if !active {
			continue
		}
		reviewCheckpointIDs[row.IssueID] = row.Review.CheckpointID
		activeReviewIssues[row.IssueID] = true
		if handoff {
			authorizedReviews = append(authorizedReviews, row)
		}
	}

	// Close pending legacy decisions with no living stage goroutine. Artifact
	// reviews are durable and remain answerable through the restored pending map.
	prows, err := e.cfg.Store.PendingDecisionRows()
	if err != nil {
		return err
	}
	for _, row := range prows {
		if row.Review != nil {
			continue
		}
		e.mu.Lock()
		_, alive := e.pend[row.ID]
		e.mu.Unlock()
		if alive {
			continue
		}
		_ = e.cfg.Store.CloseDecision(row.ID, "orphaned")
		e.emit(core.EvDecisionAnswered, row.IssueID, map[string]any{
			"decision_id": row.ID, "option": -1, "orphaned": true})
	}

	for _, row := range rows {
		if activeReviewIssues[row.ID] {
			continue
		}
		if row.State == "done" || row.State == "done (unmerged)" || row.State == "merged" || row.State == "abandoned" {
			continue
		}
		if row.State == "paused" {
			e.mu.Lock()
			_, known := e.issues[row.ID]
			e.mu.Unlock()
			if known {
				continue
			}
			is, err := e.restorePersistedRun(row)
			if err != nil {
				return err
			}
			e.mu.Lock()
			e.issues[row.ID] = is
			e.mu.Unlock()
			continue
		}
		integration, hasIntegration, err := e.cfg.Store.IssueIntegration(row.ID)
		if err != nil {
			return err
		}
		if hasIntegration && integration.State == store.IntegrationClaimed &&
			(row.State == "claimed" || row.State == "failed") {
			finalIdx := 0
			if row.State == "failed" {
				var finalErr error
				finalIdx, finalErr = finalStageIndex(e.cfg.Flows[row.Flow])
				if finalErr != nil {
					return finalErr
				}
			}
			is := &issueState{
				id: row.ID, title: row.Title, body: row.Body, flowName: row.Flow,
				matrix: matrixFromStrings(row.Levers), priority: row.Priority,
				dependsOn: append([]string(nil), row.DependsOn...),
				claimed:   row.State == "claimed", externalSession: true,
				terminal: row.State == "failed", stageIdx: finalIdx, planReview: row.PlanReviewPolicy,
			}
			if err := e.restoreClaimedWorkspace(is, integration); err != nil {
				return fmt.Errorf("restore claim %s: %w", row.ID, err)
			}
			e.mu.Lock()
			e.issues[row.ID] = is
			e.mu.Unlock()
			continue
		}
		if hasIntegration && integration.State == store.IntegrationVerificationReady {
			is := &issueState{
				id: row.ID, title: row.Title, body: row.Body, flowName: row.Flow,
				matrix: matrixFromStrings(row.Levers), priority: row.Priority,
				dependsOn: append([]string(nil), row.DependsOn...), running: true,
				planReview: row.PlanReviewPolicy,
			}
			e.mu.Lock()
			e.issues[row.ID] = is
			e.mu.Unlock()
			go func(integration store.IssueIntegration) {
				err := e.retryVerifiedFinalization(context.Background(), is, integration)
				e.mu.Lock()
				is.running = false
				is.terminal = err != nil
				if err == nil {
					is.wsRelease = nil
					is.wsPath = ""
					is.branch = ""
					is.baseRef = ""
				}
				e.mu.Unlock()
			}(integration)
			continue
		}
		run, hasRun, err := e.cfg.Store.LoadRunState(row.ID)
		if err != nil {
			return err
		}
		if hasRun && run.Lifecycle == "active" &&
			(row.State == "running" || strings.HasPrefix(row.State, "running:")) {
			e.mu.Lock()
			_, known := e.issues[row.ID]
			e.mu.Unlock()
			if known {
				continue
			}
			is, err := e.restorePersistedRun(row)
			if err != nil {
				return err
			}
			stage, attempt, of, _, _ := e.cfg.Store.LastStageEvents(row.ID)
			if stage == "" {
				stage = run.Stage
			}
			e.mu.Lock()
			e.issues[row.ID] = is
			e.mu.Unlock()
			e.emit(core.EvStageFailed, row.ID, map[string]any{
				"stage": stage, "attempt": attempt, "of": of,
				"error": "daemon restarted — press R to retry", "final": true})
			continue
		}
		if row.State == "backlog" {
			e.mu.Lock()
			if _, known := e.issues[row.ID]; !known {
				matrix := levers.Matrix{}
				for st, lv := range row.Levers {
					matrix[st] = flow.Lever(lv)
				}
				e.issues[row.ID] = &issueState{
					id: row.ID, title: row.Title, body: row.Body, flowName: row.Flow,
					matrix: matrix, priority: row.Priority, draft: true, planReview: row.PlanReviewPolicy,
				}
			}
			e.mu.Unlock()
			continue
		}
		if row.State == "waiting_dependencies" {
			e.mu.Lock()
			e.issues[row.ID] = &issueState{
				id: row.ID, title: row.Title, body: row.Body, flowName: row.Flow,
				matrix: matrixFromStrings(row.Levers), priority: row.Priority,
				dependsOn: append([]string(nil), row.DependsOn...), waitingDependencies: true,
				planReview: row.PlanReviewPolicy,
			}
			e.mu.Unlock()
			continue
		}
		e.mu.Lock()
		_, known := e.issues[row.ID]
		e.mu.Unlock()
		if known {
			continue
		}
		persistedAttempt, hasPersistedAttempt, err := e.latestRunnerAttempt(row.ID)
		if err != nil {
			return err
		}
		// Best-effort: on error or missing events the zero values fall back
		// to the flow's first stage below.
		stage, attempt, of, _, _ := e.cfg.Store.LastStageEvents(row.ID)
		f, ok := e.cfg.Flows[row.Flow]
		if !ok || len(f.Stages) == 0 {
			// Never insert an issue whose flow can't run: a retry through a
			// zero-stage flow would fake-complete it. Surface it instead.
			e.emit(core.EvStageFailed, row.ID, map[string]any{
				"stage": stage, "error": fmt.Sprintf("flow %q no longer configured", row.Flow), "final": true})
			continue
		}
		stageIdx := 0
		for i, st := range f.Stages {
			if st.Name == stage {
				stageIdx = i
				break
			}
		}
		if stage == "" {
			stage = f.Stages[0].Name
		}
		matrix := levers.Matrix{}
		for st, lv := range row.Levers {
			matrix[st] = flow.Lever(lv)
		}
		is := &issueState{
			id: row.ID, title: row.Title, body: row.Body, flowName: row.Flow,
			matrix: matrix, priority: row.Priority, stageIdx: stageIdx, terminal: true,
			planReview: row.PlanReviewPolicy,
		}
		restartError := "daemon restarted — press R to retry"
		if hasPersistedAttempt {
			stage = persistedAttempt.Stage
			if persistedAttempt.State == runner.AttemptRunning || persistedAttempt.State == runner.AttemptReserved {
				interrupted := persistedAttempt
				interrupted.State = runner.AttemptTerminal
				interrupted.FailureClass = runner.FailureCancellation
				if err := e.RecordAttempt(context.Background(), interrupted); err != nil {
					return err
				}
				restartError = fmt.Sprintf("daemon restarted during %s %s attempt — press R to retry", persistedAttempt.Stage, persistedAttempt.Kind)
			} else if persistedAttempt.State == runner.AttemptTerminal {
				restartError = fmt.Sprintf("daemon restarted after %s %s attempt reached terminal failure — press R to retry", persistedAttempt.Stage, persistedAttempt.Kind)
			}
		}
		e.restoreInterruptedWorkspace(is)
		e.mu.Lock()
		e.issues[row.ID] = is
		e.mu.Unlock()
		e.emit(core.EvStageFailed, row.ID, map[string]any{
			"stage": stage, "attempt": attempt, "of": of,
			"error": restartError, "final": true})
	}
	for issueID := range decisionPageIssues {
		if row, ok := issueRows[issueID]; ok {
			e.refreshDecisionPageFromRow(row, decisionRowsByIssue[issueID])
		}
	}
	for _, row := range authorizedReviews {
		e.scheduleArtifactContinuation(row.ID, row.IssueID, row.Stage, *row.Review, row.Response, true)
	}
	return nil
}

func (e *Engine) latestRunnerAttempt(issueID string) (runner.Attempt, bool, error) {
	runs, err := e.cfg.Store.StageRuns(issueID)
	if err != nil {
		return runner.Attempt{}, false, err
	}
	for index := len(runs) - 1; index >= 0; index-- {
		attempts, err := e.cfg.Store.LoadOperation(context.Background(), strconv.FormatInt(runs[index].ID, 10))
		if err != nil {
			return runner.Attempt{}, false, err
		}
		if len(attempts) == 0 {
			continue
		}
		latest := attempts[0]
		for _, attempt := range attempts[1:] {
			if attempt.Kind == runner.AttemptFallback {
				latest = attempt
			}
		}
		return latest, true, nil
	}
	return runner.Attempt{}, false, nil
}

func matrixFromStrings(values map[string]string) levers.Matrix {
	matrix := levers.Matrix{}
	for stage, value := range values {
		matrix[stage] = flow.Lever(value)
	}
	return matrix
}

func (e *Engine) restorePersistedRun(row store.IssueRow) (*issueState, error) {
	run, ok, err := e.cfg.Store.LoadRunState(row.ID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("paused or active issue %s has no persisted run state", row.ID)
	}
	if run.Lifecycle != "paused" && run.Lifecycle != "active" {
		return nil, fmt.Errorf("issue %s has unsupported run lifecycle %q", row.ID, run.Lifecycle)
	}
	f, ok := e.cfg.Flows[row.Flow]
	if !ok {
		return nil, fmt.Errorf("flow %q is not configured", row.Flow)
	}
	if run.StageIndex < 0 || run.StageIndex >= len(f.Stages) {
		return nil, fmt.Errorf("run state for %s has invalid stage index %d", row.ID, run.StageIndex)
	}
	if f.Stages[run.StageIndex].Name != run.Stage {
		return nil, fmt.Errorf("run state for %s names stage %q at index %d", row.ID, run.Stage, run.StageIndex)
	}
	is := &issueState{
		id: row.ID, title: row.Title, body: row.Body, flowName: row.Flow,
		matrix: matrixFromStrings(row.Levers), priority: row.Priority,
		dependsOn: append([]string(nil), row.DependsOn...), stageIdx: run.StageIndex,
		terminal: true, paused: run.Lifecycle == "paused", pauseStage: run.StageIndex,
		planReview: row.PlanReviewPolicy,
	}
	if err := e.restorePersistedWorkspace(is, run); err != nil {
		return nil, fmt.Errorf("restore run %s: %w", row.ID, err)
	}
	return is, nil
}

func (e *Engine) SetDependencies(issueID string, parents []string) error {
	parents = deps.Normalize(parents)
	graph, err := e.cfg.Store.DependencyGraph()
	if err != nil {
		return err
	}
	graph[issueID] = append([]string(nil), parents...)
	if err := deps.Graph(graph).Validate(); err != nil {
		return err
	}
	if err := e.cfg.Store.ReplaceDependencies(issueID, parents); err != nil {
		return err
	}
	e.mu.Lock()
	if is := e.issues[issueID]; is != nil {
		is.dependsOn = append([]string(nil), parents...)
	}
	e.mu.Unlock()
	return nil
}

func (e *Engine) unmetDependencies(issueID string) ([]string, error) {
	rows, err := e.cfg.Store.Issues()
	if err != nil {
		return nil, err
	}
	states := map[string]string{}
	var parents []string
	for _, row := range rows {
		states[row.ID] = row.State
		if row.ID == issueID {
			parents = row.DependsOn
		}
	}
	var unmet []string
	for _, parent := range parents {
		if states[parent] != "merged" {
			unmet = append(unmet, parent)
		}
	}
	return unmet, nil
}

func (e *Engine) startOrWait(ctx context.Context, is *issueState) error {
	return e.startOrWaitWithPlannerBudget(ctx, is, nil)
}

func (e *Engine) startOrWaitWithPlannerBudget(ctx context.Context, is *issueState, plannerOverride *plannerbudget.Override) error {
	unmet, err := e.unmetDependencies(is.id)
	if err != nil {
		return err
	}
	if len(unmet) > 0 {
		e.mu.Lock()
		is.waitingDependencies = true
		e.mu.Unlock()
		e.emit(core.EvIssueWaitingDependencies, is.id, map[string]any{"unmet": unmet})
		return nil
	}
	return e.runAndRecordWithPlannerBudget(ctx, is, 0, plannerOverride)
}

func (e *Engine) wakeDependents(ctx context.Context, mergedID string) {
	ids, err := e.cfg.Store.Dependents(mergedID)
	if err != nil {
		return
	}
	for _, id := range ids {
		e.mu.Lock()
		is := e.issues[id]
		waiting := is != nil && is.waitingDependencies
		e.mu.Unlock()
		if !waiting {
			continue
		}
		unmet, err := e.unmetDependencies(id)
		if err != nil || len(unmet) != 0 {
			continue
		}
		e.mu.Lock()
		is.waitingDependencies = false
		e.mu.Unlock()
		e.emit(core.EvIssueDependenciesSatisfied, id, nil)
		go e.runAndRecord(ctx, is, 0)
	}
}

// issueNumber extracts n from a GH-n issue ID.
func issueNumber(id string) (int, bool) {
	rest, ok := strings.CutPrefix(id, "GH-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return n, true
}

// ActiveTouchsets returns a snapshot of plans that are still in flight.
func (e *Engine) ActiveTouchsets() map[string][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string][]string{}
	for id, issue := range e.issues {
		if issue.activeTouchset != nil {
			out[id] = append([]string(nil), issue.activeTouchset.Globs...)
		}
	}
	return out
}

// CanReset reports whether replacing the repository configuration is safe.
// Inactive durable states survive a daemon restart; a live stage or decision
// session does not.
func (e *Engine) CanReset() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.pend) > 0 {
		return fmt.Errorf("cannot reset while %d pending decision(s) are live", len(e.pend))
	}
	for id, issue := range e.issues {
		if issue.running {
			return fmt.Errorf("cannot reset while issue %s has a running stage", id)
		}
	}
	return nil
}

// Pause stops an issue at the next boundary between stages.
func (e *Engine) Pause(issueID string) error {
	e.mu.Lock()
	is, ok := e.issues[issueID]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("unknown issue %s", issueID)
	}
	if is.paused || is.pauseRequested {
		e.mu.Unlock()
		return nil
	}
	previousGate := is.pauseGate
	previousRequested := is.pauseRequested
	previousPaused := is.paused
	previousStage := is.pauseStage
	if is.pauseGate == nil {
		is.pauseGate = make(chan struct{})
	}
	is.pauseRequested = true
	is.pauseStage = is.stageIdx
	if is.running {
		is.pauseStage++
	}
	gate := is.pauseGate
	stageIdx := is.pauseStage
	e.mu.Unlock()

	run, err := e.runState(is, stageIdx, "paused", "before_stage")
	if err == nil {
		err = e.cfg.Store.PersistPausedRun(run)
	}
	if err != nil {
		e.mu.Lock()
		if is.pauseGate == gate {
			is.pauseGate = previousGate
			is.pauseRequested = previousRequested
			is.paused = previousPaused
			is.pauseStage = previousStage
		}
		e.mu.Unlock()
		return err
	}
	e.mu.Lock()
	if is.pauseGate == gate {
		is.paused = true
	}
	e.mu.Unlock()
	return nil
}

// Resume continues a stopped lane. A lane whose runFrom goroutine is alive is
// released from its between-stage gate. A lane whose goroutine has already
// exited — killed, aborted, or rehydrated after a daemon restart — has no
// waiter to release, so it is restarted at the stage it stopped on instead;
// closing the gate there would report success and do nothing.
func (e *Engine) Resume(issueID string) error {
	e.mu.Lock()
	is, ok := e.issues[issueID]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("unknown issue %s", issueID)
	}
	running := is.running
	gate := is.pauseGate
	terminal := is.terminal
	startIdx := is.stageIdx
	paused := is.paused || is.pauseRequested
	e.mu.Unlock()
	if running {
		if gate == nil || !paused {
			return fmt.Errorf("issue %s is not paused", issueID)
		}
		if err := e.persistResumeState(is, startIdx); err != nil {
			return err
		}
		e.mu.Lock()
		if is.pauseGate == gate {
			close(gate)
			is.pauseGate = nil
			is.pauseRequested = false
			is.paused = false
		}
		e.mu.Unlock()
		return nil
	}
	// terminal, not merely stopped: a lane created and paused before it ever
	// started must stay unstarted, and only clear its gate.
	if !terminal {
		if !paused {
			return nil
		}
		if err := e.persistResumeState(is, startIdx); err != nil {
			return err
		}
		e.mu.Lock()
		is.pauseGate = nil
		is.pauseRequested = false
		is.paused = false
		e.mu.Unlock()
		return nil
	}
	if err := e.persistResumeState(is, startIdx); err != nil {
		return err
	}
	e.mu.Lock()
	is.pauseGate = nil
	is.pauseRequested = false
	is.paused = false
	is.killRequested = false
	is.terminal = false
	e.mu.Unlock()
	// Detached, like start_issue and retry_stage: failures surface as
	// stage_failed events, not in the caller's response.
	go e.runAndRecord(context.Background(), is, startIdx)
	return nil
}

func (e *Engine) runState(is *issueState, stageIdx int, lifecycle, boundary string) (store.RunState, error) {
	e.mu.Lock()
	flowName := is.flowName
	issueID := is.id
	worktree := is.wsPath
	branch := is.branch
	baseRef := is.baseRef
	e.mu.Unlock()
	f, ok := e.cfg.Flows[flowName]
	if !ok {
		return store.RunState{}, fmt.Errorf("flow %q is not configured", flowName)
	}
	if stageIdx < 0 || stageIdx >= len(f.Stages) {
		return store.RunState{}, fmt.Errorf("stage index %d is outside flow %q", stageIdx, flowName)
	}
	artifacts, err := e.cfg.Store.ArtifactPaths(issueID)
	if err != nil {
		return store.RunState{}, err
	}
	return store.RunState{
		IssueID: issueID, Lifecycle: lifecycle, Stage: f.Stages[stageIdx].Name,
		StageIndex: stageIdx, Boundary: boundary, Worktree: worktree,
		Branch: branch, BaseRef: baseRef, Artifacts: artifacts,
	}, nil
}

func (e *Engine) resumeRunState(is *issueState, stageIdx int) (store.RunState, error) {
	run, ok, err := e.cfg.Store.LoadRunState(is.id)
	if err != nil {
		return store.RunState{}, err
	}
	if ok {
		return run, nil
	}
	return e.runState(is, stageIdx, "active", "in_stage")
}

func (e *Engine) persistResumeState(is *issueState, stageIdx int) error {
	run, err := e.resumeRunState(is, stageIdx)
	if err != nil {
		return err
	}
	return e.cfg.Store.ResumeRun(run)
}

func (e *Engine) persistActiveRun(is *issueState, stageIdx int) error {
	run, err := e.runState(is, stageIdx, "active", "in_stage")
	if err != nil {
		return err
	}
	return e.cfg.Store.PersistActiveRun(run)
}

// KillStage cancels only the currently running stage and leaves the issue
// paused so an operator can intervene before resuming or retrying it.
func (e *Engine) KillStage(issueID string) error {
	e.mu.Lock()
	is, ok := e.issues[issueID]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("unknown issue %s", issueID)
	}
	if is.stageCancel == nil {
		e.mu.Unlock()
		return fmt.Errorf("issue %s has no running stage", issueID)
	}
	is.killRequested = true
	if is.pauseGate == nil {
		is.pauseGate = make(chan struct{})
	}
	cancel := is.stageCancel
	killed := e.dropPendingDecisions(issueID)
	e.mu.Unlock()
	e.markDecisionsKilled(killed)
	cancel()
	return nil
}

// dropPendingDecisions unblocks and forgets every in-memory decision waiting
// on issueID, returning their ids. The caller must hold e.mu.
func (e *Engine) dropPendingDecisions(issueID string) []int64 {
	var killed []int64
	for id, p := range e.pend {
		if p.IssueID != issueID {
			continue
		}
		delete(e.pend, id)
		if p.reply != nil {
			close(p.reply)
		}
		killed = append(killed, id)
	}
	return killed
}

// markDecisionsKilled records the killed outcome in the store best-effort;
// it must be called without e.mu held.
func (e *Engine) markDecisionsKilled(ids []int64) {
	for _, id := range ids {
		_ = e.cfg.Store.CloseDecision(id, "killed")
	}
}

// Abandon removes an issue for good: any running stage is cancelled, its
// pending decisions are closed, and the lane disappears from every surface
// via EvIssueAbandoned. Abandon is a state, not a purge — rows, events, and
// artifacts stay in the store. Attachments are the one exception: their bytes
// and rows go, because nothing left on an abandoned lane reads them.
func (e *Engine) Abandon(issueID string) error {
	e.mu.Lock()
	is, ok := e.issues[issueID]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("unknown issue %s", issueID)
	}
	is.killRequested = true
	cancel := is.stageCancel
	killed := e.dropPendingDecisions(issueID)
	delete(e.issues, issueID)
	e.mu.Unlock()
	e.markDecisionsKilled(killed)
	if cancel != nil {
		cancel()
	}
	e.emit(core.EvIssueAbandoned, issueID, nil)
	// The first on-disk deletion in Abandon, scoped deliberately: attachment
	// bytes and rows only. The issues row, events, and stage artifacts stay so
	// an abandoned lane is still inspectable. Failure is logged, never
	// returned — Abandon must stay idempotent and must not half-abandon an
	// issue because a file was locked.
	if err := attach.DeleteAll(e.issueDir(issueID)); err != nil {
		fmt.Fprintf(os.Stderr, "watchtower: abandon %s: remove attachments: %v\n", issueID, err)
	}
	if err := e.cfg.Store.DeleteAttachments(issueID); err != nil {
		fmt.Fprintf(os.Stderr, "watchtower: abandon %s: delete attachment rows: %v\n", issueID, err)
	}
	return nil
}

// RequeueIssue returns an abandoned issue to the backlog under the same ID.
// The abandoned work stays discarded; a later launch starts the flow again
// from its first stage in a fresh workspace.
func (e *Engine) RequeueIssue(issueID string) error {
	rows, err := e.cfg.Store.Issues()
	if err != nil {
		return err
	}
	var row *store.IssueRow
	for i := range rows {
		if rows[i].ID == issueID {
			row = &rows[i]
			break
		}
	}
	if row == nil {
		return fmt.Errorf("unknown issue %s", issueID)
	}
	if row.State != "abandoned" {
		return fmt.Errorf("issue %s is not abandoned", issueID)
	}
	if _, ok := e.cfg.Flows[row.Flow]; !ok {
		return fmt.Errorf("unknown flow %q", row.Flow)
	}
	if discarder, ok := e.cfg.Workspace.(workspace.IssueDiscarder); ok {
		if err := discarder.DiscardIssue(issueID); err != nil {
			return fmt.Errorf("discard abandoned issue workspace: %w", err)
		}
	}
	for _, name := range []string{"artifacts", "evidence"} {
		if err := os.RemoveAll(filepath.Join(e.issueDir(issueID), name)); err != nil {
			return fmt.Errorf("discard abandoned issue %s: %w", name, err)
		}
	}
	if err := e.cfg.Store.DiscardIssueRun(issueID); err != nil {
		return fmt.Errorf("discard abandoned issue run state: %w", err)
	}

	is := &issueState{
		id: issueID, title: row.Title, body: row.Body, flowName: row.Flow,
		matrix: matrixFromStrings(row.Levers), priority: row.Priority,
		dependsOn: append([]string(nil), row.DependsOn...), draft: true,
		planReview: row.PlanReviewPolicy,
	}
	e.mu.Lock()
	if current, ok := e.issues[issueID]; ok && current.running {
		e.mu.Unlock()
		return fmt.Errorf("issue %s is still running", issueID)
	}
	e.issues[issueID] = is
	e.mu.Unlock()

	row.State = "backlog"
	if err := e.cfg.Store.UpsertIssue(*row); err != nil {
		e.mu.Lock()
		if e.issues[issueID] == is {
			delete(e.issues, issueID)
		}
		e.mu.Unlock()
		return err
	}
	e.emit(core.EvIssueDrafted, issueID, map[string]any{
		"title": row.Title, "body": row.Body, "flow": row.Flow,
		"priority": row.Priority, "levers": row.Levers,
		"depends_on": row.DependsOn, "requeued": true,
	})
	return nil
}

func (e *Engine) wasKilled(is *issueState) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return is.killRequested
}

// emit appends an event best-effort: marshal or store failures are dropped
// rather than aborting the stage, since events are observability, not state.
func (e *Engine) emit(t core.EventType, issueID string, payload any) {
	_, _ = e.appendEvent(t, issueID, payload)
}

func (e *Engine) appendEvent(t core.EventType, issueID string, payload any) (core.Event, error) {
	ev, err := e.storeEvent(t, issueID, payload)
	if err != nil {
		return core.Event{}, err
	}
	e.notifyObservers(ev)
	return ev, nil
}

func (e *Engine) storeEvent(t core.EventType, issueID string, payload any) (core.Event, error) {
	ev, err := core.NewEvent(t, issueID, payload)
	if err != nil {
		return core.Event{}, err
	}
	return e.cfg.Store.Append(ev)
}

func (e *Engine) notifyObservers(ev core.Event) {
	for _, observer := range e.cfg.Observers {
		observer(ev)
	}
}

var errDecisionPublicationCanceled = errors.New("decision publication canceled")

func (e *Engine) registerPendingDecision(is *issueState, p *pending) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	current, ok := e.issues[p.IssueID]
	if !ok || current.killRequested || is.killRequested {
		return false
	}
	e.pend[p.ID] = p
	return true
}

func (e *Engine) unregisterPendingDecision(p *pending) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pend[p.ID] != p {
		return false
	}
	delete(e.pend, p.ID)
	return true
}

func (e *Engine) publishPendingDecision(p *pending, payload map[string]any) error {
	e.mu.Lock()
	if e.pend[p.ID] != p {
		e.mu.Unlock()
		return errDecisionPublicationCanceled
	}
	ev, err := e.storeEvent(core.EvDecisionRequired, p.IssueID, payload)
	if err != nil {
		delete(e.pend, p.ID)
		e.mu.Unlock()
		return err
	}
	p.published = true
	e.mu.Unlock()
	e.notifyObservers(ev)
	return nil
}

func (e *Engine) publishPendingPlanReview(
	p *pending, requestedPayload, decisionPayload map[string]any,
) error {
	requested, err := core.NewEvent(core.EvPlanReviewRequested, p.IssueID, requestedPayload)
	if err != nil {
		return err
	}
	required, err := core.NewEvent(core.EvDecisionRequired, p.IssueID, decisionPayload)
	if err != nil {
		return err
	}
	e.mu.Lock()
	if e.pend[p.ID] != p {
		e.mu.Unlock()
		return errDecisionPublicationCanceled
	}
	events, err := e.cfg.Store.AppendBatch(requested, required)
	if err != nil {
		delete(e.pend, p.ID)
		e.mu.Unlock()
		return err
	}
	p.published = true
	e.mu.Unlock()
	for _, event := range events {
		e.notifyObservers(event)
	}
	return nil
}

func (e *Engine) cancelPendingDecisionPublication(issueID string, decisionID int64) {
	_ = e.cfg.Store.CloseDecision(decisionID, "killed")
	_ = e.removePendingDecisionPublicationPages(issueID, decisionID)
}

func (e *Engine) removePendingDecisionPublicationPages(issueID string, decisionID int64) error {
	paths := []string{
		filepath.Join(e.issueDir(issueID), "decisions", fmt.Sprintf("%d.html", decisionID)),
		filepath.Join(e.issueDir(issueID), decisionpage.FileName),
	}
	var removeErrors []error
	for _, pagePath := range paths {
		if err := os.Remove(pagePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			removeErrors = append(removeErrors, err)
		}
	}
	e.refreshDecisionPage(issueID)
	return errors.Join(removeErrors...)
}

func (e *Engine) rollbackPendingDecisionPublication(issueID string, decisionID int64) error {
	if err := e.cfg.Store.DeleteDecision(decisionID); err != nil {
		return err
	}
	return e.removePendingDecisionPublicationPages(issueID, decisionID)
}

// issueDir is where an issue's attachments and "none"-stage artifacts live.
func (e *Engine) issueDir(id string) string {
	return filepath.Join(e.cfg.DataDir, id)
}

// rollbackCreate undoes a half-created issue: a failed attach must not burn an
// ID or leave an issue in memory. nextID is deliberately not rewound — IDs stay
// monotonic so a rehydrate can never mint a colliding GH-n.
func (e *Engine) rollbackCreate(id string) {
	e.mu.Lock()
	delete(e.issues, id)
	e.mu.Unlock()
	_ = attach.DeleteAll(e.issueDir(id))
	_ = e.cfg.Store.DeleteAttachments(id)
	_ = e.cfg.Store.DeleteIssue(id)
}

func (e *Engine) CreateIssue(title, body, flowName string, m levers.Matrix, priority int, attachments []string) (string, error) {
	return e.CreateIssueWithDependencies(title, body, flowName, m, priority, attachments, nil)
}

func (e *Engine) CreateIssueWithDependencies(title, body, flowName string, m levers.Matrix,
	priority int, attachments, dependsOn []string) (string, error) {
	if _, ok := e.cfg.Flows[flowName]; !ok {
		return "", fmt.Errorf("unknown flow %q", flowName)
	}
	// Validate before allocating: a refusal here costs no ID and creates no dir.
	set, err := attach.Plan(nil, attachments)
	if err != nil {
		return "", err
	}
	planReview := e.resolvePlanReviewPolicy(flowName, m)
	e.mu.Lock()
	e.nextID++
	id := fmt.Sprintf("GH-%d", e.nextID)
	e.issues[id] = &issueState{
		id: id, title: title, body: body, flowName: flowName, matrix: m, priority: priority,
		planReview: planReview,
	}
	e.mu.Unlock()
	if err := attach.Save(e.issueDir(id), set); err != nil {
		e.rollbackCreate(id)
		return "", err
	}
	if err := e.cfg.Store.UpsertIssue(store.IssueRow{
		ID: id, Title: title, Body: body, Flow: flowName, State: "running", Levers: matrixStrings(m), Priority: priority,
		PlanReviewPolicy: planReview,
	}); err != nil {
		e.rollbackCreate(id)
		return "", err
	}
	if err := e.cfg.Store.ReplaceAttachments(id, attach.Rows(id, set, time.Now().UTC())); err != nil {
		e.rollbackCreate(id)
		return "", err
	}
	if err := e.SetDependencies(id, deps.Normalize(dependsOn)); err != nil {
		e.rollbackCreate(id)
		return "", err
	}
	e.emit(core.EvIssueCreated, id, map[string]any{
		"title": title, "flow": flowName, "body": body, "priority": priority,
		"attachments": set.Names(), "depends_on": deps.Normalize(dependsOn)})
	return id, nil
}

// DraftIssue records an issue in the backlog without starting anything: no
// flow run, no slot. The draft is durable and editable until launched.
func (e *Engine) DraftIssue(title, body, flowName, preset string, m levers.Matrix, priority int, attachments []string) (string, error) {
	return e.DraftIssueWithDependencies(title, body, flowName, preset, m, priority, attachments, nil)
}

func (e *Engine) DraftIssueWithDependencies(title, body, flowName, preset string, m levers.Matrix,
	priority int, attachments, dependsOn []string) (string, error) {
	if _, ok := e.cfg.Flows[flowName]; !ok {
		return "", fmt.Errorf("unknown flow %q", flowName)
	}
	set, err := attach.Plan(nil, attachments)
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	e.nextID++
	id := fmt.Sprintf("GH-%d", e.nextID)
	e.issues[id] = &issueState{id: id, title: title, body: body, flowName: flowName, matrix: m, priority: priority, draft: true}
	e.mu.Unlock()
	if err := attach.Save(e.issueDir(id), set); err != nil {
		e.rollbackCreate(id)
		return "", err
	}
	if err := e.cfg.Store.UpsertIssue(store.IssueRow{
		ID: id, Title: title, Body: body, Flow: flowName, State: "backlog", Levers: matrixStrings(m), Priority: priority,
	}); err != nil {
		e.rollbackCreate(id)
		return "", err
	}
	if err := e.cfg.Store.ReplaceAttachments(id, attach.Rows(id, set, time.Now().UTC())); err != nil {
		e.rollbackCreate(id)
		return "", err
	}
	if err := e.SetDependencies(id, deps.Normalize(dependsOn)); err != nil {
		e.rollbackCreate(id)
		return "", err
	}
	e.emit(core.EvIssueDrafted, id, map[string]any{
		"title": title, "body": body, "flow": flowName, "preset": preset,
		"priority": priority, "levers": matrixStrings(m), "attachments": set.Names(),
		"depends_on": deps.Normalize(dependsOn)})
	return id, nil
}

// UpdateIssue rewrites a draft's fields. Only legal while the issue is a
// backlog draft; launched issues are immutable through this path. The
// attachment set is replaced wholesale: whatever is in the field on save is the
// new set. Bytes of dropped names are deleted *last*, so a mid-sequence failure
// leaves extra bytes on disk rather than a missing file the table still claims.
func (e *Engine) UpdateIssue(id, title, body, flowName, preset string, m levers.Matrix, priority int, attachments []string) error {
	current, err := e.cfg.Store.Dependencies(id)
	if err != nil {
		return err
	}
	return e.UpdateIssueWithDependencies(id, title, body, flowName, preset, m, priority, attachments, current)
}

func (e *Engine) UpdateIssueWithDependencies(id, title, body, flowName, preset string,
	m levers.Matrix, priority int, attachments, dependsOn []string) error {
	if _, ok := e.cfg.Flows[flowName]; !ok {
		return fmt.Errorf("unknown flow %q", flowName)
	}
	dependsOn = deps.Normalize(dependsOn)
	graph, err := e.cfg.Store.DependencyGraph()
	if err != nil {
		return err
	}
	graph[id] = dependsOn
	if err := graph.Validate(); err != nil {
		return err
	}
	// Check draftness before touching anything: a later refusal must not leave
	// in-memory fields rewritten.
	e.mu.Lock()
	is, ok := e.issues[id]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("unknown issue %s", id)
	}
	if !is.draft {
		e.mu.Unlock()
		return fmt.Errorf("issue %s is not in the backlog", id)
	}
	e.mu.Unlock()

	existing, err := e.cfg.Store.Attachments(id)
	if err != nil {
		return err
	}
	set, err := attach.Plan(existing, attachments)
	if err != nil {
		return err
	}
	if err := attach.Save(e.issueDir(id), set); err != nil {
		return err
	}

	// Re-check under the second lock: the draftness test above was released for
	// the disk work, and the daemon serves each connection on its own goroutine,
	// so the issue can be launched or abandoned in that window. Writing on
	// regardless would push a running issue's row back to "backlog" or resurrect
	// an abandoned one. Bailing here leaves the just-copied bytes orphaned, which
	// is the failure this path deliberately prefers.
	e.mu.Lock()
	if current, ok := e.issues[id]; !ok || current != is {
		e.mu.Unlock()
		return fmt.Errorf("unknown issue %s", id)
	}
	if !is.draft {
		e.mu.Unlock()
		return fmt.Errorf("issue %s is not in the backlog", id)
	}
	is.title, is.body, is.flowName, is.matrix, is.priority = title, body, flowName, m, priority
	e.mu.Unlock()
	if err := e.cfg.Store.UpsertIssue(store.IssueRow{
		ID: id, Title: title, Body: body, Flow: flowName, State: "backlog", Levers: matrixStrings(m), Priority: priority,
	}); err != nil {
		return err
	}
	if err := e.cfg.Store.ReplaceAttachments(id, attach.Rows(id, set, time.Now().UTC())); err != nil {
		return err
	}
	if err := e.SetDependencies(id, dependsOn); err != nil {
		return err
	}
	if err := attach.DeleteDropped(e.issueDir(id), existing, set); err != nil {
		return err
	}
	e.emit(core.EvIssueUpdated, id, map[string]any{
		"title": title, "body": body, "flow": flowName, "preset": preset,
		"priority": priority, "levers": matrixStrings(m), "attachments": set.Names(),
		"depends_on": dependsOn})
	return nil
}

func (e *Engine) ClaimIssue(id string) (Claim, error) {
	e.mu.Lock()
	is, ok := e.issues[id]
	if !ok {
		e.mu.Unlock()
		return Claim{}, fmt.Errorf("unknown issue %s", id)
	}
	if is.claimed {
		claim := e.claimFromState(is)
		e.mu.Unlock()
		return claim, nil
	}
	if is.claiming {
		e.mu.Unlock()
		return Claim{}, fmt.Errorf("issue %s claim is already in progress", id)
	}
	if !is.draft {
		e.mu.Unlock()
		return Claim{}, fmt.Errorf("issue %s is not in the backlog", id)
	}
	e.mu.Unlock()

	unmet, err := e.unmetMergedDependencies(id)
	if err != nil {
		return Claim{}, err
	}
	if len(unmet) > 0 {
		return Claim{}, fmt.Errorf("issue %s is blocked by %s", id, strings.Join(unmet, ", "))
	}
	if e.cfg.Workspace == nil {
		return Claim{}, fmt.Errorf("issue %s cannot be claimed: no workspace provider", id)
	}

	e.mu.Lock()
	if is.claimed {
		claim := e.claimFromState(is)
		e.mu.Unlock()
		return claim, nil
	}
	if is.claiming || !is.draft {
		e.mu.Unlock()
		return Claim{}, fmt.Errorf("issue %s is no longer available to claim", id)
	}
	is.claiming = true
	e.mu.Unlock()

	rollback := func(release func() error) {
		if release != nil {
			_ = release()
		}
		e.mu.Lock()
		is.claiming = false
		is.claimed = false
		is.draft = true
		is.wsPath = ""
		is.wsRelease = nil
		is.branch = ""
		is.baseRef = ""
		e.mu.Unlock()
	}
	path, release, err := e.cfg.Workspace.Acquire(id)
	if err != nil {
		rollback(nil)
		return Claim{}, err
	}
	if e.cfg.Train != nil && e.cfg.Train.Repo != "" {
		if err := repocfg.BindWorktree(e.cfg.Train.Repo, path); err != nil {
			rollback(release)
			return Claim{}, err
		}
	}
	branch, err := gitCommandOutput(path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		rollback(release)
		return Claim{}, fmt.Errorf("read claimed branch: %w", err)
	}
	baseSHA, err := gitRevision(path, "HEAD")
	if err != nil {
		rollback(release)
		return Claim{}, fmt.Errorf("read claim base: %w", err)
	}
	if branch != "issue/"+id {
		rollback(release)
		return Claim{}, fmt.Errorf("claimed branch %s does not match issue/%s", branch, id)
	}
	if err := e.cfg.Store.SetIssueIntegration(store.IssueIntegration{
		IssueID: id, State: store.IntegrationClaimed, PreSHA: baseSHA,
		Worktree: path, Branch: branch,
	}); err != nil {
		rollback(release)
		return Claim{}, err
	}
	if err := e.cfg.Store.UpsertIssue(store.IssueRow{
		ID: id, Title: is.title, Body: is.body, State: "claimed", Flow: is.flowName,
		Levers: matrixStrings(is.matrix), Priority: is.priority,
	}); err != nil {
		_ = e.cfg.Store.DeleteIssueIntegration(id)
		rollback(release)
		return Claim{}, err
	}
	e.mu.Lock()
	is.claiming = false
	is.claimed = true
	is.draft = false
	is.externalSession = true
	is.wsPath, is.wsRelease = path, release
	is.branch, is.baseRef = branch, baseSHA
	claim := e.claimFromState(is)
	e.mu.Unlock()
	e.emit(core.EvIssueClaimed, id, map[string]string{
		"worktree": path, "branch": branch, "base_sha": baseSHA,
	})
	return claim, nil
}

func (e *Engine) Claims() ([]Claim, error) {
	e.mu.Lock()
	claims := make([]Claim, 0)
	for _, is := range e.issues {
		if is.claimed {
			claims = append(claims, e.claimFromState(is))
		}
	}
	e.mu.Unlock()
	sort.Slice(claims, func(i, j int) bool { return claims[i].IssueID < claims[j].IssueID })
	return claims, nil
}

func (e *Engine) ClaimForWorktree(path string) (Claim, error) {
	requested, err := canonicalWorktreePath(path)
	if err != nil {
		return Claim{}, err
	}
	claims, err := e.Claims()
	if err != nil {
		return Claim{}, err
	}
	var matches []Claim
	for _, claim := range claims {
		recorded, pathErr := canonicalWorktreePath(claim.Worktree)
		if pathErr == nil && recorded == requested {
			matches = append(matches, claim)
		}
	}
	if len(matches) == 0 {
		return Claim{}, fmt.Errorf("no claim for worktree %s", path)
	}
	if len(matches) > 1 {
		return Claim{}, fmt.Errorf("multiple claims for worktree %s", path)
	}
	return matches[0], nil
}

func (e *Engine) claimFromState(is *issueState) Claim {
	repository := ""
	if e.cfg.Train != nil {
		repository = e.cfg.Train.Repo
	}
	return Claim{
		IssueID: is.id, Title: is.title, Body: is.body, Priority: is.priority,
		Flow: is.flowName, Repository: repository, Worktree: is.wsPath,
		Branch: is.branch, BaseSHA: is.baseRef,
	}
}

func (e *Engine) restoreClaimedWorkspace(is *issueState, integration store.IssueIntegration) error {
	if integration.Worktree == "" || integration.Branch == "" || integration.PreSHA == "" {
		return fmt.Errorf("claim is missing workspace identity")
	}
	info, err := os.Stat(integration.Worktree)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("claimed worktree %s is unavailable", integration.Worktree)
	}
	branch, err := gitCommandOutput(integration.Worktree, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if branch != integration.Branch {
		return fmt.Errorf("claimed branch changed: current %s, claim %s", branch, integration.Branch)
	}
	releaser, ok := e.cfg.Workspace.(workspace.Releaser)
	if !ok {
		return fmt.Errorf("workspace provider cannot restore claimed worktrees")
	}
	if e.cfg.Train != nil && e.cfg.Train.Repo != "" {
		if err := repocfg.BindWorktree(e.cfg.Train.Repo, integration.Worktree); err != nil {
			return err
		}
	}
	is.wsPath = integration.Worktree
	is.wsRelease = func() error { return releaser.ReleasePath(integration.Worktree) }
	is.branch = integration.Branch
	is.baseRef = integration.PreSHA
	return nil
}

func (e *Engine) unmetMergedDependencies(issueID string) ([]string, error) {
	parents, err := e.cfg.Store.Dependencies(issueID)
	if err != nil {
		return nil, err
	}
	return deps.ActiveBlockers(parents, func(parent string) (bool, error) {
		integration, ok, err := e.cfg.Store.IssueIntegration(parent)
		if err != nil {
			return false, err
		}
		return ok && (integration.State == store.IntegrationMerged ||
			integration.State == store.IntegrationCleanupNeeded), nil
	})
}

func (e *Engine) ClaimBlockers(issueID string) ([]string, error) {
	return e.unmetMergedDependencies(issueID)
}

func (e *Engine) ReleaseClaim(id string) error {
	e.mu.Lock()
	is, ok := e.issues[id]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("unknown issue %s", id)
	}
	if !is.claimed || is.running {
		e.mu.Unlock()
		return fmt.Errorf("issue %s is not an idle claim", id)
	}
	path, branch, baseSHA, release := is.wsPath, is.branch, is.baseRef, is.wsRelease
	e.mu.Unlock()
	if status, err := gitCommandOutput(path, "status", "--porcelain"); err != nil {
		return err
	} else if status != "" {
		return fmt.Errorf("issue %s worktree has uncommitted changes", id)
	}
	head, err := gitRevision(path, "HEAD")
	if err != nil {
		return err
	}
	if head != baseSHA {
		return fmt.Errorf("issue %s branch has commits beyond claim base", id)
	}
	currentBranch, err := gitCommandOutput(path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if currentBranch != branch {
		return fmt.Errorf("issue %s branch changed: current %s, claim %s", id, currentBranch, branch)
	}
	if release == nil {
		return fmt.Errorf("issue %s claim has no workspace release", id)
	}
	if err := release(); err != nil {
		return err
	}
	if err := e.cfg.Store.DeleteIssueIntegration(id); err != nil {
		return err
	}
	if err := e.cfg.Store.UpsertIssue(store.IssueRow{
		ID: id, Title: is.title, Body: is.body, State: "backlog", Flow: is.flowName,
		Levers: matrixStrings(is.matrix), Priority: is.priority,
	}); err != nil {
		return err
	}
	e.mu.Lock()
	is.draft = true
	is.claimed = false
	is.externalSession = false
	is.wsPath, is.wsRelease = "", nil
	is.branch, is.baseRef = "", ""
	e.mu.Unlock()
	e.emit(core.EvIssueReleased, id, nil)
	return nil
}

func (e *Engine) FinishClaim(req FinishClaimRequest) error {
	e.mu.Lock()
	is, ok := e.issues[req.IssueID]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("unknown issue %s", req.IssueID)
	}
	if !is.claimed || is.running {
		e.mu.Unlock()
		return fmt.Errorf("issue %s is not an idle claim", req.IssueID)
	}
	e.mu.Unlock()

	integration, ok, err := e.cfg.Store.IssueIntegration(req.IssueID)
	if err != nil {
		return err
	}
	if !ok || integration.State != store.IntegrationClaimed {
		return fmt.Errorf("issue %s has no durable claim", req.IssueID)
	}
	requested, err := canonicalWorktreePath(req.Worktree)
	if err != nil {
		return err
	}
	recorded, err := canonicalWorktreePath(integration.Worktree)
	if err != nil {
		return err
	}
	if filepath.Clean(requested) != filepath.Clean(recorded) {
		return fmt.Errorf("worktree %s does not match claim %s", requested, recorded)
	}
	branch, err := gitCommandOutput(recorded, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if branch != integration.Branch {
		return fmt.Errorf("branch %s does not match claim %s", branch, integration.Branch)
	}
	status, err := gitCommandOutput(recorded, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("issue %s worktree has uncommitted changes", req.IssueID)
	}
	head, err := gitRevision(recorded, "HEAD")
	if err != nil {
		return err
	}
	if head == integration.PreSHA && !req.AllowNoChange {
		return fmt.Errorf("issue %s has no commits beyond the claim base", req.IssueID)
	}
	finalIdx, err := finalStageIndex(e.cfg.Flows[is.flowName])
	if err != nil {
		return err
	}

	e.mu.Lock()
	if !is.claimed || is.running {
		e.mu.Unlock()
		return fmt.Errorf("issue %s is no longer ready to finish", req.IssueID)
	}
	is.claimed = false
	is.draft = false
	is.externalSession = true
	is.stageIdx = finalIdx
	e.mu.Unlock()
	e.emit(core.EvIssueCreated, is.id, map[string]any{
		"title": is.title, "body": is.body, "flow": is.flowName, "priority": is.priority,
	})
	go func() { _ = e.runAndRecord(context.Background(), is, finalIdx) }()
	return nil
}

func canonicalWorktreePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func finalStageIndex(f flow.Flow) (int, error) {
	_, index, ok := f.IntegrationStage()
	if !ok {
		return 0, fmt.Errorf("flow %q has no merge barrier", f.Name)
	}
	return index, nil
}

func verificationOwner(f flow.Flow) string {
	stage, _, ok := f.IntegrationStage()
	if !ok {
		return "none"
	}
	return stage.Name
}

// LaunchIssue promotes a backlog draft into a running lane: the row flips to
// running, the standard issue_created event fires (projection and steward
// already treat it as the start of a lane), and the flow runs detached like
// Resume — failures surface as stage_failed events, not in this response.
func (e *Engine) LaunchIssue(id string) error {
	return e.LaunchIssueWithBudget(id, nil)
}

func (e *Engine) LaunchIssueWithBudget(id string, plannerOverride *plannerbudget.Override) error {
	e.mu.Lock()
	is, ok := e.issues[id]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("unknown issue %s", id)
	}
	if !is.draft {
		e.mu.Unlock()
		return fmt.Errorf("issue %s is not in the backlog", id)
	}
	if _, ok := e.cfg.Flows[is.flowName]; !ok {
		e.mu.Unlock()
		return fmt.Errorf("unknown flow %q", is.flowName)
	}
	is.draft = false
	title, body, flowName, matrix, priority := is.title, is.body, is.flowName, is.matrix, is.priority
	planReview := e.resolvePlanReviewPolicy(flowName, matrix)
	is.planReview = planReview
	e.mu.Unlock()
	if err := e.cfg.Store.UpsertIssue(store.IssueRow{
		ID: id, Title: title, Body: body, Flow: flowName, State: "running", Levers: matrixStrings(matrix), Priority: priority,
		PlanReviewPolicy: planReview,
	}); err != nil {
		e.mu.Lock()
		is.draft = true
		e.mu.Unlock()
		return err
	}
	e.emit(core.EvIssueCreated, id, map[string]any{
		"title": title, "flow": flowName, "body": body, "priority": priority})
	go e.startOrWaitWithPlannerBudget(context.Background(), is, plannerOverride)
	return nil
}

func matrixStrings(m levers.Matrix) map[string]string {
	values := make(map[string]string, len(m))
	for stage, lever := range m {
		values[stage] = string(lever)
	}
	return values
}

// PendingDecisions returns only decisions whose page and required event have
// both been published; rows inside the fail-closed publication window stay
// hidden.
func (e *Engine) PendingDecisions() []PendingDecision {
	rows, err := e.cfg.Store.PendingDecisionRows()
	if err != nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []PendingDecision
	for _, row := range rows {
		if p, ok := e.pend[row.ID]; ok && p.published {
			out = append(out, p.PendingDecision)
		}
	}
	return out
}

func (e *Engine) Answer(decisionID int64, response levers.Response) error {
	return e.AnswerAs(decisionID, response, review.DefaultActorID)
}

func reportResolvedDecisionArchiveError(decisionID int64, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "watchtower: decision %d resolved but archive update failed: %v\n", decisionID, err)
	}
}

func (e *Engine) AnswerAs(decisionID int64, response levers.Response, actor string) error {
	actor = review.NormalizeActor(actor)
	e.mu.Lock()
	p, ok := e.pend[decisionID]
	if !ok || !p.published {
		e.mu.Unlock()
		return fmt.Errorf("no pending decision %d", decisionID)
	}
	if !p.D.Accepts(response) {
		e.mu.Unlock()
		return fmt.Errorf("invalid response for decision %d", decisionID)
	}
	if p.Review != nil {
		target := *p.Review
		e.mu.Unlock()
		provenance := &review.ApprovalProvenance{Kind: review.ApprovalHuman, ActorID: actor}
		if _, err := e.cfg.Store.ResolveArtifactReview(decisionID, target, response, provenance); err != nil {
			return fmt.Errorf("resolve artifact review %d: %w", decisionID, err)
		}
		if p.ReviewPolicy != nil {
			policy := *p.ReviewPolicy
			payload := planReviewPayload(p.ID, p.Stage, policy)
			payload["approval_kind"] = string(review.ApprovalHuman)
			payload["actor_id"] = actor
			eventType := core.EvPlanReviewHumanApproved
			if legacyAnswer(response) != 0 {
				eventType = core.EvPlanReviewRejected
			}
			if _, err := e.appendEvent(eventType, p.IssueID, payload); err != nil {
				return fmt.Errorf("append plan review outcome: %w", err)
			}
		}
		archiveErr := e.writeResolvedDecisionArchive(p.ID)
		e.mu.Lock()
		delete(e.pend, decisionID)
		e.mu.Unlock()
		e.refreshDecisionPage(p.IssueID)
		e.emit(core.EvDecisionAnswered, p.IssueID, map[string]any{
			"decision_id": p.ID, "response": response, "review": target, "actor_id": actor})
		if p.reply != nil {
			p.reply <- response
		} else {
			e.scheduleArtifactContinuation(p.ID, p.IssueID, p.Stage, target, response, false)
		}
		reportResolvedDecisionArchiveError(p.ID, archiveErr)
		return nil
	}
	delete(e.pend, decisionID)
	e.mu.Unlock()
	if err := e.cfg.Store.AnswerDecision(decisionID, response, "answered"); err != nil {
		return err
	}
	archiveErr := e.writeResolvedDecisionArchive(p.ID)
	e.refreshDecisionPage(p.IssueID)
	e.emit(core.EvDecisionAnswered, p.IssueID, map[string]any{
		"decision_id": p.ID, "response": response})
	p.reply <- response
	reportResolvedDecisionArchiveError(p.ID, archiveErr)
	return nil
}

// escalate blocks until the human answers; returns the typed response.
func (e *Engine) escalate(is *issueState, stage, agentPkg string, d levers.Decision) (levers.Response, error) {
	decisionContext, err := e.buildDecisionContext(is, agentPkg)
	if err != nil {
		return levers.Response{}, err
	}
	return e.escalateWithContext(is, stage, d, decisionContext)
}

func (e *Engine) buildDecisionContext(is *issueState, agentPkg string) (decision.DecisionContext, error) {
	e.mu.Lock()
	taskSummary := is.taskSummary
	e.mu.Unlock()
	if taskSummary == "" {
		return decision.DecisionContext{}, fmt.Errorf("decision context: task summary is unavailable")
	}
	identity, ok := e.cfg.DecisionIdentities[agentPkg]
	if !ok {
		return decision.DecisionContext{}, fmt.Errorf("decision context: agent package %q identity is missing", agentPkg)
	}
	context := decision.DecisionContext{
		TaskSummary: taskSummary,
		AgentName:   identity.Name,
		AgentColor:  identity.Color,
		AgentSymbol: identity.Symbol,
	}
	if err := decision.ValidateDecisionContext(context); err != nil {
		return decision.DecisionContext{}, fmt.Errorf("decision context: %w", err)
	}
	return context, nil
}

func validateDecisionEnvelope(issueID, stage string, d levers.Decision, context decision.DecisionContext) error {
	pending := PendingDecision{
		ID: maxDecisionID, IssueID: issueID, Stage: stage, D: d, Context: &context,
	}
	if err := validateDecisionWireValue("pending decision", pending); err != nil {
		return err
	}
	requiredEvent := decisionRequiredPayload(maxDecisionID, stage, d, context)
	return validateDecisionWireValue("decision event", requiredEvent)
}

const maxDecisionID int64 = 9223372036854775807

func validateDecisionWireValue(name string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("decision context: marshal %s: %w", name, err)
	}
	if len(encoded) > decision.MaxMessageBytes {
		return fmt.Errorf("decision context: %s exceeds %d-byte message budget", name, decision.MaxMessageBytes)
	}
	return nil
}

func decisionRequiredPayload(id int64, stage string, d levers.Decision, context decision.DecisionContext) map[string]any {
	return map[string]any{
		"decision_id": id, "stage": stage, "question": d.Question,
		"options": d.Options, "recommended": d.Recommended, "why": d.Why,
		"kind": d.Kind, "recommended_response": d.RecommendedResponse,
		"allow_freeform": d.AllowFreeform, "importance": d.Importance,
		"requires_option": d.RequiresOption,
		"consequences":    d.Consequences, "reversible": d.Reversible, "paths": d.Paths,
		"context": context,
	}
}

func decisionRequiredPayloadWithReview(
	id int64, stage string, d levers.Decision, context decision.DecisionContext, target review.Target,
) map[string]any {
	payload := decisionRequiredPayload(id, stage, d, context)
	payload["review"] = target
	return payload
}

func planReviewPayload(id int64, stage string, policy review.ResolvedPolicy) map[string]any {
	return map[string]any{
		"review_kind": "plan", "stage": stage, "decision_id": id,
		"mode": policy.Mode, "human_required": policy.HumanRequired,
		"policy_auto_approval": policy.PolicyAutoApproval,
		"policy_id":            policy.PolicyID, "policy_version": policy.PolicyVersion,
		"reason": policy.Reason,
	}
}

func (e *Engine) artifactReviewTarget(is *issueState, stage string, checkpointID int64, artifacts []contextpack.Artifact) (review.Target, error) {
	f, ok := e.cfg.Flows[is.flowName]
	if !ok {
		return review.Target{}, fmt.Errorf("flow %q is not configured", is.flowName)
	}
	nextStage := ""
	for i, configured := range f.Stages {
		if configured.Name == stage && i+1 < len(f.Stages) {
			nextStage = f.Stages[i+1].Name
			break
		}
	}
	target, err := (review.Target{
		IssueID: is.id, Stage: stage, CheckpointID: checkpointID,
		Artifacts: artifacts, NextStage: nextStage,
	}).Canonical()
	if err != nil {
		return review.Target{}, fmt.Errorf("build artifact review target: %w", err)
	}
	return target, nil
}

func (e *Engine) requestArtifactReview(
	is *issueState, st flow.Stage, checkpointID int64, artifacts []contextpack.Artifact,
) (levers.Response, error) {
	target, err := e.artifactReviewTarget(is, st.Name, checkpointID, artifacts)
	if err != nil {
		return levers.Response{}, err
	}
	agentPkg, err := e.decisionAgentPackage(is.flowName, st.Name)
	if err != nil {
		return levers.Response{}, err
	}
	decisionContext, err := e.buildDecisionContext(is, agentPkg)
	if err != nil {
		return levers.Response{}, err
	}
	d := artifactReviewDecision(target, false)
	if err := validateDecisionEnvelope(is.id, st.Name, d, decisionContext); err != nil {
		return levers.Response{}, err
	}
	if err := validateDecisionWireValue("artifact review target", target); err != nil {
		return levers.Response{}, err
	}
	rowID, err := e.cfg.Store.RequestArtifactReview(target, store.DecisionRow{
		IssueID: is.id, Stage: st.Name, Question: d.Question,
		Options: d.Options, Recommended: d.Recommended, Kind: d.Kind,
		Importance: d.Importance, RequiresOption: d.RequiresOption,
		Why: d.Why, Consequences: d.Consequences,
		Reversible: d.Reversible, Briefing: d.Briefing, Context: &decisionContext,
		BlockingCost: e.blockingCost(is.id),
	})
	if err != nil {
		return levers.Response{}, fmt.Errorf("request artifact review: %w", err)
	}
	p := &pending{
		PendingDecision: PendingDecision{
			ID: rowID, IssueID: is.id, Stage: st.Name, D: d,
			Context: &decisionContext, Review: &target,
		},
		reply: make(chan levers.Response, 1),
	}
	if !e.registerPendingDecision(is, p) {
		e.cancelPendingDecisionPublication(is.id, rowID)
		return levers.Response{}, nil
	}
	if err := e.writeDecisionPage(is, st.Name, rowID, d, &decisionContext, &target, nil); err != nil {
		if !e.unregisterPendingDecision(p) {
			e.cancelPendingDecisionPublication(is.id, rowID)
			return levers.Response{}, nil
		}
		cleanupErr := e.rollbackPendingDecisionPublication(is.id, rowID)
		if cleanupErr != nil {
			return levers.Response{}, fmt.Errorf("write artifact review decision page: %w (cleanup decision %d: %v)", err, rowID, cleanupErr)
		}
		return levers.Response{}, fmt.Errorf("write artifact review decision page: %w", err)
	}
	if err := e.publishPendingDecision(
		p, decisionRequiredPayloadWithReview(rowID, st.Name, d, decisionContext, target),
	); err != nil {
		if errors.Is(err, errDecisionPublicationCanceled) {
			e.cancelPendingDecisionPublication(is.id, rowID)
			return levers.Response{}, nil
		}
		cleanupErr := e.rollbackPendingDecisionPublication(is.id, rowID)
		if cleanupErr != nil {
			return levers.Response{}, fmt.Errorf("append artifact review event: %w (cleanup decision %d: %v)", err, rowID, cleanupErr)
		}
		return levers.Response{}, fmt.Errorf("append artifact review event: %w", err)
	}
	response, ok := <-p.reply
	if !ok {
		return levers.Response{}, nil
	}
	return response, nil
}

func (e *Engine) requestPlanReview(
	is *issueState, st flow.Stage, checkpointID int64, artifacts []contextpack.Artifact,
) (levers.Response, error) {
	target, err := e.artifactReviewTarget(is, st.Name, checkpointID, artifacts)
	if err != nil {
		return levers.Response{}, err
	}
	agentPkg, err := e.decisionAgentPackage(is.flowName, st.Name)
	if err != nil {
		return levers.Response{}, err
	}
	decisionContext, err := e.buildDecisionContext(is, agentPkg)
	if err != nil {
		return levers.Response{}, err
	}
	e.mu.Lock()
	policy := is.planReview
	e.mu.Unlock()
	d := artifactReviewDecision(target, true)
	if err := validateDecisionEnvelope(is.id, st.Name, d, decisionContext); err != nil {
		return levers.Response{}, err
	}
	if err := validateDecisionWireValue("plan review target", target); err != nil {
		return levers.Response{}, err
	}
	var pageSnapshot *decisionpage.PageData
	if policy.PolicyAutoApproval {
		pageSnapshot, err = e.decisionPageSnapshot(is, st.Name, d, &decisionContext, &target)
		if err != nil {
			return levers.Response{}, fmt.Errorf("build plan review page snapshot: %w", err)
		}
	}
	rowID, err := e.cfg.Store.RequestArtifactReview(target, store.DecisionRow{
		IssueID: is.id, Stage: st.Name, Question: d.Question,
		Options: d.Options, Recommended: d.Recommended, Kind: d.Kind,
		Importance: d.Importance, RequiresOption: d.RequiresOption,
		Why: d.Why, Consequences: d.Consequences,
		Reversible: d.Reversible, Briefing: d.Briefing,
		Context: &decisionContext, ReviewPolicy: &policy, PageSnapshot: pageSnapshot,
		BlockingCost: e.blockingCost(is.id),
	})
	if err != nil {
		return levers.Response{}, fmt.Errorf("request plan review: %w", err)
	}
	requested := planReviewPayload(rowID, st.Name, policy)
	if policy.PolicyAutoApproval {
		if _, err := e.appendEvent(core.EvPlanReviewRequested, is.id, requested); err != nil {
			_ = e.cfg.Store.DeleteDecision(rowID)
			return levers.Response{}, fmt.Errorf("append plan review request: %w", err)
		}
		provenance := &review.ApprovalProvenance{
			Kind: review.ApprovalPolicy, PolicyID: policy.PolicyID, PolicyVersion: policy.PolicyVersion,
		}
		if _, err := e.cfg.Store.ResolveArtifactReview(rowID, target, levers.ChoiceResponse(0), provenance); err != nil {
			return levers.Response{}, fmt.Errorf("resolve plan review policy: %w", err)
		}
		payload := planReviewPayload(rowID, st.Name, policy)
		payload["approval_kind"] = string(review.ApprovalPolicy)
		payload["response"] = levers.ChoiceResponse(0)
		if _, err := e.appendEvent(core.EvDecisionAutoResolved, is.id, payload); err != nil {
			return levers.Response{}, fmt.Errorf("append plan policy resolution: %w", err)
		}
		if _, err := e.appendEvent(core.EvPlanReviewPolicyApproved, is.id, payload); err != nil {
			return levers.Response{}, fmt.Errorf("append plan policy approval: %w", err)
		}
		if err := e.writeResolvedDecisionArchive(rowID); err != nil {
			return levers.Response{}, fmt.Errorf("archive policy-approved plan review: %w", err)
		}
		e.refreshDecisionPage(is.id)
		return levers.ChoiceResponse(0), nil
	}
	if !policy.HumanRequired {
		return levers.Response{}, fmt.Errorf("plan review policy is unresolved")
	}
	decisionPayload := decisionRequiredPayloadWithReview(rowID, st.Name, d, decisionContext, target)
	decisionPayload["review_policy"] = policy
	p := &pending{
		PendingDecision: PendingDecision{
			ID: rowID, IssueID: is.id, Stage: st.Name, D: d,
			Context: &decisionContext, Review: &target, ReviewPolicy: &policy,
		},
		reply: make(chan levers.Response, 1),
	}
	if !e.registerPendingDecision(is, p) {
		e.cancelPendingDecisionPublication(is.id, rowID)
		return levers.Response{}, nil
	}
	if err := e.writeDecisionPage(is, st.Name, rowID, d, &decisionContext, &target, nil); err != nil {
		if !e.unregisterPendingDecision(p) {
			e.cancelPendingDecisionPublication(is.id, rowID)
			return levers.Response{}, nil
		}
		cleanupErr := e.rollbackPendingDecisionPublication(is.id, rowID)
		if cleanupErr != nil {
			return levers.Response{}, fmt.Errorf("write plan review decision page: %w (cleanup decision %d: %v)", err, rowID, cleanupErr)
		}
		return levers.Response{}, fmt.Errorf("write plan review decision page: %w", err)
	}
	if err := e.publishPendingPlanReview(p, requested, decisionPayload); err != nil {
		if errors.Is(err, errDecisionPublicationCanceled) {
			e.cancelPendingDecisionPublication(is.id, rowID)
			return levers.Response{}, nil
		}
		cleanupErr := e.rollbackPendingDecisionPublication(is.id, rowID)
		if cleanupErr != nil {
			return levers.Response{}, fmt.Errorf("append plan review decision: %w (cleanup decision %d: %v)", err, rowID, cleanupErr)
		}
		return levers.Response{}, fmt.Errorf("append plan review decision: %w", err)
	}
	response, ok := <-p.reply
	if !ok {
		return levers.Response{}, nil
	}
	return response, nil
}

func (e *Engine) scheduleArtifactContinuation(
	decisionID int64, issueID, stage string, target review.Target, response levers.Response, emitAnswered bool,
) {
	e.mu.Lock()
	if e.reviewContinuations[decisionID] {
		e.mu.Unlock()
		return
	}
	e.reviewContinuations[decisionID] = true
	e.mu.Unlock()
	go e.continueArtifactReview(decisionID, issueID, stage, target, response, emitAnswered)
}

func (e *Engine) continueArtifactReview(
	decisionID int64, issueID, stage string, target review.Target, response levers.Response, emitAnswered bool,
) {
	e.mu.Lock()
	is, ok := e.issues[issueID]
	e.mu.Unlock()
	if !ok {
		e.emit(core.EvStageFailed, issueID, map[string]any{
			"stage": stage, "error": "artifact review issue state is unavailable", "final": true})
		return
	}
	f, ok := e.cfg.Flows[is.flowName]
	if !ok {
		e.emit(core.EvStageFailed, issueID, map[string]any{
			"stage": stage, "error": fmt.Sprintf("flow %q is not configured", is.flowName), "final": true})
		return
	}
	var approvedStage flow.Stage
	nextIdx := -1
	for i, configured := range f.Stages {
		if configured.Name == stage {
			approvedStage = configured
			nextIdx = i + 1
			break
		}
	}
	if nextIdx < 0 {
		e.emit(core.EvStageFailed, issueID, map[string]any{
			"stage": stage, "error": "artifact review stage is not in the configured flow", "final": true})
		return
	}
	if err := e.cfg.Store.CompleteArtifactReview(target.CheckpointID, target); err != nil {
		e.emit(core.EvStageFailed, issueID, map[string]any{
			"stage": stage, "error": fmt.Sprintf("artifact review handoff: %v", err), "final": true})
		return
	}
	e.registerApprovedTouchset(is, approvedStage)
	if emitAnswered {
		e.emit(core.EvDecisionAnswered, issueID, map[string]any{
			"decision_id": decisionID, "response": response, "review": target})
	}
	e.emit(core.EvStageCompleted, issueID, map[string]string{"stage": stage})

	e.mu.Lock()
	is.terminal = false
	is.stageIdx = 0
	e.mu.Unlock()
	go func() { _ = e.runAndRecord(context.Background(), is, nextIdx) }()
}

func (e *Engine) registerApprovedTouchset(is *issueState, st flow.Stage) {
	_, _, integrating := e.cfg.Flows[is.flowName].IntegrationStage()
	if !integrating || e.cfg.Marshal == nil || !st.DeclaresArtifact("touchset.json") {
		return
	}
	ts, err := touchset.Load(filepath.Join(e.stageWorkdir(is, st), "touchset.json"))
	if err != nil {
		return
	}
	e.mu.Lock()
	snapshot := ts
	is.activeTouchset = &snapshot
	e.mu.Unlock()
	e.cfg.Marshal.PlanApproved(is.id, ts)
}

func (e *Engine) freezeTaskSummary(is *issueState) error {
	e.mu.Lock()
	if is.taskSummary != "" {
		e.mu.Unlock()
		return nil
	}
	title, body := is.title, is.body
	e.mu.Unlock()
	summary, err := decision.BuildTaskSummary(title, body)
	if err != nil {
		return fmt.Errorf("decision context: build task summary: %w", err)
	}
	e.mu.Lock()
	if is.taskSummary == "" {
		is.taskSummary = summary
	}
	e.mu.Unlock()
	return nil
}

func (e *Engine) decisionAgentPackage(flowName, stage string) (string, error) {
	f, ok := e.cfg.Flows[flowName]
	if !ok {
		return "", fmt.Errorf("flow %q is not configured", flowName)
	}
	for _, configuredStage := range f.Stages {
		if configuredStage.Name == stage {
			if len(configuredStage.Agents) == 0 {
				return "", fmt.Errorf("stage %q has no decision agent", stage)
			}
			return configuredStage.Agents[0].Package, nil
		}
	}
	return "", fmt.Errorf("stage %q is not configured", stage)
}

func reportAskError(a runner.Ask, err error) {
	if a.Error != nil {
		a.Error <- err
		return
	}
	if a.Reply != nil {
		a.Reply <- levers.Response{}
	}
}

func legacyAnswer(response levers.Response) int {
	if response.Option == nil {
		return -1
	}
	return *response.Option
}

func (e *Engine) blockingCost(issueID string) int {
	if e.cfg.Marshal == nil {
		return 1
	}
	return 1 + e.cfg.Marshal.BlockedBehind(issueID)
}

func (e *Engine) FileProposal(issueID, title, body string) {
	e.FileProposalWithDependencies(issueID, title, body, nil)
}

func (e *Engine) FileProposalWithDependencies(issueID, title, body string, dependsOn []string) {
	if _, err := e.cfg.Store.InsertProposal(issueID, title, body, dependsOn); err != nil {
		return
	}
	e.emit(core.EvProposalFiled, issueID, map[string]any{
		"title": title, "depends_on": deps.Normalize(dependsOn)})
}

func (e *Engine) FileProposalBatch(issueID string, proposals []runner.Proposal) {
	rows := make([]store.ProposalRow, 0, len(proposals))
	seen := map[string]bool{}
	for _, proposal := range proposals {
		key := strings.TrimSpace(proposal.Key)
		if key == "" || seen[key] || strings.TrimSpace(proposal.Title) == "" {
			return
		}
		seen[key] = true
		rows = append(rows, store.ProposalRow{
			Key: key, Title: proposal.Title, Body: proposal.Body,
			DependsOn: deps.Normalize(proposal.DependsOn),
		})
	}
	batchID, err := e.cfg.Store.InsertProposalBatch(issueID, rows)
	if err != nil {
		return
	}
	e.emit(core.EvProposalFiled, issueID, map[string]any{
		"batch_id": batchID, "tasks": len(rows)})
}

func (e *Engine) ResolveProposal(id int64, accept bool, flowName, preset string) (string, error) {
	ps, err := e.cfg.Store.PendingProposals()
	if err != nil {
		return "", err
	}
	var row *store.ProposalRow
	for i := range ps {
		if ps[i].ID == id {
			row = &ps[i]
			break
		}
	}
	if row == nil {
		return "", fmt.Errorf("no pending proposal %d", id)
	}
	if !accept {
		var err error
		if row.BatchID != 0 {
			err = e.cfg.Store.SetProposalBatchStatus(row.BatchID, "rejected")
		} else {
			err = e.cfg.Store.SetProposalStatus(id, "rejected")
		}
		if err != nil {
			return "", err
		}
		e.emit(core.EvProposalRejected, "", map[string]any{"proposal_id": id})
		return "", nil
	}
	f, ok := e.cfg.Flows[flowName]
	if !ok {
		return "", fmt.Errorf("unknown flow %q", flowName)
	}
	lever := flow.Lever(preset)
	if lever != flow.LeverYolo && lever != flow.LeverRegular && lever != flow.LeverStrict {
		lever = flow.LeverRegular
	}
	if row.BatchID != 0 {
		var batch []store.ProposalRow
		for _, proposal := range ps {
			if proposal.BatchID == row.BatchID {
				batch = append(batch, proposal)
			}
		}
		return e.resolveProposalBatch(batch, flowName, levers.Preset(f, lever))
	}
	newID, err := e.CreateIssueWithDependencies(
		row.Title, row.Body, flowName, levers.Preset(f, lever), 0, nil, row.DependsOn)
	if err != nil {
		return "", err
	}
	if err := e.cfg.Store.SetProposalStatus(id, "accepted"); err != nil {
		return "", err
	}
	e.emit(core.EvProposalAccepted, newID, map[string]any{"proposal_id": id})
	return newID, nil
}

func (e *Engine) resolveProposalBatch(
	proposals []store.ProposalRow, flowName string, matrix levers.Matrix,
) (string, error) {
	if len(proposals) == 0 {
		return "", fmt.Errorf("proposal batch is empty")
	}
	keyToID := make(map[string]string, len(proposals))
	e.mu.Lock()
	for _, proposal := range proposals {
		if proposal.Key == "" {
			e.mu.Unlock()
			return "", fmt.Errorf("proposal batch has an empty key")
		}
		if _, exists := keyToID[proposal.Key]; exists {
			e.mu.Unlock()
			return "", fmt.Errorf("proposal batch repeats key %s", proposal.Key)
		}
		e.nextID++
		keyToID[proposal.Key] = fmt.Sprintf("GH-%d", e.nextID)
	}
	e.mu.Unlock()

	graph, err := e.cfg.Store.DependencyGraph()
	if err != nil {
		return "", err
	}
	issues := make([]store.IssueRow, 0, len(proposals))
	edges := make(map[string][]string, len(proposals))
	planReview := e.resolvePlanReviewPolicy(flowName, matrix)
	for _, proposal := range proposals {
		id := keyToID[proposal.Key]
		for _, dependency := range proposal.DependsOn {
			if resolved, ok := keyToID[dependency]; ok {
				edges[id] = append(edges[id], resolved)
			} else {
				edges[id] = append(edges[id], dependency)
			}
		}
		edges[id] = deps.Normalize(edges[id])
		graph[id] = edges[id]
		issues = append(issues, store.IssueRow{
			ID: id, Title: proposal.Title, Body: proposal.Body, State: "running",
			Flow: flowName, Levers: matrixStrings(matrix), PlanReviewPolicy: planReview,
		})
	}
	if err := graph.Validate(); err != nil {
		return "", err
	}
	if err := e.cfg.Store.AcceptProposalBatch(proposals[0].BatchID, issues, edges); err != nil {
		return "", err
	}
	e.mu.Lock()
	for _, issue := range issues {
		e.issues[issue.ID] = &issueState{
			id: issue.ID, title: issue.Title, body: issue.Body, flowName: issue.Flow,
			matrix: matrix, dependsOn: append([]string(nil), edges[issue.ID]...), planReview: planReview,
		}
	}
	e.mu.Unlock()
	for _, issue := range issues {
		e.emit(core.EvIssueCreated, issue.ID, map[string]any{
			"title": issue.Title, "body": issue.Body, "flow": issue.Flow,
			"depends_on": edges[issue.ID]})
		e.emit(core.EvProposalAccepted, issue.ID, map[string]any{
			"batch_id": proposals[0].BatchID})
	}
	return issues[0].ID, nil
}

func (e *Engine) handleAsk(is *issueState, stage, agentPkg string, a runner.Ask) {
	decisionContext, err := e.buildDecisionContext(is, agentPkg)
	if err != nil {
		reportAskError(a, err)
		return
	}
	lever := is.matrix[stage]
	if levers.Route(a.Decision, lever, e.cfg.Rules) {
		response, err := e.escalateWithContext(is, stage, a.Decision, decisionContext)
		if err != nil {
			reportAskError(a, err)
			return
		}
		a.Reply <- response
		return
	}
	if err := validateDecisionEnvelope(is.id, stage, a.Decision, decisionContext); err != nil {
		reportAskError(a, err)
		return
	}
	pageSnapshot, err := e.decisionPageSnapshot(is, stage, a.Decision, &decisionContext, nil)
	if err != nil {
		reportAskError(a, fmt.Errorf("build auto decision page snapshot: %w", err))
		return
	}
	resolvedAt := time.Now().UTC()
	rowID, err := e.cfg.Store.InsertDecision(store.DecisionRow{
		IssueID: is.id, Stage: stage, Question: a.Decision.Question,
		Options: a.Decision.Options, Recommended: a.Decision.Recommended,
		Kind: a.Decision.Kind, RecommendedResponse: a.Decision.RecommendedResponse,
		AllowFreeform: a.Decision.AllowFreeform, Importance: a.Decision.Importance,
		Paths: a.Decision.Paths,
		Why:   a.Decision.Why, Consequences: a.Decision.Consequences, Reversible: a.Decision.Reversible,
		Briefing:     a.Decision.Briefing,
		Context:      &decisionContext,
		PageSnapshot: pageSnapshot,
		Status:       "auto", Response: a.Decision.RecommendedAnswer(),
		CreatedAt: resolvedAt, AnsweredAt: resolvedAt,
		BlockingCost: e.blockingCost(is.id),
	})
	if err != nil {
		reportAskError(a, fmt.Errorf("insert auto decision: %w", err))
		return
	}
	if _, err := e.appendEvent(core.EvDecisionAutoResolved, is.id, map[string]any{
		"stage": stage, "question": a.Decision.Question,
		"response": a.Decision.RecommendedAnswer(), "context": decisionContext}); err != nil {
		cleanupErr := e.cfg.Store.DeleteDecision(rowID)
		if cleanupErr != nil {
			err = fmt.Errorf("append auto decision event: %w (cleanup decision %d: %v)", err, rowID, cleanupErr)
		} else {
			err = fmt.Errorf("append auto decision event: %w", err)
		}
		reportAskError(a, err)
		return
	}
	if err := e.writeResolvedDecisionArchive(rowID); err != nil {
		reportAskError(a, fmt.Errorf("archive auto decision: %w", err))
		return
	}
	e.refreshDecisionPage(is.id)
	a.Reply <- a.Decision.RecommendedAnswer()
}

func (e *Engine) escalateWithContext(is *issueState, stage string, d levers.Decision, decisionContext decision.DecisionContext) (levers.Response, error) {
	if err := validateDecisionEnvelope(is.id, stage, d, decisionContext); err != nil {
		return levers.Response{}, err
	}
	rowID, err := e.cfg.Store.InsertDecision(store.DecisionRow{
		IssueID: is.id, Stage: stage, Question: d.Question,
		Options: d.Options, Recommended: d.Recommended,
		Kind: d.Kind, RecommendedResponse: d.RecommendedResponse,
		AllowFreeform: d.AllowFreeform, RequiresOption: d.RequiresOption,
		Importance: d.Importance, Paths: d.Paths,
		Why: d.Why, Consequences: d.Consequences, Reversible: d.Reversible,
		EngineContinuation: d.EngineContinuation,
		Briefing:           d.Briefing,
		Context:            &decisionContext,
		BlockingCost:       e.blockingCost(is.id),
	})
	if err != nil {
		return levers.Response{}, fmt.Errorf("insert decision: %w", err)
	}
	p := &pending{
		PendingDecision: PendingDecision{ID: rowID, IssueID: is.id, Stage: stage, D: d, Context: &decisionContext},
		reply:           make(chan levers.Response, 1),
	}
	if !e.registerPendingDecision(is, p) {
		e.cancelPendingDecisionPublication(is.id, rowID)
		return levers.Response{}, nil
	}
	if err := e.writeDecisionPage(is, stage, rowID, d, &decisionContext, nil, nil); err != nil {
		if !e.unregisterPendingDecision(p) {
			e.cancelPendingDecisionPublication(is.id, rowID)
			return levers.Response{}, nil
		}
		cleanupErr := e.rollbackPendingDecisionPublication(is.id, rowID)
		if cleanupErr != nil {
			return levers.Response{}, fmt.Errorf("write decision page: %w (cleanup decision %d: %v)", err, rowID, cleanupErr)
		}
		return levers.Response{}, fmt.Errorf("write decision page: %w", err)
	}
	if err := e.publishPendingDecision(p, decisionRequiredPayload(rowID, stage, d, decisionContext)); err != nil {
		if errors.Is(err, errDecisionPublicationCanceled) {
			e.cancelPendingDecisionPublication(is.id, rowID)
			return levers.Response{}, nil
		}
		cleanupErr := e.rollbackPendingDecisionPublication(is.id, rowID)
		if cleanupErr != nil {
			return levers.Response{}, fmt.Errorf("append decision event: %w (cleanup decision %d: %v)", err, rowID, cleanupErr)
		}
		return levers.Response{}, fmt.Errorf("append decision event: %w", err)
	}
	choice, ok := <-p.reply
	if !ok {
		return levers.Response{}, nil
	}
	return choice, nil
}

func (e *Engine) stageContext(issueID string) ([]string, string, error) {
	checkpoints, err := e.cfg.Store.StageCheckpoints(issueID)
	if err != nil {
		return nil, "", err
	}
	seen := map[string]bool{}
	var inputs []string
	lastSuccessful := ""
	for _, checkpoint := range checkpoints {
		if checkpoint.Status != "succeeded" {
			continue
		}
		lastSuccessful = checkpoint.Stage
		for _, artifact := range checkpoint.Artifacts {
			if !seen[artifact.Name] {
				seen[artifact.Name] = true
				inputs = append(inputs, artifact.Name)
			}
		}
	}
	return inputs, lastSuccessful, nil
}

func (e *Engine) decisionLedger(issueID string) (string, error) {
	rows, err := e.cfg.Store.AllDecisionRows()
	if err != nil {
		return "", err
	}
	var decisions []contextpack.Decision
	for _, row := range rows {
		if row.IssueID != issueID {
			continue
		}
		decisions = append(decisions, contextpack.Decision{
			Stage: row.Stage, Question: row.Question, Kind: row.Kind,
			Options: row.Options, Response: row.Response, Why: row.Why,
			Consequences: row.Consequences, Status: row.Status, At: row.CreatedAt,
			Context: row.Context,
		})
	}
	return contextpack.DecisionLedger(decisions), nil
}

func repositoryState(workdir string, contextPaths []string) (head, branch string, dirty bool) {
	if output, err := exec.Command("git", "-C", workdir, "rev-parse", "HEAD").Output(); err == nil {
		head = strings.TrimSpace(string(output))
	}
	if output, err := exec.Command(
		"git", "-C", workdir, "rev-parse", "--abbrev-ref", "HEAD",
	).Output(); err == nil {
		branch = strings.TrimSpace(string(output))
	}
	if output, err := exec.Command("git", "-C", workdir, "status", "--porcelain").Output(); err == nil {
		ignored := map[string]bool{
			"ISSUE.md": true, "STAGE.md": true, "decisions.md": true,
		}
		for _, name := range contextPaths {
			ignored[filepath.ToSlash(name)] = true
		}
		for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			if len(line) < 4 {
				continue
			}
			path := filepath.ToSlash(strings.TrimSpace(line[3:]))
			if ignored[path] || strings.HasPrefix(path, "attachments/") {
				continue
			}
			dirty = true
			break
		}
	}
	return head, branch, dirty
}

func (e *Engine) runStageOnce(
	ctx context.Context, is *issueState, st flow.Stage, attempt, of int, plannerOverride *plannerbudget.Override,
) (runErr error) {
	// "none" stages share one per-issue dir so artifacts flow between stages
	// (brainstorm.md -> spec stage, etc.); worktree/readonly stages share the
	// acquired workspace for the same reason.
	workdir := e.stageWorkdir(is, st)
	defer func() {
		runErr = e.recordStageFailure(ctx, is, st, attempt, of, workdir, runErr)
	}()
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return err
	}
	// Attachment state is not cached in issueState: querying it here is what
	// lets Rehydrate stay untouched.
	rows, err := e.cfg.Store.Attachments(is.id)
	if err != nil {
		return err
	}
	// Every stage, every run: a "none" stage's workdir already holds the files,
	// a worktree stage's does not, and ISSUE.md must be true in both.
	if err := attach.Materialize(workdir, e.issueDir(is.id), rows); err != nil {
		return fmt.Errorf("stage %s: %w", st.Name, err)
	}
	// Materialize the issue for the agents: ISSUE.md is the contract for how
	// a stage learns what it is working on. The attachment list goes after the
	// body and before project memory, adjacent to the issue it belongs to.
	issueMD := fmt.Sprintf("# %s: %s\n\n%s\n", is.id, is.title, is.body)
	issueMD += attach.Section(rows)
	if e.cfg.Librarian != nil {
		if mem, err := e.cfg.Librarian.Context(); err == nil && mem != "" {
			issueMD += "\n# Project memory (curated by the Librarian)\n\n" + mem + "\n"
		}
	}
	if err := os.WriteFile(filepath.Join(workdir, "ISSUE.md"), []byte(issueMD), 0o644); err != nil {
		return err
	}
	requiredInputs, lastSuccessful, err := e.stageContext(is.id)
	if err != nil {
		return err
	}
	if err := contextpack.Materialize(e.issueDir(is.id), workdir, requiredInputs); err != nil {
		return fmt.Errorf("materialize stage context: %w", err)
	}
	ledger, err := e.decisionLedger(is.id)
	if err != nil {
		return err
	}
	if err := contextpack.WriteDecisionLedger(workdir, ledger); err != nil {
		return err
	}
	contextPaths := append(append([]string(nil), requiredInputs...), st.Artifacts...)
	startCommit, branch, dirty := repositoryState(workdir, contextPaths)
	_, _, _, lastFailure, err := e.cfg.Store.LastStageEvents(is.id)
	if err != nil {
		return err
	}
	var recovery *contextpack.Recovery
	if attempt > 1 || lastFailure != "" {
		recovery = &contextpack.Recovery{
			LastSuccessfulStage: lastSuccessful, CurrentHead: startCommit,
			Dirty: dirty, LastFailure: lastFailure,
			OutstandingOutputs: append([]string(nil), st.Artifacts...),
		}
	}
	expectedOutputs := append([]string(nil), st.Artifacts...)
	if len(expectedOutputs) == 0 {
		expectedOutputs = []string{"committed repository changes or a stage result"}
	}
	prohibited := []string{
		"do not merge or publish the issue branch",
		"do not commit ISSUE.md, STAGE.md, decisions.md, or materialized workflow artifacts",
	}
	if st.MergeBarrier {
		prohibited = []string{
			"do not bypass serialized integration",
			"do not commit ISSUE.md, STAGE.md, decisions.md, or materialized workflow artifacts",
		}
	}
	finalizationContract := ""
	if st.MergeBarrier {
		finalizationContract = marshal.FinalizationContractMarkdown()
	}
	f := e.cfg.Flows[is.flowName]
	if err := contextpack.WriteStageBrief(workdir, contextpack.Brief{
		IssueID: is.id, Stage: st.Name, StartCommit: startCommit,
		BaseCommit: is.baseRef, Branch: branch, RequiredInputs: requiredInputs,
		ExpectedOutputs: expectedOutputs, ProhibitedActions: prohibited,
		VerificationOwner: verificationOwner(f), FinalizationContract: finalizationContract,
		Recovery: recovery,
	}); err != nil {
		return err
	}
	var artifactAuthority *plannerartifact.Authority
	if st.DeclaresArtifact("plan.md") && st.DeclaresArtifact("touchset.json") {
		artifactAuthority, err = plannerartifact.CreateOrLoad(e.cfg.Store, plannerartifact.Binding{
			IssueID: is.id, Stage: st.Name, Attempt: attempt, Worktree: workdir,
		})
		if err != nil {
			return fmt.Errorf("initialize planner authority: %w", err)
		}
		defer func() {
			if runErr == nil {
				_ = artifactAuthority.Expire()
				return
			}
			_ = artifactAuthority.Close()
		}()
	}
	checkpointID, err := e.cfg.Store.InsertStageCheckpoint(store.StageCheckpoint{
		IssueID: is.id, Stage: st.Name, StartCommit: startCommit, Status: "running",
	})
	if err != nil {
		return err
	}
	var checkpointArtifacts []contextpack.Artifact
	var sessionIDs []string
	defer func() {
		endCommit, _, _ := repositoryState(workdir, contextPaths)
		if runErr != nil {
			if checkpoints, checkpointErr := e.cfg.Store.StageCheckpoints(is.id); checkpointErr == nil {
				for _, checkpoint := range checkpoints {
					if checkpoint.ID == checkpointID && checkpoint.Status == "revision_required" {
						return
					}
				}
			}
		}
		status, failure := "succeeded", ""
		switch {
		case errors.Is(runErr, errDependenciesDiscovered):
			status = "waiting_dependencies"
		case runErr != nil && ctx.Err() != nil:
			status, failure = "killed", runErr.Error()
		case runErr != nil:
			status, failure = "failed", runErr.Error()
		}
		_ = e.cfg.Store.FinishStageCheckpoint(
			checkpointID, status, endCommit, strings.Join(sessionIDs, ","),
			failure, checkpointArtifacts)
		e.refreshDecisionPage(is.id)
	}()
	e.emit(core.EvStageStarted, is.id, map[string]any{
		"stage": st.Name, "attempt": attempt, "of": of,
		"merge_barrier": st.MergeBarrier})
	var plannerController *plannerbudget.Controller
	var plannerGate runner.ExplorationGate
	var plannerRunner runner.PlannerRunner
	var emitPlannerSnapshot func(plannerbudget.Outcome, stageusage.Snapshot)
	if st.Name == "plan" {
		emitPlannerSnapshot = func(outcome plannerbudget.Outcome, snapshot stageusage.Snapshot) {
			e.emit(core.EvPlannerBudgetUpdated, is.id, map[string]any{
				"stage": st.Name, "attempt": attempt,
				"outcome": string(outcome), "snapshot": snapshot,
			})
		}
		profile, resolveErr := plannerbudget.Resolve(e.cfg.PlannerBudget, plannerOverride)
		if resolveErr != nil {
			snapshot := stageusage.Snapshot{Stage: st.Name, Attempt: attempt, Status: stageusage.StatusConfigurationErr}
			emitPlannerSnapshot(plannerbudget.OutcomeConfigurationError, snapshot)
			return fmt.Errorf("planner budget configuration: %w", resolveErr)
		}
		var controllerErr error
		plannerController, controllerErr = plannerbudget.NewController(st.Name, attempt, profile, time.Now)
		if controllerErr != nil {
			return fmt.Errorf("planner budget: %w", controllerErr)
		}
		var ok bool
		plannerRunner, ok = e.cfg.Runner.(runner.PlannerRunner)
		if !ok {
			err := fmt.Errorf("planner runner does not implement exploration admission")
			outcome := plannerController.Finish(err)
			emitPlannerSnapshot(outcome, plannerController.Snapshot())
			return err
		}
		plannerGate = &plannerExplorationGate{
			controller: plannerController,
			onSnapshot: func(snapshot stageusage.Snapshot) {
				emitPlannerSnapshot(plannerController.Outcome(), snapshot)
			},
		}
		emitPlannerSnapshot(plannerbudget.OutcomeNormal, plannerController.Snapshot())
	}

	var verificationLease *verificationcache.Lease
	verificationLeaseSealed := false
	if st.MergeBarrier && e.cfg.Train != nil && len(e.cfg.Train.TestCmd) > 0 {
		branchSHA, err := gitRevision(workdir, "HEAD")
		if err != nil {
			return fmt.Errorf("verification branch identity: %w", err)
		}
		treeSHA, err := gitRevision(workdir, "HEAD^{tree}")
		if err != nil {
			return fmt.Errorf("verification tree identity: %w", err)
		}
		cacheRoot := e.cfg.CacheRoot
		if cacheRoot == "" {
			cacheRoot = e.cfg.DataDir
		}
		runtime, err := verificationcache.New(verificationcache.Config{
			CacheRoot: cacheRoot, RepoDir: e.cfg.Train.Repo,
		})
		if err != nil {
			return fmt.Errorf("initialize verification cache: %w", err)
		}
		verificationLease, err = runtime.Acquire(ctx, verificationcache.Config{
			RepoDir: e.cfg.Train.Repo, BaseSHA: is.baseRef, BranchSHA: branchSHA,
			TreeSHA: treeSHA, Argv: append([]string(nil), e.cfg.Train.TestCmd...),
		})
		if err != nil {
			return fmt.Errorf("acquire verification cache lease: %w", err)
		}
	}
	defer func() {
		if verificationLease == nil {
			return
		}
		if !verificationLeaseSealed {
			reason := "verification stage did not complete"
			if runErr != nil {
				reason = runErr.Error()
			}
			_ = verificationLease.Quarantine(reason)
		}
		_ = verificationLease.Close()
	}()

	type agentDone struct {
		pkg string
		res runner.Result
	}
	var plannerTokensMu sync.Mutex
	plannerTokensRecorded := false
	dones := make(chan agentDone, len(st.Agents))
	runAgent := func(a flow.AgentRef) {
		runID, insErr := e.cfg.Store.InsertStageRun(store.StageRun{
			IssueID: is.id, Stage: st.Name, Agent: a.Package,
			Worktree: workdir, Status: "running"})
		if insErr != nil {
			dones <- agentDone{pkg: a.Package, res: runner.Result{Err: fmt.Errorf("insert stage run: %w", insErr)}}
			return
		}
		agentCtx := runner.WithOperationID(ctx, strconv.FormatInt(runID, 10))
		if verificationLease != nil {
			agentCtx = runner.WithManagedEnvironment(agentCtx, verificationLease.ManagedEnvironment())
		}
		if artifactAuthority != nil {
			agentCtx = runner.WithPlannerArtifactAuthority(agentCtx, artifactAuthority)
		}
		asks := make(chan runner.Ask)
		var resc <-chan runner.Result
		if plannerGate != nil {
			if plannerRunner != nil {
				resc = plannerRunner.RunPlanner(agentCtx, is.id, st.Name, a.Package, workdir, asks, plannerGate)
			} else {
				resc = e.cfg.Runner.Run(agentCtx, is.id, st.Name, a.Package, workdir, asks)
			}
		} else {
			resc = e.cfg.Runner.Run(agentCtx, is.id, st.Name, a.Package, workdir, asks)
		}
		for {
			select {
			case ask := <-asks:
				e.handleAsk(is, st.Name, a.Package, ask)
			case res := <-resc:
				if insErr == nil {
					status := "succeeded"
					if res.Err != nil {
						status = "failed"
					}
					tokens := res.Tokens
					if plannerController != nil {
						plannerTokensMu.Lock()
						if !plannerTokensRecorded {
							tokens = int(plannerController.Snapshot().ChargedTokens)
							plannerTokensRecorded = true
						} else {
							tokens = 0
						}
						plannerTokensMu.Unlock()
					}
					if err := e.cfg.Store.FinishStageRun(runID, status, res.SessionID, tokens); err != nil && res.Err == nil {
						res.Err = fmt.Errorf("finish stage run: %w", err)
					}
				}
				dones <- agentDone{a.Package, res}
				return
			}
		}
	}
	if st.Parallel {
		for _, a := range st.Agents {
			go runAgent(a)
		}
	} else {
		go func() {
			for _, a := range st.Agents {
				runAgent(a)
			}
		}()
	}

	need := len(st.Agents)
	var firstErr error
	succeeded := 0
	var discovered []string
	for i := 0; i < need; i++ {
		d := <-dones
		if d.res.SessionID != "" {
			sessionIDs = append(sessionIDs, d.res.SessionID)
		}
		if d.res.Err != nil {
			if firstErr == nil {
				firstErr = &runnerStageError{Agent: d.pkg, Result: d.res}
			}
			continue
		}
		succeeded++
		discovered = append(discovered, d.res.DependsOn...)
		if st.Completion == flow.CompletionAny {
			break
		}
	}
	if plannerController != nil {
		outcome := plannerController.Finish(firstErr)
		emitPlannerSnapshot(outcome, plannerController.Snapshot())
	}
	if st.Completion == flow.CompletionAll && firstErr != nil {
		return firstErr
	}
	if st.Completion == flow.CompletionAny && succeeded == 0 {
		return firstErr
	}
	if artifactAuthority != nil {
		if err := artifactAuthority.ValidateComplete(); err != nil {
			return fmt.Errorf("validate planner artifacts: %w", err)
		}
	}
	if st.MergeBarrier {
		if err := e.writeVerificationReceipt(ctx, is, workdir, verificationLease); err != nil {
			return err
		}
		verificationLeaseSealed = verificationLease != nil
	}
	// validate artifacts
	for _, name := range st.Artifacts {
		p := filepath.Join(workdir, name)
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("stage %s missing artifact %s", st.Name, name)
		}
	}
	checkpointArtifacts, err = contextpack.Archive(workdir, e.issueDir(is.id), st.Artifacts)
	if err != nil {
		return fmt.Errorf("archive stage artifacts: %w", err)
	}
	for _, artifact := range checkpointArtifacts {
		e.emit(core.EvArtifactProduced, is.id, map[string]string{
			"stage": st.Name, "artifact": artifact.Name,
			"path": filepath.Join(e.issueDir(is.id), "artifacts", artifact.Name)})
	}
	if st.Gate == flow.GatePlanReview {
		response, err := e.requestPlanReview(is, st, checkpointID, checkpointArtifacts)
		if err != nil {
			return err
		}
		if legacyAnswer(response) != 0 {
			if e.wasKilled(is) {
				e.emit(core.EvStageKilled, is.id, map[string]string{"stage": st.Name})
				return context.Canceled
			}
			return fmt.Errorf("plan review rejected")
		}
	} else if st.Gate == flow.GateApproveArtifact {
		response, err := e.requestArtifactReview(is, st, checkpointID, checkpointArtifacts)
		if err != nil {
			return err
		}
		if legacyAnswer(response) != 0 {
			if e.wasKilled(is) {
				e.emit(core.EvStageKilled, is.id, map[string]string{"stage": st.Name})
				return context.Canceled
			}
			return fmt.Errorf("stage %s artifacts require revision", st.Name)
		}
	}
	if normalized := deps.Normalize(discovered); len(normalized) > 0 {
		e.mu.Lock()
		all := append(append([]string(nil), is.dependsOn...), normalized...)
		e.mu.Unlock()
		if err := e.SetDependencies(is.id, deps.Normalize(all)); err != nil {
			return fmt.Errorf("discovered dependencies: %w", err)
		}
		return errDependenciesDiscovered
	}
	if st.MergeBarrier {
		prepared, err := e.prepareFinalization(is, verificationLease)
		if err != nil {
			return err
		}
		if prepared.Decision.Decision == "merge" {
			if err := e.checkpointVerificationReady(is); err != nil {
				return fmt.Errorf("persist verification ready: %w", err)
			}
		}
	}
	return nil
}

func (e *Engine) stageWorkdir(is *issueState, st flow.Stage) string {
	workdir := filepath.Join(e.cfg.DataDir, is.id)
	if st.Workspace != "none" && is.wsPath != "" {
		workdir = is.wsPath
	}
	return workdir
}

type runnerStageError struct {
	Agent  string
	Result runner.Result
}

func (e *runnerStageError) Error() string {
	if e.Result.Err == nil {
		return fmt.Sprintf("agent %s failed", e.Agent)
	}
	return fmt.Sprintf("agent %s: %v", e.Agent, e.Result.Err)
}

func (e *runnerStageError) Unwrap() error { return e.Result.Err }

func runnerFailurePayload(err error) map[string]any {
	var failure *runnerStageError
	if !errors.As(err, &failure) {
		return nil
	}
	result := failure.Result
	return map[string]any{
		"agent":             failure.Agent,
		"failure_class":     string(result.FailureClass),
		"attempt_kind":      string(result.Attempt.Kind),
		"attempt_state":     string(result.Attempt.State),
		"fallback_consumed": result.FallbackConsumed,
		"next_action":       result.NextAction,
		"attempt_count":     len(result.Attempts),
		"redacted_argv":     result.Attempt.RedactedArgv,
	}
}

func (e *Engine) runStage(ctx context.Context, is *issueState, st flow.Stage, plannerOverride *plannerbudget.Override) error {
	stageCtx, cancel := context.WithCancel(ctx)
	e.mu.Lock()
	is.stageCancel = cancel
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		is.stageCancel = nil
		e.mu.Unlock()
		cancel()
	}()

	if st.MergeBarrier && e.cfg.Marshal != nil {
		if err := e.cfg.Marshal.ReadyToMerge(stageCtx, is.id); err != nil {
			return err
		}
	}
	if st.HeavySlot {
		e.emit(core.EvSlotQueued, is.id, map[string]string{"stage": st.Name})
		release, err := e.cfg.Pool.Acquire(stageCtx, is.id, is.priority)
		if err != nil {
			return err
		}
		e.emit(core.EvSlotAcquired, is.id, map[string]string{"stage": st.Name})
		defer func() {
			release()
			e.emit(core.EvSlotReleased, is.id, map[string]string{"stage": st.Name})
		}()
	}

	var err error
	of := st.Retries + 1
	for attempt := 0; attempt <= st.Retries; attempt++ {
		err = e.runStageOnce(stageCtx, is, st, attempt+1, of, plannerOverride)
		if err == nil {
			break
		}
		if errors.Is(err, errDependenciesDiscovered) {
			return err
		}
		if e.wasKilled(is) {
			e.emit(core.EvStageKilled, is.id, map[string]any{"stage": st.Name})
			return context.Canceled
		}
		payload := map[string]any{
			"stage": st.Name, "error": err.Error(),
			"attempt": attempt + 1, "of": of, "final": attempt == st.Retries}
		for key, value := range runnerFailurePayload(err) {
			payload[key] = value
		}
		e.emit(core.EvStageFailed, is.id, payload)
	}
	if err != nil {
		return err
	}
	if e.wasKilled(is) {
		e.emit(core.EvStageKilled, is.id, map[string]any{"stage": st.Name})
		return context.Canceled
	}
	if st.Workspace == "worktree" && is.branch != "" && is.baseRef != "" {
		evDir := filepath.Join(e.cfg.DataDir, is.id, "evidence", st.Name)
		if b, err := evidence.Collect(is.wsPath, is.baseRef, evDir); err == nil {
			e.emit(core.EvArtifactProduced, is.id, map[string]any{
				"stage": st.Name, "artifact": "evidence.json",
				"path": filepath.Join(evDir, "evidence.json"), "area_weight": b.AreaWeight})
			e.emit(core.EvArtifactProduced, is.id, map[string]any{
				"stage": st.Name, "artifact": "diff.patch", "path": filepath.Join(evDir, "diff.patch")})
		}
	}

	// Any stage may declare a touchset describing files the issue will change;
	// register it so the marshal can sequence overlapping integrations.
	e.registerApprovedTouchset(is, st)
	verificationReady := false
	if st.MergeBarrier {
		integration, ok, err := e.cfg.Store.IssueIntegration(is.id)
		if err != nil {
			e.emit(core.EvStageFailed, is.id, map[string]any{
				"stage": st.Name, "error": err.Error(), "attempt": of, "of": of, "final": true,
			})
			return err
		}
		verificationReady = ok && integration.State == store.IntegrationVerificationReady
	}
	e.emit(core.EvStageCompleted, is.id, map[string]string{"stage": st.Name})
	if verificationReady {
		e.emit(core.EvVerificationReady, is.id, nil)
	}
	return nil
}

func sameResolvedPlanReviewPolicy(left, right review.ResolvedPolicy) bool {
	return left == right
}

func hasEventForDecision(events []core.Event, typ core.EventType, issueID string, decisionID int64) bool {
	for _, event := range events {
		if event.IssueID != issueID || event.Type != typ {
			continue
		}
		var payload struct {
			DecisionID int64 `json:"decision_id"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.DecisionID == decisionID {
			return true
		}
	}
	return false
}

func (e *Engine) planReviewAuthorization(is *issueState) (store.DecisionRow, []core.Event, error) {
	rows, err := e.cfg.Store.ArtifactReviewRows(is.id)
	if err != nil {
		return store.DecisionRow{}, nil, err
	}
	var found *store.DecisionRow
	for i := range rows {
		if rows[i].Review != nil && rows[i].ReviewPolicy != nil {
			found = &rows[i]
		}
	}
	if found == nil {
		return store.DecisionRow{}, nil, fmt.Errorf("plan review approval is missing")
	}
	if !sameResolvedPlanReviewPolicy(is.planReview, *found.ReviewPolicy) {
		return store.DecisionRow{}, nil, fmt.Errorf("plan review policy snapshot is inconsistent")
	}
	if found.Status != "answered" && found.Status != "auto" {
		return store.DecisionRow{}, nil, fmt.Errorf("plan review is not approved")
	}
	if found.Response.Kind != levers.DecisionChoice || found.Response.Option == nil || *found.Response.Option != 0 ||
		found.Approval == nil {
		return store.DecisionRow{}, nil, fmt.Errorf("plan review approval provenance is missing")
	}
	policy := *found.ReviewPolicy
	approval := *found.Approval
	var approvalEvent core.EventType
	switch {
	case policy.HumanRequired && approval.Kind == review.ApprovalHuman && approval.ActorID != "":
		if found.Status != "answered" {
			return store.DecisionRow{}, nil, fmt.Errorf("human plan approval has invalid status")
		}
		approvalEvent = core.EvPlanReviewHumanApproved
	case policy.PolicyAutoApproval && approval.Kind == review.ApprovalPolicy &&
		approval.PolicyID == policy.PolicyID && approval.PolicyVersion == policy.PolicyVersion:
		if found.Status != "auto" {
			return store.DecisionRow{}, nil, fmt.Errorf("policy plan approval has invalid status")
		}
		approvalEvent = core.EvPlanReviewPolicyApproved
	default:
		return store.DecisionRow{}, nil, fmt.Errorf("plan review approval provenance is not permitted")
	}
	events, err := e.cfg.Store.EventsSince(0)
	if err != nil {
		return store.DecisionRow{}, nil, err
	}
	if !hasEventForDecision(events, approvalEvent, is.id, found.ID) {
		return store.DecisionRow{}, nil, fmt.Errorf("plan review approval audit event is missing")
	}
	return *found, events, nil
}

func (e *Engine) runFrom(ctx context.Context, is *issueState, startIdx int, plannerOverride *plannerbudget.Override) error {
	if err := e.freezeTaskSummary(is); err != nil {
		return err
	}
	e.mu.Lock()
	if is.running {
		e.mu.Unlock()
		return fmt.Errorf("issue %s is already running", is.id)
	}
	is.running = true
	e.mu.Unlock()
	f := e.cfg.Flows[is.flowName]
	// Fresh issues should build on the latest shared code. Fast-forward only
	// and non-fatal: a diverged or dirty base is reported, not a blocker.
	if startIdx == 0 && e.cfg.Train != nil {
		if err := e.cfg.Train.SyncBase(); err != nil {
			e.emit(core.EvBaseStale, is.id, map[string]string{"error": err.Error()})
		}
	}
	aborted := true
	landed := false
	preserveWorkspace := false
	var verification marshal.Verification
	defer func() {
		e.mu.Lock()
		is.activeTouchset = nil
		release := is.wsRelease
		preserveExternal := aborted && is.externalSession
		preserveIdentity := preserveWorkspace || preserveExternal
		if !preserveIdentity {
			is.wsRelease = nil
			is.wsPath = ""
			is.branch = ""
			is.baseRef = ""
		}
		is.running = false
		e.mu.Unlock()
		if release != nil && !landed && !preserveWorkspace && !preserveExternal {
			_ = release()
		}
		if aborted && e.cfg.Marshal != nil {
			e.cfg.Marshal.Aborted(is.id)
		}
	}()
	for i := startIdx; i < len(f.Stages); i++ {
		st := f.Stages[i]
		e.mu.Lock()
		is.stageIdx = i
		gate := is.pauseGate
		pauseStage := is.pauseStage
		e.mu.Unlock()
		if gate != nil {
			if pauseStage == 0 && i != 0 {
				pauseStage = i
			}
			paused, err := e.runState(is, pauseStage, "paused", "before_stage")
			if err != nil {
				return err
			}
			if err := e.cfg.Store.PersistPausedRun(paused); err != nil {
				return err
			}
			e.mu.Lock()
			is.pauseStage = pauseStage
			is.paused = true
			e.mu.Unlock()
			e.emit(core.EvIssuePaused, is.id, map[string]string{"stage": paused.Stage})
			select {
			case <-gate:
				e.emit(core.EvIssueResumed, is.id, nil)
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		e.mu.Lock()
		needsWorkspace := st.Workspace != "none" && e.cfg.Workspace != nil && is.wsPath == ""
		e.mu.Unlock()
		if needsWorkspace {
			path, release, err := e.cfg.Workspace.Acquire(is.id)
			if err != nil {
				_ = e.recordBoundaryFailure(ctx, is.id, st.Name, 0,
					failure.SiteWorkspace, failure.ClassUnavailable, failure.RetryAfterStateChange, failure.StateWorkspace, err)
				e.emit(core.EvStageFailed, is.id, map[string]string{"stage": st.Name, "error": "workspace: " + err.Error()})
				return err
			}
			branch := ""
			if out, err := exec.Command("git", "-C", path, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
				branch = strings.TrimSpace(string(out))
			}
			baseRef := ""
			if out, err := exec.Command("git", "-C", path, "rev-parse", "HEAD").Output(); err == nil {
				baseRef = strings.TrimSpace(string(out))
			}
			if e.cfg.Train != nil && e.cfg.Train.Repo != "" {
				if baseHead, headErr := gitRevision(e.cfg.Train.Repo, "HEAD"); headErr == nil {
					if mergeBase, mergeErr := gitCommandOutput(
						path, "merge-base", "HEAD", baseHead,
					); mergeErr == nil {
						baseRef = mergeBase
					}
				}
			}
			e.mu.Lock()
			is.wsPath, is.wsRelease = path, release
			is.branch, is.baseRef = branch, baseRef
			e.mu.Unlock()
		}
		if err := e.persistActiveRun(is, i); err != nil {
			return err
		}
		if err := e.checkBudget(is, st.Name); err != nil {
			return err
		}
		planReviewBeforeExecute := false
		if st.Name == "execute" {
			for _, previous := range f.Stages[:i] {
				if previous.Gate == flow.GatePlanReview {
					planReviewBeforeExecute = true
					break
				}
			}
		}
		if planReviewBeforeExecute {
			approval, events, err := e.planReviewAuthorization(is)
			if err != nil {
				return err
			}
			if !hasEventForDecision(events, core.EvExecutionStarted, is.id, approval.ID) {
				payload := map[string]any{
					"stage": st.Name, "decision_id": approval.ID,
					"mode":           approval.ReviewPolicy.Mode,
					"policy_id":      approval.ReviewPolicy.PolicyID,
					"policy_version": approval.ReviewPolicy.PolicyVersion,
					"approval_kind":  approval.Approval.Kind,
				}
				if approval.Approval.Kind == review.ApprovalHuman {
					payload["actor_id"] = approval.Approval.ActorID
				}
				if _, err := e.appendEvent(core.EvExecutionStarted, is.id, payload); err != nil {
					return fmt.Errorf("record execution start: %w", err)
				}
			}
		}
		if err := e.runStage(ctx, is, st, plannerOverride); errors.Is(err, errDependenciesDiscovered) {
			unmet, depErr := e.unmetDependencies(is.id)
			if depErr != nil {
				return depErr
			}
			e.mu.Lock()
			is.waitingDependencies = true
			e.mu.Unlock()
			e.emit(core.EvIssueWaitingDependencies, is.id, map[string]any{
				"unmet": unmet, "restart_stage": f.Stages[0].Name})
			return nil
		} else if err != nil {
			return err
		}
	}
	if _, _, integrating := f.IntegrationStage(); !integrating {
		var err error
		preserveWorkspace, err = e.completeWithoutIntegration(is)
		if err != nil {
			return err
		}
		aborted = false
		return nil
	}
	if _, _, integrating := f.IntegrationStage(); integrating {
		decision, receipt, err := e.finalVerificationDecision(is)
		if err != nil {
			return err
		}
		if decision == "hold" {
			e.emit(core.EvIssueCompleted, is.id, map[string]string{
				"merge": "left-unmerged", "branch": is.branch})
			if e.cfg.Marshal != nil {
				e.cfg.Marshal.Merged(is.id)
			}
			aborted = false
			return nil
		}
		verification = receipt
	}
	var err error
	landed, preserveWorkspace, err = e.finalizeIntegration(ctx, is, verification)
	if err != nil {
		return err
	}
	aborted = false
	return nil
}

func (e *Engine) completeWithoutIntegration(is *issueState) (bool, error) {
	if is.wsPath == "" || is.branch == "" {
		e.emit(core.EvIssueCompleted, is.id, map[string]string{"merge": "none"})
		return false, nil
	}
	head, err := gitRevision(is.wsPath, "HEAD")
	if err != nil {
		return false, err
	}
	_, _, dirty := repositoryState(is.wsPath, nil)
	if !dirty && head == is.baseRef {
		if is.wsRelease != nil {
			if err := is.wsRelease(); err != nil {
				return false, err
			}
			is.wsRelease = nil
		}
		if e.cfg.Train != nil {
			if err := e.cfg.Train.DeleteBranch(is.branch); err != nil {
				return false, err
			}
		}
		e.emit(core.EvIssueCompleted, is.id, map[string]string{"merge": "none"})
		return false, nil
	}
	if err := e.cfg.Store.SetIssueIntegration(store.IssueIntegration{
		IssueID: is.id, State: store.IntegrationPreserved, PreSHA: is.baseRef,
		Worktree: is.wsPath, Branch: is.branch,
	}); err != nil {
		return false, err
	}
	e.emit(core.EvIssueCompleted, is.id, map[string]string{
		"merge": "left-unmerged", "branch": is.branch, "worktree": is.wsPath,
	})
	return true, nil
}

func (e *Engine) finalVerificationDecision(
	is *issueState,
) (string, marshal.Verification, error) {
	prepared, err := e.prepareFinalization(is, nil)
	if err != nil {
		return "", marshal.Verification{}, err
	}
	return prepared.Decision.Decision, prepared.Verification, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func gitRevision(dir, revision string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", revision).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %v: %s", revision, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// runAndRecord runs an issue from startIdx and records whether it ended
// terminally. StartIssue, RetryStage, and Resume all restart a lane through
// this one path so their bookkeeping cannot drift apart.
func (e *Engine) runAndRecord(ctx context.Context, is *issueState, startIdx int) error {
	return e.runAndRecordWithPlannerBudget(ctx, is, startIdx, nil)
}

func (e *Engine) runAndRecordWithPlannerBudget(ctx context.Context, is *issueState, startIdx int, plannerOverride *plannerbudget.Override) error {
	err := e.runFrom(ctx, is, startIdx, plannerOverride)
	e.mu.Lock()
	is.terminal = err != nil
	e.mu.Unlock()
	return err
}

func (e *Engine) StartIssue(ctx context.Context, id string) error {
	return e.StartIssueWithBudget(ctx, id, nil)
}

func (e *Engine) StartIssueWithBudget(ctx context.Context, id string, plannerOverride *plannerbudget.Override) error {
	e.mu.Lock()
	is, ok := e.issues[id]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown issue %s", id)
	}
	return e.startOrWaitWithPlannerBudget(ctx, is, plannerOverride)
}

func (e *Engine) loadDoneUnmergedForRetry(issueID string) (*issueState, error) {
	rows, err := e.cfg.Store.Issues()
	if err != nil {
		return nil, err
	}
	var found *store.IssueRow
	for i := range rows {
		if rows[i].ID == issueID {
			found = &rows[i]
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("unknown issue %s", issueID)
	}
	if found.State != "done (unmerged)" {
		return nil, fmt.Errorf("unknown issue %s", issueID)
	}
	f, ok := e.cfg.Flows[found.Flow]
	if !ok {
		return nil, fmt.Errorf("flow %q no longer configured", found.Flow)
	}
	stageIdx, err := finalStageIndex(f)
	if err != nil {
		return nil, err
	}
	is := &issueState{
		id: found.ID, title: found.Title, body: found.Body, flowName: found.Flow,
		matrix: matrixFromStrings(found.Levers), priority: found.Priority,
		dependsOn: append([]string(nil), found.DependsOn...),
		stageIdx:  stageIdx, terminal: true, planReview: found.PlanReviewPolicy,
	}
	e.restoreInterruptedWorkspace(is)
	return is, nil
}

// RetryStage restarts a terminal issue at the stage that last failed or was
// killed. A persisted unmerged hold is loaded on demand so it stays inert
// across daemon restarts until the operator explicitly retries it. Earlier
// successful stages are not repeated.
func (e *Engine) RetryStage(ctx context.Context, issueID string) error {
	return e.RetryStageWithBudget(ctx, issueID, nil)
}

func (e *Engine) RetryStageWithBudget(ctx context.Context, issueID string, plannerOverride *plannerbudget.Override) error {
	e.mu.Lock()
	is, ok := e.issues[issueID]
	if !ok {
		e.mu.Unlock()
		loaded, err := e.loadDoneUnmergedForRetry(issueID)
		if err != nil {
			return err
		}
		e.mu.Lock()
		if existing, exists := e.issues[issueID]; exists {
			is = existing
		} else {
			is = loaded
			e.issues[issueID] = loaded
		}
	}
	if is.running {
		e.mu.Unlock()
		return fmt.Errorf("issue %s is already running", issueID)
	}
	integration, publishPending, err := e.cfg.Store.IssueIntegration(issueID)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	if publishPending && integration.State == store.IntegrationCleanupNeeded {
		is.running = true
		e.mu.Unlock()
		err := e.retryCleanup(ctx, is, integration)
		e.mu.Lock()
		is.running = false
		e.mu.Unlock()
		return err
	}
	if publishPending && integration.State == store.IntegrationPublishPending {
		is.running = true
		e.mu.Unlock()
		err := e.retryPublish(ctx, is, integration)
		e.mu.Lock()
		is.running = false
		is.terminal = err != nil
		e.mu.Unlock()
		return err
	}
	if publishPending && integration.State == store.IntegrationVerificationReady {
		is.running = true
		is.terminal = false
		is.killRequested = false
		e.mu.Unlock()
		err := e.retryVerifiedFinalization(ctx, is, integration)
		e.mu.Lock()
		is.running = false
		is.terminal = err != nil
		if err == nil {
			is.wsRelease = nil
			is.wsPath = ""
			is.branch = ""
			is.baseRef = ""
		}
		e.mu.Unlock()
		return err
	}
	if !is.terminal {
		e.mu.Unlock()
		return fmt.Errorf("issue %s has no failed stage to retry", issueID)
	}
	startIdx := is.stageIdx
	is.terminal = false
	is.killRequested = false
	e.mu.Unlock()
	return e.runAndRecordWithPlannerBudget(ctx, is, startIdx, plannerOverride)
}

// SetLever changes the routing lever for one stage and records the change.
func (e *Engine) SetLever(issueID, stage string, l flow.Lever) error {
	if l != flow.LeverYolo && l != flow.LeverRegular && l != flow.LeverStrict {
		return fmt.Errorf("invalid lever %q", l)
	}
	e.mu.Lock()
	is, ok := e.issues[issueID]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("unknown issue %s", issueID)
	}
	f := e.cfg.Flows[is.flowName]
	found := false
	for _, st := range f.Stages {
		if st.Name == stage {
			found = true
			break
		}
	}
	if !found {
		e.mu.Unlock()
		return fmt.Errorf("unknown stage %s", stage)
	}
	if is.matrix == nil {
		is.matrix = levers.Matrix{}
	}
	is.matrix[stage] = l
	if e.cfg.Store != nil {
		if err := e.cfg.Store.SetIssueLever(issueID, stage, string(l)); err != nil {
			e.mu.Unlock()
			return err
		}
	}
	e.mu.Unlock()
	e.emit(core.EvLeverChanged, issueID, map[string]string{"stage": stage, "lever": string(l)})
	return nil
}

func (e *Engine) landWithEscalation(
	ctx context.Context, is *issueState, verification marshal.Verification,
) (marshal.LandResult, error) {
	result, err := e.cfg.Train.LandVerified(ctx, is.id, is.branch, verification)
	for err != nil {
		var pending *marshal.PublishPendingError
		if errors.As(err, &pending) {
			return result, err
		}
		var conflict *marshal.ConflictError
		if !errors.As(err, &conflict) {
			return result, err
		}
		e.emit(core.EvMergeConflict, is.id, map[string]any{
			"error": conflict.Error(), "base_branch": conflict.BaseBranch,
			"base_sha": conflict.BaseSHA, "files": conflict.Files,
		})
		decision, resolveErr := e.resolveConflict(ctx, is, conflict)
		if resolveErr != nil {
			return result, resolveErr
		}
		if decision == "hold" {
			return result, errConflictHeld
		}
		// The resolver changed the issue branch after its verification receipt
		// was recorded. Force the full recorded command set to replay against
		// the newly integrated tree even if content happens to converge.
		verification.TreeSHA = ""
		result, err = e.cfg.Train.LandVerified(ctx, is.id, is.branch, verification)
	}
	return result, nil
}

func (e *Engine) resolveConflict(
	ctx context.Context, is *issueState, conflict *marshal.ConflictError,
) (string, error) {
	body := fmt.Sprintf(
		"# Merge conflict\n\n"+
			"- Issue branch: `%s`\n"+
			"- Current base branch: `%s`\n"+
			"- Current base SHA: `%s`\n"+
			"- Original base SHA: `%s`\n"+
			"- Error: `%s`\n"+
			"- Conflicting files:\n",
		is.branch, conflict.BaseBranch, conflict.BaseSHA, is.baseRef, conflict.Error())
	for _, name := range conflict.Files {
		body += fmt.Sprintf("  - `%s`\n", name)
	}
	body += "\n## Merge output\n\n```\n" + conflict.Output + "\n```\n"
	if err := os.WriteFile(filepath.Join(is.wsPath, "CONFLICT.md"), []byte(body), 0o644); err != nil {
		return "", err
	}
	artifacts, err := contextpack.Archive(is.wsPath, e.issueDir(is.id), []string{"CONFLICT.md"})
	if err != nil {
		return "", fmt.Errorf("archive conflict context: %w", err)
	}
	for _, artifact := range artifacts {
		e.emit(core.EvArtifactProduced, is.id, map[string]string{
			"stage": "conflict-resolution", "artifact": artifact.Name,
			"path": filepath.Join(e.issueDir(is.id), "artifacts", artifact.Name),
		})
	}
	stage := flow.Stage{
		Name: "conflict-resolution",
		Agents: []flow.AgentRef{{
			Package: "conflict-resolver",
		}},
		Workspace:  "worktree",
		Gate:       flow.GateAuto,
		Completion: flow.CompletionAll,
		Artifacts:  []string{"conflict-report.md", "conflict-decision.json"},
	}
	if err := e.runStage(ctx, is, stage, nil); err != nil {
		return "", err
	}
	decision, err := loadConflictDecision(filepath.Join(is.wsPath, "conflict-decision.json"))
	if err != nil {
		return "", err
	}
	if decision == "hold" {
		return decision, nil
	}
	if decision != "resolved" {
		return "", fmt.Errorf("invalid conflict decision %q: want resolved or hold", decision)
	}
	if status, err := gitCommandOutput(is.wsPath, "status", "--porcelain", "--untracked-files=no"); err != nil {
		return "", err
	} else if status != "" {
		return "", fmt.Errorf("resolved issue branch is dirty: %s", status)
	}
	if output, err := exec.Command(
		"git", "-C", is.wsPath, "merge-base", "--is-ancestor", conflict.BaseSHA, "HEAD",
	).CombinedOutput(); err != nil {
		return "", fmt.Errorf(
			"resolved issue branch is not rebased onto %s: %v: %s",
			conflict.BaseSHA, err, strings.TrimSpace(string(output)))
	}
	return decision, nil
}

func loadConflictDecision(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read conflict decision: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	var document struct {
		Decision string `json:"decision"`
	}
	if err := decoder.Decode(&document); err != nil {
		return "", fmt.Errorf("decode conflict decision: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return "", fmt.Errorf("decode conflict decision: %w", err)
	}
	return document.Decision, nil
}

func gitCommandOutput(dir string, args ...string) (string, error) {
	output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err,
			strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func (e *Engine) retryPublish(
	ctx context.Context, is *issueState, integration store.IssueIntegration,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.emit(core.EvPublishRetry, is.id, map[string]string{
		"branch": integration.BaseBranch, "commit": integration.LandedSHA})
	if err := e.cfg.Train.Publish(integration.LandedSHA, integration.BaseBranch); err != nil {
		integration.LastError = err.Error()
		if storeErr := e.cfg.Store.SetIssueIntegration(integration); storeErr != nil {
			return fmt.Errorf("%v (persist publish retry: %w)", err, storeErr)
		}
		e.emit(core.EvPublishPending, is.id, map[string]string{
			"branch": integration.BaseBranch, "commit": integration.LandedSHA,
			"error": err.Error(),
		})
		return err
	}
	e.emit(core.EvPublishSucceeded, is.id, map[string]string{
		"branch": integration.BaseBranch, "commit": integration.LandedSHA})
	e.emit(core.EvIssueMerged, is.id, map[string]string{
		"branch": integration.BaseBranch, "commit": integration.LandedSHA})
	e.wakeDependents(context.Background(), is.id)
	if err := e.retryCleanup(ctx, is, integration); err != nil {
		return err
	}
	if e.cfg.Marshal != nil {
		e.cfg.Marshal.Merged(is.id)
	}
	e.emit(core.EvIssueCompleted, is.id, nil)
	return nil
}

const (
	cleanupReleasePrefix = "release_worktree:"
	cleanupDeletePrefix  = "delete_branch:"
)

func cleanupOperations(worktreePath, branch string) []string {
	var operations []string
	if worktreePath != "" {
		operations = append(operations, cleanupReleasePrefix+worktreePath)
	}
	if branch != "" {
		operations = append(operations, cleanupDeletePrefix+branch)
	}
	return operations
}

func (e *Engine) finishLandingCleanup(
	issueID string,
	integration store.IssueIntegration,
	worktreePath, branch string,
	release func() error,
) error {
	operations := cleanupOperations(worktreePath, branch)
	if release != nil {
		if err := release(); err != nil {
			return e.recordCleanupNeeded(integration, operations, err)
		}
	}
	if branch != "" && e.cfg.Train != nil {
		if err := e.cfg.Train.DeleteBranch(branch); err != nil {
			return e.recordCleanupNeeded(
				integration, []string{cleanupDeletePrefix + branch}, err)
		}
	}
	integration.State = store.IntegrationMerged
	integration.Cleanup = nil
	integration.LastError = ""
	return e.cfg.Store.SetIssueIntegration(integration)
}

func (e *Engine) retryCleanup(
	ctx context.Context, is *issueState, integration store.IssueIntegration,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	remaining := append([]string(nil), integration.Cleanup...)
	for len(remaining) > 0 {
		operation := remaining[0]
		var err error
		switch {
		case strings.HasPrefix(operation, cleanupReleasePrefix):
			path := strings.TrimPrefix(operation, cleanupReleasePrefix)
			releaser, ok := e.cfg.Workspace.(workspace.Releaser)
			if !ok {
				err = fmt.Errorf("workspace provider cannot retry release of %s", path)
			} else {
				err = releaser.ReleasePath(path)
			}
		case strings.HasPrefix(operation, cleanupDeletePrefix):
			branch := strings.TrimPrefix(operation, cleanupDeletePrefix)
			if e.cfg.Train == nil {
				err = fmt.Errorf("merge train cannot retry branch deletion of %s", branch)
			} else {
				err = e.cfg.Train.DeleteBranch(branch)
			}
		default:
			err = fmt.Errorf("unknown cleanup operation %q", operation)
		}
		if err != nil {
			return e.recordCleanupNeeded(integration, remaining, err)
		}
		remaining = remaining[1:]
	}
	integration.State = store.IntegrationMerged
	integration.Cleanup = nil
	integration.LastError = ""
	if err := e.cfg.Store.SetIssueIntegration(integration); err != nil {
		return err
	}
	e.emit(core.EvCleanupCompleted, is.id, map[string]string{
		"commit": integration.LandedSHA})
	return nil
}

func (e *Engine) recordCleanupNeeded(
	integration store.IssueIntegration, operations []string, cleanupErr error,
) error {
	integration.State = store.IntegrationCleanupNeeded
	integration.Cleanup = append([]string(nil), operations...)
	integration.LastError = cleanupErr.Error()
	if err := e.cfg.Store.SetIssueIntegration(integration); err != nil {
		return fmt.Errorf("%v (persist cleanup: %w)", cleanupErr, err)
	}
	e.emit(core.EvCleanupNeeded, integration.IssueID, map[string]any{
		"operations": operations, "error": cleanupErr.Error(),
		"commit": integration.LandedSHA,
	})
	return nil
}

func (e *Engine) checkBudget(is *issueState, stage string) error {
	if e.cfg.TokenBudget <= 0 || is.budgetWaived {
		return nil
	}
	spent, err := e.cfg.Store.IssueTokens(is.id)
	if err != nil || spent <= e.cfg.TokenBudget {
		return nil
	}
	e.emit(core.EvBudgetExceeded, is.id, map[string]any{"spent": spent, "budget": e.cfg.TokenBudget})
	d := levers.Decision{
		Kind:        levers.DecisionChoice,
		Question:    fmt.Sprintf("Issue %s exceeded its token budget (%d/%d). Continue?", is.id, spent, e.cfg.TokenBudget),
		Options:     []string{"continue", "abort"},
		Recommended: 1,
		Importance:  1.0,
		Why:         "The durable token total is above the configured limit for this issue.",
		Consequences: []string{
			fmt.Sprintf("Continues into %s and waives further token-budget checks for this issue until Watchtower restarts.", stage),
			fmt.Sprintf("Stops this run before %s starts; retrying %s asks for budget authorization again.", stage, stage),
		},
		Reversible:         "Aborting preserves completed stages and can be retried; continuing cannot recover tokens already spent.",
		EngineContinuation: fmt.Sprintf("Continue to resume %s with the budget waived, or abort to stop before %s starts.", stage, stage),
		RequiresOption:     true,
		Briefing: &levers.Briefing{
			Proof: []levers.BriefingProof{{
				Claim: fmt.Sprintf("The issue has consumed %d tokens against a configured budget of %d.", spent, e.cfg.TokenBudget),
				Cite:  "durable stage-run token accounting",
			}},
		},
	}
	agentPkg, agentErr := e.decisionAgentPackage(is.flowName, stage)
	if agentErr != nil {
		return agentErr
	}
	response, err := e.escalate(is, stage, agentPkg, d)
	if err != nil {
		return err
	}
	if legacyAnswer(response) == 0 {
		is.budgetWaived = true
		return nil
	}
	err = fmt.Errorf("issue %s aborted: token budget exceeded", is.id)
	e.emit(core.EvStageFailed, is.id, map[string]string{"stage": stage, "error": err.Error()})
	return err
}
