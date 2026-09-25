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

// heldExpr is a SQL predicate that is true when a task cannot be worked on
// now: its status is of kind blocked, or is absent from the config (a leftover,
// which is never actionable), or it has a dependency whose status is not of
// kind closed. It only orders results; Blocked-section membership and
// TaskView.Actionable are decided in Go.
func (s *Store) heldExpr() string {
	st := s.statuses()
	return `(
  status IN ` + st.blockedList + ` OR status NOT IN ` + st.configuredList + ` OR EXISTS (
    SELECT 1 FROM dep d JOIN task b ON b.id = d.blocked_by_id
    WHERE d.task_id = task.id AND b.status NOT IN ` + st.closedList + `
  )
)`
}

// orderBy sorts workable tasks first: those not held (see heldExpr) before
// held ones, then by priority (1 highest), then sibling position, then id.
func (s *Store) orderBy() string {
	return ` ORDER BY ` + s.heldExpr() + `, priority, position, id`
}

// ListOpts filters ListTasks.
type ListOpts struct {
	Status        string // exact status filter, configured or not; empty for any
	Parent        string // parent slug; "" for any parent
	IncludeClosed bool   // when false (and Status is empty), closed-kind tasks are omitted
}

// ListTasks returns tasks in actionable order.
func (s *Store) ListTasks(o ListOpts) ([]TaskView, error) {
	where := []string{"1=1"}
	var args []any
	if o.Status != "" {
		if err := checkFilter(o.Status); err != nil {
			return nil, err
		}
		where = append(where, "status = ?")
		args = append(args, o.Status)
	} else if !o.IncludeClosed {
		where = append(where, "status NOT IN "+s.statuses().closedList)
	}
	if o.Parent != "" {
		p, err := s.taskBySlug(o.Parent)
		if err != nil {
			return nil, fmt.Errorf("parent %q: %w", o.Parent, err)
		}
		where = append(where, "parent_id = ?")
		args = append(args, p.ID)
	}
	tasks, err := s.queryTasks(taskCols+" WHERE "+strings.Join(where, " AND ")+s.orderBy(), args...)
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
	actionable := s.statuses().cfg.IsActionable(t.Status) && len(blockers) == 0
	return TaskView{Task: t, Blockers: slugs, ChildCount: children, Actionable: actionable}, nil
}

// NextTask returns the highest-priority actionable task — one whose status is
// of kind open or active, with no active blockers — or nil when nothing is
// actionable.
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
	tasks, err := s.queryTasks(taskCols + " WHERE parent_id IS NULL" + s.orderBy())
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
	kids, err := s.queryTasks(taskCols+" WHERE parent_id = ?"+s.orderBy(), t.ID)
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
