package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/decisionpage"
	"github.com/weston6142/watchtower/internal/evidence"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/review"
	"github.com/weston6142/watchtower/internal/stagelifecycle"
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
	return filepath.Join(root, issueID, decisionpage.FileName)
}

type decisionPageResolution struct {
	Response  levers.Response
	Stamp     string
	Approval  *review.ApprovalProvenance
	Automatic bool
}

func resolvedDecisionPage(
	response levers.Response, approval *review.ApprovalProvenance, answeredAt time.Time,
) *decisionPageResolution {
	if answeredAt.IsZero() {
		answeredAt = time.Now().UTC()
	}
	resolution := &decisionPageResolution{Response: response, Stamp: answerStamp(response, answeredAt)}
	if approval == nil {
		return resolution
	}
	stored := *approval
	resolution.Approval = &stored
	if stored.Kind == review.ApprovalPolicy {
		resolution.Stamp = fmt.Sprintf(
			"Automatically approved by policy %s@%s: %s · %s",
			stored.PolicyID, stored.PolicyVersion, answerText(response),
			answeredAt.UTC().Format("2006-01-02 15:04"),
		)
	}
	return resolution
}

func resolvedStoredDecisionPage(
	response levers.Response, approval *review.ApprovalProvenance, answeredAt time.Time, status string,
) *decisionPageResolution {
	if status != "auto" || approval != nil {
		if !answeredAt.IsZero() {
			return resolvedDecisionPage(response, approval, answeredAt)
		}
		resolution := &decisionPageResolution{
			Response: response,
			Stamp:    fmt.Sprintf("Answered: %s · timestamp unavailable", answerText(response)),
		}
		if approval == nil {
			return resolution
		}
		stored := *approval
		resolution.Approval = &stored
		if stored.Kind == review.ApprovalPolicy {
			resolution.Stamp = fmt.Sprintf(
				"Automatically approved by policy %s@%s: %s · timestamp unavailable",
				stored.PolicyID, stored.PolicyVersion, answerText(response),
			)
		}
		return resolution
	}
	resolution := &decisionPageResolution{
		Response:  response,
		Automatic: true,
	}
	if answeredAt.IsZero() {
		resolution.Stamp = fmt.Sprintf("Automatically resolved: %s · timestamp unavailable", answerText(response))
	} else {
		resolution.Stamp = fmt.Sprintf(
			"Automatically resolved: %s · %s",
			answerText(response), answeredAt.UTC().Format("2006-01-02 15:04"),
		)
	}
	return resolution
}

func (r *decisionPageResolution) policyApproval() (*review.ApprovalProvenance, bool) {
	if r == nil || r.Approval == nil || r.Approval.Kind != review.ApprovalPolicy {
		return nil, false
	}
	return r.Approval, true
}

func (e *Engine) buildPageData(
	issueID, flowName, title, currentStage string,
	dec *levers.Decision, ctx *decision.DecisionContext, reviewTarget *review.Target,
	decisionID int64,
	resolution *decisionPageResolution,
) (decisionpage.PageData, error) {
	decisionRows, err := e.cfg.Store.AllDecisionRows()
	if err != nil {
		return decisionpage.PageData{}, err
	}
	return e.buildPageDataWithDecisionRows(
		issueID, flowName, title, currentStage, dec, ctx, reviewTarget,
		decisionID, resolution, decisionRows,
	)
}

