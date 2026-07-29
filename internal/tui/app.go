package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/weston6142/watchtower/internal/archmap"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/evidence"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/priority"
	"github.com/weston6142/watchtower/internal/projection"
	"github.com/weston6142/watchtower/internal/proto"
	"github.com/weston6142/watchtower/internal/store"
)

// msgNoLaneFocused is what every issue-scoped key says when nothing is focused.
const msgNoLaneFocused = "no lane focused — press j or 1-9 to focus"

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
	toastSel         int
	pager            pagerState
	openArtifacts    bool
	openEvidence     bool
	evidenceDecision *projection.DecisionView
	evidenceOpened   map[int64]bool
	acceptStreak     int
	modes            []string
	proposals        []store.ProposalRow
	doorSel          int
	doorLines        []string
	events           []core.Event
	archMode         string
	archSel          int
	archFilter       string
	help             bool
	rows             bool
	warExpanded      bool
	retireAfter      time.Duration
	retired          map[string]bool
	shelfSel         int
	modal            *modalState
	backlog          *backlogState
	confirm          *confirmState
	leverEditor      *leverEditorState
	wantLeverEditor  bool
	aliases          map[string]string
	reducedMotion    bool
	herdrReporter    overviewReporter
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

type proposalsMsg struct {
	proposals []store.ProposalRow
	err       error
}

type transcriptMsg struct {
	lines []string
	err   error
}

type confirmState struct {
	IssueID string
	Prompt  string
	Op      string // daemon op sent on y — kill_stage, abandon_issue, …
}

type commandMsg struct {
	response proto.Response
	err      error
}

type leverApplyMsg struct {
	response proto.Response
	values   map[string]string
	err      error
}

type createIssueMsg struct {
	response proto.Response
	err      error
}

type backlogState struct{ Sel int }

// backlogEntries returns drafts for display: highest priority first, id as
// the tiebreak so the order is stable.
func backlogEntries(s *projection.State) []*projection.IssueView {
	if s == nil {
		return nil
	}
	var entries []*projection.IssueView
	for _, id := range s.Backlog {
		if iv := s.Issues[id]; iv != nil {
			entries = append(entries, iv)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Priority != entries[j].Priority {
			return entries[i].Priority > entries[j].Priority
		}
		return entries[i].ID < entries[j].ID
	})
	return entries
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
		retireAfter:    5 * time.Minute,
		retired:        map[string]bool{},
	}
}

func (m *Model) SetStageAliases(aliases map[string]string) { m.aliases = aliases }

func (m *Model) SetReducedMotion(reduced bool) { m.reducedMotion = reduced }

// overviewReporter receives every overview snapshot; satisfied by
// *herdr.Reporter. An interface so tests can substitute a spy.
type overviewReporter interface {
	Report(needYou, failing, building int)
}

func (m *Model) SetHerdrReporter(r overviewReporter) { m.herdrReporter = r }

