package proto

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/agentprotocol"
	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/engine"
	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/plannerbudget"
	"github.com/weston6142/watchtower/internal/review"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/scaffold"
	"github.com/weston6142/watchtower/internal/slots"
	"github.com/weston6142/watchtower/internal/stageusage"
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

func TestPendingDecisionJSONExposesRequiredResponseCapability(t *testing.T) {
	pending := engine.PendingDecision{
		ID: 32, IssueID: "GH-32", Stage: "spec",
		D: levers.Decision{
			Question: "Approve spec?", Options: []string{"approve", "revise"},
			RequiresOption: true,
		},
	}
	encoded, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	var decoded engine.PendingDecision
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.D.RequiresOption {
		t.Fatalf("pending JSON omitted required response capability: %s", encoded)
	}
}

func TestPendingDecisionJSONCarriesEscalationEvidence(t *testing.T) {
	importance := 0.2
	hash := strings.Repeat("a", 64)
	evaluation := review.Evaluation{
		Outcome: review.OutcomeRequiresApproval, RequiredFloor: review.FloorPolicy, EffectiveFloor: review.FloorOperator,
		PolicyID: "team-safety", PolicyVersion: "7", Item: review.ItemBinding{
			Kind: review.ItemDecision, Hash: hash, Path: "payments/charge.go", Operation: "decision",
		}, Model: review.ModelMetadata{Importance: &importance, Options: []string{"approve"}, Rationale: "advisory"},
	}
	pending := engine.PendingDecision{ID: 64, IssueID: "GH-64", Stage: "execute", Evaluation: &evaluation,
		Bindings: []review.Binding{{Item: evaluation.Item, RequiredFloor: evaluation.RequiredFloor, EffectiveFloor: evaluation.EffectiveFloor,
			PolicyID: evaluation.PolicyID, PolicyVersion: evaluation.PolicyVersion, Model: evaluation.Model}}}
	encoded, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	var decoded engine.PendingDecision
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Evaluation == nil || decoded.Evaluation.Outcome != evaluation.Outcome ||
		decoded.Evaluation.RequiredFloor != review.FloorPolicy || decoded.Evaluation.PolicyID != "team-safety" ||
		len(decoded.Bindings) != 1 || decoded.Bindings[0].Item.Hash != hash || decoded.Bindings[0].Model.Rationale != "advisory" {
		t.Fatalf("pending escalation evidence = %#v JSON=%s", decoded, encoded)
	}
}

