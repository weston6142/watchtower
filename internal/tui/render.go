package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/weston6142/watchtower/internal/projection"
	"github.com/weston6142/watchtower/internal/proto"
)

var themeDim = lipgloss.NewStyle().Faint(true)

func styleStatusBad() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(activeTheme.Err)
}
func styleStatusWarn() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(activeTheme.Warn)
}
func styleStatusOk() lipgloss.Style { return lipgloss.NewStyle().Foreground(activeTheme.Ok) }

func renderHeader(ov *proto.Overview, width int) string {
	if ov == nil {
		ov = &proto.Overview{}
	}
	var details []string
	if ov.Building > 0 {
		details = append(details, fmt.Sprintf("%d building", ov.Building))
	}
	if ov.ShippedToday > 0 {
		details = append(details, fmt.Sprintf("%d shipped today", ov.ShippedToday))
	}
	if ov.TokensTotal > 0 {
		details = append(details, fmt.Sprintf("%s tokens", compactTokens(ov.TokensTotal)))
	}
	if ov.DollarsTotal > 0 {
		details = append(details, fmt.Sprintf("~$%.2f", ov.DollarsTotal))
	}
	var badges []badge
	if ov.NeedYou > 0 {
		badges = append(badges, badge{Text: fmt.Sprintf("%d question%s for you", ov.NeedYou, pluralSuffix(ov.NeedYou)), Kind: badgeWarn})
	}
	if ov.Failing > 0 {
		badges = append(badges, badge{Text: fmt.Sprintf("%d build%s failing", ov.Failing, pluralSuffix(ov.Failing)), Kind: badgeErr})
	}
	if len(badges) == 0 {
		badges = append(badges, badge{Text: "all clear", Kind: badgeOk})
	}
	return renderChromeHeader(width, badges, strings.Join(details, " · "))
}

func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func compactTokens(tokens int) string {
	if tokens >= 1000 {
		return fmt.Sprintf("%.0fk", float64(tokens)/1000)
	}
	return fmt.Sprintf("%d", tokens)
}

func renderNoticeRow(st *projection.State, ids map[string]Identity, width int) string {
	text := ""
	switch {
	case st != nil && len(st.Notices) > 0:
		text = strings.ReplaceAll(st.Notices[len(st.Notices)-1].Text, "\n", " ")
	default:
		text = parkedHint(st, ids)
	}
	if text == "" {
		if width > 0 {
			return strings.Repeat(" ", width)
		}
		return " "
	}
	return truncate(text, width)
}

// parkedHint names the first parked lane and the key that continues it. A lane
// waiting on a human with nothing on screen saying so reads as a hung tower.
// Killed lanes are left out: R is their verb, and the kill copy already says so.
func parkedHint(st *projection.State, ids map[string]Identity) string {
	if st == nil {
		return ""
	}
	for _, id := range st.Order {
		iv := st.Issues[id]
		if iv == nil || iv.Killed || !(iv.Paused || iv.State == "paused") {
			continue
		}
		tag := id
		if identity, ok := ids[id]; ok && identity.Tag != "" {
			tag = identity.Tag
		}
		if iv.CurrentStage != "" {
			return glyphParked + " " + tag + " parked at " + iv.CurrentStage + " — p resumes"
		}
		return glyphParked + " " + tag + " parked — p resumes"
	}
	return ""
}

// floorCards returns issue IDs on a rendered stage floor in creation order.
func floorCards(st *projection.State, stages []string, floor int) []string {
	if st == nil || floor <= 0 || floor > len(stages) {
		return nil
	}
	stage := stages[floor-1]
	var out []string
	for _, id := range st.Order {
		iv := st.Issues[id]
		if iv == nil {
			continue
		}
		current := iv.CurrentStage
		if iv.State == "done" || iv.Merged {
			current = stages[len(stages)-1]
		}
		if current == stage {
			out = append(out, id)
		}
	}
	return out
}

func truncate(s string, width int) string {
	if width <= 0 || lipgloss.Width(s) <= width {
		return s
	}
	// ansi.Truncate is display-width-aware and never splits escape
	// sequences or wide runes (emoji flags would break a byte slice).
	return ansi.Truncate(s, width, "…")
}

