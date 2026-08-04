package tui

import (
	"context"
	"errors"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/projection"
	"github.com/weston6142/watchtower/internal/proto"
)

// Session is one serialized connection to the repository daemon.
type Session interface {
	Do(proto.Command) (proto.Response, error)
	Close() error
}

// Dialer creates a replacement session without owning daemon startup.
type Dialer func() (Session, error)

type connectionState uint8

const (
	connectionConnected connectionState = iota
	connectionReconnecting
)

const (
	initialRetryDelay = 250 * time.Millisecond
	maxRetryDelay     = 4 * time.Second
)

type retryScheduler func(context.Context, uint64, time.Duration) tea.Cmd

type reconnectTimerMsg struct{ generation uint64 }

type reconnectAttemptMsg struct {
	generation uint64
	session    Session
	state      *projection.State
	events     []core.Event
	lastSeq    int64
	overview   *proto.Overview
	detail     *proto.IssueDetail
	err        error
	retryable  bool
}

type reconnectCanceledMsg struct{ generation uint64 }

func defaultRetryScheduler(ctx context.Context, generation uint64, delay time.Duration) tea.Cmd {
	return func() tea.Msg {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
			return reconnectTimerMsg{generation: generation}
		case <-ctx.Done():
			return reconnectCanceledMsg{generation: generation}
		}
	}
}

func (m *Model) scheduleReconnect(ctx context.Context, generation uint64, delay time.Duration) tea.Cmd {
	if m.shuttingDown {
		return nil
	}
	if m.retryScheduler != nil {
		return m.retryScheduler(ctx, generation, delay)
	}
	return defaultRetryScheduler(ctx, generation, delay)
}

