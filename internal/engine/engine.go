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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/weston6142/watchtower/internal/attach"
	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/deps"
	"github.com/weston6142/watchtower/internal/evidence"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/librarian"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/slots"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/touchset"
	"github.com/weston6142/watchtower/internal/workspace"
)

var errDependenciesDiscovered = errors.New("new dependencies discovered")
var errConflictHeld = errors.New("merge conflict held")

type Config struct {
	Store       *store.Store
	Runner      runner.Runner
	Marshal     Sequencer
	Train       *marshal.Train
	Librarian   *librarian.Librarian
	Observers   []func(core.Event)
	Pool        *slots.Pool
	Flows       map[string]flow.Flow
	Rules       levers.Rules
	DataDir     string
	Workspace   workspace.Provider
	TokenBudget int
	OnLine      func(issueID, stage, line string)
}

type Sequencer interface {
	BlockedBehind(string) int
	PlanApproved(string, touchset.Set)
	ReadyToMerge(context.Context, string) error
	Merged(string)
	Aborted(string)
}

type PendingDecision struct {
	ID      int64
	IssueID string
	Stage   string
	D       levers.Decision
}

type pending struct {
	PendingDecision
	reply chan levers.Response
}

type issueState struct {
	id                  string
	title               string
	body                string
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
	terminal            bool
	running             bool
	draft               bool
	budgetWaived        bool
	activeTouchset      *touchset.Set
	dependsOn           []string
	waitingDependencies bool
}

type Engine struct {
	cfg    Config
	mu     sync.Mutex
	nextID int
	issues map[string]*issueState
	pend   map[int64]*pending
}

func New(cfg Config) *Engine {
	e := &Engine{cfg: cfg, issues: map[string]*issueState{}, pend: map[int64]*pending{}}
	if cfg.OnLine != nil {
		if sink, ok := cfg.Runner.(runner.LineSink); ok {
			sink.SetOnLine(cfg.OnLine)
		}
	}
	return e
}

