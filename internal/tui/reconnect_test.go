package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/proto"
	"github.com/weston6142/watchtower/internal/store"
)

type reconnectTestSession struct {
	doCalls       []proto.Command
	closeCalls    int
	responses     map[string]proto.Response
	errors        map[string]error
	blockOp       string
	started       chan struct{}
	release       chan struct{}
	startedOnce   bool
	closeUnblocks bool
}

func (s *reconnectTestSession) Do(command proto.Command) (proto.Response, error) {
	s.doCalls = append(s.doCalls, command)
	if command.Op == s.blockOp && s.release != nil {
		if !s.startedOnce && s.started != nil {
			close(s.started)
			s.startedOnce = true
		}
		<-s.release
	}
	if err := s.errors[command.Op]; err != nil {
		return proto.Response{}, err
	}
	if response, ok := s.responses[command.Op]; ok {
		return response, nil
	}
	return proto.Response{OK: true}, nil
}

func (s *reconnectTestSession) Close() error {
	s.closeCalls++
	if s.closeUnblocks && s.release != nil {
		select {
		case <-s.release:
		default:
			close(s.release)
		}
	}
	return nil
}

func fakeRetryScheduler(delays *[]time.Duration, due func(uint64) tea.Msg) retryScheduler {
	return func(_ context.Context, generation uint64, delay time.Duration) tea.Cmd {
		*delays = append(*delays, delay)
		return func() tea.Msg { return due(generation) }
	}
}

func TestTransportLossShowsReconnectingState(t *testing.T) {
	var delays []time.Duration
	m := NewModel(&reconnectTestSession{}, []string{"brainstorm", "spec"})
	m.Width, m.Height = 100, 20
	m.retryScheduler = fakeRetryScheduler(&delays, func(generation uint64) tea.Msg {
		return reconnectTimerMsg{generation: generation}
	})

	next, cmd := m.Update(pollErrorMsg{err: errors.New("write unix watchtower.sock: broken pipe"), transport: true})
	m = next.(Model)
	plain := ansi.Strip(m.View())
	if !strings.Contains(plain, "reconnecting") {
		t.Fatalf("view did not show reconnecting state:\n%s", plain)
	}
	if strings.Contains(plain, "broken pipe") {
		t.Fatalf("view exposed raw transport error:\n%s", plain)
	}
	if cmd == nil {
		t.Fatal("transport loss did not schedule a retry")
	}
	if len(delays) != 1 || delays[0] != initialRetryDelay {
		t.Fatalf("scheduled delays = %v, want [%s]", delays, initialRetryDelay)
	}
}

func TestReconnectBackoffUsesCappedExponentialCadence(t *testing.T) {
	var delays []time.Duration
	m := NewModel(nil, []string{"brainstorm", "spec"})
	m.retryScheduler = fakeRetryScheduler(&delays, func(generation uint64) tea.Msg {
		return reconnectTimerMsg{generation: generation}
	})
	m.SetReconnectDialer(func() (Session, error) {
		return nil, errors.New("connection refused")
	})

	next, cmd := m.Update(pollErrorMsg{err: errors.New("connection lost"), transport: true})
	m = next.(Model)
	for i := 0; i < 6; i++ {
		if cmd == nil {
			t.Fatalf("retry %d returned no timer command", i)
		}
		msg := cmd()
		next, attempt := m.Update(msg)
		m = next.(Model)
		if attempt == nil {
			t.Fatalf("retry %d returned no dial attempt", i)
		}
		next, cmd = m.Update(attempt())
		m = next.(Model)
	}

	want := []time.Duration{
		250 * time.Millisecond,
		500 * time.Millisecond,
		time.Second,
		2 * time.Second,
		4 * time.Second,
		4 * time.Second,
		4 * time.Second,
	}
	if len(delays) != len(want) {
		t.Fatalf("scheduled delays = %v, want %v", delays, want)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("delay %d = %s, want %s", i, delays[i], want[i])
		}
	}
}

