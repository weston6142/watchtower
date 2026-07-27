package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/wbushyeager/guildhall/internal/core"
	"github.com/wbushyeager/guildhall/internal/flow"
	"github.com/wbushyeager/guildhall/internal/levers"
	"github.com/wbushyeager/guildhall/internal/runner"
	"github.com/wbushyeager/guildhall/internal/slots"
	"github.com/wbushyeager/guildhall/internal/store"
	"github.com/wbushyeager/guildhall/internal/workspace"
)

type Config struct {
	Store       *store.Store
	Runner      runner.Runner
	Pool        *slots.Pool
	Flows       map[string]flow.Flow
	Rules       levers.Rules
	DataDir     string
	Workspace   workspace.Provider
	TokenBudget int
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
	id           string
	title        string
	body         string
	flowName     string
	matrix       levers.Matrix
	priority     int
	wsPath       string
	wsRelease    func() error
	budgetWaived bool
}

type Engine struct {
	cfg     Config
	mu      sync.Mutex
	nextID  int
	nextDec int64
	issues  map[string]*issueState
	pend    map[int64]*pending
}

func New(cfg Config) *Engine {
	return &Engine{cfg: cfg, issues: map[string]*issueState{}, pend: map[int64]*pending{}}
}

// emit appends an event best-effort: marshal or store failures are dropped
// rather than aborting the stage, since events are observability, not state.
func (e *Engine) emit(t core.EventType, issueID string, payload any) {
	ev, err := core.NewEvent(t, issueID, payload)
	if err != nil {
		return
	}
	_, _ = e.cfg.Store.Append(ev)
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
	e.emit(core.EvIssueCreated, id, map[string]string{"title": title, "flow": flowName})
	return id, nil
}

func (e *Engine) PendingDecisions() []PendingDecision {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []PendingDecision
	for _, p := range e.pend {
		out = append(out, p.PendingDecision)
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
	return nil
}

// escalate blocks until the human answers; returns chosen option.
func (e *Engine) escalate(issueID, stage string, d levers.Decision) int {
	e.mu.Lock()
	e.nextDec++
	p := &pending{
		PendingDecision: PendingDecision{ID: e.nextDec, IssueID: issueID, Stage: stage, D: d},
		reply:           make(chan int, 1),
	}
	e.pend[p.ID] = p
	e.mu.Unlock()
	e.emit(core.EvDecisionRequired, issueID, map[string]any{
		"decision_id": p.ID, "stage": stage, "question": d.Question,
		"options": d.Options, "recommended": d.Recommended})
	return <-p.reply
}

func (e *Engine) handleAsk(is *issueState, stage string, a runner.Ask) {
	lever := is.matrix[stage]
	if levers.Route(a.Decision, lever, e.cfg.Rules) {
		a.Reply <- e.escalate(is.id, stage, a.Decision)
		return
	}
	e.emit(core.EvDecisionAutoResolved, is.id, map[string]any{
		"stage": stage, "question": a.Decision.Question, "option": a.Decision.Recommended})
	a.Reply <- a.Decision.Recommended
}

func (e *Engine) runStageOnce(ctx context.Context, is *issueState, st flow.Stage) error {
	// "none" stages share one per-issue dir so artifacts flow between stages
	// (brainstorm.md -> spec stage, etc.); worktree/readonly stages share the
	// acquired workspace for the same reason.
	workdir := filepath.Join(e.cfg.DataDir, is.id)
	if st.Workspace != "none" && is.wsPath != "" {
		workdir = is.wsPath
	}
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return err
	}
	// Materialize the issue for the agents: ISSUE.md is the contract for how
	// a stage learns what it is working on.
	issueMD := fmt.Sprintf("# %s: %s\n\n%s\n", is.id, is.title, is.body)
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

func (e *Engine) runStage(ctx context.Context, is *issueState, st flow.Stage) error {
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
	defer func() {
		if is.wsRelease != nil {
			is.wsRelease()
			is.wsRelease = nil
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
		}
		if err := e.checkBudget(is, st.Name); err != nil {
			return err
		}
		if err := e.runStage(ctx, is, st); err != nil {
			return err
		}
	}
	e.emit(core.EvIssueCompleted, id, nil)
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
