package store

import (
	"database/sql"
	"encoding/json"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/wbushyeager/guildhall/internal/core"
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

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
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

func (s *Store) Close() error { return s.db.Close() }
