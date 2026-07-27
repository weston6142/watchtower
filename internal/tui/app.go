package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/wbushyeager/guildhall/internal/archmap"
	"github.com/wbushyeager/guildhall/internal/core"
	"github.com/wbushyeager/guildhall/internal/evidence"
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
	State         *projection.State
	Overview      *proto.Overview
	Flow          flow.Flow
	Ids           map[string]Identity
	Focus         Focus
	Toast         *projection.DecisionView
	Detail        *proto.IssueDetail
	Evidence      *evidence.Bundle
	EvidenceTitle string
	Arch          *archmap.Map
	Repo          string
	Width         int
	Height        int
	Err           string

	client           *proto.Client
	stages           []string
	lastSeq          int64
	dismissed        map[int64]bool
	optionMode       bool
	pager            pagerState
	openArtifacts    bool
	openEvidence     bool
	evidenceDecision *projection.DecisionView
	evidenceOpened   map[int64]bool
	acceptStreak     int
	archMode         string
	help             bool
	aliases          map[string]string
	reducedMotion    bool
	ticks            int
}

type Msg struct{ Events []core.Event }

type tickMsg struct{}

type pollErrorMsg struct{ err error }

type overviewMsg struct {
	overview *proto.Overview
	err      error
}

type detailMsg struct {
	detail *proto.IssueDetail
	err    error
}

type answerMsg struct {
	decisionID int64
	response   proto.Response
	err        error
}

type archMsg struct {
	arch *archmap.Map
	err  error
}

func NewModel(client *proto.Client, stages []string) Model {
	flowStages := make([]flow.Stage, len(stages))
	for i, name := range stages {
		flowStages[i] = flow.Stage{Name: name}
	}
	return Model{
		State:          projection.NewState(),
		Flow:           flow.Flow{Name: "default", Stages: flowStages},
		Ids:            map[string]Identity{},
		Focus:          Focus{},
		client:         client,
		stages:         append([]string(nil), stages...),
		dismissed:      map[int64]bool{},
		evidenceOpened: map[int64]bool{},
	}
}

func (m *Model) SetStageAliases(aliases map[string]string) { m.aliases = aliases }

func (m *Model) SetReducedMotion(reduced bool) { m.reducedMotion = reduced }

func ParseStageAliases(raw string) map[string]string {
	aliases := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		parts := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			aliases[parts[0]] = parts[1]
		}
	}
	return aliases
}

func (m Model) Init() tea.Cmd {
	return m.tick()
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		m.ticks++
		if m.client == nil {
			return m, m.tick()
		}
		return m, tea.Batch(m.poll(), m.pollOverview())
	case Msg:
		m = m.applyEvents(msg.Events)
		return m, m.tick()
	case pollErrorMsg:
		m.Err = msg.err.Error()
		return m, m.tick()
	case overviewMsg:
		if msg.err != nil {
			m.Err = msg.err.Error()
			return m, nil
		}
		m.Overview = msg.overview
		return m, nil
	case detailMsg:
		if msg.err != nil {
			m.Err = msg.err.Error()
			m.openArtifacts = false
			return m, nil
		}
		m.Detail = msg.detail
		if m.openEvidence && msg.detail != nil {
			m.EvidenceTitle = msg.detail.Issue.ID + " " + msg.detail.Issue.Title
			if path := latestEvidencePath(msg.detail.Artifacts); path != "" {
				bundle, err := readEvidenceBundle(path)
				if err != nil {
					m.Err = err.Error()
					m.Evidence = nil
				} else {
					m.Evidence = &bundle
				}
			} else {
				m.Evidence = nil
			}
			m.openEvidence = false
			return m, nil
		}
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
	case archMsg:
		if msg.err != nil {
			m.Err = msg.err.Error()
			return m, nil
		}
		m.Arch = msg.arch
		return m, nil
	case tea.WindowSizeMsg:
		m.Width, m.Height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		key := msg.String()
		switch key {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "?":
			m.help = !m.help
			return m, nil
		}
		if m.archMode != "" {
			if key == "esc" || key == "a" {
				m.archMode = ""
				return m, nil
			}
			if key == "A" {
				m.archMode = "full"
				return m, m.fetchArch()
			}
			return m, nil
		}
		if m.pager.Mode != "" {
			cmd := m.updatePagerKey(key)
			return m, cmd
		}
		if m.Evidence != nil || m.evidenceDecision != nil {
			switch key {
			case "esc":
				m.Evidence = nil
				m.evidenceDecision = nil
				m.EvidenceTitle = ""
			case "enter":
				return m, m.openDiffPager()
			}
			return m, nil
		}
		if m.Toast != nil {
			switch {
			case key == "y":
				if !m.evidenceOpened[m.Toast.ID] {
					m.acceptStreak++
				}
				return m, m.answerDecision(m.Toast.Recommended)
			case key == "n":
				m.acceptStreak = 0
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
				m.acceptStreak = 0
				cmd := m.openEvidenceFor(m.Toast.IssueID, m.Toast.ID)
				return m, cmd
			}
			return m, nil
		}
		if (key == "enter" || key == "o") && m.Focus.Issue != "" {
			if key == "o" {
				m.acceptStreak = 0
				return m, m.openEvidenceFor(m.Focus.Issue, 0)
			}
			cmd := m.openArtifactsFor(m.Focus.Issue)
			return m, cmd
		}
		if key == "a" {
			m.archMode = "pane"
			return m, m.fetchArch()
		}
		if key == "A" {
			m.archMode = "full"
			return m, m.fetchArch()
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

func (m *Model) openEvidenceFor(issueID string, decisionID int64) tea.Cmd {
	m.Focus = focusIssue(m.State, m.stages, issueID)
	m.Detail = nil
	m.Evidence = nil
	m.EvidenceTitle = issueID
	m.openEvidence = true
	if m.evidenceOpened == nil {
		m.evidenceOpened = map[int64]bool{}
	}
	if decisionID != 0 {
		decision := m.State.Decisions[decisionID]
		m.evidenceDecision = &decision
		m.evidenceOpened[decisionID] = true
	} else {
		for id, decision := range m.State.Decisions {
			if decision.IssueID == issueID {
				decisionCopy := decision
				m.evidenceDecision = &decisionCopy
				m.evidenceOpened[id] = true
				break
			}
		}
	}
	if m.Toast != nil && m.Toast.IssueID == issueID {
		m.dismissToast()
	}
	if m.client == nil {
		m.openEvidence = false
		return nil
	}
	return m.fetchDetail(issueID)
}

func latestEvidencePath(paths []string) string {
	for i := len(paths) - 1; i >= 0; i-- {
		if filepath.Base(paths[i]) == "evidence.json" {
			return paths[i]
		}
	}
	return ""
}

// Evidence files are local daemon artifacts; the current TUI intentionally
// reads them directly because the daemon and client run on the same machine.
func readEvidenceBundle(path string) (evidence.Bundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return evidence.Bundle{}, err
	}
	var bundle evidence.Bundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return evidence.Bundle{}, err
	}
	return bundle, nil
}

