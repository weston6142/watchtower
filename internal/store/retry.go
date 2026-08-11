package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/retry"
)

const retryContextSelect = `
	SELECT context_key,schema_version,latest_record_id,issue_id,stage,failure_site,failure_class,
	       retry_disposition,failure_fingerprint,state_vector,shared_used,shared_cap,
	       model_resample_used,model_resample_cap,policy_evidence,lifecycle,version,updated_at
	FROM retry_contexts`

func scanRetryContext(row *sql.Row) (retry.Context, error) {
	var stored retry.Context
	var site, class, disposition, stateJSON, policyJSON string
	if err := row.Scan(
		&stored.ContextKey, &stored.SchemaVersion, &stored.LatestRecordID,
		&stored.IssueID, &stored.Stage, &site, &class, &disposition,
		&stored.FailureFingerprint, &stateJSON, &stored.SharedUsed, &stored.SharedCap,
		&stored.ModelResampleUsed, &stored.ModelResampleCap, &policyJSON,
		&stored.Lifecycle, &stored.Version, &stored.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return retry.Context{}, retry.ErrContextNotFound
		}
		return retry.Context{}, err
	}
	stored.FailureSite = failure.Site(site)
	stored.FailureClass = failure.Class(class)
	stored.RetryDisposition = failure.RetryDisposition(disposition)
	if err := json.Unmarshal([]byte(stateJSON), &stored.State); err != nil {
		return retry.Context{}, fmt.Errorf("decode retry state vector: %w", err)
	}
	if err := json.Unmarshal([]byte(policyJSON), &stored.Policy); err != nil {
		return retry.Context{}, fmt.Errorf("decode retry policy evidence: %w", err)
	}
	if err := validateRetryContext(stored); err != nil {
		return retry.Context{}, err
	}
	if stored.Lifecycle == retry.ContextUnavailable {
		return retry.Context{}, retry.ErrContextUnavailable
	}
	return stored, nil
}

func validateRetryContext(stored retry.Context) error {
	if stored.ContextKey == "" || stored.SchemaVersion != retry.ContextSchemaVersion ||
		stored.LatestRecordID <= 0 || stored.IssueID == "" || stored.Stage == "" || stored.Version <= 0 {
		return fmt.Errorf("invalid retry context identity")
	}
	if expected := retry.BuildContextKey(
		stored.IssueID, stored.Stage, stored.FailureSite, stored.FailureClass, stored.FailureFingerprint,
	); stored.ContextKey != expected {
		return fmt.Errorf("retry context key does not match durable identity")
	}
	if stored.SharedUsed < 0 || stored.SharedCap < 0 || stored.ModelResampleUsed < 0 || stored.ModelResampleCap < 0 {
		return fmt.Errorf("invalid retry context counters")
	}
	if err := failure.ValidateRecordInput(failure.RecordInput{
		IssueID: stored.IssueID, Stage: stored.Stage, FailureSite: stored.FailureSite,
		FailureClass: stored.FailureClass, RetryDisposition: stored.RetryDisposition,
		RequiredStateChange: failure.StateNone, Fingerprint: stored.FailureFingerprint,
		StateVector: &stored.State,
	}); err != nil {
		return err
	}
	switch stored.Lifecycle {
	case retry.ContextActive, retry.ContextClosed, retry.ContextUnavailable:
	default:
		return fmt.Errorf("invalid retry context lifecycle %q", stored.Lifecycle)
	}
	if _, err := time.Parse(time.RFC3339Nano, stored.UpdatedAt); err != nil {
		return fmt.Errorf("invalid retry context timestamp: %w", err)
	}
	return nil
}

func (s *Store) LoadRetryContext(ctx context.Context, issueID, stage string) (retry.Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return retry.Context{}, err
	}
	return scanRetryContext(s.db.QueryRowContext(ctx, retryContextSelect+
		` WHERE issue_id=? AND stage=? ORDER BY latest_record_id DESC LIMIT 1`, issueID, stage))
}

func (s *Store) AuthorizeRetry(ctx context.Context, update retry.AuthorizationUpdate) (retry.Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNextRetryAuthorizations > 0 {
		s.failNextRetryAuthorizations--
		return retry.Context{}, retry.ErrAuthorizationConflict
	}
	if err := ctx.Err(); err != nil {
		return retry.Context{}, err
	}
	if update.ContextKey == "" || update.ExpectedVersion <= 0 || update.SharedCap <= 0 || update.ModelResampleCap <= 0 {
		return retry.Context{}, fmt.Errorf("invalid retry authorization update")
	}
	if err := failure.ValidateStateVector(update.Current); err != nil {
		return retry.Context{}, err
	}
	stateJSON, err := json.Marshal(update.Current)
	if err != nil {
		return retry.Context{}, err
	}
	policyJSON, err := json.Marshal(update.Policy)
	if err != nil {
		return retry.Context{}, err
	}
	modelIncrement := 0
	modelRequired := 0
	if update.Kind == retry.KindModelResample {
		modelIncrement = 1
		modelRequired = 1
	}
	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return retry.Context{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		UPDATE retry_contexts SET
		  state_vector=?, shared_used=shared_used+1, shared_cap=?,
		  model_resample_used=model_resample_used+?, model_resample_cap=?,
		  policy_evidence=?, version=version+1, updated_at=?
		WHERE context_key=? AND version=? AND lifecycle=?
		  AND shared_used < ?
		  AND (?=0 OR model_resample_used < ?)`,
		string(stateJSON), update.SharedCap, modelIncrement, update.ModelResampleCap,
		string(policyJSON), updatedAt, update.ContextKey, update.ExpectedVersion,
		retry.ContextActive, update.SharedCap, modelRequired, update.ModelResampleCap)
	if err != nil {
		return retry.Context{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return retry.Context{}, err
	}
	if rows != 1 {
		return retry.Context{}, retry.ErrAuthorizationConflict
	}
	stored, err := scanRetryContext(tx.QueryRowContext(ctx, retryContextSelect+` WHERE context_key=?`, update.ContextKey))
	if err != nil {
		return retry.Context{}, err
	}
	if err := tx.Commit(); err != nil {
		return retry.Context{}, err
	}
	return stored, nil
}

func (s *Store) CloseRetryContext(ctx context.Context, issueID, stage string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `
		UPDATE retry_contexts SET lifecycle=?, version=version+1, updated_at=?
		WHERE context_key=(
		  SELECT context_key FROM retry_contexts
		  WHERE issue_id=? AND stage=? ORDER BY latest_record_id DESC LIMIT 1
		) AND lifecycle=?`, retry.ContextClosed, updatedAt, issueID, stage, retry.ContextActive)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return retry.ErrContextNotFound
	}
	return nil
}

func (s *Store) FailNextRetryAuthorizationsForTest(count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if count < 0 {
		count = 0
	}
	s.failNextRetryAuthorizations = count
}
