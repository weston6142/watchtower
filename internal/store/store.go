package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/deps"
	"github.com/weston6142/watchtower/internal/levers"
)

const schema = `
CREATE TABLE IF NOT EXISTS events(
  id INTEGER PRIMARY KEY AUTOINCREMENT, seq INTEGER UNIQUE,
  type TEXT, issue_id TEXT, payload TEXT, at TEXT);
CREATE TABLE IF NOT EXISTS issues(
  id TEXT PRIMARY KEY, title TEXT, body TEXT, state TEXT,
  flow TEXT, levers TEXT, priority INTEGER, links TEXT);
CREATE TABLE IF NOT EXISTS stage_runs(
  id INTEGER PRIMARY KEY AUTOINCREMENT, issue_id TEXT, stage TEXT, agent TEXT,
  session_id TEXT, worktree TEXT, artifacts TEXT, status TEXT, tokens INTEGER);
CREATE TABLE IF NOT EXISTS stage_checkpoints(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  issue_id TEXT NOT NULL,
  stage TEXT NOT NULL,
  start_commit TEXT,
  end_commit TEXT,
  artifacts TEXT NOT NULL,
  status TEXT NOT NULL,
  session_id TEXT,
  failure TEXT,
  created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS decisions(
  id INTEGER PRIMARY KEY AUTOINCREMENT, issue_id TEXT, question TEXT, options TEXT,
  recommended INTEGER, evidence TEXT, lever TEXT, status TEXT, answer TEXT,
  answered_by TEXT, blocking_cost INTEGER, created_at TEXT);
CREATE TABLE IF NOT EXISTS proposals(
  id INTEGER PRIMARY KEY AUTOINCREMENT, issue_id TEXT, title TEXT, body TEXT,
  status TEXT, depends_on TEXT NOT NULL DEFAULT '[]',
  batch_id INTEGER NOT NULL DEFAULT 0, task_key TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS attachments(
  id INTEGER PRIMARY KEY AUTOINCREMENT, issue_id TEXT, name TEXT,
  size INTEGER, source_path TEXT, added_at TEXT, ord INTEGER);
CREATE INDEX IF NOT EXISTS attachments_issue ON attachments(issue_id);
CREATE TABLE IF NOT EXISTS issue_dependencies(
  issue_id TEXT NOT NULL, depends_on TEXT NOT NULL,
  PRIMARY KEY(issue_id, depends_on));
CREATE INDEX IF NOT EXISTS issue_dependencies_parent
  ON issue_dependencies(depends_on);
CREATE TABLE IF NOT EXISTS issue_integration(
  issue_id TEXT PRIMARY KEY,
  state TEXT NOT NULL,
  base_branch TEXT,
  pre_sha TEXT,
  landed_sha TEXT,
  last_error TEXT,
  worktree TEXT NOT NULL DEFAULT '',
  branch TEXT NOT NULL DEFAULT '',
  cleanup TEXT NOT NULL DEFAULT '[]',
  updated_at TEXT NOT NULL);
`

type Store struct {
	db  *sql.DB
	mu  sync.Mutex
	seq int64
}

type StageRun struct {
	ID        int64
	IssueID   string
	Stage     string
	Agent     string
	SessionID string
	Worktree  string
	Status    string
	Tokens    int
}

type StageCheckpoint struct {
	ID          int64
	IssueID     string
	Stage       string
	StartCommit string
	EndCommit   string
	Artifacts   []contextpack.Artifact
	Status      string
	SessionID   string
	Failure     string
	CreatedAt   time.Time
}

type DecisionRow struct {
	ID                  int64
	IssueID             string
	Stage               string
	Kind                levers.DecisionKind
	Question            string
	Options             []string
	Recommended         int
	RecommendedResponse string
	AllowFreeform       bool
	Importance          float64
	Paths               []string
	Why                 string
	Consequences        []string
	Reversible          string
	Status              string
	Response            levers.Response
	BlockingCost        int
	CreatedAt           time.Time
}

type ProposalRow struct {
	ID        int64
	BatchID   int64
	IssueID   string
	Key       string
	Title     string
	Body      string
	Status    string
	DependsOn []string
}

