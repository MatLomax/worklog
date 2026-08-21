package store

import (
	"fmt"
	"strings"
)

// ChildRef is a lightweight reference to a child task.
type ChildRef struct {
	Slug   string `json:"slug"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// TaskDetail is the full picture of one task: its body, what still blocks it,
// its children, and — the point of worklog — the links, decisions, and journal
// that record what was done and why.
type TaskDetail struct {
	Task
	Blockers  []string       `json:"blocked_by,omitempty"`
	Children  []ChildRef     `json:"children,omitempty"`
	Links     []Link         `json:"links,omitempty"`
	Decisions []Decision     `json:"decisions,omitempty"`
	Journal   []JournalEntry `json:"journal,omitempty"`
}

// Detail assembles the full record for one task.
func (s *Store) Detail(slug string) (*TaskDetail, error) {
	t, err := s.taskBySlug(slug)
	if err != nil {
		return nil, err
	}
	d := &TaskDetail{Task: *t}

	blockers, err := s.ActiveBlockers(t.ID)
	if err != nil {
		return nil, err
	}
	for _, b := range blockers {
		d.Blockers = append(d.Blockers, b.Slug)
	}

	kids, err := s.queryTasks(taskCols+" WHERE parent_id = ?"+orderBy, t.ID)
	if err != nil {
		return nil, err
	}
	for _, k := range kids {
		d.Children = append(d.Children, ChildRef{Slug: k.Slug, Title: k.Title, Status: k.Status})
	}

	if d.Links, err = s.Links(t.ID); err != nil {
		return nil, err
	}
	if d.Decisions, err = s.Decisions(t.ID); err != nil {
		return nil, err
	}
	if d.Journal, err = s.TaskJournal(t.ID, 15); err != nil {
		return nil, err
	}
	return d, nil
}

// Markdown renders a task detail as human-readable markdown, overlaying the
// stored links and decisions as their own sections beneath the authored body.
func (d *TaskDetail) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s  (`%s`)\n\n", d.Title, d.Slug)
	fmt.Fprintf(&b, "**status:** %s · **priority:** %d", d.Status, d.Priority)
	if len(d.Blockers) > 0 {
		fmt.Fprintf(&b, " · **blocked by:** %s", strings.Join(d.Blockers, ", "))
	}
	b.WriteString("\n\n")

	if strings.TrimSpace(d.Body) != "" {
		b.WriteString(strings.TrimRight(d.Body, "\n"))
		b.WriteString("\n\n")
	}
	if len(d.Children) > 0 {
		b.WriteString("## Subtasks\n\n")
		for _, c := range d.Children {
			fmt.Fprintf(&b, "- [%s] %s (`%s`)\n", c.Status, c.Title, c.Slug)
		}
		b.WriteString("\n")
	}
	if len(d.Links) > 0 {
		b.WriteString("## Links\n\n")
		for _, l := range d.Links {
			label := l.Label
			if label == "" {
				label = l.Kind
			}
			fmt.Fprintf(&b, "- %s: %s\n", label, l.URL)
		}
		b.WriteString("\n")
	}
	if len(d.Decisions) > 0 {
		b.WriteString("## Decisions\n\n")
		for _, dec := range d.Decisions {
			fmt.Fprintf(&b, "- **%s** — %s", dec.Ts[:10], dec.Decision)
			if dec.Rationale != "" {
				fmt.Fprintf(&b, " _(%s)_", dec.Rationale)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}