func (m *Model) beginReconnect(_ error) tea.Cmd {
	if m.connection == connectionReconnecting || m.shuttingDown {
		return nil
	}
	m.connection = connectionReconnecting
	m.generation++
	m.Err = ""
	m.reconnectFocus = m.Focus
	m.reconnectModes = append([]string(nil), m.modes...)
	if m.client != nil {
		_ = m.client.Close()
		m.client = nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.reconnectContext, m.reconnectCancel = ctx, cancel
	m.retryDelay = initialRetryDelay
	return m.scheduleReconnect(ctx, m.generation, m.retryDelay)
}

func (m Model) staleGeneration(generation uint64) bool {
	return generation != m.generation
}

func (m *Model) startReconnectAttempt(generation uint64) tea.Cmd {
	if m.shuttingDown || m.connection != connectionReconnecting || generation != m.generation || m.reconnectAttemptActive {
		return nil
	}
	m.reconnectAttemptActive = true
	dialer := m.reconnectDialer
	savedFocus := m.reconnectFocus
	stages := append([]string(nil), m.stages...)
	retired := cloneRetired(m.retired)
	return func() tea.Msg {
		if dialer == nil {
			return reconnectAttemptMsg{generation: generation, err: errors.New("reconnect dialer is not configured"), retryable: true}
		}
		session, err := dialer()
		if err != nil {
			return reconnectAttemptMsg{generation: generation, err: err, retryable: true}
		}
		if session == nil {
			return reconnectAttemptMsg{generation: generation, err: errors.New("reconnect dialer returned no session"), retryable: true}
		}
		fail := func(err error, retryable bool) tea.Msg {
			_ = session.Close()
			return reconnectAttemptMsg{generation: generation, err: err, retryable: retryable}
		}
		tail, err := session.Do(proto.Command{Op: "tail", SinceSeq: 0})
		if err != nil {
			return fail(err, true)
		}
		if !tail.OK {
			return fail(errors.New(tail.Error), false)
		}
		state, lastSeq := buildRecoveryState(tail.Events)
		overview, err := session.Do(proto.Command{Op: "overview"})
		if err != nil {
			return fail(err, true)
		}
		if !overview.OK {
			return fail(errors.New(overview.Error), false)
		}
		var detail *proto.IssueDetail
		if savedFocus.Issue != "" && focusIssue(state, stages, savedFocus.Issue, retired).Issue != "" {
			response, err := session.Do(proto.Command{Op: "issue_detail", IssueID: savedFocus.Issue})
			if err != nil {
				return fail(err, true)
			}
			if !response.OK {
				return fail(errors.New(response.Error), false)
			}
			detail = response.Detail
		}
		return reconnectAttemptMsg{
			generation: generation,
			session:    session,
			state:      state,
			events:     tail.Events,
			lastSeq:    lastSeq,
			overview:   overview.Overview,
			detail:     detail,
		}
	}
}

func cloneRetired(retired map[string]bool) map[string]bool {
	if retired == nil {
		return nil
	}
	copyRetired := make(map[string]bool, len(retired))
	for issueID, value := range retired {
		copyRetired[issueID] = value
	}
	return copyRetired
}

func buildRecoveryState(events []core.Event) (*projection.State, int64) {
	state := projection.NewState()
	var lastSeq int64
	for _, event := range events {
		state.Apply(event)
		if event.Seq > lastSeq {
			lastSeq = event.Seq
		}
	}
	return state, lastSeq
}

func (m *Model) retryAfterReconnectFailure(generation uint64) tea.Cmd {
	if m.shuttingDown || m.connection != connectionReconnecting || generation != m.generation {
		return nil
	}
	m.reconnectAttemptActive = false
	m.retryDelay = minRetryDelay(m.retryDelay*2, maxRetryDelay)
	return m.scheduleReconnect(m.reconnectContext, generation, m.retryDelay)
}

func (m *Model) applyReconnectAttempt(msg reconnectAttemptMsg) tea.Cmd {
	if m.staleGeneration(msg.generation) || m.connection != connectionReconnecting || m.shuttingDown {
		if msg.session != nil {
			_ = msg.session.Close()
		}
		return nil
	}
	if msg.err != nil || msg.session == nil || msg.state == nil {
		if msg.session != nil {
			_ = msg.session.Close()
		}
		if msg.err != nil && !msg.retryable {
			m.reconnectAttemptActive = false
			m.Err = msg.err.Error()
			return nil
		}
		return m.retryAfterReconnectFailure(msg.generation)
	}
	savedFocus := m.reconnectFocus
	savedModes := append([]string(nil), m.reconnectModes...)
	m.client = msg.session
	m.State = msg.state
	m.events = append([]core.Event(nil), msg.events...)
	m.lastSeq = msg.lastSeq
	m.Overview = msg.overview
	m.Detail = msg.detail
	m.Focus = savedFocus
	m.modes = savedModes
	m.Err = ""
	m.focusPinnedEmpty = false
	*m = m.applyEvents(nil)
	if savedFocus.Issue != "" && focusIssue(m.State, m.stages, savedFocus.Issue, m.retired).Issue != "" {
		m.Focus = normalizeFocus(savedFocus, m.State, m.stages, m.retired)
	} else if savedFocus.Issue != "" {
		m.Focus = Focus{}
		m.Detail = nil
		m.modes = nil
		m.doorLines = nil
		m.openArtifacts = false
		m.openEvidence = false
		m.Evidence = nil
		m.EvidenceTitle = ""
		m.evidenceDecision = nil
		m.leverEditor = nil
		m.wantLeverEditor = false
		m.focusPinnedEmpty = true
	}
	m.connection = connectionConnected
	m.reconnectAttemptActive = false
	m.retryDelay = initialRetryDelay
	if m.reconnectCancel != nil {
		m.reconnectCancel()
		m.reconnectCancel = nil
	}
	return m.tick()
}

func minRetryDelay(delay, maxDelay time.Duration) time.Duration {
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

func isNavigationKey(key string) bool {
	if key == "j" || key == "k" || key == "h" || key == "l" || key == "g" || key == "tab" ||
		key == "up" || key == "down" || key == "left" || key == "right" {
		return true
	}
	return len(key) == 1 && key >= "1" && key <= "9"
}
