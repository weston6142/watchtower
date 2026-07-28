package store

import (
	"database/sql"
	"encoding/json"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/weston6142/watchtower/internal/core"
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
CREATE TABLE IF NOT EXISTS decisions(
  id INTEGER PRIMARY KEY AUTOINCREMENT, issue_id TEXT, question TEXT, options TEXT,
  recommended INTEGER, evidence TEXT, lever TEXT, status TEXT, answer TEXT,
  answered_by TEXT, blocking_cost INTEGER, created_at TEXT);
CREATE TABLE IF NOT EXISTS proposals(
  id INTEGER PRIMARY KEY AUTOINCREMENT, issue_id TEXT, title TEXT, body TEXT, status TEXT);
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

type DecisionRow struct {
	ID           int64
	IssueID      string
	Stage        string
	Question     string
	Options      []string
	Recommended  int
	Why          string
	Consequences []string
	Reversible   string
	Status       string
	Answer       int
	BlockingCost int
	CreatedAt    time.Time
}

type ProposalRow struct {
	ID      int64
	IssueID string
	Title   string
	Body    string
	Status  string
}

type IssueRow struct {
	ID       string
	Title    string
	Body     string
	State    string
	Flow     string
	Levers   map[string]string
	Priority int
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
	var max sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&max); err != nil {
		return nil, err
	}
	return &Store{db: db, seq: max.Int64}, nil
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

// LastStageEvents returns the attempt metadata and error state represented by
// the newest stage_started/stage_failed event for an issue.
func (s *Store) LastStageEvents(issueID string) (attempt, of int, lastErr string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT type,payload FROM events
		 WHERE issue_id=? AND type IN (?,?) ORDER BY seq DESC LIMIT 1`,
		issueID, string(core.EvStageStarted), string(core.EvStageFailed))
	if err != nil {
		return 0, 0, "", err
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, 0, "", rows.Err()
	}
	var typ, payload string
	if err := rows.Scan(&typ, &payload); err != nil {
		return 0, 0, "", err
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return 0, 0, "", err
	}
	if v, ok := p["attempt"].(float64); ok {
		attempt = int(v)
	}
	if v, ok := p["of"].(float64); ok {
		of = int(v)
	}
	if typ == string(core.EvStageFailed) {
		lastErr, _ = p["error"].(string)
	}
	return attempt, of, lastErr, nil
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
	Why          string   `json:"why"`
	Consequences []string `json:"consequences"`
	Reversible   string   `json:"reversible"`
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
	evidence, err := json.Marshal(decisionContext{d.Why, d.Consequences, d.Reversible})
	if err != nil {
		return 0, err
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
		d.Answer, "", d.BlockingCost, d.CreatedAt.Format(time.RFC3339Nano), string(evidence))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) AnswerDecision(id int64, answer int, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE decisions SET status=?, answer=? WHERE id=?`, status, answer, id)
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
		var opts, evidence, created string
		// The legacy Plan 1 schema calls the stage column "lever"; keep using
		// it as the persisted stage name without a migration.
		if err := rows.Scan(&d.ID, &d.IssueID, &d.Stage, &d.Question, &opts,
			&d.Recommended, &evidence, &d.Status, &d.Answer, &d.BlockingCost, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(opts), &d.Options); err != nil {
			return nil, err
		}
		if evidence != "" {
			var context decisionContext
			if json.Unmarshal([]byte(evidence), &context) == nil {
				d.Why, d.Consequences, d.Reversible = context.Why, context.Consequences, context.Reversible
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

func (s *Store) InsertProposal(issueID, title, body string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		`INSERT INTO proposals(issue_id,title,body,status) VALUES(?,?,?,'pending')`,
		issueID, title, body)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) SetProposalStatus(id int64, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE proposals SET status=? WHERE id=?`, status, id)
	return err
}

func (s *Store) PendingProposals() ([]ProposalRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT id,issue_id,title,body,status FROM proposals WHERE status='pending' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProposalRow
	for rows.Next() {
		var p ProposalRow
		if err := rows.Scan(&p.ID, &p.IssueID, &p.Title, &p.Body, &p.Status); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
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
	return out, rows.Err()
}

func (s *Store) Close() error { return s.db.Close() }
