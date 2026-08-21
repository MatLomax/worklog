package store

import "database/sql"

// Session is one agent working session: the unit worklog attributes decisions
// and journal entries to, so a later session can see who did what and when.
type Session struct {
	ID        int64  `json:"id"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at,omitempty"`
	Agent     string `json:"agent"`
	Summary   string `json:"summary_md,omitempty"`
}

// StartSession opens a new session and marks it current, so subsequent writes
// (decisions, journal) are attributed to it. agent is a free label, typically
// the client name from the MCP handshake.
func (s *Store) StartSession(agent string) (*Session, error) {
	res, err := s.db.Exec(
		`INSERT INTO session (started_at, agent) VALUES (?, ?)`, now(), agent)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	s.sessionID = id
	return s.GetSession(id)
}

// EndSession records summary against the session and stamps its end time. When
// summary is empty the existing summary is left untouched.
func (s *Store) EndSession(id int64, summary string) (*Session, error) {
	if summary != "" {
		if _, err := s.db.Exec(
			`UPDATE session SET summary_md = ?, ended_at = ? WHERE id = ?`,
			summary, now(), id); err != nil {
			return nil, err
		}
	} else if _, err := s.db.Exec(
		`UPDATE session SET ended_at = ? WHERE id = ?`, now(), id); err != nil {
		return nil, err
	}
	return s.GetSession(id)
}

// CurrentSessionID returns the session writes are attributed to, or 0 if none.
func (s *Store) CurrentSessionID() int64 { return s.sessionID }

// EndCurrentSession closes the in-process session (set by StartSession), if any.
// Safe to call more than once.
func (s *Store) EndCurrentSession() error {
	if s.sessionID == 0 {
		return nil
	}
	_, err := s.EndSession(s.sessionID, "")
	s.sessionID = 0
	return err
}

// EndLatestOpenSession closes the most recent session that has no end time,
// recording summary when non-empty. It is the entry point a Stop hook uses, so
// it need not know the session id. A no-op when nothing is open.
func (s *Store) EndLatestOpenSession(summary string) (*Session, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM session WHERE ended_at IS NULL ORDER BY id DESC LIMIT 1`).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.EndSession(id, summary)
}

// sessionRef returns the current session id as a nullable column value.
func (s *Store) sessionRef() sql.NullInt64 {
	if s.sessionID == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: s.sessionID, Valid: true}
}

// GetSession returns one session by id.
func (s *Store) GetSession(id int64) (*Session, error) {
	var se Session
	var ended, summary sql.NullString
	err := s.db.QueryRow(
		`SELECT id, started_at, ended_at, agent, summary_md FROM session WHERE id = ?`, id).
		Scan(&se.ID, &se.StartedAt, &ended, &se.Agent, &summary)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	se.EndedAt = ended.String
	se.Summary = summary.String
	return &se, nil
}
