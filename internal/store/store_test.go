package store

import (
	"database/sql"
	"errors"
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

func TestEditRecordDecisionAndJournal(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Choose storage"})
	d, err := s.AddDecision("choose-storage", "Use SQLite", "local, single-writer")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	j, err := s.AddJournal("choose-storage", "started investigating")
	if err != nil {
		t.Fatalf("journal: %v", err)
	}

	journalCountBefore := journalRowCount(t, s)

	if err := s.EditRecord("decision", d.ID, "decision", "Use SQLite, not Postgres"); err != nil {
		t.Fatalf("edit decision field: %v", err)
	}
	if err := s.EditRecord("decision", d.ID, "rationale", "local, single-writer, zero-install"); err != nil {
		t.Fatalf("edit rationale field: %v", err)
	}
	if err := s.EditRecord("journal", j.ID, "text", "started investigating storage options"); err != nil {
		t.Fatalf("edit journal field: %v", err)
	}

	decisions, err := s.Decisions(1)
	if err != nil {
		t.Fatalf("decisions: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Decision != "Use SQLite, not Postgres" || decisions[0].Rationale != "local, single-writer, zero-install" {
		t.Fatalf("decision not updated: %+v", decisions)
	}

	entries, err := s.TaskJournal(1, 0)
	if err != nil {
		t.Fatalf("task journal: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.ID == j.ID {
			found = true
			if e.Text != "started investigating storage options" {
				t.Fatalf("journal text not updated: %q", e.Text)
			}
		}
	}
	if !found {
		t.Fatal("edited journal entry not found via TaskJournal")
	}

	recent, err := s.RecentJournal(0)
	if err != nil {
		t.Fatalf("recent journal: %v", err)
	}
	found = false
	for _, e := range recent {
		if e.ID == j.ID && e.Text == "started investigating storage options" {
			found = true
		}
	}
	if !found {
		t.Fatal("edited journal entry not found via RecentJournal")
	}

	// EditRecord must not create a new journal entry — an in-place correction
	// is deliberately silent.
	if got := journalRowCount(t, s); got != journalCountBefore {
		t.Fatalf("journal row count changed from %d to %d after EditRecord", journalCountBefore, got)
	}
}

func TestEditRecordErrors(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Choose storage"})
	d, err := s.AddDecision("choose-storage", "Use SQLite", "local")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}

	if err := s.EditRecord("widget", d.ID, "decision", "x"); err == nil {
		t.Fatal("unknown kind should error")
	}
	if err := s.EditRecord("decision", d.ID, "nope", "x"); err == nil {
		t.Fatal("unknown field should error")
	}
	if err := s.EditRecord("decision", 99999, "decision", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nonexistent id: got %v, want ErrNotFound", err)
	}
}

func TestDeleteRecord(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Choose storage"})
	d, err := s.AddDecision("choose-storage", "Use SQLite", "local")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	j, err := s.AddJournal("choose-storage", "a note")
	if err != nil {
		t.Fatalf("journal: %v", err)
	}

	if err := s.DeleteRecord("decision", d.ID); err != nil {
		t.Fatalf("delete decision: %v", err)
	}
	decisions, _ := s.Decisions(1)
	if len(decisions) != 0 {
		t.Fatalf("decision still present after delete: %+v", decisions)
	}

	if err := s.DeleteRecord("journal", j.ID); err != nil {
		t.Fatalf("delete journal: %v", err)
	}
	entries, _ := s.TaskJournal(1, 0)
	for _, e := range entries {
		if e.ID == j.ID {
			t.Fatalf("journal entry still present after delete: %+v", e)
		}
	}

	if err := s.DeleteRecord("widget", d.ID); err == nil {
		t.Fatal("unknown kind should error")
	}
	if err := s.DeleteRecord("decision", 99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nonexistent id: got %v, want ErrNotFound", err)
	}
}

func TestEditRecordDecisionCascadesToJournalMirror(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Choose storage"})
	d, err := s.AddDecision("choose-storage", "Use SQLite", "local, single-writer")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}

	journalCountBefore := journalRowCount(t, s)

	if err := s.EditRecord("decision", d.ID, "decision", "Adopt Postgres"); err != nil {
		t.Fatalf("edit decision field: %v", err)
	}

	recent, err := s.RecentJournal(0)
	if err != nil {
		t.Fatalf("recent journal: %v", err)
	}
	var sawNew, sawOld bool
	for _, e := range recent {
		if e.Kind != "decision" {
			continue
		}
		if e.Text == "Adopt Postgres" {
			sawNew = true
		}
		if e.Text == "Use SQLite" {
			sawOld = true
		}
	}
	if !sawNew {
		t.Fatal("mirror journal entry not updated to new decision text")
	}
	if sawOld {
		t.Fatal("mirror journal entry still carries the old decision text")
	}

	// work-find must not surface the stale mirror text, and must surface the new one.
	stale, err := s.Find(FindOpts{Query: "Use SQLite"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(stale.Journal) != 0 {
		t.Fatalf("find surfaced stale mirror text: %+v", stale.Journal)
	}
	fresh, err := s.Find(FindOpts{Query: "Adopt Postgres"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(fresh.Journal) != 1 {
		t.Fatalf("find journal = %d, want 1 mirror entry with the new text", len(fresh.Journal))
	}

	// The cascade is an in-place UPDATE, not a new journal event.
	if got := journalRowCount(t, s); got != journalCountBefore {
		t.Fatalf("journal row count changed from %d to %d after cascading edit", journalCountBefore, got)
	}
}

func TestEditRecordRationaleDoesNotTouchJournalMirror(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Choose storage"})
	d, err := s.AddDecision("choose-storage", "Use SQLite", "local, single-writer")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}

	if err := s.EditRecord("decision", d.ID, "rationale", "zero-install too"); err != nil {
		t.Fatalf("edit rationale field: %v", err)
	}

	recent, err := s.RecentJournal(0)
	if err != nil {
		t.Fatalf("recent journal: %v", err)
	}
	var mirrorText string
	for _, e := range recent {
		if e.Kind == "decision" {
			mirrorText = e.Text
		}
	}
	if mirrorText != "Use SQLite" {
		t.Fatalf("mirror text = %q after editing rationale, want unchanged %q", mirrorText, "Use SQLite")
	}
}

func TestDeleteRecordDecisionRemovesJournalMirror(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, CreateTaskInput{Title: "Choose storage"})
	d, err := s.AddDecision("choose-storage", "Use SQLite", "local")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	// An unrelated freeform note must survive the decision's deletion.
	if _, err := s.AddJournal("choose-storage", "unrelated note"); err != nil {
		t.Fatalf("journal: %v", err)
	}

	if err := s.DeleteRecord("decision", d.ID); err != nil {
		t.Fatalf("delete decision: %v", err)
	}

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM journal WHERE decision_id = ?`, d.ID).Scan(&n); err != nil {
		t.Fatalf("count linked journal rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("mirror journal row still present after decision delete: n=%d", n)
	}

	recent, err := s.RecentJournal(0)
	if err != nil {
		t.Fatalf("recent journal: %v", err)
	}
	var sawDecisionKind, sawNote bool
	for _, e := range recent {
		if e.Kind == "decision" {
			sawDecisionKind = true
		}
		if e.Text == "unrelated note" {
			sawNote = true
		}
	}
	if sawDecisionKind {
		t.Fatal("kind='decision' journal row survived the decision's deletion")
	}
	if !sawNote {
		t.Fatal("unrelated journal note was wrongly removed alongside the decision's mirror")
	}
}

func TestFreshSchemaAlreadyHasJournalDecisionID(t *testing.T) {
	s := newStore(t)
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('journal') WHERE name='decision_id'`).Scan(&n); err != nil {
		t.Fatalf("check column: %v", err)
	}
	if n != 1 {
		t.Fatal("fresh schema missing journal.decision_id")
	}
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("user_version = %d (err %v), want %d", v, err, schemaVersion)
	}
}

