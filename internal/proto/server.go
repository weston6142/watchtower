package proto

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/weston6142/watchtower/internal/agentprotocol"
	"github.com/weston6142/watchtower/internal/archmap"
	"github.com/weston6142/watchtower/internal/claude"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/engine"
	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/plannerbudget"
	"github.com/weston6142/watchtower/internal/review"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/scaffold"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/transcript"
)

type Server struct {
	eng            *engine.Engine
	st             *store.Store
	flows          map[string]flow.Flow
	packages       map[string]pkgs.Package
	transcript     *transcript.Buffer
	pricePerMTok   float64
	budget         int
	repoSetup      RepoSetup
	listenerMu     sync.Mutex
	listener       net.Listener
	plannerBudget  plannerbudget.Profile
	repoRoot       string
	loadedSnapshot *scaffold.InputSnapshot
}

const tailPageEventLimit = 256

func NewServer(e *engine.Engine, s *store.Store) *Server {
	return &Server{eng: e, st: s, plannerBudget: plannerbudget.DefaultProfile()}
}

// SetFlows lets the daemon share loaded flows for preset expansion.
func (sv *Server) SetFlows(f map[string]flow.Flow) { sv.flows = f }

// SetPackages lets the daemon share loaded agent packages so issue_detail can
// report which model/effort a stage runs with.
func (sv *Server) SetPackages(p map[string]pkgs.Package) { sv.packages = p }

func (sv *Server) SetTranscript(b *transcript.Buffer) { sv.transcript = b }

func (sv *Server) SetPricePerMTok(price float64) { sv.pricePerMTok = price }

func (sv *Server) SetBudget(budget int) { sv.budget = budget }

func (sv *Server) SetPlannerBudget(profile plannerbudget.Profile) { sv.plannerBudget = profile }

func (sv *Server) resolvePlannerOverride(override *plannerbudget.Override) error {
	_, err := plannerbudget.Resolve(sv.plannerBudget, override)
	return err
}

// SetRepoSetup shares the resolved repo config so the setup inspector can
// report what the daemon is running rather than what config.yaml says. One
// setter rather than five: these values only ever travel together.
func (sv *Server) SetRepoSetup(r RepoSetup) { sv.repoSetup = r }

// SetConfigurationInputs records the repository and the exact managed-input
// snapshot used to start the daemon. Health rendering compares disk against
// this boundary without making presentation code read repository files.
func (sv *Server) SetConfigurationInputs(repoRoot string, snapshot scaffold.InputSnapshot) {
	sv.repoRoot = repoRoot
	copySnapshot := snapshot
	copySnapshot.Files = append([]scaffold.SnapshotFile(nil), snapshot.Files...)
	sv.loadedSnapshot = &copySnapshot
}

func (sv *Server) configurationHealth() *scaffold.ConfigurationHealth {
	if sv.repoRoot == "" {
		return nil
	}
	health, _ := scaffold.Inspect(sv.repoRoot, sv.loadedSnapshot)
	return &health
}

func (sv *Server) Serve(l net.Listener) error {
	sv.listenerMu.Lock()
	sv.listener = l
	sv.listenerMu.Unlock()
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go sv.handle(conn)
	}
}

func (sv *Server) handle(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, maxMessageBytes), maxMessageBytes)
	enc := json.NewEncoder(conn)
	for sc.Scan() {
		var cmd Command
		if err := json.Unmarshal(sc.Bytes(), &cmd); err != nil {
			enc.Encode(Response{Error: err.Error()})
			continue
		}
		response := sv.exec(cmd)
		if err := enc.Encode(response); err != nil {
			return
		}
		if cmd.Op == "shutdown" && response.OK {
			sv.closeListener()
			return
		}
	}
}

func (sv *Server) closeListener() {
	sv.listenerMu.Lock()
	defer sv.listenerMu.Unlock()
	if sv.listener != nil {
		_ = sv.listener.Close()
	}
}

