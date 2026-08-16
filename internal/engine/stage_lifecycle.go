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

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/stagelifecycle"
	"github.com/weston6142/watchtower/internal/stageresult"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/workspace"
)

const capabilityRecoverySchemaVersion = 1

type capabilityRecoveryEnvelope struct {
	SchemaVersion   int      `json:"schema_version"`
	Stage           string   `json:"stage"`
	AttemptID       string   `json:"attempt_id"`
	TrustedTree     string   `json:"trusted_tree"`
	AuthorityDigest string   `json:"authority_digest"`
	Reason          string   `json:"reason"`
	OriginalState   string   `json:"original_state"`
	OriginalPreSHA  string   `json:"original_pre_sha"`
	OriginalCleanup []string `json:"original_cleanup,omitempty"`
}

func capabilityRecoveryReason(err error) (capability.FailureReason, bool) {
	var policy *capability.PolicyError
	if !errors.As(err, &policy) {
		return "", false
	}
	switch policy.Reason {
	case capability.ReasonRuntimeDenied, capability.ReasonPostStageViolation:
		return policy.Reason, true
	default:
		return "", false
	}
}

func (e *Engine) markCapabilityWorkspaceRejected(
	is *issueState,
	stage string,
	identity capability.AttemptIdentity,
	contract capability.CompiledContract,
	baseline capability.Baseline,
	cause error,
) error {
	reason, ok := capabilityRecoveryReason(cause)
	if !ok || is.wsPath == "" || baseline.Git.Head == "" || baseline.Git.Tree == "" {
		return nil
	}
	_, ok = e.cfg.Workspace.(workspace.RecoveryProvider)
	if !ok {
		return fmt.Errorf("capability recovery provider is unavailable")
	}
	observedRef, _ := gitRevision(is.wsPath, "refs/heads/"+baseline.Git.Branch)
	integration, found, err := e.cfg.Store.IssueIntegration(is.id)
	if err != nil {
		return err
	}
	if !found {
		integration = store.IssueIntegration{
			IssueID: is.id, State: store.IntegrationClaimed, PreSHA: is.baseRef,
			Worktree: is.wsPath, Branch: is.branch,
		}
	}
	envelope := capabilityRecoveryEnvelope{
		SchemaVersion: capabilityRecoverySchemaVersion, Stage: stage, AttemptID: identity.AttemptID,
		TrustedTree: baseline.Git.Tree, AuthorityDigest: contract.AuthorityDigest,
		Reason: string(reason), OriginalState: integration.State, OriginalPreSHA: integration.PreSHA,
		OriginalCleanup: append([]string(nil), integration.Cleanup...),
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	integration.State = store.IntegrationCapabilityRecoveryNeeded
	integration.PreSHA = baseline.Git.Head
	integration.LandedSHA = observedRef
	integration.LastError = string(encoded)
	integration.Worktree = is.wsPath
	integration.Branch = baseline.Git.Branch
	integration.Cleanup = nil
	if err := e.cfg.Store.SetIssueIntegration(integration); err != nil {
		return err
	}
	if err := e.cfg.Store.AppendCapabilityAudit(capability.AuditRecord{
		Attempt: identity, ContractID: contract.ContractID, Phase: "quarantine", Outcome: "passed", Reason: reason,
	}); err != nil {
		return err
	}
	e.emit(core.EvCapabilityRejected, is.id, map[string]any{
		"stage": stage, "attempt_id": identity.AttemptID, "contract_id": contract.ContractID,
		"reason": reason, "retry_disposition": failure.RetryAfterStateChange, "required_state": failure.StateTrustedWorkspace,
	})
	return nil
}

func (e *Engine) recoverPendingCapabilityWorkspace(_ context.Context, is *issueState) (bool, error) {
	integration, found, err := e.cfg.Store.IssueIntegration(is.id)
	if err != nil || !found || integration.State != store.IntegrationCapabilityRecoveryNeeded {
		return false, err
	}
	var envelope capabilityRecoveryEnvelope
	if json.Unmarshal([]byte(integration.LastError), &envelope) != nil || envelope.SchemaVersion != capabilityRecoverySchemaVersion ||
		envelope.Stage == "" || envelope.AttemptID == "" || envelope.TrustedTree == "" || envelope.AuthorityDigest == "" {
		return false, fmt.Errorf("capability recovery evidence is incomplete")
	}
	if envelope.Reason != string(capability.ReasonRuntimeDenied) && envelope.Reason != string(capability.ReasonPostStageViolation) {
		return false, fmt.Errorf("capability recovery reason is invalid")
	}
	record, found, err := e.cfg.Store.CapabilityAttempt(is.id, envelope.Stage, envelope.AttemptID)
	if err != nil || !found || record.Contract.AuthorityDigest != envelope.AuthorityDigest {
		return false, fmt.Errorf("capability recovery authority mismatch")
	}
	provider, ok := e.cfg.Workspace.(workspace.RecoveryProvider)
	if !ok {
		return false, fmt.Errorf("capability recovery provider is unavailable")
	}
	path, release, err := provider.Recover(workspace.RecoveryRequest{
		IssueID: is.id, RejectedPath: integration.Worktree, Provider: provider.Name(),
		Repository: provider.RepositoryRoot(), Branch: integration.Branch,
		TrustedCommit: integration.PreSHA, TrustedTree: envelope.TrustedTree,
		ObservedCommit: integration.LandedSHA, ObservedRef: integration.LandedSHA,
		CapabilityAttemptID: envelope.AttemptID,
	})
	if err != nil {
		return false, err
	}
	baseline, err := (capability.Observer{}).Capture(path, nil)
	if err != nil || baseline.Git.Head != integration.PreSHA || baseline.Git.Tree != envelope.TrustedTree {
		_ = release()
		return false, fmt.Errorf("recovered capability workspace does not match trusted baseline")
	}
	identity := capability.AttemptIdentity{IssueID: is.id, Stage: envelope.Stage, AttemptID: envelope.AttemptID}
	if err := e.cfg.Store.AppendCapabilityAudit(capability.AuditRecord{
		Attempt: identity, ContractID: record.Contract.ContractID, Phase: "recovery", Outcome: "passed",
	}); err != nil {
		return false, err
	}
	integration.State = envelope.OriginalState
	if integration.State == "" {
		integration.State = store.IntegrationClaimed
	}
	integration.PreSHA = envelope.OriginalPreSHA
	integration.LandedSHA = ""
	integration.LastError = ""
	integration.Worktree = path
	integration.Cleanup = append([]string(nil), envelope.OriginalCleanup...)
	if err := e.cfg.Store.SetIssueIntegration(integration); err != nil {
		return false, err
	}
	e.mu.Lock()
	is.wsPath, is.wsRelease = path, release
	is.branch = integration.Branch
	e.mu.Unlock()
	e.emit(core.EvCapabilityValidated, is.id, map[string]any{
		"stage": envelope.Stage, "attempt_id": envelope.AttemptID, "contract_id": record.Contract.ContractID,
		"outcome": "workspace_recovered",
	})
	return true, nil
}

func resultProducerForStage(st flow.Stage) (stageresult.Kind, string, bool, error) {
	var kind stageresult.Kind
	producer := ""
	for _, agent := range st.Agents {
		candidate, ok := stageresult.KindForAgentPackage(agent.Package)
		if !ok {
			continue
		}
		if producer != "" {
			return "", "", false, fmt.Errorf("stage %q has multiple structured result producers %q and %q", st.Name, producer, agent.Package)
		}
		kind, producer = candidate, agent.Package
	}
	return kind, producer, producer != "", nil
}

func (e *Engine) latestValidStageResult(issueID, actualStage string, kind stageresult.Kind) (*stageresult.Result, error) {
	summary, found, err := e.cfg.Store.LatestValidStageResultAttempt(issueID, actualStage, kind)
	if err != nil || !found {
		return nil, err
	}
	reference, found, err := e.cfg.Store.StageLifecycleResult(summary)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, &stagelifecycle.DiagnosticError{Code: stagelifecycle.CodeMissingResult, Message: "structured stage result summary has no immutable manifest"}
	}
	loaded, err := contextpack.LoadAttemptResult(e.issueDir(issueID), reference)
	if err != nil {
		return nil, &stagelifecycle.DiagnosticError{Code: stagelifecycle.CodeIntegrity, Message: "structured stage result manifest cannot be validated"}
	}
	if loaded.StageResult == nil || loaded.StageResult.StageKind != summary.StageResultKind ||
		loaded.StageResult.Outcome != summary.StageResultOutcome ||
		loaded.StageResult.ValidationStatus != summary.StageResultStatus ||
		loaded.StageResult.PredecessorAttemptID != summary.PredecessorAttemptID {
		return nil, &stagelifecycle.DiagnosticError{Code: stagelifecycle.CodeIntegrity, Message: "structured stage result summary conflicts with immutable manifest"}
	}
	result := *loaded.StageResult
	return &result, nil
}