// AttachmentRow is one attachment's metadata. Bytes live on disk under the
// issue dir, never in SQLite; SourcePath is provenance for operator debugging
// and nothing reads it to locate a file.
type AttachmentRow struct {
	ID         int64
	IssueID    string
	Name       string
	Size       int64
	SourcePath string
	AddedAt    time.Time
	Ord        int
}

type IssueRow struct {
	ID        string
	Title     string
	Body      string
	State     string
	Flow      string
	Levers    map[string]string
	Priority  int
	DependsOn []string
}

const (
	IntegrationClaimed           = "claimed"
	IntegrationVerificationReady = "verification_ready"
	IntegrationPublishPending    = "publish_pending"
	IntegrationCleanupNeeded     = "cleanup_needed"
	IntegrationMerged            = "merged"
	IntegrationPreserved         = "preserved"
)

type IssueIntegration struct {
	IssueID    string
	State      string
	BaseBranch string
	PreSHA     string
	LandedSHA  string
	LastError  string
	Worktree   string
	Branch     string
	Cleanup    []string
	UpdatedAt  time.Time
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// Parallel stage agents insert stage_runs concurrently; without a busy
	// timeout SQLite returns SQLITE_BUSY and rows are silently lost.
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	if err := ensureColumn(db, "proposals", "depends_on",
		`ALTER TABLE proposals ADD COLUMN depends_on TEXT NOT NULL DEFAULT '[]'`); err != nil {
		return nil, err
	}
	if err := ensureColumn(db, "proposals", "batch_id",
		`ALTER TABLE proposals ADD COLUMN batch_id INTEGER NOT NULL DEFAULT 0`); err != nil {
		return nil, err
	}
	if err := ensureColumn(db, "proposals", "task_key",
		`ALTER TABLE proposals ADD COLUMN task_key TEXT NOT NULL DEFAULT ''`); err != nil {
		return nil, err
	}
	if err := ensureColumn(db, "issue_integration", "cleanup",
		`ALTER TABLE issue_integration ADD COLUMN cleanup TEXT NOT NULL DEFAULT '[]'`); err != nil {
		return nil, err
	}
	if err := ensureColumn(db, "issue_integration", "worktree",
		`ALTER TABLE issue_integration ADD COLUMN worktree TEXT NOT NULL DEFAULT ''`); err != nil {
		return nil, err
	}
	if err := ensureColumn(db, "issue_integration", "branch",
		`ALTER TABLE issue_integration ADD COLUMN branch TEXT NOT NULL DEFAULT ''`); err != nil {
		return nil, err
	}
	var max sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&max); err != nil {
		return nil, err
	}
	return &Store{db: db, seq: max.Int64}, nil
}

func ensureColumn(db *sql.DB, table, column, alter string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = db.Exec(alter)
	return err
}

func (s *Store) Append(ev core.Event) (core.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	ev.Seq = s.seq
	res, err := s.db.Exec(
		`INSERT INTO events(seq,type,issue_id,payload,at) VALUES(?,?,?,?,?)`,
		ev.Seq, string(ev.Type), ev.IssueID, string(ev.Payload), ev.At.Format(time.RFC3339Nano))
	if err != nil {
		return ev, err
	}
	ev.ID, _ = res.LastInsertId()
	return ev, nil
}

