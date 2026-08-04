package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/evidence"
	"github.com/weston6142/watchtower/internal/projection"
	"github.com/weston6142/watchtower/internal/proto"
	"github.com/weston6142/watchtower/internal/review"
	"github.com/weston6142/watchtower/internal/store"
)

func boundedLines(lines []string, width int) string {
	if width <= 0 {
		return strings.Join(lines, "\n")
	}
	bounded := make([]string, len(lines))
	for i, line := range lines {
		bounded[i] = truncate(line, width)
	}
	return strings.Join(bounded, "\n")
}

// wrapLines word-wraps each line to width instead of truncating; long
// prose (decision questions, rationale) must stay fully readable.
func wrapLines(lines []string, width int) string {
	if width <= 0 {
		return strings.Join(lines, "\n")
	}
	wrapped := make([]string, len(lines))
	for i, line := range lines {
		wrapped[i] = ansi.Wrap(line, width, "")
	}
	return strings.Join(wrapped, "\n")
}

// wrapIndent wraps text with a marker on the first line and a hanging
// indent of the same width on continuation lines.
func wrapIndent(text string, width int, first string) []string {
	rest := strings.Repeat(" ", lipgloss.Width(first))
	inner := max(1, width-lipgloss.Width(first))
	parts := strings.Split(ansi.Wrap(text, inner, ""), "\n")
	out := make([]string, len(parts))
	for i, part := range parts {
		if i == 0 {
			out[i] = first + part
		} else {
			out[i] = rest + part
		}
	}
	return out
}

func decisionContextLines(context *decision.DecisionContext, width int) []string {
	if context == nil {
		return nil
	}
	width = max(1, width)
	var lines []string
	lines = append(lines, wrapIndent(context.TaskSummary, width, "task · ")...)
	lines = append(lines, wrapIndent(context.AgentLabel(), width, "agent · ")...)
	return lines
}

func decisionReviewLines(target *review.Target, width int) []string {
	if target == nil {
		return nil
	}
	width = max(1, width)
	lines := []string{"artifact review"}
	lines = append(lines, wrapIndent(fmt.Sprintf("checkpoint: %d", target.CheckpointID), width, "")...)
	lines = append(lines, wrapIndent("artifact_version: "+target.ArtifactVersion, width, "")...)
	if target.NextStage != "" {
		lines = append(lines, wrapIndent("next: "+target.NextStage, width, "")...)
	}
	for _, artifact := range target.Artifacts {
		lines = append(lines, wrapIndent(artifact.Name+": "+artifact.SHA256, width, "")...)
	}
	return lines
}

func decisionPolicyLines(policy *review.ResolvedPolicy, approval *review.ApprovalProvenance, width int) []string {
	if policy == nil {
		return nil
	}
	width = max(1, width)
	requirement := "human approval required"
	if policy.PolicyAutoApproval && approval != nil && approval.Kind == review.ApprovalPolicy {
		requirement = "approved automatically by policy"
	} else if policy.PolicyAutoApproval {
		requirement = "policy approval pending"
	}
	lines := []string{"review policy: " + requirement}
	lines = append(lines, wrapIndent("mode: "+policy.Mode, width, "")...)
	lines = append(lines, wrapIndent("policy: "+policy.PolicyID+"@"+policy.PolicyVersion, width, "")...)
	if approval != nil {
		switch approval.Kind {
		case review.ApprovalHuman:
			actor := approval.ActorID
			if actor == "" {
				actor = "operator"
			}
			lines = append(lines, wrapIndent("approved by: "+actor, width, "")...)
		case review.ApprovalPolicy:
			id := approval.PolicyID
			version := approval.PolicyVersion
			if id == "" {
				id = policy.PolicyID
			}
			if version == "" {
				version = policy.PolicyVersion
			}
			lines = append(lines, wrapIndent("policy approval: "+id+"@"+version, width, "")...)
		}
	}
	return lines
}

