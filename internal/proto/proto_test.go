package proto

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/agentprotocol"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/engine"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/scaffold"
	"github.com/weston6142/watchtower/internal/slots"
	"github.com/weston6142/watchtower/internal/steward"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/workspace"
)

func TestPendingDecisionJSONContext(t *testing.T) {
	pending := engine.PendingDecision{
		ID: 31, IssueID: "GH-31", Stage: "execute",
		Context: &decision.DecisionContext{
			TaskSummary: "Ship decision context.", AgentName: "Executor",
			AgentColor: "green", AgentSymbol: "⚙",
		},
	}
	encoded, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Context *decision.DecisionContext `json:"context"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Context == nil || decoded.Context.TaskSummary != "Ship decision context." ||
		decoded.Context.AgentName != "Executor" || decoded.Context.AgentColor != "green" ||
		decoded.Context.AgentSymbol != "⚙" {
		t.Fatalf("pending JSON context = %#v, JSON = %s", decoded.Context, encoded)
	}
}

func protoDecisionIdentities(f flow.Flow) map[string]decision.AgentIdentity {
	identities := map[string]decision.AgentIdentity{}
	for _, stage := range f.Stages {
		for _, agent := range stage.Agents {
			identities[agent.Package] = decision.AgentIdentity{
				Name: agent.Package, Color: "green", Symbol: "⚙",
			}
		}
	}
	return identities
}

func TestSetupOutlineReportsConfiguredWorkflow(t *testing.T) {
	root := t.TempDir()
	if _, _, err := scaffold.Init(root); err != nil {
		t.Fatal(err)
	}
	watchtower := filepath.Join(root, ".watchtower")
	f, err := flow.Load(filepath.Join(watchtower, "flows", "default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	packages, err := pkgs.LoadDir(filepath.Join(watchtower, "packages"))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := newConfigClient(t, f, packages, fixtureRepoSetup())
	response, err := c.Do(Command{Op: "setup_outline"})
	if err != nil || !response.OK || response.Setup == nil {
		t.Fatalf("setup_outline: %+v err=%v", response, err)
	}
	if len(response.Setup.Stages) != len(f.Stages) {
		t.Fatalf("setup stages = %d, configured stages = %d", len(response.Setup.Stages), len(f.Stages))
	}
	for index, configured := range f.Stages {
		actual := response.Setup.Stages[index]
		if actual.Name != configured.Name || len(actual.Agents) != len(configured.Agents) {
			t.Fatalf("setup stage %d = %+v, configured = %+v", index, actual, configured)
		}
		for _, agent := range actual.Agents {
			if agent.Missing {
				t.Fatalf("missing configured package: %+v", agent)
			}
		}
	}
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	f := flow.Flow{
		Name: "default",
		Stages: []flow.Stage{{
			Name:   "run",
			Agents: []flow.AgentRef{{Package: "agent"}},
			Gate:   flow.GateAuto,
		}},
	}
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e := engine.New(engine.Config{
		Store: s,
		Runner: &runner.FakeRunner{Scripts: map[string]runner.Script{
			"run/agent": {},
		}},
		Pool:               slots.NewPool(1),
		Flows:              map[string]flow.Flow{"default": f},
		DecisionIdentities: protoDecisionIdentities(f),
		DataDir:            t.TempDir(),
	})
	sock := filepath.Join(t.TempDir(), "g.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(e, s)
	srv.SetFlows(map[string]flow.Flow{"default": f})
	go srv.Serve(l)
	t.Cleanup(func() { l.Close() })
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestCreateAnswerAndTailOverSocket(t *testing.T) {
	f, err := flow.Load("../flow/testdata/default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := store.Open("file:proto?mode=memory&cache=shared")
	defer s.Close()
	fr := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"brainstorm/brainstorm":      {},
		"spec/spec-writer":           {Artifacts: map[string]string{"spec.md": ""}},
		"execute/executor":           {Artifacts: map[string]string{"diff": ""}, Tokens: 10},
		"review/clean-code-reviewer": {Artifacts: map[string]string{"review.md": ""}},
		"review/reviewer":            {},
		"review/doc-writer":          {Artifacts: map[string]string{"docs": ""}},
	}}
	e := engine.New(engine.Config{Store: s, Runner: fr, Pool: slots.NewPool(2),
		Flows: map[string]flow.Flow{"default": f}, DecisionIdentities: protoDecisionIdentities(f), DataDir: t.TempDir()})
	_ = levers.Rules{}

	sock := filepath.Join(t.TempDir(), "g.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(e, s)
	srv.SetFlows(map[string]flow.Flow{"default": f})
	go srv.Serve(l)

	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	r, err := c.Do(Command{Op: "create_issue", Title: "hi", Flow: "default", Preset: "yolo"})
	if err != nil || !r.OK || r.IssueID == "" {
		t.Fatalf("create failed: %+v %v", r, err)
	}
	id := r.IssueID
	if r, _ = c.Do(Command{Op: "start_issue", IssueID: id}); !r.OK {
		t.Fatalf("start failed: %+v", r)
	}

	// spec gate escalates even on yolo — answer it
	deadline := time.After(5 * time.Second)
	for {
		r, _ = c.Do(Command{Op: "list_decisions"})
		if len(r.Decisions) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no decision appeared")
		case <-time.After(10 * time.Millisecond):
		}
	}
	option := 0
	if r, _ = c.Do(Command{Op: "answer_decision", DecisionID: r.Decisions[0].ID, Option: &option}); !r.OK {
		t.Fatalf("answer failed: %+v", r)
	}

	// tail until issue completes all 4 stages
	deadline = time.After(5 * time.Second)
	completed := 0
	var since int64
	for completed < 4 {
		r, _ = c.Do(Command{Op: "tail", SinceSeq: since})
		for _, ev := range r.Events {
			since = ev.Seq
			if ev.Type == core.EvStageCompleted {
				completed++
			}
		}
		select {
		case <-deadline:
			t.Fatalf("only %d stages completed", completed)
		case <-time.After(10 * time.Millisecond):
		}
	}

	r, err = c.Do(Command{Op: "issue_detail", IssueID: id})
	if err != nil || !r.OK || r.Detail == nil || r.Detail.Issue.ID != id {
		t.Fatalf("issue detail failed: %+v %v", r, err)
	}
	if len(r.Detail.Runs) != 6 {
		t.Fatalf("want 6 stage runs, got %d", len(r.Detail.Runs))
	}
	r, _ = c.Do(Command{Op: "overview"})
	if !r.OK || r.Overview == nil {
		t.Fatalf("overview: %+v", r)
	}
	if r.Overview.ShippedToday != 0 || r.Overview.Building != 0 || r.Overview.NeedYou != 0 || r.Overview.TokensTotal <= 0 {
		t.Fatalf("overview values: %+v", r.Overview)
	}

	r, _ = c.Do(Command{Op: "create_issue", Title: "pending", Flow: "default", Preset: "yolo"})
	if !r.OK {
		t.Fatalf("second create failed: %+v", r)
	}
	second := r.IssueID
	if r, _ = c.Do(Command{Op: "start_issue", IssueID: second}); !r.OK {
		t.Fatalf("second start failed: %+v", r)
	}
	deadline = time.After(5 * time.Second)
	for {
		r, _ = c.Do(Command{Op: "overview"})
		if r.Overview != nil && r.Overview.NeedYou == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("second decision never appeared: %+v", r.Overview)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestAnswerDecisionAcceptsFreeformText(t *testing.T) {
	f := oneAgentFlow("agent")
	s, err := store.Open("file:freeform-proto?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fr := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"run/agent": {Asks: []levers.Decision{{
			Kind: levers.DecisionFreeform, Question: "Review spec.md",
			RecommendedResponse: "Approve spec.md as written.",
			Importance:          1.0,
		}}},
	}}
	e := engine.New(engine.Config{
		Store: s, Runner: fr, Pool: slots.NewPool(1),
		Flows: map[string]flow.Flow{"default": f}, DecisionIdentities: protoDecisionIdentities(f), DataDir: t.TempDir(),
	})
	sock := sockPath(t)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	srv := NewServer(e, s)
	srv.SetFlows(map[string]flow.Flow{"default": f})
	go srv.Serve(l)
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	r, _ := c.Do(Command{Op: "create_issue", Title: "spec", Flow: "default", Preset: "yolo"})
	if !r.OK {
		t.Fatalf("create: %+v", r)
	}
	if started, _ := c.Do(Command{Op: "start_issue", IssueID: r.IssueID}); !started.OK {
		t.Fatalf("start: %+v", started)
	}
	var decisionID int64
	deadline := time.After(5 * time.Second)
	for decisionID == 0 {
		pending, _ := c.Do(Command{Op: "list_decisions"})
		if len(pending.Decisions) == 1 {
			decisionID = pending.Decisions[0].ID
			break
		}
		select {
		case <-deadline:
			t.Fatal("freeform decision never appeared")
		case <-time.After(10 * time.Millisecond):
		}
	}
	answer, _ := c.Do(Command{
		Op: "answer_decision", DecisionID: decisionID,
		Text: "Clarify the rollout before approval.",
	})
	if !answer.OK {
		t.Fatalf("answer: %+v", answer)
	}
	deadline = time.After(5 * time.Second)
	for {
		events, _ := c.Do(Command{Op: "tail"})
		for _, event := range events.Events {
			if event.IssueID == r.IssueID && event.Type == core.EvIssueCompleted {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatal("issue did not finish after freeform answer")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestAnswerDecisionAcceptsChoiceNoteText(t *testing.T) {
	f := oneAgentFlow("agent")
	s, err := store.Open("file:choice-note-proto?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	responses := make(chan levers.Response, 1)
	fr := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"run/agent": {Asks: []levers.Decision{{
			Kind: levers.DecisionChoice, Question: "Proceed?",
			Options: []string{"approve", "hold"}, Recommended: 0,
			AllowFreeform: false, Importance: 1.0,
		}}},
	}, OnResponse: func(_ string, _ string, response levers.Response) { responses <- response }}
	e := engine.New(engine.Config{
		Store: s, Runner: fr, Pool: slots.NewPool(1),
		Flows: map[string]flow.Flow{"default": f}, DecisionIdentities: protoDecisionIdentities(f), DataDir: t.TempDir(),
	})
	sock := sockPath(t)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	srv := NewServer(e, s)
	srv.SetFlows(map[string]flow.Flow{"default": f})
	go srv.Serve(l)
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	created, _ := c.Do(Command{Op: "create_issue", Title: "choice note", Flow: "default", Preset: "yolo"})
	if !created.OK {
		t.Fatalf("create: %+v", created)
	}
	if started, _ := c.Do(Command{Op: "start_issue", IssueID: created.IssueID}); !started.OK {
		t.Fatalf("start: %+v", started)
	}
	var decisionID int64
	deadline := time.After(5 * time.Second)
	for decisionID == 0 {
		pending, _ := c.Do(Command{Op: "list_decisions"})
		if len(pending.Decisions) == 1 {
			decisionID = pending.Decisions[0].ID
			break
		}
		select {
		case <-deadline:
			t.Fatal("choice decision never appeared")
		case <-time.After(10 * time.Millisecond):
		}
	}
	answer, _ := c.Do(Command{
		Op: "answer_decision", DecisionID: decisionID,
		Text: "Clarify the rollout before approval.",
	})
	if !answer.OK {
		t.Fatalf("answer: %+v", answer)
	}
	deadline = time.After(5 * time.Second)
	for {
		pending, _ := c.Do(Command{Op: "list_decisions"})
		if len(pending.Decisions) == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("decision remains listed: %+v", pending.Decisions)
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case got := <-responses:
		if got.Kind != levers.DecisionFreeform || got.Text != "Clarify the rollout before approval." {
			t.Fatalf("runner response = %#v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not receive choice note")
	}
	for {
		events, _ := c.Do(Command{Op: "tail"})
		for _, event := range events.Events {
			if event.IssueID == created.IssueID && event.Type == core.EvIssueCompleted {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatal("issue did not complete")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestBacklogOps(t *testing.T) {
	c := newTestClient(t)
	parent, err := c.Do(Command{Op: "draft_issue", Title: "parent", Flow: "default", Preset: "regular"})
	if err != nil || !parent.OK {
		t.Fatalf("parent draft_issue: %v %+v", err, parent)
	}
	r, err := c.Do(Command{Op: "draft_issue", Title: "t", Body: "b", Flow: "default", Preset: "regular", Priority: 2,
		DependsOn: []string{" " + parent.IssueID + " ", parent.IssueID}})
	if err != nil || !r.OK {
		t.Fatalf("draft_issue: %v %+v", err, r)
	}
	id := r.IssueID

	r, err = c.Do(Command{Op: "update_issue", IssueID: id, Title: "t2", Body: "b2", Flow: "default", Preset: "strict", Priority: 5})
	if err != nil || !r.OK {
		t.Fatalf("update_issue: %v %+v", err, r)
	}

	r, _ = c.Do(Command{Op: "list_issues"})
	found := false
	for _, row := range r.Issues {
		if row.ID == id {
			found = true
			if row.State != "backlog" || row.Title != "t2" || row.Priority != 5 || row.Body != "b2" {
				t.Fatalf("row wrong after update: %+v", row)
			}
			if len(row.DependsOn) != 1 || row.DependsOn[0] != parent.IssueID {
				t.Fatalf("dependencies not normalized and retained: %+v", row.DependsOn)
			}
		}
	}
	if !found {
		t.Fatal("draft missing from list_issues")
	}

	r, err = c.Do(Command{Op: "launch_issue", IssueID: id})
	if err != nil || !r.OK {
		t.Fatalf("launch_issue: %v %+v", err, r)
	}
	r, _ = c.Do(Command{Op: "launch_issue", IssueID: id})
	if r.OK {
		t.Fatal("second launch succeeded")
	}
}

func TestClaimProtocolReturnsStructuredReadyBlockedAndResumableTasks(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "develop")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "README.md")
	git("commit", "-qm", "base")

	st, err := store.Open("file:claim-protocol?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fl := flow.Flow{Name: "default", Stages: []flow.Stage{{Name: "merge-verification"}}}
	eng := engine.New(engine.Config{
		Store: st, Runner: &runner.FakeRunner{}, Pool: slots.NewPool(1),
		Flows: map[string]flow.Flow{"default": fl}, DecisionIdentities: protoDecisionIdentities(fl), DataDir: t.TempDir(),
		Workspace: workspace.GitWorktree{Repo: repo}, Train: &marshal.Train{Repo: repo},
		Observers: []func(core.Event){(&steward.Steward{Store: st}).Observe},
	})
	parent, _ := eng.DraftIssue("ready", "full body", "default", "regular", levers.Matrix{}, 2, nil)
	child, _ := eng.DraftIssue("blocked", "", "default", "regular", levers.Matrix{}, 1, nil)
	if err := eng.SetDependencies(child, []string{parent}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(eng, st)

	listed := srv.exec(Command{Op: "list_backlog"})
	if !listed.OK || len(listed.Backlog) != 2 || len(listed.Claims) != 0 {
		t.Fatalf("list_backlog = %+v", listed)
	}
	byID := map[string]BacklogItem{}
	for _, item := range listed.Backlog {
		byID[item.Issue.ID] = item
	}
	if !byID[parent].Claimable || byID[parent].Issue.Body != "full body" ||
		byID[child].Claimable || len(byID[child].BlockedBy) != 1 || byID[child].BlockedBy[0] != parent {
		t.Fatalf("backlog items = %+v", byID)
	}
	claimed := srv.exec(Command{Op: "claim_issue", IssueID: parent})
	if !claimed.OK || claimed.Claim == nil || claimed.Claim.IssueID != parent {
		t.Fatalf("claim_issue = %+v", claimed)
	}
	resolved := srv.exec(Command{Op: "claim_for_worktree", Worktree: claimed.Claim.Worktree})
	if !resolved.OK || resolved.Claim == nil || resolved.Claim.IssueID != parent {
		t.Fatalf("claim_for_worktree = %+v", resolved)
	}
	finish := srv.exec(Command{
		Op: "finish_claim", IssueID: parent, Worktree: claimed.Claim.Worktree,
	})
	if finish.OK || !strings.Contains(finish.Error, "no commits beyond") {
		t.Fatalf("finish_claim without work = %+v", finish)
	}
	listed = srv.exec(Command{Op: "list_backlog"})
	if len(listed.Claims) != 1 || listed.Claims[0].IssueID != parent {
		t.Fatalf("resumable claims = %+v", listed.Claims)
	}
	released := srv.exec(Command{Op: "release_claim", IssueID: parent})
	if !released.OK {
		t.Fatalf("release_claim = %+v", released)
	}
}

func TestCanResetAndShutdownFlushesResponse(t *testing.T) {
	f := oneAgentFlow("agent")
	s, err := store.Open("file:reset-proto?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e := engine.New(engine.Config{
		Store: s, Runner: &runner.FakeRunner{Scripts: map[string]runner.Script{"run/agent": {}}},
		Pool: slots.NewPool(1), Flows: map[string]flow.Flow{"default": f}, DecisionIdentities: protoDecisionIdentities(f), DataDir: t.TempDir(),
	})
	socket := sockPath(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(e, s)
	server.SetFlows(map[string]flow.Flow{"default": f})
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	client, err := Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if response, err := client.Do(Command{Op: "can_reset"}); err != nil || !response.OK {
		t.Fatalf("can_reset: %+v err=%v", response, err)
	}
	response, err := client.Do(Command{Op: "shutdown"})
	if err != nil || !response.OK {
		t.Fatalf("shutdown response was not flushed: %+v err=%v", response, err)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop")
	}
	if replacement, err := Dial(socket); err == nil {
		replacement.Close()
		t.Fatal("server still accepts connections")
	}
}

func TestDraftNotCountedInOverview(t *testing.T) {
	c := newTestClient(t)
	if r, err := c.Do(Command{Op: "draft_issue", Title: "t", Flow: "default", Preset: "regular"}); err != nil || !r.OK {
		t.Fatalf("draft_issue: %v %+v", err, r)
	}
	r, err := c.Do(Command{Op: "overview"})
	if err != nil || !r.OK {
		t.Fatal(err)
	}
	if r.Overview.Building != 0 || r.Overview.Failing != 0 || r.Overview.Queued != 0 {
		t.Fatalf("draft counted in overview: %+v", r.Overview)
	}
}

func TestClaimedNotCountedInOverview(t *testing.T) {
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.UpsertIssue(store.IssueRow{
		ID: "GH-9", Title: "explore", State: "claimed", Flow: "default",
	}); err != nil {
		t.Fatal(err)
	}
	overview, err := NewServer(nil, s).overview()
	if err != nil {
		t.Fatal(err)
	}
	if overview.Building != 0 || overview.Queued != 0 || overview.Failing != 0 || overview.NeedYou != 0 {
		t.Fatalf("claimed issue counted in overview: %+v", overview)
	}
}

func TestOverviewIgnoresTrailingFailureForAbandonedIssue(t *testing.T) {
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.UpsertIssue(store.IssueRow{
		ID: "GH-3", Title: "abandoned", State: "abandoned", Flow: "default",
	}); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []core.EventType{core.EvIssueAbandoned, core.EvStageFailed} {
		event, err := core.NewEvent(typ, "GH-3", map[string]string{"stage": "merge"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Append(event); err != nil {
			t.Fatal(err)
		}
	}

	socketDir, err := os.MkdirTemp("", "overview")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	sock := filepath.Join(socketDir, "watchtower.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go NewServer(nil, s).Serve(listener)

	client, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	response, err := client.Do(Command{Op: "overview"})
	if err != nil || !response.OK || response.Overview == nil {
		t.Fatalf("overview: %v %+v", err, response)
	}
	if response.Overview.Failing != 0 || response.Overview.Building != 0 || response.Overview.NeedYou != 0 {
		t.Fatalf("abandoned issue counted in overview: %+v", response.Overview)
	}
}

func TestOverviewClassifiesFinalizationStates(t *testing.T) {
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for index, state := range []string{
		"verifying", "waiting:integration", "integrating", "failed:finalize",
	} {
		if err := s.UpsertIssue(store.IssueRow{
			ID: fmt.Sprintf("GH-%d", index+1), Title: state, State: state, Flow: "default",
		}); err != nil {
			t.Fatal(err)
		}
	}
	overview, err := NewServer(nil, s).overview()
	if err != nil {
		t.Fatal(err)
	}
	if overview.Building != 3 || overview.Queued != 0 || overview.Failing != 1 {
		t.Fatalf("overview = %+v", overview)
	}
}

// sockPath returns a Unix socket path short enough for macOS's 104-byte
// sun_path limit; t.TempDir() embeds the test name and can exceed it.
func sockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "g.sock")
}

// newConfigClient serves one flow, one package set, and one resolved repo
// config over a real socket. Separate from newTestClient so the setup ops can
// pose three-agent stages, missing packages, and levers without disturbing
// that helper's fixtures. The store is returned so a test can seed events
// directly instead of racing the engine.
func newConfigClient(t *testing.T, f flow.Flow, packages map[string]pkgs.Package, repo RepoSetup) (*Client, *store.Store) {
	t.Helper()
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	scripts := map[string]runner.Script{}
	for _, stg := range f.Stages {
		for _, ref := range stg.Agents {
			scripts[stg.Name+"/"+ref.Package] = runner.Script{}
		}
	}
	e := engine.New(engine.Config{
		Store: s, Runner: &runner.FakeRunner{Scripts: scripts},
		Pool: slots.NewPool(1), Flows: map[string]flow.Flow{f.Name: f},
		DecisionIdentities: protoDecisionIdentities(f),
		DataDir:            t.TempDir(),
	})
	sock := sockPath(t)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(e, s)
	srv.SetFlows(map[string]flow.Flow{f.Name: f})
	srv.SetPackages(packages)
	srv.SetRepoSetup(repo)
	go srv.Serve(l)
	t.Cleanup(func() { l.Close() })
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, s
}

// oneAgentFlow is the smallest flow the setup ops accept: one auto stage named
// "run" with a single agent, so a test only has to say which package.
func oneAgentFlow(pkg string) flow.Flow {
	return flow.Flow{Name: "default", Stages: []flow.Stage{{
		Name: "run", Agents: []flow.AgentRef{{Package: pkg}},
		Gate: flow.GateAuto, Completion: flow.CompletionAll, Workspace: "none",
	}}}
}

// overrideFlow is a one-stage flow whose AgentRef.Model contradicts the
// package's model. internal/claude/runner.go passes pkg.Model alone, so the
// package must win everywhere.
func overrideFlow() flow.Flow {
	f := oneAgentFlow("agent")
	f.Stages[0].Agents[0].Model = "sonnet"
	return f
}

func overridePackages() map[string]pkgs.Package {
	return map[string]pkgs.Package{"agent": {
		Name: "agent", Model: "opus", Effort: "medium",
		AllowedTools: []string{"Bash", "Read"}, MaxTurns: 12,
		Prompt: "You are the agent.\n\nDo the work.\n",
	}}
}

// issue_detail used to let AgentRef.Model win. The runner cannot see it, so
// the rail and the stream-door subtitle were printing a model no CLI ever
// received.
func TestIssueDetailReportsPackageModelNotAgentRefOverride(t *testing.T) {
	c, s := newConfigClient(t, overrideFlow(), overridePackages(), RepoSetup{})
	r, err := c.Do(Command{Op: "create_issue", Title: "t", Flow: "default", Preset: "regular"})
	if err != nil || !r.OK {
		t.Fatalf("create: %+v %v", r, err)
	}
	id := r.IssueID
	ev, err := core.NewEvent(core.EvStageStarted, id, map[string]any{"stage": "run"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ev); err != nil {
		t.Fatal(err)
	}
	r, err = c.Do(Command{Op: "issue_detail", IssueID: id})
	if err != nil || !r.OK || r.Detail == nil {
		t.Fatalf("issue_detail: %+v %v", r, err)
	}
	if r.Detail.Model != "opus" {
		t.Errorf("Detail.Model = %q, want opus (the package model)", r.Detail.Model)
	}
	if r.Detail.Effort != "medium" {
		t.Errorf("Detail.Effort = %q, want medium", r.Detail.Effort)
	}
}

func TestIssueDetailExposesPreservedWork(t *testing.T) {
	c, s := newConfigClient(t, oneAgentFlow("agent"), overridePackages(), RepoSetup{})
	if err := s.UpsertIssue(store.IssueRow{
		ID: "GH-1", Title: "research", State: "done (unmerged)", Flow: "default",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIssueIntegration(store.IssueIntegration{
		IssueID: "GH-1", State: store.IntegrationPreserved,
		Worktree: "/tmp/GH-1", Branch: "issue/GH-1",
	}); err != nil {
		t.Fatal(err)
	}
	response, err := c.Do(Command{Op: "issue_detail", IssueID: "GH-1"})
	if err != nil || !response.OK || response.Detail == nil ||
		response.Detail.IntegrationState != store.IntegrationPreserved ||
		response.Detail.Worktree != "/tmp/GH-1" ||
		response.Detail.Branch != "issue/GH-1" {
		t.Fatalf("detail = %+v err=%v", response.Detail, err)
	}
}

// reviewFlow mirrors the shape of .watchtower/flows/default.yaml's review
// stage: three agents, parallel, heavy, worktree. A stage→one-package model is
// wrong and this is the fixture that proves it.
func reviewFlow() flow.Flow {
	f, err := flow.Load("../flow/testdata/default.yaml")
	if err != nil {
		panic(err)
	}
	return f
}

func reviewPackages() map[string]pkgs.Package {
	out := map[string]pkgs.Package{}
	for _, name := range []string{"brainstorm", "spec-writer", "executor",
		"clean-code-reviewer", "reviewer", "doc-writer"} {
		out[name] = pkgs.Package{
			Name: name, Model: "opus", Effort: "medium",
			AllowedTools: []string{"Bash", "Read", "Edit"},
			Prompt:       "Package " + name + ".\n\nSecond line.\n\nThird line.\nFourth line.\n",
		}
	}
	return out
}

func fixtureRepoSetup() RepoSetup {
	return RepoSetup{
		Runner: "claude", Slots: 4, Budget: 0, PricePerMTok: 15,
		ClaudeBin: "claude", TestCmd: "go test ./...",
		Pull: true, Push: true, Workspace: "treehouse", LoadedAt: "12:55",
	}
}

func fixtureCodexRepoSetup() RepoSetup {
	return RepoSetup{
		Runner: "codex", Slots: 4, CodexBin: "codex",
		CodexModel: "gpt-5.6-luna", CodexEffort: "xhigh",
		ClaudeBin: "claude", Workspace: "treehouse", LoadedAt: "12:55",
	}
}

func TestCodexSetupResolvesRepoDefaultsAndLabelsClaudeTools(t *testing.T) {
	packages := reviewPackages()
	for name, pkg := range packages {
		pkg.Model, pkg.Effort = "", ""
		packages[name] = pkg
	}
	c, _ := newConfigClient(t, reviewFlow(), packages, fixtureCodexRepoSetup())
	r, _ := c.Do(Command{Op: "setup_outline"})
	ag := r.Setup.Stages[0].Agents[0]
	if ag.Model != "gpt-5.6-luna" || ag.Effort != "xhigh" || ag.ThinkingTokens != "" {
		t.Fatalf("effective Codex agent = %+v", ag)
	}
	if len(ag.AllowedTools) != 0 || len(ag.DeclaredAllowedTools) == 0 || ag.ToolSource != "codex config" {
		t.Fatalf("Codex tools = %+v", ag)
	}
}

func TestCodexSetupPackageModelAndEffortOverrideRepoDefaults(t *testing.T) {
	packages := reviewPackages()
	pkg := packages["brainstorm"]
	pkg.Model, pkg.Effort = "custom", "high"
	packages["brainstorm"] = pkg
	c, _ := newConfigClient(t, reviewFlow(), packages, fixtureCodexRepoSetup())
	r, _ := c.Do(Command{Op: "setup_outline"})
	ag := r.Setup.Stages[0].Agents[0]
	if ag.Model != "custom" || ag.Effort != "high" {
		t.Fatalf("effective Codex override = %+v", ag)
	}
}

func TestClaudeSetupRetainsThinkingBudgetAndEffectiveAllowedTools(t *testing.T) {
	c, _ := newConfigClient(t, reviewFlow(), reviewPackages(), fixtureRepoSetup())
	r, _ := c.Do(Command{Op: "setup_outline"})
	ag := r.Setup.Stages[0].Agents[0]
	if ag.ThinkingTokens != "8192" || len(ag.AllowedTools) == 0 ||
		len(ag.DeclaredAllowedTools) != 0 || ag.ToolSource != "" {
		t.Fatalf("effective Claude agent = %+v", ag)
	}
}

func TestSetupOutlineReportsResolvedRepoConfig(t *testing.T) {
	c, _ := newConfigClient(t, reviewFlow(), reviewPackages(), fixtureRepoSetup())
	r, err := c.Do(Command{Op: "setup_outline"})
	if err != nil || !r.OK || r.Setup == nil {
		t.Fatalf("setup_outline: %+v %v", r, err)
	}
	got := r.Setup.Repo
	want := fixtureRepoSetup()
	if got != want {
		t.Fatalf("Repo = %+v, want %+v", got, want)
	}
	if r.Setup.Flow != "default" {
		t.Errorf("Flow = %q, want default", r.Setup.Flow)
	}
}

// The review stage has three agents. issue_detail shipped with an Agents[0]
// bug; this is the regression guard for the inspector.
func TestSetupOutlineCoversEveryAgentInStage(t *testing.T) {
	c, _ := newConfigClient(t, reviewFlow(), reviewPackages(), fixtureRepoSetup())
	r, _ := c.Do(Command{Op: "setup_outline"})
	if r.Setup == nil {
		t.Fatal("no setup view")
	}
	var review *StageSetup
	for i := range r.Setup.Stages {
		if r.Setup.Stages[i].Name == "review" {
			review = &r.Setup.Stages[i]
		}
	}
	if review == nil {
		t.Fatal("no review stage in outline")
	}
	if len(review.Agents) != 3 {
		t.Fatalf("review has %d agents, want 3", len(review.Agents))
	}
	if !review.Parallel || review.Completion != flow.CompletionAll || review.Workspace != "worktree" {
		t.Errorf("review knobs wrong: %+v", *review)
	}
	for _, ag := range review.Agents {
		if ag.Model != "opus" || ag.Effort != "medium" || ag.ThinkingTokens != "8192" {
			t.Errorf("agent %s: %+v", ag.Package, ag)
		}
		if len(ag.PromptPreview) != 3 {
			t.Errorf("agent %s preview has %d lines, want 3", ag.Package, len(ag.PromptPreview))
		}
		if ag.PromptLines == 0 {
			t.Errorf("agent %s has no prompt line count", ag.Package)
		}
	}
	// Every stage in the flow is reported, in flow order.
	want := reviewFlow().Stages
	if len(r.Setup.Stages) != len(want) {
		t.Fatalf("got %d stages, want %d", len(r.Setup.Stages), len(want))
	}
	for i, stg := range want {
		if r.Setup.Stages[i].Name != stg.Name {
			t.Fatalf("stage %d is %q, want %q", i, r.Setup.Stages[i].Name, stg.Name)
		}
	}
}

func TestSetupOutlineReportsPackageModelNotAgentRefOverride(t *testing.T) {
	c, _ := newConfigClient(t, overrideFlow(), overridePackages(), fixtureRepoSetup())
	r, _ := c.Do(Command{Op: "setup_outline"})
	if r.Setup == nil || len(r.Setup.Stages) != 1 || len(r.Setup.Stages[0].Agents) != 1 {
		t.Fatalf("outline: %+v", r.Setup)
	}
	ag := r.Setup.Stages[0].Agents[0]
	if ag.Model != "opus" {
		t.Errorf("Model = %q, want opus (the package model)", ag.Model)
	}
	if ag.DeclaredModel != "sonnet" {
		t.Errorf("DeclaredModel = %q, want sonnet (declared, not applied)", ag.DeclaredModel)
	}
	if ag.MaxTurns != 12 {
		t.Errorf("MaxTurns = %d, want 12 reported as declared-and-unapplied", ag.MaxTurns)
	}
}

// Both ops resolve the model through effectiveAgent for this reason: if the
// inspector reported runner truth while issue_detail kept the override, two
// surfaces in one TUI would print different models for the same stage.
func TestIssueDetailAgreesWithSetupOutlineOnModel(t *testing.T) {
	for _, setup := range []RepoSetup{fixtureRepoSetup(), fixtureCodexRepoSetup()} {
		t.Run(setup.Runner, func(t *testing.T) {
			c, s := newConfigClient(t, overrideFlow(), overridePackages(), setup)
			r, err := c.Do(Command{Op: "create_issue", Title: "t", Flow: "default", Preset: "regular"})
			if err != nil || !r.OK {
				t.Fatalf("create: %+v %v", r, err)
			}
			id := r.IssueID
			ev, err := core.NewEvent(core.EvStageStarted, id, map[string]any{"stage": "run"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Append(ev); err != nil {
				t.Fatal(err)
			}
			detail, _ := c.Do(Command{Op: "issue_detail", IssueID: id})
			outline, _ := c.Do(Command{Op: "setup_outline", IssueID: id})
			if detail.Detail == nil || outline.Setup == nil {
				t.Fatalf("detail=%+v outline=%+v", detail, outline)
			}
			want := outline.Setup.Stages[0].Agents[0].Model
			if detail.Detail.Model != want {
				t.Fatalf("issue_detail model %q, setup_outline model %q", detail.Detail.Model, want)
			}
		})
	}
}

func TestSetupOutlineMarksMissingPackage(t *testing.T) {
	c, _ := newConfigClient(t, oneAgentFlow("ghost"), map[string]pkgs.Package{}, fixtureRepoSetup())
	r, _ := c.Do(Command{Op: "setup_outline"})
	if r.Setup == nil {
		t.Fatal("no setup view")
	}
	ag := r.Setup.Stages[0].Agents[0]
	if ag.Package != "ghost" || !ag.Missing {
		t.Fatalf("agent = %+v, want ghost with Missing", ag)
	}
	if ag.Model != "" || ag.Effort != "" || len(ag.AllowedTools) != 0 || len(ag.PromptPreview) != 0 {
		t.Errorf("missing package carries effective fields: %+v", ag)
	}
}

func TestSetupOutlineIssueScopedLevers(t *testing.T) {
	c, _ := newConfigClient(t, reviewFlow(), reviewPackages(), fixtureRepoSetup())
	r, err := c.Do(Command{Op: "create_issue", Title: "t", Flow: "default", Preset: "regular"})
	if err != nil || !r.OK {
		t.Fatalf("create: %+v %v", r, err)
	}
	id := r.IssueID
	// set_lever persists through engine.SetLever → store.SetIssueLever, so
	// store.Issues()[].Levers — where setupView reads — reflects it.
	if r, _ = c.Do(Command{Op: "set_lever", IssueID: id, Stage: "review", Lever: "strict"}); !r.OK {
		t.Fatalf("set_lever: %+v", r)
	}
	r, _ = c.Do(Command{Op: "setup_outline", IssueID: id})
	if r.Setup == nil {
		t.Fatal("no setup view")
	}
	if r.Setup.IssueID != id {
		t.Errorf("IssueID = %q, want %q", r.Setup.IssueID, id)
	}
	for _, stg := range r.Setup.Stages {
		if stg.Name == "review" {
			if stg.Lever != "strict" {
				t.Errorf("review lever = %q, want strict", stg.Lever)
			}
			continue
		}
		if stg.Lever == "strict" {
			t.Errorf("stage %s leaked the review lever", stg.Name)
		}
	}
}

func TestSetupOutlineUnknownFlowAndIssue(t *testing.T) {
	c, _ := newConfigClient(t, reviewFlow(), reviewPackages(), fixtureRepoSetup())
	for _, tc := range []struct {
		name string
		cmd  Command
		want string
	}{
		{"unknown flow", Command{Op: "setup_outline", Flow: "nope"}, "unknown flow nope"},
		{"unknown issue", Command{Op: "setup_outline", IssueID: "GH-404"}, "unknown issue GH-404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := c.Do(tc.cmd)
			if err != nil {
				t.Fatal(err)
			}
			if r.OK || r.Error != tc.want {
				t.Fatalf("got OK=%v err=%q, want error %q", r.OK, r.Error, tc.want)
			}
		})
	}
}

// A server built without SetFlows must say so rather than returning an empty
// panel that reads as a flow with no stages.
func TestSetupOutlineWithoutFlowsLoaded(t *testing.T) {
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	sv := NewServer(nil, s)
	if r := sv.exec(Command{Op: "setup_outline"}); r.OK || r.Error != "no flows loaded" {
		t.Fatalf("got %+v, want error \"no flows loaded\"", r)
	}
}

func TestSetupPromptIncludesTaskLineAndPrompt(t *testing.T) {
	packages := reviewPackages()
	c, _ := newConfigClient(t, reviewFlow(), packages, fixtureRepoSetup())
	r, err := c.Do(Command{Op: "setup_prompt", Stage: "review", Package: "reviewer", IssueID: ""})
	if err != nil || !r.OK {
		t.Fatalf("setup_prompt: %+v %v", r, err)
	}
	joined := strings.Join(r.Lines, "\n")
	// The synthesized first user message lives in Go source and is invisible in
	// the package files — showing it verbatim is the point of the op.
	if !strings.Contains(joined, agentprotocol.TaskMessage("review", unscopedIssueID)) {
		t.Errorf("task line missing from:\n%s", joined)
	}
	if r.Lines[0] != promptTaskHeading {
		t.Errorf("first line = %q, want %q", r.Lines[0], promptTaskHeading)
	}
	if !strings.Contains(joined, promptSystemHeadingPrefix+"reviewer/prompt.md") {
		t.Errorf("system-prompt heading missing from:\n%s", joined)
	}
	for _, line := range strings.Split(strings.TrimSuffix(packages["reviewer"].Prompt, "\n"), "\n") {
		found := false
		for _, got := range r.Lines {
			if got == line {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("prompt line %q missing from response", line)
		}
	}
	if r.Lines[len(r.Lines)-1] != promptBaseNote {
		t.Errorf("last line = %q, want the base-prompt note", r.Lines[len(r.Lines)-1])
	}
}

// Scoped to an issue, the task line names that issue rather than the template
// placeholder.
func TestSetupPromptScopedToIssueNamesIt(t *testing.T) {
	c, _ := newConfigClient(t, reviewFlow(), reviewPackages(), fixtureRepoSetup())
	created, _ := c.Do(Command{Op: "create_issue", Title: "t", Flow: "default", Preset: "regular"})
	r, _ := c.Do(Command{Op: "setup_prompt", Stage: "review", Package: "reviewer", IssueID: created.IssueID})
	if !r.OK {
		t.Fatalf("setup_prompt: %+v", r)
	}
	want := agentprotocol.TaskMessage("review", created.IssueID)
	if !strings.Contains(strings.Join(r.Lines, "\n"), want) {
		t.Errorf("task line does not name %s", created.IssueID)
	}
}

// The op must not become a way to dump arbitrary package bodies by guessing a
// name: the package has to be one this stage actually names.
func TestSetupPromptRejectsUnpairedStagePackage(t *testing.T) {
	c, _ := newConfigClient(t, reviewFlow(), reviewPackages(), fixtureRepoSetup())
	r, err := c.Do(Command{Op: "setup_prompt", Stage: "spec", Package: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK || r.Error != "stage spec has no agent reviewer" {
		t.Fatalf("got OK=%v err=%q, want the unpaired error", r.OK, r.Error)
	}
	if len(r.Lines) != 0 {
		t.Fatalf("rejected request still returned %d lines", len(r.Lines))
	}
}

func TestSetupPromptRejectsUnloadedPackage(t *testing.T) {
	c, _ := newConfigClient(t, oneAgentFlow("ghost"), map[string]pkgs.Package{}, fixtureRepoSetup())
	r, _ := c.Do(Command{Op: "setup_prompt", Stage: "run", Package: "ghost"})
	if r.OK || r.Error != "package ghost not loaded" {
		t.Fatalf("got OK=%v err=%q, want the not-loaded error", r.OK, r.Error)
	}
}

// A 300 KiB prompt must not blow the 1 MiB frame, and the operator must be
// told the body was clipped rather than silently shown a partial prompt.
func TestSetupPromptTruncatesOversizePrompt(t *testing.T) {
	big := strings.Repeat("x123456789\n", 30_000) // ~330 KiB
	packages := map[string]pkgs.Package{"agent": {Name: "agent", Prompt: big}}
	c, _ := newConfigClient(t, oneAgentFlow("agent"), packages, fixtureRepoSetup())
	r, err := c.Do(Command{Op: "setup_prompt", Stage: "run", Package: "agent"})
	if err != nil {
		t.Fatalf("client could not read the frame: %v", err)
	}
	if !r.OK {
		t.Fatalf("setup_prompt: %+v", r)
	}
	joined := strings.Join(r.Lines, "\n")
	if !strings.Contains(joined, "— truncated at 256 KiB (prompt is") {
		t.Fatal("no truncation line in an oversize prompt")
	}
	if len(joined) > maxMessageBytes {
		t.Fatalf("response body is %d bytes, over the %d frame limit", len(joined), maxMessageBytes)
	}
}