func (s *Store) EventsSince(seq int64) ([]core.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT id,seq,type,issue_id,payload,at FROM events WHERE seq > ? ORDER BY seq`, seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Event
	for rows.Next() {
		var ev core.Event
		var typ, payload, at string
		if err := rows.Scan(&ev.ID, &ev.Seq, &typ, &ev.IssueID, &payload, &at); err != nil {
			return nil, err
		}
		ev.Type = core.EventType(typ)
		ev.Payload = json.RawMessage(payload)
		ev.At, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *Store) SetIssueIntegration(integration IssueIntegration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	updatedAt := integration.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	cleanup, err := json.Marshal(integration.Cleanup)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO issue_integration(
		   issue_id,state,base_branch,pre_sha,landed_sha,last_error,worktree,branch,cleanup,updated_at
		 ) VALUES(?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(issue_id) DO UPDATE SET
		   state=excluded.state,
		   base_branch=excluded.base_branch,
		   pre_sha=excluded.pre_sha,
		   landed_sha=excluded.landed_sha,
		   last_error=excluded.last_error,
		   worktree=excluded.worktree,
		   branch=excluded.branch,
		   cleanup=excluded.cleanup,
		   updated_at=excluded.updated_at`,
		integration.IssueID, integration.State, integration.BaseBranch,
		integration.PreSHA, integration.LandedSHA, integration.LastError,
		integration.Worktree, integration.Branch, string(cleanup),
		updatedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) IssueIntegration(issueID string) (IssueIntegration, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var integration IssueIntegration
	var cleanup, updatedAt string
	err := s.db.QueryRow(
		`SELECT issue_id,state,base_branch,pre_sha,landed_sha,last_error,worktree,branch,cleanup,updated_at
		 FROM issue_integration WHERE issue_id=?`, issueID,
	).Scan(&integration.IssueID, &integration.State, &integration.BaseBranch,
		&integration.PreSHA, &integration.LandedSHA, &integration.LastError,
		&integration.Worktree, &integration.Branch, &cleanup, &updatedAt)
	if err == sql.ErrNoRows {
		return IssueIntegration{}, false, nil
	}
	if err != nil {
		return IssueIntegration{}, false, err
	}
	if err := json.Unmarshal([]byte(cleanup), &integration.Cleanup); err != nil {
		return IssueIntegration{}, false, fmt.Errorf("decode integration cleanup: %w", err)
	}
	integration.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return IssueIntegration{}, false, fmt.Errorf("parse integration update time: %w", err)
	}
	return integration, true, nil
}