// laneWidth is the fixed width of one issue lane; stageGutterWidth is the
// left gutter that holds the stage label on every grid row.
const (
	laneWidth        = 14
	stageGutterWidth = 12
)

func padCell(s string, width int) string {
	s = truncate(s, width)
	return s + strings.Repeat(" ", max(0, width-lipgloss.Width(s)))
}

func titleLines(title string) [2]string {
	runes := []rune(strings.TrimSpace(title))
	if len(runes) > 24 {
		runes = runes[:24]
	}
	var lines [2]string
	for i := 0; i < 2 && len(runes) > 0; i++ {
		if len(runes) <= 12 {
			lines[i] = string(runes)
			break
		}
		cut := 12
		for j := 12; j > 0; j-- {
			if runes[j] == ' ' {
				cut = j
				break
			}
		}
		lines[i] = string(runes[:cut])
		runes = []rune(strings.TrimSpace(string(runes[cut:])))
	}
	return lines
}

func stageName(aliases map[string]string, stage string) string {
	if aliases != nil && aliases[stage] != "" {
		return aliases[stage]
	}
	return stage
}

func completedStage(iv *projection.IssueView, stage string) bool {
	for _, completed := range iv.Completed {
		if completed == stage {
			return true
		}
	}
	return false
}

// pausedAtStage reports whether this cell is where a parked lane stopped.
// CurrentStage is authoritative when it names a stage that has not finished;
// rehydrated lanes and pre-payload pause events leave it pointing at a
// completed stage, so fall back to the first unfinished one.
func pausedAtStage(iv *projection.IssueView, stage string, stageIdx int) bool {
	if iv.CurrentStage != "" && !completedStage(iv, iv.CurrentStage) {
		return iv.CurrentStage == stage
	}
	return stageIdx == len(iv.Completed)
}

func cellContentForStage(iv *projection.IssueView, ids map[string]Identity, stage string, stageIdx, tick int, focused, reducedMotion bool) string {
	if iv == nil {
		return ""
	}
	if iv.Merged && (stage == "merge" || stageIdx == len(iv.Completed)) {
		return styleStatusOk().Render(glyphShipped + " shipped")
	}
	if iv.Behind != "" && stage == "merge" {
		blocker := iv.Behind
		if blockerIdentity, ok := ids[iv.Behind]; ok {
			return themeDim.Render("after ") + identityStyle(blockerIdentity).Render("▐"+blockerIdentity.Tag+"▌")
		}
		return themeDim.Render("after " + blocker)
	}
	if (iv.Paused || iv.Killed || iv.State == "paused") && pausedAtStage(iv, stage, stageIdx) {
		return lipgloss.NewStyle().Foreground(activeTheme.Dim).Render(glyphParked + " paused")
	}
	if iv.CurrentStage == stage && iv.State == "waiting_decision" {
		return styleStatusWarn().Render(glyphNeedYou + " need-you")
	}
	if iv.CurrentStage == stage && iv.State == "failed" {
		style := styleStatusBad()
		if !reducedMotion && tick%2 == 1 {
			style = style.Reverse(true)
		}
		return style.Render(glyphFailed + " failed")
	}
	if iv.State == "queued_for_slot" && iv.CurrentStage == stage {
		return lipgloss.NewStyle().Foreground(activeTheme.Dim).Render("⧗ queued")
	}
	if iv.CurrentStage == stage && iv.State == "running" {
		// Working is Structure blue — an agent doing its job needs nothing
		// from you. The glyph alternates for motion; the word stays put.
		spinner := glyphWorking
		if !reducedMotion && (tick/2)%2 == 1 {
			spinner = "◓"
		}
		working := lipgloss.NewStyle().Foreground(activeTheme.Structure).Render(spinner + " working")
		if iv.Tokens > 0 {
			working += lipgloss.NewStyle().Foreground(activeTheme.Dim).Render(" " + compactTokens(iv.Tokens))
		}
		return working
	}
	if completedStage(iv, stage) {
		return styleStatusOk().Render(glyphDone)
	}
	return lipgloss.NewStyle().Foreground(activeTheme.Dimmer).Render(glyphWaiting)
}

func headerCell(content string, identity Identity, focused bool) string {
	style := identityStyle(identity)
	if focused {
		// The focused lane's header sits on the selection ground.
		style = style.Bold(true).Background(activeTheme.Bg2)
	}
	return padCell(style.Render(content), laneWidth)
}

