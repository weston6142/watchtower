package engine

import (
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/decisionpage"
	"github.com/weston6142/watchtower/internal/evidence"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/touchset"
)

// DecisionPagePath returns the stable progress/decision page path for an
// issue. The daemon data directory is normally absolute; normalize it here so
// protocol clients always receive an absolute path.
func (e *Engine) DecisionPagePath(issueID string) string {
	root, err := filepath.Abs(e.cfg.DataDir)
	if err != nil {
		root = e.cfg.DataDir
	}
	return filepath.Join(root, issueID, "decision.html")
}

func (e *Engine) buildPageData(
	issueID, flowName, title, currentStage string,
	dec *levers.Decision, ctx *decision.DecisionContext, decisionID int64,
	answered string,
) (decisionpage.PageData, error) {
	fl, ok := e.cfg.Flows[flowName]
	if !ok {
		return decisionpage.PageData{}, fmt.Errorf("unknown flow %q", flowName)
	}

	checkpoints, err := e.cfg.Store.StageCheckpoints(issueID)
	if err != nil {
		return decisionpage.PageData{}, err
	}
	byStage := make(map[string]store.StageCheckpoint, len(checkpoints))
	for _, checkpoint := range checkpoints {
		byStage[checkpoint.Stage] = checkpoint
	}
	tokensByStage := map[string]int{}
	if runs, runErr := e.cfg.Store.StageRuns(issueID); runErr == nil {
		for _, run := range runs {
			tokensByStage[run.Stage] += run.Tokens
		}
	}

	if currentStage == "" {
		currentStage = firstIncompleteStage(fl, byStage)
	}
	data := decisionpage.PageData{
		IssueID: issueID, Title: title, StageTotal: len(fl.Stages),
		CurrentStage: currentStage, Answered: answered,
	}
	if decisionID > 0 {
		if rows, rowErr := e.cfg.Store.AllDecisionRows(); rowErr == nil {
			for _, row := range rows {
				if row.ID != decisionID {
					continue
				}
				if !row.CreatedAt.IsZero() {
					elapsed := time.Since(row.CreatedAt)
					if elapsed < 0 {
						elapsed = 0
					}
					data.BlockedFor = fmt.Sprintf("%d min", int(elapsed.Minutes()))
				}
				break
			}
		}
	}
	for _, stage := range fl.Stages {
		if stage.Name == currentStage && stage.HeavySlot {
			data.HeldSlots = "holding 1 heavy slot"
			break
		}
	}
	for index, stage := range fl.Stages {
		checkpoint, hasCheckpoint := byStage[stage.Name]
		floor := decisionpage.Floor{Name: stage.Name, Status: decisionpage.FloorPending}
		switch {
		case hasCheckpoint && checkpoint.Status == "succeeded":
			floor.Status = decisionpage.FloorDone
			floor.Note = checkpointNote(checkpoint, tokensByStage[stage.Name])
			data.DoneCount++
		case hasCheckpoint && checkpoint.Status == "handoff_authorized":
			floor.Status = decisionpage.FloorDone
			floor.Note = checkpointNote(checkpoint, tokensByStage[stage.Name])
			data.DoneCount++
		case hasCheckpoint && (checkpoint.Status == "failed" || checkpoint.Status == "killed"):
			floor.Status = decisionpage.FloorFailed
			floor.Note = checkpoint.Failure
		case stage.Name == currentStage:
			floor.Status = decisionpage.FloorCurrent
			data.StageIndex = index + 1
		default:
			floor.Note = futureStageNote(stage)
		}
		data.Floors = append(data.Floors, floor)
	}
	if data.StageIndex == 0 && len(fl.Stages) > 0 {
		data.StageIndex = len(fl.Stages)
	}

	e.fillPageFiles(&data, issueID, currentStage)
	if dec != nil {
		data.Briefing = buildDecisionPageBriefing(dec, ctx, decisionID)
	}
	return data, nil
}

func firstIncompleteStage(fl flow.Flow, checkpoints map[string]store.StageCheckpoint) string {
	for _, stage := range fl.Stages {
		checkpoint, ok := checkpoints[stage.Name]
		if !ok || (checkpoint.Status != "succeeded" && checkpoint.Status != "handoff_authorized") {
			return stage.Name
		}
	}
	if len(fl.Stages) == 0 {
		return ""
	}
	return fl.Stages[len(fl.Stages)-1].Name
}

