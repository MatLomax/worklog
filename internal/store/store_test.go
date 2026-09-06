package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustCreate(t *testing.T, s *Store, in CreateTaskInput) *Task {
	t.Helper()
	task, err := s.CreateTask(in)
	if err != nil {
		t.Fatalf("create %q: %v", in.Title, err)
	}
	return task
}

func TestCreateAndSlugUniqueness(t *testing.T) {
	s := newStore(t)
	a := mustCreate(t, s, CreateTaskInput{Title: "Do the thing"})
	if a.Slug != "do-the-thing" {
		t.Fatalf("slug = %q, want do-the-thing", a.Slug)
	}
	b := mustCreate(t, s, CreateTaskInput{Title: "Do the thing"})
	if b.Slug != "do-the-thing-2" {
		t.Fatalf("second slug = %q, want do-the-thing-2", b.Slug)
	}
	if a.Status != "pending" || a.Priority != 3 {
		t.Fatalf("defaults wrong: status=%q priority=%d", a.Status, a.Priority)
	}
}

func TestParentChildTree(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Root"})
	mustCreate(t, s, CreateTaskInput{Title: "Child A", Parent: "root"})
	mustCreate(t, s, CreateTaskInput{Title: "Child B", Parent: "root"})

	tree, err := s.Tree("")
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	if len(tree) != 1 {
		t.Fatalf("top-level count = %d, want 1", len(tree))
	}
	if len(tree[0].Children) != 2 {
		t.Fatalf("children = %d, want 2", len(tree[0].Children))
	}
	if tree[0].ChildCount != 2 {
		t.Fatalf("child_count = %d, want 2", tree[0].ChildCount)
	}
}

func TestBlockingAndDoneUnblocks(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Blocker"})
	mustCreate(t, s, CreateTaskInput{Title: "Dependent", BlockedBy: []string{"blocker"}})

	// While the blocker is open, "next" is the blocker, and the dependent is
	// not actionable.
	next, err := s.NextTask()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if next == nil || next.Slug != "blocker" {
		t.Fatalf("next = %v, want blocker", next)
	}
	views, _ := s.ListTasks(ListOpts{})
	for _, v := range views {
		if v.Slug == "dependent" && v.Actionable {
			t.Fatal("dependent should not be actionable while blocked")
		}
	}

	// Closing the blocker unblocks the dependent.
	if _, err := s.UpdateTask(UpdateTaskInput{Slug: "blocker", Status: ptr("done")}); err != nil {
		t.Fatalf("update: %v", err)
	}
	next, _ = s.NextTask()
	if next == nil || next.Slug != "dependent" {
		t.Fatalf("after close, next = %v, want dependent", next)
	}
	d, _ := s.Detail("dependent")
	if len(d.Blockers) != 0 {
		t.Fatalf("dependent still blocked by %v", d.Blockers)
	}
}

func TestStatusChangeStampsCloseAndJournals(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Task"})
	up, err := s.UpdateTask(UpdateTaskInput{Slug: "task", Status: ptr("done")})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if up.ClosedAt == "" {
		t.Fatal("closed_at not stamped on done")
	}
	reopened, _ := s.UpdateTask(UpdateTaskInput{Slug: "task", Status: ptr("in_progress")})
	if reopened.ClosedAt != "" {
		t.Fatal("closed_at not cleared on reopen")
	}
	d, _ := s.Detail("task")
	var sawStatusChange bool
	for _, e := range d.Journal {
		if e.Kind == "status_change" {
			sawStatusChange = true
		}
	}
	if !sawStatusChange {
		t.Fatal("no status_change journal entry")
	}
}

func TestLinkKindDetection(t *testing.T) {
	cases := map[string]string{
		"https://github.com/org/repo/issues/412":    "github_issue",
		"https://github.com/org/repo/pull/7":        "github_pr",
		"https://github.com/org/repo/commit/abc123": "commit",
		"https://example.com/whatever":              "url",
	}
	for url, want := range cases {
		if got := DetectKind(url); got != want {
			t.Errorf("DetectKind(%q) = %q, want %q", url, got, want)
		}
	}

	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "T"})
	l, err := s.AddLink("t", "https://github.com/org/repo/issues/412", "", "issue 412")
	if err != nil {
		t.Fatalf("add link: %v", err)
	}
	if l.Kind != "github_issue" {
		t.Fatalf("kind = %q, want github_issue", l.Kind)
	}
}

