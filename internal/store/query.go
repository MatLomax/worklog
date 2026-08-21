package store

import (
	"fmt"
	"strings"
)

// TaskView is a task plus the derived facts a reader needs to decide what to do
// next: its still-active blockers and whether it has children.
type TaskView struct {
	Task
	Blockers   []string `json:"blocked_by,omitempty"`
	ChildCount int      `json:"child_count,omitempty"`
	Actionable bool     `json:"actionable"`
}

// activeBlockedExpr is a SQL predicate that is true when a task is held up,
// either by an unclosed dependency or by an explicit "blocked" status.
const activeBlockedExpr = `(
  status = 'blocked' OR EXISTS (
    SELECT 1 FROM dep d JOIN task b ON b.id = d.blocked_by_id
    WHERE d.task_id = task.id AND b.status NOT IN ('done','dropped')
  )
)`

// orderBy sorts actionable work first: unblocked before blocked, then by
// priority (1 highest), then sibling position, then id.
const orderBy = ` ORDER BY ` + activeBlockedExpr + `, priority, position, id`

// ListOpts filters ListTasks.
type ListOpts struct {
	Status        string // exact status filter; empty for any
	Parent        string // parent slug; "" for any parent
	IncludeClosed bool   // when false, done/dropped tasks are omitted
}

// ListTasks returns tasks in actionable order.
func (s *Store) ListTasks(o ListOpts) ([]TaskView, error) {
	where := []string{"1=1"}
	var args []any
	if o.Status != "" {
		if !validStatus(o.Status) {
			return nil, fmt.Errorf("invalid status %q", o.Status)
		}
		where = append(where, "status = ?")
		args = append(args, o.Status)
	} else if !o.IncludeClosed {
		where = append(where, "status NOT IN ('done','dropped')")
	}
	if o.Parent != "" {
		p, err := s.taskBySlug(o.Parent)
		if err != nil {
			return nil, fmt.Errorf("parent %q: %w", o.Parent, err)
		}
		where = append(where, "parent_id = ?")
		args = append(args, p.ID)
	}
	tasks, err := s.queryTasks(taskCols+" WHERE "+strings.Join(where, " AND ")+orderBy, args...)
	if err != nil {
		return nil, err
	}
	out := make([]TaskView, 0, len(tasks))
	for _, t := range tasks {
		v, err := s.view(t)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// queryTasks runs a taskCols query and scans every row into a Task slice.
func (s *Store) queryTasks(q string, args ...any) ([]Task, error) {
	rows, err := s.db.Query(q, args...)
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

func (s *Store) view(t Task) (TaskView, error) {
	blockers, err := s.ActiveBlockers(t.ID)
	if err != nil {
		return TaskView{}, err
	}
	slugs := make([]string, len(blockers))
	for i, b := range blockers {
		slugs[i] = b.Slug
	}
	var children int
	s.db.QueryRow(`SELECT COUNT(*) FROM task WHERE parent_id = ?`, t.ID).Scan(&children)
	actionable := (t.Status == "pending" || t.Status == "in_progress") && len(blockers) == 0
	return TaskView{Task: t, Blockers: slugs, ChildCount: children, Actionable: actionable}, nil
}

// NextTask returns the highest-priority actionable task — pending or
// in_progress, with no active blockers — or nil when nothing is actionable.
func (s *Store) NextTask() (*TaskView, error) {
	views, err := s.ListTasks(ListOpts{})
	if err != nil {
		return nil, err
	}
	for _, v := range views {
		if v.Actionable {
			vv := v
			return &vv, nil
		}
	}
	return nil, nil
}

// TreeNode is a task with its recursively-nested children.
type TreeNode struct {
	TaskView
	Children []TreeNode `json:"children,omitempty"`
}

// Tree returns the forest of top-level tasks (rootSlug == "") or the subtree
// rooted at a given slug. Every node carries closed tasks too, so the full
// history of a branch stays visible.
func (s *Store) Tree(rootSlug string) ([]TreeNode, error) {
	if rootSlug != "" {
		r, err := s.taskBySlug(rootSlug)
		if err != nil {
			return nil, err
		}
		node, err := s.treeNode(*r)
		if err != nil {
			return nil, err
		}
		return []TreeNode{node}, nil
	}
	tasks, err := s.queryTasks(taskCols + " WHERE parent_id IS NULL" + orderBy)
	if err != nil {
		return nil, err
	}
	return s.treeNodes(tasks)
}

// treeNode builds one node and recurses into its children.
func (s *Store) treeNode(t Task) (TreeNode, error) {
	v, err := s.view(t)
	if err != nil {
		return TreeNode{}, err
	}
	kids, err := s.queryTasks(taskCols+" WHERE parent_id = ?"+orderBy, t.ID)
	if err != nil {
		return TreeNode{}, err
	}
	children, err := s.treeNodes(kids)
	if err != nil {
		return TreeNode{}, err
	}
	return TreeNode{TaskView: v, Children: children}, nil
}

func (s *Store) treeNodes(tasks []Task) ([]TreeNode, error) {
	out := make([]TreeNode, 0, len(tasks))
	for _, t := range tasks {
		node, err := s.treeNode(t)
		if err != nil {
			return nil, err
		}
		out = append(out, node)
	}
	return out, nil
}