func (sv *Server) exec(cmd Command) Response {
	switch cmd.Op {
	case "can_reset":
		if sv.eng == nil {
			return Response{Error: "engine unavailable"}
		}
		if err := sv.eng.CanReset(); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true}
	case "shutdown":
		if sv.eng == nil {
			return Response{Error: "engine unavailable"}
		}
		if err := sv.eng.CanReset(); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true}
	case "get_flow":
		name := cmd.Flow
		if name == "" {
			name = "default"
		}
		f, ok := sv.flowFor(name)
		if !ok {
			return Response{Error: "unknown flow " + name}
		}
		stages := make([]string, len(f.Stages))
		for i, stage := range f.Stages {
			stages[i] = stage.Name
		}
		return Response{OK: true, FlowStages: stages}
	case "arch_map":
		result := &archmap.Map{}
		if cmd.Repo != "" {
			modules, err := archmap.Scan(cmd.Repo)
			if err != nil {
				return Response{Error: err.Error()}
			}
			result.Modules = modules
		}
		if sv.eng != nil {
			active := sv.eng.ActiveTouchsets()
			ids := make([]string, 0, len(active))
			for id := range active {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				result.Overlays = append(result.Overlays, archmap.Overlay{IssueID: id, Globs: active[id]})
			}
		}
		return Response{OK: true, Arch: result}
	case "create_issue":
		fl, lever, ok := sv.flowAndPreset(cmd)
		if !ok {
			return Response{Error: "unknown flow " + cmd.Flow}
		}
		id, err := sv.eng.CreateIssueWithDependencies(
			cmd.Title, cmd.Body, cmd.Flow, levers.Preset(fl, lever), cmd.Priority, cmd.Attach, cmd.DependsOn)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: id}
	case "draft_issue":
		fl, lever, ok := sv.flowAndPreset(cmd)
		if !ok {
			return Response{Error: "unknown flow " + cmd.Flow}
		}
		id, err := sv.eng.DraftIssueWithDependencies(
			cmd.Title, cmd.Body, cmd.Flow, string(lever), levers.Preset(fl, lever),
			cmd.Priority, cmd.Attach, cmd.DependsOn)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: id}
	case "update_issue":
		fl, lever, ok := sv.flowAndPreset(cmd)
		if !ok {
			return Response{Error: "unknown flow " + cmd.Flow}
		}
		var err error
		if cmd.DependsOn == nil {
			err = sv.eng.UpdateIssue(
				cmd.IssueID, cmd.Title, cmd.Body, cmd.Flow, string(lever),
				levers.Preset(fl, lever), cmd.Priority, cmd.Attach)
		} else {
			err = sv.eng.UpdateIssueWithDependencies(
				cmd.IssueID, cmd.Title, cmd.Body, cmd.Flow, string(lever),
				levers.Preset(fl, lever), cmd.Priority, cmd.Attach, cmd.DependsOn)
		}
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: cmd.IssueID}
	case "launch_issue":
		if err := sv.resolvePlannerOverride(cmd.PlannerBudget); err != nil {
			return Response{Error: err.Error()}
		}
		if err := sv.eng.LaunchIssueWithBudget(cmd.IssueID, cmd.PlannerBudget); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: cmd.IssueID}
	case "requeue_issue":
		if err := sv.eng.RequeueIssue(cmd.IssueID); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: cmd.IssueID}
	case "claim_issue":
		claim, err := sv.eng.ClaimIssue(cmd.IssueID)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: cmd.IssueID, Claim: &claim}
	case "release_claim":
		if err := sv.eng.ReleaseClaim(cmd.IssueID); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: cmd.IssueID}
	case "claim_for_worktree":
		claim, err := sv.eng.ClaimForWorktree(cmd.Worktree)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: claim.IssueID, Claim: &claim}
	case "finish_claim":
		if err := sv.eng.FinishClaim(engine.FinishClaimRequest{
			IssueID: cmd.IssueID, Worktree: cmd.Worktree, AllowNoChange: cmd.AllowNoChange,
		}); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: cmd.IssueID}
	case "start_issue":
		if err := sv.resolvePlannerOverride(cmd.PlannerBudget); err != nil {
			return Response{Error: err.Error()}
		}
		// Runs asynchronously; failures surface as stage_failed events
		// in the log rather than in this response.
		go sv.eng.StartIssueWithBudget(context.Background(), cmd.IssueID, cmd.PlannerBudget)
		return Response{OK: true, IssueID: cmd.IssueID}
	case "pause_issue":
		if err := sv.eng.Pause(cmd.IssueID); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: cmd.IssueID}
	case "resume_issue":
		if err := sv.eng.Resume(cmd.IssueID); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: cmd.IssueID}
	case "kill_stage":
		if err := sv.eng.KillStage(cmd.IssueID); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: cmd.IssueID}
	case "retry_stage":
		if err := sv.resolvePlannerOverride(cmd.PlannerBudget); err != nil {
			return Response{Error: err.Error()}
		}
		go sv.eng.RetryStageWithBudget(context.Background(), cmd.IssueID, cmd.PlannerBudget)
		return Response{OK: true, IssueID: cmd.IssueID}
	case "abandon_issue":
		if err := sv.eng.Abandon(cmd.IssueID); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: cmd.IssueID}
	case "set_lever":
		if err := sv.eng.SetLever(cmd.IssueID, cmd.Stage, flow.Lever(cmd.Lever)); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: cmd.IssueID}
	case "list_decisions":
		return Response{OK: true, Decisions: sv.eng.PendingDecisions()}
	case "answer_decision":
		if cmd.Option != nil && cmd.Text != "" {
			return Response{Error: "answer_decision accepts either option or text, not both"}
		}
		var answer levers.Response
		switch {
		case cmd.Option != nil:
			answer = levers.ChoiceResponse(*cmd.Option)
		case cmd.Text != "":
			answer = levers.FreeformResponse(cmd.Text)
		default:
			return Response{Error: "answer_decision requires an option or text"}
		}
		actor := review.NormalizeActor(cmd.Actor)
		if err := sv.eng.AnswerAs(cmd.DecisionID, answer, actor); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true}
	case "list_proposals":
		ps, err := sv.st.PendingProposals()
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, Proposals: ps}
	case "list_issues":
		issues, err := sv.st.Issues()
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, Issues: issues}
	case "list_backlog":
		items, claims, err := sv.backlogItems()
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, Backlog: items, Claims: claims}
	case "issue_detail":
		issues, err := sv.st.Issues()
		if err != nil {
			return Response{Error: err.Error()}
		}
		var issue store.IssueRow
		found := false
		for _, candidate := range issues {
			if candidate.ID == cmd.IssueID {
				issue = candidate
				found = true
				break
			}
		}
		if !found {
			return Response{Error: "unknown issue " + cmd.IssueID}
		}
		history := FailureHistory{Status: "available", Records: make([]failure.FailureRecord, 0)}
		if records, historyErr := sv.st.FailureHistory(context.Background(), cmd.IssueID); historyErr != nil {
			history.Status = "unavailable"
			history.Records = nil
		} else {
			history.Records = records
		}
		runs, err := sv.st.StageRuns(cmd.IssueID)
		if err != nil {
			return Response{Error: err.Error()}
		}
		attempts := make([]runner.Attempt, 0)
		for _, run := range runs {
			loaded, err := sv.st.LoadOperation(context.Background(), strconv.FormatInt(run.ID, 10))
			if err != nil {
				return Response{Error: err.Error()}
			}
			attempts = append(attempts, loaded...)
		}
		tokens, err := sv.st.IssueTokens(cmd.IssueID)
		if err != nil {
			return Response{Error: err.Error()}
		}
		artifacts, err := sv.st.ArtifactPaths(cmd.IssueID)
		if err != nil {
			return Response{Error: err.Error()}
		}
		plannerSnapshot, plannerOutcome, err := sv.st.LatestPlannerSnapshot(cmd.IssueID)
		if err != nil {
			return Response{Error: err.Error()}
		}
		stage, attempt, attemptOf, lastError, err := sv.st.LastStageEvents(cmd.IssueID)
		if err != nil {
			return Response{Error: err.Error()}
		}
		integration, _, err := sv.st.IssueIntegration(cmd.IssueID)
		if err != nil {
			return Response{Error: err.Error()}
		}
		decisionPage := ""
		if sv.eng != nil {
			candidate := sv.eng.DecisionPagePath(cmd.IssueID)
			if _, statErr := os.Stat(candidate); statErr == nil {
				decisionPage = candidate
			}
		}
		model, effort := "", ""
		if f, ok := sv.flows[issue.Flow]; ok {
			for _, stg := range f.Stages {
				if stg.Name != stage || len(stg.Agents) == 0 {
					continue
				}
				// Agents[0] only: this op reports one model for the stage, and
				// the setup inspector is where every agent is listed.
				if p, _, found := sv.effectiveAgent(stg.Agents[0]); found {
					model, effort = p.Model, p.Effort
				}
				break
			}
		}
		return Response{OK: true, Detail: &IssueDetail{
			Issue: issue, Runs: runs, Attempts: attempts, Tokens: tokens, Artifacts: artifacts,
			Model: model, Effort: effort,
			LastError: lastError, Attempt: attempt, AttemptOf: attemptOf, Budget: sv.budget, Levers: issue.Levers,
			Cleanup: integration.Cleanup, Dollars: float64(tokens) / 1_000_000 * sv.pricePerMTok,
			IntegrationState: integration.State,
			Worktree:         integration.Worktree, Branch: integration.Branch,
			Planner: plannerSnapshot, PlannerOutcome: plannerOutcome,
			DecisionPage:   decisionPage,
			FailureHistory: history,
		}}
	case "resolve_proposal":
		issueID, err := sv.eng.ResolveProposal(cmd.ProposalID, cmd.Accept, cmd.Flow, cmd.Preset)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: issueID}
	case "tail":
		throughSeq := cmd.ThroughSeq
		if throughSeq == 0 {
			throughSeq = sv.st.LatestEventSeq()
		}
		evs, err := sv.st.EventsBetweenLimit(cmd.SinceSeq, throughSeq, tailPageEventLimit)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return boundedTailResponse(evs, throughSeq)
	case "transcript_tail":
		n := cmd.N
		if n <= 0 {
			n = 50
		}
		if sv.transcript == nil {
			return Response{OK: true, Lines: []string{}}
		}
		return Response{OK: true, Lines: sv.transcript.Tail(cmd.IssueID, n)}
	case "setup_outline":
		view, err := sv.setupView(cmd)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, Setup: view}
	case "setup_prompt":
		lines, err := sv.setupPrompt(cmd)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, Lines: lines}
	case "overview":
		overview, err := sv.overview()
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, Overview: &overview}
	default:
		return Response{Error: "unknown op " + cmd.Op}
	}
}

