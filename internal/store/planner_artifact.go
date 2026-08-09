package store

import (
	"database/sql"
	"fmt"
	"time"
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
	var updatedAt string
	err = s.db.QueryRow(`
		SELECT status, capability_digest, manifest, sections, updated_at
		FROM planner_artifacts
		WHERE issue_id=? AND stage=? AND attempt=? AND worktree=?`,
		issueID, stage, attempt, worktree).Scan(&status, &digest, &manifest, &sections, &updatedAt)
	if err == sql.ErrNoRows {
		return "", nil, nil, nil, false, nil
	}
	if err != nil {
		return "", nil, nil, nil, false, err
	}
	return status, append([]byte(nil), digest...), []byte(manifest), []byte(sections), true, nil
}

// CreatePlannerArtifact inserts one exact-tuple authority record.
func (s *Store) CreatePlannerArtifact(issueID, stage string, attempt int, worktree, status string, digest, manifest, sections []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNextPlannerArtifactWrite {
		s.failNextPlannerArtifactWrite = false
		return fmt.Errorf("injected planner artifact registry write failure")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
		INSERT INTO planner_artifacts(
			issue_id, stage, attempt, worktree, status, capability_digest,
			manifest, sections, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		issueID, stage, attempt, worktree, status, append([]byte(nil), digest...),
		string(manifest), string(sections), now, now)
	return err
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
		time.Now().UTC().Format(time.RFC3339Nano), issueID, stage, attempt, worktree)
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
		"expired", time.Now().UTC().Format(time.RFC3339Nano), issueID, stage, attempt, worktree)
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