func (m *Model) SetRetireAfter(after time.Duration) {
	if after > 0 {
		m.retireAfter = after
	}
}

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
		m.autoRetire(time.Now())
		if m.currentMode() == "timeline" {
			m.doorLines = humanizeEvents(m.events, m.Focus.Issue)
		}
		if m.client == nil {
			return m, m.tick()
		}
		cmds := []tea.Cmd{m.poll(), m.pollOverview()}
		if m.currentMode() == "transcript" {
			cmds = append(cmds, m.fetchTranscript())
		}
		return m, tea.Batch(cmds...)
	case Msg:
		m = m.applyEvents(msg.Events)
		if m.currentMode() == "timeline" {
			m.doorLines = humanizeEvents(m.events, m.Focus.Issue)
		}
		if m.currentMode() == "transcript" {
			return m, tea.Batch(m.tick(), m.fetchTranscript())
		}
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
		if m.herdrReporter != nil && msg.overview != nil {
			m.herdrReporter.Report(msg.overview.NeedYou, msg.overview.Failing, msg.overview.Building)
		}
		return m, nil
	case proposalsMsg:
		if msg.err != nil {
			m.Err = msg.err.Error()
			return m, nil
		}
		m.proposals = msg.proposals
		m.doorSel = min(m.doorSel, max(0, len(m.proposals)-1))
		return m, nil
	case transcriptMsg:
		if msg.err != nil {
			m.Err = msg.err.Error()
			return m, nil
		}
		m.doorLines = msg.lines
		return m, nil
	case detailMsg:
		if msg.err != nil {
			m.Err = msg.err.Error()
			m.openArtifacts = false
			return m, nil
		}
		m.Detail = msg.detail
		if m.wantLeverEditor && msg.detail != nil {
			m.leverEditor = newLeverEditor(msg.detail.Issue.ID, m.stages, msg.detail.Levers)
			m.wantLeverEditor = false
		}
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
			m.setToast(nil)
		}
		return m, nil
	case commandMsg:
		if msg.err != nil {
			m.Err = msg.err.Error()
			return m, nil
		}
		if !msg.response.OK {
			m.Err = msg.response.Error
		}
		return m, nil
	case leverApplyMsg:
		if msg.err != nil {
			m.Err = msg.err.Error()
			return m, nil
		}
		if !msg.response.OK {
			m.Err = msg.response.Error
			return m, nil
		}
		if m.Detail != nil {
			m.Detail.Levers = cloneStringMap(msg.values)
			m.Detail.Issue.Levers = cloneStringMap(msg.values)
		}
		m.leverEditor = nil
		return m, nil
	case createIssueMsg:
		if msg.err != nil {
			m.Err = msg.err.Error()
			return m, nil
		}
		if !msg.response.OK {
			m.Err = msg.response.Error
			return m, nil
		}
		if m.modal != nil && m.modal.EditID != "" {
			m.backlog = &backlogState{}
		}
		m.modal = nil
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
		if key == "ctrl+c" {
			return m, tea.Quit
		}
		// The help overlay is modal. It paints over the grid, so it has to
		// swallow the grid's keys — otherwise p pauses a lane and x arms a
		// kill confirm behind a screen the operator cannot see.
		if m.help {
			if key == "?" || key == "esc" {
				m.help = false
			}
			return m, nil
		}
		// The issue modal takes raw text and the backlog box owns its own
		// keys, so q and ? belong to them while they're open — quitting or
		// opening help under a live input reads as broken. ctrl+c above
		// stays the global escape hatch.
		if m.modal == nil && m.backlog == nil {
			if key == "q" {
				return m, tea.Quit
			}
			if key == "?" {
				m.help = true
				return m, nil
			}
		}
		if m.modal != nil {
			switch key {
			case "esc":
				if m.modal.EditID != "" {
					m.backlog = &backlogState{}
				}
				m.modal = nil
			case "enter", "ctrl+s":
				if strings.TrimSpace(m.modal.Title) == "" {
					m.Err = "title is required"
					return m, nil
				}
				if m.client == nil {
					m.modal = nil
					return m, nil
				}
				switch {
				case m.modal.EditID != "":
					return m, m.updateIssue(*m.modal)
				case key == "ctrl+s":
					return m, m.draftIssue(*m.modal)
				default:
					return m, m.createIssue(*m.modal)
				}
			default:
				// h/l cycle the priority chooser, but only while it is focused:
				// with the title focused, typing "hello" must still insert h
				// and l. left/right are handled here too — the arrow→vim
				// aliasing below runs after this branch returns, so it never
				// reaches the cycler.
				if m.modal.Field == priorityField {
					switch key {
					case "h", "left":
						m.modal.Priority = priority.Cycle(m.modal.Priority, -1)
						return m, nil
					case "l", "right":
						m.modal.Priority = priority.Cycle(m.modal.Priority, 1)
						return m, nil
					}
				}
				updated := m.modal.input(key)
				m.modal = &updated
			}
			return m, nil
		}
		// Arrow keys act as vim motions everywhere below; the modal
		// above takes raw text input, so it must not see the aliases.
		switch key {
		case "up":
			key = "k"
		case "down":
			key = "j"
		case "left":
			key = "h"
		case "right":
			key = "l"
		}
		if m.confirm != nil {
			switch key {
			case "y":
				issueID, op := m.confirm.IssueID, m.confirm.Op
				m.confirm = nil
				return m, m.issueCommand(issueID, op)
			case "n", "esc":
				m.confirm = nil
			}
			return m, nil
		}
		if m.backlog != nil {
			entries := backlogEntries(m.State)
			switch key {
			case "esc", "b":
				m.backlog = nil
			case "j":
				m.backlog.Sel = min(m.backlog.Sel+1, max(0, len(entries)-1))
			case "k":
				m.backlog.Sel = max(m.backlog.Sel-1, 0)
			case "enter":
				if m.backlog.Sel < len(entries) {
					iv := entries[m.backlog.Sel]
					m.Err = ""
					// The stored int is shown as-is, even when it falls outside
					// the named levels: renumbering an issue just because
					// someone opened it would lose data silently. The first
					// h/l moves it into the set.
					m.modal = &modalState{EditID: iv.ID, Title: iv.Title, Body: iv.Body,
						FlowName: iv.Flow, Preset: iv.Preset, Priority: iv.Priority}
					m.backlog = nil
				}
			case "l":
				if m.backlog.Sel < len(entries) {
					iv := entries[m.backlog.Sel]
					m.confirm = &confirmState{IssueID: iv.ID, Op: "launch_issue",
						Prompt: fmt.Sprintf("launch %s? the lane starts now. y/n", iv.Title)}
				}
			case "X":
				if m.backlog.Sel < len(entries) {
					iv := entries[m.backlog.Sel]
					m.confirm = &confirmState{IssueID: iv.ID, Op: "abandon_issue",
						Prompt: fmt.Sprintf("delete draft %s? it is removed for good. y/n", iv.Title)}
				}
			}
			return m, nil
		}
		if m.leverEditor != nil {
			switch key {
			case "esc":
				m.leverEditor = nil
			case "j":
				m.leverEditor.Sel = min(m.leverEditor.Sel+1, max(0, len(m.leverEditor.Stages)-1))
			case "k":
				m.leverEditor.Sel = max(m.leverEditor.Sel-1, 0)
			case "h":
				m.cycleSelectedLever(-1)
			case "l":
				m.cycleSelectedLever(1)
			case "enter":
				return m, m.applyLevers()
			}
			return m, nil
		}
		if m.archMode != "" {
			if key == "esc" || key == "a" {
				m.archMode = ""
				m.archFilter = ""
				return m, nil
			}
			if key == "A" {
				m.archMode = "full"
				return m, m.fetchArch()
			}
			switch key {
			case "j":
				m.archSel++
			case "k":
				m.archSel = max(0, m.archSel-1)
			default:
				if len(key) == 1 && key >= "1" && key <= "9" {
					index := int(key[0] - '1')
					if m.State != nil && index < len(m.State.Order) {
						m.archFilter = m.State.Order[index]
					}
				}
			}
			return m, nil
		}
		if m.pager.Mode != "" {
			cmd := m.updatePagerKey(key)
			return m, cmd
		}
		if len(m.modes) > 0 {
			return m, m.updateDoorKey(key)
		}
		switch key {
		case "d":
			m.modes = append(m.modes, "decisions")
			m.doorSel = 0
			return m, nil
		case "t":
			m.modes = append(m.modes, "tray")
			m.doorSel = 0
			return m, m.fetchProposals()
		case "e":
			if m.Focus.Issue == "" {
				m.Err = msgNoLaneFocused
				return m, nil
			}
			m.modes = append(m.modes, "timeline")
			m.doorLines = humanizeEvents(m.events, m.Focus.Issue)
			return m, nil
		case "T":
			// Issue-scoped like p and R: without focus fetchTranscript returns
			// nil and the door opens empty, which reads as broken.
			if m.Focus.Issue == "" {
				m.Err = msgNoLaneFocused
				return m, nil
			}
			m.modes = append(m.modes, "transcript")
			return m, m.fetchTranscript()
		case "u":
			m.modes = append(m.modes, "shelf")
			m.shelfSel = 0
			return m, nil
		case "z":
			m.rows = !m.rows
			return m, nil
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
			switch key {
			case "y":
				if !m.evidenceOpened[m.Toast.ID] {
					m.acceptStreak++
				}
				return m, m.answerDecision(m.Toast.Recommended)
			case "j":
				m.toastSel = min(m.toastSel+1, max(0, len(m.Toast.Options)-1))
			case "k":
				m.toastSel = max(m.toastSel-1, 0)
			case "enter":
				if m.toastSel == m.Toast.Recommended {
					if !m.evidenceOpened[m.Toast.ID] {
						m.acceptStreak++
					}
				} else {
					m.acceptStreak = 0
				}
				return m, m.answerDecision(m.toastSel)
			case "esc":
				m.dismissToast()
			case "o":
				m.acceptStreak = 0
				return m, m.openEvidenceFor(m.Toast.IssueID, m.Toast.ID)
			}
			return m, nil
		}
		if key == "n" {
			m.Err = ""
			m.modal = &modalState{FlowName: "default", Preset: "regular"}
			return m, nil
		}
		if key == "b" {
			m.Err = ""
			m.backlog = &backlogState{}
			return m, nil
		}
		if m.Focus.Issue != "" {
			switch key {
			case "c":
				m.retireFocused()
				return m, nil
			case "p":
				if iv := m.State.Issues[m.Focus.Issue]; iv != nil {
					op := "pause_issue"
					if iv.Paused || iv.Killed || iv.State == "paused" {
						op = "resume_issue"
					}
					return m, m.issueCommand(m.Focus.Issue, op)
				}
			case "x":
				if iv := m.State.Issues[m.Focus.Issue]; iv != nil {
					// Kill only means something for an in-flight stage; asking
					// first and erroring later reads as broken.
					if iv.State != "running" && iv.State != "waiting_decision" {
						m.Err = "nothing running — R retries · X abandons"
						return m, nil
					}
					stage := iv.CurrentStage
					if stage == "" {
						stage = "current"
					}
					m.confirm = &confirmState{IssueID: iv.ID, Op: "kill_stage",
						Prompt: fmt.Sprintf("kill the running %s stage of %s? y/n", stage, iv.Title)}
					return m, nil
				}
			case "X":
				if iv := m.State.Issues[m.Focus.Issue]; iv != nil {
					m.confirm = &confirmState{IssueID: iv.ID, Op: "abandon_issue",
						Prompt: fmt.Sprintf("abandon %s? the lane is removed for good. y/n", iv.Title)}
					return m, nil
				}
			case "R":
				if iv := m.State.Issues[m.Focus.Issue]; iv != nil && (iv.State == "failed" || iv.Killed) {
					return m, m.issueCommand(m.Focus.Issue, "retry_stage")
				}
			case "L":
				return m, m.openLeverEditor(m.Focus.Issue)
			}
		}
		if (key == "enter" || key == "o") && m.Focus.Issue != "" {
			if key == "o" {
				m.acceptStreak = 0
				return m, m.openEvidenceFor(m.Focus.Issue, 0)
			}
			cmd := m.openArtifactsFor(m.Focus.Issue)
			return m, cmd
		}
		if m.Focus.Issue == "" {
			// Issue-op keys act on the focused lane; with nothing focused
			// they would silently no-op, which reads as broken.
			switch key {
			case "p", "x", "X", "R", "L", "c", "o", "enter":
				m.Err = msgNoLaneFocused
				return m, nil
			}
		}
		if key == "g" {
			m.warExpanded = !m.warExpanded
			return m, nil
		}
		if key == "a" {
			m.archMode = "pane"
			return m, m.fetchArch()
		}
		if key == "A" {
			m.archMode = "full"
			return m, m.fetchArch()
		}
		moved := moveFocus(m.Focus, m.State, m.stages, key, m.retired)
		if moved != m.Focus {
			m.Err = ""
			m.Focus = moved
			m.warExpanded = false
			m.Detail = nil
			m.openArtifacts = false
			if m.Focus.Issue != "" {
				return m, m.fetchDetail(m.Focus.Issue)
			}
		}
	}
	return m, nil
}