func boundedTailResponse(events []core.Event, throughSeq int64) Response {
	response := Response{OK: true, ThroughSeq: throughSeq}
	for _, event := range events {
		response.Events = append(response.Events, event)
		encoded, err := json.Marshal(response)
		if err != nil {
			return Response{Error: err.Error()}
		}
		// Scanner's token excludes the newline, but keeping one byte of room
		// avoids depending on its exact maximum-token boundary behavior.
		if len(encoded) < maxMessageBytes {
			continue
		}
		response.Events = response.Events[:len(response.Events)-1]
		if len(response.Events) == 0 {
			return Response{Error: fmt.Sprintf("event sequence %d exceeds the protocol frame limit", event.Seq)}
		}
		return response
	}
	return response
}

func (sv *Server) overview() (Overview, error) {
	issues, err := sv.st.Issues()
	if err != nil {
		return Overview{}, err
	}
	needYou := 0
	if sv.eng != nil {
		needYou = len(sv.eng.PendingDecisions())
	} else {
		pending, err := sv.st.PendingDecisionRows()
		if err != nil {
			return Overview{}, err
		}
		needYou = len(pending)
	}
	allEvents, err := sv.st.EventsSince(0)
	if err != nil {
		return Overview{}, err
	}
	latest := map[string]core.Event{}
	for _, ev := range allEvents {
		latest[ev.IssueID] = ev
	}
	var out Overview
	out.ConfigurationHealth = sv.configurationHealth()
	out.NeedYou = needYou
	for _, issue := range issues {
		state := issue.State
		if state == "done" || state == "done (unmerged)" || state == "merged" || state == "abandoned" {
			continue
		}
		if state == "paused" {
			continue
		}
		if ev, ok := latest[issue.ID]; ok {
			switch ev.Type {
			case core.EvStageFailed, core.EvFinalizationFailed:
				out.Failing++
				continue
			case core.EvVerificationReady:
				out.Building++
				continue
			case core.EvMergeStarted:
				out.Building++
				continue
			case core.EvDecisionRequired, core.EvIssuePaused, core.EvStageKilled,
				core.EvIssueCompleted, core.EvIssueMerged, core.EvIssueAbandoned:
				continue
			}
		}
		switch {
		case strings.HasPrefix(state, "failed"):
			out.Failing++
		case strings.HasPrefix(state, "queued"):
			out.Queued++
		case strings.HasPrefix(state, "running"), state == "verifying",
			state == "waiting:integration", state == "integrating":
			out.Building++
		}
	}
	now := time.Now()
	midnight := core.StartOfDay(now)
	today, err := sv.st.EventsSinceTime(midnight)
	if err != nil {
		return Overview{}, err
	}
	for _, ev := range today {
		if ev.Type == core.EvIssueMerged {
			out.ShippedToday++
		}
	}
	out.TokensTotal, err = sv.st.TotalTokens()
	if err != nil {
		return Overview{}, err
	}
	if sv.pricePerMTok > 0 {
		out.DollarsTotal = float64(out.TokensTotal) / 1_000_000 * sv.pricePerMTok
	}
	return out, nil
}