func issuePolicyLines(iv *projection.IssueView, width int) []string {
	if iv == nil || iv.ReviewPolicy.PolicyID == "" {
		return nil
	}
	lines := decisionPolicyLines(&iv.ReviewPolicy, iv.Approval, width)
	if iv.ReviewStatus != "" && iv.ReviewStatus != "pending" {
		lines = append(lines, wrapIndent("status: "+strings.ReplaceAll(iv.ReviewStatus, "_", " "), max(1, width), "")...)
	}
	return lines
}

// renderRail draws the focused issue's plain-language FOCUS panel above the
// pending decision queue. Paths and session IDs intentionally stay here: they
// are diagnostic details, not grid copy.
func renderRail(st *projection.State, ids map[string]Identity, focusID string, det *proto.IssueDetail, width int) string {
	inner := max(16, width-4) // border + padding
	var lines []string
	var issueID, title, flow, state, lastError string
	var tokens, attempt, attemptOf int
	var cleanup []string
	var metadata *proto.IssueDetail
	if st != nil && focusID != "" {
		if iv := st.Issues[focusID]; iv != nil {
			issueID, title, flow, state = iv.ID, iv.Title, iv.Flow, iv.State
			tokens, lastError = iv.Tokens, iv.LastError
			attempt, attemptOf, cleanup = iv.Attempt, iv.AttemptOf, iv.Cleanup
			if det != nil && det.Issue.ID == focusID {
				metadata = det
			}
		}
	} else if det != nil {
		issueID, title, flow, state = det.Issue.ID, det.Issue.Title, det.Issue.Flow, det.Issue.State
		tokens, lastError = det.Tokens, det.LastError
		attempt, attemptOf, cleanup = det.Attempt, det.AttemptOf, det.Cleanup
		metadata = det
	}
	if issueID != "" {
		identity := ids[issueID]
		lines = append(lines,
			fmt.Sprintf("%s %s · %s", identity.Tag, issueID, title),
			fmt.Sprintf("%s · %s", flow, focusStatus(state)),
		)
		model, effort := "", ""
		if metadata != nil {
			model, effort = metadata.Model, metadata.Effort
		}
		if model == "" {
			model = "cli default"
		}
		if effort == "" {
			effort = "default"
		}
		lines = append(lines, "model "+model+" · effort "+effort)
		if strings.HasPrefix(state, "failed") {
			lines = append(lines, fmt.Sprintf("error: %s · attempt %d of %d", lastError, attempt, attemptOf))
		}
		if state == "cleanup_needed" {
			lines = append(lines, "cleanup needed: "+strings.Join(cleanup, ", "))
		}
		if metadata != nil && metadata.IntegrationState == store.IntegrationPreserved {
			lines = append(lines,
				"preserved: "+metadata.Branch,
				"worktree: "+metadata.Worktree,
			)
		}
		if metadata != nil && metadata.Budget > 0 {
			percent := tokens * 100 / metadata.Budget
			percent = max(0, min(100, percent))
			const budgetBarWidth = 4
			filled := (percent*budgetBarWidth + 50) / 100 // +50 rounds to nearest cell
			bar := strings.Repeat("▰", filled) + strings.Repeat("▱", budgetBarWidth-filled)
			cost := ""
			if metadata.Dollars > 0 {
				cost = fmt.Sprintf(" ($%.2f)", metadata.Dollars)
			}
			lines = append(lines, fmt.Sprintf("budget %s %d%%%s", bar, percent, cost))
		} else {
			lines = append(lines, "spent "+compactTokens(tokens)+" tokens")
		}
		if metadata != nil {
			if leverLine := renderLeverLine(metadata.Levers); leverLine != "" {
				lines = append(lines, leverLine)
			}
			for i := len(metadata.Runs) - 1; i >= 0; i-- {
				run := metadata.Runs[i]
				if run.Worktree != "" || run.SessionID != "" {
					lines = append(lines, themeDim.Render(fmt.Sprintf("%s · session %s", run.Worktree, run.SessionID)))
					break
				}
			}
		}
		if st != nil {
			if iv := st.Issues[focusID]; iv != nil {
				if reviewLines := issuePolicyLines(iv, inner); len(reviewLines) > 0 {
					lines = append(lines, reviewLines...)
				}
			}
		}
	}
	lines = append(lines, "", "DECISION QUEUE")
	if st == nil || len(st.Decisions) == 0 {
		lines = append(lines, themeDim.Render("—"))
	} else {
		idsInQueue := make([]int64, 0, len(st.Decisions))
		for id := range st.Decisions {
			idsInQueue = append(idsInQueue, id)
		}
		sort.Slice(idsInQueue, func(i, j int) bool { return idsInQueue[i] < idsInQueue[j] })
		for i, id := range idsInQueue {
			d := st.Decisions[id]
			identity := ids[d.IssueID]
			mark := "  "
			if i == 0 {
				mark = "▶ "
			}
			lines = append(lines, fmt.Sprintf("%s[%d] %s %s — %s", mark, d.ID, identity.Tag, d.Stage, d.Question))
		}
	}
	t := activeTheme
	band := lipgloss.NewStyle().Foreground(t.Bright).Background(t.Bg2).Bold(true).Render(" FOCUS ")
	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(t.Dimmer).
		Padding(0, 1).
		Width(width - 2).
		Render(band + "\n" + boundedLines(lines, inner))
}