func TestReconnectLoopDoesNotDuplicateOnRepeatedLoss(t *testing.T) {
	var delays []time.Duration
	var dialCalls int
	m := NewModel(&reconnectTestSession{}, []string{"brainstorm", "spec"})
	m.retryScheduler = fakeRetryScheduler(&delays, func(generation uint64) tea.Msg {
		return reconnectTimerMsg{generation: generation}
	})
	m.SetReconnectDialer(func() (Session, error) {
		dialCalls++
		return nil, errors.New("connection refused")
	})

	next, first := m.Update(pollErrorMsg{err: errors.New("connection lost"), transport: true})
	m = next.(Model)
	_, second := m.Update(pollErrorMsg{err: errors.New("connection lost again"), transport: true})
	if first == nil {
		t.Fatal("first transport loss did not schedule a retry")
	}
	if second != nil {
		t.Fatal("repeated transport loss scheduled a duplicate retry")
	}
	if len(delays) != 1 {
		t.Fatalf("scheduled delays = %v, want one pending retry", delays)
	}
	if dialCalls != 0 {
		t.Fatalf("dialer called before the first retry fired: %d", dialCalls)
	}
}

func recoveryEvent(t *testing.T, seq int64, issueID, title string) core.Event {
	t.Helper()
	event, err := core.NewEvent(core.EvIssueCreated, issueID, map[string]any{
		"title": title, "flow": "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	event.Seq = seq
	return event
}

func recoveryStageEvent(t *testing.T, seq int64, issueID, stage string) core.Event {
	t.Helper()
	event, err := core.NewEvent(core.EvStageStarted, issueID, map[string]any{"stage": stage})
	if err != nil {
		t.Fatal(err)
	}
	event.Seq = seq
	return event
}

func recoveryModel(t *testing.T, replacement *reconnectTestSession) (Model, tea.Cmd) {
	t.Helper()
	old := NewModel(&reconnectTestSession{}, []string{"spec", "execute"})
	old = old.applyEvents([]core.Event{
		recoveryEvent(t, 1, "GH-1", "old projection"),
		recoveryStageEvent(t, 2, "GH-1", "spec"),
	})
	old.Focus = Focus{Floor: 1, Card: 0, Issue: "GH-1"}
	old.Detail = &proto.IssueDetail{Issue: store.IssueRow{ID: "GH-1", Title: "old detail"}}
	old.Width, old.Height = 100, 20
	var delays []time.Duration
	old.retryScheduler = fakeRetryScheduler(&delays, func(generation uint64) tea.Msg {
		return reconnectTimerMsg{generation: generation}
	})
	old.SetReconnectDialer(func() (Session, error) { return replacement, nil })
	next, timer := old.Update(pollErrorMsg{err: errors.New("daemon stopped"), transport: true})
	model := next.(Model)
	if timer == nil {
		t.Fatal("transport loss did not schedule recovery")
	}
	next, attempt := model.Update(timer())
	return next.(Model), attempt
}

func TestReconnectRefreshesOverviewProjectionAndFocusedDetailBeforeConnected(t *testing.T) {
	replacement := &reconnectTestSession{
		responses: map[string]proto.Response{
			"tail": {
				OK: true,
				Events: []core.Event{
					recoveryEvent(t, 11, "GH-1", "replacement projection"),
					recoveryStageEvent(t, 12, "GH-1", "spec"),
				},
			},
			"overview": {OK: true, Overview: &proto.Overview{Building: 7}},
			"issue_detail": {OK: true, Detail: &proto.IssueDetail{
				Issue: store.IssueRow{ID: "GH-1", Title: "replacement detail"},
			}},
		},
		errors: map[string]error{},
	}
	m, attempt := recoveryModel(t, replacement)
	if !strings.Contains(ansi.Strip(m.View()), "reconnecting") {
		t.Fatal("model became interactive before recovery result was applied")
	}
	if attempt == nil {
		t.Fatal("reconnect timer did not start a recovery attempt")
	}
	result := attempt()
	if !strings.Contains(ansi.Strip(m.View()), "reconnecting") {
		t.Fatal("model changed state while recovery command was still in flight")
	}
	next, _ := m.Update(result)
	m = next.(Model)
	if strings.Contains(ansi.Strip(m.View()), "reconnecting") {
		t.Fatal("model stayed reconnecting after complete recovery")
	}
	if m.State.Issues["GH-1"].Title != "replacement projection" {
		t.Fatalf("projection title = %q, want replacement projection", m.State.Issues["GH-1"].Title)
	}
	if m.Overview == nil || m.Overview.Building != 7 {
		t.Fatalf("overview = %+v, want replacement overview", m.Overview)
	}
	if m.Detail == nil || m.Detail.Issue.Title != "replacement detail" {
		t.Fatalf("detail = %+v, want replacement detail", m.Detail)
	}
	if m.Focus.Issue != "GH-1" {
		t.Fatalf("focus = %+v, want saved focus restored", m.Focus)
	}
}

func TestRefreshFailureClosesPartialSessionAndContinuesBackoff(t *testing.T) {
	replacement := &reconnectTestSession{
		responses: map[string]proto.Response{
			"tail": {OK: true, Events: []core.Event{recoveryEvent(t, 11, "GH-1", "replacement")}},
		},
		errors: map[string]error{"overview": errors.New("overview unavailable")},
	}
	m, attempt := recoveryModel(t, replacement)
	if attempt == nil {
		t.Fatal("reconnect timer did not start a recovery attempt")
	}
	next, retry := m.Update(attempt())
	m = next.(Model)
	if replacement.closeCalls != 1 {
		t.Fatalf("partial session close count = %d, want 1", replacement.closeCalls)
	}
	if !strings.Contains(ansi.Strip(m.View()), "reconnecting") {
		t.Fatal("refresh failure left the reconnecting surface")
	}
	if retry == nil {
		t.Fatal("refresh failure did not schedule the next retry")
	}
	if m.client != nil {
		t.Fatal("partial session remained interactive after refresh failure")
	}
}

func TestMissingFocusedIssueFallsBackToRepositoryOverview(t *testing.T) {
	replacement := &reconnectTestSession{
		responses: map[string]proto.Response{
			"tail": {
				OK: true,
				Events: []core.Event{
					recoveryEvent(t, 21, "GH-2", "replacement other issue"),
					recoveryStageEvent(t, 22, "GH-2", "spec"),
				},
			},
			"overview": {OK: true, Overview: &proto.Overview{}},
		},
		errors: map[string]error{},
	}
	m, attempt := recoveryModel(t, replacement)
	next, _ := m.Update(attempt())
	m = next.(Model)
	if m.Focus.Issue != "" {
		t.Fatalf("focus = %+v, want overview fallback", m.Focus)
	}
	if m.Detail != nil || len(m.modes) != 0 {
		t.Fatalf("item-specific state survived missing focus: detail=%+v modes=%v", m.Detail, m.modes)
	}
	if m.State.Issues["GH-1"] != nil || m.State.Issues["GH-2"] == nil {
		t.Fatalf("replacement projection = %+v", m.State.Issues)
	}
	next, _ = m.Update(Msg{generation: m.generation})
	m = next.(Model)
	if m.Focus.Issue != "" {
		t.Fatalf("empty later poll reselected missing focus: %+v", m.Focus)
	}
}

func TestDelayedOldGenerationCannotOverwriteReplacementState(t *testing.T) {
	oldSession := &reconnectTestSession{
		blockOp: "issue_detail", started: make(chan struct{}), release: make(chan struct{}),
		responses: map[string]proto.Response{"issue_detail": {
			OK: true, Detail: &proto.IssueDetail{Issue: store.IssueRow{ID: "GH-1", Title: "stale detail"}},
		}},
		errors: map[string]error{},
	}
	m := NewModel(oldSession, []string{"spec", "execute"})
	m = m.applyEvents([]core.Event{
		recoveryEvent(t, 31, "GH-1", "old projection"), recoveryStageEvent(t, 32, "GH-1", "spec"),
	})
	m.Focus = Focus{Floor: 1, Card: 0, Issue: "GH-1"}
	oldCommand := m.fetchDetail("GH-1")
	oldResult := make(chan tea.Msg, 1)
	go func() { oldResult <- oldCommand() }()
	<-oldSession.started

	replacement := &reconnectTestSession{responses: map[string]proto.Response{
		"tail":         {OK: true, Events: []core.Event{recoveryEvent(t, 41, "GH-1", "fresh projection"), recoveryStageEvent(t, 42, "GH-1", "spec")}},
		"overview":     {OK: true, Overview: &proto.Overview{Building: 9}},
		"issue_detail": {OK: true, Detail: &proto.IssueDetail{Issue: store.IssueRow{ID: "GH-1", Title: "fresh detail"}}},
	}, errors: map[string]error{}}
	var delays []time.Duration
	m.retryScheduler = fakeRetryScheduler(&delays, func(generation uint64) tea.Msg {
		return reconnectTimerMsg{generation: generation}
	})
	m.SetReconnectDialer(func() (Session, error) { return replacement, nil })
	next, timer := m.Update(pollErrorMsg{err: errors.New("daemon stopped"), transport: true})
	m = next.(Model)
	next, attempt := m.Update(timer())
	m = next.(Model)
	next, _ = m.Update(attempt())
	m = next.(Model)
	if m.Detail == nil || m.Detail.Issue.Title != "fresh detail" || m.Overview.Building != 9 {
		t.Fatalf("replacement state not installed: detail=%+v overview=%+v", m.Detail, m.Overview)
	}

	close(oldSession.release)
	oldMessage := <-oldResult
	next, _ = m.Update(oldMessage)
	m = next.(Model)
	if m.Detail.Issue.Title != "fresh detail" || m.Overview.Building != 9 || m.connection != connectionConnected {
		t.Fatalf("stale generation overwrote replacement state: detail=%+v overview=%+v state=%v", m.Detail, m.Overview, m.connection)
	}
}

func disconnectedInputModel(t *testing.T) (Model, *reconnectTestSession) {
	t.Helper()
	session := &reconnectTestSession{}
	m := NewModel(session, []string{"spec", "execute"})
	m = m.applyEvents([]core.Event{
		recoveryEvent(t, 51, "GH-1", "running"), recoveryStageEvent(t, 52, "GH-1", "spec"),
	})
	m.Focus = Focus{Floor: 1, Card: 0, Issue: "GH-1"}
	var delays []time.Duration
	m.retryScheduler = fakeRetryScheduler(&delays, func(generation uint64) tea.Msg {
		return reconnectTimerMsg{generation: generation}
	})
	next, _ := m.Update(pollErrorMsg{err: errors.New("daemon stopped"), transport: true})
	m = next.(Model)
	// Keep a session-shaped fixture in the model so connection-dependent keys
	// can prove they are gated by the lifecycle state, not by a nil guard.
	m.client = session
	return m, session
}

func TestReconnectIgnoresConnectionDependentKeysButKeepsHelpAndQuit(t *testing.T) {
	m, session := disconnectedInputModel(t)
	before := len(session.doCalls)
	if next, cmd := pressKeyCmd(t, m, "p"); cmd != nil || len(session.doCalls) != before {
		t.Fatalf("p was not gated while reconnecting: cmd=%v calls=%v", cmd != nil, session.doCalls)
	} else {
		m = next
	}
	if next, cmd := pressKeyCmd(t, m, "enter"); cmd != nil || len(session.doCalls) != before {
		t.Fatalf("enter was not gated while reconnecting: cmd=%v calls=%v", cmd != nil, session.doCalls)
	} else {
		m = next
	}
	m, _ = pressKeyCmd(t, m, "?")
	if !m.help {
		t.Fatal("help did not remain available while reconnecting")
	}
	beforeClose := session.closeCalls
	m, cmd := pressKeyCmd(t, m, "q")
	if !isQuit(cmd) {
		t.Fatal("q did not quit while reconnecting")
	}
	if session.closeCalls <= beforeClose {
		t.Fatalf("quit did not close the active session: before=%d after=%d", beforeClose, session.closeCalls)
	}
	if !m.shuttingDown {
		t.Fatal("quit did not make the model terminal")
	}
}

func TestQuitDuringReconnectCancelsTimerAndClosesPartialSession(t *testing.T) {
	replacement := &reconnectTestSession{
		blockOp: "overview", started: make(chan struct{}), release: make(chan struct{}), closeUnblocks: true,
		responses: map[string]proto.Response{}, errors: map[string]error{},
	}
	old := NewModel(&reconnectTestSession{}, []string{"spec", "execute"})
	old = old.applyEvents([]core.Event{
		recoveryEvent(t, 61, "GH-1", "running"), recoveryStageEvent(t, 62, "GH-1", "spec"),
	})
	old.Focus = Focus{Floor: 1, Card: 0, Issue: "GH-1"}
	var delays []time.Duration
	old.retryScheduler = fakeRetryScheduler(&delays, func(generation uint64) tea.Msg {
		return reconnectTimerMsg{generation: generation}
	})
	dialCalls := 0
	old.SetReconnectDialer(func() (Session, error) {
		dialCalls++
		return replacement, nil
	})
	next, timer := old.Update(pollErrorMsg{err: errors.New("daemon stopped"), transport: true})
	m := next.(Model)
	timerMsg := timer()
	next, attempt := m.Update(timerMsg)
	m = next.(Model)
	if attempt == nil {
		t.Fatal("timer did not begin reconnect attempt")
	}
	result := make(chan tea.Msg, 1)
	go func() { result <- attempt() }()
	select {
	case <-replacement.started:
	case <-time.After(time.Second):
		t.Fatal("replacement refresh did not reach its blocking operation")
	}
	m, quit := pressKeyCmd(t, m, "q")
	if !isQuit(quit) {
		t.Fatal("q did not quit during a blocked recovery")
	}
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("quit did not unblock the replacement refresh")
	}
	if replacement.closeCalls != 1 {
		t.Fatalf("replacement close count = %d, want 1", replacement.closeCalls)
	}
	if _, pending := m.Update(timerMsg); pending != nil || dialCalls != 1 {
		t.Fatalf("canceled retry produced future work: cmd=%v dialCalls=%d", pending != nil, dialCalls)
	}
}

func TestRecoveredModelResumesNormalKeyHandling(t *testing.T) {
	replacement := &reconnectTestSession{
		responses: map[string]proto.Response{
			"tail": {OK: true, Events: []core.Event{
				recoveryEvent(t, 71, "GH-1", "running"), recoveryStageEvent(t, 72, "GH-1", "spec"),
			}},
			"overview":     {OK: true, Overview: &proto.Overview{}},
			"issue_detail": {OK: true, Detail: &proto.IssueDetail{Issue: store.IssueRow{ID: "GH-1", Title: "detail"}}},
		}, errors: map[string]error{},
	}
	m, attempt := recoveryModel(t, replacement)
	next, _ := m.Update(attempt())
	m = next.(Model)
	before := len(replacement.doCalls)
	m, cmd := pressKeyCmd(t, m, "p")
	if cmd == nil {
		t.Fatal("recovered model did not resume connection-dependent input")
	}
	_ = cmd()
	if len(replacement.doCalls) != before+1 || replacement.doCalls[len(replacement.doCalls)-1].Op != "pause_issue" {
		t.Fatalf("recovered input calls = %v, want pause_issue", replacement.doCalls)
	}
}

func TestDetailTransportLossStartsReconnect(t *testing.T) {
	session := &reconnectTestSession{
		errors: map[string]error{"issue_detail": errors.New("write unix watchtower.sock: broken pipe")},
	}
	m := NewModel(session, []string{"spec", "execute"})
	m = m.applyEvents([]core.Event{
		recoveryEvent(t, 81, "GH-1", "running"), recoveryStageEvent(t, 82, "GH-1", "spec"),
	})
	m.Focus = Focus{Floor: 1, Card: 0, Issue: "GH-1"}
	var delays []time.Duration
	m.retryScheduler = fakeRetryScheduler(&delays, func(generation uint64) tea.Msg {
		return reconnectTimerMsg{generation: generation}
	})

	recoveryResult := m.fetchDetail("GH-1")()
	next, retry := m.Update(recoveryResult)
	m = next.(Model)

	if m.connection != connectionReconnecting {
		t.Fatalf("detail transport loss left connection state %v, want reconnecting", m.connection)
	}
	if retry == nil || len(delays) != 1 || delays[0] != initialRetryDelay {
		t.Fatalf("detail transport loss scheduled retry=%v delays=%v, want one initial retry", retry != nil, delays)
	}
	if !strings.Contains(ansi.Strip(m.View()), "reconnecting") {
		t.Fatalf("detail transport loss did not render reconnecting state:\n%s", ansi.Strip(m.View()))
	}
}
