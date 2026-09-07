package store

import "database/sql"

// JournalEntry is one line in the running log of what happened — the trail a
// fresh session reads to reconstruct where the last one left off.
type JournalEntry struct {
	ID        int64  `json:"id"`
	TaskSlug  string `json:"task_slug,omitempty"`
	SessionID int64  `json:"session_id,omitempty"`
	Ts        string `json:"ts"`
	Kind      string `json:"kind"`
	Text      string `json:"text_md"`
}

// AddJournal appends a freeform note, optionally against a task (slug ""
// records a project-level note).
func (s *Store) AddJournal(slug, text string) (*JournalEntry, error) {
	var taskID int64
	if slug != "" {
		t, err := s.taskBySlug(slug)
		if err != nil {
			return nil, err
		}
		taskID = t.ID
	}
	return s.journalKind(taskID, "note", text)
}

// journal appends an entry with the default "note" kind.
// querier is the read/write surface shared by *sql.DB and *sql.Tx, so a helper
// can run either standalone or inside a caller's transaction.
type querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func (s *Store) journal(taskID int64, kind, text string) (*JournalEntry, error) {
	return s.journalKind(taskID, kind, text)
}

// journalOn appends a typed entry on the given executor (DB or Tx), attributed
// to the current session. Used when the write must join a transaction.
func (s *Store) journalOn(x querier, taskID int64, kind, text string) error {
	var tid sql.NullInt64
	if taskID != 0 {
		tid = sql.NullInt64{Int64: taskID, Valid: true}
	}
	_, err := x.Exec(
		`INSERT INTO journal (task_id, session_id, ts, kind, text_md) VALUES (?,?,?,?,?)`,
		tid, s.sessionRef(), now(), kind, text)
	return err
}

// journalKind appends a typed entry, attributed to the current session. A
// taskID of 0 records a project-level entry with no task.
func (s *Store) journalKind(taskID int64, kind, text string) (*JournalEntry, error) {
	var tid sql.NullInt64
	if taskID != 0 {
		tid = sql.NullInt64{Int64: taskID, Valid: true}
	}
	ts := now()
	res, err := s.db.Exec(
		`INSERT INTO journal (task_id, session_id, ts, kind, text_md) VALUES (?,?,?,?,?)`,
		tid, s.sessionRef(), ts, kind, text)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &JournalEntry{ID: id, SessionID: s.sessionID, Ts: ts, Kind: kind, Text: text}, nil
}

// RecentJournal returns the most recent entries across the whole project,
// newest first, joined to their task slug when they have one.
func (s *Store) RecentJournal(limit int) ([]JournalEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(
		`SELECT j.id, COALESCE(t.slug,''), COALESCE(j.session_id,0), j.ts, j.kind, j.text_md
		 FROM journal j LEFT JOIN task t ON t.id = j.task_id
		 ORDER BY j.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JournalEntry
	for rows.Next() {
		var e JournalEntry
		if err := rows.Scan(&e.ID, &e.TaskSlug, &e.SessionID, &e.Ts, &e.Kind, &e.Text); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// TaskJournal returns a task's own journal entries, newest first.
func (s *Store) TaskJournal(taskID int64, limit int) ([]JournalEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(
		`SELECT id, COALESCE(session_id,0), ts, kind, text_md
		 FROM journal WHERE task_id = ? ORDER BY id DESC LIMIT ?`, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JournalEntry
	for rows.Next() {
		var e JournalEntry
		if err := rows.Scan(&e.ID, &e.SessionID, &e.Ts, &e.Kind, &e.Text); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