func TestPlannerOverrideRoundTripsThroughCommandJSON(t *testing.T) {
	warn, hard := int64(4), int64(5)
	elapsedWarn, elapsedHard := 2*time.Minute, 3*time.Minute
	want := Command{
		Op:      "start_issue",
		IssueID: "GH-39",
		PlannerBudget: &plannerbudget.Override{
			Calls:   &plannerbudget.DimensionOverride{Warning: &warn, Hard: &hard},
			Elapsed: &plannerbudget.ElapsedOverride{Warning: &elapsedWarn, Hard: &elapsedHard},
		},
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Command
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.PlannerBudget == nil || *got.PlannerBudget.Calls.Warning != warn ||
		*got.PlannerBudget.Calls.Hard != hard ||
		*got.PlannerBudget.Elapsed.Warning != elapsedWarn {
		t.Fatalf("planner override = %+v from %s", got.PlannerBudget, data)
	}
}

func TestInvalidPlannerOverrideIsRejectedBeforeStart(t *testing.T) {
	zero := int64(0)
	sv := NewServer(nil, nil)
	response := sv.exec(Command{
		Op: "start_issue", IssueID: "GH-39",
		PlannerBudget: &plannerbudget.Override{
			Calls: &plannerbudget.DimensionOverride{Warning: &zero},
		},
	})
	if response.Error == "" {
		t.Fatal("invalid planner override reached engine start")
	}
}

func TestIssueDetailExposesLatestPlannerSnapshot(t *testing.T) {
	s, err := store.Open("file:planner-detail?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.UpsertIssue(store.IssueRow{ID: "GH-39", Title: "bounded", State: "running", Flow: "default"}); err != nil {
		t.Fatal(err)
	}
	snapshot := stageusage.Snapshot{Stage: "plan", CallsUsed: 2, ChargedTokens: 40, Status: stageusage.StatusWarning}
	ev, err := core.NewEvent(core.EvPlannerBudgetUpdated, "GH-39", map[string]any{
		"stage": "plan", "outcome": "warning", "snapshot": snapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ev); err != nil {
		t.Fatal(err)
	}
	response := NewServer(nil, s).exec(Command{Op: "issue_detail", IssueID: "GH-39"})
	if !response.OK || response.Detail == nil || response.Detail.Planner == nil ||
		response.Detail.Planner.ChargedTokens != 40 || response.Detail.PlannerOutcome != "warning" {
		t.Fatalf("issue detail = %+v", response)
	}
}

func TestIssueDetailFailureHistoryIsAvailableWhenEmpty(t *testing.T) {
	s, err := store.Open("file:failure-detail-empty?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.UpsertIssue(store.IssueRow{ID: "GH-63", Title: "failure history", State: "running", Flow: "default"}); err != nil {
		t.Fatal(err)
	}
	response := NewServer(nil, s).exec(Command{Op: "issue_detail", IssueID: "GH-63"})
	if !response.OK || response.Detail == nil || response.Detail.FailureHistory.Status != failureHistoryAvailable ||
		response.Detail.FailureHistory.Records == nil || len(response.Detail.FailureHistory.Records) != 0 {
		t.Fatalf("empty failure history = %+v", response)
	}
}

func TestIssueDetailFailureHistoryUsesCanonicalOrderAndReportsUnavailableReads(t *testing.T) {
	s, err := store.Open("file:failure-detail-history?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.UpsertIssue(store.IssueRow{ID: "GH-63", Title: "failure history", State: "running", Flow: "default"}); err != nil {
		t.Fatal(err)
	}
	input := failure.RecordInput{
		IssueID: "GH-63", Stage: "execute", StageAttempt: 1, FailureSite: failure.SiteRunner,
		FailureClass: failure.ClassExecution, RetryDisposition: failure.RetryNow,
		RequiredStateChange: failure.StateRunnerInput, Fingerprint: "sha256:" + strings.Repeat("a", 64),
	}
	first, err := s.AppendFailure(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	input.Fingerprint = "sha256:" + strings.Repeat("b", 64)
	second, err := s.AppendFailure(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	response := NewServer(nil, s).exec(Command{Op: "issue_detail", IssueID: "GH-63"})
	if !response.OK || response.Detail == nil || response.Detail.FailureHistory.Status != failureHistoryAvailable ||
		len(response.Detail.FailureHistory.Records) != 2 || response.Detail.FailureHistory.Records[0] != first ||
		response.Detail.FailureHistory.Records[1] != second {
		t.Fatalf("ordered failure history = %+v", response)
	}
	s.FailNextFailureHistoryForTest()
	response = NewServer(nil, s).exec(Command{Op: "issue_detail", IssueID: "GH-63"})
	if !response.OK || response.Detail == nil || response.Detail.FailureHistory.Status != failureHistoryUnavailable ||
		response.Detail.FailureHistory.Records != nil || response.Detail.Issue.ID != "GH-63" {
		t.Fatalf("unavailable failure history = %+v", response)
	}
}

func TestIssueDetailExposesDecisionPageWhenPresent(t *testing.T) {
	s, err := store.Open("file:decision-page-detail?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dataDir := t.TempDir()
	e := engine.New(engine.Config{Store: s, DataDir: dataDir})
	if err := s.UpsertIssue(store.IssueRow{ID: "GH-43", Title: "brief", State: "running", Flow: "default"}); err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(dataDir, "GH-43", "decision.html")
	if err := os.MkdirAll(filepath.Dir(page), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(page, []byte("<!doctype html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	response := NewServer(e, s).exec(Command{Op: "issue_detail", IssueID: "GH-43"})
	if !response.OK || response.Detail == nil || response.Detail.DecisionPage != page {
		t.Fatalf("issue detail = %+v, want page %q", response, page)
	}
	if err := os.Remove(page); err != nil {
		t.Fatal(err)
	}
	response = NewServer(e, s).exec(Command{Op: "issue_detail", IssueID: "GH-43"})
	if response.Detail == nil || response.Detail.DecisionPage != "" {
		t.Fatalf("missing decision page was exposed: %+v", response.Detail)
	}
}

func TestAnswerCommandCarriesActor(t *testing.T) {
	option := 0
	want := Command{Op: "answer_decision", DecisionID: 35, Option: &option, Actor: "alice"}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Command
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got.Actor != "alice" || got.DecisionID != want.DecisionID || got.Option == nil || *got.Option != option {
		t.Fatalf("answer command = %#v, JSON = %s", got, encoded)
	}
}

func TestLegacyPendingDecisionJSONOmitsContext(t *testing.T) {
	encoded, err := json.Marshal(engine.PendingDecision{
		ID: 32, IssueID: "GH-31", Stage: "spec",
		D: levers.Decision{Question: "Legacy?", Kind: levers.DecisionFreeform},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"context"`) || !strings.Contains(string(encoded), "Legacy?") {
		t.Fatalf("legacy pending JSON = %s", encoded)
	}
}

func TestPendingDecisionJSONReviewTarget(t *testing.T) {
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	want := review.Target{
		IssueID: "GH-26", Stage: "spec", CheckpointID: 9,
		Artifacts:       []contextpack.Artifact{{Name: "spec.md", SHA256: digest}},
		ArtifactVersion: "9|spec.md=" + digest, NextStage: "plan",
	}
	encoded, err := json.Marshal(engine.PendingDecision{ID: 33, IssueID: "GH-26", Stage: "spec", Review: &want})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Review *review.Target `json:"review"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Review == nil || decoded.Review.IssueID != want.IssueID ||
		decoded.Review.CheckpointID != want.CheckpointID || decoded.Review.ArtifactVersion != want.ArtifactVersion ||
		len(decoded.Review.Artifacts) != 1 || decoded.Review.Artifacts[0] != want.Artifacts[0] ||
		decoded.Review.NextStage != want.NextStage {
		t.Fatalf("pending JSON review = %#v, JSON = %s", decoded.Review, encoded)
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

func assertDecisionContext(t *testing.T, pending engine.PendingDecision, question string) {
	t.Helper()
	if pending.D.Question != question || pending.Context == nil ||
		pending.Context.TaskSummary == "" || pending.Context.AgentName == "" ||
		pending.Context.AgentColor == "" || pending.Context.AgentSymbol == "" {
		t.Fatalf("pending decision context = %+v", pending)
	}
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

func TestOverviewIncludesConfigurationHealthWithoutChangingWorkloadFields(t *testing.T) {
	root := t.TempDir()
	if _, _, err := scaffold.Init(root); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open("file:configuration-health-overview?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snapshot, err := scaffold.CaptureInputSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	sv := NewServer(nil, s)
	sv.SetConfigurationInputs(root, snapshot)
	response := sv.exec(Command{Op: "overview"})
	if !response.OK || response.Overview == nil || response.Overview.ConfigurationHealth == nil {
		t.Fatalf("overview = %+v", response)
	}
	if response.Overview.Building != 0 || response.Overview.NeedYou != 0 ||
		response.Overview.Queued != 0 || response.Overview.Failing != 0 ||
		response.Overview.ShippedToday != 0 || response.Overview.TokensTotal != 0 ||
		response.Overview.DollarsTotal != 0 {
		t.Fatalf("workload fields changed: %+v", response.Overview)
	}
	if response.Overview.ConfigurationHealth.Overall != scaffold.HealthCurrent {
		t.Fatalf("configuration health = %+v", response.Overview.ConfigurationHealth)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Response
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Overview == nil || decoded.Overview.ConfigurationHealth == nil ||
		decoded.Overview.ConfigurationHealth.DefaultsVersion != scaffold.DefaultsVersion {
		t.Fatalf("health JSON round trip = %+v", decoded)
	}
}

func TestSetupOutlineSharesConfigurationHealth(t *testing.T) {
	root := t.TempDir()
	if _, _, err := scaffold.Init(root); err != nil {
		t.Fatal(err)
	}
	f, err := flow.Load(filepath.Join(root, ".watchtower", "flows", "default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	packages, err := pkgs.LoadDir(filepath.Join(root, ".watchtower", "packages"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open("file:configuration-health-setup?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snapshot, err := scaffold.CaptureInputSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	sv := NewServer(nil, s)
	sv.SetFlows(map[string]flow.Flow{"default": f})
	sv.SetPackages(packages)
	sv.SetConfigurationInputs(root, snapshot)
	overview := sv.exec(Command{Op: "overview"})
	setup := sv.exec(Command{Op: "setup_outline"})
	if overview.Overview == nil || setup.Setup == nil || setup.Setup.ConfigurationHealth == nil {
		t.Fatalf("overview=%+v setup=%+v", overview, setup)
	}
	if !reflect.DeepEqual(overview.Overview.ConfigurationHealth, setup.Setup.ConfigurationHealth) {
		t.Fatalf("health differs between surfaces: overview=%+v setup=%+v",
			overview.Overview.ConfigurationHealth, setup.Setup.ConfigurationHealth)
	}
}

func TestReloadRequiredTracksLoadedSnapshot(t *testing.T) {
	root := t.TempDir()
	if _, _, err := scaffold.Init(root); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open("file:configuration-health-reload?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loaded, err := scaffold.CaptureInputSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	sv := NewServer(nil, s)
	sv.SetConfigurationInputs(root, loaded)
	config := filepath.Join(root, ".watchtower", "config.yaml")
	body, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, append(body, []byte("# changed\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	changed := sv.exec(Command{Op: "overview"})
	if changed.Overview == nil || changed.Overview.ConfigurationHealth == nil ||
		!changed.Overview.ConfigurationHealth.ReloadRequired {
		t.Fatalf("changed health = %+v", changed)
	}
	reloaded, err := scaffold.CaptureInputSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	sv.SetConfigurationInputs(root, reloaded)
	current := sv.exec(Command{Op: "overview"})
	if current.Overview == nil || current.Overview.ConfigurationHealth == nil ||
		current.Overview.ConfigurationHealth.ReloadRequired {
		t.Fatalf("reloaded health = %+v", current)
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
		Observers:          []func(core.Event){(&steward.Steward{Store: s}).Observe},
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

func TestTailReplaysHistoryLargerThanOneFrame(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp")
	s, err := store.Open(filepath.Join(t.TempDir(), "watchtower.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const eventCount = 80
	largeError := strings.Repeat("<oversized-history>", 2_048)
	for i := 0; i < eventCount; i++ {
		event, err := core.NewEvent(core.EvStageFailed, fmt.Sprintf("GH-%d", i+1), map[string]string{
			"error": largeError,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	history, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	unpaged, err := json.Marshal(Response{OK: true, Events: history})
	if err != nil {
		t.Fatal(err)
	}
	if len(unpaged) <= maxMessageBytes {
		t.Fatalf("test history is only %d bytes; want more than one %d-byte frame", len(unpaged), maxMessageBytes)
	}

	socket := filepath.Join(t.TempDir(), "watchtower.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil, s)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = listener.Close() })

	client, err := Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	response, err := client.Do(Command{Op: "tail", SinceSeq: 0})
	if err != nil {
		t.Fatalf("tail could not replay the durable history: %v", err)
	}
	if !response.OK {
		t.Fatalf("tail response: %+v", response)
	}
	if len(response.Events) != eventCount {
		t.Fatalf("tail returned %d events, want %d", len(response.Events), eventCount)
	}
	for i, event := range response.Events {
		wantSeq := int64(i + 1)
		if event.Seq != wantSeq {
			t.Fatalf("event %d has sequence %d, want %d", i, event.Seq, wantSeq)
		}
	}
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
			assertDecisionContext(t, pending.Decisions[0], "Review spec.md")
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
			assertDecisionContext(t, pending.Decisions[0], "Proceed?")
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

func TestRequeueAbandonedIssueProtocol(t *testing.T) {
	c := newTestClient(t)
	draft, err := c.Do(Command{Op: "draft_issue", Title: "t", Body: "b", Flow: "default", Preset: "regular", Priority: 2})
	if err != nil || !draft.OK {
		t.Fatalf("draft_issue: %v %+v", err, draft)
	}
	if response, err := c.Do(Command{Op: "abandon_issue", IssueID: draft.IssueID}); err != nil || !response.OK {
		t.Fatalf("abandon_issue: %v %+v", err, response)
	}
	response, err := c.Do(Command{Op: "requeue_issue", IssueID: draft.IssueID})
	if err != nil || !response.OK || response.IssueID != draft.IssueID {
		t.Fatalf("requeue_issue: %v %+v", err, response)
	}
	listed, _ := c.Do(Command{Op: "list_backlog"})
	found := false
	for _, item := range listed.Backlog {
		if item.Issue.ID == draft.IssueID && item.Issue.State == "backlog" {
			found = true
		}
	}
	if !found {
		t.Fatalf("requeued issue missing from backlog: %+v", listed.Backlog)
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

func TestListBacklogReportsActiveBlockersAndRawDependencies(t *testing.T) {
	st, err := store.Open("file:list-backlog-active-blockers?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fl := flow.Flow{Name: "default", Stages: []flow.Stage{{Name: "merge-verification"}}}
	eng := engine.New(engine.Config{
		Store: st, Runner: &runner.FakeRunner{}, Pool: slots.NewPool(1),
		Flows: map[string]flow.Flow{"default": fl}, DecisionIdentities: protoDecisionIdentities(fl), DataDir: t.TempDir(),
	})
	parentIDs := make([]string, 0, 5)
	for _, title := range []string{"merged", "cleanup", "preserved", "unknown", "missing"} {
		parent, err := eng.DraftIssue(title, "", "default", "regular", levers.Matrix{}, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		parentIDs = append(parentIDs, parent)
	}
	child, err := eng.DraftIssue("child", "", "default", "regular", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.SetDependencies(child, parentIDs); err != nil {
		t.Fatal(err)
	}
	for id, state := range map[string]string{
		parentIDs[0]: store.IntegrationMerged,
		parentIDs[1]: store.IntegrationCleanupNeeded,
		parentIDs[2]: store.IntegrationPreserved,
		parentIDs[3]: "mystery",
	} {
		if err := st.SetIssueIntegration(store.IssueIntegration{IssueID: id, State: state}); err != nil {
			t.Fatal(err)
		}
	}
	srv := NewServer(eng, st)
	assertBacklog := func(wantBlockers []string, wantClaimable bool) {
		t.Helper()
		response := srv.exec(Command{Op: "list_backlog"})
		if !response.OK {
			t.Fatalf("list_backlog = %+v", response)
		}
		var item *BacklogItem
		for i := range response.Backlog {
			if response.Backlog[i].Issue.ID == child {
				item = &response.Backlog[i]
				break
			}
		}
		if item == nil {
			t.Fatalf("child %s missing from backlog: %+v", child, response.Backlog)
		}
		if len(item.BlockedBy) != len(wantBlockers) || (len(wantBlockers) > 0 && !reflect.DeepEqual(item.BlockedBy, wantBlockers)) || item.Claimable != wantClaimable {
			t.Fatalf("child backlog item = %+v, want blockers %v claimable %v", *item, wantBlockers, wantClaimable)
		}
		if !reflect.DeepEqual(item.Issue.DependsOn, parentIDs) {
			t.Fatalf("raw dependencies = %v, want %v", item.Issue.DependsOn, parentIDs)
		}
	}
	assertBacklog([]string{parentIDs[2], parentIDs[3], parentIDs[4]}, false)

	for _, transition := range []struct {
		index int
		state string
		want  []string
	}{
		{2, store.IntegrationMerged, []string{parentIDs[3], parentIDs[4]}},
		{3, store.IntegrationMerged, []string{parentIDs[4]}},
		{4, store.IntegrationMerged, nil},
	} {
		if err := st.SetIssueIntegration(store.IssueIntegration{IssueID: parentIDs[transition.index], State: transition.state}); err != nil {
			t.Fatal(err)
		}
		assertBacklog(transition.want, len(transition.want) == 0)
	}
	if err := st.SetIssueIntegration(store.IssueIntegration{IssueID: parentIDs[2], State: store.IntegrationPreserved}); err != nil {
		t.Fatal(err)
	}
	assertBacklog([]string{parentIDs[2]}, false)
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

func TestOverviewDoesNotExposeUnpublishedDecision(t *testing.T) {
	fl := oneAgentFlow("agent")
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e := engine.New(engine.Config{
		Store: s, Runner: &runner.FakeRunner{}, Pool: slots.NewPool(1),
		Flows: map[string]flow.Flow{"default": fl}, DataDir: t.TempDir(),
	})
	if _, err := s.InsertDecision(store.DecisionRow{
		IssueID: "GH-1", Stage: "run", Question: "Ready?",
	}); err != nil {
		t.Fatal(err)
	}
	overview, err := NewServer(e, s).overview()
	if err != nil {
		t.Fatal(err)
	}
	if overview.NeedYou != 0 {
		t.Fatalf("unpublished decision counted in overview: %+v", overview)
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

func TestOverviewDoesNotCountParkedIssueAsBuilding(t *testing.T) {
	for _, eventType := range []core.EventType{core.EvIssuePaused, core.EvStageKilled} {
		t.Run(string(eventType), func(t *testing.T) {
			s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			if err := s.UpsertIssue(store.IssueRow{
				ID: "GH-26", Title: "review plan", State: "running", Flow: "default",
			}); err != nil {
				t.Fatal(err)
			}
			event, err := core.NewEvent(eventType, "GH-26", map[string]string{"stage": "plan"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Append(event); err != nil {
				t.Fatal(err)
			}

			overview, err := NewServer(nil, s).overview()
			if err != nil {
				t.Fatal(err)
			}
			if overview.Building != 0 || overview.Failing != 0 || overview.Queued != 0 {
				t.Fatalf("parked issue counted in overview: %+v", overview)
			}
		})
	}
}

func TestOverviewTreatsDurablePausedStateAsIdle(t *testing.T) {
	s, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.UpsertIssue(store.IssueRow{
		ID: "GH-36", Title: "paused", State: "paused", Flow: "default",
	}); err != nil {
		t.Fatal(err)
	}
	event, err := core.NewEvent(core.EvStageFailed, "GH-36", map[string]string{
		"stage": "plan", "error": "stale failure",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(event); err != nil {
		t.Fatal(err)
	}
	overview, err := NewServer(nil, s).overview()
	if err != nil {
		t.Fatal(err)
	}
	if overview.Building != 0 || overview.Failing != 0 || overview.Queued != 0 {
		t.Fatalf("paused issue counted in overview: %+v", overview)
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
	primary := &CodexProfileSetup{
		Label: "primary", Bin: "codex", Model: "gpt-5.6-luna", Effort: "xhigh",
		FeatureOverrides: map[string]bool{"unified_exec": false},
		InitialArgv:      []string{"exec", "--json", "-c", "features.unified_exec=false", "[redacted]"},
		ResumedArgv:      []string{"exec", "resume", "--json", "-c", "features.unified_exec=false", "[redacted]"},
	}
	fallback := &CodexProfileSetup{
		Label: "fallback", Bin: "codex", Model: "gpt-5.6-luna", Effort: "xhigh",
		FeatureOverrides: map[string]bool{"unified_exec": true},
		InitialArgv:      []string{"exec", "--json", "-c", "features.unified_exec=true", "[redacted]"},
		ResumedArgv:      []string{"exec", "resume", "--json", "-c", "features.unified_exec=true", "[redacted]"},
	}
	return RepoSetup{
		Runner: "codex", Slots: 4, CodexBin: "codex",
		CodexModel: "gpt-5.6-luna", CodexEffort: "xhigh",
		CodexPolicy: "fallback_once", CodexPrimary: primary, CodexFallback: fallback,
		ClaudeBin: "claude", Workspace: "treehouse", LoadedAt: "12:55",
	}
}

func TestCodexSetupReportsCachedRedactedProfiles(t *testing.T) {
	c, _ := newConfigClient(t, reviewFlow(), reviewPackages(), fixtureCodexRepoSetup())
	r, err := c.Do(Command{Op: "setup_outline"})
	if err != nil || r.Setup == nil || r.Setup.Repo.CodexPrimary == nil || r.Setup.Repo.CodexFallback == nil {
		t.Fatalf("setup profiles = %+v, err = %v", r.Setup, err)
	}
	if r.Setup.Repo.CodexPolicy != "fallback_once" || !strings.Contains(strings.Join(r.Setup.Repo.CodexPrimary.InitialArgv, " "), "features.unified_exec=false") {
		t.Fatalf("Codex setup = %+v", r.Setup.Repo)
	}
	for _, secret := range []string{"prompt secret", "developer instruction", "thread-secret", "environment-secret", "stderr-secret"} {
		encoded, _ := json.Marshal(r.Setup.Repo)
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("setup leaked %q", secret)
		}
	}
}

func TestIssueDetailReportsOnlyRedactedRunnerAttempts(t *testing.T) {
	c, s := newConfigClient(t, reviewFlow(), reviewPackages(), fixtureCodexRepoSetup())
	created, err := c.Do(Command{Op: "create_issue", Title: "runner attempt", Flow: "default", Preset: "regular"})
	if err != nil || !created.OK {
		t.Fatalf("create issue: %+v, err = %v", created, err)
	}
	if _, err := s.InsertStageRun(store.StageRun{IssueID: created.IssueID, Stage: "execute", Agent: "executor", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAttempt(context.Background(), runner.Attempt{
		OperationID: "1", IssueID: created.IssueID, Stage: "execute", AgentPackage: "executor",
		Kind: runner.AttemptPrimary, State: runner.AttemptFailed, FailureClass: runner.FailureExecution,
		RedactedArgv: []string{"exec", "--json", "[redacted-prompt]", "features.unified_exec=false"},
	}); err != nil {
		t.Fatal(err)
	}
	detail, err := c.Do(Command{Op: "issue_detail", IssueID: created.IssueID})
	if err != nil || !detail.OK || detail.Detail == nil || len(detail.Detail.Attempts) != 1 {
		t.Fatalf("issue detail: %+v, err = %v", detail, err)
	}
	if detail.Detail.Attempts[0].OperationID != "1" || detail.Detail.Attempts[0].RedactedArgv[2] != "[redacted-prompt]" {
		t.Fatalf("attempt evidence = %+v", detail.Detail.Attempts)
	}
	encoded, _ := json.Marshal(detail.Detail)
	for _, secret := range []string{"prompt secret", "developer instruction", "thread-secret", "environment-secret", "stderr-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("issue detail leaked %q", secret)
		}
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