func (s *Store) DeleteIssueIntegration(issueID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM issue_integration WHERE issue_id=?`, issueID)
	return err
}

// EventsSinceTime returns events at or after t in sequence order.
func (s *Store) EventsSinceTime(t time.Time) ([]core.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT id,seq,type,issue_id,payload,at FROM events WHERE at >= ? ORDER BY seq`,
		t.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Event
	for rows.Next() {
		var ev core.Event
		var typ, payload, at string
		if err := rows.Scan(&ev.ID, &ev.Seq, &typ, &ev.IssueID, &payload, &at); err != nil {
			return nil, err
		}
		ev.Type = core.EventType(typ)
		ev.Payload = json.RawMessage(payload)
		ev.At, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// LastStageEvents returns the stage name, attempt metadata, and error state
// represented by the newest stage_started/stage_failed event for an issue.
func (s *Store) LastStageEvents(issueID string) (stage string, attempt, of int, lastErr string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT type,payload FROM events
		 WHERE issue_id=? AND type IN (?,?) ORDER BY seq DESC LIMIT 1`,
		issueID, string(core.EvStageStarted), string(core.EvStageFailed))
	if err != nil {
		return "", 0, 0, "", err
	}
	defer rows.Close()
	if !rows.Next() {
		return "", 0, 0, "", rows.Err()
	}
	var typ, payload string
	if err := rows.Scan(&typ, &payload); err != nil {
		return "", 0, 0, "", err
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return "", 0, 0, "", err
	}
	stage, _ = p["stage"].(string)
	if v, ok := p["attempt"].(float64); ok {
		attempt = int(v)
	}
	if v, ok := p["of"].(float64); ok {
		of = int(v)
	}
	if typ == string(core.EvStageFailed) {
		lastErr, _ = p["error"].(string)
	}
	return stage, attempt, of, lastErr, nil
}

func (s *Store) InsertStageRun(r StageRun) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		`INSERT INTO stage_runs(issue_id,stage,agent,session_id,worktree,status,tokens)
		 VALUES(?,?,?,?,?,?,?)`,
		r.IssueID, r.Stage, r.Agent, r.SessionID, r.Worktree, r.Status, r.Tokens)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishStageRun(id int64, status, sessionID string, tokens int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`UPDATE stage_runs SET status=?, session_id=?, tokens=? WHERE id=?`,
		status, sessionID, tokens, id)
	return err
}

func (s *Store) StageRuns(issueID string) ([]StageRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT id,issue_id,stage,agent,session_id,worktree,status,tokens
		 FROM stage_runs WHERE issue_id=? ORDER BY id`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StageRun
	for rows.Next() {
		var r StageRun
		if err := rows.Scan(&r.ID, &r.IssueID, &r.Stage, &r.Agent,
			&r.SessionID, &r.Worktree, &r.Status, &r.Tokens); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) InsertStageCheckpoint(checkpoint StageCheckpoint) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	artifacts, err := json.Marshal(checkpoint.Artifacts)
	if err != nil {
		return 0, err
	}
	createdAt := checkpoint.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	result, err := s.db.Exec(
		`INSERT INTO stage_checkpoints(
			issue_id,stage,start_commit,end_commit,artifacts,status,session_id,failure,created_at
		 ) VALUES(?,?,?,?,?,?,?,?,?)`,
		checkpoint.IssueID, checkpoint.Stage, checkpoint.StartCommit, checkpoint.EndCommit,
		string(artifacts), checkpoint.Status, checkpoint.SessionID, checkpoint.Failure,
		createdAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) FinishStageCheckpoint(
	id int64, status, endCommit, sessionID, failure string, artifacts []contextpack.Artifact,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	encoded, err := json.Marshal(artifacts)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`UPDATE stage_checkpoints
		 SET status=?,end_commit=?,session_id=?,failure=?,artifacts=? WHERE id=?`,
		status, endCommit, sessionID, failure, string(encoded), id)
	return err
}

func (s *Store) StageCheckpoints(issueID string) ([]StageCheckpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT id,issue_id,stage,start_commit,end_commit,artifacts,status,
		        session_id,failure,created_at
		 FROM stage_checkpoints WHERE issue_id=? ORDER BY id`,
		issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var checkpoints []StageCheckpoint
	for rows.Next() {
		var checkpoint StageCheckpoint
		var artifacts, createdAt string
		if err := rows.Scan(
			&checkpoint.ID, &checkpoint.IssueID, &checkpoint.Stage,
			&checkpoint.StartCommit, &checkpoint.EndCommit, &artifacts,
			&checkpoint.Status, &checkpoint.SessionID, &checkpoint.Failure, &createdAt,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(artifacts), &checkpoint.Artifacts); err != nil {
			return nil, err
		}
		checkpoint.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, err
		}
		checkpoints = append(checkpoints, checkpoint)
	}
	return checkpoints, rows.Err()
}

func (s *Store) LastSuccessfulCheckpoint(issueID string) (StageCheckpoint, bool, error) {
	checkpoints, err := s.StageCheckpoints(issueID)
	if err != nil {
		return StageCheckpoint{}, false, err
	}
	for index := len(checkpoints) - 1; index >= 0; index-- {
		if checkpoints[index].Status == "succeeded" {
			return checkpoints[index], true, nil
		}
	}
	return StageCheckpoint{}, false, nil
}

func (s *Store) IssueTokens(issueID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	err := s.db.QueryRow(
		`SELECT COALESCE(SUM(tokens),0) FROM stage_runs WHERE issue_id=?`, issueID).Scan(&n)
	return n, err
}

func (s *Store) TotalTokens() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	err := s.db.QueryRow(`SELECT COALESCE(SUM(tokens),0) FROM stage_runs`).Scan(&n)
	return n, err
}

// ArtifactPaths returns artifact paths emitted for an issue, in event order.
func (s *Store) ArtifactPaths(issueID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT payload FROM events WHERE type=? AND issue_id=? ORDER BY seq`,
		string(core.EvArtifactProduced), issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	seen := map[string]struct{}{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var artifact struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(payload), &artifact); err != nil {
			return nil, err
		}
		if artifact.Path != "" {
			if _, ok := seen[artifact.Path]; ok {
				continue
			}
			seen[artifact.Path] = struct{}{}
			out = append(out, artifact.Path)
		}
	}
	return out, rows.Err()
}

// decisionContext is the v2 rationale metadata persisted in the decisions
// table's evidence column.
type decisionContext struct {
	Kind                levers.DecisionKind `json:"kind,omitempty"`
	RecommendedResponse string              `json:"recommended_response,omitempty"`
	AllowFreeform       bool                `json:"allow_freeform,omitempty"`
	Importance          float64             `json:"importance,omitempty"`
	Paths               []string            `json:"paths,omitempty"`
	Why                 string              `json:"why"`
	Consequences        []string            `json:"consequences"`
	Reversible          string              `json:"reversible"`
}

func (s *Store) InsertDecision(d DecisionRow) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	opts, err := json.Marshal(d.Options)
	if err != nil {
		return 0, err
	}
	// Reuse the existing evidence column for v2 decision context; this avoids
	// a schema migration while keeping rationale metadata with the decision.
	kind := d.Kind
	if kind == "" {
		kind = levers.DecisionChoice
	}
	evidence, err := json.Marshal(decisionContext{
		Kind: kind, RecommendedResponse: d.RecommendedResponse,
		AllowFreeform: d.AllowFreeform, Importance: d.Importance, Paths: d.Paths,
		Why: d.Why, Consequences: d.Consequences, Reversible: d.Reversible,
	})
	if err != nil {
		return 0, err
	}
	answer := ""
	if d.Response.Kind != "" {
		encoded, err := json.Marshal(d.Response)
		if err != nil {
			return 0, err
		}
		answer = string(encoded)
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	if d.Status == "" {
		d.Status = "pending"
	}
	res, err := s.db.Exec(
		`INSERT INTO decisions(issue_id,question,options,recommended,lever,status,answer,answered_by,blocking_cost,created_at,evidence)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		d.IssueID, d.Question, string(opts), d.Recommended, d.Stage, d.Status,
		answer, "", d.BlockingCost, d.CreatedAt.Format(time.RFC3339Nano), string(evidence))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) AnswerDecision(id int64, response levers.Response, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	answer, err := json.Marshal(response)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE decisions SET status=?, answer=? WHERE id=?`, status, string(answer), id)
	return err
}

func (s *Store) CloseDecision(id int64, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE decisions SET status=?, answer='' WHERE id=?`, status, id)
	return err
}

func (s *Store) decisionRows(where string) ([]DecisionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT id,issue_id,lever,question,options,recommended,evidence,status,answer,blocking_cost,created_at
		 FROM decisions ` + where + ` ORDER BY blocking_cost DESC, created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DecisionRow
	for rows.Next() {
		var d DecisionRow
		var opts, evidence, answer, created string
		// The legacy Plan 1 schema calls the stage column "lever"; keep using
		// it as the persisted stage name without a migration.
		if err := rows.Scan(&d.ID, &d.IssueID, &d.Stage, &d.Question, &opts,
			&d.Recommended, &evidence, &d.Status, &answer, &d.BlockingCost, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(opts), &d.Options); err != nil {
			return nil, err
		}
		if evidence != "" {
			var context decisionContext
			if json.Unmarshal([]byte(evidence), &context) == nil {
				d.Kind = context.Kind
				d.RecommendedResponse = context.RecommendedResponse
				d.AllowFreeform = context.AllowFreeform
				d.Importance, d.Paths = context.Importance, context.Paths
				d.Why, d.Consequences, d.Reversible =
					context.Why, context.Consequences, context.Reversible
			}
		}
		if d.Kind == "" {
			d.Kind = levers.DecisionChoice
		}
		if answer != "" && (d.Status == "answered" || d.Status == "auto") {
			if err := json.Unmarshal([]byte(answer), &d.Response); err != nil {
				legacy, legacyErr := strconv.Atoi(answer)
				if legacyErr != nil {
					return nil, err
				}
				d.Response = levers.ChoiceResponse(legacy)
			}
		}
		d.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) PendingDecisionRows() ([]DecisionRow, error) {
	return s.decisionRows(`WHERE status='pending'`)
}

func (s *Store) AllDecisionRows() ([]DecisionRow, error) {
	return s.decisionRows(``)
}

func (s *Store) InsertProposal(issueID, title, body string, dependsOn []string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	encoded, err := json.Marshal(deps.Normalize(dependsOn))
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(
		`INSERT INTO proposals(issue_id,title,body,status,depends_on) VALUES(?,?,?,'pending',?)`,
		issueID, title, body, string(encoded))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) InsertProposalBatch(issueID string, proposals []ProposalRow) (int64, error) {
	if len(proposals) == 0 {
		return 0, fmt.Errorf("proposal batch is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var batchID int64
	for _, proposal := range proposals {
		encoded, err := json.Marshal(deps.Normalize(proposal.DependsOn))
		if err != nil {
			return 0, err
		}
		res, err := tx.Exec(
			`INSERT INTO proposals(
				issue_id,title,body,status,depends_on,batch_id,task_key
			 ) VALUES(?,?,?,'pending',?,?,?)`,
			issueID, proposal.Title, proposal.Body, string(encoded), batchID, proposal.Key)
		if err != nil {
			return 0, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return 0, err
		}
		if batchID == 0 {
			batchID = id
			if _, err := tx.Exec(`UPDATE proposals SET batch_id=? WHERE id=?`, batchID, id); err != nil {
				return 0, err
			}
		}
	}
	return batchID, tx.Commit()
}

func (s *Store) SetProposalStatus(id int64, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE proposals SET status=? WHERE id=?`, status, id)
	return err
}