// setToast swaps the raised decision and rests the j/k cursor on the
// recommended option.
func (m *Model) setToast(d *projection.DecisionView) {
	m.Toast = d
	if d != nil {
		m.toastSel = d.Recommended
	} else {
		m.toastSel = 0
	}
}

func (m *Model) dismissToast() {
	if m.Toast == nil {
		return
	}
	if m.dismissed == nil {
		m.dismissed = map[int64]bool{}
	}
	m.dismissed[m.Toast.ID] = true
	m.setToast(nil)
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	copyValues := make(map[string]string, len(values))
	for key, value := range values {
		copyValues[key] = value
	}
	return copyValues
}

func (m Model) issueCommand(issueID, op string) tea.Cmd {
	if m.client == nil || issueID == "" {
		return nil
	}
	client := m.client
	return func() tea.Msg {
		r, err := client.Do(proto.Command{Op: op, IssueID: issueID})
		return commandMsg{response: r, err: err}
	}
}

func (m *Model) openLeverEditor(issueID string) tea.Cmd {
	if m.Detail != nil && m.Detail.Issue.ID == issueID {
		m.leverEditor = newLeverEditor(issueID, m.stages, m.Detail.Levers)
		return nil
	}
	m.wantLeverEditor = true
	if m.client == nil {
		m.leverEditor = newLeverEditor(issueID, m.stages, nil)
		m.wantLeverEditor = false
		return nil
	}
	return m.fetchDetail(issueID)
}