func focusStatus(state string) string {
	switch {
	case state == "claimed":
		return "claimed · external session"
	case state == "verifying":
		return "verifying"
	case state == "waiting:integration":
		return "verified · queued"
	case state == "integrating":
		return "integrating"
	case state == "failed:finalize":
		return "finalization failed"
	case strings.HasPrefix(state, "running"):
		return "building"
	case strings.HasPrefix(state, "queued"):
		return "queued"
	case strings.HasPrefix(state, "waiting"):
		return "needs you"
	case strings.HasPrefix(state, "failed"):
		return "failed"
	case strings.HasPrefix(state, "paused"):
		return "paused"
	case state == "cleanup_needed":
		return "shipped · cleanup needed"
	case state == "done":
		return "shipped"
	default:
		return state
	}
}

func renderLeverLine(levers map[string]string) string {
	if len(levers) == 0 {
		return ""
	}
	stages := orderedLeverStages(levers)
	parts := make([]string, 0, len(stages))
	for _, stage := range stages {
		value := levers[stage]
		label := value
		switch value {
		case "yolo":
			label = "auto"
		case "strict":
			label = "you"
		}
		letter := "?"
		if stage != "" {
			letter = strings.ToUpper(string([]rune(stage)[0]))
		}
		parts = append(parts, letter+":"+label)
	}
	return "levers " + strings.Join(parts, " ")
}

func orderedLeverStages(levers map[string]string) []string {
	preferred := []string{"brainstorm", "spec", "plan", "execute", "review", "merge"}
	seen := map[string]bool{}
	stages := make([]string, 0, len(levers))
	for _, stage := range preferred {
		if _, ok := levers[stage]; ok {
			stages = append(stages, stage)
			seen[stage] = true
		}
	}
	remaining := make([]string, 0, len(levers)-len(stages))
	for stage := range levers {
		if !seen[stage] {
			remaining = append(remaining, stage)
		}
	}
	sort.Strings(remaining)
	return append(stages, remaining...)
}