// visibleLanes keeps the focused lane and its immediate neighbors at the
// normal lane width. Edge lanes are returned separately so the caller can
// render them as fixed-width count gutters without changing grid geometry.
func visibleLanes(order []string, focus, width int) (full, compactLeft, compactRight []string) {
	if len(order) == 0 {
		return nil, nil, nil
	}
	if focus < 0 || focus >= len(order) {
		focus = 0
	}
	capacity := 3
	if width > 0 {
		capacity = max(capacity, (width-stageGutterWidth)/laneWidth)
	}
	capacity = min(capacity, len(order))
	if len(order) <= capacity {
		return append([]string(nil), order...), nil, nil
	}
	start := max(0, focus-1)
	if start+capacity > len(order) {
		start = len(order) - capacity
	}
	end := start + capacity
	return append([]string(nil), order[start:end]...), append([]string(nil), order[:start]...), append([]string(nil), order[end:]...)
}

func edgeGutter(issueIDs []string) string {
	if len(issueIDs) == 0 {
		return ""
	}
	return padCell(fmt.Sprintf("‹%d›", len(issueIDs)), 5)
}

func withEdgeGutters(content string, left, right []string) string {
	if len(left) == 0 && len(right) == 0 {
		return content
	}
	return edgeGutter(left) + " " + content + " " + edgeGutter(right)
}

func renderTower(st *projection.State, stages []string, ids map[string]Identity, focus Focus, tick, width int) string {
	return renderTowerConfigured(st, stages, ids, focus, nil, false, tick, width)
}

func renderTowerConfigured(st *projection.State, stages []string, ids map[string]Identity, focus Focus, aliases map[string]string, reducedMotion bool, tick, width int) string {
	if st == nil {
		st = projection.NewState()
	}
	lines := []string{warRoom(st, ids), "MAP · " + mapInsight(st.Issues) + " · a full map"}
	if len(st.Order) == 0 {
		for _, stage := range stages {
			lines = append(lines, stageLabel(aliases, stage)+"  "+themeDim.Render("—"))
		}
		return boundedLines(lines, width)
	}
	var allIssueIDs []string
	for _, id := range st.Order {
		iv := st.Issues[id]
		if iv == nil {
			continue
		}
		allIssueIDs = append(allIssueIDs, id)
	}
	focusIndex := 0
	for i, id := range allIssueIDs {
		if id == focus.Issue {
			focusIndex = i
			break
		}
	}
	issueIDs, compactLeft, compactRight := visibleLanes(allIssueIDs, focusIndex, width)
	var chips, first, second []string
	for _, id := range issueIDs {
		iv := st.Issues[id]
		identity := ids[id]
		marker := " "
		if focus.Issue == id {
			marker = glyphCursor
		}
		chips = append(chips, headerCell(marker+identity.Tag, identity, focus.Issue == id))
		wrapped := titleLines(iv.Title)
		first = append(first, headerCell(wrapped[0], identity, focus.Issue == id))
		second = append(second, headerCell(wrapped[1], identity, focus.Issue == id))
	}
	// The stage label occupies the left gutter; each issue lane remains fixed at
	// fourteen cells so transient status text can never reflow the grid.
	lines = append(lines, withEdgeGutters(padCell("", stageGutterWidth)+" "+strings.Join(chips, ""), compactLeft, compactRight))
	lines = append(lines, withEdgeGutters(padCell("", stageGutterWidth)+" "+strings.Join(first, ""), compactLeft, compactRight))
	lines = append(lines, withEdgeGutters(padCell("", stageGutterWidth)+" "+strings.Join(second, ""), compactLeft, compactRight))
	var idRow []string
	for _, id := range issueIDs {
		identity := ids[id]
		idRow = append(idRow, headerCell(id, identity, focus.Issue == id))
	}
	lines = append(lines, withEdgeGutters(padCell("", stageGutterWidth)+" "+strings.Join(idRow, ""), compactLeft, compactRight))
	for stageIdx, stage := range stages {
		label := stageLabel(aliases, stage) + " "
		var cells []string
		for _, id := range issueIDs {
			cells = append(cells, padCell(cellContentForStage(st.Issues[id], ids, stage, stageIdx, tick, focus.Issue == id, reducedMotion), laneWidth))
		}
		lines = append(lines, withEdgeGutters(label+strings.Join(cells, ""), compactLeft, compactRight))
	}
	return boundedLines(lines, width)
}

