package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/slots"
	"github.com/weston6142/watchtower/internal/steward"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/workspace"
)

func newTestEngine(t *testing.T) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return newEngineOver(t, st), st
}

func newEngineOver(t *testing.T, st *store.Store) *Engine {
	t.Helper()
	sw := &steward.Steward{Store: st}
	return New(Config{
		Store:              st,
		Runner:             &runner.FakeRunner{Scripts: scripts()},
		Pool:               slots.NewPool(2),
		Flows:              capabilityTestFlows(map[string]flow.Flow{"default": testFlow()}),
		DataDir:            t.TempDir(),
		DecisionIdentities: testDecisionIdentities(),
		Observers:          []func(core.Event){sw.Observe},
	})
}

func issueRow(t *testing.T, st *store.Store, id string) store.IssueRow {
	t.Helper()
	rows, err := st.Issues()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("issue %s not found", id)
	return store.IssueRow{}
}

func hasEvent(t *testing.T, st *store.Store, issueID string, typ core.EventType) bool {
	t.Helper()
	events, err := st.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.IssueID == issueID && event.Type == typ {
			return true
		}
	}
	return false
}

func waitForEvent(t *testing.T, st *store.Store, issueID string, typ core.EventType) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hasEvent(t, st, issueID, typ) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("event %s for %s did not arrive", typ, issueID)
}