// renderToast draws the raised decision as a self-contained card: banded
// header (tag, title, reversibility verdict), question + why, bordered
// selectable choice rows and a note row, with the ★ recommendation, and a
// chip keybar. sel is the choice or note row the j/k cursor is on.
func renderToast(d projection.DecisionView, id Identity, sel, streak, width int) string {
	inner := max(20, width-8)
	t := activeTheme
	dim := lipgloss.NewStyle().Foreground(t.Dim)
	lines := []string{}
	if context := decisionContextLines(d.Context, inner); len(context) > 0 {
		lines = append(lines, context...)
		lines = append(lines, "")
	}
	if review := decisionReviewLines(d.Review, inner); len(review) > 0 {
		lines = append(lines, review...)
		lines = append(lines, "")
	}
	if policy := decisionPolicyLines(d.ReviewPolicy, d.Approval, inner); len(policy) > 0 {
		lines = append(lines, policy...)
		lines = append(lines, "")
	}
	lines = append(lines, wrapIndent(d.Question, inner, "")...)
	if d.Why != "" {
		for _, line := range wrapIndent(d.Why, inner, "why · ") {
			lines = append(lines, dim.Render(line))
		}
	}
	lines = append(lines, "")
	if d.Kind == "freeform" {
		lines = append(lines, strings.Split(renderOption(
			d.RecommendedResponse, "", true, true, inner), "\n")...)
	} else {
		for i, option := range d.Options {
			consequence := ""
			if i < len(d.Consequences) {
				consequence = d.Consequences[i]
			}
			lines = append(lines, strings.Split(renderOption(option, consequence,
				i == sel, i == d.Recommended, inner), "\n")...)
		}
		lines = append(lines, strings.Split(renderOption(
			"Add note...", "send feedback instead of selecting an option", sel == len(d.Options), false, inner), "\n")...)
	}
	if streak >= 3 {
		lines = append(lines, "", dim.Render(fmt.Sprintf("you've accepted %d recommendations in a row without opening evidence", streak)))
	}
	hint := keyChip("enter") + dim.Render(" edit  ") +
		keyChip("y") + dim.Render(" accept ★  ")
	if d.Kind != "freeform" {
		hint = keyChip("j/k") + dim.Render(" choose  ") +
			keyChip("enter") + dim.Render(" select  ") +
			keyChip("y") + dim.Render(" accept ★  ")
	}
	hint +=
		keyChip("o") + dim.Render(" evidence  ") +
			keyChip("1..9") + dim.Render(" by number  ") +
			keyChip("esc") + dim.Render(" dismiss")
	lines = append(lines, "", hint)
	verdict := reversibleVerdict(d.Reversible)
	title := fmt.Sprintf("DECISION %d · %s %s", d.ID, id.Tag, d.Stage)
	return renderBox(title, verdict, " esc dismiss ", strings.Join(lines, "\n"))
}

func renderDecisionEditor(d projection.DecisionView, editor decisionEditor, width int) string {
	inner := max(20, width-8)
	value := editor.Value
	if value == "" {
		value = "…"
	}
	lines := decisionContextLines(d.Context, inner)
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	if review := decisionReviewLines(d.Review, inner); len(review) > 0 {
		lines = append(lines, review...)
		lines = append(lines, "")
	}
	if policy := decisionPolicyLines(d.ReviewPolicy, d.Approval, inner); len(policy) > 0 {
		lines = append(lines, policy...)
		lines = append(lines, "")
	}
	lines = append(lines, wrapIndent(d.Question, inner, "")...)
	lines = append(lines, "", lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
		BorderForeground(activeTheme.Accent).Padding(0, 1).Width(inner-4).
		Render(value+"█"), "", keyChip("enter")+themeDim.Render(" submit  ")+keyChip("esc")+themeDim.Render(" choices"))
	content := strings.Join(lines, "\n")
	return renderBox(fmt.Sprintf("RESPONSE %d · %s", d.ID, d.Stage), "", " editing ", content)
}

