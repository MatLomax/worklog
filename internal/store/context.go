package store

import (
	"fmt"
	"strings"
)

// Fixed section headings of the WarmContext briefing, which share the page
// with one section per configured status.
const (
	headingBlocked        = "Blocked"
	headingRecentActivity = "Recent activity"
)

// fixedSectionHeadings lists every fixed "## " heading WarmContext writes.
// Config.Validate rejects a status whose own section heading would equal one.
var fixedSectionHeadings = []string{headingBlocked, headingRecentActivity}

// leftoverHeadingSuffix marks the section of a status a task still holds but
// the config no longer lists, so its heading never equals a configured or
// fixed one and the reader sees why it is not grouped by kind.
const leftoverHeadingSuffix = " (not in config)"

// WarmContext renders the "where was I" briefing a new session opens with: the
// next actionable task, then the open work grouped by status, then the tail of
// the journal so the last session's trail is visible immediately.
//
// Open work is grouped into one section per active-kind status and then one per
// open-kind status (each in config order), then "Blocked" (tasks with a
// blocked-kind status or an unclosed dependency), then one section per status
// absent from the config that an unclosed task still holds (sorted by name). A
// dependency-blocked task appears both under its status and under Blocked. A
// leftover section's heading carries the suffix " (not in config)".
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

	open, err := s.ListTasks(ListOpts{})
	if err != nil {
		return "", err
	}
	cfg := s.statuses().cfg
	byStatus := map[string][]TaskView{}
	leftover := map[string][]TaskView{}
	var blocked []TaskView
	for _, t := range open {
		if cfg.Has(t.Status) {
			byStatus[t.Status] = append(byStatus[t.Status], t)
		} else {
			leftover[t.Status] = append(leftover[t.Status], t)
		}
		if cfg.IsBlocked(t.Status) || len(t.Blockers) > 0 {
			blocked = append(blocked, t)
		}
	}

	listed := false
	for _, name := range cfg.NamesOfKind(KindActive) {
		listed = writeSection(&b, statusHeading(name), byStatus[name]) || listed
	}
	for _, name := range cfg.NamesOfKind(KindOpen) {
		listed = writeSection(&b, statusHeading(name), byStatus[name]) || listed
	}
	writeSection(&b, headingBlocked, blocked)
	for _, name := range sortedKeys(leftover) {
		listed = writeSection(&b, statusHeading(name)+leftoverHeadingSuffix, leftover[name]) || listed
	}

	journal, err := s.RecentJournal(journalLimit)
	if err != nil {
		return "", err
	}
	if len(journal) > 0 {
		b.WriteString("## " + headingRecentActivity + "\n\n")
		for _, e := range journal {
			slug := ""
			if e.TaskSlug != "" {
				slug = " (`" + e.TaskSlug + "`)"
			}
			fmt.Fprintf(&b, "- %s [%s]%s %s\n", e.Ts[:16], e.Kind, slug, firstLine(e.Text))
		}
		b.WriteString("\n")
	}

	if next == nil && !listed {
		b.WriteString("_No open tasks._\n")
	}
	return b.String(), nil
}

// NextLine renders the single highest-priority actionable task as a one-line
// notice, for a visible session-start message. It returns "" when nothing is
// actionable, so a caller emits nothing rather than an empty banner.
func (s *Store) NextLine() (string, error) {
	next, err := s.NextTask()
	if err != nil {
		return "", err
	}
	if next == nil {
		return "", nil
	}
	// firstLine keeps the notice to a single line even if a title spans several
	// (titles are stored verbatim); the full title still reaches the model via
	// WarmContext.
	return fmt.Sprintf("worklog next task: %s (P%d)", firstLine(next.Title), next.Priority), nil
}

// writeSection writes a "## heading" section listing tasks, or nothing when
// tasks is empty; it reports whether it wrote anything.
func writeSection(b *strings.Builder, heading string, tasks []TaskView) bool {
	if len(tasks) == 0 {
		return false
	}
	b.WriteString("## " + heading + "\n\n")
	for _, t := range tasks {
		writeTaskLine(b, t)
	}
	b.WriteString("\n")
	return true
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
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}
