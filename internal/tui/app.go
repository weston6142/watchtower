package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wbushyeager/guildhall/internal/core"
	"github.com/wbushyeager/guildhall/internal/flow"
	"github.com/wbushyeager/guildhall/internal/projection"
	"github.com/wbushyeager/guildhall/internal/proto"
)

type Focus struct {
	Floor int
	Card  int
	Issue string
}

type Model struct {
	State  *projection.State
	Flow   flow.Flow
	Ids    map[string]Identity
	Focus  Focus
	Toast  *projection.DecisionView
	Detail *proto.IssueDetail
	Width  int
	Height int
	Err    string

	client    *proto.Client
	stages    []string
	lastSeq   int64
	dismissed map[int64]bool
}

type Msg struct{ Events []core.Event }

type tickMsg struct{}

type pollErrorMsg struct{ err error }

type detailMsg struct {
	detail *proto.IssueDetail
	err    error
}

func NewModel(client *proto.Client, stages []string) Model {
	flowStages := make([]flow.Stage, len(stages))
	for i, name := range stages {
		flowStages[i] = flow.Stage{Name: name}
	}
	return Model{
		State:     projection.NewState(),
		Flow:      flow.Flow{Name: "default", Stages: flowStages},
		Ids:       map[string]Identity{},
		Focus:     Focus{},
		client:    client,
		stages:    append([]string(nil), stages...),
		dismissed: map[int64]bool{},
	}
}

func (m Model) Init() tea.Cmd {
	return m.tick()
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		if m.client == nil {
			return m, m.tick()
		}
		return m, m.poll()
	case Msg:
		m = m.applyEvents(msg.Events)
		return m, m.tick()
	case pollErrorMsg:
		m.Err = msg.err.Error()
		return m, m.tick()
	case detailMsg:
		if msg.err != nil {
			m.Err = msg.err.Error()
			return m, nil
		}
		m.Detail = msg.detail
		return m, nil
	case tea.WindowSizeMsg:
		m.Width, m.Height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}
		moved := moveFocus(m.Focus, m.State, m.stages, msg.String())
		if moved != m.Focus {
			m.Focus = moved
			m.Detail = nil
			if m.Focus.Issue != "" {
				return m, m.fetchDetail(m.Focus.Issue)
			}
		}
	}
	return m, nil
}

func (m Model) fetchDetail(issueID string) tea.Cmd {
	if m.client == nil {
		return nil
	}
	client := m.client
	return func() tea.Msg {
		r, err := client.Do(proto.Command{Op: "issue_detail", IssueID: issueID})
		if err != nil {
			return detailMsg{err: err}
		}
		if !r.OK {
			return detailMsg{err: errors.New(r.Error)}
		}
		return detailMsg{detail: r.Detail}
	}
}

func (m Model) applyEvents(evs []core.Event) Model {
	if m.State == nil {
		m.State = projection.NewState()
	}
	for _, ev := range evs {
		m.State.Apply(ev)
		if ev.Seq > m.lastSeq {
			m.lastSeq = ev.Seq
		}
	}
	titles := make(map[string]string, len(m.State.Issues))
	for id, issue := range m.State.Issues {
		titles[id] = issue.Title
	}
	m.Ids = Identify(m.State.Order, titles)
	if m.dismissed == nil {
		m.dismissed = map[int64]bool{}
	}
	if m.Toast != nil {
		if _, pending := m.State.Decisions[m.Toast.ID]; !pending {
			m.Toast = nil
		}
	}
	if m.Toast == nil {
		var selected *projection.DecisionView
		for id, decision := range m.State.Decisions {
			if m.dismissed[id] || (selected != nil && id >= selected.ID) {
				continue
			}
			copy := decision
			selected = &copy
		}
		m.Toast = selected
	}
	return m
}

func (m Model) tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m Model) poll() tea.Cmd {
	client := m.client
	since := m.lastSeq
	return func() tea.Msg {
		r, err := client.Do(proto.Command{Op: "tail", SinceSeq: since})
		if err != nil {
			return pollErrorMsg{err: err}
		}
		if !r.OK {
			return pollErrorMsg{err: errors.New(r.Error)}
		}
		return Msg{Events: r.Events}
	}
}

func (m Model) View() string {
	issues := 0
	if m.State != nil {
		issues = len(m.State.Issues)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "◆ GUILD TOWER · %d issues · q quit · ? help\n", issues)
	b.WriteString(renderTower(m.State, m.stages, m.Ids, m.Focus, m.Width))
	if m.Err != "" {
		fmt.Fprintf(&b, "\nerror: %s", m.Err)
	}
	return b.String()
}
