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

func renderReconnectPanel(width, height int) string {
	panel := styleStatusWarn().Render(reconnectingLabel)
	if height <= 0 {
		return panel
	}
	return lipgloss.NewStyle().Width(max(1, width)).Height(height).
		Align(lipgloss.Center, lipgloss.Center).Render(panel)
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

// visibleOrder returns issue IDs still on the grid — retired lanes live on the shelf.
func visibleOrder(st *projection.State, retired map[string]bool) []string {
	if st == nil {
		return nil
	}
	out := make([]string, 0, len(st.Order))
	for _, id := range st.Order {
		if st.Issues[id] == nil || retired[id] {
			continue
		}
		out = append(out, id)
	}
	return out
}

// floorCards returns issue IDs on a rendered stage floor in creation order.
func floorCards(st *projection.State, stages []string, floor int, retired map[string]bool) []string {
	if st == nil || floor <= 0 || floor > len(stages) {
		return nil
	}
	var out []string
	for _, id := range visibleOrder(st, retired) {
		iv := st.Issues[id]
		if displayedFloor(iv, stages) == floor {
			out = append(out, id)
		}
	}
	return out
}

// displayedFloor returns the one-based floor where a lane's card is painted.
// Projection state can briefly point at a completed stage between workers, so
// both navigation and rendering fall forward to the first unfinished stage.
func displayedFloor(iv *projection.IssueView, stages []string) int {
	if iv == nil || len(stages) == 0 {
		return 0
	}
	if iv.State == "done" || iv.Merged {
		return len(stages)
	}
	for index, stage := range stages {
		if iv.CurrentStage == stage && !completedStage(iv, stage) {
			return index + 1
		}
	}
	if iv.State == "claimed" || iv.State == "paused" || iv.Paused || iv.Killed || completedStage(iv, iv.CurrentStage) {
		next := len(iv.Completed) + 1
		if next <= len(stages) {
			return next
		}
	}
	return 0
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

type interactionRect struct {
	X, Y          int
	Width, Height int
}

func (r interactionRect) contains(x, y int) bool {
	return x >= 0 && y >= 0 && x >= r.X && y >= r.Y && x < r.X+r.Width && y < r.Y+r.Height
}

type mouseLaneTarget struct {
	IssueID string
	Bounds  interactionRect
}

type interactionGeometry struct {
	Lanes      []mouseLaneTarget
	Pager      interactionRect
	Transcript interactionRect
}

func (g interactionGeometry) laneAt(x, y int) (mouseLaneTarget, bool) {
	for _, target := range g.Lanes {
		if target.Bounds.contains(x, y) {
			return target, true
		}
	}
	return mouseLaneTarget{}, false
}

// interactionGeometry is derived from the same final screen that View renders.
// Mouse coordinates therefore follow vertical centering, rail stacking, and
// any layout truncation instead of repeating the tower's coordinate formula.
func (m Model) interactionGeometry() interactionGeometry {
	screen := ansi.Strip(m.View())
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	geometry := interactionGeometry{}

	if m.pager.Mode == "pager" {
		towerWidth, _, _ := mainColumnWidths(m.layoutWidth())
		pagerHeight := lipgloss.Height(renderPager(m.pager, towerWidth, m.pagerBodyHeight()))
		for y, line := range lines {
			if m.pager.Title != "" && strings.Contains(line, m.pager.Title) {
				geometry.Pager = interactionRect{X: 0, Y: y, Width: towerWidth, Height: min(pagerHeight, len(lines)-y)}
				break
			}
		}
	}

	if m.currentMode() == "transcript" {
		bindings := [][2]string{{"j/k", "scroll"}, {"d/u", "page"}, {"g/G", "oldest/newest"}, {"esc", "back"}, {"q", "quit"}}
		footerRows := lipgloss.Height(renderKeybar(m.layoutWidth(), bindings, errText(m.Err)))
		streamHeight := m.Height
		if streamHeight > 0 {
			streamHeight = max(1, streamHeight-max(0, footerRows-1))
		}
		rendered := renderStreamDoor(m.streamSubtitle(), m.doorLines, m.stream, m.layoutWidth(), streamHeight)
		geometry.Transcript = interactionRect{
			X: 0, Y: towerHeaderRows, Width: m.layoutWidth(),
			Height: min(lipgloss.Height(rendered), max(0, len(lines)-towerHeaderRows)),
		}
	}

	if m.rows || m.currentMode() != "" || m.pager.Mode != "" {
		return geometry
	}
	towerWidth, _, _ := mainColumnWidths(m.layoutWidth())
	tower := renderTowerLayout(m.State, m.stages, m.Ids, m.Focus, m.aliases, m.reducedMotion, m.ticks, towerWidth, m.warExpanded, m.retired)
	if len(tower.Lanes) == 0 || len(m.stages) == 0 {
		return geometry
	}
	firstStage := strings.ToUpper(stageName(m.aliases, m.stages[0]))
	for _, localTarget := range tower.Lanes {
		for y, line := range lines {
			if y+1 >= len(lines) || !strings.Contains(lines[y+1], firstStage) {
				continue
			}
			byteX := strings.Index(line, localTarget.IssueID)
			if byteX < 0 || lipgloss.Width(line[:byteX]) != localTarget.Bounds.X {
				continue
			}
			width := min(localTarget.Bounds.Width, max(0, lipgloss.Width(line)-localTarget.Bounds.X))
			if width == 0 {
				continue
			}
			startY := max(0, y-3)
			geometry.Lanes = append(geometry.Lanes, mouseLaneTarget{
				IssueID: localTarget.IssueID,
				Bounds: interactionRect{
					X: localTarget.Bounds.X, Y: startY, Width: width,
					Height: min(localTarget.Bounds.Height, max(0, len(lines)-startY)),
				},
			})
			break
		}
	}
	return geometry
}

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

func cellContentForStage(iv *projection.IssueView, ids map[string]Identity, stages []string, stage string, stageIdx, tick int, focused, reducedMotion bool) string {
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
	if (iv.Paused || iv.Killed || iv.State == "paused") && displayedFloor(iv, stages) == stageIdx+1 {
		return lipgloss.NewStyle().Foreground(activeTheme.Dim).Render(glyphParked + " paused")
	}
	if iv.State == "claimed" && displayedFloor(iv, stages) == stageIdx+1 {
		return lipgloss.NewStyle().Foreground(activeTheme.Dim).Render(glyphParked + " claimed")
	}
	if iv.CurrentStage == stage {
		switch iv.State {
		case "verifying":
			return lipgloss.NewStyle().Foreground(activeTheme.Structure).Render(glyphWorking + " verifying")
		case "waiting:integration":
			return lipgloss.NewStyle().Foreground(activeTheme.Dim).Render("⧗ integrate")
		case "integrating":
			return lipgloss.NewStyle().Foreground(activeTheme.Structure).Render(glyphWorking + " merging")
		case "failed:finalize":
			return styleStatusBad().Render(glyphFailed + " finalize")
		}
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

type towerRenderLayout struct {
	Content string
	Lanes   []mouseLaneTarget
}

func renderTower(st *projection.State, stages []string, ids map[string]Identity, focus Focus, tick, width int) string {
	return renderTowerConfigured(st, stages, ids, focus, nil, false, tick, width, false, nil)
}

func renderTowerConfigured(st *projection.State, stages []string, ids map[string]Identity, focus Focus, aliases map[string]string, reducedMotion bool, tick, width int, warExpanded bool, retired map[string]bool) string {
	return renderTowerLayout(st, stages, ids, focus, aliases, reducedMotion, tick, width, warExpanded, retired).Content
}

func renderTowerLayout(st *projection.State, stages []string, ids map[string]Identity, focus Focus, aliases map[string]string, reducedMotion bool, tick, width int, warExpanded bool, retired map[string]bool) towerRenderLayout {
	if st == nil {
		st = projection.NewState()
	}
	lines := append(warRoomLines(st, ids, warExpanded), "MAP · "+mapInsight(st.Issues)+" · a full map")
	allIssueIDs := visibleOrder(st, retired)
	if len(allIssueIDs) == 0 {
		for _, stage := range stages {
			lines = append(lines, stageLabel(aliases, stage)+"  "+themeDim.Render("—"))
		}
		return towerRenderLayout{Content: boundedLines(lines, width)}
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
			cells = append(cells, padCell(cellContentForStage(st.Issues[id], ids, stages, stage, stageIdx, tick, focus.Issue == id, reducedMotion), laneWidth))
		}
		lines = append(lines, withEdgeGutters(label+strings.Join(cells, ""), compactLeft, compactRight))
	}
	leftOffset := 0
	if len(compactLeft) > 0 || len(compactRight) > 0 {
		leftOffset = 1 // withEdgeGutters reserves a leading separator
	}
	if len(compactLeft) > 0 {
		leftOffset += 6 // five-cell gutter plus its separating space
	}
	localY := len(warRoomLines(st, ids, warExpanded)) + 1
	lanes := make([]mouseLaneTarget, 0, len(issueIDs))
	for index, issueID := range issueIDs {
		lanes = append(lanes, mouseLaneTarget{
			IssueID: issueID,
			Bounds: interactionRect{
				X: leftOffset + stageGutterWidth + 1 + index*laneWidth,
				Y: localY, Width: laneWidth, Height: 4 + len(stages),
			},
		})
	}
	return towerRenderLayout{Content: boundedLines(lines, width), Lanes: lanes}
}

// stageLabel renders the fixed-width dim stage name for the left gutter.
func stageLabel(aliases map[string]string, stage string) string {
	return lipgloss.NewStyle().Foreground(activeTheme.Dim).
		Render(padCell(strings.ToUpper(stageName(aliases, stage)), stageGutterWidth))
}

// renderRows is the wide, one-row-per-issue orientation. It calls the same
// cell state renderer as the tower so wording cannot drift between views.
func renderRows(st *projection.State, stages []string, ids map[string]Identity, focus Focus, tick, width int) string {
	return renderRowsConfigured(st, stages, ids, focus, false, tick, width, nil)
}

func renderRowsConfigured(st *projection.State, stages []string, ids map[string]Identity, focus Focus, reducedMotion bool, tick, width int, retired map[string]bool) string {
	if st == nil {
		st = projection.NewState()
	}
	lines := []string{"ROWS · z tower"}
	issueIDs := visibleOrder(st, retired)
	for _, issueID := range issueIDs {
		iv := st.Issues[issueID]
		identity := ids[issueID]
		prefix := identity.Tag + " " + iv.Title + " · "
		var cells []string
		for i, stage := range stages {
			cell := cellContentForStage(iv, ids, stages, stage, i, tick, focus.Issue == issueID, reducedMotion)
			cells = append(cells, stage+":"+cell)
		}
		line := prefix + strings.Join(cells, "  ")
		if identity.Color != "" {
			line = identityStyle(identity).Render(prefix) + strings.Join(cells, "  ")
		}
		lines = append(lines, truncate(line, width))
	}
	if len(issueIDs) == 0 {
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

func renderHelpOverlay(width int) string {
	t := activeTheme
	keyStyle := lipgloss.NewStyle().Foreground(t.Accent).Bold(true).Width(8)
	descStyle := lipgloss.NewStyle().Foreground(t.Text)
	headStyle := lipgloss.NewStyle().Foreground(t.Structure).Bold(true)
	projection := projectMainKeybindingHelp()

	var cols []string
	for _, col := range projection.Columns {
		var blocks []string
		for _, g := range col {
			lines := []string{headStyle.Render(g.Name)}
			for _, row := range g.Rows {
				lines = append(lines, keyStyle.Render(row.Key)+descStyle.Render(row.Description))
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

	foot := keyChip(projection.Close.Key) + dim.Render(" "+projection.Close.Description+"  ") + keyChip(projection.Quit.Key) + dim.Render(" "+projection.Quit.Description)
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

// warRoomLines renders the war-room strip: the summary line, plus a breakout
// line when expanded via the g key.
func warRoomLines(st *projection.State, ids map[string]Identity, expanded bool) []string {
	summary := warRoom(st, ids)
	if !expanded {
		return []string{summary}
	}
	t := activeTheme
	head := lipgloss.NewStyle().Foreground(t.Bright).Background(t.Bg2).Bold(true).Render(summary)
	var lane []string
	busy := 0
	if st != nil {
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
				lane = append(lane, tag)
			}
			if iv.State == "running" && iv.CurrentStage != "" {
				busy++
			}
		}
	}
	order := "—"
	if len(lane) > 0 {
		order = strings.Join(lane, " → ") // full order, no +n cap
	}
	detail := fmt.Sprintf("shipping order %s · %d building · g collapse", order, busy)
	return []string{head, themeDim.Render(detail)}
}