// hasResumableStageLifecycle reports whether retrying can continue from a
// durable model result without starting another runner. Invalid or incomplete
// results are deliberately excluded and must use the ordinary retry gate.
func (e *Engine) hasResumableStageLifecycle(issueID, stage string) (bool, error) {
	attempts, err := e.cfg.Store.StageLifecycleAttempts(issueID, stage)
	if err != nil {
		return false, err
	}
	for index := len(attempts) - 1; index >= 0; index-- {
		attempt := attempts[index]
		if attempt.ResultPath == "" || attempt.ResultSHA256 == "" {
			continue
		}
		if attempt.StageResultSchemaVersion == 0 {
			return true, nil
		}
		return attempt.StageResultSchemaVersion == stageresult.SchemaVersion &&
			attempt.StageResultStatus == stageresult.ValidationValid &&
			attempt.StageResultOutcome == stageresult.OutcomeCompleted, nil
	}
	return false, nil
}

func (e *Engine) lifecycleAttemptFor(is *issueState, st flow.Stage, checkpointID int64) (store.StageLifecycleAttempt, []stagelifecycle.Record, error) {
	records, err := e.cfg.Store.StageLifecycleRecords(is.id, st.Name, "")
	if err != nil {
		return store.StageLifecycleAttempt{}, nil, err
	}
	attempts, err := e.cfg.Store.StageLifecycleAttempts(is.id, st.Name)
	if err != nil {
		return store.StageLifecycleAttempt{}, nil, err
	}
	attemptID := fmt.Sprintf("checkpoint-%d", checkpointID)
	revisionRequired := false
	checkpoints, err := e.cfg.Store.StageCheckpoints(is.id)
	if err != nil {
		return store.StageLifecycleAttempt{}, nil, err
	}
	for index := len(checkpoints) - 1; index >= 0; index-- {
		checkpoint := checkpoints[index]
		if checkpoint.ID == checkpointID || checkpoint.Stage != st.Name {
			continue
		}
		revisionRequired = checkpoint.Status == "revision_required"
		break
	}
	if !revisionRequired && !(is.retryUnmerged && st.MergeBarrier) {
		var latestAttempt *store.StageLifecycleAttempt
		for _, candidate := range attempts {
			if candidate.ResultPath != "" &&
				(latestAttempt == nil || lifecycleAttemptAfter(candidate.AttemptID, latestAttempt.AttemptID)) {
				copy := candidate
				latestAttempt = &copy
			}
		}
		latestAttemptID := ""
		if latestAttempt != nil {
			switch {
			case latestAttempt.StageResultSchemaVersion == 0:
				latestAttemptID = latestAttempt.AttemptID
			case latestAttempt.StageResultSchemaVersion == stageresult.SchemaVersion &&
				latestAttempt.StageResultStatus == stageresult.ValidationValid &&
				latestAttempt.StageResultOutcome == stageresult.OutcomeCompleted:
				latestAttemptID = latestAttempt.AttemptID
			}
		}
		if latestAttempt == nil {
			for _, record := range records {
				if record.AttemptID != "" &&
					(latestAttemptID == "" || lifecycleAttemptAfter(record.AttemptID, latestAttemptID)) {
					latestAttemptID = record.AttemptID
				}
			}
		}
		if latestAttemptID != "" {
			attemptID = latestAttemptID
		}
	}
	selectedRecords := records[:0]
	for _, record := range records {
		if record.AttemptID == attemptID {
			selectedRecords = append(selectedRecords, record)
		}
	}
	attempt := store.BeginAttempt(is.id, st.Name, attemptID)
	if err := e.cfg.Store.CreateStageLifecycleAttempt(attempt); err != nil {
		return store.StageLifecycleAttempt{}, nil, err
	}
	return attempt, selectedRecords, nil
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

func lifecycleRunnerRecord(result contextpack.AttemptResult) stagelifecycle.Record {
	return stagelifecycle.Record{
		SchemaVersion: stagelifecycle.SchemaVersion,
		IssueID:       result.IssueID,
		Stage:         result.Stage,
		AttemptID:     result.AttemptID,
		Version:       1,
		Substate:      stagelifecycle.RunnerSucceeded,
		TransitionID:  result.AttemptID + ":" + string(stagelifecycle.RunnerSucceeded),
		PayloadDigest: lifecyclePayloadDigest(stagelifecycle.RunnerSucceeded, result, result.Artifacts),
		ResultRef:     result.ResultPath,
		ResultDigest:  result.ResultSHA256,
		Artifacts:     lifecycleArtifactRefs(result.Artifacts),
	}
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
	if target == "" {
		return false
	}
	for current != target {
		next, ok := stagelifecycle.Next(target)
		if !ok {
			return false
		}
		target = next
	}
	return true
}

func (e *Engine) restoreLifecycleResult(issueDir string, record stagelifecycle.Record, workdir string) (contextpack.AttemptResult, error) {
	result := e.lifecycleResultFromRecord(record)
	if result.ResultPath == "" || result.ResultSHA256 == "" {
		return contextpack.AttemptResult{}, &stagelifecycle.DiagnosticError{
			Code: stagelifecycle.CodeMissingResult, Message: "durable model result is missing",
		}
	}
	loaded, err := contextpack.LoadAttemptResult(issueDir, result)
	if err != nil {
		code := stagelifecycle.CodeIntegrity
		if errors.Is(err, os.ErrNotExist) {
			code = stagelifecycle.CodeMissingResult
		}
		return contextpack.AttemptResult{}, &stagelifecycle.DiagnosticError{
			Code: code, Message: "durable model result cannot be validated",
		}
	}
	if !lifecycleArtifactsMatch(record.Artifacts, loaded.Artifacts) {
		return contextpack.AttemptResult{}, &stagelifecycle.DiagnosticError{
			Code: stagelifecycle.CodeIntegrity, Message: "durable model output conflicts with checkpoint",
		}
	}
	if err := contextpack.MaterializeAttemptArtifacts(issueDir, workdir, loaded.Artifacts); err != nil {
		return contextpack.AttemptResult{}, &stagelifecycle.DiagnosticError{
			Code: stagelifecycle.CodeIntegrity, Message: "durable model output does not validate",
		}
	}
	return loaded, nil
}

func lifecycleArtifactsMatch(recorded []stagelifecycle.ArtifactRef, loaded []contextpack.AttemptArtifact) bool {
	if len(recorded) != len(loaded) {
		return false
	}
	byName := make(map[string]contextpack.AttemptArtifact, len(loaded))
	for _, artifact := range loaded {
		byName[artifact.Name] = artifact
	}
	for _, artifact := range recorded {
		candidate, ok := byName[artifact.Name]
		if !ok || candidate.Path != artifact.Path || candidate.SHA256 != artifact.SHA256 {
			return false
		}
	}
	return true
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
	return e.notifyBoundary(context.Background(), DurableBoundary{
		Kind: BoundaryStageLifecycle, ID: string(substate), IssueID: attempt.IssueID, Stage: attempt.Stage,
	})
}

func (e *Engine) commitRehydratedArtifactReviewGate(issueID, stage string, checkpointID int64) error {
	attemptID := fmt.Sprintf("checkpoint-%d", checkpointID)
	records, err := e.cfg.Store.StageLifecycleRecords(issueID, stage, attemptID)
	if err != nil || len(records) == 0 {
		return err
	}
	attempt := store.BeginAttempt(issueID, stage, attemptID)
	if err := e.cfg.Store.CreateStageLifecycleAttempt(attempt); err != nil {
		return err
	}
	latest, found, err := e.cfg.Store.LatestCommittedStageLifecycle(attempt)
	if err != nil {
		return err
	}
	if !found || lifecycleReached(latest.Substate, stagelifecycle.GateResolved) {
		return nil
	}
	runnerRecord, ok := lifecycleRecordResult(records)
	if !ok {
		return &stagelifecycle.DiagnosticError{Code: stagelifecycle.CodeMissingResult, Message: "artifact review result checkpoint is missing"}
	}
	result := e.lifecycleResultFromRecord(runnerRecord)
	archiveRefs := attemptArtifactRefs(latest.Artifacts)
	return e.commitLifecycleSubstate(attempt, stagelifecycle.GateResolved,
		lifecyclePayloadDigest(stagelifecycle.GateResolved, result, archiveRefs), result, archiveRefs)
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
		latestAttemptID := ""
		for _, record := range records {
			if record.Committed && lifecycleReached(record.Substate, stagelifecycle.ArtifactsArchived) &&
				(latestAttemptID == "" || lifecycleAttemptAfter(record.AttemptID, latestAttemptID)) {
				latestAttemptID = record.AttemptID
			}
		}
		if latestAttemptID == "" {
			continue
		}
		var latest stagelifecycle.Record
		found := false
		for _, record := range records {
			if record.AttemptID == latestAttemptID && record.Committed &&
				lifecycleReached(record.Substate, stagelifecycle.ArtifactsArchived) &&
				(!found || record.Version > latest.Version) {
				latest, found = record, true
			}
		}
		if !found {
			continue
		}
		for _, artifact := range latest.Artifacts {
			if needed[artifact.Name] && !strings.Contains(artifact.Path, "/result/") {
				refsByName[artifact.Name] = contextpack.AttemptArtifact{
					Name: artifact.Name, Path: artifact.Path, SHA256: artifact.SHA256,
				}
			}
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
	committedStages, err := e.cfg.Store.CommittedStageLifecycleStages(row.ID)
	if err != nil {
		return nil, false, err
	}
	rejectGap := func(index int) error {
		prefix := make(map[string]struct{}, index)
		for _, earlier := range configured.Stages[:index] {
			prefix[earlier.Name] = struct{}{}
		}
		for _, committed := range committedStages {
			if _, ok := prefix[committed]; !ok {
				return &stagelifecycle.DiagnosticError{
					Code:    stagelifecycle.CodeInvalidState,
					Message: fmt.Sprintf("stage %q has committed lifecycle state outside the valid prefix ending before %q", committed, configured.Stages[index].Name),
				}
			}
		}
		return nil
	}
	completedThrough := -1
	for index, stage := range configured.Stages {
		records, err := e.cfg.Store.StageLifecycleRecords(row.ID, stage.Name, "")
		if err != nil {
			return nil, false, err
		}
		if len(records) == 0 {
			if err := rejectGap(index); err != nil {
				return nil, true, err
			}
			break
		}
		attemptID := ""
		for _, record := range records {
			if record.Committed && (attemptID == "" || lifecycleAttemptAfter(record.AttemptID, attemptID)) {
				attemptID = record.AttemptID
			}
		}
		if attemptID == "" {
			if err := rejectGap(index); err != nil {
				return nil, true, err
			}
			break
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
		if latest.Substate == "" {
			if err := rejectGap(index); err != nil {
				return nil, true, err
			}
			break
		}
		if lifecycleReached(latest.Substate, stagelifecycle.FinalizationReady) {
			completedThrough = index
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
	if next := completedThrough + 1; completedThrough >= 0 && next < len(configured.Stages) {
		return &issueState{
			id: row.ID, title: row.Title, body: row.Body, flowName: row.Flow,
			matrix: matrixFromStrings(row.Levers), priority: row.Priority,
			dependsOn: append([]string(nil), row.DependsOn...), stageIdx: next,
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
	if _, err := contextpack.LoadAttemptResult(issueDir, contextpack.AttemptResult{
		AttemptID: record.AttemptID, IssueID: record.IssueID, Stage: record.Stage,
		ResultPath: record.ResultRef, ResultSHA256: record.ResultDigest,
	}); err != nil {
		code := stagelifecycle.CodeIntegrity
		if errors.Is(err, os.ErrNotExist) {
			code = stagelifecycle.CodeMissingResult
		}
		return "", &stagelifecycle.DiagnosticError{Code: code, Message: "durable model result cannot be validated"}
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