func waitForIssueState(t *testing.T, st *store.Store, issueID, state string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if issueRow(t, st, issueID).State == state {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("issue %s did not reach state %q: %+v", issueID, state, issueRow(t, st, issueID))
}

func useAutoLaunchFlow(e *Engine) {
	e.cfg.Flows = map[string]flow.Flow{
		"default": {
			Name: "default",
			Stages: []flow.Stage{{
				Name:              "run",
				CapabilityProfile: flow.ProfileArtifact,
				Agents:            []flow.AgentRef{{Package: "agent"}},
				Gate:              flow.GateAuto,
				Completion:        flow.CompletionAll,
			}},
		},
	}
	e.cfg.Runner = &runner.FakeRunner{Scripts: map[string]runner.Script{"run/agent": {}}}
}

func TestDraftIssueStaysInBacklog(t *testing.T) {
	e, st := newTestEngine(t)
	id, err := e.DraftIssue("t", "b", "default", "regular", levers.Matrix{}, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	row := issueRow(t, st, id)
	if row.State != "backlog" {
		t.Fatalf("state = %q, want backlog", row.State)
	}
	if row.Priority != 2 || row.Title != "t" {
		t.Fatalf("row fields not persisted: %+v", row)
	}
	if !hasEvent(t, st, id, core.EvIssueDrafted) {
		t.Fatal("no issue_drafted event")
	}
	runs, err := st.StageRuns(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("draft ran %d stages", len(runs))
	}
}

func TestLaunchWaitsForUnmergedDependencyWithoutRunningStages(t *testing.T) {
	e, st := newTestEngine(t)
	useAutoLaunchFlow(e)
	parent, _ := e.DraftIssue("parent", "", "default", "regular", levers.Matrix{}, 0, nil)
	child, _ := e.DraftIssue("child", "", "default", "regular", levers.Matrix{}, 0, nil)
	if err := e.SetDependencies(child, []string{parent}); err != nil {
		t.Fatal(err)
	}
	if err := e.LaunchIssue(child); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, st, child, core.EvIssueWaitingDependencies)
	if row := issueRow(t, st, child); row.State != "waiting_dependencies" {
		t.Fatalf("state = %q", row.State)
	}
	if runs, _ := st.StageRuns(child); len(runs) != 0 {
		t.Fatalf("waiting issue ran stages: %#v", runs)
	}
}

func TestDurableDependencyReadinessMatrix(t *testing.T) {
	cases := []struct {
		name        string
		checkpoint  string
		parentEvent core.EventType
		ready       bool
	}{
		{name: "done with merged proof", checkpoint: store.IntegrationMerged, parentEvent: core.EvIssueCompleted, ready: true},
		{name: "done without proof", parentEvent: core.EvIssueCompleted},
		{name: "cleanup needed", checkpoint: store.IntegrationCleanupNeeded, parentEvent: core.EvCleanupNeeded, ready: true},
		{name: "preserved", checkpoint: store.IntegrationPreserved, parentEvent: core.EvIssueCompleted},
		{name: "verification ready", checkpoint: store.IntegrationVerificationReady, parentEvent: core.EvIssueCompleted},
		{name: "unknown", checkpoint: "unknown", parentEvent: core.EvIssueCompleted},
		{name: "absent", parentEvent: core.EvIssueCompleted},
	}

	for _, tc := range cases {
		t.Run(tc.name+" launch", func(t *testing.T) {
			e, st := newTestEngine(t)
			useAutoLaunchFlow(e)
			parent, _ := e.DraftIssue("parent", "", "default", "regular", levers.Matrix{}, 0, nil)
			child, _ := e.DraftIssue("child", "", "default", "regular", levers.Matrix{}, 0, nil)
			if err := e.SetDependencies(child, []string{parent}); err != nil {
				t.Fatal(err)
			}
			if tc.checkpoint != "" {
				if err := st.SetIssueIntegration(store.IssueIntegration{IssueID: parent, State: tc.checkpoint}); err != nil {
					t.Fatal(err)
				}
			}
			e.emit(tc.parentEvent, parent, nil)

			if err := e.LaunchIssue(child); err != nil {
				t.Fatal(err)
			}
			if tc.ready {
				waitForEvent(t, st, child, core.EvIssueCompleted)
				if runs, _ := st.StageRuns(child); len(runs) == 0 {
					t.Fatal("ready dependency ran no stages")
				}
				return
			}
			waitForEvent(t, st, child, core.EvIssueWaitingDependencies)
			if row := issueRow(t, st, child); row.State != "waiting_dependencies" {
				t.Fatalf("state = %q, want waiting_dependencies", row.State)
			}
			if runs, _ := st.StageRuns(child); len(runs) != 0 {
				t.Fatalf("waiting issue ran stages: %#v", runs)
			}
		})

		t.Run(tc.name+" claim blockers", func(t *testing.T) {
			e, st, _, _ := newClaimTestEngine(t)
			parent, _ := e.DraftIssue("parent", "", "default", "regular", levers.Matrix{}, 0, nil)
			child, _ := e.DraftIssue("child", "", "default", "regular", levers.Matrix{}, 0, nil)
			if err := e.SetDependencies(child, []string{parent}); err != nil {
				t.Fatal(err)
			}
			if tc.checkpoint != "" {
				if err := st.SetIssueIntegration(store.IssueIntegration{IssueID: parent, State: tc.checkpoint}); err != nil {
					t.Fatal(err)
				}
			}
			e.emit(tc.parentEvent, parent, nil)

			got, err := e.ClaimBlockers(child)
			if err != nil {
				t.Fatal(err)
			}
			if tc.ready {
				if len(got) != 0 {
					t.Fatalf("ClaimBlockers = %v, want no blockers", got)
				}
				return
			}
			if !reflect.DeepEqual(got, []string{parent}) {
				t.Fatalf("ClaimBlockers = %v, want [%s]", got, parent)
			}
		})

		t.Run(tc.name+" wake", func(t *testing.T) {
			e, st := newTestEngine(t)
			useAutoLaunchFlow(e)
			parent, _ := e.DraftIssue("parent", "", "default", "regular", levers.Matrix{}, 0, nil)
			child, _ := e.DraftIssue("child", "", "default", "regular", levers.Matrix{}, 0, nil)
			if err := e.SetDependencies(child, []string{parent}); err != nil {
				t.Fatal(err)
			}
			if err := e.LaunchIssue(child); err != nil {
				t.Fatal(err)
			}
			waitForEvent(t, st, child, core.EvIssueWaitingDependencies)
			if tc.checkpoint != "" {
				if err := st.SetIssueIntegration(store.IssueIntegration{IssueID: parent, State: tc.checkpoint}); err != nil {
					t.Fatal(err)
				}
			}
			e.emit(tc.parentEvent, parent, nil)
			if err := e.wakeDependents(context.Background(), parent); err != nil {
				t.Fatal(err)
			}

			if tc.ready {
				waitForEvent(t, st, child, core.EvIssueDependenciesSatisfied)
				waitForEvent(t, st, child, core.EvIssueCompleted)
				if runs, _ := st.StageRuns(child); len(runs) == 0 {
					t.Fatal("ready dependency ran no stages")
				}
				return
			}
			if hasEvent(t, st, child, core.EvIssueDependenciesSatisfied) {
				t.Fatal("unready dependency released waiting issue")
			}
			if row := issueRow(t, st, child); row.State != "waiting_dependencies" {
				t.Fatalf("state = %q, want waiting_dependencies", row.State)
			}
			if runs, _ := st.StageRuns(child); len(runs) != 0 {
				t.Fatalf("waiting issue ran stages: %#v", runs)
			}
		})
	}
}

func TestLaunchPropagatesDurableLookupFailure(t *testing.T) {
	e, st := newTestEngine(t)
	useAutoLaunchFlow(e)
	parent, _ := e.DraftIssue("parent", "", "default", "regular", levers.Matrix{}, 0, nil)
	child, _ := e.DraftIssue("child", "", "default", "regular", levers.Matrix{}, 0, nil)
	if err := e.SetDependencies(child, []string{parent}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("integration store unavailable")
	e.dependencyReadiness = durableDependencyReadiness{source: failingIntegrationSource{err: wantErr}}
	if err := e.LaunchIssue(child); !errors.Is(err, wantErr) || !strings.Contains(err.Error(), parent) {
		t.Fatalf("LaunchIssue error = %v, want parent context and %v", err, wantErr)
	}
	if row := issueRow(t, st, child); row.State != "backlog" {
		t.Fatalf("failed launch changed state to %q", row.State)
	}
	if hasEvent(t, st, child, core.EvIssueCreated) {
		t.Fatal("failed durable lookup committed a launch")
	}
	if runs, _ := st.StageRuns(child); len(runs) != 0 {
		t.Fatalf("failed launch ran stages: %#v", runs)
	}
}

func TestMergedDependencyWakesWaitingIssue(t *testing.T) {
	e, st := newTestEngine(t)
	useAutoLaunchFlow(e)
	parent, _ := e.DraftIssue("parent", "", "default", "regular", levers.Matrix{}, 0, nil)
	child, _ := e.DraftIssue("child", "", "default", "regular", levers.Matrix{}, 0, nil)
	if err := e.SetDependencies(child, []string{parent}); err != nil {
		t.Fatal(err)
	}
	if err := e.LaunchIssue(child); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, st, child, core.EvIssueWaitingDependencies)
	if err := st.SetIssueIntegration(store.IssueIntegration{
		IssueID: parent, State: store.IntegrationMerged, LandedSHA: "merged",
	}); err != nil {
		t.Fatal(err)
	}
	e.emit(core.EvIssueMerged, parent, nil)
	if err := e.wakeDependents(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, st, child, core.EvIssueDependenciesSatisfied)
	waitForEvent(t, st, child, core.EvIssueCompleted)
}

type failingIntegrationSource struct {
	err error
}

func (f failingIntegrationSource) IssueIntegration(string) (store.IssueIntegration, bool, error) {
	return store.IssueIntegration{}, false, f.err
}

type failOnIntegrationRead struct {
	store  *store.Store
	failAt int
	calls  int
	err    error
}

func (f *failOnIntegrationRead) IssueIntegration(issueID string) (store.IssueIntegration, bool, error) {
	f.calls++
	if f.calls == f.failAt {
		return store.IssueIntegration{}, false, f.err
	}
	return f.store.IssueIntegration(issueID)
}

func TestFinalizationPropagatesWakeFailureAfterCleanupCheckpoint(t *testing.T) {
	e, st, repo := verificationEngine(t, "merge", [][]string{{"true"}}, "")
	e.cfg.Observers = []func(core.Event){(&steward.Steward{Store: st}).Observe}
	e.cfg.Workspace = &failOnceReleaseWorkspace{delegate: workspace.GitWorktree{Repo: repo}}
	parent, err := e.CreateIssue("parent", "", "default", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	child, err := e.DraftIssue("child", "", "default", "regular", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SetDependencies(child, []string{parent}); err != nil {
		t.Fatal(err)
	}
	if err := e.LaunchIssue(child); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, st, child, core.EvIssueWaitingDependencies)

	wantErr := errors.New("integration store unavailable during wake")
	e.dependencyReadiness = durableDependencyReadiness{source: &failOnIntegrationRead{
		store: st, failAt: 1, err: wantErr,
	}}
	err = e.StartIssue(context.Background(), parent)
	if !errors.Is(err, wantErr) {
		t.Fatalf("StartIssue error = %v, want wake failure %v", err, wantErr)
	}
	integration, ok, readErr := st.IssueIntegration(parent)
	if readErr != nil || !ok || integration.State != store.IntegrationCleanupNeeded {
		t.Fatalf("parent integration = %+v ok=%v err=%v, want durable cleanup_needed", integration, ok, readErr)
	}
	if row := issueRow(t, st, child); row.State != "waiting_dependencies" {
		t.Fatalf("failed wake changed child state to %q", row.State)
	}
	if hasEvent(t, st, child, core.EvIssueDependenciesSatisfied) {
		t.Fatal("failed wake released the dependent")
	}
}

type staleMergeOnReleaseWorkspace struct {
	delegate workspace.GitWorktree
	store    *store.Store
}

func (w *staleMergeOnReleaseWorkspace) Acquire(issueID string) (string, func() error, error) {
	path, _, err := w.delegate.Acquire(issueID)
	if err != nil {
		return "", nil, err
	}
	return path, func() error {
		if err := w.store.SetIssueIntegration(store.IssueIntegration{
			IssueID: issueID, State: store.IntegrationMerged,
		}); err != nil {
			return err
		}
		w.store.FailIssueIntegrationWriteAfterForTest(0)
		return errors.New("injected cleanup checkpoint failure")
	}, nil
}

func (w *staleMergeOnReleaseWorkspace) ReleasePath(path string) error {
	return w.delegate.ReleasePath(path)
}

func (w *staleMergeOnReleaseWorkspace) Name() string { return "stale merge on release" }

func TestFinalizationDoesNotWakeAfterCheckpointWriteFailure(t *testing.T) {
	e, st, repo := verificationEngine(t, "merge", [][]string{{"true"}}, "")
	e.cfg.Observers = []func(core.Event){(&steward.Steward{Store: st}).Observe}
	parent, err := e.CreateIssue("parent", "", "default", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	child, err := e.DraftIssue("child", "", "default", "regular", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SetDependencies(child, []string{parent}); err != nil {
		t.Fatal(err)
	}
	if err := e.LaunchIssue(child); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, st, child, core.EvIssueWaitingDependencies)

	e.cfg.Workspace = &staleMergeOnReleaseWorkspace{
		delegate: workspace.GitWorktree{Repo: repo}, store: st,
	}
	err = e.StartIssue(context.Background(), parent)
	if err == nil || !strings.Contains(err.Error(), "injected integration persistence failure") {
		t.Fatalf("StartIssue error = %v, want checkpoint persistence failure", err)
	}
	integration, ok, readErr := st.IssueIntegration(parent)
	if readErr != nil || !ok || integration.State != store.IntegrationMerged {
		t.Fatalf("parent integration = %+v ok=%v err=%v, want stale merged row", integration, ok, readErr)
	}
	if row := issueRow(t, st, child); row.State != "waiting_dependencies" {
		t.Fatalf("failed checkpoint write changed child state to %q", row.State)
	}
	if hasEvent(t, st, child, core.EvIssueDependenciesSatisfied) {
		t.Fatal("failed checkpoint write released the dependent")
	}
}

func TestWakeDependentsPropagatesDurableLookupFailure(t *testing.T) {
	e, st := newTestEngine(t)
	useAutoLaunchFlow(e)
	parent, _ := e.DraftIssue("parent", "", "default", "regular", levers.Matrix{}, 0, nil)
	child, _ := e.DraftIssue("child", "", "default", "regular", levers.Matrix{}, 0, nil)
	if err := e.SetDependencies(child, []string{parent}); err != nil {
		t.Fatal(err)
	}
	if err := e.LaunchIssue(child); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, st, child, core.EvIssueWaitingDependencies)

	wantErr := errors.New("integration store unavailable")
	e.dependencyReadiness = durableDependencyReadiness{source: failingIntegrationSource{err: wantErr}}
	err := e.wakeDependents(context.Background(), parent)
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), parent) {
		t.Fatalf("wakeDependents error = %v, want parent context and %v", err, wantErr)
	}
	if row := issueRow(t, st, child); row.State != "waiting_dependencies" {
		t.Fatalf("failed wake changed state to %q", row.State)
	}
	if hasEvent(t, st, child, core.EvIssueDependenciesSatisfied) {
		t.Fatal("failed durable lookup released the dependent")
	}
	if runs, _ := st.StageRuns(child); len(runs) != 0 {
		t.Fatalf("failed wake ran stages: %#v", runs)
	}
}

