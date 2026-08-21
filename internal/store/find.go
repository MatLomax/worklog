package store

import (
	"fmt"
	"strings"
)

// FindOpts constrains a work-find search. Any subset may be set.
type FindOpts struct {
	Query        string // free text matched against titles, bodies, decisions, journal, link URLs
	Status       string // restrict matched tasks to this status
	Since        string // RFC3339 lower bound on decision/journal timestamps
	HasDecisions bool   // only tasks that carry at least one decision
}

// FindResult is what a search turned up, kept in three buckets so "what was
// decided" and "what was done" are answerable, not just "what's open".
type FindResult struct {
	Tasks     []TaskView     `json:"tasks,omitempty"`
	Decisions []Decision     `json:"decisions,omitempty"`
	Journal   []JournalEntry `json:"journal,omitempty"`
}

// Find searches across tasks, decisions, and the journal.
func (s *Store) Find(o FindOpts) (*FindResult, error) {
	res := &FindResult{}
	like := "%" + o.Query + "%"

	// Tasks: text match on title/body, or (with an empty query) all tasks that
	// pass the filters. Link matches fold in the task that owns the link.
	taskWhere := []string{}
	var taskArgs []any
	if o.Query != "" {
		taskWhere = append(taskWhere,
			`(title LIKE ? OR body_md LIKE ? OR id IN (SELECT task_id FROM link WHERE url LIKE ? OR label LIKE ?))`)
		taskArgs = append(taskArgs, like, like, like, like)
	}
	if o.Status != "" {
		if !validStatus(o.Status) {
			return nil, fmt.Errorf("invalid status %q", o.Status)
		}
		taskWhere = append(taskWhere, "status = ?")
		taskArgs = append(taskArgs, o.Status)
	}
	if o.HasDecisions {
		taskWhere = append(taskWhere, "id IN (SELECT task_id FROM decision)")
	}
	if len(taskWhere) > 0 {
		tasks, err := s.queryTasks(taskCols+" WHERE "+strings.Join(taskWhere, " AND ")+orderBy, taskArgs...)
		if err != nil {
			return nil, err
		}
		for _, t := range tasks {
			v, err := s.view(t)
			if err != nil {
				return nil, err
			}
			res.Tasks = append(res.Tasks, v)
		}
	}

	// Decisions matching the query and time bound.
	decWhere := []string{"1=1"}
	var decArgs []any
	if o.Query != "" {
		decWhere = append(decWhere, "(d.decision_md LIKE ? OR d.rationale_md LIKE ?)")
		decArgs = append(decArgs, like, like)
	}
	if o.Since != "" {
		decWhere = append(decWhere, "d.ts >= ?")
		decArgs = append(decArgs, o.Since)
	}
	if o.Query != "" || o.Since != "" {
		rows, err := s.db.Query(
			`SELECT d.id, t.slug, COALESCE(d.session_id,0), d.ts, d.decision_md, d.rationale_md
			 FROM decision d JOIN task t ON t.id = d.task_id
			 WHERE `+strings.Join(decWhere, " AND ")+` ORDER BY d.id DESC LIMIT 50`, decArgs...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var d Decision
			if err := rows.Scan(&d.ID, &d.TaskSlug, &d.SessionID, &d.Ts, &d.Decision, &d.Rationale); err != nil {
				rows.Close()
				return nil, err
			}
			res.Decisions = append(res.Decisions, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	// Journal matching the query and time bound.
	if o.Query != "" || o.Since != "" {
		jWhere := []string{"1=1"}
		var jArgs []any
		if o.Query != "" {
			jWhere = append(jWhere, "j.text_md LIKE ?")
			jArgs = append(jArgs, like)
		}
		if o.Since != "" {
			jWhere = append(jWhere, "j.ts >= ?")
			jArgs = append(jArgs, o.Since)
		}
		rows, err := s.db.Query(
			`SELECT j.id, COALESCE(t.slug,''), COALESCE(j.session_id,0), j.ts, j.kind, j.text_md
			 FROM journal j LEFT JOIN task t ON t.id = j.task_id
			 WHERE `+strings.Join(jWhere, " AND ")+` ORDER BY j.id DESC LIMIT 50`, jArgs...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var e JournalEntry
			if err := rows.Scan(&e.ID, &e.TaskSlug, &e.SessionID, &e.Ts, &e.Kind, &e.Text); err != nil {
				rows.Close()
				return nil, err
			}
			res.Journal = append(res.Journal, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return res, nil
}