func checkpointNote(checkpoint store.StageCheckpoint, tokens int) string {
	parts := make([]string, 0, len(checkpoint.Artifacts)+1)
	for _, artifact := range checkpoint.Artifacts {
		parts = append(parts, artifact.Name)
	}
	if tokens > 0 {
		parts = append(parts, fmt.Sprintf("%d tok", tokens))
	}
	return strings.Join(parts, ", ")
}

func futureStageNote(stage flow.Stage) string {
	if stage.MergeBarrier {
		return "merge barrier"
	}
	if len(stage.Artifacts) > 0 {
		return "expects " + strings.Join(stage.Artifacts, ", ")
	}
	return "upcoming stage"
}

func buildDecisionPageBriefing(
	dec *levers.Decision, ctx *decision.DecisionContext, decisionID int64,
) *decisionpage.Briefing {
	briefing := &decisionpage.Briefing{
		Question: dec.Question, Importance: dec.Importance, Reversible: dec.Reversible,
	}
	if ctx != nil {
		briefing.AgentLabel = ctx.AgentLabel()
	}
	for index, option := range dec.Options {
		item := decisionpage.Option{
			Key: fmt.Sprintf("%d", index+1), Label: option, Recommended: index == dec.Recommended,
		}
		if dec.Briefing != nil && index < len(dec.Briefing.OptionDetails) {
			item.OneLiner = dec.Briefing.OptionDetails[index]
		} else if index < len(dec.Consequences) {
			item.OneLiner = dec.Consequences[index]
		}
		briefing.Options = append(briefing.Options, item)
	}
	if dec.Kind == levers.DecisionFreeform || dec.AllowFreeform {
		briefing.Options = append(briefing.Options, decisionpage.Option{
			Key: "f", Label: "Freeform", OneLiner: "Type your own instruction back to the agent.",
		})
	}
	if dec.Briefing != nil {
		briefing.Wins = append(briefing.Wins, dec.Briefing.Wins...)
		if len(briefing.Wins) > 5 {
			briefing.Wins = briefing.Wins[:5]
		}
		for _, excerpt := range dec.Briefing.Excerpts {
			if len(briefing.Excerpts) == 3 {
				break
			}
			briefing.Excerpts = append(briefing.Excerpts, decisionpage.Excerpt{
				Text: excerpt.Text, Cite: excerpt.Cite,
			})
		}
		briefing.OverrideNote = dec.Briefing.OverrideNote
		briefing.NextAction = dec.Briefing.NextAction
		briefing.DiagramCaption = dec.Briefing.DiagramCaption
		if dec.Briefing.DiagramSVG != "" {
			if err := decisionpage.ValidateSVG(dec.Briefing.DiagramSVG); err == nil {
				briefing.DiagramSVG = template.HTML(dec.Briefing.DiagramSVG)
			} else {
				briefing.DiagramMissing = true
			}
		}
	}
	if briefing.NextAction == "" {
		briefing.NextAction = fmt.Sprintf("Answer decision %d in the TUI.", decisionID)
	}
	return briefing
}

func (e *Engine) fillPageFiles(data *decisionpage.PageData, issueID, currentStage string) {
	var active touchset.Set
	var worktree string
	e.mu.Lock()
	if issue := e.issues[issueID]; issue != nil {
		if issue.activeTouchset != nil {
			active = *issue.activeTouchset
		}
		worktree = issue.wsPath
	}
	e.mu.Unlock()

	var planned touchset.Set
	var loaded bool
	if len(active.Globs) > 0 {
		planned, loaded = active, true
	}
	paths := []string{
		filepath.Join(e.issueDir(issueID), "artifacts", "touchset.json"),
		filepath.Join(e.issueDir(issueID), "touchset.json"),
	}
	if worktree != "" {
		paths = append(paths, filepath.Join(worktree, "touchset.json"))
	}
	if !loaded {
		for _, candidate := range paths {
			loadedSet, err := touchset.Load(candidate)
			if err == nil {
				planned, loaded = loadedSet, true
				break
			}
		}
	}
	if !loaded {
		data.TouchsetMissing = true
	} else {
		data.TouchsetGlobs = append([]string(nil), planned.Globs...)
	}

	evidencePath := filepath.Join(e.cfg.DataDir, issueID, "evidence", currentStage, "evidence.json")
	encoded, err := os.ReadFile(evidencePath)
	if err != nil {
		data.EvidenceMissing = true
		return
	}
	var bundle evidence.Bundle
	if err := json.Unmarshal(encoded, &bundle); err != nil {
		data.EvidenceMissing = true
		return
	}
	for _, file := range bundle.Files {
		data.Files = append(data.Files, decisionpage.FileRow{
			Path: file.Path, Added: file.Added, Removed: file.Removed,
			InBounds: loaded && matchesTouchset(file.Path, planned),
		})
	}
}