func TestRehydratePreservesDependencyWait(t *testing.T) {
	e, st := newTestEngine(t)
	useAutoLaunchFlow(e)
	parent, _ := e.DraftIssue("parent", "", "default", "regular", levers.Matrix{}, 0, nil)
	child, _ := e.DraftIssue("child", "", "default", "regular", levers.Matrix{}, 0, nil)
	if err := e.SetDependencies(child, []string{parent}); err != nil {
		t.Fatal(err)
	}
	if err := e.LaunchIssue(child); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, st, child, core.EvIssueWaitingDependencies)

	restarted := newEngineOver(t, st)
	useAutoLaunchFlow(restarted)
	if err := restarted.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if runs, _ := st.StageRuns(child); len(runs) != 0 {
		t.Fatalf("rehydrate started waiting issue: %#v", runs)
	}
	if state := restarted.issues[child]; state == nil || !state.waitingDependencies {
		t.Fatalf("waiting state not rehydrated: %#v", state)
	}
}

func TestCanResetRefusesActiveExecutionAndPendingDecision(t *testing.T) {
	t.Run("active execution", func(t *testing.T) {
		e, _ := newTestEngine(t)
		useAutoLaunchFlow(e)
		started := make(chan struct{})
		release := make(chan struct{})
		e.cfg.Runner.(*runner.FakeRunner).OnStart = func(_, _, _, _ string) error {
			close(started)
			<-release
			return nil
		}
		id, _ := e.CreateIssue("active", "", "default", levers.Matrix{}, 0, nil)
		done := make(chan error, 1)
		go func() { done <- e.StartIssue(context.Background(), id) }()
		<-started
		if err := e.CanReset(); err == nil || !strings.Contains(err.Error(), "running") {
			t.Fatalf("CanReset = %v", err)
		}
		close(release)
		<-done
	})

	t.Run("pending decision", func(t *testing.T) {
		e, _ := newTestEngine(t)
		scripts := map[string]runner.Script{"ask/agent": {Asks: []levers.Decision{{
			Question: "approve?", Options: []string{"yes", "no"}, Recommended: 0,
			Importance: 1.0,
		}}}}
		e.cfg.Flows = map[string]flow.Flow{"default": {
			Name: "default", Stages: []flow.Stage{{
				Name: "ask", Agents: []flow.AgentRef{{Package: "agent"}}, Gate: flow.GateAuto, CapabilityProfile: flow.ProfileArtifact,
			}},
		}}
		e.cfg.Runner = &runner.FakeRunner{Scripts: scripts}
		id, _ := e.CreateIssue("pending", "", "default", levers.Matrix{}, 0, nil)
		done := make(chan error, 1)
		go func() { done <- e.StartIssue(context.Background(), id) }()
		deadline := time.Now().Add(2 * time.Second)
		for len(e.PendingDecisions()) == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if err := e.CanReset(); err == nil || !strings.Contains(err.Error(), "pending decision") {
			t.Fatalf("CanReset = %v", err)
		}
		decision := e.PendingDecisions()[0]
		if err := e.Answer(decision.ID, levers.ChoiceResponse(0)); err != nil {
			t.Fatal(err)
		}
		<-done
	})
}