func (s *Store) SetProposalBatchStatus(batchID int64, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE proposals SET status=? WHERE batch_id=?`, status, batchID)
	return err
}

func (s *Store) PendingProposals() ([]ProposalRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT id,batch_id,issue_id,task_key,title,body,status,depends_on
		 FROM proposals WHERE status='pending' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProposalRow
	for rows.Next() {
		var p ProposalRow
		var dependsOn string
		if err := rows.Scan(
			&p.ID, &p.BatchID, &p.IssueID, &p.Key, &p.Title, &p.Body, &p.Status, &dependsOn); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(dependsOn), &p.DependsOn); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AcceptProposalBatch stores every generated issue and edge, then accepts the
// whole pending batch in the same SQLite transaction.
func (s *Store) AcceptProposalBatch(batchID int64, issues []IssueRow, edges map[string][]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, issue := range issues {
		encodedLevers, err := json.Marshal(issue.Levers)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO issues(id,title,body,state,flow,levers,priority)
			 VALUES(?,?,?,?,?,?,?)`,
			issue.ID, issue.Title, issue.Body, issue.State, issue.Flow,
			string(encodedLevers), issue.Priority); err != nil {
			return err
		}
		for _, parent := range edges[issue.ID] {
			if _, err := tx.Exec(
				`INSERT INTO issue_dependencies(issue_id,depends_on) VALUES(?,?)`,
				issue.ID, parent); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(
		`UPDATE proposals SET status='accepted' WHERE batch_id=? AND status='pending'`,
		batchID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpsertIssue(r IssueRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	levers, err := json.Marshal(r.Levers)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO issues(id,title,body,state,flow,levers,priority)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			title=excluded.title, body=excluded.body, state=excluded.state,
			flow=excluded.flow, levers=excluded.levers, priority=excluded.priority`,
		r.ID, r.Title, r.Body, r.State, r.Flow, string(levers), r.Priority)
	return err
}

// SetIssueLever persists one stage lever without replacing the other stages.
func (s *Store) SetIssueLever(issueID, stage, lever string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var raw string
	if err := s.db.QueryRow(`SELECT levers FROM issues WHERE id=?`, issueID).Scan(&raw); err != nil {
		return err
	}
	values := map[string]string{}
	if raw != "" && raw != "null" {
		if err := json.Unmarshal([]byte(raw), &values); err != nil {
			return err
		}
	}
	values[stage] = lever
	encoded, err := json.Marshal(values)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE issues SET levers=? WHERE id=?`, string(encoded), issueID)
	return err
}

func (s *Store) Issues() ([]IssueRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT id,title,body,state,flow,levers,priority FROM issues ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IssueRow
	for rows.Next() {
		var r IssueRow
		var raw string
		if err := rows.Scan(&r.ID, &r.Title, &r.Body, &r.State, &r.Flow, &raw, &r.Priority); err != nil {
			return nil, err
		}
		if raw != "" && raw != "null" {
			if err := json.Unmarshal([]byte(raw), &r.Levers); err != nil {
				return nil, err
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	dependencies, err := s.db.Query(
		`SELECT issue_id,depends_on FROM issue_dependencies ORDER BY issue_id,depends_on`)
	if err != nil {
		return nil, err
	}
	defer dependencies.Close()
	byIssue := map[string][]string{}
	for dependencies.Next() {
		var issueID, parent string
		if err := dependencies.Scan(&issueID, &parent); err != nil {
			return nil, err
		}
		byIssue[issueID] = append(byIssue[issueID], parent)
	}
	if err := dependencies.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].DependsOn = byIssue[out[i].ID]
	}
	return out, nil
}

func (s *Store) ReplaceDependencies(issueID string, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM issue_dependencies WHERE issue_id=?`, issueID); err != nil {
		return err
	}
	for _, parent := range ids {
		if _, err := tx.Exec(
			`INSERT INTO issue_dependencies(issue_id,depends_on) VALUES(?,?)`,
			issueID, parent); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Dependencies(issueID string) ([]string, error) {
	return s.dependencyIDs(
		`SELECT depends_on FROM issue_dependencies WHERE issue_id=? ORDER BY depends_on`,
		issueID)
}

func (s *Store) Dependents(issueID string) ([]string, error) {
	return s.dependencyIDs(
		`SELECT issue_id FROM issue_dependencies WHERE depends_on=? ORDER BY issue_id`,
		issueID)
}

func (s *Store) DependencyGraph() (deps.Graph, error) {
	issues, err := s.Issues()
	if err != nil {
		return nil, err
	}
	graph := make(deps.Graph, len(issues))
	for _, issue := range issues {
		graph[issue.ID] = append([]string(nil), issue.DependsOn...)
	}
	return graph, nil
}

func (s *Store) dependencyIDs(query, issueID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(query, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ReplaceAttachments swaps an issue's whole attachment set in one transaction,
// numbering ord by index. Rows are cheap and bytes are not, so churning rows on
// every edit is fine and keeps display order equal to the order typed.
func (s *Store) ReplaceAttachments(issueID string, rows []AttachmentRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM attachments WHERE issue_id=?`, issueID); err != nil {
		return err
	}
	for i, r := range rows {
		at := r.AddedAt
		if at.IsZero() {
			at = time.Now().UTC()
		}
		if _, err := tx.Exec(
			`INSERT INTO attachments(issue_id,name,size,source_path,added_at,ord)
			 VALUES(?,?,?,?,?,?)`,
			issueID, r.Name, r.Size, r.SourcePath, at.UTC().Format(time.RFC3339Nano), i); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Attachments(issueID string) ([]AttachmentRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT id,issue_id,name,size,source_path,added_at,ord
		 FROM attachments WHERE issue_id=? ORDER BY ord`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AttachmentRow
	for rows.Next() {
		var r AttachmentRow
		var at string
		if err := rows.Scan(&r.ID, &r.IssueID, &r.Name, &r.Size, &r.SourcePath, &at, &r.Ord); err != nil {
			return nil, err
		}
		r.AddedAt, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAttachments(issueID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM attachments WHERE issue_id=?`, issueID)
	return err
}

// DeleteIssue removes a creation that failed before its public event was
// emitted. It is intentionally narrow and is not an operator-facing delete.
func (s *Store) DeleteIssue(issueID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, query := range []string{
		`DELETE FROM issue_dependencies WHERE issue_id=? OR depends_on=?`,
		`DELETE FROM attachments WHERE issue_id=?`,
		`DELETE FROM issues WHERE id=?`,
	} {
		var execErr error
		if strings.Contains(query, " OR ") {
			_, execErr = tx.Exec(query, issueID, issueID)
		} else {
			_, execErr = tx.Exec(query, issueID)
		}
		if execErr != nil {
			return execErr
		}
	}
	return tx.Commit()
}

func (s *Store) Close() error { return s.db.Close() }
