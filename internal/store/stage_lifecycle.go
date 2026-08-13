package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/stagelifecycle"
	"github.com/weston6142/watchtower/internal/stageresult"
)

type StageLifecycleAttempt struct {
	IssueID                  string
	Stage                    string
	AttemptID                string
	LegacyCheckpointID       int64
	ResultPath               string
	ResultSHA256             string
	StageResultSchemaVersion int
	StageResultKind          stageresult.Kind
	StageResultOutcome       stageresult.Outcome
	StageResultStatus        stageresult.ValidationStatus
	PredecessorAttemptID     string
	CapabilitySchemaVersion  int
	CapabilityContractSHA256 string
	CreatedAt                time.Time
}

type Substate = stagelifecycle.Substate
type Record = stagelifecycle.Record
type ArtifactRef = stagelifecycle.ArtifactRef
type DiagnosticError = stagelifecycle.DiagnosticError
type DiagnosticCode = stagelifecycle.DiagnosticCode

const (
	RunnerSucceeded    = stagelifecycle.RunnerSucceeded
	ArtifactsValidated = stagelifecycle.ArtifactsValidated
	ArtifactsArchived  = stagelifecycle.ArtifactsArchived
	GateResolved       = stagelifecycle.GateResolved
	VerificationPassed = stagelifecycle.VerificationPassed
	FinalizationReady  = stagelifecycle.FinalizationReady
	SchemaVersion      = stagelifecycle.SchemaVersion
)

const (
	CodeUnknownVersion         = stagelifecycle.CodeUnknownVersion
	CodeInvalidState           = stagelifecycle.CodeInvalidState
	CodeConflict               = stagelifecycle.CodeConflict
	CodeIntegrity              = stagelifecycle.CodeIntegrity
	CodeMissingResult          = stagelifecycle.CodeMissingResult
	CodeLegacyNormalization    = stagelifecycle.CodeLegacyNormalization
	CodeCheckpointFinalization = stagelifecycle.CodeCheckpointFinalization
)

const (
	stageLifecycleRecordColumns  = "schema_version,issue_id,stage,attempt_id,version,substate,predecessor_version,transition_id,payload_digest,result_path,result_sha256,artifacts,status"
	stageLifecycleAttemptColumns = "issue_id,stage,attempt_id,legacy_checkpoint_id,result_path,result_sha256,stage_result_schema_version,stage_result_kind,stage_result_outcome,stage_result_status,predecessor_attempt_id,capability_schema_version,capability_contract_sha256,created_at"
)

func BeginAttempt(issueID, stage, attemptID string) StageLifecycleAttempt {
	return StageLifecycleAttempt{
		IssueID: issueID, Stage: stage, AttemptID: attemptID,
		LegacyCheckpointID: legacyCheckpointID(attemptID), CapabilitySchemaVersion: 1,
		CreatedAt: time.Now().UTC(),
	}
}