// Rehydrate rebuilds in-memory state from the store after a daemon restart.
// Non-terminal issues become terminal (retryable via RetryStage) — stages are
// never auto-restarted, so a reboot cannot spend tokens on its own. Pending
// decisions whose stage goroutine died with the old process are closed as
// "orphaned"; retrying the stage re-raises them. Paused and killed issues are
// rehydrated the same way: a pause gate with no parked goroutine is
// meaningless. Idempotent.
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

	// Close pending decisions with no living stage goroutine before marking
	// their issues failed: the answered event returns projection state to
	// "running" so the failure marker below lands last.
	prows, err := e.cfg.Store.PendingDecisionRows()
	if err != nil {
		return err
	}
	for _, row := range prows {
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
		if row.State == "done" || row.State == "done (unmerged)" || row.State == "merged" || row.State == "abandoned" {
			continue
		}
		integration, hasIntegration, err := e.cfg.Store.IssueIntegration(row.ID)
		if err != nil {
			return err
		}
		if hasIntegration && integration.State == store.IntegrationVerificationReady {
			is := &issueState{
				id: row.ID, title: row.Title, body: row.Body, flowName: row.Flow,
				matrix: matrixFromStrings(row.Levers), priority: row.Priority,
				dependsOn: append([]string(nil), row.DependsOn...), running: true,
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
		if row.State == "backlog" {
			e.mu.Lock()
			if _, known := e.issues[row.ID]; !known {
				matrix := levers.Matrix{}
				for st, lv := range row.Levers {
					matrix[st] = flow.Lever(lv)
				}
				e.issues[row.ID] = &issueState{
					id: row.ID, title: row.Title, body: row.Body, flowName: row.Flow,
					matrix: matrix, priority: row.Priority, draft: true,
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
		}
		e.restoreInterruptedWorkspace(is)
		e.mu.Lock()
		e.issues[row.ID] = is
		e.mu.Unlock()
		e.emit(core.EvStageFailed, row.ID, map[string]any{
			"stage": stage, "attempt": attempt, "of": of,
			"error": "daemon restarted — press R to retry", "final": true})
	}
	return nil
}

func matrixFromStrings(values map[string]string) levers.Matrix {
	matrix := levers.Matrix{}
	for stage, value := range values {
		matrix[stage] = flow.Lever(value)
	}
	return matrix
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
	return e.runAndRecord(ctx, is, 0)
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
	defer e.mu.Unlock()
	is, ok := e.issues[issueID]
	if !ok {
		return fmt.Errorf("unknown issue %s", issueID)
	}
	if is.pauseGate == nil {
		is.pauseGate = make(chan struct{})
	}
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
	if is.running {
		if is.pauseGate == nil {
			e.mu.Unlock()
			return fmt.Errorf("issue %s is not paused", issueID)
		}
		close(is.pauseGate)
		is.pauseGate = nil
		e.mu.Unlock()
		return nil
	}
	// terminal, not merely stopped: a lane created and paused before it ever
	// started must stay unstarted, and only clear its gate.
	if !is.terminal {
		is.pauseGate = nil
		e.mu.Unlock()
		return nil
	}
	startIdx := is.stageIdx
	is.pauseGate = nil
	is.killRequested = false
	is.terminal = false
	e.mu.Unlock()
	// Detached, like start_issue and retry_stage: failures surface as
	// stage_failed events, not in the caller's response.
	go e.runAndRecord(context.Background(), is, startIdx)
	return nil
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
		close(p.reply)
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

func (e *Engine) wasKilled(is *issueState) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return is.killRequested
}

// emit appends an event best-effort: marshal or store failures are dropped
// rather than aborting the stage, since events are observability, not state.
func (e *Engine) emit(t core.EventType, issueID string, payload any) {
	ev, err := core.NewEvent(t, issueID, payload)
	if err != nil {
		return
	}
	ev, err = e.cfg.Store.Append(ev)
	if err != nil {
		return
	}
	for _, observer := range e.cfg.Observers {
		observer(ev)
	}
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
	e.mu.Lock()
	e.nextID++
	id := fmt.Sprintf("GH-%d", e.nextID)
	e.issues[id] = &issueState{id: id, title: title, body: body, flowName: flowName, matrix: m, priority: priority}
	e.mu.Unlock()
	if err := attach.Save(e.issueDir(id), set); err != nil {
		e.rollbackCreate(id)
		return "", err
	}
	if err := e.cfg.Store.UpsertIssue(store.IssueRow{
		ID: id, Title: title, Body: body, Flow: flowName, State: "running", Levers: matrixStrings(m), Priority: priority,
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

// LaunchIssue promotes a backlog draft into a running lane: the row flips to
// running, the standard issue_created event fires (projection and steward
// already treat it as the start of a lane), and the flow runs detached like
// Resume — failures surface as stage_failed events, not in this response.
func (e *Engine) LaunchIssue(id string) error {
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
	e.mu.Unlock()
	if err := e.cfg.Store.UpsertIssue(store.IssueRow{
		ID: id, Title: title, Body: body, Flow: flowName, State: "running", Levers: matrixStrings(matrix), Priority: priority,
	}); err != nil {
		e.mu.Lock()
		is.draft = true
		e.mu.Unlock()
		return err
	}
	e.emit(core.EvIssueCreated, id, map[string]any{
		"title": title, "flow": flowName, "body": body, "priority": priority})
	go e.startOrWait(context.Background(), is)
	return nil
}

func matrixStrings(m levers.Matrix) map[string]string {
	values := make(map[string]string, len(m))
	for stage, lever := range m {
		values[stage] = string(lever)
	}
	return values
}

func (e *Engine) PendingDecisions() []PendingDecision {
	rows, err := e.cfg.Store.PendingDecisionRows()
	if err != nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []PendingDecision
	for _, row := range rows {
		if p, ok := e.pend[row.ID]; ok {
			out = append(out, p.PendingDecision)
		}
	}
	return out
}

func (e *Engine) Answer(decisionID int64, response levers.Response) error {
	e.mu.Lock()
	p, ok := e.pend[decisionID]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("no pending decision %d", decisionID)
	}
	if !p.D.Accepts(response) {
		e.mu.Unlock()
		return fmt.Errorf("invalid response for decision %d", decisionID)
	}
	delete(e.pend, decisionID)
	e.mu.Unlock()
	e.emit(core.EvDecisionAnswered, p.IssueID, map[string]any{
		"decision_id": p.ID, "response": response})
	p.reply <- response
	return e.cfg.Store.AnswerDecision(decisionID, response, "answered")
}

// escalate blocks until the human answers; returns the typed response.
func (e *Engine) escalate(issueID, stage string, d levers.Decision) levers.Response {
	rowID, err := e.cfg.Store.InsertDecision(store.DecisionRow{
		IssueID: issueID, Stage: stage, Question: d.Question,
		Options: d.Options, Recommended: d.Recommended,
		Kind: d.Kind, RecommendedResponse: d.RecommendedResponse,
		AllowFreeform: d.AllowFreeform, Importance: d.Importance, Paths: d.Paths,
		Why: d.Why, Consequences: d.Consequences, Reversible: d.Reversible,
		BlockingCost: e.blockingCost(issueID),
	})
	if err != nil {
		panic(fmt.Sprintf("insert decision: %v", err))
	}
	p := &pending{
		PendingDecision: PendingDecision{ID: rowID, IssueID: issueID, Stage: stage, D: d},
		reply:           make(chan levers.Response, 1),
	}
	e.mu.Lock()
	e.pend[rowID] = p
	e.mu.Unlock()
	e.emit(core.EvDecisionRequired, issueID, map[string]any{
		"decision_id": p.ID, "stage": stage, "question": d.Question,
		"options": d.Options, "recommended": d.Recommended, "why": d.Why,
		"kind": d.Kind, "recommended_response": d.RecommendedResponse,
		"allow_freeform": d.AllowFreeform, "importance": d.Importance,
		"consequences": d.Consequences, "reversible": d.Reversible, "paths": d.Paths})
	choice, ok := <-p.reply
	if !ok {
		return levers.Response{}
	}
	return choice
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
			Flow: flowName, Levers: matrixStrings(matrix),
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
			matrix: matrix, dependsOn: append([]string(nil), edges[issue.ID]...),
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

func (e *Engine) handleAsk(is *issueState, stage string, a runner.Ask) {
	lever := is.matrix[stage]
	if levers.Route(a.Decision, lever, e.cfg.Rules) {
		a.Reply <- e.escalate(is.id, stage, a.Decision)
		return
	}
	if _, err := e.cfg.Store.InsertDecision(store.DecisionRow{
		IssueID: is.id, Stage: stage, Question: a.Decision.Question,
		Options: a.Decision.Options, Recommended: a.Decision.Recommended,
		Kind: a.Decision.Kind, RecommendedResponse: a.Decision.RecommendedResponse,
		AllowFreeform: a.Decision.AllowFreeform, Importance: a.Decision.Importance,
		Paths: a.Decision.Paths,
		Why:   a.Decision.Why, Consequences: a.Decision.Consequences, Reversible: a.Decision.Reversible,
		Status: "auto", Response: a.Decision.RecommendedAnswer(),
		BlockingCost: e.blockingCost(is.id),
	}); err != nil {
		panic(fmt.Sprintf("insert auto decision: %v", err))
	}
	e.emit(core.EvDecisionAutoResolved, is.id, map[string]any{
		"stage": stage, "question": a.Decision.Question,
		"response": a.Decision.RecommendedAnswer()})
	a.Reply <- a.Decision.RecommendedAnswer()
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
	ctx context.Context, is *issueState, st flow.Stage, attempt, of int,
) (runErr error) {
	// "none" stages share one per-issue dir so artifacts flow between stages
	// (brainstorm.md -> spec stage, etc.); worktree/readonly stages share the
	// acquired workspace for the same reason.
	workdir := e.stageWorkdir(is, st)
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
	if st.Name == "merge-verification" {
		finalizationContract = marshal.FinalizationContractMarkdown()
	}
	if err := contextpack.WriteStageBrief(workdir, contextpack.Brief{
		IssueID: is.id, Stage: st.Name, StartCommit: startCommit,
		BaseCommit: is.baseRef, Branch: branch, RequiredInputs: requiredInputs,
		ExpectedOutputs: expectedOutputs, ProhibitedActions: prohibited,
		VerificationOwner: "merge-verification", FinalizationContract: finalizationContract,
		Recovery: recovery,
	}); err != nil {
		return err
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
	}()
	e.emit(core.EvStageStarted, is.id, map[string]any{
		"stage": st.Name, "attempt": attempt, "of": of})

	type agentDone struct {
		pkg string
		res runner.Result
	}
	dones := make(chan agentDone, len(st.Agents))
	runAgent := func(a flow.AgentRef) {
		runID, insErr := e.cfg.Store.InsertStageRun(store.StageRun{
			IssueID: is.id, Stage: st.Name, Agent: a.Package,
			Worktree: workdir, Status: "running"})
		asks := make(chan runner.Ask)
		resc := e.cfg.Runner.Run(ctx, is.id, st.Name, a.Package, workdir, asks)
		for {
			select {
			case ask := <-asks:
				e.handleAsk(is, st.Name, ask)
			case res := <-resc:
				if insErr == nil {
					status := "succeeded"
					if res.Err != nil {
						status = "failed"
					}
					e.cfg.Store.FinishStageRun(runID, status, res.SessionID, res.Tokens)
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
				firstErr = fmt.Errorf("agent %s: %w", d.pkg, d.res.Err)
			}
			continue
		}
		succeeded++
		discovered = append(discovered, d.res.DependsOn...)
		if st.Completion == flow.CompletionAny {
			break
		}
	}
	if st.Completion == flow.CompletionAll && firstErr != nil {
		return firstErr
	}
	if st.Completion == flow.CompletionAny && succeeded == 0 {
		return firstErr
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
	if normalized := deps.Normalize(discovered); len(normalized) > 0 {
		e.mu.Lock()
		all := append(append([]string(nil), is.dependsOn...), normalized...)
		e.mu.Unlock()
		if err := e.SetDependencies(is.id, deps.Normalize(all)); err != nil {
			return fmt.Errorf("discovered dependencies: %w", err)
		}
		return errDependenciesDiscovered
	}
	if st.Name == "merge-verification" {
		prepared, err := e.prepareFinalization(is)
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

func (e *Engine) runStage(ctx context.Context, is *issueState, st flow.Stage) error {
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
		err = e.runStageOnce(stageCtx, is, st, attempt+1, of)
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
		e.emit(core.EvStageFailed, is.id, map[string]any{
			"stage": st.Name, "error": err.Error(),
			"attempt": attempt + 1, "of": of, "final": attempt == st.Retries})
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

	if st.Gate == flow.GateApproveArtifact {
		d := levers.Decision{
			Question:    fmt.Sprintf("Approve %s artifacts?", st.Name),
			Options:     []string{"approve", "reject"},
			Recommended: 0,
			Importance:  1.0,
		}
		if legacyAnswer(e.escalate(is.id, st.Name, d)) != 0 {
			if e.wasKilled(is) {
				e.emit(core.EvStageKilled, is.id, map[string]any{"stage": st.Name})
				return context.Canceled
			}
			err := fmt.Errorf("stage %s artifacts rejected", st.Name)
			e.emit(core.EvStageFailed, is.id, map[string]any{
				"stage": st.Name, "error": err.Error(), "attempt": of, "of": of, "final": true})
			return err
		}
	}
	// A completed "plan" stage may leave a touchset.json declaring which files
	// the implementation will touch; register it so the marshal can sequence
	// overlapping merges. Absence of the file just means no sequencing.
	if e.cfg.Marshal != nil && st.Name == "plan" {
		if ts, err := touchset.Load(filepath.Join(e.stageWorkdir(is, st), "touchset.json")); err == nil {
			e.mu.Lock()
			snapshot := ts
			is.activeTouchset = &snapshot
			e.mu.Unlock()
			e.cfg.Marshal.PlanApproved(is.id, ts)
		}
	}
	verificationReady := false
	if st.Name == "merge-verification" {
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

func (e *Engine) runFrom(ctx context.Context, is *issueState, startIdx int) error {
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
		is.wsRelease = nil
		is.wsPath = ""
		is.branch = ""
		is.baseRef = ""
		is.running = false
		e.mu.Unlock()
		if release != nil && !landed && !preserveWorkspace {
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
		e.mu.Unlock()
		if gate != nil {
			e.emit(core.EvIssuePaused, is.id, map[string]string{"stage": st.Name})
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
			e.mu.Lock()
			is.wsPath, is.wsRelease = path, release
			is.branch, is.baseRef = branch, baseRef
			e.mu.Unlock()
		}
		if err := e.checkBudget(is, st.Name); err != nil {
			return err
		}
		if err := e.runStage(ctx, is, st); errors.Is(err, errDependenciesDiscovered) {
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
	if len(f.Stages) > 0 && f.Stages[len(f.Stages)-1].Name == "merge-verification" {
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

func (e *Engine) finalVerificationDecision(
	is *issueState,
) (string, marshal.Verification, error) {
	prepared, err := e.prepareFinalization(is)
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
	err := e.runFrom(ctx, is, startIdx)
	e.mu.Lock()
	is.terminal = err != nil
	e.mu.Unlock()
	return err
}

func (e *Engine) StartIssue(ctx context.Context, id string) error {
	e.mu.Lock()
	is, ok := e.issues[id]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown issue %s", id)
	}
	return e.startOrWait(ctx, is)
}

// RetryStage restarts a terminal issue at the stage that last failed or was
// killed. Earlier successful stages are not repeated.
func (e *Engine) RetryStage(ctx context.Context, issueID string) error {
	e.mu.Lock()
	is, ok := e.issues[issueID]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("unknown issue %s", issueID)
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
	return e.runAndRecord(ctx, is, startIdx)
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
	if err := e.runStage(ctx, is, stage); err != nil {
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
	decoder.DisallowUnknownFields()
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
		Question:    fmt.Sprintf("Issue %s exceeded its token budget (%d/%d). Continue?", is.id, spent, e.cfg.TokenBudget),
		Options:     []string{"continue", "abort"},
		Recommended: 1,
		Importance:  1.0,
	}
	if legacyAnswer(e.escalate(is.id, stage, d)) == 0 {
		is.budgetWaived = true
		return nil
	}
	err = fmt.Errorf("issue %s aborted: token budget exceeded", is.id)
	e.emit(core.EvStageFailed, is.id, map[string]string{"stage": stage, "error": err.Error()})
	return err
}
