package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/weston6142/watchtower/internal/plannerartifact"
)

// LoadPlannerArtifact reads the exact authority tuple. The boolean is false
// when no record exists; it is never a wildcard match on issue or worktree.
func (s *Store) LoadPlannerArtifact(issueID, stage string, attempt int, worktree string) (status string, digest, manifest, sections []byte, found bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNextPlannerArtifactRead {
		s.failNextPlannerArtifactRead = false
		return "", nil, nil, nil, false, fmt.Errorf("injected planner artifact registry read failure")
	}
	err = s.db.QueryRow(`
		SELECT status, capability_digest, manifest, sections
		FROM planner_artifacts
		WHERE issue_id=? AND stage=? AND attempt=? AND worktree=?`,
		issueID, stage, attempt, worktree).Scan(&status, &digest, &manifest, &sections)
	if err == sql.ErrNoRows {
		return "", nil, nil, nil, false, nil
	}
	if err != nil {
		return "", nil, nil, nil, false, err
	}
	return status, append([]byte(nil), digest...), []byte(manifest), []byte(sections), true, nil
}

// ActivePlannerArtifactBindings returns the exact active scopes for one
// canonical worktree. It is used only during daemon recovery; callers still
// validate the durable record before issuing a new capability.
func (s *Store) ActivePlannerArtifactBindings(worktree string) ([]plannerartifact.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`
		SELECT issue_id, stage, attempt, worktree
		FROM planner_artifacts
		WHERE status=? AND worktree=?`, "active", worktree)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bindings []plannerartifact.Binding
	for rows.Next() {
		var binding plannerartifact.Binding
		if err := rows.Scan(&binding.IssueID, &binding.Stage, &binding.Attempt, &binding.Worktree); err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return bindings, nil
}

// LoadLatestPlannerArtifactBefore returns the newest active planner record in
// one exact issue/stage/worktree scope before the given attempt. A retry uses
// this durable prefix without trusting client or worktree content.
func (s *Store) LoadLatestPlannerArtifactBefore(issueID, stage string, beforeAttempt int, worktree string) (attempt int, status string, digest, manifest, sections []byte, found bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.db.QueryRow(`
		SELECT attempt, status, capability_digest, manifest, sections
		FROM planner_artifacts
		WHERE issue_id=? AND stage=? AND worktree=? AND status=? AND attempt < ?
		ORDER BY attempt DESC LIMIT 1`,
		issueID, stage, worktree, "active", beforeAttempt).Scan(&attempt, &status, &digest, &manifest, &sections)
	if err == sql.ErrNoRows {
		return 0, "", nil, nil, nil, false, nil
	}
	if err != nil {
		return 0, "", nil, nil, nil, false, err
	}
	return attempt, status, append([]byte(nil), digest...), []byte(manifest), []byte(sections), true, nil
}

// CreatePlannerArtifact inserts one exact-tuple authority record.
func (s *Store) CreatePlannerArtifact(issueID, stage string, attempt int, worktree, status string, digest, manifest, sections []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNextPlannerArtifactWrite {
		s.failNextPlannerArtifactWrite = false
		return fmt.Errorf("injected planner artifact registry write failure")
	}
	now := s.now().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
		INSERT INTO planner_artifacts(
			issue_id, stage, attempt, worktree, status, capability_digest,
			manifest, sections, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		issueID, stage, attempt, worktree, status, append([]byte(nil), digest...),
		string(manifest), string(sections), now, now)
	return err
}

// PromotePlannerArtifact atomically retires the selected prior attempt and
// creates the recovered current attempt. This prevents a live prior handle
// from mutating the shared planner pair after cross-attempt recovery.
func (s *Store) PromotePlannerArtifact(issueID, stage string, priorAttempt, attempt int, worktree string, digest, manifest, sections []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNextPlannerArtifactWrite {
		s.failNextPlannerArtifactWrite = false
		return fmt.Errorf("injected planner artifact registry write failure")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().Format(time.RFC3339Nano)
	result, err := tx.Exec(`
		UPDATE planner_artifacts SET status=?, updated_at=?
		WHERE issue_id=? AND stage=? AND attempt=? AND worktree=? AND status=?`,
		"expired", now, issueID, stage, priorAttempt, worktree, "active")
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return sql.ErrNoRows
	}
	if _, err := tx.Exec(`
		INSERT INTO planner_artifacts(
			issue_id, stage, attempt, worktree, status, capability_digest,
			manifest, sections, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		issueID, stage, attempt, worktree, "active", append([]byte(nil), digest...),
		string(manifest), string(sections), now, now); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdatePlannerArtifact replaces only the exact bound record. The caller has
// already verified the capability and binding; this method remains serialized
// with all other coordinator writes by Store.mu.
func (s *Store) UpdatePlannerArtifact(issueID, stage string, attempt int, worktree, status string, digest, manifest, sections []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNextPlannerArtifactWrite {
		s.failNextPlannerArtifactWrite = false
		return fmt.Errorf("injected planner artifact registry write failure")
	}
	result, err := s.db.Exec(`
		UPDATE planner_artifacts
		SET status=?, capability_digest=?, manifest=?, sections=?, updated_at=?
		WHERE issue_id=? AND stage=? AND attempt=? AND worktree=?`,
		status, append([]byte(nil), digest...), string(manifest), string(sections),
		s.now().Format(time.RFC3339Nano), issueID, stage, attempt, worktree)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// ExpirePlannerArtifact closes an exact authority record without deleting its
// durable progress, allowing history and diagnostics to survive stage cleanup.
func (s *Store) ExpirePlannerArtifact(issueID, stage string, attempt int, worktree string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(`
		UPDATE planner_artifacts SET status=?, updated_at=?
		WHERE issue_id=? AND stage=? AND attempt=? AND worktree=?`,
		"expired", s.now().Format(time.RFC3339Nano), issueID, stage, attempt, worktree)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// FailNextPlannerArtifactReadForTest causes the next registry read to fail.
func (s *Store) FailNextPlannerArtifactReadForTest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNextPlannerArtifactRead = true
}

// FailNextPlannerArtifactWriteForTest causes the next registry write to fail.
func (s *Store) FailNextPlannerArtifactWriteForTest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNextPlannerArtifactWrite = true
}
