package store

import (
	"fmt"
	"regexp"
)

// Link ties a task to an external artifact — most importantly a GitHub issue or
// PR by its full URL, so "which task touched issue #412" is a lookup, not a
// grep through prose.
type Link struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	URL       string `json:"url"`
	Label     string `json:"label,omitempty"`
	CreatedAt string `json:"created_at"`
}

var (
	reGitHubIssue  = regexp.MustCompile(`^https?://github\.com/[^/]+/[^/]+/issues/\d+`)
	reGitHubPR     = regexp.MustCompile(`^https?://github\.com/[^/]+/[^/]+/pull/\d+`)
	reGitHubCommit = regexp.MustCompile(`^https?://github\.com/[^/]+/[^/]+/commit/[0-9a-f]+`)
)

// DetectKind infers a link kind from a URL so callers can just pass the URL.
func DetectKind(url string) string {
	switch {
	case reGitHubIssue.MatchString(url):
		return "github_issue"
	case reGitHubPR.MatchString(url):
		return "github_pr"
	case reGitHubCommit.MatchString(url):
		return "commit"
	default:
		return "url"
	}
}

// AddLink attaches a link to a task. When kind is empty it is detected from the
// URL. The addition is journaled.
func (s *Store) AddLink(slug, url, kind, label string) (*Link, error) {
	t, err := s.taskBySlug(slug)
	if err != nil {
		return nil, err
	}
	if kind == "" {
		kind = DetectKind(url)
	}
	if !validKind(kind) {
		return nil, fmt.Errorf("invalid link kind %q", kind)
	}
	ts := now()
	res, err := s.db.Exec(
		`INSERT INTO link (task_id, kind, url, label, created_at) VALUES (?,?,?,?,?)`,
		t.ID, kind, url, label, ts)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	s.journalKind(t.ID, "link_added", fmt.Sprintf("%s: %s", kind, url))
	return &Link{ID: id, Kind: kind, URL: url, Label: label, CreatedAt: ts}, nil
}

// Links returns every link on a task, oldest first.
func (s *Store) Links(taskID int64) ([]Link, error) {
	rows, err := s.db.Query(
		`SELECT id, kind, url, label, created_at FROM link WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.ID, &l.Kind, &l.URL, &l.Label, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func validKind(k string) bool {
	switch k {
	case "github_issue", "github_pr", "commit", "file", "url":
		return true
	}
	return false
}