func TestMigrateAddsJournalDecisionID(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "tasks.db") + "?_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer db.Close()

	// Start from the full current schema (so task/session/decision exist for the
	// journal's foreign keys), then swap journal back to its pre-migration-2
	// form: no decision_id column. Stamp user_version at 1, as if migration 1
	// (the kind CHECK drop) had already run but migration 2 had not.
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("base schema: %v", err)
	}
	legacy := `DROP TABLE journal;
CREATE TABLE journal (
  id         INTEGER PRIMARY KEY,
  task_id    INTEGER REFERENCES task(id) ON DELETE CASCADE,
  session_id INTEGER REFERENCES session(id) ON DELETE SET NULL,
  ts         TEXT NOT NULL,
  kind       TEXT NOT NULL DEFAULT 'note',
  text_md    TEXT NOT NULL DEFAULT ''
);`
	if _, err := db.Exec(legacy); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("stamp v1: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO journal (ts, kind, text_md) VALUES ('t','note','keep me')`); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	// The legacy table actually lacks the column before migrating — otherwise
	// this test would pass without proving the migration did anything.
	if _, err := db.Exec(`INSERT INTO journal (ts, kind, text_md, decision_id) VALUES ('t','decision','x',1)`); err == nil {
		t.Fatal("legacy journal should reject decision_id before migration")
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Data survived, and the column now exists and is writable.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM journal WHERE text_md='keep me'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("seeded row lost: n=%d err=%v", n, err)
	}
	if _, err := db.Exec(`INSERT INTO journal (ts, kind, text_md, decision_id) VALUES ('t','decision','x',NULL)`); err != nil {
		t.Fatalf("decision_id column missing after migrate: %v", err)
	}
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("user_version = %d (err %v), want %d", v, err, schemaVersion)
	}
	// Re-running is a no-op: the ALTER must not run again against a column that
	// now already exists.
	if err := migrate(db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	// End to end through the store API, on the migrated database: add, cascade
	// edit, and cascade delete all work exactly as on a fresh database.
	s := &Store{db: db}
	mustCreate(t, s, CreateTaskInput{Title: "Choose storage"})
	d, err := s.AddDecision("choose-storage", "Use SQLite", "local")
	if err != nil {
		t.Fatalf("decide on migrated db: %v", err)
	}
	if err := s.EditRecord("decision", d.ID, "decision", "Adopt Postgres"); err != nil {
		t.Fatalf("edit on migrated db: %v", err)
	}
	recent, err := s.RecentJournal(0)
	if err != nil {
		t.Fatalf("recent journal on migrated db: %v", err)
	}
	var sawNew bool
	for _, e := range recent {
		if e.Kind == "decision" && e.Text == "Adopt Postgres" {
			sawNew = true
		}
	}
	if !sawNew {
		t.Fatal("cascading edit did not work on a migrated database")
	}
	if err := s.DeleteRecord("decision", d.ID); err != nil {
		t.Fatalf("delete on migrated db: %v", err)
	}
	var linked int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM journal WHERE decision_id = ?`, d.ID).Scan(&linked); err != nil || linked != 0 {
		t.Fatalf("mirror not removed on migrated db: n=%d err=%v", linked, err)
	}
}

func journalRowCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM journal`).Scan(&n); err != nil {
		t.Fatalf("count journal rows: %v", err)
	}
	return n
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