func (m *Model) openDiffPager() tea.Cmd {
	if m.Detail == nil {
		return nil
	}
	for i := len(m.Detail.Artifacts) - 1; i >= 0; i-- {
		if filepath.Base(m.Detail.Artifacts[i]) != "diff.patch" {
			continue
		}
		loaded, err := readArtifact(pagerState{Mode: "artifacts", Files: []string{m.Detail.Artifacts[i]}})
		if err != nil {
			m.Err = err.Error()
			return nil
		}
		m.Evidence = nil
		m.evidenceDecision = nil
		m.pager = loaded
		return nil
	}
	m.Err = "no diff artifact available"
	return nil
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

func (m Model) fetchArch() tea.Cmd {
	if m.client == nil {
		return nil
	}
	client := m.client
	repo := m.Repo
	return func() tea.Msg {
		r, err := client.Do(proto.Command{Op: "arch_map", Repo: repo})
		if err != nil {
			return archMsg{err: err}
		}
		if !r.OK {
			return archMsg{err: errors.New(r.Error)}
		}
		return archMsg{arch: r.Arch}
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
			candidate := decision
			selected = &candidate
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

func (m Model) pollOverview() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		r, err := client.Do(proto.Command{Op: "overview"})
		if err != nil {
			return overviewMsg{err: err}
		}
		if !r.OK {
			return overviewMsg{err: errors.New(r.Error)}
		}
		return overviewMsg{overview: r.Overview}
	}
}

func (m Model) View() string {
	layoutWidth := m.Width
	if layoutWidth <= 0 {
		layoutWidth = 120
	}
	railWidth := max(24, min(40, layoutWidth/3))
	towerWidth := max(1, layoutWidth-railWidth-1)
	tower := renderTowerConfigured(m.State, m.stages, m.Ids, m.Focus, m.aliases, m.reducedMotion, m.ticks, towerWidth)
	if m.pager.Mode == "artifacts" {
		tower = renderArtifactList(m.pager, m.Ids[m.Focus.Issue], towerWidth, m.Height)
	} else if m.pager.Mode == "pager" {
		tower = renderPager(m.pager, towerWidth, m.Height)
	} else if m.Evidence != nil {
		lastError := ""
		var artifacts []string
		if m.Detail != nil {
			lastError = m.Detail.LastError
			artifacts = m.Detail.Artifacts
		}
		tower = renderEvidenceDetails(*m.Evidence, m.EvidenceTitle, lastError, artifacts, towerWidth)
	} else if m.evidenceDecision != nil {
		tower = renderEvidenceFallback(*m.evidenceDecision, m.Detail, towerWidth)
	} else if m.Toast != nil {
		tower = renderToast(*m.Toast, m.Ids[m.Toast.IssueID], m.acceptStreak, towerWidth)
	}
	var body string
	if m.archMode == "full" {
		body = renderArch(m.Arch, m.Ids, layoutWidth, m.Height)
	} else {
		right := renderRail(m.State, m.Ids, m.Detail, railWidth)
		if m.archMode == "pane" {
			right = renderArch(m.Arch, m.Ids, railWidth, m.Height)
		}
		body = lipgloss.JoinHorizontal(lipgloss.Top, tower, right)
	}
	var b strings.Builder
	b.WriteString(renderHeader(m.Overview, layoutWidth))
	b.WriteByte('\n')
	b.WriteString(renderNoticeRow(m.State, layoutWidth))
	b.WriteByte('\n')
	b.WriteString(body)
	if m.help {
		b.WriteString("\n\nKEYS: j/k floors · h/l cards · 1-9 issue · tab attention · g war room · enter drill · esc back · d decisions · t triage · L levers · a arch pane · A arch full · ? close help · q quit")
	} else {
		b.WriteString("\n\nj/k floors · h/l cards · tab attention · 1-9 jump · enter drill · a arch · ? help · q quit")
	}
	if m.Err != "" {
		fmt.Fprintf(&b, "\nerror: %s", m.Err)
	}
	return b.String()
}
