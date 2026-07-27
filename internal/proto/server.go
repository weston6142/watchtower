package proto

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"sort"

	"github.com/wbushyeager/guildhall/internal/archmap"
	"github.com/wbushyeager/guildhall/internal/engine"
	"github.com/wbushyeager/guildhall/internal/flow"
	"github.com/wbushyeager/guildhall/internal/levers"
	"github.com/wbushyeager/guildhall/internal/store"
)

type Server struct {
	eng   *engine.Engine
	st    *store.Store
	flows map[string]flow.Flow
}

func NewServer(e *engine.Engine, s *store.Store) *Server {
	return &Server{eng: e, st: s}
}

// SetFlows lets the daemon share loaded flows for preset expansion.
func (sv *Server) SetFlows(f map[string]flow.Flow) { sv.flows = f }

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
		return Response{OK: true, Detail: &IssueDetail{
			Issue: issue, Runs: runs, Tokens: tokens, Artifacts: artifacts,
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
	default:
		return Response{Error: "unknown op " + cmd.Op}
	}
}

func (sv *Server) flowFor(name string) (flow.Flow, bool) {
	if sv.flows != nil {
		f, ok := sv.flows[name]
		return f, ok
	}
	return flow.Flow{}, false
}
