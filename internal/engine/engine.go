package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/wbushyeager/guildhall/internal/core"
	"github.com/wbushyeager/guildhall/internal/evidence"
	"github.com/wbushyeager/guildhall/internal/flow"
	"github.com/wbushyeager/guildhall/internal/levers"
	"github.com/wbushyeager/guildhall/internal/librarian"
	"github.com/wbushyeager/guildhall/internal/marshal"
	"github.com/wbushyeager/guildhall/internal/runner"
	"github.com/wbushyeager/guildhall/internal/slots"
	"github.com/wbushyeager/guildhall/internal/store"
	"github.com/wbushyeager/guildhall/internal/touchset"
	"github.com/wbushyeager/guildhall/internal/workspace"
)

type Config struct {
	Store       *store.Store
	Runner      runner.Runner
	Marshal     Sequencer
	Train       *marshal.Train
	Librarian   *librarian.Librarian
	Reconcile   func(context.Context, string) error
	Observers   []func(core.Event)
	Pool        *slots.Pool
	Flows       map[string]flow.Flow
	Rules       levers.Rules
	DataDir     string
	Workspace   workspace.Provider
	TokenBudget int
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
	reply chan int
}

type issueState struct {
	id             string
	title          string
	body           string
	flowName       string
	matrix         levers.Matrix
	priority       int
	wsPath         string
	branch         string
	baseRef        string
	wsRelease      func() error
	budgetWaived   bool
	activeTouchset *touchset.Set
}

type Engine struct {
	cfg    Config
	mu     sync.Mutex
	nextID int
	issues map[string]*issueState
	pend   map[int64]*pending
}

func New(cfg Config) *Engine {
	return &Engine{cfg: cfg, issues: map[string]*issueState{}, pend: map[int64]*pending{}}
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

func (e *Engine) CreateIssue(title, body, flowName string, m levers.Matrix, priority int) (string, error) {
	if _, ok := e.cfg.Flows[flowName]; !ok {
		return "", fmt.Errorf("unknown flow %q", flowName)
	}
	e.mu.Lock()
	e.nextID++
	id := fmt.Sprintf("GH-%d", e.nextID)
	e.issues[id] = &issueState{id: id, title: title, body: body, flowName: flowName, matrix: m, priority: priority}
	e.mu.Unlock()
	if err := e.cfg.Store.UpsertIssue(store.IssueRow{
		ID: id, Title: title, Body: body, Flow: flowName, State: "running", Priority: priority,
	}); err != nil {
		return "", err
	}
	e.emit(core.EvIssueCreated, id, map[string]any{
		"title": title, "flow": flowName, "body": body, "priority": priority})
	return id, nil
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

func (e *Engine) Answer(decisionID int64, option int) error {
	e.mu.Lock()
	p, ok := e.pend[decisionID]
	if ok {
		delete(e.pend, decisionID)
	}
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("no pending decision %d", decisionID)
	}
	e.emit(core.EvDecisionAnswered, p.IssueID, map[string]any{
		"decision_id": p.ID, "option": option})
	p.reply <- option
	return e.cfg.Store.AnswerDecision(decisionID, option, "answered")
}

// escalate blocks until the human answers; returns chosen option.
func (e *Engine) escalate(issueID, stage string, d levers.Decision) int {
	rowID, err := e.cfg.Store.InsertDecision(store.DecisionRow{
		IssueID: issueID, Stage: stage, Question: d.Question,
		Options: d.Options, Recommended: d.Recommended,
		Why: d.Why, Consequences: d.Consequences, Reversible: d.Reversible,
		BlockingCost: e.blockingCost(issueID),
	})
	if err != nil {
		panic(fmt.Sprintf("insert decision: %v", err))
	}
	p := &pending{
		PendingDecision: PendingDecision{ID: rowID, IssueID: issueID, Stage: stage, D: d},
		reply:           make(chan int, 1),
	}
	e.mu.Lock()
	e.pend[rowID] = p
	e.mu.Unlock()
	e.emit(core.EvDecisionRequired, issueID, map[string]any{
		"decision_id": p.ID, "stage": stage, "question": d.Question,
		"options": d.Options, "recommended": d.Recommended})
	return <-p.reply
}

func (e *Engine) blockingCost(issueID string) int {
	if e.cfg.Marshal == nil {
		return 1
	}
	return 1 + e.cfg.Marshal.BlockedBehind(issueID)
}

func (e *Engine) FileProposal(issueID, title, body string) {
	if _, err := e.cfg.Store.InsertProposal(issueID, title, body); err != nil {
		return
	}
	e.emit(core.EvProposalFiled, issueID, map[string]string{"title": title})
}

func (e *Engine) ResolveProposal(id int64, accept bool, flowName, preset string) (string, error) {
	if !accept {
		if err := e.cfg.Store.SetProposalStatus(id, "rejected"); err != nil {
			return "", err
		}
		e.emit(core.EvProposalRejected, "", map[string]any{"proposal_id": id})
		return "", nil
	}
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
	f, ok := e.cfg.Flows[flowName]
	if !ok {
		return "", fmt.Errorf("unknown flow %q", flowName)
	}
	lever := flow.Lever(preset)
	if lever != flow.LeverYolo && lever != flow.LeverRegular && lever != flow.LeverStrict {
		lever = flow.LeverRegular
	}
	newID, err := e.CreateIssue(row.Title, row.Body, flowName, levers.Preset(f, lever), 0)
	if err != nil {
		return "", err
	}
	if err := e.cfg.Store.SetProposalStatus(id, "accepted"); err != nil {
		return "", err
	}
	e.emit(core.EvProposalAccepted, newID, map[string]any{"proposal_id": id})
	return newID, nil
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
		Why: a.Decision.Why, Consequences: a.Decision.Consequences, Reversible: a.Decision.Reversible,
		Status: "auto", Answer: a.Decision.Recommended,
		BlockingCost: e.blockingCost(is.id),
	}); err != nil {
		panic(fmt.Sprintf("insert auto decision: %v", err))
	}
	e.emit(core.EvDecisionAutoResolved, is.id, map[string]any{
		"stage": stage, "question": a.Decision.Question, "option": a.Decision.Recommended})
	a.Reply <- a.Decision.Recommended
}