func TestDecisionsRecordedAndRendered(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Choose storage"})
	if _, err := s.AddDecision("choose-storage", "Use SQLite, not shared Postgres", "local, single-writer, zero-install"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	d, _ := s.Detail("choose-storage")
	if len(d.Decisions) != 1 {
		t.Fatalf("decisions = %d, want 1", len(d.Decisions))
	}
	md := d.Markdown()
	if !strings.Contains(md, "## Decisions") || !strings.Contains(md, "Use SQLite") {
		t.Fatalf("decision not rendered in markdown:\n%s", md)
	}
}

func TestSections(t *testing.T) {
	s := newStore(t)
	body := "## Design\ntext1\n### Storage\ntext2\n## Other\ntext3\n"
	mustCreate(t, s, CreateTaskInput{Title: "Doc", Body: body})

	toc, _ := s.TOC("doc")
	var paths []string
	for _, sec := range toc {
		paths = append(paths, sec.Path)
	}
	want := []string{"Design", "Design/Storage", "Other"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("toc paths = %v, want %v", paths, want)
	}

	sub, err := s.GetSection("doc", "Design/Storage")
	if err != nil {
		t.Fatalf("get section: %v", err)
	}
	if strings.TrimSpace(sub) != "text2" {
		t.Fatalf("Design/Storage = %q, want text2", sub)
	}

	parent, _ := s.GetSection("doc", "Design")
	if !strings.Contains(parent, "text1") || !strings.Contains(parent, "text2") {
		t.Fatalf("Design section should include subtree, got %q", parent)
	}

	if _, err := s.SetSection("doc", "Design/Storage", "NEWTEXT"); err != nil {
		t.Fatalf("set section: %v", err)
	}
	after, _ := s.GetSection("doc", "Design/Storage")
	if !strings.Contains(after, "NEWTEXT") {
		t.Fatalf("section not updated, got %q", after)
	}
	full, _ := s.GetTask("doc")
	if !strings.Contains(full.Body, "text3") || !strings.Contains(full.Body, "text1") {
		t.Fatal("SetSection clobbered other sections")
	}
}

func TestFind(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Migrate auth to Postgres"})
	mustCreate(t, s, CreateTaskInput{Title: "Unrelated work"})
	s.AddDecision("migrate-auth-to-postgres", "Adopt connection pooling", "avoid exhausting connections")

	res, err := s.Find(FindOpts{Query: "postgres"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(res.Tasks) != 1 || res.Tasks[0].Slug != "migrate-auth-to-postgres" {
		t.Fatalf("find tasks = %v, want the migrate task", res.Tasks)
	}

	res2, _ := s.Find(FindOpts{Query: "pooling"})
	if len(res2.Decisions) != 1 {
		t.Fatalf("find decisions = %d, want 1", len(res2.Decisions))
	}

	res3, _ := s.Find(FindOpts{HasDecisions: true})
	if len(res3.Tasks) != 1 || res3.Tasks[0].Slug != "migrate-auth-to-postgres" {
		t.Fatalf("has_decisions = %v, want the migrate task", res3.Tasks)
	}
}

func TestWarmContext(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Active task"})
	s.UpdateTask(UpdateTaskInput{Slug: "active-task", Status: ptr("in_progress")})
	out, err := s.WarmContext(10)
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	if !strings.Contains(out, "In progress") || !strings.Contains(out, "Active task") {
		t.Fatalf("warm context missing in-progress task:\n%s", out)
	}
}

func TestNextLine(t *testing.T) {
	s := newStore(t)
	// No tasks: no line.
	if line, err := s.NextLine(); err != nil || line != "" {
		t.Fatalf("empty store: got %q, err %v; want empty", line, err)
	}
	// A dependent blocked by open groundwork is not actionable; the groundwork is,
	// so the line names the groundwork, not the blocked dependent.
	a := mustCreate(t, s, CreateTaskInput{Title: "Groundwork"})
	mustCreate(t, s, CreateTaskInput{Title: "Depends on groundwork", BlockedBy: []string{a.Slug}})
	line, err := s.NextLine()
	if err != nil {
		t.Fatalf("next line: %v", err)
	}
	if !strings.HasPrefix(line, "worklog next task: ") || !strings.Contains(line, "Groundwork") {
		t.Fatalf("next line = %q, want the actionable groundwork task", line)
	}
	// Closing the groundwork unblocks the dependent, which then becomes next.
	if _, err := s.UpdateTask(UpdateTaskInput{Slug: a.Slug, Status: ptr("done")}); err != nil {
		t.Fatalf("close groundwork: %v", err)
	}
	line, err = s.NextLine()
	if err != nil {
		t.Fatalf("next line after unblock: %v", err)
	}
	if !strings.Contains(line, "Depends on groundwork") {
		t.Fatalf("next line = %q, want the now-unblocked dependent", line)
	}
}

func TestResolveWalksUp(t *testing.T) {
	dir := t.TempDir()
	// No .worklog anywhere: resolves to dir/.worklog/tasks.db.
	got, err := Resolve(dir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := filepath.Join(dir, DirName, FileName)
	if got != want {
		t.Fatalf("resolve = %q, want %q", got, want)
	}
}

func ptr[T any](v T) *T { return &v }
