package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/weston6142/watchtower/internal/failure"
)

// VerificationAttemptStatus is the durable lifecycle of one verification
// receipt. A receipt is never edited after its attempt is quarantined.
type VerificationAttemptStatus string

const (
	VerificationAttemptCurrent     VerificationAttemptStatus = "current"
	VerificationAttemptPending     VerificationAttemptStatus = "pending"
	VerificationAttemptPassed      VerificationAttemptStatus = "passed"
	VerificationAttemptFailed      VerificationAttemptStatus = "failed"
	VerificationAttemptQuarantined VerificationAttemptStatus = "quarantined"
)

const verificationAttemptSelect = `
	SELECT id,issue_id,stage,parent_id,retry_key,status,reason,receipt_json,created_at,updated_at
	FROM verification_attempts`

// VerificationAttempt is the receipt journal row plus its current lifecycle
// state. ReceiptJSON is immutable once a verification attempt is finished and
// copied at both store boundaries.
type VerificationAttempt struct {
	ID          int64
	IssueID     string
	Stage       string
	ParentID    int64
	RetryKey    string
	Status      VerificationAttemptStatus
	Reason      string
	ReceiptJSON []byte
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// VerificationRetry identifies one explicit operator retry of a stale proof.
// Failure is validated with the same canonical failure contract as all other
// durable failure records.
type VerificationRetry struct {
	IssueID           string
	Stage             string
	ParentID          int64
	RetryKey          string
	Reason            string
	LegacyReceiptJSON []byte
	Failure           failure.RecordInput
}

func (s *Store) FailNextVerificationRetryForTest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNextVerificationRetry = true
}

func (s *Store) FailNextVerificationAttemptFinishForTest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNextVerificationAttemptFinish = true
}

// RecordVerificationAttempt journals a new root verification attempt.
func (s *Store) RecordVerificationAttempt(attempt VerificationAttempt) (VerificationAttempt, error) {
	if err := validateVerificationAttempt(attempt, true); err != nil {
		return VerificationAttempt{}, err
	}
	if attempt.Status == "" {
		attempt.Status = VerificationAttemptCurrent
	}
	if attempt.CreatedAt.IsZero() {
		attempt.CreatedAt = s.now()
	}
	if attempt.UpdatedAt.IsZero() {
		attempt.UpdatedAt = attempt.CreatedAt
	}
	attempt.ReceiptJSON = append([]byte(nil), attempt.ReceiptJSON...)

	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(`
		INSERT INTO verification_attempts(
			issue_id,stage,parent_id,retry_key,status,reason,receipt_json,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?)`,
		attempt.IssueID, attempt.Stage, attempt.ParentID, attempt.RetryKey,
		attempt.Status, attempt.Reason, attempt.ReceiptJSON,
		attempt.CreatedAt.Format(time.RFC3339Nano), attempt.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return VerificationAttempt{}, err
	}
	attempt.ID, err = result.LastInsertId()
	if err != nil {
		return VerificationAttempt{}, err
	}
	return cloneVerificationAttempt(attempt), nil
}