// stageLabel renders the fixed-width dim stage name for the left gutter.
func stageLabel(aliases map[string]string, stage string) string {
	return lipgloss.NewStyle().Foreground(activeTheme.Dim).
		Render(padCell(strings.ToUpper(stageName(aliases, stage)), stageGutterWidth))
}

// renderRows is the wide, one-row-per-issue orientation. It calls the same
// cell state renderer as the tower so wording cannot drift between views.
func renderRows(st *projection.State, stages []string, ids map[string]Identity, focus Focus, tick, width int) string {
	return renderRowsConfigured(st, stages, ids, focus, false, tick, width)
}

func renderRowsConfigured(st *projection.State, stages []string, ids map[string]Identity, focus Focus, reducedMotion bool, tick, width int) string {
	if st == nil {
		st = projection.NewState()
	}
	lines := []string{"ROWS · z tower"}
	for _, issueID := range st.Order {
		iv := st.Issues[issueID]
		if iv == nil {
			continue
		}
		identity := ids[issueID]
		prefix := identity.Tag + " " + iv.Title + " · "
		var cells []string
		for i, stage := range stages {
			cell := cellContentForStage(iv, ids, stage, i, tick, focus.Issue == issueID, reducedMotion)
			cells = append(cells, stage+":"+cell)
		}
		line := prefix + strings.Join(cells, "  ")
		if identity.Color != "" {
			line = identityStyle(identity).Render(prefix) + strings.Join(cells, "  ")
		}
		lines = append(lines, truncate(line, width))
	}
	if len(st.Order) == 0 {
		lines = append(lines, themeDim.Render("—"))
	}
	return boundedLines(lines, width)
}

type shelfItem struct {
	ID     string
	Title  string
	Parked bool
}

func renderShelf(items []shelfItem, ids map[string]Identity, width int) string {
	shipped := make([]shelfItem, 0, len(items))
	parked := make([]shelfItem, 0, len(items))
	for _, item := range items {
		if item.Parked {
			parked = append(parked, item)
		} else {
			shipped = append(shipped, item)
		}
	}
	t := activeTheme
	label := lipgloss.NewStyle().Foreground(t.Dim)
	lines := []string{}
	if len(shipped) > 0 {
		lines = append(lines, label.Render("SHIPPED today"))
		for _, item := range shipped {
			lines = append(lines, shelfLine(item, ids[item.ID], lipgloss.NewStyle().Foreground(t.Ok).Render(glyphShipped)))
		}
	}
	if len(parked) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, label.Render("PARKED"))
		for _, item := range parked {
			lines = append(lines, shelfLine(item, ids[item.ID], label.Render(glyphParked)))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return boundedLines(lines, width)
}

func shelfLine(item shelfItem, identity Identity, status string) string {
	tag := "▓ " + identity.Tag
	if identity.Color != "" {
		tag = identityStyle(identity).Render(tag)
	}
	title := lipgloss.NewStyle().Foreground(activeTheme.Text).Render(item.Title)
	return status + " " + tag + " " + title
}

type helpGroup struct {
	name string
	rows [][2]string // key, description
}

var helpGroups = [][]helpGroup{
	{ // left column
		{"NAVIGATION", [][2]string{
			{"j / k", "floors"},
			{"h / l", "cards"},
			{"1..9", "focus issue"},
			{"tab", "attention / next field"},
			{"g", "war room"},
			{"enter", "open artifacts"},
			{"esc", "back"},
			{"z", "rows / tower layout"},
		}},
		{"CONTROL", [][2]string{
			{"p", "pause / resume"},
			{"x", "kill stage"},
			{"X", "abandon lane"},
			{"R", "retry failed stage"},
			{"L", "lever editor"},
			{"n", "new issue"},
			{"c", "retire shipped lane"},
			{"u", "shipped shelf"},
		}},
	},
	{ // right column
		{"DOORS", [][2]string{
			{"d", "decisions"},
			{"t", "triage"},
			{"e", "timeline"},
			{"T", "stream"},
			{"r", "reject tray item"},
			{"a / A", "architecture pane / map"},
		}},
		{"DECISIONS", [][2]string{
			{"y", "accept recommendation"},
			{"n", "choose option"},
			{"o", "show evidence"},
			{"1..9", "choose option by number"},
		}},
	},
}

