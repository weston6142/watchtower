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
	"github.com/weston6142/watchtower/internal/attach"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/deps"
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
	stream           streamState
	events           []core.Event
	archMode         string
	archSel          int
	archFilter       string
	help             bool
	rows             bool
	warExpanded      bool
	retireAfter      time.Duration
	retired          map[string]bool
	// dayStart is local midnight of the current day, refreshed on every tick.
	// Injected rather than read from the clock so render paths stay
	// deterministic and the goldens stay byte-comparable.
	dayStart        time.Time
	shelfSel        int
	modal           *modalState
	backlog         *backlogState
	confirm         *confirmState
	decisionEditor  *decisionEditor
	leverEditor     *leverEditorState
	wantLeverEditor bool
	setup           *setupState
	wantSetup       bool
	aliases         map[string]string
	reducedMotion   bool
	herdrReporter   overviewReporter
	ticks           int
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

type decisionEditor struct {
	DecisionID int64
	Value      string
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

type setupMsg struct {
	view *proto.SetupView
	err  error
}

type setupPromptMsg struct {
	stage string
	pkg   string
	lines []string
	err   error
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
		if m.followingTranscript() {
			cmds = append(cmds, m.fetchTranscript())
		}
		return m, tea.Batch(cmds...)
	case Msg:
		m = m.applyEvents(msg.Events)
		if m.currentMode() == "timeline" {
			m.doorLines = humanizeEvents(m.events, m.Focus.Issue)
		}
		if m.followingTranscript() {
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
		// The gate above stops new fetches; this drops the one already in
		// flight when the operator scrolled up. The mode check matters: a late
		// message arriving under the timeline door is not this door's to eat.
		if m.currentMode() == "transcript" && !m.stream.Follow && len(m.doorLines) > 0 {
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
	case setupMsg:
		m.wantSetup = false
		if msg.err != nil {
			m.Err = msg.err.Error()
			return m, nil
		}
		m.setup = &setupState{View: msg.view, Expanded: map[string]bool{}}
		m.setup.clampTop(m.Height)
		return m, nil
	case setupPromptMsg:
		if m.setup == nil {
			// f closed the panel while the fetch was in flight. Opening the
			// pager now would strand it with no outline underneath and Files
			// nil, so esc would drop the operator into an empty "no artifacts"
			// box instead of back where they were.
			return m, nil
		}
		if msg.err != nil {
			m.Err = msg.err.Error()
			return m, nil
		}
		// The setup prompt has no file behind it, so readArtifact cannot serve
		// it. Title is set here because renderPager prints a bare position
		// indicator when it is empty.
		m.pager = pagerState{
			Mode:  "pager",
			Title: msg.stage + " · " + msg.pkg + " · prompt",
			Lines: msg.lines,
		}
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
		if m.modal != nil && (m.modal.EditID != "" || m.modal.FromBacklog) {
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
		if m.decisionEditor != nil {
			switch key {
			case "esc":
				m.decisionEditor = nil
			case "enter":
				if strings.TrimSpace(m.decisionEditor.Value) == "" {
					m.Err = "response is required"
					return m, nil
				}
				return m, m.answerDecisionText(m.decisionEditor.Value)
			case "backspace":
				runes := []rune(m.decisionEditor.Value)
				if len(runes) > 0 {
					m.decisionEditor.Value = string(runes[:len(runes)-1])
				}
			default:
				if key != "" && !strings.ContainsAny(key, "\n\r\t") {
					m.decisionEditor.Value += key
				}
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
				if m.modal.EditID != "" || m.modal.FromBacklog {
					m.backlog = &backlogState{}
				}
				m.modal = nil
			case "enter", "ctrl+s":
				if strings.TrimSpace(m.modal.Title) == "" {
					m.Err = "title is required"
					return m, nil
				}
				if m.client == nil {
					// Nothing to send, but the way back is still owed: without
					// this the operator is dropped on the grid whenever no
					// daemon is attached.
					if m.modal.EditID != "" || m.modal.FromBacklog {
						m.backlog = &backlogState{}
					}
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
			case "n":
				// Ungated by Sel, unlike enter/l/X: filing the first draft into an
				// empty backlog is the case the empty-state hint advertises. This
				// has to live inside the backlog switch — the confirm branch above
				// binds n as "no".
				m.Err = ""
				m.modal = &modalState{FlowName: "default", Preset: "regular", FromBacklog: true}
				m.backlog = nil
			case "enter":
				if m.backlog.Sel < len(entries) {
					iv := entries[m.backlog.Sel]
					m.Err = ""
					// The stored int is shown as-is, even when it falls outside
					// the named levels: renumbering an issue just because
					// someone opened it would lose data silently. The first
					// h/l moves it into the set.
					m.modal = &modalState{EditID: iv.ID, Title: iv.Title, Body: iv.Body,
						FlowName: iv.Flow, Preset: iv.Preset, Priority: iv.Priority,
						DependsOn: strings.Join(iv.DependsOn, ", "),
						Attach:    strings.Join(iv.Attachments, ", "), OrigAttach: iv.Attachments}
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
		// The && m.pager.Mode == "" is load-bearing, not defensive: the
		// pager's key branch sits below this one, so a bare m.setup != nil
		// arm would swallow j/k/esc and the prompt body could not scroll.
		if m.setup != nil && m.pager.Mode == "" {
			switch key {
			case "esc", "f":
				m.setup = nil
			case "j":
				m.setup.Sel++
				m.setup.clampTop(m.Height)
			case "k":
				m.setup.Sel--
				m.setup.clampTop(m.Height)
			case "g":
				m.setup.Sel = 0
				m.setup.clampTop(m.Height)
			case "G":
				m.setup.Sel = m.setup.selectableCount() - 1
				m.setup.clampTop(m.Height)
			case "enter":
				return m, m.setupEnter()
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
			// Follow cannot carry a safe zero value — it is derived, and g
			// legitimately produces {Top: 0, Follow: false} — so every site
			// that opens the door says so.
			m.stream = streamState{Follow: true}
			return m, m.fetchTranscript()
		case "u":
			m.modes = append(m.modes, "shelf")
			m.shelfSel = 0
			return m, nil
		case "f":
			// The grid only: this switch sits below the door check, so f
			// inside a door is the door's to ignore.
			m.Err = ""
			// Two statements, not `return m, m.openSetup()`: openSetup has a
			// pointer receiver and sets m.wantSetup, and the order of the
			// copy into the result slot versus the call is unspecified. This
			// is the shape openArtifactsFor already uses.
			cmd := m.openSetup()
			return m, cmd
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
			if m.Toast.Kind == "freeform" {
				switch key {
				case "y":
					return m, m.answerDecisionText(m.Toast.RecommendedResponse)
				case "enter":
					m.decisionEditor = &decisionEditor{
						DecisionID: m.Toast.ID, Value: m.Toast.RecommendedResponse}
				case "esc":
					m.dismissToast()
				case "o":
					return m, m.openEvidenceFor(m.Toast.IssueID, m.Toast.ID)
				}
				return m, nil
			}
			switch key {
			case "y":
				if !m.evidenceOpened[m.Toast.ID] {
					m.acceptStreak++
				}
				return m, m.answerDecision(m.Toast.Recommended)
			case "j":
				last := len(m.Toast.Options) - 1
				if m.Toast.AllowFreeform {
					last = len(m.Toast.Options)
				}
				m.toastSel = min(m.toastSel+1, max(0, last))
			case "k":
				m.toastSel = max(m.toastSel-1, 0)
			case "enter":
				if m.Toast.AllowFreeform && m.toastSel == len(m.Toast.Options) {
					m.decisionEditor = &decisionEditor{DecisionID: m.Toast.ID}
					return m, nil
				}
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
		m.decisionEditor = nil
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

// openSetup fetches the focused lane's setup outline, or the repo default when
// nothing is focused. Same fetch-then-open shape as the lever editor: the
// panel opens when the response lands, never before.
//
// wantSetup records that f was accepted. Unlike wantLeverEditor it gates
// nothing — setupMsg has a single producer, so there is no second response to
// disambiguate — but with no daemon attached the fetch below is a no-op, and
// the flag is the only sign the grid handled the key rather than a door.
func (m *Model) openSetup() tea.Cmd {
	m.wantSetup = true
	if m.client == nil {
		// Fixtures and tests pose m.setup directly. With no daemon there is no
		// running config to report, so f only records the intent.
		return nil
	}
	client := m.client
	issueID := m.Focus.Issue
	return func() tea.Msg {
		r, err := client.Do(proto.Command{Op: "setup_outline", IssueID: issueID})
		if err != nil {
			return setupMsg{err: err}
		}
		if !r.OK {
			return setupMsg{err: errors.New(r.Error)}
		}
		return setupMsg{view: r.Setup}
	}
}

// setupEnter toggles a stage row or fetches the selected agent's prompt — the
// same expand-or-open path the artifact list already uses.
func (m *Model) setupEnter() tea.Cmd {
	if m.setup == nil {
		return nil
	}
	row, ok := m.setup.selectedRow()
	if !ok {
		return nil
	}
	if row.Kind == setupRowStage {
		m.setup.Expanded[row.Stage] = !m.setup.Expanded[row.Stage]
		m.setup.clampTop(m.Height)
		return nil
	}
	return m.fetchSetupPrompt(row.Stage, row.Pkg)
}

func (m Model) fetchSetupPrompt(stage, pkg string) tea.Cmd {
	if m.client == nil || m.setup == nil || m.setup.View == nil {
		return nil
	}
	client := m.client
	issueID, flowName := m.setup.View.IssueID, m.setup.View.Flow
	return func() tea.Msg {
		r, err := client.Do(proto.Command{Op: "setup_prompt", Stage: stage, Package: pkg,
			IssueID: issueID, Flow: flowName})
		if err != nil {
			return setupPromptMsg{stage: stage, pkg: pkg, err: err}
		}
		if !r.OK {
			return setupPromptMsg{stage: stage, pkg: pkg, err: errors.New(r.Error)}
		}
		return setupPromptMsg{stage: stage, pkg: pkg, lines: r.Lines}
	}
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

// resolveModalAttach turns the attach field's text into what the daemon wants:
// absolute paths, plus the bare names of attachments being retained. The cwd
// and home come from the client because the daemon has neither.
func resolveModalAttach(modal modalState) ([]string, error) {
	if strings.TrimSpace(modal.Attach) == "" {
		return nil, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	return attach.Resolve(modal.Attach, modal.OrigAttach, cwd, home)
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
	attachments, err := resolveModalAttach(modal)
	if err != nil {
		return func() tea.Msg { return createIssueMsg{err: err} }
	}
	client := m.client
	return func() tea.Msg {
		r, err := client.Do(proto.Command{Op: "create_issue", Title: modal.Title,
			Body: modal.Body, Flow: flowName, Preset: preset, Priority: modal.Priority,
			Attach: attachments, DependsOn: parseDependencies(modal.DependsOn)})
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
	attachments, err := resolveModalAttach(modal)
	if err != nil {
		return func() tea.Msg { return createIssueMsg{err: err} }
	}
	client := m.client
	return func() tea.Msg {
		r, err := client.Do(proto.Command{Op: op, IssueID: issueID, Title: modal.Title,
			Body: modal.Body, Flow: flowName, Preset: preset, Priority: modal.Priority,
			Attach: attachments, DependsOn: parseDependencies(modal.DependsOn)})
		return createIssueMsg{response: r, err: err}
	}
}

func parseDependencies(value string) []string {
	return deps.Normalize(strings.Split(value, ","))
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
		if m.setup != nil {
			// Entered from the setup panel, not the artifact list: Files is nil,
			// so the artifacts branch below would drop the operator into an
			// empty "no artifacts" box. Clear the pager and land back on the
			// outline, with Sel and Top untouched.
			m.pager = pagerState{}
			return nil
		}
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
		r, err := client.Do(proto.Command{Op: "answer_decision", DecisionID: decisionID, Option: &option})
		return answerMsg{decisionID: decisionID, response: r, err: err}
	}
}

func (m Model) answerDecisionText(text string) tea.Cmd {
	if m.Toast == nil || m.client == nil {
		return nil
	}
	client := m.client
	decisionID := m.Toast.ID
	return func() tea.Msg {
		r, err := client.Do(proto.Command{
			Op: "answer_decision", DecisionID: decisionID, Text: text})
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

// followingTranscript is the refetch gate. Both refetch sites go through it so
// they cannot disagree about when the transcript is live: m.doorLines is
// replaced wholesale on every tick and every event batch from an evicting ring
// buffer, so an offset alone does not anchor to content — freezing the fetch is
// what makes reading older output stable rather than merely tolerable.
func (m Model) followingTranscript() bool {
	return m.currentMode() == "transcript" && m.stream.Follow
}

// layoutWidth is the one place the non-positive-width fallback lives: the whole
// screen — rail, tower, keybar and the stream door's inner width — has to agree
// on the number, and a key arm computing it separately from View is exactly the
// disagreement these helpers exist to prevent.
func (m Model) layoutWidth() int {
	if m.Width <= 0 {
		return 120
	}
	return m.Width
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
	m.stream = streamState{Follow: true}
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
	case "transcript":
		switch key {
		case "j", "k", "d", "u", "g", "G":
		default:
			// Every other key leaves the position alone. Routing every key
			// through scroll would re-clamp Top against a total that ring
			// eviction may have shrunk, silently re-attaching a held view on
			// an unrelated keypress.
			return nil
		}
		before := m.stream.Follow
		total := len(streamBody(m.doorLines, streamInner(m.layoutWidth())))
		m.stream = m.stream.scroll(key, streamRows(m.Height), total)
		if !before && m.stream.Follow {
			// Returning to live must not look stalled for up to a tick.
			return m.fetchTranscript()
		}
		return nil
	}
	return nil
}

// staleShipped reports whether a merged lane was merged before the current
// day began. A zero MergedAt and a zero dayStart both fail open: a lane is
// never hidden on a missing timestamp.
func (m *Model) staleShipped(iv *projection.IssueView) bool {
	return iv != nil && iv.Merged && !iv.MergedAt.IsZero() && iv.MergedAt.Before(m.dayStart)
}

func (m *Model) autoRetire(now time.Time) {
	if m.State == nil || m.retireAfter <= 0 {
		return
	}
	if m.retired == nil {
		m.retired = map[string]bool{}
	}
	for _, issueID := range m.State.Shipped {
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
	for _, issueID := range m.State.Shipped {
		if !m.retired[issueID] || seen[issueID] {
			continue
		}
		if iv := m.State.Issues[issueID]; iv != nil {
			// The heading says "SHIPPED today", so earlier days' merges are
			// not on this shelf.
			if m.staleShipped(iv) {
				continue
			}
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
	layoutWidth := m.layoutWidth()
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
		tower = renderStreamDoor(m.streamSubtitle(), m.doorLines, m.stream, layoutWidth, m.Height)
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
		switch m.currentMode() {
		case "tray":
			bindings = [][2]string{{"j/k", "select"}, {"enter", "accept → new issue"}, {"r", "reject"}, {"esc", "back"}, {"q", "quit"}}
		case "transcript":
			// A reading surface, not a list: "select"/"open" describe neither
			// what the keys do here nor anything the door can act on.
			bindings = [][2]string{{"j/k", "scroll"}, {"d/u", "page"}, {"g/G", "oldest/newest"}, {"esc", "back"}, {"q", "quit"}}
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
		overlayBox = renderBacklog(entries, m.backlog.Sel, layoutWidth, m.Height)
	} else if m.leverEditor != nil {
		overlayBox = renderLeverEditor(m.leverEditor.Stages, m.leverEditor.Matrix, m.leverEditor.Sel)
	} else if m.setup != nil && m.pager.Mode == "" {
		// The && is load-bearing: View() is a single if/else-if chain, so a bare
		// m.setup arm placed above the pager arms wins unconditionally and the
		// prompt would never render.
		overlayBox = renderSetup(*m.setup, layoutWidth, m.Height)
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
	} else if m.decisionEditor != nil && m.Toast != nil {
		editor := renderDecisionEditor(*m.Toast, *m.decisionEditor, towerWidth)
		tower = lipgloss.JoinVertical(lipgloss.Left, tower, "", editor)
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