// VerificationAttempts returns all journal rows in durable creation order.
func (s *Store) VerificationAttempts(issueID string) ([]VerificationAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(verificationAttemptSelect+` WHERE issue_id=? ORDER BY id`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var attempts []VerificationAttempt
	for rows.Next() {
		attempt, err := scanVerificationAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if attempts == nil {
		attempts = make([]VerificationAttempt, 0)
	}
	return attempts, nil
}

// CurrentVerificationAttempt returns the latest active attempt. Quarantined
// and failed attempts are historical and cannot become current again.
func (s *Store) CurrentVerificationAttempt(issueID string) (VerificationAttempt, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(verificationAttemptSelect+`
		WHERE issue_id=? AND status IN ('current','pending','passed')
		ORDER BY id DESC LIMIT 1`, issueID)
	attempt, err := scanVerificationAttempt(row)
	if err == sql.ErrNoRows {
		return VerificationAttempt{}, false, nil
	}
	if err != nil {
		return VerificationAttempt{}, false, err
	}
	return attempt, true, nil
}

// BeginVerificationRetry atomically records the stale failure, quarantines
// its parent receipt, creates the idempotent child attempt, and moves the
// integration checkpoint to pending reverification.
func (s *Store) BeginVerificationRetry(ctx context.Context, retry VerificationRetry) (VerificationAttempt, error) {
	if err := validateVerificationRetry(retry); err != nil {
		return VerificationAttempt{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return VerificationAttempt{}, err
	}
	defer tx.Rollback()

	var existing VerificationAttempt
	row := tx.QueryRow(verificationAttemptSelect+` WHERE issue_id=? AND retry_key=?`, retry.IssueID, retry.RetryKey)
	existing, err = scanVerificationAttempt(row)
	if err == nil {
		return existing, nil
	}
	if err != sql.ErrNoRows {
		return VerificationAttempt{}, err
	}

	var parent VerificationAttempt
	now := s.now().Format(time.RFC3339Nano)
	if retry.ParentID > 0 {
		parent, err = scanVerificationAttempt(tx.QueryRow(verificationAttemptSelect+` WHERE id=? AND issue_id=?`, retry.ParentID, retry.IssueID))
		if err != nil {
			if err == sql.ErrNoRows {
				return VerificationAttempt{}, fmt.Errorf("verification parent %d is not current", retry.ParentID)
			}
			return VerificationAttempt{}, err
		}
	} else {
		parent, err = insertLegacyVerificationAttempt(tx, retry, now)
		if err != nil {
			return VerificationAttempt{}, err
		}
	}
	if parent.Status != VerificationAttemptCurrent && parent.Status != VerificationAttemptPassed {
		return VerificationAttempt{}, fmt.Errorf("verification parent %d is not current", parent.ID)
	}
	if _, err := insertFailureTx(ctx, tx, retry.Failure, s.now()); err != nil {
		return VerificationAttempt{}, err
	}
	result, err := tx.Exec(`
		UPDATE verification_attempts SET status=?,reason=?,updated_at=?
		WHERE id=? AND issue_id=? AND status IN ('current','passed')`,
		VerificationAttemptQuarantined, retry.Reason, now, parent.ID, retry.IssueID)
	if err != nil {
		return VerificationAttempt{}, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return VerificationAttempt{}, err
	}
	if updated != 1 {
		return VerificationAttempt{}, fmt.Errorf("verification parent %d changed during retry", parent.ID)
	}

	result, err = tx.Exec(`
		INSERT INTO verification_attempts(
			issue_id,stage,parent_id,retry_key,status,reason,receipt_json,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?)`,
		retry.IssueID, parent.Stage, parent.ID, retry.RetryKey,
		VerificationAttemptPending, retry.Reason, []byte{}, now, now)
	if err != nil {
		return VerificationAttempt{}, err
	}
	childID, err := result.LastInsertId()
	if err != nil {
		return VerificationAttempt{}, err
	}

	result, err = tx.Exec(`
		UPDATE issue_integration SET state=?,last_error=?,updated_at=? WHERE issue_id=?`,
		IntegrationPendingReverification, retry.Reason, now, retry.IssueID)
	if err != nil {
		return VerificationAttempt{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return VerificationAttempt{}, err
	}
	if changed == 0 {
		if _, err := tx.Exec(`
			INSERT INTO issue_integration(
				issue_id,state,last_error,worktree,branch,cleanup,updated_at
			) VALUES(?,?,?,'','',?,?)`,
			retry.IssueID, IntegrationPendingReverification, retry.Reason, "[]", now); err != nil {
			return VerificationAttempt{}, err
		}
	}

	if s.failNextVerificationRetry {
		s.failNextVerificationRetry = false
		return VerificationAttempt{}, fmt.Errorf("injected verification retry transition failure")
	}
	if err := tx.Commit(); err != nil {
		return VerificationAttempt{}, err
	}

	child, err := s.verificationAttemptByIDLocked(childID)
	if err != nil {
		return VerificationAttempt{}, err
	}
	return child, nil
}

// FinishVerificationAttempt records the outcome and fresh receipt of a
// pending child attempt. Failed attempts may have no fresh receipt because a
// gate can fail before receipt creation; passing attempts may not.
func (s *Store) FinishVerificationAttempt(
	issueID string, attemptID int64, status VerificationAttemptStatus,
	receiptJSON []byte, reason string,
) (VerificationAttempt, error) {
	if attemptID <= 0 || strings.TrimSpace(issueID) == "" {
		return VerificationAttempt{}, fmt.Errorf("verification attempt identity is required")
	}
	if status != VerificationAttemptPassed && status != VerificationAttemptFailed {
		return VerificationAttempt{}, fmt.Errorf("invalid finished verification status %q", status)
	}
	if status == VerificationAttemptPassed && len(receiptJSON) == 0 {
		return VerificationAttempt{}, fmt.Errorf("passing verification receipt is required")
	}
	receiptJSON = append([]byte(nil), receiptJSON...)
	if receiptJSON == nil {
		receiptJSON = []byte{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNextVerificationAttemptFinish {
		s.failNextVerificationAttemptFinish = false
		return VerificationAttempt{}, fmt.Errorf("injected verification attempt finish failure")
	}
	now := s.now().Format(time.RFC3339Nano)
	result, err := s.db.Exec(`
		UPDATE verification_attempts SET status=?,reason=?,receipt_json=?,updated_at=?
		WHERE id=? AND issue_id=? AND status='pending'`,
		status, reason, receiptJSON, now, attemptID, issueID)
	if err != nil {
		return VerificationAttempt{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return VerificationAttempt{}, err
	}
	if changed != 1 {
		return VerificationAttempt{}, fmt.Errorf("verification attempt %d is not pending", attemptID)
	}
	return s.verificationAttemptByIDLocked(attemptID)
}

func (s *Store) verificationAttemptByIDLocked(id int64) (VerificationAttempt, error) {
	return scanVerificationAttempt(s.db.QueryRow(verificationAttemptSelect+` WHERE id=?`, id))
}

func validateVerificationAttempt(attempt VerificationAttempt, requireReceipt bool) error {
	if strings.TrimSpace(attempt.IssueID) == "" || strings.TrimSpace(attempt.Stage) == "" {
		return fmt.Errorf("verification attempt issue and stage are required")
	}
	if attempt.ID < 0 || attempt.ParentID < 0 {
		return fmt.Errorf("verification attempt IDs cannot be negative")
	}
	if attempt.Status != "" && !validVerificationAttemptStatus(attempt.Status) {
		return fmt.Errorf("invalid verification attempt status %q", attempt.Status)
	}
	if requireReceipt && len(attempt.ReceiptJSON) == 0 {
		return fmt.Errorf("verification receipt is required")
	}
	return nil
}

func validateVerificationRetry(retry VerificationRetry) error {
	if strings.TrimSpace(retry.IssueID) == "" || retry.ParentID < 0 || strings.TrimSpace(retry.RetryKey) == "" {
		return fmt.Errorf("verification retry identity is required")
	}
	if retry.ParentID == 0 && (strings.TrimSpace(retry.Stage) == "" || len(retry.LegacyReceiptJSON) == 0) {
		return fmt.Errorf("legacy verification receipt and stage are required")
	}
	if retry.ParentID > 0 && len(retry.LegacyReceiptJSON) != 0 {
		return fmt.Errorf("legacy verification receipt is only valid without a parent")
	}
	if strings.TrimSpace(retry.Reason) == "" {
		return fmt.Errorf("verification retry reason is required")
	}
	if retry.Failure.IssueID != retry.IssueID {
		return fmt.Errorf("verification retry failure issue does not match attempt")
	}
	return failure.ValidateRecordInput(retry.Failure)
}

func insertLegacyVerificationAttempt(tx *sql.Tx, retry VerificationRetry, now string) (VerificationAttempt, error) {
	result, err := tx.Exec(`
		INSERT INTO verification_attempts(
			issue_id,stage,parent_id,retry_key,status,reason,receipt_json,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?)`,
		retry.IssueID, retry.Stage, 0, "", VerificationAttemptCurrent,
		"legacy pre-journal verification receipt", retry.LegacyReceiptJSON, now, now)
	if err != nil {
		return VerificationAttempt{}, err
	}
	parentID, err := result.LastInsertId()
	if err != nil {
		return VerificationAttempt{}, err
	}
	return scanVerificationAttempt(tx.QueryRow(verificationAttemptSelect+` WHERE id=?`, parentID))
}

func validVerificationAttemptStatus(status VerificationAttemptStatus) bool {
	switch status {
	case VerificationAttemptCurrent, VerificationAttemptPending,
		VerificationAttemptPassed, VerificationAttemptFailed, VerificationAttemptQuarantined:
		return true
	default:
		return false
	}
}

func cloneVerificationAttempt(attempt VerificationAttempt) VerificationAttempt {
	attempt.ReceiptJSON = append([]byte(nil), attempt.ReceiptJSON...)
	return attempt
}

type verificationAttemptScanner interface {
	Scan(dest ...any) error
}

func scanVerificationAttempt(row verificationAttemptScanner) (VerificationAttempt, error) {
	var attempt VerificationAttempt
	var status, createdAt, updatedAt string
	var receipt []byte
	if err := row.Scan(
		&attempt.ID, &attempt.IssueID, &attempt.Stage, &attempt.ParentID,
		&attempt.RetryKey, &status, &attempt.Reason, &receipt,
		&createdAt, &updatedAt,
	); err != nil {
		return VerificationAttempt{}, err
	}
	attempt.Status = VerificationAttemptStatus(status)
	attempt.ReceiptJSON = append([]byte(nil), receipt...)
	var err error
	attempt.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return VerificationAttempt{}, fmt.Errorf("parse verification attempt created time: %w", err)
	}
	attempt.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return VerificationAttempt{}, fmt.Errorf("parse verification attempt updated time: %w", err)
	}
	return cloneVerificationAttempt(attempt), nil
}

func insertFailureTx(ctx context.Context, tx *sql.Tx, input failure.RecordInput, now time.Time) (failure.FailureRecord, error) {
	if err := failure.ValidateRecordInput(input); err != nil {
		return failure.FailureRecord{}, err
	}
	occurredAt := now.UTC()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO failure_records(
			schema_version,issue_id,stage,stage_attempt,failure_site,failure_class,
			retry_disposition,required_state_change,fingerprint,occurred_at
		) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		failure.SchemaVersion, input.IssueID, input.Stage, input.StageAttempt,
		input.FailureSite, input.FailureClass, input.RetryDisposition,
		input.RequiredStateChange, input.Fingerprint, occurredAt.Format(time.RFC3339Nano))
	if err != nil {
		return failure.FailureRecord{}, err
	}
	recordID, err := result.LastInsertId()
	if err != nil {
		return failure.FailureRecord{}, err
	}
	record := failure.FailureRecord{
		RecordID: recordID, SchemaVersion: failure.SchemaVersion, IssueID: input.IssueID,
		Stage: input.Stage, StageAttempt: input.StageAttempt, FailureSite: input.FailureSite,
		FailureClass: input.FailureClass, RetryDisposition: input.RetryDisposition,
		RequiredStateChange: input.RequiredStateChange, Fingerprint: input.Fingerprint,
		OccurredAt: occurredAt,
	}
	if err := failure.ValidateRecord(record); err != nil {
		return failure.FailureRecord{}, err
	}
	return record, nil
}