func (s *Store) CreateStageLifecycleAttempt(attempt StageLifecycleAttempt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateAttempt(attempt); err != nil {
		return err
	}
	created := attempt.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	var existing StageLifecycleAttempt
	var createdAt string
	err := s.db.QueryRow("SELECT "+stageLifecycleAttemptColumns+`
		FROM stage_lifecycle_attempts WHERE issue_id=? AND stage=? AND attempt_id=?`,
		attempt.IssueID, attempt.Stage, attempt.AttemptID).Scan(
		&existing.IssueID, &existing.Stage, &existing.AttemptID, &existing.LegacyCheckpointID,
		&existing.ResultPath, &existing.ResultSHA256, &existing.StageResultSchemaVersion,
		&existing.StageResultKind, &existing.StageResultOutcome, &existing.StageResultStatus,
		&existing.PredecessorAttemptID, &existing.CapabilitySchemaVersion,
		&existing.CapabilityContractSHA256, &createdAt)
	if err == nil {
		if existing.LegacyCheckpointID != attempt.LegacyCheckpointID ||
			existing.CapabilitySchemaVersion != attempt.CapabilitySchemaVersion ||
			(attempt.CapabilityContractSHA256 != "" && existing.CapabilityContractSHA256 != attempt.CapabilityContractSHA256) ||
			(attempt.ResultPath != "" && existing.ResultPath != attempt.ResultPath) ||
			(attempt.ResultSHA256 != "" && existing.ResultSHA256 != attempt.ResultSHA256) {
			return lifecycleDiagnostic(CodeConflict, "attempt identity already exists with different data")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO stage_lifecycle_attempts(
		issue_id,stage,attempt_id,legacy_checkpoint_id,result_path,result_sha256,
		stage_result_schema_version,stage_result_kind,stage_result_outcome,stage_result_status,predecessor_attempt_id,
		capability_schema_version,capability_contract_sha256,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, attempt.IssueID, attempt.Stage, attempt.AttemptID,
		attempt.LegacyCheckpointID, attempt.ResultPath, attempt.ResultSHA256,
		attempt.StageResultSchemaVersion, attempt.StageResultKind, attempt.StageResultOutcome,
		attempt.StageResultStatus, attempt.PredecessorAttemptID, attempt.CapabilitySchemaVersion,
		attempt.CapabilityContractSHA256,
		created.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) StageLifecycleAttempts(issueID, stage string) ([]StageLifecycleAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query("SELECT "+stageLifecycleAttemptColumns+` FROM stage_lifecycle_attempts
		WHERE issue_id=? AND stage=? ORDER BY created_at,attempt_id`, issueID, stage)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var attempts []StageLifecycleAttempt
	for rows.Next() {
		attempt, err := scanStageLifecycleAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func (s *Store) StageLifecycleResult(attempt StageLifecycleAttempt) (contextpack.AttemptResult, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateAttempt(attempt); err != nil {
		return contextpack.AttemptResult{}, false, err
	}
	var result contextpack.AttemptResult
	err := s.db.QueryRow(`SELECT result_path,result_sha256 FROM stage_lifecycle_attempts
		WHERE issue_id=? AND stage=? AND attempt_id=?`, attempt.IssueID, attempt.Stage, attempt.AttemptID).
		Scan(&result.ResultPath, &result.ResultSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return contextpack.AttemptResult{}, false, nil
	}
	if err != nil {
		return contextpack.AttemptResult{}, false, err
	}
	if result.ResultPath == "" || result.ResultSHA256 == "" {
		return contextpack.AttemptResult{}, false, nil
	}
	result.AttemptID, result.IssueID, result.Stage = attempt.AttemptID, attempt.IssueID, attempt.Stage
	return result, true, nil
}

// PutStageLifecycleResult records the immutable result slot identity. The
// variadic form keeps this boundary convenient for callers that already hold
// an AttemptResult while retaining one unambiguous storage operation.
func (s *Store) PutStageLifecycleResult(attempt StageLifecycleAttempt, results ...contextpack.AttemptResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putStageLifecycleResultLocked(s.db, attempt, results...)
}

// CommitCapabilityValidatedResult atomically binds successful policy evidence
// to the immutable result slot that restart recovery consumes.
func (s *Store) CommitCapabilityValidatedResult(
	attempt StageLifecycleAttempt,
	identity capability.AttemptIdentity,
	validation capability.ValidationResult,
	result contextpack.AttemptResult,
) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	transaction, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = transaction.Rollback()
		}
	}()
	if err = bindCapabilityValidationLocked(transaction, identity, result.ResultSHA256, validation); err != nil {
		return err
	}
	if err = s.putStageLifecycleResultLocked(transaction, attempt, result); err != nil {
		return err
	}
	err = transaction.Commit()
	return err
}

func (s *Store) putStageLifecycleResultLocked(database capabilitySQL, attempt StageLifecycleAttempt, results ...contextpack.AttemptResult) error {
	if err := validateAttempt(attempt); err != nil {
		return err
	}
	if len(results) != 1 {
		return lifecycleDiagnostic(CodeMissingResult, "exactly one attempt result is required")
	}
	result := results[0]
	expectedResultPath := path.Join("artifacts", "attempts", attempt.AttemptID, "result", "manifest.json")
	if result.AttemptID != attempt.AttemptID || result.ResultPath != expectedResultPath {
		return lifecycleDiagnostic(CodeIntegrity, "attempt result identity or path conflicts")
	}
	if strings.TrimSpace(result.ResultPath) == "" || !validDigest(result.ResultSHA256) {
		return lifecycleDiagnostic(CodeMissingResult, "attempt result identity or digest is missing")
	}
	if err := requireAttempt(database, attempt); err != nil {
		return err
	}
	if err := requireCapabilityValidation(database, attempt, result.ResultSHA256); err != nil {
		return err
	}
	summary := StageLifecycleAttempt{}
	if result.StageResult != nil {
		validated, err := stageresult.ValidatePersisted(*result.StageResult)
		if err != nil {
			return lifecycleDiagnostic(CodeIntegrity, "structured stage result is invalid: %v", err)
		}
		if result.IssueID != attempt.IssueID || result.Stage != attempt.Stage ||
			validated.IssueID != attempt.IssueID || validated.AttemptID != attempt.AttemptID {
			return lifecycleDiagnostic(CodeIntegrity, "structured stage result identity conflicts")
		}
		summary.StageResultSchemaVersion = validated.SchemaVersion
		summary.StageResultKind = validated.StageKind
		summary.StageResultOutcome = validated.Outcome
		summary.StageResultStatus = validated.ValidationStatus
		summary.PredecessorAttemptID = validated.PredecessorAttemptID
	}
	var existing StageLifecycleAttempt
	err := database.QueryRow(`SELECT result_path,result_sha256,stage_result_schema_version,stage_result_kind,
		stage_result_outcome,stage_result_status,predecessor_attempt_id FROM stage_lifecycle_attempts
		WHERE issue_id=? AND stage=? AND attempt_id=?`, attempt.IssueID, attempt.Stage, attempt.AttemptID).
		Scan(&existing.ResultPath, &existing.ResultSHA256, &existing.StageResultSchemaVersion,
			&existing.StageResultKind, &existing.StageResultOutcome, &existing.StageResultStatus,
			&existing.PredecessorAttemptID)
	if err != nil {
		return err
	}
	if existing.ResultPath != "" || existing.ResultSHA256 != "" {
		if existing.ResultPath == result.ResultPath && existing.ResultSHA256 == result.ResultSHA256 &&
			existing.StageResultSchemaVersion == summary.StageResultSchemaVersion &&
			existing.StageResultKind == summary.StageResultKind &&
			existing.StageResultOutcome == summary.StageResultOutcome &&
			existing.StageResultStatus == summary.StageResultStatus &&
			existing.PredecessorAttemptID == summary.PredecessorAttemptID {
			return nil
		}
		return lifecycleDiagnostic(CodeConflict, "attempt result identity already has different data")
	}
	if summary.StageResultStatus == stageresult.ValidationValid {
		var latestID string
		err = database.QueryRow(`SELECT attempt_id FROM stage_lifecycle_attempts
			WHERE issue_id=? AND stage=? AND attempt_id<>? AND stage_result_schema_version=?
			AND stage_result_kind=? AND stage_result_status=?
			ORDER BY created_at DESC,attempt_id DESC LIMIT 1`, attempt.IssueID, attempt.Stage,
			attempt.AttemptID, stageresult.SchemaVersion, summary.StageResultKind, stageresult.ValidationValid).Scan(&latestID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if latestID == "" && summary.PredecessorAttemptID != "" {
			return lifecycleDiagnostic(CodeConflict, "initial structured result must not name a predecessor")
		}
		if latestID != "" && summary.PredecessorAttemptID != latestID {
			return lifecycleDiagnostic(CodeConflict, "structured result predecessor %q is not newest valid attempt %q", summary.PredecessorAttemptID, latestID)
		}
	}
	if s.failNextStageResultPut {
		s.failNextStageResultPut = false
		return lifecycleDiagnostic(CodeIntegrity, "structured stage result persistence failed")
	}
	updated, err := database.Exec(`UPDATE stage_lifecycle_attempts SET result_path=?,result_sha256=?,
		stage_result_schema_version=?,stage_result_kind=?,stage_result_outcome=?,stage_result_status=?,predecessor_attempt_id=?
		WHERE issue_id=? AND stage=? AND attempt_id=?`, result.ResultPath, result.ResultSHA256,
		summary.StageResultSchemaVersion, summary.StageResultKind, summary.StageResultOutcome,
		summary.StageResultStatus, summary.PredecessorAttemptID,
		attempt.IssueID, attempt.Stage, attempt.AttemptID)
	if err != nil {
		return err
	}
	if affected, err := updated.RowsAffected(); err != nil {
		return err
	} else if affected != 1 {
		return lifecycleDiagnostic(CodeMissingResult, "attempt result slot was not persisted")
	}
	return nil
}

func (s *Store) StageResultAttempts(issueID, stage string, kind stageresult.Kind) ([]StageLifecycleAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query("SELECT "+stageLifecycleAttemptColumns+` FROM stage_lifecycle_attempts WHERE issue_id=? AND stage=? AND stage_result_schema_version=?
		AND stage_result_kind=? AND stage_result_status=? ORDER BY created_at,attempt_id`,
		issueID, stage, stageresult.SchemaVersion, kind, stageresult.ValidationValid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var attempts []StageLifecycleAttempt
	for rows.Next() {
		attempt, err := scanStageLifecycleAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func (s *Store) LatestValidStageResultAttempt(issueID, stage string, kind stageresult.Kind) (StageLifecycleAttempt, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow("SELECT "+stageLifecycleAttemptColumns+` FROM stage_lifecycle_attempts WHERE issue_id=? AND stage=? AND stage_result_schema_version=?
		AND stage_result_kind=? AND stage_result_status=? ORDER BY created_at DESC,attempt_id DESC LIMIT 1`,
		issueID, stage, stageresult.SchemaVersion, kind, stageresult.ValidationValid)
	attempt, err := scanStageLifecycleAttempt(row)
	if errors.Is(err, sql.ErrNoRows) {
		return StageLifecycleAttempt{}, false, nil
	}
	return attempt, err == nil, err
}

type rowScanner interface {
	Scan(...any) error
}

func scanStageLifecycleAttempt(row rowScanner) (StageLifecycleAttempt, error) {
	var attempt StageLifecycleAttempt
	var createdAt string
	if err := row.Scan(&attempt.IssueID, &attempt.Stage, &attempt.AttemptID, &attempt.LegacyCheckpointID,
		&attempt.ResultPath, &attempt.ResultSHA256, &attempt.StageResultSchemaVersion,
		&attempt.StageResultKind, &attempt.StageResultOutcome, &attempt.StageResultStatus,
		&attempt.PredecessorAttemptID, &attempt.CapabilitySchemaVersion,
		&attempt.CapabilityContractSHA256, &createdAt); err != nil {
		return StageLifecycleAttempt{}, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return StageLifecycleAttempt{}, err
	}
	attempt.CreatedAt = parsed
	return attempt, nil
}

func (s *Store) FailNextStageResultPutForTest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNextStageResultPut = true
}

// PutStageArchiveManifest records the immutable archive refs after the
// filesystem publication step. It accepts an exact replay and rejects any
// replacement under the same transition identity.
func (s *Store) PutStageArchiveManifest(attempt StageLifecycleAttempt, transitionID string,
	refs ...[]contextpack.AttemptArtifact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateAttempt(attempt); err != nil {
		return err
	}
	if strings.TrimSpace(transitionID) == "" || len(refs) != 1 {
		return lifecycleDiagnostic(CodeIntegrity, "archive manifest identity or refs are missing")
	}
	if s.failNextStageArchiveManifest {
		s.failNextStageArchiveManifest = false
		return lifecycleDiagnostic(CodeIntegrity, "archive manifest publication failed")
	}
	if err := s.requireAttemptLocked(attempt); err != nil {
		return err
	}
	for _, ref := range refs[0] {
		if !validArchiveRef(attempt.AttemptID, ref) {
			return lifecycleDiagnostic(CodeIntegrity, "archive reference for %q is invalid", ref.Name)
		}
		var path, digest string
		err := s.db.QueryRow(`SELECT path,sha256 FROM stage_attempt_archives
			WHERE issue_id=? AND stage=? AND attempt_id=? AND transition_id=? AND name=?`,
			attempt.IssueID, attempt.Stage, attempt.AttemptID, transitionID, ref.Name).Scan(&path, &digest)
		if err == nil {
			if path != ref.Path || digest != ref.SHA256 {
				return lifecycleDiagnostic(CodeConflict, "archive replay for %q conflicts", ref.Name)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := s.db.Exec(`INSERT INTO stage_attempt_archives(
			issue_id,stage,attempt_id,transition_id,name,path,sha256,created_at)
			VALUES(?,?,?,?,?,?,?,?)`, attempt.IssueID, attempt.Stage, attempt.AttemptID,
			transitionID, ref.Name, ref.Path, ref.SHA256, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) PrepareStageLifecycle(record stagelifecycle.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := stagelifecycle.ValidateRecord(record); err != nil {
		return err
	}
	attempt := StageLifecycleAttempt{IssueID: record.IssueID, Stage: record.Stage, AttemptID: record.AttemptID}
	if err := validateAttempt(attempt); err != nil {
		return err
	}
	if err := s.requireAttemptLocked(attempt); err != nil {
		return err
	}
	var existing Record
	var status string
	found, err := s.loadByTransitionLocked(record, &existing, &status)
	if err != nil {
		return err
	}
	if found {
		if stagelifecycle.SameTransition(existing, record) {
			return nil
		}
		return lifecycleDiagnostic(CodeConflict, "transition identity already has conflicting data")
	}
	if err := s.validatePredecessorLocked(record); err != nil {
		return err
	}
	if s.failNextStageLifecyclePrepare {
		s.failNextStageLifecyclePrepare = false
		return lifecycleDiagnostic(CodeCheckpointFinalization, "checkpoint preparation failed")
	}
	encoded, err := json.Marshal(record.Artifacts)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO stage_lifecycle_checkpoints(
		issue_id,stage,attempt_id,schema_version,version,substate,predecessor_version,
		transition_id,payload_digest,result_path,result_sha256,artifacts,status,failure,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.IssueID, record.Stage, record.AttemptID,
		record.SchemaVersion, record.Version, record.Substate, record.PredecessorVersion,
		record.TransitionID, record.PayloadDigest, record.ResultRef, record.ResultDigest,
		string(encoded), "prepared", "", time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) CommitPreparedStageLifecycle(record stagelifecycle.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := stagelifecycle.ValidateRecord(record); err != nil {
		return err
	}
	attempt := StageLifecycleAttempt{IssueID: record.IssueID, Stage: record.Stage, AttemptID: record.AttemptID}
	if err := validateAttempt(attempt); err != nil {
		return err
	}
	var existing Record
	var status string
	found, err := s.loadByTransitionLocked(record, &existing, &status)
	if err != nil {
		return err
	}
	if !found {
		return lifecycleDiagnostic(CodeInvalidState, "prepared lifecycle transition is missing")
	}
	if !stagelifecycle.SameTransition(existing, record) {
		return lifecycleDiagnostic(CodeConflict, "prepared lifecycle transition conflicts")
	}
	if status == "committed" {
		return nil
	}
	if status != "prepared" {
		return lifecycleDiagnostic(CodeInvalidState, "lifecycle transition is not prepared")
	}
	if err := s.validatePredecessorLocked(record); err != nil {
		return err
	}
	if err := s.validateResultLocked(attempt, record); err != nil {
		return err
	}
	if record.Substate == stagelifecycle.RunnerSucceeded {
		if err := s.requireCapabilityValidationLocked(attempt, record.ResultDigest); err != nil {
			return err
		}
	}
	if err := s.validateArchiveLocked(attempt, record); err != nil {
		return err
	}
	if s.failNextStageLifecycleCommit {
		s.failNextStageLifecycleCommit = false
		return lifecycleDiagnostic(CodeCheckpointFinalization, "checkpoint finalization failed")
	}
	result, err := s.db.Exec(`UPDATE stage_lifecycle_checkpoints SET status='committed',failure=''
		WHERE issue_id=? AND stage=? AND attempt_id=? AND version=? AND status='prepared'`,
		record.IssueID, record.Stage, record.AttemptID, record.Version)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected != 1 {
		return lifecycleDiagnostic(CodeCheckpointFinalization, "checkpoint finalization did not publish a row")
	}
	return nil
}

func (s *Store) LatestCommittedStageLifecycle(attempt StageLifecycleAttempt) (stagelifecycle.Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateAttempt(attempt); err != nil {
		return stagelifecycle.Record{}, false, err
	}
	return s.latestCommittedLocked(attempt)
}

func (s *Store) StageLifecycleRecords(issueID, stage, attemptID string) ([]stagelifecycle.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	query := `SELECT ` + stageLifecycleRecordColumns + `
		FROM stage_lifecycle_checkpoints WHERE issue_id=? AND stage=?`
	args := []any{issueID, stage}
	if attemptID != "" {
		query += ` AND attempt_id=?`
		args = append(args, attemptID)
	}
	query += ` ORDER BY attempt_id,version`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stagelifecycle.Record
	for rows.Next() {
		record, status, err := scanLifecycleRecord(rows)
		if err != nil {
			return nil, err
		}
		record.Committed = status == "committed"
		out = append(out, record)
	}
	return out, rows.Err()
}

// LegacyStageLifecycle exposes the old checkpoint shape through a read-only
// normalized view. Its empty v1 references intentionally remain non-authority;
// callers must use the legacy artifact reader until a v1 attempt is committed.
func (s *Store) LegacyStageLifecycle(issueID, stage string) (stagelifecycle.Record, bool, error) {
	checkpoints, err := s.StageCheckpoints(issueID)
	if err != nil {
		return stagelifecycle.Record{}, false, err
	}
	for index := len(checkpoints) - 1; index >= 0; index-- {
		checkpoint := checkpoints[index]
		if checkpoint.Stage != stage || (checkpoint.Status != "succeeded" && checkpoint.Status != "handoff_authorized") {
			continue
		}
		artifacts := make([]stagelifecycle.ArtifactRef, 0, len(checkpoint.Artifacts))
		for _, artifact := range checkpoint.Artifacts {
			artifacts = append(artifacts, stagelifecycle.ArtifactRef{
				Name: artifact.Name, Path: "artifacts/" + artifact.Name, SHA256: artifact.SHA256,
			})
		}
		return stagelifecycle.Record{
			IssueID: issueID, Stage: stage,
			TransitionID: "legacy:" + strconv.FormatInt(checkpoint.ID, 10),
			Artifacts:    artifacts, Committed: true,
		}, true, nil
	}
	return stagelifecycle.Record{}, false, nil
}

func (s *Store) FailNextStageLifecycleCommitForTest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNextStageLifecycleCommit = true
}

func (s *Store) FailNextStageLifecyclePrepareForTest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNextStageLifecyclePrepare = true
}

func (s *Store) FailNextStageArchiveManifestForTest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNextStageArchiveManifest = true
}

func (s *Store) FailNextStageCheckpointFinishForTest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNextStageCheckpointFinish = true
}

func (s *Store) requireAttemptLocked(attempt StageLifecycleAttempt) error {
	return requireAttempt(s.db, attempt)
}

func requireAttempt(database capabilitySQL, attempt StageLifecycleAttempt) error {
	var issue, stage, id string
	err := database.QueryRow(`SELECT issue_id,stage,attempt_id FROM stage_lifecycle_attempts
		WHERE issue_id=? AND stage=? AND attempt_id=?`, attempt.IssueID, attempt.Stage, attempt.AttemptID).
		Scan(&issue, &stage, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return lifecycleDiagnostic(CodeInvalidState, "lifecycle attempt is missing")
	}
	return err
}

func (s *Store) validatePredecessorLocked(record stagelifecycle.Record) error {
	var predecessor Record
	var status string
	var artifacts string
	err := s.db.QueryRow(`SELECT `+stageLifecycleRecordColumns+`
		FROM stage_lifecycle_checkpoints WHERE issue_id=? AND stage=? AND attempt_id=? AND version=?`,
		record.IssueID, record.Stage, record.AttemptID, record.PredecessorVersion).Scan(
		&predecessor.SchemaVersion, &predecessor.IssueID, &predecessor.Stage, &predecessor.AttemptID,
		&predecessor.Version, &predecessor.Substate, &predecessor.PredecessorVersion,
		&predecessor.TransitionID, &predecessor.PayloadDigest, &predecessor.ResultRef,
		&predecessor.ResultDigest, &artifacts, &status)
	if record.PredecessorVersion == 0 {
		if !errors.Is(err, sql.ErrNoRows) {
			if err == nil {
				return lifecycleDiagnostic(CodeInvalidState, "first lifecycle checkpoint has a predecessor")
			}
			return err
		}
		return stagelifecycle.ValidateTransition(nil, record)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return lifecycleDiagnostic(CodeInvalidState, "lifecycle predecessor is not committed")
	}
	if err != nil {
		return err
	}
	if status != "committed" {
		return lifecycleDiagnostic(CodeInvalidState, "lifecycle predecessor is not committed")
	}
	if artifacts == "" {
		artifacts = "[]"
	}
	if err := json.Unmarshal([]byte(artifacts), &predecessor.Artifacts); err != nil {
		return lifecycleDiagnostic(CodeIntegrity, "lifecycle predecessor artifacts are invalid")
	}
	return stagelifecycle.ValidateTransition(&predecessor, record)
}

func (s *Store) validateResultLocked(attempt StageLifecycleAttempt, record stagelifecycle.Record) error {
	if strings.TrimSpace(record.ResultRef) == "" {
		return lifecycleDiagnostic(CodeMissingResult, "lifecycle checkpoint has no result reference")
	}
	var path, digest string
	err := s.db.QueryRow(`SELECT result_path,result_sha256 FROM stage_lifecycle_attempts
		WHERE issue_id=? AND stage=? AND attempt_id=?`, attempt.IssueID, attempt.Stage, attempt.AttemptID).
		Scan(&path, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return lifecycleDiagnostic(CodeMissingResult, "lifecycle attempt result is not durable")
	}
	if err != nil {
		return err
	}
	if path == "" || digest == "" {
		return lifecycleDiagnostic(CodeMissingResult, "lifecycle attempt result is incomplete")
	}
	if path != record.ResultRef || digest != record.ResultDigest {
		return lifecycleDiagnostic(CodeConflict, "lifecycle result reference conflicts with attempt result")
	}
	if record.ResultDigest != "" && !validDigest(record.ResultDigest) {
		return lifecycleDiagnostic(CodeIntegrity, "lifecycle result digest is invalid")
	}
	return nil
}

func (s *Store) validateArchiveLocked(attempt StageLifecycleAttempt, record stagelifecycle.Record) error {
	if !requiresArchiveValidation(record.Substate) || len(record.Artifacts) == 0 {
		return nil
	}
	for _, artifact := range record.Artifacts {
		var path, digest string
		err := s.db.QueryRow(`SELECT path,sha256 FROM stage_attempt_archives
			WHERE issue_id=? AND stage=? AND attempt_id=? AND transition_id=? AND name=?`,
			attempt.IssueID, attempt.Stage, attempt.AttemptID, record.TransitionID, artifact.Name).Scan(&path, &digest)
		if errors.Is(err, sql.ErrNoRows) {
			err = s.db.QueryRow(`SELECT path,sha256 FROM stage_attempt_archives
				WHERE issue_id=? AND stage=? AND attempt_id=? AND name=?
				ORDER BY created_at DESC LIMIT 1`, attempt.IssueID, attempt.Stage,
				attempt.AttemptID, artifact.Name).Scan(&path, &digest)
			if errors.Is(err, sql.ErrNoRows) {
				return lifecycleDiagnostic(CodeIntegrity, "archive reference %q is not committed", artifact.Name)
			}
		}
		if err != nil {
			return err
		}
		if path != artifact.Path || digest != artifact.SHA256 {
			return lifecycleDiagnostic(CodeIntegrity, "archive reference %q does not match manifest", artifact.Name)
		}
	}
	return nil
}

func requiresArchiveValidation(substate stagelifecycle.Substate) bool {
	switch substate {
	case stagelifecycle.ArtifactsArchived, stagelifecycle.GateResolved,
		stagelifecycle.VerificationPassed, stagelifecycle.FinalizationReady:
		return true
	default:
		return false
	}
}

func (s *Store) loadByTransitionLocked(want stagelifecycle.Record, out *stagelifecycle.Record, status *string) (bool, error) {
	row := s.db.QueryRow(`SELECT `+stageLifecycleRecordColumns+`
		FROM stage_lifecycle_checkpoints WHERE issue_id=? AND stage=? AND attempt_id=? AND transition_id=?`,
		want.IssueID, want.Stage, want.AttemptID, want.TransitionID)
	record, currentStatus, err := scanLifecycleRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	*out = record
	*status = currentStatus
	return true, nil
}

func (s *Store) latestCommittedLocked(attempt StageLifecycleAttempt) (stagelifecycle.Record, bool, error) {
	row := s.db.QueryRow(`SELECT `+stageLifecycleRecordColumns+` FROM stage_lifecycle_checkpoints WHERE issue_id=? AND stage=? AND attempt_id=? AND status='committed'
		ORDER BY version DESC LIMIT 1`, attempt.IssueID, attempt.Stage, attempt.AttemptID)
	record, status, err := scanLifecycleRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return stagelifecycle.Record{}, false, nil
	}
	if err != nil {
		return stagelifecycle.Record{}, false, err
	}
	record.Committed = status == "committed"
	return record, true, nil
}

type lifecycleScanner interface {
	Scan(dest ...any) error
}

func scanLifecycleRecord(row lifecycleScanner) (stagelifecycle.Record, string, error) {
	var record stagelifecycle.Record
	var artifacts, status string
	err := row.Scan(&record.SchemaVersion, &record.IssueID, &record.Stage, &record.AttemptID,
		&record.Version, &record.Substate, &record.PredecessorVersion, &record.TransitionID,
		&record.PayloadDigest, &record.ResultRef, &record.ResultDigest, &artifacts, &status)
	if err != nil {
		return stagelifecycle.Record{}, "", err
	}
	if artifacts == "" {
		artifacts = "[]"
	}
	if err := json.Unmarshal([]byte(artifacts), &record.Artifacts); err != nil {
		return stagelifecycle.Record{}, "", err
	}
	return record, status, nil
}

func validateAttempt(attempt StageLifecycleAttempt) error {
	if strings.TrimSpace(attempt.IssueID) == "" || strings.TrimSpace(attempt.Stage) == "" ||
		!safeComponent(attempt.AttemptID) {
		return lifecycleDiagnostic(CodeInvalidState, "lifecycle attempt identity is invalid")
	}
	return nil
}

func validArchiveRef(attemptID string, ref contextpack.AttemptArtifact) bool {
	cleanName := path.Clean(ref.Name)
	if ref.Name == "" || strings.ContainsAny(ref.Name, `\\`) || strings.Contains(ref.Name, "\x00") ||
		cleanName != ref.Name || cleanName == "." || strings.HasPrefix(cleanName, "../") {
		return false
	}
	return ref.Name != "" && safeRelativePath(ref.Path) && validDigest(ref.SHA256) &&
		ref.Path == path.Join("artifacts", "attempts", attemptID, ref.Name)
}

func lifecycleDiagnostic(code stagelifecycle.DiagnosticCode, format string, args ...any) error {
	return &stagelifecycle.DiagnosticError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func legacyCheckpointID(attemptID string) int64 {
	const prefix = "checkpoint-"
	if !strings.HasPrefix(attemptID, prefix) {
		return 0
	}
	id, _ := strconv.ParseInt(strings.TrimPrefix(attemptID, prefix), 10, 64)
	return id
}

func safeComponent(value string) bool {
	return value != "" && value != "." && value != ".." &&
		!strings.ContainsAny(value, `/\\`) && !strings.Contains(value, "\x00")
}

func safeRelativePath(value string) bool {
	if value == "" || strings.ContainsAny(value, `\\`) || strings.HasPrefix(value, "/") {
		return false
	}
	clean := strings.TrimPrefix(value, "./")
	return clean == value && clean != ".." && !strings.HasPrefix(clean, "../")
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}