func (m *Model) cycleSelectedLever(delta int) {
	if m.leverEditor == nil || len(m.leverEditor.Stages) == 0 {
		return
	}
	stage := m.leverEditor.Stages[m.leverEditor.Sel]
	m.leverEditor.Matrix[stage] = cycleLever(m.leverEditor.Matrix[stage], delta)
}

func (m Model) applyLevers() tea.Cmd {
	if m.leverEditor == nil {
		return nil
	}
	values := cloneStringMap(m.leverEditor.Matrix)
	changed := false
	for _, stage := range m.leverEditor.Stages {
		if m.leverEditor.Original[stage] != m.leverEditor.Matrix[stage] {
			changed = true
			break
		}
	}
	if !changed {
		return func() tea.Msg { return leverApplyMsg{response: proto.Response{OK: true}, values: values} }
	}
	if m.client == nil {
		return func() tea.Msg { return leverApplyMsg{response: proto.Response{OK: true}, values: values} }
	}
	client := m.client
	issueID := m.leverEditor.IssueID
	stages := append([]string(nil), m.leverEditor.Stages...)
	original := cloneStringMap(m.leverEditor.Original)
	return func() tea.Msg {
		for _, stage := range stages {
			if original[stage] == values[stage] {
				continue
			}
			r, err := client.Do(proto.Command{Op: "set_lever", IssueID: issueID, Stage: stage, Lever: values[stage]})
			if err != nil {
				return leverApplyMsg{err: err}
			}
			if !r.OK {
				return leverApplyMsg{response: r}
			}
		}
		return leverApplyMsg{response: proto.Response{OK: true}, values: values}
	}
}