func matchesTouchset(file string, planned touchset.Set) bool {
	file = filepath.ToSlash(file)
	for _, glob := range planned.Globs {
		glob = filepath.ToSlash(glob)
		if strings.HasSuffix(glob, "/**") && strings.HasPrefix(file, strings.TrimSuffix(glob, "**")) {
			return true
		}
		if ok, err := path.Match(glob, file); err == nil && ok {
			return true
		}
	}
	return false
}

func (e *Engine) writeDecisionPage(
	is *issueState, stage string, decisionID int64, d levers.Decision,
	ctx *decision.DecisionContext, answered string,
) {
	if is == nil {
		return
	}
	data, err := e.buildPageData(is.id, is.flowName, is.title, stage, &d, ctx, decisionID, answered)
	if err != nil {
		return
	}
	e.writeRenderedDecisionPage(is.id, stage, decisionID, data)
}

func (e *Engine) writeRenderedDecisionPage(issueID, stage string, decisionID int64, data decisionpage.PageData) {
	content, err := decisionpage.Render(data)
	if err != nil {
		return
	}
	decisionsDir := filepath.Join(e.issueDir(issueID), "decisions")
	if err := os.MkdirAll(decisionsDir, 0o755); err != nil {
		return
	}
	if decisionID > 0 {
		if err := os.WriteFile(filepath.Join(decisionsDir, fmt.Sprintf("%d.html", decisionID)), content, 0o644); err != nil {
			return
		}
	}
	latest := filepath.Join(e.issueDir(issueID), "decision.html")
	if err := os.WriteFile(latest, content, 0o644); err != nil {
		return
	}
	e.emit(core.EvArtifactProduced, issueID, map[string]any{
		"stage": stage, "artifact": "decision.html", "path": latest, "decision_id": decisionID,
	})
}

func (e *Engine) refreshDecisionPage(issueID string) {
	e.mu.Lock()
	is := e.issues[issueID]
	if is == nil {
		e.mu.Unlock()
		return
	}
	title, flowName, currentStage := is.title, is.flowName, ""
	if fl, ok := e.cfg.Flows[flowName]; ok && is.stageIdx >= 0 && is.stageIdx < len(fl.Stages) {
		currentStage = fl.Stages[is.stageIdx].Name
	}
	for _, pending := range e.pend {
		if pending.IssueID == issueID {
			dec := pending.D
			ctx := pending.Context
			stage := pending.Stage
			id := pending.ID
			e.mu.Unlock()
			e.writeDecisionPage(is, stage, id, dec, ctx, "")
			return
		}
	}
	e.mu.Unlock()

	if currentStage == "" {
		if fl, ok := e.cfg.Flows[flowName]; ok {
			checkpoints, err := e.cfg.Store.StageCheckpoints(issueID)
			if err == nil {
				byStage := make(map[string]store.StageCheckpoint, len(checkpoints))
				for _, checkpoint := range checkpoints {
					byStage[checkpoint.Stage] = checkpoint
				}
				currentStage = firstIncompleteStage(fl, byStage)
			}
		}
	}
	data, err := e.buildPageData(issueID, flowName, title, currentStage, nil, nil, 0, "")
	if err == nil {
		e.writeRenderedDecisionPage(issueID, currentStage, 0, data)
	}
}

func answerStamp(response levers.Response) string {
	answer := ""
	if response.Kind == levers.DecisionChoice && response.Option != nil {
		answer = fmt.Sprintf("option %d", *response.Option+1)
	} else if response.Kind == levers.DecisionFreeform {
		answer = strings.TrimSpace(response.Text)
	}
	if answer == "" {
		answer = "response recorded"
	}
	return fmt.Sprintf("Answered: %s · %s", answer, time.Now().UTC().Format("2006-01-02 15:04"))
}
