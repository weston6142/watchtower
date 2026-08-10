package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/stagelifecycle"
	"github.com/weston6142/watchtower/internal/store"
)

func (e *Engine) lifecycleAttemptFor(is *issueState, st flow.Stage, checkpointID int64) (store.StageLifecycleAttempt, []stagelifecycle.Record, error) {
	records, err := e.cfg.Store.StageLifecycleRecords(is.id, st.Name, "")
	if err != nil {
		return store.StageLifecycleAttempt{}, nil, err
	}
	attemptID := fmt.Sprintf("checkpoint-%d", checkpointID)
	if !(is.retryUnmerged && st.MergeBarrier) {
		for index := len(records) - 1; index >= 0; index-- {
			if records[index].AttemptID != "" {
				attemptID = records[index].AttemptID
				break
			}
		}
	}
	attempt := store.BeginAttempt(is.id, st.Name, attemptID)
	if err := e.cfg.Store.CreateStageLifecycleAttempt(attempt); err != nil {
		return store.StageLifecycleAttempt{}, nil, err
	}
	return attempt, records, nil
}

// startOrResumeLifecycleAttempt establishes the stable attempt identity before
// any model call. The durable result and checkpoint records are read by the
// caller to decide whether the runner can be skipped.
func (e *Engine) startOrResumeLifecycleAttempt(
	_ context.Context, is *issueState, st flow.Stage, checkpointID int64,
) error {
	_, _, err := e.lifecycleAttemptFor(is, st, checkpointID)
	return err
}

func (e *Engine) lifecycleResultFromRecord(record stagelifecycle.Record) contextpack.AttemptResult {
	result := contextpack.AttemptResult{
		AttemptID: record.AttemptID, IssueID: record.IssueID, Stage: record.Stage,
		ResultPath: record.ResultRef, ResultSHA256: record.ResultDigest,
	}
	for _, artifact := range record.Artifacts {
		result.Artifacts = append(result.Artifacts, contextpack.AttemptArtifact{
			Name: artifact.Name, Path: artifact.Path, SHA256: artifact.SHA256,
		})
	}
	return result
}

func lifecycleRecordResult(records []stagelifecycle.Record) (stagelifecycle.Record, bool) {
	for index := len(records) - 1; index >= 0; index-- {
		if records[index].Substate == stagelifecycle.RunnerSucceeded && records[index].ResultRef != "" {
			return records[index], true
		}
	}
	return stagelifecycle.Record{}, false
}

func lifecycleReached(current stagelifecycle.Substate, target stagelifecycle.Substate) bool {
	order := []stagelifecycle.Substate{
		stagelifecycle.RunnerSucceeded, stagelifecycle.ArtifactsValidated,
		stagelifecycle.ArtifactsArchived, stagelifecycle.GateResolved,
		stagelifecycle.VerificationPassed, stagelifecycle.FinalizationReady,
	}
	position := func(value stagelifecycle.Substate) int {
		for index, item := range order {
			if item == value {
				return index
			}
		}
		return -1
	}
	return position(current) >= position(target) && position(target) >= 0
}