func (sv *Server) backlogItems() ([]BacklogItem, []engine.Claim, error) {
	issues, err := sv.st.Issues()
	if err != nil {
		return nil, nil, err
	}
	items := make([]BacklogItem, 0)
	for _, issue := range issues {
		if issue.State != "backlog" {
			continue
		}
		blockedBy, err := sv.eng.ClaimBlockers(issue.ID)
		if err != nil {
			return nil, nil, err
		}
		attachments, err := sv.st.Attachments(issue.ID)
		if err != nil {
			return nil, nil, err
		}
		summaries := make([]AttachmentSummary, 0, len(attachments))
		for _, attachment := range attachments {
			summaries = append(summaries, AttachmentSummary{
				Name: attachment.Name, Size: attachment.Size,
			})
		}
		items = append(items, BacklogItem{
			Issue: issue, Attachments: summaries,
			Claimable: len(blockedBy) == 0, BlockedBy: blockedBy,
		})
	}
	claims, err := sv.eng.Claims()
	if err != nil {
		return nil, nil, err
	}
	return items, claims, nil
}

// flowAndPreset resolves the command's flow and clamps its preset to a
// known lever, falling back to regular for anything unrecognized.
func (sv *Server) flowAndPreset(cmd Command) (flow.Flow, flow.Lever, bool) {
	fl, ok := sv.flowFor(cmd.Flow)
	if !ok {
		return flow.Flow{}, "", false
	}
	lever := flow.Lever(cmd.Preset)
	if lever != flow.LeverYolo && lever != flow.LeverRegular && lever != flow.LeverStrict {
		lever = flow.LeverRegular
	}
	return fl, lever, true
}

