package proto

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/weston6142/watchtower/internal/archmap"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/engine"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/transcript"
)

type Server struct {
	eng          *engine.Engine
	st           *store.Store
	flows        map[string]flow.Flow
	transcript   *transcript.Buffer
	pricePerMTok float64
	budget       int
}

func NewServer(e *engine.Engine, s *store.Store) *Server {
	return &Server{eng: e, st: s}
}

// SetFlows lets the daemon share loaded flows for preset expansion.
func (sv *Server) SetFlows(f map[string]flow.Flow) { sv.flows = f }

func (sv *Server) SetTranscript(b *transcript.Buffer) { sv.transcript = b }

func (sv *Server) SetPricePerMTok(price float64) { sv.pricePerMTok = price }

func (sv *Server) SetBudget(budget int) { sv.budget = budget }

func (sv *Server) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
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
		enc.Encode(sv.exec(cmd))
	}
}

func (sv *Server) exec(cmd Command) Response {
	switch cmd.Op {
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
		fl, ok := sv.flowFor(cmd.Flow)
		if !ok {
			return Response{Error: "unknown flow " + cmd.Flow}
		}
		lever := flow.Lever(cmd.Preset)
		if lever != flow.LeverYolo && lever != flow.LeverRegular && lever != flow.LeverStrict {
			lever = flow.LeverRegular
		}
		id, err := sv.eng.CreateIssue(cmd.Title, cmd.Body, cmd.Flow, levers.Preset(fl, lever), cmd.Priority)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: id}
	case "start_issue":
		// Runs asynchronously; failures surface as stage_failed events
		// in the log rather than in this response.
		go sv.eng.StartIssue(context.Background(), cmd.IssueID)
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
		go sv.eng.RetryStage(context.Background(), cmd.IssueID)
		return Response{OK: true, IssueID: cmd.IssueID}
	case "set_lever":
		if err := sv.eng.SetLever(cmd.IssueID, cmd.Stage, flow.Lever(cmd.Lever)); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: cmd.IssueID}
	case "list_decisions":
		return Response{OK: true, Decisions: sv.eng.PendingDecisions()}
	case "answer_decision":
		if err := sv.eng.Answer(cmd.DecisionID, cmd.Option); err != nil {
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
		runs, err := sv.st.StageRuns(cmd.IssueID)
		if err != nil {
			return Response{Error: err.Error()}
		}
		tokens, err := sv.st.IssueTokens(cmd.IssueID)
		if err != nil {
			return Response{Error: err.Error()}
		}
		artifacts, err := sv.st.ArtifactPaths(cmd.IssueID)
		if err != nil {
			return Response{Error: err.Error()}
		}
		_, attempt, attemptOf, lastError, err := sv.st.LastStageEvents(cmd.IssueID)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, Detail: &IssueDetail{
			Issue: issue, Runs: runs, Tokens: tokens, Artifacts: artifacts,
			LastError: lastError, Attempt: attempt, AttemptOf: attemptOf, Budget: sv.budget, Levers: issue.Levers,
			Dollars: float64(tokens) / 1_000_000 * sv.pricePerMTok,
		}}
	case "resolve_proposal":
		issueID, err := sv.eng.ResolveProposal(cmd.ProposalID, cmd.Accept, cmd.Flow, cmd.Preset)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, IssueID: issueID}
	case "tail":
		evs, err := sv.st.EventsSince(cmd.SinceSeq)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, Events: evs}
	case "transcript_tail":
		n := cmd.N
		if n <= 0 {
			n = 50
		}
		if sv.transcript == nil {
			return Response{OK: true, Lines: []string{}}
		}
		return Response{OK: true, Lines: sv.transcript.Tail(cmd.IssueID, n)}
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

func (sv *Server) overview() (Overview, error) {
	issues, err := sv.st.Issues()
	if err != nil {
		return Overview{}, err
	}
	pending, err := sv.st.PendingDecisionRows()
	if err != nil {
		return Overview{}, err
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
	out.NeedYou = len(pending)
	for _, issue := range issues {
		state := issue.State
		if ev, ok := latest[issue.ID]; ok {
			switch ev.Type {
			case core.EvStageFailed:
				out.Failing++
				continue
			case core.EvDecisionRequired, core.EvIssueCompleted, core.EvIssueMerged:
				continue
			}
		}
		switch {
		case strings.HasPrefix(state, "failed"):
			out.Failing++
		case strings.HasPrefix(state, "queued"):
			out.Queued++
		case strings.HasPrefix(state, "running"):
			out.Building++
		}
	}
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
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

func (sv *Server) flowFor(name string) (flow.Flow, bool) {
	if sv.flows != nil {
		f, ok := sv.flows[name]
		return f, ok
	}
	return flow.Flow{}, false
}
