package tui

import (
	"context"
	"errors"
	"time"

	tea "github.com/charmbracelet/bubbletea"

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
	err        error
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
	if m.client != nil {
		_ = m.client.Close()
		m.client = nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.reconnectContext, m.reconnectCancel = ctx, cancel
	m.retryDelay = initialRetryDelay
	return m.scheduleReconnect(ctx, m.generation, m.retryDelay)
}

func (m *Model) startReconnectAttempt(generation uint64) tea.Cmd {
	if m.shuttingDown || m.connection != connectionReconnecting || generation != m.generation || m.reconnectAttemptActive {
		return nil
	}
	m.reconnectAttemptActive = true
	dialer := m.reconnectDialer
	return func() tea.Msg {
		if dialer == nil {
			return reconnectAttemptMsg{generation: generation, err: errors.New("reconnect dialer is not configured")}
		}
		session, err := dialer()
		return reconnectAttemptMsg{generation: generation, session: session, err: err}
	}
}

func (m *Model) retryAfterReconnectFailure(generation uint64) tea.Cmd {
	if m.shuttingDown || m.connection != connectionReconnecting || generation != m.generation {
		return nil
	}
	m.reconnectAttemptActive = false
	m.retryDelay = minRetryDelay(m.retryDelay*2, maxRetryDelay)
	return m.scheduleReconnect(m.reconnectContext, generation, m.retryDelay)
}

func minRetryDelay(delay, maxDelay time.Duration) time.Duration {
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}