func (e *Engine) buildPageDataWithDecisionRows(
	issueID, flowName, title, currentStage string,
	dec *levers.Decision, ctx *decision.DecisionContext, reviewTarget *review.Target,
	decisionID int64,
	resolution *decisionPageResolution,
	decisionRows []store.DecisionRow,
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
	decisionStage := ""
	if dec != nil {
		decisionStage = currentStage
	}
	tokensByStage := map[string]int{}
	if runs, runErr := e.cfg.Store.StageRuns(issueID); runErr == nil {
		for _, run := range runs {
			tokensByStage[run.Stage] += run.Tokens
		}
	}
	decisionsByStage := map[string][]decisionpage.FloorDecision{}
	for _, row := range decisionRows {
		if row.IssueID != issueID || row.Status == "pending" {
			continue
		}
		past := decisionpage.FloorDecision{Question: row.Question, Answer: answerSummary(row)}
		href := fmt.Sprintf("decisions/%d.html", row.ID)
		if _, statErr := os.Stat(filepath.Join(e.issueDir(issueID), "decisions", fmt.Sprintf("%d.html", row.ID))); statErr == nil {
			past.Href = href
		}
		decisionsByStage[row.Stage] = append(decisionsByStage[row.Stage], past)
	}

	if currentStage == "" {
		currentStage = firstIncompleteStage(fl, byStage)
	} else if checkpoint, ok := byStage[currentStage]; ok &&
		(checkpoint.Status == "succeeded" || checkpoint.Status == "handoff_authorized") {
		// At a stage boundary the caller's notion of "current" is the stage
		// that just finished; the tower should already point at the next one.
		currentStage = firstIncompleteStage(fl, byStage)
	}
	data := decisionpage.PageData{
		IssueID: issueID, Title: title, StageTotal: len(fl.Stages),
		CurrentStage: currentStage, DecisionStage: decisionStage,
	}
	if resolution != nil {
		data.Answered = resolution.Stamp
		_, data.PolicyApproved = resolution.policyApproval()
		data.AutoResolved = resolution.Automatic
	}
	if decisionID > 0 {
		for _, row := range decisionRows {
			if row.ID != decisionID {
				continue
			}
			data.BlockedFor = decisionBlockedFor(row)
			break
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
		if stage.Name == currentStage {
			data.StageIndex = index + 1
		}
		switch {
		case hasCheckpoint && (checkpoint.Status == "succeeded" || checkpoint.Status == "handoff_authorized"):
			floor.Status = decisionpage.FloorDone
			floor.Note = checkpointNote(checkpoint, tokensByStage[stage.Name])
			data.DoneCount++
			for _, artifact := range checkpoint.Artifacts {
				href, hrefErr := e.decisionArtifactHref(issueID, checkpoint, artifact)
				if hrefErr != nil {
					return decisionpage.PageData{}, hrefErr
				}
				floor.Artifacts = append(floor.Artifacts, decisionpage.FloorLink{
					Name: artifact.Name, Href: href})
			}
			floor.Decisions = decisionsByStage[stage.Name]
		case hasCheckpoint && (checkpoint.Status == "failed" || checkpoint.Status == "killed"):
			floor.Status = decisionpage.FloorFailed
			floor.Note = checkpoint.Failure
		case stage.Name == currentStage:
			floor.Status = decisionpage.FloorCurrent
		default:
			floor.Note = futureStageNote(stage)
		}
		data.Floors = append(data.Floors, floor)
	}
	if data.StageIndex == 0 && len(fl.Stages) > 0 {
		data.StageIndex = len(fl.Stages)
	}

	filesStage := currentStage
	if decisionStage != "" {
		filesStage = decisionStage
	}
	e.fillPageFiles(&data, issueID, filesStage)
	if dec != nil {
		data.Briefing = buildDecisionPageBriefing(
			dec, ctx, reviewTarget, currentStage, decisionID, resolution,
		)
		for _, row := range decisionRows {
			if row.ID == decisionID {
				appendEscalationBriefingEvidence(data.Briefing, row.Evaluation, row.Bindings)
				break
			}
		}
	}
	return data, nil
}

// buildDecisionPageForCheckpoint renders a checkpoint's page using the same
// data path as the daemon. Tests use it to exercise persisted checkpoint and
// archive state without private fixtures.
func (e *Engine) buildDecisionPageForCheckpoint(issueID string, checkpointID int64) (string, error) {
	rows, err := e.cfg.Store.Issues()
	if err != nil {
		return "", err
	}
	var issue store.IssueRow
	foundIssue := false
	for _, row := range rows {
		if row.ID == issueID {
			issue = row
			foundIssue = true
			break
		}
	}
	if !foundIssue {
		return "", fmt.Errorf("issue %s is unavailable", issueID)
	}
	checkpoints, err := e.cfg.Store.StageCheckpoints(issueID)
	if err != nil {
		return "", err
	}
	for _, checkpoint := range checkpoints {
		if checkpoint.ID != checkpointID {
			continue
		}
		data, dataErr := e.buildPageData(issueID, issue.Flow, issue.Title, checkpoint.Stage,
			nil, nil, nil, 0, nil)
		if dataErr != nil {
			return "", dataErr
		}
		content, renderErr := decisionpage.Render(data)
		if renderErr != nil {
			return "", renderErr
		}
		return string(content), nil
	}
	return "", fmt.Errorf("checkpoint %d is unavailable", checkpointID)
}

func (e *Engine) decisionArtifactHref(issueID string, checkpoint store.StageCheckpoint, artifact contextpack.Artifact) (string, error) {
	attemptID := "checkpoint-" + strconv.FormatInt(checkpoint.ID, 10)
	records, err := e.cfg.Store.StageLifecycleRecords(issueID, checkpoint.Stage, attemptID)
	if err != nil {
		return "", err
	}
	var ref *stagelifecycle.ArtifactRef
	for index := len(records) - 1; index >= 0; index-- {
		record := records[index]
		if !record.Committed || !lifecycleReached(record.Substate, stagelifecycle.ArtifactsArchived) {
			continue
		}
		for artifactIndex := range record.Artifacts {
			candidate := record.Artifacts[artifactIndex]
			if candidate.Name == artifact.Name && !strings.Contains(candidate.Path, "/result/") {
				copy := candidate
				ref = &copy
				break
			}
		}
		if ref != nil {
			break
		}
	}
	if ref == nil {
		return "artifacts/" + artifact.Name, nil
	}
	if !safeDecisionArchivePath(ref.Path, ref.Name) ||
		(artifact.SHA256 != "" && ref.SHA256 != artifact.SHA256) {
		return "", &stagelifecycle.DiagnosticError{Code: stagelifecycle.CodeIntegrity, Message: "decision artifact reference does not validate"}
	}
	body, err := os.ReadFile(filepath.Join(e.issueDir(issueID), filepath.FromSlash(ref.Path)))
	if err != nil {
		return "", &stagelifecycle.DiagnosticError{Code: stagelifecycle.CodeIntegrity, Message: "decision artifact reference cannot be read"}
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != ref.SHA256 {
		return "", &stagelifecycle.DiagnosticError{Code: stagelifecycle.CodeIntegrity, Message: "decision artifact reference digest does not validate"}
	}
	return ref.Path, nil
}

func safeDecisionArchivePath(value, name string) bool {
	clean := path.Clean(value)
	return value == clean && !path.IsAbs(value) && strings.HasPrefix(value, "artifacts/attempts/") && path.Base(value) == name && !strings.Contains(value, "../")
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

	evidencePath := filepath.Join(e.issueDir(issueID), "evidence", currentStage, "evidence.json")
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
	ctx *decision.DecisionContext, reviewTarget *review.Target, resolution *decisionPageResolution,
) error {
	if is == nil {
		return fmt.Errorf("decision issue state is unavailable")
	}
	data, err := e.buildPageData(
		is.id, is.flowName, is.title, stage, &d, ctx, reviewTarget, decisionID, resolution,
	)
	if err != nil {
		return fmt.Errorf("build decision page snapshot: %w", err)
	}
	if decisionID > 0 {
		if err := e.cfg.Store.SaveDecisionPageSnapshot(decisionID, data); err != nil {
			return fmt.Errorf("save decision page snapshot: %w", err)
		}
	}
	if err := e.writeRenderedDecisionPage(is.id, stage, decisionID, data); err != nil {
		return fmt.Errorf("publish decision page: %w", err)
	}
	return nil
}

func (e *Engine) decisionPageSnapshot(
	is *issueState, stage string, d levers.Decision,
	ctx *decision.DecisionContext, reviewTarget *review.Target,
) (*decisionpage.PageData, error) {
	if is == nil {
		return nil, fmt.Errorf("decision issue state is unavailable")
	}
	data, err := e.buildPageData(
		is.id, is.flowName, is.title, stage, &d, ctx, reviewTarget, 0, nil,
	)
	if err != nil {
		return nil, err
	}
	return &data, nil
}

func (e *Engine) writeDecisionArchive(
	is *issueState, stage string, decisionID int64, d levers.Decision,
	ctx *decision.DecisionContext, reviewTarget *review.Target, resolution *decisionPageResolution,
) {
	if is == nil || decisionID <= 0 {
		return
	}
	data, err := e.buildPageData(
		is.id, is.flowName, is.title, stage, &d, ctx, reviewTarget, decisionID, resolution,
	)
	if err != nil {
		return
	}
	if err := e.cfg.Store.SaveDecisionPageSnapshot(decisionID, data); err != nil {
		return
	}
	content, err := decisionpage.Render(data)
	if err != nil {
		return
	}
	e.writeDecisionArchiveContent(is.id, decisionID, content)
}

func resolvedDecisionPageSnapshot(row store.DecisionRow) (decisionpage.PageData, bool) {
	if row.PageSnapshot == nil {
		return decisionpage.PageData{}, false
	}
	data := *row.PageSnapshot
	resolution := resolvedStoredDecisionPage(row.Response, row.Approval, row.AnsweredAt, row.Status)
	data.Answered = resolution.Stamp
	_, data.PolicyApproved = resolution.policyApproval()
	data.AutoResolved = resolution.Automatic
	data.BlockedFor = decisionBlockedFor(row)
	dec := decisionFromRow(row)
	data.Briefing = buildDecisionPageBriefing(
		&dec, row.Context, row.Review, row.Stage, row.ID, resolution,
	)
	appendEscalationBriefingEvidence(data.Briefing, row.Evaluation, row.Bindings)
	return data, true
}

func appendEscalationBriefingEvidence(briefing *decisionpage.Briefing, evaluation *review.Evaluation, bindings []review.Binding) {
	if briefing == nil || evaluation == nil {
		return
	}
	briefing.Excerpts = append(briefing.Excerpts, decisionpage.Excerpt{
		Text: decisionPageEscalationSummary(evaluation),
		Cite: "engine-owned escalation gate",
	})
	for _, evidence := range evaluation.Evidence {
		briefing.Excerpts = append(briefing.Excerpts, decisionpage.Excerpt{
			Text: fmt.Sprintf("signal: %s=%s · rule: %s · floor: %s", evidence.Signal, evidence.Value, evidence.Rule, evidence.Floor),
			Cite: "configured decision policy",
		})
	}
	for _, binding := range bindings {
		briefing.Excerpts = append(briefing.Excerpts, decisionpage.Excerpt{
			Text: fmt.Sprintf("item: %s %s %s · sha256 %s · floor: %s -> %s",
				binding.Item.Kind, binding.Item.Path, binding.Item.Operation, binding.Item.Hash,
				binding.RequiredFloor, binding.EffectiveFloor),
			Cite: "exact approval binding",
		})
		for _, dependency := range binding.Dependencies {
			briefing.Excerpts = append(briefing.Excerpts, decisionpage.Excerpt{
				Text: fmt.Sprintf("dependency: %s/%s · sha256 %s", dependency.Kind, dependency.ID, dependency.Hash),
				Cite: "exact approval binding",
			})
		}
	}
	briefing.Excerpts = append(briefing.Excerpts, decisionpage.Excerpt{
		Text: decisionPageModelSummary(evaluation.Model),
		Cite: "model metadata; not authorization",
	})
}

func (e *Engine) writeResolvedDecisionArchive(decisionID int64) error {
	row, ok, err := e.cfg.Store.DecisionByID(decisionID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("decision %d is unavailable", decisionID)
	}
	data, ok := resolvedDecisionPageSnapshot(row)
	if !ok {
		return fmt.Errorf("decision %d page snapshot is unavailable", decisionID)
	}
	content, err := decisionpage.Render(data)
	if err != nil {
		return err
	}
	return e.writeDecisionArchiveContent(row.IssueID, row.ID, content)
}

func decisionArchiveNeedsResolution(archivePath string) (bool, error) {
	content, err := os.ReadFile(archivePath)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if bytes.Contains(content, []byte(`<body data-decision-state="pending">`)) {
		return true, nil
	}
	if bytes.Contains(content, []byte(`<body data-decision-state="resolved">`)) ||
		bytes.Contains(content, []byte(`<span class="answered">`)) {
		return false, nil
	}
	return bytes.Contains(content, []byte("Do this now")) &&
		bytes.Contains(content, []byte("After you answer")), nil
}

func (e *Engine) writeDecisionArchiveContent(issueID string, decisionID int64, content []byte) error {
	decisionsDir := filepath.Join(e.issueDir(issueID), "decisions")
	if err := os.MkdirAll(decisionsDir, 0o755); err != nil {
		return err
	}
	return writeFileAtomically(
		filepath.Join(decisionsDir, fmt.Sprintf("%d.html", decisionID)), content, 0o644,
	)
}

func writeFileAtomically(filename string, content []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(filename), ".decision-page-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, filename)
}

func (e *Engine) writeRenderedDecisionPage(
	issueID, stage string, decisionID int64, data decisionpage.PageData,
) error {
	content, err := decisionpage.Render(data)
	if err != nil {
		return err
	}
	if decisionID > 0 {
		if err := e.writeDecisionArchiveContent(issueID, decisionID, content); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(e.issueDir(issueID), 0o755); err != nil {
		return err
	}
	latest := filepath.Join(e.issueDir(issueID), decisionpage.FileName)
	if err := writeFileAtomically(latest, content, 0o644); err != nil {
		return err
	}
	e.emit(core.EvArtifactProduced, issueID, map[string]any{
		"stage": stage, "artifact": decisionpage.FileName, "path": latest, "decision_id": decisionID,
	})
	return nil
}

func (e *Engine) refreshDecisionPage(issueID string) {
	decisionRows, err := e.cfg.Store.AllDecisionRows()
	if err != nil {
		return
	}
	e.refreshDecisionPageWithDecisionRows(issueID, decisionRows)
}

func (e *Engine) refreshDecisionPageWithDecisionRows(issueID string, decisionRows []store.DecisionRow) {
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
			target := pending.Review
			stage := pending.Stage
			id := pending.ID
			e.mu.Unlock()
			data, err := e.buildPageDataWithDecisionRows(
				is.id, is.flowName, is.title, stage, &dec, ctx, target, id, nil, decisionRows,
			)
			if err == nil {
				if err := e.cfg.Store.SaveDecisionPageSnapshot(id, data); err != nil {
					return
				}
				e.writeRenderedDecisionPage(is.id, stage, id, data)
			}
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
	data, err := e.buildPageDataWithDecisionRows(
		issueID, flowName, title, currentStage, nil, nil, nil, 0, nil, decisionRows,
	)
	if err == nil {
		e.writeRenderedDecisionPage(issueID, currentStage, 0, data)
	}
}

func (e *Engine) refreshDecisionPageFromRow(row store.IssueRow, decisionRows []store.DecisionRow) {
	e.mu.Lock()
	is := e.issues[row.ID]
	e.mu.Unlock()
	if is != nil {
		e.refreshDecisionPageWithDecisionRows(row.ID, decisionRows)
		return
	}
	data, err := e.buildPageDataWithDecisionRows(
		row.ID, row.Flow, row.Title, "", nil, nil, nil, 0, nil, decisionRows,
	)
	if err == nil {
		e.writeRenderedDecisionPage(row.ID, data.CurrentStage, 0, data)
	}
}

func answerText(response levers.Response) string {
	answer := ""
	if response.Kind == levers.DecisionChoice && response.Option != nil {
		answer = fmt.Sprintf("option %d", *response.Option+1)
	} else if response.Kind == levers.DecisionFreeform {
		answer = strings.TrimSpace(response.Text)
	}
	if answer == "" {
		answer = "response recorded"
	}
	return answer
}

func answerStamp(response levers.Response, answeredAt time.Time) string {
	return fmt.Sprintf("Answered: %s · %s", answerText(response), answeredAt.UTC().Format("2006-01-02 15:04"))
}

func decisionBlockedFor(row store.DecisionRow) string {
	if row.CreatedAt.IsZero() {
		return ""
	}
	var elapsed time.Duration
	switch {
	case row.Status == "pending":
		elapsed = time.Since(row.CreatedAt)
	case row.Status == "answered" && !row.AnsweredAt.IsZero():
		elapsed = row.AnsweredAt.Sub(row.CreatedAt)
	default:
		return ""
	}
	if elapsed < 0 {
		elapsed = 0
	}
	return fmt.Sprintf("%d min", int(elapsed.Minutes()))
}

// answerSummary is the short past-decision line shown inside an expanded
// done floor; auto-resolved rows are labeled as such.
func answerSummary(row store.DecisionRow) string {
	if row.Status == "auto" {
		return "auto: " + answerText(row.Response)
	}
	return answerText(row.Response)
}
