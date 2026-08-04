package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/weston6142/watchtower/internal/proto"
)

type reconnectTestSession struct {
	doCalls    []proto.Command
	closeCalls int
}

func (s *reconnectTestSession) Do(command proto.Command) (proto.Response, error) {
	s.doCalls = append(s.doCalls, command)
	return proto.Response{OK: true}, nil
}

func (s *reconnectTestSession) Close() error {
	s.closeCalls++
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

	next, cmd := m.Update(pollErrorMsg{err: errors.New("write unix watchtower.sock: broken pipe")})
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

	next, cmd := m.Update(pollErrorMsg{err: errors.New("connection lost")})
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

	next, first := m.Update(pollErrorMsg{err: errors.New("connection lost")})
	m = next.(Model)
	_, second := m.Update(pollErrorMsg{err: errors.New("connection lost again")})
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
