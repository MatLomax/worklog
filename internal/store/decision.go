package store

// Decision is a choice made while executing a task — the "why we did it this
// way" record that turns a finished task into something you can learn from
// later. Stored as rows (queryable) but rendered as a "## Decisions" section
// when a task is shown.
type Decision struct {
	ID        int64  `json:"id"`
	TaskSlug  string `json:"task_slug,omitempty"`
	SessionID int64  `json:"session_id,omitempty"`
	Ts        string `json:"ts"`
	Decision  string `json:"decision_md"`
	Rationale string `json:"rationale_md,omitempty"`
}

// AddDecision records a decision against a task, attributed to the current
// session, and journals it.
func (s *Store) AddDecision(slug, decision, rationale string) (*Decision, error) {
	t, err := s.taskBySlug(slug)
	if err != nil {
		return nil, err
	}
	ts := now()
	res, err := s.db.Exec(
		`INSERT INTO decision (task_id, session_id, ts, decision_md, rationale_md)
		 VALUES (?,?,?,?,?)`,
		t.ID, s.sessionRef(), ts, decision, rationale)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	s.journalKind(t.ID, "decision", decision)
	return &Decision{ID: id, TaskSlug: slug, SessionID: s.sessionID, Ts: ts, Decision: decision, Rationale: rationale}, nil
}

// Decisions returns the decisions recorded on a task, oldest first.
func (s *Store) Decisions(taskID int64) ([]Decision, error) {
	rows, err := s.db.Query(
		`SELECT id, COALESCE(session_id,0), ts, decision_md, rationale_md
		 FROM decision WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Decision
	for rows.Next() {
		var d Decision
		if err := rows.Scan(&d.ID, &d.SessionID, &d.Ts, &d.Decision, &d.Rationale); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