func (sv *Server) flowFor(name string) (flow.Flow, bool) {
	if sv.flows != nil {
		f, ok := sv.flows[name]
		return f, ok
	}
	return flow.Flow{}, false
}

// effectiveAgent reports what the CLI actually receives for one agent ref.
// internal/claude/runner.go passes pkg.Model alone, so the package is the
// truth; ref.Model is returned separately as declared-but-unapplied. Shared by
// issue_detail and setup_outline so two surfaces in one TUI cannot print
// different models for the same stage.
func (sv *Server) effectiveAgent(ref flow.AgentRef) (pkg pkgs.Package, declaredModel string, ok bool) {
	pkg, ok = sv.packages[ref.Package]
	if !ok {
		return pkg, "", false
	}
	if ref.Model != "" && ref.Model != pkg.Model {
		declaredModel = ref.Model
	}
	if sv.repoSetup.Runner == "codex" {
		if pkg.Model == "" {
			pkg.Model = sv.repoSetup.CodexModel
		}
		if pkg.Effort == "" {
			pkg.Effort = sv.repoSetup.CodexEffort
		}
	}
	return pkg, declaredModel, ok
}

// promptPreviewLines and promptPreviewRunes bound the per-agent preview that
// rides in the outline response, so its size does not grow with package count.
const (
	promptPreviewLines = 3
	promptPreviewRunes = 120
)

