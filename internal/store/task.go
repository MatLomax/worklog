package store

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

// Statuses are the five values a task may hold, aligned with the harness's own
// task vocabulary. "blocked" is a manual override; blocking is also computed
// from dependency edges (see ActiveBlockers).
var Statuses = []string{"pending", "in_progress", "blocked", "done", "dropped"}

// Task is one node in the work tree.
type Task struct {
	ID        int64  `json:"id"`
	Slug      string `json:"slug"`
	ParentID  int64  `json:"parent_id,omitempty"`
	Title     string `json:"title"`
	Body      string `json:"body_md,omitempty"`
	Status    string `json:"status"`
	Priority  int    `json:"priority"`
	Position  int    `json:"position"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	ClosedAt  string `json:"closed_at,omitempty"`
}

// closed reports whether a status counts as terminal — a closed task no longer
// blocks its dependents.
func closed(status string) bool { return status == "done" || status == "dropped" }

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

// slugBase normalizes text to a slug, returning "" when the input has no
// slug-able characters. slugify layers the "task" fallback on top; callers that
// must reject blank input (e.g. an explicit rename) use slugBase directly.
func slugBase(title string) string {
	s := slugStrip.ReplaceAllString(strings.ToLower(title), "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = strings.Trim(s[:60], "-")
	}
	return s
}

func slugify(title string) string {
	if s := slugBase(title); s != "" {
		return s
	}
	return "task"
}

// uniqueSlug returns base, or base-2, base-3, … until one is free.
func (s *Store) uniqueSlug(base string) (string, error) {
	slug := base
	for i := 2; ; i++ {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM task WHERE slug = ?`, slug).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return slug, nil
		}
		slug = fmt.Sprintf("%s-%d", base, i)
	}
}

// CreateTaskInput carries the fields for a new task.
type CreateTaskInput struct {
	Title     string
	Slug      string // optional; derived from Title when empty
	Parent    string // optional parent slug
	Body      string
	Status    string // optional; defaults to pending
	Priority  int    // optional; defaults to 3
	BlockedBy []string
}