func renderHelpOverlay(width int) string {
	t := activeTheme
	keyStyle := lipgloss.NewStyle().Foreground(t.Accent).Bold(true).Width(8)
	descStyle := lipgloss.NewStyle().Foreground(t.Text)
	headStyle := lipgloss.NewStyle().Foreground(t.Structure).Bold(true)

	var cols []string
	for _, col := range helpGroups {
		var blocks []string
		for _, g := range col {
			lines := []string{headStyle.Render(g.name)}
			for _, row := range g.rows {
				lines = append(lines, keyStyle.Render(row[0])+descStyle.Render(row[1]))
			}
			blocks = append(blocks, strings.Join(lines, "\n"))
		}
		cols = append(cols, strings.Join(blocks, "\n\n"))
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, cols[0], "    ", cols[1])

	// STATES is the one legitimate legend home: the glyph language, in its
	// semantic colors.
	dim := lipgloss.NewStyle().Foreground(t.Dim)
	states := headStyle.Render("STATES") + "\n" +
		lipgloss.NewStyle().Foreground(t.Ok).Render(glyphDone) + descStyle.Render(" done") + dim.Render("  ·  ") +
		lipgloss.NewStyle().Foreground(t.Warn).Render(glyphNeedYou) + descStyle.Render(" need-you") + dim.Render("  ·  ") +
		lipgloss.NewStyle().Foreground(t.Structure).Render(glyphWorking) + descStyle.Render(" working") + dim.Render("  ·  ") +
		lipgloss.NewStyle().Foreground(t.Dimmer).Render(glyphWaiting) + descStyle.Render(" waiting") + dim.Render("  ·  ") +
		lipgloss.NewStyle().Foreground(t.Err).Render(glyphFailed) + descStyle.Render(" failed") + dim.Render("  ·  ") +
		lipgloss.NewStyle().Foreground(t.Ok).Render(glyphShipped) + descStyle.Render(" shipped") + dim.Render("  ·  ") +
		dim.Render(glyphParked) + descStyle.Render(" parked")

	foot := keyChip("? / esc") + dim.Render(" close  ") + keyChip("q / ctrl+c") + dim.Render(" quit")
	rule := lipgloss.NewStyle().Foreground(t.Dimmer).Render(strings.Repeat("─", lipgloss.Width(body)))

	const subtitle = "every key in the control room"
	box := renderBox("help", subtitle, " esc close ", strings.Join([]string{body, "", states, rule, foot}, "\n"))
	if lipgloss.Width(box) >= width {
		// narrow terminal: stack the two columns
		box = renderBox("help", subtitle, " esc close ", strings.Join([]string{cols[0] + "\n\n" + cols[1], foot}, "\n"))
	}
	return box
}

func warRoom(st *projection.State, ids map[string]Identity) string {
	if st == nil {
		return "shipping order: — · builders 0/4 busy · ideas 0 · questions 0"
	}
	var lane []string
	for _, id := range st.Order {
		iv := st.Issues[id]
		if iv == nil {
			continue
		}
		if iv.Merged || iv.CurrentStage == "merge" || iv.Behind != "" || iv.State == "done" {
			tag := id
			if identity, ok := ids[id]; ok {
				tag = identity.Tag
			}
			if iv.Behind != "" {
				tag = themeDim.Render(tag)
			}
			lane = append(lane, tag)
		}
	}
	if len(lane) == 0 {
		lane = []string{"—"}
	} else if len(lane) > 3 {
		lane = append(lane[:3], fmt.Sprintf("+%d", len(lane)-3))
	}
	busy := 0
	for _, iv := range st.Issues {
		if iv.State == "running" && iv.CurrentStage != "" {
			busy++
		}
	}
	return fmt.Sprintf("shipping order: %s · builders %d/4 busy · ideas %d · questions %d",
		strings.Join(lane, "→"), busy, st.ProposalCount, len(st.Decisions))
}
