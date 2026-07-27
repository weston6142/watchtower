package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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

	client        *proto.Client
	stages        []string
	lastSeq       int64
	dismissed     map[int64]bool
	optionMode    bool
	pager         pagerState
	openArtifacts bool
}

type Msg struct{ Events []core.Event }

type tickMsg struct{}

type pollErrorMsg struct{ err error }

type detailMsg struct {
	detail *proto.IssueDetail
	err    error
}

type answerMsg struct {
	decisionID int64
	response   proto.Response
	err        error
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
			m.openArtifacts = false
			return m, nil
		}
		m.Detail = msg.detail
		if m.openArtifacts && msg.detail != nil {
			m.pager = pagerState{Mode: "artifacts", Files: append([]string(nil), msg.detail.Artifacts...)}
			m.openArtifacts = false
		}
		return m, nil
	case answerMsg:
		if msg.err != nil {
			m.Err = msg.err.Error()
			return m, nil
		}
		if !msg.response.OK {
			m.Err = msg.response.Error
			return m, nil
		}
		if m.Toast != nil && m.Toast.ID == msg.decisionID {
			m.Toast = nil
			m.optionMode = false
		}
		return m, nil
	case tea.WindowSizeMsg:
		m.Width, m.Height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		key := msg.String()
		switch key {
		case "q", "ctrl+c":
			return m, tea.Quit
		}
		if m.pager.Mode != "" {
			return m, m.updatePagerKey(key)
		}
		if m.Toast != nil {
			switch {
			case key == "y":
				return m, m.answerDecision(m.Toast.Recommended)
			case key == "n":
				m.optionMode = true
				return m, nil
			case m.optionMode && len(key) == 1 && key >= "0" && key <= "9":
				option := int(key[0] - '0')
				if option < len(m.Toast.Options) {
					return m, m.answerDecision(option)
				}
			case key == "esc":
				m.dismissToast()
				return m, nil
			case key == "o":
				return m, m.openArtifactsFor(m.Toast.IssueID)
			}
			return m, nil
		}
		if key == "enter" && m.Focus.Issue != "" {
			return m, m.openArtifactsFor(m.Focus.Issue)
		}
		if key == "o" && m.Focus.Issue != "" {
			return m, m.openArtifactsFor(m.Focus.Issue)
		}
		moved := moveFocus(m.Focus, m.State, m.stages, key)
		if moved != m.Focus {
			m.Focus = moved
			m.Detail = nil
			m.openArtifacts = false
			if m.Focus.Issue != "" {
				return m, m.fetchDetail(m.Focus.Issue)
			}
		}
	}
	return m, nil
}

func (m *Model) dismissToast() {
	if m.Toast == nil {
		return
	}
	if m.dismissed == nil {
		m.dismissed = map[int64]bool{}
	}
	m.dismissed[m.Toast.ID] = true
	m.Toast = nil
	m.optionMode = false
}

func (m *Model) openArtifactsFor(issueID string) tea.Cmd {
	m.Focus = focusIssue(m.State, m.stages, issueID)
	m.Detail = nil
	m.openArtifacts = true
	if m.Toast != nil {
		m.dismissToast()
	}
	return m.fetchDetail(issueID)
}

func (m *Model) updatePagerKey(key string) tea.Cmd {
	switch {
	case key == "esc":
		if m.pager.Mode == "pager" {
			m.pager.Mode = "artifacts"
			m.pager.Lines = nil
			m.pager.Top = 0
		} else {
			m.pager = pagerState{}
		}
	case m.pager.Mode == "artifacts" && key == "j":
		m.pager.Sel = min(m.pager.Sel+1, max(0, len(m.pager.Files)-1))
	case m.pager.Mode == "artifacts" && key == "k":
		m.pager.Sel = max(m.pager.Sel-1, 0)
	case m.pager.Mode == "artifacts" && key == "enter":
		loaded, err := readArtifact(m.pager)
		if err != nil {
			m.Err = err.Error()
		} else {
			m.pager = loaded
		}
	case m.pager.Mode == "pager":
		m.pager = m.pager.scroll(key, max(1, m.Height-2))
	}
	return nil
}

func (m Model) answerDecision(option int) tea.Cmd {
	if m.Toast == nil || m.client == nil {
		return nil
	}
	client := m.client
	decisionID := m.Toast.ID
	return func() tea.Msg {
		r, err := client.Do(proto.Command{Op: "answer_decision", DecisionID: decisionID, Option: option})
		return answerMsg{decisionID: decisionID, response: r, err: err}
	}
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
	layoutWidth := m.Width
	if layoutWidth <= 0 {
		layoutWidth = 120
	}
	railWidth := max(24, min(40, layoutWidth/3))
	towerWidth := max(1, layoutWidth-railWidth-1)
	tower := renderTower(m.State, m.stages, m.Ids, m.Focus, towerWidth)
	if m.pager.Mode == "artifacts" {
		tower = renderArtifactList(m.pager, m.Ids[m.Focus.Issue], towerWidth, m.Height)
	} else if m.pager.Mode == "pager" {
		tower = renderPager(m.pager, towerWidth, m.Height)
	} else if m.Toast != nil {
		tower = renderToast(*m.Toast, m.Ids[m.Toast.IssueID], towerWidth)
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, tower, renderRail(m.State, m.Ids, m.Detail, railWidth))
	var b strings.Builder
	fmt.Fprintf(&b, "◆ GUILD TOWER · %d issues · q quit · ? help\n", issues)
	b.WriteString(body)
	b.WriteString("\n\nj/k floors · h/l cards · tab attention · 1-9 jump · enter drill · q quit")
	if m.Err != "" {
		fmt.Fprintf(&b, "\nerror: %s", m.Err)
	}
	return b.String()
}