// setupView assembles the read-only picture of what the daemon is running for
// one flow, optionally scoped to an issue so its per-stage levers show. It
// reads the cached flows and packages — never the files on disk.
func (sv *Server) setupView(cmd Command) (*SetupView, error) {
	if sv.flows == nil {
		return nil, errors.New("no flows loaded")
	}
	var issue store.IssueRow
	scoped := false
	if cmd.IssueID != "" {
		issues, err := sv.st.Issues()
		if err != nil {
			return nil, err
		}
		for _, candidate := range issues {
			if candidate.ID == cmd.IssueID {
				issue, scoped = candidate, true
				break
			}
		}
		if !scoped {
			return nil, errors.New("unknown issue " + cmd.IssueID)
		}
	}
	name := cmd.Flow
	if name == "" && scoped {
		name = issue.Flow
	}
	if name == "" {
		name = "default"
	}
	f, ok := sv.flows[name]
	if !ok {
		// An explicit flow name that does not resolve is an error. An issue's
		// own stale flow name falls back to default so the panel still opens —
		// and SetupView.Flow names the flow actually shown, so the fallback is
		// visible rather than silent.
		if cmd.Flow != "" {
			return nil, errors.New("unknown flow " + name)
		}
		if f, ok = sv.flows["default"]; !ok {
			return nil, errors.New("unknown flow " + name)
		}
		name = "default"
	}
	view := &SetupView{Flow: name, Repo: sv.repoSetup, ConfigurationHealth: sv.configurationHealth()}
	if scoped {
		view.IssueID, view.IssueTitle = issue.ID, issue.Title
	}
	for _, stg := range f.Stages {
		ss := StageSetup{
			Name: stg.Name, Gate: string(stg.Gate), Workspace: stg.Workspace,
			Parallel: stg.Parallel, Completion: stg.Completion,
			HeavySlot: stg.HeavySlot, MergeBarrier: stg.MergeBarrier,
			Retries: stg.Retries, Artifacts: append([]string(nil), stg.Artifacts...),
		}
		if scoped {
			ss.Lever = issue.Levers[stg.Name]
		}
		for _, ref := range stg.Agents {
			ss.Agents = append(ss.Agents, sv.agentSetup(ref))
		}
		view.Stages = append(view.Stages, ss)
	}
	return view, nil
}