func (m Model) createIssue(modal modalState) tea.Cmd {
	if m.client == nil {
		return nil
	}
	flowName := strings.TrimSpace(modal.FlowName)
	if flowName == "" {
		flowName = "default"
	}
	preset := strings.TrimSpace(modal.Preset)
	if preset == "" {
		preset = "regular"
	}
	client := m.client
	return func() tea.Msg {
		r, err := client.Do(proto.Command{Op: "create_issue", Title: modal.Title, Body: modal.Body, Flow: flowName, Preset: preset, Priority: modal.Priority})
		if err != nil {
			return createIssueMsg{err: err}
		}
		if !r.OK {
			return createIssueMsg{response: r}
		}
		started, err := client.Do(proto.Command{Op: "start_issue", IssueID: r.IssueID})
		if err != nil {
			return createIssueMsg{err: err}
		}
		if !started.OK {
			return createIssueMsg{response: started}
		}
		return createIssueMsg{response: r}
	}
}

func (m Model) draftIssue(modal modalState) tea.Cmd {
	return m.modalCommand(modal, "draft_issue", "")
}

func (m Model) updateIssue(modal modalState) tea.Cmd {
	return m.modalCommand(modal, "update_issue", modal.EditID)
}

// modalCommand sends one modal-backed op; createIssue keeps its own start step.
func (m Model) modalCommand(modal modalState, op, issueID string) tea.Cmd {
	if m.client == nil {
		return nil
	}
	flowName := strings.TrimSpace(modal.FlowName)
	if flowName == "" {
		flowName = "default"
	}
	preset := strings.TrimSpace(modal.Preset)
	if preset == "" {
		preset = "regular"
	}
	client := m.client
	return func() tea.Msg {
		r, err := client.Do(proto.Command{Op: op, IssueID: issueID, Title: modal.Title,
			Body: modal.Body, Flow: flowName, Preset: preset, Priority: modal.Priority})
		return createIssueMsg{response: r, err: err}
	}
}

func (m *Model) openArtifactsFor(issueID string) tea.Cmd {
	m.Focus = focusIssue(m.State, m.stages, issueID, m.retired)
	m.Detail = nil
	m.openArtifacts = true
	if m.Toast != nil {
		m.dismissToast()
	}
	return m.fetchDetail(issueID)
}