// CreateTask inserts a task, optionally under a parent and with blocking edges,
// and records a "created" journal entry.
func (s *Store) CreateTask(in CreateTaskInput) (*Task, error) {
	status := in.Status
	if status == "" {
		status = "pending"
	}
	if !validStatus(status) {
		return nil, fmt.Errorf("invalid status %q", status)
	}
	priority := in.Priority
	if priority == 0 {
		priority = 3
	}
	base := in.Slug
	if base == "" {
		base = slugify(in.Title)
	} else {
		base = slugify(in.Slug)
	}
	slug, err := s.uniqueSlug(base)
	if err != nil {
		return nil, err
	}

	var parentID sql.NullInt64
	if in.Parent != "" {
		p, err := s.taskBySlug(in.Parent)
		if err != nil {
			return nil, fmt.Errorf("parent %q: %w", in.Parent, err)
		}
		parentID = sql.NullInt64{Int64: p.ID, Valid: true}
	}

	// position sorts a task after its existing siblings by default.
	var pos int
	if parentID.Valid {
		s.db.QueryRow(`SELECT COALESCE(MAX(position)+1,0) FROM task WHERE parent_id = ?`, parentID.Int64).Scan(&pos)
	} else {
		s.db.QueryRow(`SELECT COALESCE(MAX(position)+1,0) FROM task WHERE parent_id IS NULL`).Scan(&pos)
	}

	ts := now()
	res, err := s.db.Exec(
		`INSERT INTO task (slug, parent_id, title, body_md, status, priority, position, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		slug, parentID, in.Title, in.Body, status, priority, pos, ts, ts)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()

	for _, b := range in.BlockedBy {
		if err := s.AddDep(slug, b); err != nil {
			return nil, err
		}
	}
	s.journal(id, "created", "created task: "+in.Title)
	return s.GetTask(slug)
}

// GetTask returns one task by slug.
func (s *Store) GetTask(slug string) (*Task, error) { return s.taskBySlug(slug) }

func (s *Store) taskBySlug(slug string) (*Task, error) {
	return s.scanTask(s.db.QueryRow(taskCols+` WHERE slug = ?`, slug))
}

func (s *Store) taskByID(id int64) (*Task, error) {
	return s.scanTask(s.db.QueryRow(taskCols+` WHERE id = ?`, id))
}

const taskCols = `SELECT id, slug, parent_id, title, body_md, status, priority, position, created_at, updated_at, closed_at FROM task`

type scanner interface{ Scan(...any) error }

func (s *Store) scanTask(row scanner) (*Task, error) {
	var t Task
	var parent sql.NullInt64
	var closedAt sql.NullString
	err := row.Scan(&t.ID, &t.Slug, &parent, &t.Title, &t.Body, &t.Status, &t.Priority, &t.Position, &t.CreatedAt, &t.UpdatedAt, &closedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.ParentID = parent.Int64
	t.ClosedAt = closedAt.String
	return &t, nil
}

// UpdateTaskInput carries an update; nil pointers leave a field unchanged.
type UpdateTaskInput struct {
	Slug     string
	NewSlug  *string // rename the task's slug; normalized like a created slug
	Title    *string
	Status   *string
	Priority *int
	Position *int
	Parent   *string // "" detaches to root
	Body     *string
}

// UpdateTask applies a partial update. A status change to a terminal value
// stamps closed_at (and clears it when reopened), and is journaled.
func (s *Store) UpdateTask(in UpdateTaskInput) (*Task, error) {
	t, err := s.taskBySlug(in.Slug)
	if err != nil {
		return nil, err
	}
	sets := []string{"updated_at = ?"}
	args := []any{now()}

	// lookupSlug is how we re-read the task at the end; a rename changes it.
	lookupSlug := in.Slug
	var slugChange string
	if in.NewSlug != nil {
		ns := slugBase(*in.NewSlug)
		if ns == "" {
			return nil, fmt.Errorf("new slug %q has no slug-able characters", *in.NewSlug)
		}
		if ns != t.Slug {
			var n int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM task WHERE slug = ? AND id <> ?`, ns, t.ID).Scan(&n); err != nil {
				return nil, err
			}
			if n > 0 {
				return nil, fmt.Errorf("slug %q is already taken", ns)
			}
			sets = append(sets, "slug = ?")
			args = append(args, ns)
			slugChange = fmt.Sprintf("%s → %s", t.Slug, ns)
			lookupSlug = ns
		}
	}

	if in.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, *in.Title)
	}
	if in.Body != nil {
		sets = append(sets, "body_md = ?")
		args = append(args, *in.Body)
	}
	if in.Priority != nil {
		sets = append(sets, "priority = ?")
		args = append(args, *in.Priority)
	}
	if in.Position != nil {
		sets = append(sets, "position = ?")
		args = append(args, *in.Position)
	}
	if in.Parent != nil {
		if *in.Parent == "" {
			sets = append(sets, "parent_id = NULL")
		} else {
			p, err := s.taskBySlug(*in.Parent)
			if err != nil {
				return nil, fmt.Errorf("parent %q: %w", *in.Parent, err)
			}
			if p.ID == t.ID {
				return nil, fmt.Errorf("a task cannot be its own parent")
			}
			sets = append(sets, "parent_id = ?")
			args = append(args, p.ID)
		}
	}
	var statusChange string
	if in.Status != nil {
		if !validStatus(*in.Status) {
			return nil, fmt.Errorf("invalid status %q", *in.Status)
		}
		if *in.Status != t.Status {
			statusChange = fmt.Sprintf("%s → %s", t.Status, *in.Status)
		}
		sets = append(sets, "status = ?")
		args = append(args, *in.Status)
		if closed(*in.Status) {
			sets = append(sets, "closed_at = ?")
			args = append(args, now())
		} else {
			sets = append(sets, "closed_at = NULL")
		}
	}

	args = append(args, t.ID)
	if _, err := s.db.Exec(`UPDATE task SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return nil, err
	}
	if slugChange != "" {
		s.journalKind(t.ID, "slug_change", slugChange)
	}
	if statusChange != "" {
		s.journalKind(t.ID, "status_change", statusChange)
	}
	return s.taskBySlug(lookupSlug)
}

func validStatus(s string) bool {
	for _, v := range Statuses {
		if v == s {
			return true
		}
	}
	return false
}