func TestCanResetAllowsInactiveIssueStates(t *testing.T) {
	t.Run("backlog", func(t *testing.T) {
		e, _ := newTestEngine(t)
		if _, err := e.DraftIssue("draft", "", "default", "regular", levers.Matrix{}, 0, nil); err != nil {
			t.Fatal(err)
		}
		if err := e.CanReset(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("dependency waiting", func(t *testing.T) {
		e, _ := newTestEngine(t)
		useAutoLaunchFlow(e)
		parent, _ := e.DraftIssue("parent", "", "default", "regular", levers.Matrix{}, 0, nil)
		child, _ := e.DraftIssue("child", "", "default", "regular", levers.Matrix{}, 0, nil)
		if err := e.SetDependencies(child, []string{parent}); err != nil {
			t.Fatal(err)
		}
		if err := e.LaunchIssue(child); err != nil {
			t.Fatal(err)
		}
		waitForEvent(t, e.cfg.Store, child, core.EvIssueWaitingDependencies)
		if err := e.CanReset(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("completed and held", func(t *testing.T) {
		e, _ := newTestEngine(t)
		useAutoLaunchFlow(e)
		completed, _ := e.CreateIssue("done", "", "default", levers.Matrix{}, 0, nil)
		if err := e.StartIssue(context.Background(), completed); err != nil {
			t.Fatal(err)
		}
		e.cfg.Runner = &runner.FakeRunner{Scripts: map[string]runner.Script{
			"run/agent": {Fail: true},
		}}
		held, _ := e.CreateIssue("held", "", "default", levers.Matrix{}, 0, nil)
		if err := e.StartIssue(context.Background(), held); err == nil {
			t.Fatal("held fixture succeeded")
		}
		if err := e.CanReset(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestUpdateIssueOnlyLegalFromBacklog(t *testing.T) {
	e, st := newTestEngine(t)
	id, _ := e.DraftIssue("t", "b", "default", "regular", levers.Matrix{}, 0, nil)
	if err := e.UpdateIssue(id, "t2", "b2", "default", "strict", levers.Matrix{}, 5, nil); err != nil {
		t.Fatal(err)
	}
	row := issueRow(t, st, id)
	if row.Title != "t2" || row.Priority != 5 || row.State != "backlog" {
		t.Fatalf("update not persisted: %+v", row)
	}
	if !hasEvent(t, st, id, core.EvIssueUpdated) {
		t.Fatal("no issue_updated event")
	}
	rid, _ := e.CreateIssue("r", "", "default", levers.Matrix{}, 0, nil)
	if err := e.UpdateIssue(rid, "x", "", "default", "regular", levers.Matrix{}, 0, nil); err == nil {
		t.Fatal("update of non-draft succeeded")
	}
	if err := e.UpdateIssue("GH-999", "x", "", "default", "regular", levers.Matrix{}, 0, nil); err == nil {
		t.Fatal("update of unknown issue succeeded")
	}
}

func TestAbandonDraft(t *testing.T) {
	e, st := newTestEngine(t)
	id, _ := e.DraftIssue("t", "b", "default", "regular", levers.Matrix{}, 0, nil)
	if err := e.Abandon(id); err != nil {
		t.Fatal(err)
	}
	if issueRow(t, st, id).State != "abandoned" {
		t.Fatal("draft not abandoned")
	}
	if !hasEvent(t, st, id, core.EvIssueAbandoned) {
		t.Fatal("no issue_abandoned event")
	}
}

func TestRequeueAbandonedIssueReturnsSameIssueToBacklog(t *testing.T) {
	e, st := newTestEngine(t)
	parent, _ := e.DraftIssue("parent", "", "default", "regular", levers.Matrix{}, 0, nil)
	id, _ := e.DraftIssueWithDependencies(
		"t", "b", "default", "regular", levers.Matrix{"impl": "strict"}, 2, nil, []string{parent})
	if err := e.Abandon(id); err != nil {
		t.Fatal(err)
	}

	if err := e.RequeueIssue(id); err != nil {
		t.Fatal(err)
	}
	row := issueRow(t, st, id)
	if row.State != "backlog" || row.Title != "t" || row.Body != "b" || row.Priority != 2 {
		t.Fatalf("requeued row = %+v", row)
	}
	if len(row.DependsOn) != 1 || row.DependsOn[0] != parent {
		t.Fatalf("requeued dependencies = %v", row.DependsOn)
	}
	if err := e.RequeueIssue(id); err == nil {
		t.Fatal("second requeue succeeded")
	}
}

func TestRehydrateKeepsDraftsInert(t *testing.T) {
	e, st := newTestEngine(t)
	id, _ := e.DraftIssue("t", "b", "default", "regular", levers.Matrix{"impl": "yolo"}, 3, nil)
	e2 := newEngineOver(t, st)
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if hasEvent(t, st, id, core.EvStageFailed) {
		t.Fatal("rehydrate marked draft failed")
	}
	if err := e2.UpdateIssue(id, "t2", "b", "default", "regular", levers.Matrix{}, 3, nil); err != nil {
		t.Fatal(err)
	}
	nid, _ := e2.CreateIssue("n", "", "default", levers.Matrix{}, 0, nil)
	if nid == id {
		t.Fatal("id collision after rehydrate")
	}
}

func newClaimTestEngine(t *testing.T) (*Engine, *store.Store, string, *countingGitWorktree) {
	t.Helper()
	repo := t.TempDir()
	initGitRepo(t, repo)
	st, err := store.Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ws := &countingGitWorktree{repo: repo}
	eng := New(Config{
		Store: st, Runner: &runner.FakeRunner{Scripts: scripts()}, Pool: slots.NewPool(2),
		Flows: map[string]flow.Flow{"default": testFlow()}, DataDir: t.TempDir(),
		Workspace: ws, Train: &marshal.Train{Repo: repo},
		Observers: []func(core.Event){(&steward.Steward{Store: st}).Observe},
	})
	return eng, st, repo, ws
}

func TestClaimIssueCreatesOneDurableIsolatedWorkspace(t *testing.T) {
	e, st, repo, ws := newClaimTestEngine(t)
	id, err := e.DraftIssue("explore", "details", "default", "regular", levers.Matrix{}, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := e.ClaimIssue(id)
	if err != nil {
		t.Fatal(err)
	}
	if claim.IssueID != id || claim.Repository != repo || claim.Branch != "issue/"+id ||
		claim.Worktree == "" || claim.BaseSHA == "" {
		t.Fatalf("claim = %+v", claim)
	}
	if _, err := os.Stat(claim.Worktree); err != nil {
		t.Fatalf("claimed worktree: %v", err)
	}
	if row := issueRow(t, st, id); row.State != "claimed" {
		t.Fatalf("state = %q", row.State)
	}
	if runs, _ := st.StageRuns(id); len(runs) != 0 {
		t.Fatalf("claim started stages: %+v", runs)
	}
	again, err := e.ClaimIssue(id)
	if err != nil || again != claim || ws.acquired != 1 {
		t.Fatalf("idempotent claim = %+v err=%v acquired=%d", again, err, ws.acquired)
	}
}

func TestClaimIssueRequiresDurablyMergedDependencies(t *testing.T) {
	e, st, _, _ := newClaimTestEngine(t)
	parent, _ := e.DraftIssue("parent", "", "default", "regular", levers.Matrix{}, 0, nil)
	child, _ := e.DraftIssue("child", "", "default", "regular", levers.Matrix{}, 0, nil)
	if err := e.SetDependencies(child, []string{parent}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ClaimIssue(child); err == nil || !strings.Contains(err.Error(), parent) {
		t.Fatalf("blocked claim error = %v", err)
	}
	if row := issueRow(t, st, child); row.State != "backlog" {
		t.Fatalf("blocked claim changed state to %q", row.State)
	}
	if err := st.SetIssueIntegration(store.IssueIntegration{
		IssueID: parent, State: store.IntegrationMerged, LandedSHA: "merged",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ClaimIssue(child); err != nil {
		t.Fatalf("claim after durable merge: %v", err)
	}
}

func TestClaimBlockersTracksCurrentIntegrationStates(t *testing.T) {
	e, st, _, _ := newClaimTestEngine(t)
	child, _ := e.DraftIssue("child", "", "default", "regular", levers.Matrix{}, 0, nil)
	parents := make([]string, 0, 7)
	for _, title := range []string{"merged", "cleanup", "preserved", "verification", "unknown", "missing", "done"} {
		parent, err := e.DraftIssue(title, "", "default", "regular", levers.Matrix{}, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		parents = append(parents, parent)
	}
	if err := e.SetDependencies(child, parents); err != nil {
		t.Fatal(err)
	}
	for id, state := range map[string]string{
		parents[0]: store.IntegrationMerged,
		parents[1]: store.IntegrationCleanupNeeded,
		parents[2]: store.IntegrationPreserved,
		parents[3]: store.IntegrationVerificationReady,
		parents[4]: "mystery",
	} {
		if err := st.SetIssueIntegration(store.IssueIntegration{IssueID: id, State: state}); err != nil {
			t.Fatal(err)
		}
	}
	assertBlockers := func(want ...string) {
		t.Helper()
		got, err := e.ClaimBlockers(child)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) || (len(want) > 0 && !reflect.DeepEqual(got, want)) {
			t.Fatalf("ClaimBlockers = %v, want %v", got, want)
		}
	}
	e.emit(core.EvIssueCompleted, parents[6], nil)
	assertBlockers(parents[2], parents[3], parents[4], parents[5], parents[6])

	if err := st.SetIssueIntegration(store.IssueIntegration{IssueID: parents[2], State: store.IntegrationMerged}); err != nil {
		t.Fatal(err)
	}
	assertBlockers(parents[3], parents[4], parents[5], parents[6])
	if err := st.SetIssueIntegration(store.IssueIntegration{IssueID: parents[3], State: store.IntegrationMerged}); err != nil {
		t.Fatal(err)
	}
	assertBlockers(parents[4], parents[5], parents[6])
	if err := st.SetIssueIntegration(store.IssueIntegration{IssueID: parents[4], State: store.IntegrationMerged}); err != nil {
		t.Fatal(err)
	}
	assertBlockers(parents[5], parents[6])
	if err := st.SetIssueIntegration(store.IssueIntegration{IssueID: parents[5], State: store.IntegrationMerged}); err != nil {
		t.Fatal(err)
	}
	assertBlockers(parents[6])
	if err := st.SetIssueIntegration(store.IssueIntegration{IssueID: parents[6], State: store.IntegrationMerged}); err != nil {
		t.Fatal(err)
	}
	assertBlockers()

	if err := st.SetIssueIntegration(store.IssueIntegration{IssueID: parents[0], State: store.IntegrationPreserved}); err != nil {
		t.Fatal(err)
	}
	assertBlockers(parents[0])
}

type failingClaimWorkspace struct{}

func (failingClaimWorkspace) Acquire(string) (string, func() error, error) {
	return "", nil, os.ErrPermission
}

func (failingClaimWorkspace) Name() string { return "failing" }

func TestClaimWorkspaceFailureLeavesIssueInBacklog(t *testing.T) {
	e, st, _, _ := newClaimTestEngine(t)
	e.cfg.Workspace = failingClaimWorkspace{}
	id, _ := e.DraftIssue("explore", "", "default", "regular", levers.Matrix{}, 0, nil)
	if _, err := e.ClaimIssue(id); err == nil {
		t.Fatal("claim with failing workspace succeeded")
	}
	if row := issueRow(t, st, id); row.State != "backlog" {
		t.Fatalf("failed claim state = %q", row.State)
	}
	if _, ok, err := st.IssueIntegration(id); err != nil || ok {
		t.Fatalf("failed claim persisted integration: ok=%v err=%v", ok, err)
	}
}

func TestReleaseClaimRefusesToDiscardWorkAndRehydrateResumes(t *testing.T) {
	e, st, _, ws := newClaimTestEngine(t)
	id, _ := e.DraftIssue("explore", "", "default", "regular", levers.Matrix{}, 0, nil)
	claim, err := e.ClaimIssue(id)
	if err != nil {
		t.Fatal(err)
	}
	restarted := New(e.cfg)
	if err := restarted.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	claims, err := restarted.Claims()
	if err != nil || len(claims) != 1 || claims[0] != claim || ws.acquired != 1 {
		t.Fatalf("rehydrated claims=%+v err=%v acquired=%d", claims, err, ws.acquired)
	}
	changed := filepath.Join(claim.Worktree, "notes.txt")
	if err := os.WriteFile(changed, []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ReleaseClaim(id); err == nil {
		t.Fatal("release discarded uncommitted work")
	}
	if err := os.Remove(changed); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ReleaseClaim(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(claim.Worktree); !os.IsNotExist(err) {
		t.Fatalf("released worktree still exists: %v", err)
	}
	if row := issueRow(t, st, id); row.State != "backlog" {
		t.Fatalf("released state = %q", row.State)
	}
	if _, ok, err := st.IssueIntegration(id); err != nil || ok {
		t.Fatalf("claim record remains: ok=%v err=%v", ok, err)
	}
}

func TestReleaseClaimRefusesCommittedWork(t *testing.T) {
	e, _, _, _ := newClaimTestEngine(t)
	id, _ := e.DraftIssue("explore", "", "default", "regular", levers.Matrix{}, 0, nil)
	claim, err := e.ClaimIssue(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claim.Worktree, "result.txt"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, claim.Worktree, "add", "result.txt")
	gitOutput(t, claim.Worktree, "commit", "-qm", "result")
	if err := e.ReleaseClaim(id); err == nil {
		t.Fatal("release discarded committed work")
	}
}

func prepareFinishClaim(t *testing.T) (*Engine, *store.Store, Claim) {
	t.Helper()
	e, st, _, _ := newClaimTestEngine(t)
	f := verificationFlow()
	// FinishClaim exercises an externally prepared branch with no prior plan
	// review. Keep this synthetic verifier artifact-scoped so the test remains
	// about claim finalization rather than approval-bound repair authority.
	f.Stages[len(f.Stages)-1].CapabilityProfile = flow.ProfileArtifact
	e.cfg.Flows = map[string]flow.Flow{"default": f}
	e.cfg.Runner = &runner.FakeRunner{Scripts: map[string]runner.Script{
		"merge-verification/merge-verifier": {Artifacts: map[string]string{
			"merge-report.md": "", "merge-decision.json": "",
		}},
	}}
	e.cfg.Train.TestCmd = []string{"true"}
	id, _ := e.DraftIssue("explore", "", "default", "regular", levers.Matrix{}, 0, nil)
	claim, err := e.ClaimIssue(id)
	if err != nil {
		t.Fatal(err)
	}
	return e, st, claim
}

func TestFinishClaimRejectsMismatchedDirtyAndNoChangeWorktrees(t *testing.T) {
	t.Run("mismatched path", func(t *testing.T) {
		e, _, claim := prepareFinishClaim(t)
		err := e.FinishClaim(FinishClaimRequest{IssueID: claim.IssueID, Worktree: t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("FinishClaim mismatch = %v", err)
		}
	})
	t.Run("dirty", func(t *testing.T) {
		e, _, claim := prepareFinishClaim(t)
		if err := os.WriteFile(filepath.Join(claim.Worktree, "notes.txt"), []byte("work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := e.FinishClaim(FinishClaimRequest{IssueID: claim.IssueID, Worktree: claim.Worktree})
		if err == nil || !strings.Contains(err.Error(), "uncommitted") {
			t.Fatalf("FinishClaim dirty = %v", err)
		}
	})
	t.Run("no change", func(t *testing.T) {
		e, _, claim := prepareFinishClaim(t)
		err := e.FinishClaim(FinishClaimRequest{IssueID: claim.IssueID, Worktree: claim.Worktree})
		if err == nil || !strings.Contains(err.Error(), "no commits beyond") {
			t.Fatalf("FinishClaim no-change = %v", err)
		}
	})
	t.Run("canonical path alias", func(t *testing.T) {
		e, _, claim := prepareFinishClaim(t)
		alias := filepath.Join(t.TempDir(), "claim-link")
		if err := os.Symlink(claim.Worktree, alias); err != nil {
			t.Fatal(err)
		}
		resolved, err := e.ClaimForWorktree(alias)
		if err != nil || resolved.IssueID != claim.IssueID {
			t.Fatalf("ClaimForWorktree(alias) = %+v, %v", resolved, err)
		}
		err = e.FinishClaim(FinishClaimRequest{IssueID: claim.IssueID, Worktree: alias})
		if err == nil || !strings.Contains(err.Error(), "no commits beyond") {
			t.Fatalf("FinishClaim alias = %v", err)
		}
	})
}

func TestFinishClaimRunsOnlyMergeVerificationAndCompletes(t *testing.T) {
	e, st, claim := prepareFinishClaim(t)
	if err := os.WriteFile(filepath.Join(claim.Worktree, "result.txt"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, claim.Worktree, "add", "result.txt")
	gitOutput(t, claim.Worktree, "commit", "-qm", "result")
	if err := e.FinishClaim(FinishClaimRequest{
		IssueID: claim.IssueID, Worktree: claim.Worktree,
	}); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, st, claim.IssueID, core.EvIssueCompleted)
	events, err := st.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var stages []string
	for _, event := range events {
		if event.IssueID != claim.IssueID || event.Type != core.EvStageStarted {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		stages = append(stages, payload["stage"].(string))
	}
	if len(stages) != 1 || stages[0] != "merge-verification" {
		t.Fatalf("started stages = %v", stages)
	}
	waitForIssueState(t, st, claim.IssueID, "done")
}

func TestFinishClaimVerifierFailurePreservesWorkspaceForRetry(t *testing.T) {
	e, st, claim := prepareFinishClaim(t)
	if err := os.WriteFile(filepath.Join(claim.Worktree, "result.txt"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, claim.Worktree, "add", "result.txt")
	gitOutput(t, claim.Worktree, "commit", "-qm", "result")
	fake := e.cfg.Runner.(*runner.FakeRunner)
	script := fake.Scripts["merge-verification/merge-verifier"]
	script.Fail = true
	fake.Scripts["merge-verification/merge-verifier"] = script
	var worktrees []string
	fake.OnStart = func(_, _, _, worktree string) error {
		worktrees = append(worktrees, worktree)
		return nil
	}

	if err := e.FinishClaim(FinishClaimRequest{
		IssueID: claim.IssueID, Worktree: claim.Worktree,
	}); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, st, claim.IssueID, core.EvStageFailed)
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.mu.Lock()
		terminal := e.issues[claim.IssueID].terminal
		e.mu.Unlock()
		if terminal {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed verifier did not become retryable")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(claim.Worktree); err != nil {
		t.Fatalf("failed verifier removed claimed worktree: %v", err)
	}
	integration, ok, err := st.IssueIntegration(claim.IssueID)
	if err != nil || !ok || integration.State != store.IntegrationClaimed {
		t.Fatalf("claim checkpoint after verifier failure = %+v, %v, %v", integration, ok, err)
	}

	script.Fail = false
	fake.Scripts["merge-verification/merge-verifier"] = script
	if err := e.RetryStage(context.Background(), claim.IssueID); err != nil {
		t.Fatal(err)
	}
	if len(worktrees) != 2 || worktrees[0] != claim.Worktree || worktrees[1] != claim.Worktree {
		t.Fatalf("verification worktrees = %v, want original %q twice", worktrees, claim.Worktree)
	}
	if row := issueRow(t, st, claim.IssueID); row.State != "done" {
		t.Fatalf("retried row = %+v", row)
	}
}

func TestLaunchIssueRunsDraft(t *testing.T) {
	e, st := newTestEngine(t)
	useAutoLaunchFlow(e)
	id, _ := e.DraftIssue("t", "b", "default", "regular", levers.Matrix{}, 1, nil)
	if err := e.LaunchIssue(id); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, st, id, core.EvIssueCompleted)
	if !hasEvent(t, st, id, core.EvIssueCreated) {
		t.Fatal("launch did not emit issue_created")
	}
	if err := e.UpdateIssue(id, "x", "", "default", "regular", levers.Matrix{}, 0, nil); err == nil {
		t.Fatal("update after launch succeeded")
	}
}

func TestLaunchIssueRejectsNonDrafts(t *testing.T) {
	e, _ := newTestEngine(t)
	if err := e.LaunchIssue("GH-999"); err == nil {
		t.Fatal("launched unknown issue")
	}
	rid, _ := e.CreateIssue("r", "", "default", levers.Matrix{}, 0, nil)
	if err := e.LaunchIssue(rid); err == nil {
		t.Fatal("launched a non-draft issue")
	}
}

// Whatever is in the field on save is the new set: a dropped name loses its row
// and its bytes, a retained name keeps both.
func TestUpdateIssueReplacesAttachmentSet(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	dir := t.TempDir()
	keep := filepath.Join(dir, "keep.log")
	drop := filepath.Join(dir, "drop.log")
	for _, p := range []string{keep, drop} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	id, err := e.DraftIssue("t", "b", "default", "regular", levers.Matrix{}, 0, []string{keep, drop})
	if err != nil {
		t.Fatal(err)
	}
	attachDir := filepath.Join(e.cfg.DataDir, id, "attachments")

	// Retain keep.log by name; drop.log simply is not in the field any more.
	if err := e.UpdateIssue(id, "t", "b", "default", "regular", levers.Matrix{}, 0,
		[]string{"keep.log"}); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.Attachments(id)
	if len(rows) != 1 || rows[0].Name != "keep.log" {
		t.Fatalf("rows = %+v", rows)
	}
	if _, err := os.Stat(filepath.Join(attachDir, "keep.log")); err != nil {
		t.Fatalf("retained bytes deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(attachDir, "drop.log")); !os.IsNotExist(err) {
		t.Fatalf("dropped bytes survived: %v", err)
	}
}

// A refusal leaves the draft exactly as it was.
func TestUpdateIssueRefusesBadAttachment(t *testing.T) {
	e, s := newEngine(t, &runner.FakeRunner{Scripts: scripts()})
	id, _ := e.DraftIssue("t", "b", "default", "regular", levers.Matrix{}, 0, nil)
	if err := e.UpdateIssue(id, "t2", "b2", "default", "regular", levers.Matrix{}, 0,
		[]string{"/nope/ghost.log"}); err == nil {
		t.Fatal("UpdateIssue accepted a missing attachment")
	}
	issues, _ := s.Issues()
	for _, row := range issues {
		if row.ID == id && row.Title != "t" {
			t.Fatalf("a refused update still rewrote the draft: %+v", row)
		}
	}
}