func (m *Model) openEvidenceFor(issueID string, decisionID int64) tea.Cmd {
	m.Focus = focusIssue(m.State, m.stages, issueID, m.retired)
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
		m.events = append(m.events, ev)
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
		m.setToast(selected)
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

func (m Model) currentMode() string {
	if len(m.modes) == 0 {
		return ""
	}
	return m.modes[len(m.modes)-1]
}

func (m *Model) popMode() {
	if len(m.modes) == 0 {
		return
	}
	m.modes = m.modes[:len(m.modes)-1]
	m.doorLines = nil
	m.proposals = nil
	m.doorSel = 0
	m.shelfSel = 0
}

func (m *Model) updateDoorKey(key string) tea.Cmd {
	if key == "esc" {
		m.popMode()
		return nil
	}
	switch m.currentMode() {
	case "decisions":
		ds := decisionViews(m.State)
		switch key {
		case "j":
			m.doorSel = min(m.doorSel+1, max(0, len(ds)-1))
		case "k":
			m.doorSel = max(m.doorSel-1, 0)
		case "enter":
			if len(ds) > 0 {
				decision := ds[min(m.doorSel, len(ds)-1)]
				m.setToast(&decision)
				m.popMode()
			}
		default:
			if len(key) == 1 && key >= "1" && key <= "9" {
				index := int(key[0] - '1')
				if index < len(ds) {
					decision := ds[index]
					m.setToast(&decision)
					m.popMode()
				}
			}
		}
	case "tray":
		switch key {
		case "j":
			m.doorSel = min(m.doorSel+1, max(0, len(m.proposals)-1))
		case "k":
			m.doorSel = max(m.doorSel-1, 0)
		case "enter", "a":
			return m.resolveProposal(true)
		case "r":
			return m.resolveProposal(false)
		}
	case "shelf":
		items := m.shelfItems()
		switch key {
		case "j":
			m.shelfSel = min(m.shelfSel+1, max(0, len(items)-1))
		case "k":
			m.shelfSel = max(m.shelfSel-1, 0)
		case "enter":
			if m.shelfSel < len(items) && !items[m.shelfSel].Parked {
				delete(m.retired, items[m.shelfSel].ID)
				m.popMode()
			}
		}
	}
	return nil
}

func (m *Model) autoRetire(now time.Time) {
	if m.State == nil || m.retireAfter <= 0 {
		return
	}
	if m.retired == nil {
		m.retired = map[string]bool{}
	}
	for _, issueID := range m.State.ShippedToday {
		iv := m.State.Issues[issueID]
		if iv != nil && !iv.MergedAt.IsZero() && !now.Before(iv.MergedAt.Add(m.retireAfter)) {
			m.retired[issueID] = true
		}
	}
	m.refocusVisible()
}

// refocusVisible pulls focus off a lane that has just left the grid, so the
// rail can never keep rendering a lane the tower already dropped.
func (m *Model) refocusVisible() {
	if m.retired[m.Focus.Issue] {
		m.Focus = resolveFocus(m.Focus, m.State, m.stages, m.retired)
	}
}

func (m *Model) retireFocused() {
	if m.State == nil || m.Focus.Issue == "" {
		return
	}
	iv := m.State.Issues[m.Focus.Issue]
	if iv != nil && iv.Merged {
		if m.retired == nil {
			m.retired = map[string]bool{}
		}
		m.retired[iv.ID] = true
		m.refocusVisible()
	}
}

func (m Model) shelfItems() []shelfItem {
	if m.State == nil {
		return nil
	}
	items := make([]shelfItem, 0)
	seen := map[string]bool{}
	for _, issueID := range m.State.ShippedToday {
		if !m.retired[issueID] || seen[issueID] {
			continue
		}
		if iv := m.State.Issues[issueID]; iv != nil {
			items = append(items, shelfItem{ID: issueID, Title: iv.Title})
			seen[issueID] = true
		}
	}
	for _, issueID := range m.State.Parked {
		if seen[issueID] {
			continue
		}
		if iv := m.State.Issues[issueID]; iv != nil {
			items = append(items, shelfItem{ID: issueID, Title: iv.Title, Parked: true})
			seen[issueID] = true
		}
	}
	return items
}

func (m Model) fetchProposals() tea.Cmd {
	if m.client == nil {
		return nil
	}
	client := m.client
	return func() tea.Msg {
		r, err := client.Do(proto.Command{Op: "list_proposals"})
		if err != nil {
			return proposalsMsg{err: err}
		}
		if !r.OK {
			return proposalsMsg{err: errors.New(r.Error)}
		}
		return proposalsMsg{proposals: r.Proposals}
	}
}

func (m Model) resolveProposal(accept bool) tea.Cmd {
	if m.client == nil || len(m.proposals) == 0 || m.doorSel >= len(m.proposals) {
		return nil
	}
	client := m.client
	proposalID := m.proposals[m.doorSel].ID
	return func() tea.Msg {
		r, err := client.Do(proto.Command{Op: "resolve_proposal", ProposalID: proposalID, Accept: accept, Flow: "default", Preset: "regular"})
		if err == nil && r.OK && accept && r.IssueID != "" {
			_, err = client.Do(proto.Command{Op: "start_issue", IssueID: r.IssueID})
		}
		if err != nil {
			return proposalsMsg{err: err}
		}
		if !r.OK {
			return proposalsMsg{err: errors.New(r.Error)}
		}
		return proposalsMsg{}
	}
}

func (m Model) fetchTranscript() tea.Cmd {
	if m.client == nil || m.Focus.Issue == "" {
		return nil
	}
	client := m.client
	issueID := m.Focus.Issue
	return func() tea.Msg {
		r, err := client.Do(proto.Command{Op: "transcript_tail", IssueID: issueID, N: 200})
		if err != nil {
			return transcriptMsg{err: err}
		}
		if !r.OK {
			return transcriptMsg{err: errors.New(r.Error)}
		}
		return transcriptMsg{lines: r.Lines}
	}
}

// streamSubtitle names the lane and the stage whose output is on screen. The
// stage comes from the newest line's gutter, since the buffer spans stages.
func (m Model) streamSubtitle() string {
	tag := m.Focus.Issue
	if identity, ok := m.Ids[m.Focus.Issue]; ok && identity.Tag != "" {
		tag = identity.Tag + " " + m.Focus.Issue
	}
	tools := 0
	for _, line := range m.doorLines {
		text := line
		if _, rest, found := strings.Cut(line, streamGutterSep); found {
			text = rest
		}
		if strings.HasPrefix(text, streamToolPrefix) {
			tools++
		}
	}
	parts := []string{tag}
	for i := len(m.doorLines) - 1; i >= 0; i-- {
		if stage, _, found := strings.Cut(m.doorLines[i], streamGutterSep); found && stage != "" {
			parts = append(parts, stage)
			break
		}
	}
	parts = append(parts, fmt.Sprintf("%d line%s · %d tool call%s", len(m.doorLines), pluralSuffix(len(m.doorLines)), tools, pluralSuffix(tools)))
	if m.Detail != nil && m.Detail.Model != "" {
		parts = append(parts, m.Detail.Model)
	}
	return strings.Join(parts, " · ")
}

// writeHeaderRows writes the status sentence and the reserved notice row that
// every screen (grid, doors, help) keeps at the top.
func (m Model) writeHeaderRows(b *strings.Builder, width int) {
	b.WriteString(renderHeader(m.Overview, width))
	b.WriteByte('\n')
	b.WriteString(renderNoticeRow(m.State, m.Ids, width))
	b.WriteByte('\n')
}

func (m Model) View() string {
	layoutWidth := m.Width
	if layoutWidth <= 0 {
		layoutWidth = 120
	}
	railWidth := max(24, min(40, layoutWidth/3))
	towerWidth := max(1, layoutWidth-railWidth-1)
	tower := renderTowerConfigured(m.State, m.stages, m.Ids, m.Focus, m.aliases, m.reducedMotion, m.ticks, towerWidth, m.warExpanded, m.retired)
	if m.rows {
		tower = renderRowsConfigured(m.State, m.stages, m.Ids, m.Focus, m.reducedMotion, m.ticks, towerWidth, m.retired)
	}
	switch m.currentMode() {
	case "decisions":
		tower = renderDecisionsDoor(decisionViews(m.State), m.Ids, m.doorSel, layoutWidth)
	case "tray":
		tower = renderProposalsDoor(m.proposals, m.doorSel, layoutWidth)
	case "timeline":
		tower = renderTextDoor("TIMELINE", m.doorLines, layoutWidth)
	case "transcript":
		tower = renderStreamDoor(m.streamSubtitle(), m.doorLines, layoutWidth)
	case "shelf":
		tower = renderShelf(m.shelfItems(), m.Ids, layoutWidth)
	}
	if m.currentMode() != "" {
		// Doors replace the grid and rail but retain the header and footer.
		var b strings.Builder
		m.writeHeaderRows(&b, layoutWidth)
		b.WriteString(tower)
		b.WriteString("\n\n")
		bindings := [][2]string{{"j/k", "select"}, {"enter", "open"}, {"esc", "back"}, {"q", "quit"}}
		if m.currentMode() == "tray" {
			bindings = [][2]string{{"j/k", "select"}, {"enter", "accept → new issue"}, {"r", "reject"}, {"esc", "back"}, {"q", "quit"}}
		}
		b.WriteString(renderKeybar(layoutWidth, bindings, errText(m.Err)))
		screen := b.String()
		if m.help {
			return m.composite(screen, renderHelpOverlay(layoutWidth), layoutWidth)
		}
		return screen
	}
	var overlayBox string
	if m.modal != nil {
		overlayBox = renderModal(*m.modal, layoutWidth)
	} else if m.confirm != nil {
		overlayBox = renderConfirm(m.confirm.Prompt, layoutWidth)
	} else if m.backlog != nil {
		entries := backlogEntries(m.State)
		m.backlog.Sel = min(m.backlog.Sel, max(0, len(entries)-1))
		overlayBox = renderBacklog(entries, m.backlog.Sel, layoutWidth)
	} else if m.leverEditor != nil {
		overlayBox = renderLeverEditor(m.leverEditor.Stages, m.leverEditor.Matrix, m.leverEditor.Sel)
	} else if m.pager.Mode == "artifacts" {
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
		// The toast never replaces the grid — spatial memory rule: the tower
		// stays visible and the toast stacks beneath it, above the shelf.
		toast := renderToast(*m.Toast, m.Ids[m.Toast.IssueID], m.toastSel, m.acceptStreak, towerWidth)
		tower = lipgloss.JoinVertical(lipgloss.Left, tower, "", toast)
	}
	var body string
	if m.archMode == "full" {
		body = renderArchWithState(m.Arch, m.State, m.Ids, layoutWidth, m.Height, m.archSel, m.archFilter)
	} else {
		right := renderRail(m.State, m.Ids, m.Detail, railWidth)
		if m.archMode == "pane" {
			right = renderArchWithState(m.Arch, m.State, m.Ids, railWidth, m.Height, m.archSel, m.archFilter)
		}
		// Pad the tower block to its full column so the rail starts at a
		// fixed x regardless of the longest tower line.
		tower = lipgloss.NewStyle().Width(towerWidth).Render(tower)
		body = lipgloss.JoinHorizontal(lipgloss.Top, tower, right)
	}
	var b strings.Builder
	m.writeHeaderRows(&b, layoutWidth)
	b.WriteString(body)
	if shelf := renderShelf(m.shelfItems(), m.Ids, layoutWidth); shelf != "" {
		b.WriteString("\n\n")
		b.WriteString(shelf)
	}
	b.WriteString("\n\n")
	mainBindings := [][2]string{
		{"j/k", "floors"}, {"tab", "next"}, {"p", "pause/resume"}, {"x", "kill"},
		{"R", "retry"}, {"T", "stream"}, {"L", "levers"}, {"?", "help"}, {"q", "quit"},
	}
	right := errText(m.Err)
	if m.archMode == "full" {
		mainBindings = [][2]string{{"j/k", "module"}, {"/", "filter"}, {"a/esc", "back"}}
		if right == "" {
			right = archFooterDetail(m.Arch, m.State, m.Ids, m.archSel, m.archFilter)
		}
	}
	b.WriteString(renderKeybar(layoutWidth, mainBindings, right))
	screen := b.String()
	switch {
	case m.help:
		return m.composite(screen, renderHelpOverlay(layoutWidth), layoutWidth)
	case overlayBox != "":
		return m.composite(screen, overlayBox, layoutWidth)
	}
	return screen
}

// composite centers box over screen, dimming the base to the full terminal
// height (or the screen's own height if it is taller).
func (m Model) composite(screen, box string, width int) string {
	return overlayCenter(screen, box, width, max(m.Height, lipgloss.Height(screen)))
}