func (e *Engine) restoreLifecycleResult(issueDir string, record stagelifecycle.Record, workdir string) (contextpack.AttemptResult, error) {
	result := e.lifecycleResultFromRecord(record)
	if result.ResultPath == "" || result.ResultSHA256 == "" {
		return contextpack.AttemptResult{}, &stagelifecycle.DiagnosticError{
			Code: stagelifecycle.CodeMissingResult, Message: "durable model result is missing",
		}
	}
	path := filepath.Join(issueDir, filepath.FromSlash(result.ResultPath))
	body, err := os.ReadFile(path)
	if err != nil {
		return contextpack.AttemptResult{}, &stagelifecycle.DiagnosticError{
			Code: stagelifecycle.CodeMissingResult, Message: "durable model result cannot be read",
		}
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != result.ResultSHA256 {
		return contextpack.AttemptResult{}, &stagelifecycle.DiagnosticError{
			Code: stagelifecycle.CodeIntegrity, Message: "durable model result digest does not validate",
		}
	}
	if err := contextpack.MaterializeAttemptArtifacts(issueDir, workdir, result.Artifacts); err != nil {
		return contextpack.AttemptResult{}, &stagelifecycle.DiagnosticError{
			Code: stagelifecycle.CodeIntegrity, Message: "durable model output does not validate",
		}
	}
	return result, nil
}

func lifecycleArtifactRefs(values []contextpack.AttemptArtifact) []stagelifecycle.ArtifactRef {
	refs := make([]stagelifecycle.ArtifactRef, 0, len(values))
	for _, value := range values {
		refs = append(refs, stagelifecycle.ArtifactRef{Name: value.Name, Path: value.Path, SHA256: value.SHA256})
	}
	return refs
}

func attemptArtifactRefs(values []stagelifecycle.ArtifactRef) []contextpack.AttemptArtifact {
	refs := make([]contextpack.AttemptArtifact, 0, len(values))
	for _, value := range values {
		refs = append(refs, contextpack.AttemptArtifact{Name: value.Name, Path: value.Path, SHA256: value.SHA256})
	}
	return refs
}

func lifecyclePayloadDigest(substate stagelifecycle.Substate, result contextpack.AttemptResult, artifacts []contextpack.AttemptArtifact) string {
	encoded, _ := json.Marshal(struct {
		Substate  stagelifecycle.Substate
		Result    contextpack.AttemptResult
		Artifacts []contextpack.AttemptArtifact
	}{Substate: substate, Result: result, Artifacts: artifacts})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (e *Engine) commitLifecycleSubstate(
	attempt store.StageLifecycleAttempt, substate stagelifecycle.Substate,
	payloadDigest string, result contextpack.AttemptResult,
	artifacts []contextpack.AttemptArtifact,
) error {
	latest, found, err := e.cfg.Store.LatestCommittedStageLifecycle(attempt)
	if err != nil {
		return err
	}
	version := 1
	predecessorVersion := 0
	var predecessor *stagelifecycle.Record
	if found {
		version = latest.Version + 1
		predecessorVersion = latest.Version
		copy := latest
		predecessor = &copy
		if next, ok := stagelifecycle.Next(latest.Substate); !ok || next != substate {
			if latest.Substate == substate {
				return nil
			}
			return &stagelifecycle.DiagnosticError{Code: stagelifecycle.CodeInvalidState, Message: "lifecycle transition is out of order"}
		}
	}
	transitionID := attempt.AttemptID + ":" + string(substate)
	record := stagelifecycle.Record{
		SchemaVersion:      stagelifecycle.SchemaVersion,
		IssueID:            attempt.IssueID,
		Stage:              attempt.Stage,
		AttemptID:          attempt.AttemptID,
		Version:            version,
		Substate:           substate,
		PredecessorVersion: predecessorVersion,
		TransitionID:       transitionID,
		PayloadDigest:      payloadDigest,
		ResultRef:          result.ResultPath,
		ResultDigest:       result.ResultSHA256,
		Artifacts:          lifecycleArtifactRefs(artifacts),
	}
	if err := stagelifecycle.ValidateTransition(predecessor, record); err != nil {
		return err
	}
	if substate == stagelifecycle.ArtifactsArchived {
		if err := e.cfg.Store.PutStageArchiveManifest(attempt, transitionID, artifacts); err != nil {
			return err
		}
	}
	if err := e.cfg.Store.PrepareStageLifecycle(record); err != nil {
		return err
	}
	if err := e.cfg.Store.CommitPreparedStageLifecycle(record); err != nil {
		return err
	}
	return nil
}

// recoverLifecycleAttempt validates the durable result and makes it available
// in the stage workdir without invoking a runner.
func (e *Engine) recoverLifecycleAttempt(
	_ context.Context, is *issueState, _ flow.Stage, attempt store.StageLifecycleAttempt,
) error {
	records, err := e.cfg.Store.StageLifecycleRecords(attempt.IssueID, attempt.Stage, attempt.AttemptID)
	if err != nil {
		return err
	}
	record, found := lifecycleRecordResult(records)
	if !found {
		return &stagelifecycle.DiagnosticError{Code: stagelifecycle.CodeMissingResult, Message: "durable runner result checkpoint is missing"}
	}
	_, err = e.restoreLifecycleResult(e.issueDir(is.id), record, e.stageWorkdir(is, e.cfg.Flows[is.flowName].Stages[is.stageIdx]))
	return err
}

func lifecycleArchiveRefs(archive []contextpack.AttemptArtifact) []contextpack.Artifact {
	result := make([]contextpack.Artifact, 0, len(archive))
	for _, artifact := range archive {
		result = append(result, contextpack.Artifact{Name: artifact.Name, SHA256: artifact.SHA256})
	}
	return result
}

func lifecycleErrorCode(err error) stagelifecycle.DiagnosticCode {
	var diagnostic *stagelifecycle.DiagnosticError
	if errors.As(err, &diagnostic) {
		return diagnostic.Code
	}
	return ""
}

func lifecycleErrorMessage(err error) string {
	code := lifecycleErrorCode(err)
	if code == "" {
		return err.Error()
	}
	return string(code)
}

func lifecycleDigestFromArtifacts(artifacts []contextpack.Artifact) string {
	var builder strings.Builder
	for _, artifact := range artifacts {
		builder.WriteString(artifact.Name)
		builder.WriteByte(0)
		builder.WriteString(artifact.SHA256)
		builder.WriteByte(0)
	}
	digest := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(digest[:])
}

func (e *Engine) materializeStageContext(issueID, workdir string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	needed := make(map[string]bool, len(names))
	for _, name := range names {
		needed[name] = true
	}
	refsByName := map[string]contextpack.AttemptArtifact{}
	checkpoints, err := e.cfg.Store.StageCheckpoints(issueID)
	if err != nil {
		return err
	}
	for _, checkpoint := range checkpoints {
		if checkpoint.Status != "succeeded" && checkpoint.Status != "handoff_authorized" {
			continue
		}
		records, recordsErr := e.cfg.Store.StageLifecycleRecords(issueID, checkpoint.Stage, "")
		if recordsErr != nil {
			return recordsErr
		}
		for index := len(records) - 1; index >= 0; index-- {
			record := records[index]
			if !record.Committed || !lifecycleReached(record.Substate, stagelifecycle.ArtifactsArchived) {
				continue
			}
			for _, artifact := range record.Artifacts {
				if needed[artifact.Name] && !strings.Contains(artifact.Path, "/result/") {
					refsByName[artifact.Name] = contextpack.AttemptArtifact{
						Name: artifact.Name, Path: artifact.Path, SHA256: artifact.SHA256,
					}
				}
			}
			break
		}
	}
	var legacy []string
	var refs []contextpack.AttemptArtifact
	for _, name := range names {
		if ref, ok := refsByName[name]; ok {
			refs = append(refs, ref)
		} else {
			legacy = append(legacy, name)
		}
	}
	if len(refs) > 0 {
		if err := contextpack.MaterializeAttemptArtifacts(e.issueDir(issueID), workdir, refs); err != nil {
			return err
		}
	}
	if len(legacy) > 0 {
		return contextpack.MaterializeLegacy(e.issueDir(issueID), workdir, legacy)
	}
	return nil
}

// publishLegacyCompatibility preserves the old artifact URL for a newly
// created name without ever replacing an existing legacy byte stream. New
// recovery reads the attempt path; this one-time link keeps older consumers
// and finalization helpers readable during migration.
func publishLegacyCompatibility(issueDir string, refs []contextpack.AttemptArtifact) error {
	for _, ref := range refs {
		source := filepath.Join(issueDir, filepath.FromSlash(ref.Path))
		destination := filepath.Join(issueDir, "artifacts", filepath.FromSlash(ref.Name))
		if _, err := os.Lstat(destination); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		if err := os.Link(source, destination); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

func (e *Engine) lifecycleRecoveryState(row store.IssueRow) (*issueState, bool, error) {
	configured, ok := e.cfg.Flows[row.Flow]
	if !ok {
		return nil, false, nil
	}
	for index, stage := range configured.Stages {
		records, err := e.cfg.Store.StageLifecycleRecords(row.ID, stage.Name, "")
		if err != nil {
			return nil, false, err
		}
		if len(records) == 0 {
			continue
		}
		attemptID := ""
		for _, record := range records {
			if record.Committed && (attemptID == "" || lifecycleAttemptAfter(record.AttemptID, attemptID)) {
				attemptID = record.AttemptID
			}
		}
		if attemptID == "" {
			continue
		}
		var latest stagelifecycle.Record
		var predecessor *stagelifecycle.Record
		for _, record := range records {
			if !record.Committed || record.AttemptID != attemptID {
				continue
			}
			if err := stagelifecycle.ValidateRecord(record); err != nil {
				return nil, true, err
			}
			if err := stagelifecycle.ValidateTransition(predecessor, record); err != nil {
				return nil, true, err
			}
			copy := record
			predecessor = &copy
			latest = record
		}
		if latest.Substate == "" || lifecycleReached(latest.Substate, stagelifecycle.FinalizationReady) {
			continue
		}
		if _, err := validateDurableLifecycleRecord(e.issueDir(row.ID), latest); err != nil {
			return nil, true, err
		}
		return &issueState{
			id: row.ID, title: row.Title, body: row.Body, flowName: row.Flow,
			matrix: matrixFromStrings(row.Levers), priority: row.Priority,
			dependsOn: append([]string(nil), row.DependsOn...), stageIdx: index,
			terminal: true, planReview: row.PlanReviewPolicy,
		}, true, nil
	}
	return nil, false, nil
}

func lifecycleAttemptAfter(left, right string) bool {
	const prefix = "checkpoint-"
	leftNumber, leftErr := strconv.ParseInt(strings.TrimPrefix(left, prefix), 10, 64)
	rightNumber, rightErr := strconv.ParseInt(strings.TrimPrefix(right, prefix), 10, 64)
	if strings.HasPrefix(left, prefix) && strings.HasPrefix(right, prefix) && leftErr == nil && rightErr == nil {
		return leftNumber > rightNumber
	}
	return left > right
}

func validateDurableLifecycleRecord(issueDir string, record stagelifecycle.Record) (string, error) {
	if record.ResultRef == "" || record.ResultDigest == "" {
		return "", &stagelifecycle.DiagnosticError{Code: stagelifecycle.CodeMissingResult, Message: "durable model result is missing"}
	}
	body, err := os.ReadFile(filepath.Join(issueDir, filepath.FromSlash(record.ResultRef)))
	if err != nil {
		return "", &stagelifecycle.DiagnosticError{Code: stagelifecycle.CodeMissingResult, Message: "durable model result cannot be read"}
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != record.ResultDigest {
		return "", &stagelifecycle.DiagnosticError{Code: stagelifecycle.CodeIntegrity, Message: "durable model result digest does not validate"}
	}
	if len(record.Artifacts) > 0 {
		workdir, err := os.MkdirTemp("", "watchtower-lifecycle-recovery-")
		if err != nil {
			return "", &stagelifecycle.DiagnosticError{Code: stagelifecycle.CodeIntegrity, Message: "durable model output validation could not start"}
		}
		defer os.RemoveAll(workdir)
		refs := make([]contextpack.AttemptArtifact, 0, len(record.Artifacts))
		for _, artifact := range record.Artifacts {
			refs = append(refs, contextpack.AttemptArtifact{Name: artifact.Name, Path: artifact.Path, SHA256: artifact.SHA256})
		}
		if err := contextpack.MaterializeAttemptArtifacts(issueDir, workdir, refs); err != nil {
			return "", &stagelifecycle.DiagnosticError{Code: stagelifecycle.CodeIntegrity, Message: "durable model output does not validate"}
		}
	}
	return record.AttemptID, nil
}