// agentSetup reports one agent's effective CLI settings, the fields watchtower
// parses but never passes, and a short prompt preview.
func (sv *Server) agentSetup(ref flow.AgentRef) AgentSetup {
	out := AgentSetup{Package: ref.Package}
	pkg, declaredModel, ok := sv.effectiveAgent(ref)
	out.DeclaredModel = declaredModel
	if !ok {
		out.Missing = true
		return out
	}
	out.Model, out.Effort = pkg.Model, pkg.Effort
	if sv.repoSetup.Runner == "codex" {
		out.DeclaredAllowedTools = append([]string(nil), pkg.AllowedTools...)
		out.ToolSource = "codex config"
	} else {
		out.ThinkingTokens = claude.ThinkingTokens(pkg.Effort)
		out.AllowedTools = append([]string(nil), pkg.AllowedTools...)
	}
	out.MaxTurns = pkg.MaxTurns
	body := strings.TrimSuffix(pkg.Prompt, "\n")
	if body == "" {
		return out
	}
	lines := strings.Split(body, "\n")
	out.PromptLines = len(lines)
	for _, line := range lines {
		if len(out.PromptPreview) == promptPreviewLines {
			break
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		out.PromptPreview = append(out.PromptPreview, truncateRunes(line, promptPreviewRunes))
	}
	return out
}

// truncateRunes clips s to at most n runes. The preview is a hint on the wire,
// not a rendering, so a plain rune cut is enough — no lipgloss in the daemon.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// maxPromptBytes bounds a prompt body on the wire. proto's frame limit is
// 1 MiB (maxMessageBytes, symmetric with client.go's scanner buffer), so a
// clipped body plus its headings always fits with room to spare.
const maxPromptBytes = 256 << 10

const (
	promptTaskHeading         = "first user message · internal/claude/runner.go"
	promptSystemHeadingPrefix = "--append-system-prompt · .watchtower/packages/"
	promptBaseNote            = "Claude Code's own base system prompt is added by the CLI and is not shown here."
	// unscopedIssueID stands in for the issue id when nothing is focused, so
	// the task line reads as a template rather than naming a lane at random.
	unscopedIssueID = "«issue-id»"
)

// setupPrompt assembles the full prompt one agent receives: the synthesized
// first user message, then the package's system prompt. The stage/package pair
// is validated against the flow, so the op cannot be used to dump arbitrary
// package bodies by guessing a name.
func (sv *Server) setupPrompt(cmd Command) ([]string, error) {
	view, err := sv.setupView(cmd)
	if err != nil {
		return nil, err
	}
	paired := false
	for _, stg := range view.Stages {
		if stg.Name != cmd.Stage {
			continue
		}
		for _, ag := range stg.Agents {
			if ag.Package == cmd.Package {
				paired = true
				break
			}
		}
		break
	}
	if !paired {
		return nil, fmt.Errorf("stage %s has no agent %s", cmd.Stage, cmd.Package)
	}
	pkg, ok := sv.packages[cmd.Package]
	if !ok {
		return nil, fmt.Errorf("package %s not loaded", cmd.Package)
	}
	issueID := cmd.IssueID
	if issueID == "" {
		issueID = unscopedIssueID
	}
	lines := []string{
		promptTaskHeading, "",
		agentprotocol.TaskMessage(cmd.Stage, issueID), "",
		promptSystemHeadingPrefix + cmd.Package + "/prompt.md", "",
	}
	lines = append(lines, promptBody(pkg.Prompt)...)
	return append(lines, "", promptBaseNote), nil
}

// promptBody splits a package prompt into wire lines, clipped on a line
// boundary when it would blow past maxPromptBytes. Clipping mid-line would
// misrepresent the prompt; a named truncation line does not.
func promptBody(prompt string) []string {
	body := strings.Split(strings.TrimSuffix(prompt, "\n"), "\n")
	total := 0
	for i, line := range body {
		total += len(line) + 1
		if total > maxPromptBytes {
			out := append([]string(nil), body[:i]...)
			return append(out, fmt.Sprintf("— truncated at 256 KiB (prompt is %d bytes) —", len(prompt)))
		}
	}
	return body
}
