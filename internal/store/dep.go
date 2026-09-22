package store

import (
	"database/sql"
	"fmt"
)

// AddDep records that task (slug) is blocked by another task (blockerSlug).
func (s *Store) AddDep(slug, blockerSlug string) error {
	return s.AddDeps(slug, []string{blockerSlug})
}

// AddDeps records that task (slug) is blocked by each named task, applying the
// whole batch in one transaction: a single unresolvable, self-blocking, or
// cycle-forming slug rolls back every edge in the call, so the edge set never
// lands half-applied.
func (s *Store) AddDeps(slug string, blockers []string) error {
	if len(blockers) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, b := range blockers {
		if err := s.addDep(tx, slug, b); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// addDep resolves the edge on x, rejects a self-block and any edge that would
// close a dependency cycle, then inserts it (a duplicate edge is a no-op). It
// runs on any querier so it can join a caller's transaction — the cycle check
// then sees edges added earlier in the same batch.
func (s *Store) addDep(x querier, slug, blockerSlug string) error {
	t, err := s.scanTask(x.QueryRow(taskCols+` WHERE slug = ?`, slug))
	if err != nil {
		return fmt.Errorf("task %q: %w", slug, err)
	}
	b, err := s.scanTask(x.QueryRow(taskCols+` WHERE slug = ?`, blockerSlug))
	if err != nil {
		return fmt.Errorf("blocker %q: %w", blockerSlug, err)
	}
	if t.ID == b.ID {
		return fmt.Errorf("a task cannot block itself")
	}
	// The new edge means b blocks t; it closes a cycle exactly when t already
	// blocks b — that is, when b is already blocked by t transitively.
	cyclic, err := blockedByTransitively(x, b.ID, t.ID)
	if err != nil {
		return err
	}
	if cyclic {
		return fmt.Errorf("adding blocker %q to %q would create a dependency cycle", blockerSlug, slug)
	}
	_, err = x.Exec(
		`INSERT OR IGNORE INTO dep (task_id, blocked_by_id) VALUES (?, ?)`, t.ID, b.ID)
	return err
}

// RemoveDep drops a blocking edge; a missing edge is not an error.
func (s *Store) RemoveDep(slug, blockerSlug string) error {
	return s.RemoveDeps(slug, []string{blockerSlug})
}

// RemoveDeps drops each named blocking edge in one transaction, so an
// unresolvable slug mid-batch leaves the whole edge set untouched.
func (s *Store) RemoveDeps(slug string, blockers []string) error {
	if len(blockers) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, b := range blockers {
		if err := s.removeDep(tx, slug, b); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// removeDep resolves the edge on x and deletes it; a missing edge is a no-op.
func (s *Store) removeDep(x querier, slug, blockerSlug string) error {
	t, err := s.scanTask(x.QueryRow(taskCols+` WHERE slug = ?`, slug))
	if err != nil {
		return fmt.Errorf("task %q: %w", slug, err)
	}
	b, err := s.scanTask(x.QueryRow(taskCols+` WHERE slug = ?`, blockerSlug))
	if err != nil {
		return fmt.Errorf("blocker %q: %w", blockerSlug, err)
	}
	_, err = x.Exec(`DELETE FROM dep WHERE task_id = ? AND blocked_by_id = ?`, t.ID, b.ID)
	return err
}

// blockedByTransitively reports whether task taskID is blocked — directly or
// through a chain of blocked_by edges — by task blockerID. The recursive walk
// collects taskID's blockers and their blockers in turn (UNION dedupes, so an
// existing cycle among older edges cannot loop the query forever) and checks
// whether blockerID is among them.
func blockedByTransitively(x querier, taskID, blockerID int64) (bool, error) {
	var one int
	err := x.QueryRow(`
		WITH RECURSIVE reach(id) AS (
			SELECT blocked_by_id FROM dep WHERE task_id = ?
			UNION
			SELECT d.blocked_by_id FROM dep d JOIN reach r ON d.task_id = r.id
		)
		SELECT 1 FROM reach WHERE id = ? LIMIT 1`, taskID, blockerID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ActiveBlockers returns the tasks still blocking the given task — those
// dependency targets that are not yet closed. A done or dropped dependency no
// longer blocks and is omitted.
func (s *Store) ActiveBlockers(taskID int64) ([]Task, error) {
	rows, err := s.db.Query(
		taskCols+` INNER JOIN dep ON dep.blocked_by_id = task.id
		           WHERE dep.task_id = ? AND task.status NOT IN ('done','dropped')
		           ORDER BY task.priority, task.position, task.id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := s.scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}