// reversibleVerdict compresses the reversibility text for the card band,
// colored Ok when reversal is cheap and Warn otherwise.
func reversibleVerdict(reversible string) string {
	if reversible == "" {
		return ""
	}
	t := activeTheme
	style := lipgloss.NewStyle().Foreground(t.Warn).Background(t.Bg2)
	lower := strings.ToLower(reversible)
	if strings.Contains(lower, "cheap") || strings.Contains(lower, "easy") {
		style = style.Foreground(t.Ok)
	}
	return style.Render("↺ " + truncate(reversible, 48))
}

// renderOption is one bordered selectable decision row: radio glyph,
// headline (★ when recommended), dim consequence line inside the border.
func renderOption(text, consequence string, selected, recommended bool, width int) string {
	t := activeTheme
	radio := glyphWaiting
	border := t.Dimmer
	head := lipgloss.NewStyle().Foreground(t.Text)
	if selected {
		radio = "◉"
		border = t.Accent
		head = lipgloss.NewStyle().Foreground(t.Bright)
	}
	headline := head.Render(text)
	if recommended {
		headline += " " + lipgloss.NewStyle().Foreground(t.Warn).Render("★")
	}
	body := lipgloss.NewStyle().Foreground(border).Render(radio) + " " + headline
	if consequence != "" {
		dim := lipgloss.NewStyle().Foreground(t.Dim)
		for _, line := range wrapIndent(consequence, max(10, width-6), "") {
			body += "\n  " + dim.Render(line)
		}
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Padding(0, 1).Width(max(10, width-2)).
		Render(body)
}

func renderEvidence(b evidence.Bundle, title string, width int) string {
	return renderEvidenceDetails(b, title, "", nil, width)
}

func renderEvidenceDetails(b evidence.Bundle, title, lastError string, artifacts []string, width int) string {
	t := activeTheme
	head := panelTitle("Evidence · "+title, "")
	added := lipgloss.NewStyle().Foreground(t.Ok).Render(fmt.Sprintf("+%d", b.Added))
	removed := lipgloss.NewStyle().Foreground(t.Err).Render(fmt.Sprintf("−%d", b.Removed))
	lines := []string{
		head,
		fmt.Sprintf("%d files · %s · %s", len(b.Files), added, removed),
	}
	if b.Biggest != "" {
		lines = append(lines, "biggest: "+b.Biggest)
	}
	if len(b.AreaWeight) > 0 {
		type areaStat struct {
			name   string
			weight int
		}
		areas := make([]areaStat, 0, len(b.AreaWeight))
		for name, weight := range b.AreaWeight {
			areas = append(areas, areaStat{name: name, weight: weight})
		}
		sort.Slice(areas, func(i, j int) bool {
			if areas[i].weight == areas[j].weight {
				return areas[i].name < areas[j].name
			}
			return areas[i].weight > areas[j].weight
		})
		lines = append(lines, "areas:")
		for _, area := range areas[:min(5, len(areas))] {
			lines = append(lines, fmt.Sprintf("  %s %d", area.name, area.weight))
		}
	}
	if lastError != "" {
		lines = append(lines, "error: "+lastError)
	} else if len(artifacts) > 0 {
		lines = append(lines, "tests: see available review artifacts")
	} else {
		lines = append(lines, "tests: no test result artifact available")
	}
	lines = append(lines, "enter artifact · esc back")
	return wrapLines(lines, width)
}

func renderEvidenceFallback(d projection.DecisionView, det *proto.IssueDetail, width int) string {
	lines := []string{"EVIDENCE · " + d.IssueID, "no evidence bundle yet"}
	if len(d.Paths) > 0 {
		lines = append(lines, "paths:")
		for _, path := range d.Paths {
			lines = append(lines, "  "+path)
		}
	}
	if det != nil && len(det.Artifacts) > 0 {
		lines = append(lines, "available artifacts:")
		for _, artifact := range det.Artifacts {
			lines = append(lines, "  "+artifact)
		}
	} else {
		lines = append(lines, "available artifacts: none")
	}
	lines = append(lines, "enter artifact · esc back")
	return wrapLines(lines, width)
}
