package store

import "fmt"

// AddDep records that task (slug) is blocked by another task (blockerSlug).
func (s *Store) AddDep(slug, blockerSlug string) error {
	t, err := s.taskBySlug(slug)
	if err != nil {
		return fmt.Errorf("task %q: %w", slug, err)
	}
	b, err := s.taskBySlug(blockerSlug)
	if err != nil {
		return fmt.Errorf("blocker %q: %w", blockerSlug, err)
	}
	if t.ID == b.ID {
		return fmt.Errorf("a task cannot block itself")
	}
	_, err = s.db.Exec(
		`INSERT OR IGNORE INTO dep (task_id, blocked_by_id) VALUES (?, ?)`, t.ID, b.ID)
	return err
}

// RemoveDep drops a blocking edge; a missing edge is not an error.
func (s *Store) RemoveDep(slug, blockerSlug string) error {
	t, err := s.taskBySlug(slug)
	if err != nil {
		return fmt.Errorf("task %q: %w", slug, err)
	}
	b, err := s.taskBySlug(blockerSlug)
	if err != nil {
		return fmt.Errorf("blocker %q: %w", blockerSlug, err)
	}
	_, err = s.db.Exec(`DELETE FROM dep WHERE task_id = ? AND blocked_by_id = ?`, t.ID, b.ID)
	return err
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