func (e *Engine) runStageOnce(ctx context.Context, is *issueState, st flow.Stage) error {
	// "none" stages share one per-issue dir so artifacts flow between stages
	// (brainstorm.md -> spec stage, etc.); worktree/readonly stages share the
	// acquired workspace for the same reason.
	workdir := e.stageWorkdir(is, st)
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return err
	}
	// Materialize the issue for the agents: ISSUE.md is the contract for how
	// a stage learns what it is working on.
	issueMD := fmt.Sprintf("# %s: %s\n\n%s\n", is.id, is.title, is.body)
	if e.cfg.Librarian != nil {
		if mem, err := e.cfg.Librarian.Context(); err == nil && mem != "" {
			issueMD += "\n# Project memory (curated by the Librarian)\n\n" + mem + "\n"
		}
	}
	if err := os.WriteFile(filepath.Join(workdir, "ISSUE.md"), []byte(issueMD), 0o644); err != nil {
		return err
	}
	e.emit(core.EvStageStarted, is.id, map[string]string{"stage": st.Name})

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
	for i := 0; i < need; i++ {
		d := <-dones
		if d.res.Err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("agent %s: %w", d.pkg, d.res.Err)
			}
			continue
		}
		succeeded++
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
		e.emit(core.EvArtifactProduced, is.id, map[string]string{"stage": st.Name, "artifact": name, "path": p})
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
	if st.MergeBarrier && e.cfg.Marshal != nil {
		if err := e.cfg.Marshal.ReadyToMerge(ctx, is.id); err != nil {
			return err
		}
	}
	if st.HeavySlot {
		e.emit(core.EvSlotQueued, is.id, map[string]string{"stage": st.Name})
		release, err := e.cfg.Pool.Acquire(ctx, is.id, is.priority)
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
	for attempt := 0; attempt <= st.Retries; attempt++ {
		err = e.runStageOnce(ctx, is, st)
		if err == nil {
			break
		}
	}
	if err != nil {
		e.emit(core.EvStageFailed, is.id, map[string]string{"stage": st.Name, "error": err.Error()})
		return err
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
		if e.escalate(is.id, st.Name, d) != 0 {
			err := fmt.Errorf("stage %s artifacts rejected", st.Name)
			e.emit(core.EvStageFailed, is.id, map[string]string{"stage": st.Name, "error": err.Error()})
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
	e.emit(core.EvStageCompleted, is.id, map[string]string{"stage": st.Name})
	return nil
}

func (e *Engine) StartIssue(ctx context.Context, id string) error {
	e.mu.Lock()
	is, ok := e.issues[id]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown issue %s", id)
	}
	f := e.cfg.Flows[is.flowName]
	aborted := true
	defer func() {
		e.mu.Lock()
		is.activeTouchset = nil
		e.mu.Unlock()
		if is.wsRelease != nil {
			is.wsRelease()
			is.wsRelease = nil
		}
		if aborted && e.cfg.Marshal != nil {
			e.cfg.Marshal.Aborted(id)
		}
	}()
	for _, st := range f.Stages {
		if st.Workspace != "none" && e.cfg.Workspace != nil && is.wsPath == "" {
			path, release, err := e.cfg.Workspace.Acquire(is.id)
			if err != nil {
				e.emit(core.EvStageFailed, is.id, map[string]string{"stage": st.Name, "error": "workspace: " + err.Error()})
				return err
			}
			is.wsPath, is.wsRelease = path, release
			if out, err := exec.Command("git", "-C", path, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
				is.branch = strings.TrimSpace(string(out))
			}
			if out, err := exec.Command("git", "-C", path, "rev-parse", "HEAD").Output(); err == nil {
				is.baseRef = strings.TrimSpace(string(out))
			}
		}
		if err := e.checkBudget(is, st.Name); err != nil {
			return err
		}
		if err := e.runStage(ctx, is, st); err != nil {
			return err
		}
	}
	if e.cfg.Train != nil && is.branch != "" {
		e.emit(core.EvMergeStarted, id, map[string]string{"branch": is.branch})
		if err := e.landWithEscalation(ctx, is); err != nil {
			e.emit(core.EvIssueCompleted, id, map[string]string{
				"merge": "left-unmerged", "branch": is.branch})
			if e.cfg.Marshal != nil {
				e.cfg.Marshal.Merged(is.id)
			}
			aborted = false
			return nil
		}
		if e.cfg.Reconcile != nil {
			if err := e.cfg.Reconcile(ctx, is.id); err == nil {
				e.emit(core.EvDocsReconciled, is.id, nil)
			}
		}
		e.emit(core.EvIssueMerged, id, map[string]string{"branch": is.branch})
	}
	if e.cfg.Marshal != nil {
		e.cfg.Marshal.Merged(is.id)
	}
	aborted = false
	e.emit(core.EvIssueCompleted, id, nil)
	return nil
}

func (e *Engine) landWithEscalation(ctx context.Context, is *issueState) error {
	err := e.cfg.Train.Land(ctx, is.id, is.branch)
	for err != nil {
		e.emit(core.EvMergeConflict, is.id, map[string]string{"error": err.Error()})
		d := levers.Decision{
			Question:    fmt.Sprintf("Merge of %s failed: %v. Retry, or leave the branch for manual merge?", is.id, err),
			Options:     []string{"retry", "leave branch"},
			Recommended: 1,
			Importance:  1.0,
		}
		if e.escalate(is.id, "merge", d) != 0 {
			return err
		}
		err = e.cfg.Train.Land(ctx, is.id, is.branch)
	}
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
	if e.escalate(is.id, stage, d) == 0 {
		is.budgetWaived = true
		return nil
	}
	err = fmt.Errorf("issue %s aborted: token budget exceeded", is.id)
	e.emit(core.EvStageFailed, is.id, map[string]string{"stage": stage, "error": err.Error()})
	return err
}
