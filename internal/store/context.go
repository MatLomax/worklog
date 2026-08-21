package store

import (
	"fmt"
	"strings"
)

// WarmContext renders the "where was I" briefing a new session opens with: the
// next actionable task, in-progress and blocked work, and the tail of the
// journal so the last session's trail is visible immediately.
func (s *Store) WarmContext(journalLimit int) (string, error) {
	var b strings.Builder
	b.WriteString("# worklog — open work\n\n")

	next, err := s.NextTask()
	if err != nil {
		return "", err
	}
	if next != nil {
		fmt.Fprintf(&b, "**Next up:** %s (`%s`, priority %d)\n\n", next.Title, next.Slug, next.Priority)
	}

	inProg, err := s.ListTasks(ListOpts{Status: "in_progress"})
	if err != nil {
		return "", err
	}
	if len(inProg) > 0 {
		b.WriteString("## In progress\n\n")
		for _, t := range inProg {
			writeTaskLine(&b, t)
		}
		b.WriteString("\n")
	}

	pending, err := s.ListTasks(ListOpts{Status: "pending"})
	if err != nil {
		return "", err
	}
	if len(pending) > 0 {
		b.WriteString("## Pending\n\n")
		for _, t := range pending {
			writeTaskLine(&b, t)
		}
		b.WriteString("\n")
	}

	blocked, err := s.blockedTasks()
	if err != nil {
		return "", err
	}
	if len(blocked) > 0 {
		b.WriteString("## Blocked\n\n")
		for _, t := range blocked {
			writeTaskLine(&b, t)
		}
		b.WriteString("\n")
	}

	journal, err := s.RecentJournal(journalLimit)
	if err != nil {
		return "", err
	}
	if len(journal) > 0 {
		b.WriteString("## Recent activity\n\n")
		for _, e := range journal {
			slug := ""
			if e.TaskSlug != "" {
				slug = " (`" + e.TaskSlug + "`)"
			}
			fmt.Fprintf(&b, "- %s [%s]%s %s\n", e.Ts[:16], e.Kind, slug, firstLine(e.Text))
		}
		b.WriteString("\n")
	}

	if next == nil && len(inProg) == 0 && len(pending) == 0 {
		b.WriteString("_No open tasks._\n")
	}
	return b.String(), nil
}

// blockedTasks returns tasks that are actively blocked (by status or by an
// unclosed dependency).
func (s *Store) blockedTasks() ([]TaskView, error) {
	all, err := s.ListTasks(ListOpts{})
	if err != nil {
		return nil, err
	}
	var out []TaskView
	for _, t := range all {
		if t.Status == "blocked" || len(t.Blockers) > 0 {
			out = append(out, t)
		}
	}
	return out, nil
}

func writeTaskLine(b *strings.Builder, t TaskView) {
	fmt.Fprintf(b, "- **%s** (`%s`)", t.Title, t.Slug)
	if len(t.Blockers) > 0 {
		fmt.Fprintf(b, " — blocked by %s", strings.Join(t.Blockers, ", "))
	}
	if t.ChildCount > 0 {
		fmt.Fprintf(b, " · %d subtasks", t.ChildCount)
	}
	b.WriteString("\n")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