func TestMigrateDropsLegacyJournalKindCheck(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "tasks.db") + "?_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer db.Close()

	// Start from the full current schema (so task/session exist for the journal's
	// foreign keys), then swap journal back to its pre-migration form: the old
	// kind CHECK, plus a seeded row.
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("base schema: %v", err)
	}
	legacy := `DROP TABLE journal;
CREATE TABLE journal (
  id         INTEGER PRIMARY KEY,
  task_id    INTEGER REFERENCES task(id) ON DELETE CASCADE,
  session_id INTEGER REFERENCES session(id) ON DELETE SET NULL,
  ts         TEXT NOT NULL,
  kind       TEXT NOT NULL DEFAULT 'note'
             CHECK (kind IN ('note','status_change','link_added','decision','created')),
  text_md    TEXT NOT NULL DEFAULT ''
);`
	if _, err := db.Exec(legacy); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO journal (ts, kind, text_md) VALUES ('t','note','keep me')`); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	// The legacy CHECK must actually reject the new kind — otherwise this test
	// would pass without proving the migration did anything.
	if _, err := db.Exec(`INSERT INTO journal (ts, kind, text_md) VALUES ('t','slug_change','x')`); err == nil {
		t.Fatal("legacy CHECK should reject slug_change before migration")
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Data survived the table rebuild.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM journal WHERE text_md='keep me'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("seeded row lost: n=%d err=%v", n, err)
	}
	// The kinds that the CHECK used to reject now insert.
	for _, k := range []string{"slug_change", "ref_rewrite"} {
		if _, err := db.Exec(`INSERT INTO journal (ts, kind, text_md) VALUES ('t', ?, 'x')`, k); err != nil {
			t.Fatalf("kind %q still rejected after migrate: %v", k, err)
		}
	}
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("user_version = %d (err %v), want %d", v, err, schemaVersion)
	}
	// Re-running is a no-op.
	if err := migrate(db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }
