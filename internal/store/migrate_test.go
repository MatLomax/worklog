package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// legacyV2Schema is the schema as it stood at user_version 2, verbatim: the
// task table still constrains status to five fixed values with a 'pending'
// default.
const legacyV2Schema = `
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS task (
  id         INTEGER PRIMARY KEY,
  slug       TEXT NOT NULL UNIQUE,
  parent_id  INTEGER REFERENCES task(id) ON DELETE CASCADE,
  title      TEXT NOT NULL,
  body_md    TEXT NOT NULL DEFAULT '',
  status     TEXT NOT NULL DEFAULT 'pending'
             CHECK (status IN ('pending','in_progress','blocked','done','dropped')),
  priority   INTEGER NOT NULL DEFAULT 3,
  position   INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  closed_at  TEXT
);
CREATE INDEX IF NOT EXISTS idx_task_parent ON task(parent_id);
CREATE INDEX IF NOT EXISTS idx_task_status ON task(status);

CREATE TABLE IF NOT EXISTS dep (
  task_id       INTEGER NOT NULL REFERENCES task(id) ON DELETE CASCADE,
  blocked_by_id INTEGER NOT NULL REFERENCES task(id) ON DELETE CASCADE,
  PRIMARY KEY (task_id, blocked_by_id)
);

CREATE TABLE IF NOT EXISTS session (
  id         INTEGER PRIMARY KEY,
  started_at TEXT NOT NULL,
  ended_at   TEXT,
  agent      TEXT NOT NULL DEFAULT '',
  summary_md TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS link (
  id         INTEGER PRIMARY KEY,
  task_id    INTEGER NOT NULL REFERENCES task(id) ON DELETE CASCADE,
  kind       TEXT NOT NULL
             CHECK (kind IN ('github_issue','github_pr','commit','file','url')),
  url        TEXT NOT NULL,
  label      TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_link_task ON link(task_id);

CREATE TABLE IF NOT EXISTS decision (
  id           INTEGER PRIMARY KEY,
  task_id      INTEGER NOT NULL REFERENCES task(id) ON DELETE CASCADE,
  session_id   INTEGER REFERENCES session(id) ON DELETE SET NULL,
  ts           TEXT NOT NULL,
  decision_md  TEXT NOT NULL,
  rationale_md TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_decision_task ON decision(task_id);

CREATE TABLE IF NOT EXISTS journal (
  id          INTEGER PRIMARY KEY,
  task_id     INTEGER REFERENCES task(id) ON DELETE CASCADE,
  session_id  INTEGER REFERENCES session(id) ON DELETE SET NULL,
  ts          TEXT NOT NULL,
  kind        TEXT NOT NULL DEFAULT 'note',
  text_md     TEXT NOT NULL DEFAULT '',
  decision_id INTEGER REFERENCES decision(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_journal_task     ON journal(task_id);
CREATE INDEX IF NOT EXISTS idx_journal_ts       ON journal(ts);
CREATE INDEX IF NOT EXISTS idx_journal_decision ON journal(decision_id);

PRAGMA user_version = 2;
`

// legacyV2Seed populates every table that references task: a parent and child
// (parent_id), a closed task with closed_at, a dep edge, a link, a decision,
// and the decision's journal mirror carrying decision_id. Ids are explicit and
// non-contiguous so a renumbering copy would be caught.
const legacyV2Seed = `
INSERT INTO session (id, started_at, agent) VALUES (7, '2026-01-01T00:00:00Z', 'agent-x');
INSERT INTO task (id, slug, parent_id, title, body_md, status, priority, position, created_at, updated_at, closed_at) VALUES
  (10, 'parent',  NULL, 'Parent',  '# body', 'in_progress', 2, 0, '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z', NULL),
  (11, 'child',   10,   'Child',   '',       'blocked',     3, 1, '2026-01-01T00:00:01Z', '2026-01-02T00:00:01Z', NULL),
  (15, 'shipped', NULL, 'Shipped', 'done it','done',        1, 2, '2026-01-01T00:00:02Z', '2026-01-03T00:00:00Z', '2026-01-03T00:00:00Z');
INSERT INTO dep (task_id, blocked_by_id) VALUES (11, 15);
INSERT INTO link (id, task_id, kind, url, label, created_at) VALUES (3, 11, 'url', 'https://example.com', 'ex', '2026-01-01T00:00:03Z');
INSERT INTO decision (id, task_id, session_id, ts, decision_md, rationale_md) VALUES (4, 10, 7, '2026-01-01T00:00:04Z', 'Use SQLite', 'local');
INSERT INTO journal (id, task_id, session_id, ts, kind, text_md, decision_id) VALUES (5, 10, 7, '2026-01-01T00:00:04Z', 'decision', 'Use SQLite', 4);
`

// buildLegacyV2DB writes a user_version 2 database at path, with extra SQL
// (seed rows) applied on a connection with foreign keys as given.
func buildLegacyV2DB(t *testing.T, path string, fk bool, seed string) {
	t.Helper()
	fkv := 0
	if fk {
		fkv = 1
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("%s?_pragma=foreign_keys(%d)", path, fkv))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(legacyV2Schema); err != nil {
		t.Fatalf("build legacy v2 schema: %v", err)
	}
	if !fk {
		if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
			t.Fatalf("fk off: %v", err)
		}
	}
	if _, err := db.Exec(seed); err != nil {
		t.Fatalf("seed legacy v2 db: %v", err)
	}
}

// dumpRows renders every row of a query as strings, for exact before/after
// comparison.
func dumpRows(t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%v", vals))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

var dataQueries = []string{
	`SELECT id, slug, parent_id, title, body_md, status, priority, position, created_at, updated_at, closed_at FROM task ORDER BY id`,
	`SELECT task_id, blocked_by_id FROM dep ORDER BY task_id, blocked_by_id`,
	`SELECT id, task_id, kind, url, label, created_at FROM link ORDER BY id`,
	`SELECT id, task_id, session_id, ts, decision_md, rationale_md FROM decision ORDER BY id`,
	`SELECT id, task_id, session_id, ts, kind, text_md, decision_id FROM journal ORDER BY id`,
	`SELECT id, started_at, ended_at, agent, summary_md FROM session ORDER BY id`,
}

func dumpAll(t *testing.T, db *sql.DB) [][]string {
	t.Helper()
	var all [][]string
	for _, q := range dataQueries {
		all = append(all, dumpRows(t, db, q))
	}
	return all
}

func tableDDL(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&ddl); err != nil {
		t.Fatalf("ddl of %s: %v", name, err)
	}
	return ddl
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	return v
}

func assertNoFKViolations(t *testing.T, db *sql.DB) {
	t.Helper()
	if got := dumpRows(t, db, `PRAGMA foreign_key_check`); len(got) != 0 {
		t.Fatalf("foreign_key_check reported violations: %v", got)
	}
}

// assertFKsOnEveryConn holds n pooled connections at once — so the one the
// migration borrowed and returned is necessarily among them — and checks each
// has foreign-key enforcement on. n must not exceed the pool's max open conns.
func assertFKsOnEveryConn(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	ctx := context.Background()
	var conns []*sql.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < n; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("conn %d: %v", i, err)
		}
		conns = append(conns, c)
		var on int
		if err := c.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&on); err != nil {
			t.Fatalf("foreign_keys on conn %d: %v", i, err)
		}
		if on != 1 {
			t.Fatalf("foreign_keys = %d on pooled conn %d after Open, want 1", on, i)
		}
	}
}

// TestOpenMigratesLegacyV2TaskStatusCheck drives the real Open path over a
// user_version 2 database whose task table still carries the status CHECK, and
// proves migration 3 rebuilds task without it while every row, every foreign
// key reference into task, and foreign-key enforcement survive.
func TestOpenMigratesLegacyV2TaskStatusCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	buildLegacyV2DB(t, path, true, legacyV2Seed)

	// Snapshot the legacy data, and prove the legacy table really rejects a
	// custom status — otherwise the post-migration insert proves nothing.
	raw, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	before := dumpAll(t, raw)
	if _, err := raw.Exec(`INSERT INTO task (slug, title, status, created_at, updated_at) VALUES ('r0','R','review','t','t')`); err == nil {
		t.Fatal("legacy task table should reject status 'review' before migration")
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on legacy v2 db: %v", err)
	}
	defer s.Close()

	if v := userVersion(t, s.db); v != 3 {
		t.Fatalf("user_version = %d, want 3", v)
	}
	taskDDL := tableDDL(t, s.db, "task")
	if strings.Contains(taskDDL, "CHECK") || strings.Contains(taskDDL, "DEFAULT 'pending'") {
		t.Fatalf("task DDL still constrains status: %s", taskDDL)
	}
	// The rename must leave task's self-reference and every other table's
	// reference pointing at "task" — never at the transient task_new.
	if !strings.Contains(taskDDL, "REFERENCES task(id) ON DELETE CASCADE") {
		t.Fatalf("task self-reference lost: %s", taskDDL)
	}
	for _, tbl := range []string{"dep", "link", "decision", "journal"} {
		ddl := tableDDL(t, s.db, tbl)
		if !strings.Contains(ddl, "REFERENCES task(id) ON DELETE CASCADE") {
			t.Fatalf("%s no longer references task: %s", tbl, ddl)
		}
	}
	var leftovers int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE sql LIKE '%task_new%' OR name = 'task_new'`).Scan(&leftovers); err != nil || leftovers != 0 {
		t.Fatalf("schema still mentions task_new: n=%d err=%v", leftovers, err)
	}
	for _, idx := range []string{"idx_task_parent", "idx_task_status"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=? AND tbl_name='task'`, idx).Scan(&n); err != nil || n != 1 {
			t.Fatalf("index %s missing on task: n=%d err=%v", idx, n, err)
		}
	}

	// Every row and column (ids included) survived unchanged.
	if after := dumpAll(t, s.db); !reflect.DeepEqual(before, after) {
		t.Fatalf("data changed by migration:\nbefore %v\nafter  %v", before, after)
	}
	// Referencing rows still join to their tasks.
	var joined int
	if err := s.db.QueryRow(`
SELECT (SELECT COUNT(*) FROM dep d JOIN task a ON a.id = d.task_id JOIN task b ON b.id = d.blocked_by_id WHERE a.slug='child' AND b.slug='shipped')
     + (SELECT COUNT(*) FROM link l JOIN task t ON t.id = l.task_id WHERE t.slug='child')
     + (SELECT COUNT(*) FROM decision d JOIN task t ON t.id = d.task_id WHERE t.slug='parent')
     + (SELECT COUNT(*) FROM journal j JOIN decision d ON d.id = j.decision_id JOIN task t ON t.id = j.task_id WHERE t.slug='parent')
     + (SELECT COUNT(*) FROM task c JOIN task p ON p.id = c.parent_id WHERE c.slug='child' AND p.slug='parent')`).Scan(&joined); err != nil || joined != 5 {
		t.Fatalf("referencing rows no longer join: got %d of 5 (err %v)", joined, err)
	}
	assertNoFKViolations(t, s.db)
	assertFKsOnEveryConn(t, s.db, 4)

	// The DB no longer constrains status.
	if _, err := s.db.Exec(`INSERT INTO task (slug, title, status, created_at, updated_at) VALUES ('rev','Rev','review','t','t')`); err != nil {
		t.Fatalf("custom status rejected after migration: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM task WHERE slug='rev'`); err != nil {
		t.Fatal(err)
	}

	// Reopening is a no-op: same version, same DDL, same data.
	after := dumpAll(t, s.db)
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen migrated db: %v", err)
	}
	defer s2.Close()
	if v := userVersion(t, s2.db); v != 3 {
		t.Fatalf("user_version after reopen = %d, want 3", v)
	}
	if got := tableDDL(t, s2.db, "task"); got != taskDDL {
		t.Fatalf("task DDL changed on reopen:\n%s\nvs\n%s", got, taskDDL)
	}
	if again := dumpAll(t, s2.db); !reflect.DeepEqual(after, again) {
		t.Fatalf("data changed on reopen:\nbefore %v\nafter  %v", after, again)
	}

	// Cascades through the rebuilt table still fire: deleting the parent takes
	// the child (self-reference), the child's dep edge and link, and the
	// parent's decision and journal mirror.
	if _, err := s2.db.Exec(`DELETE FROM task WHERE slug='parent'`); err != nil {
		t.Fatalf("delete parent: %v", err)
	}
	var remaining int
	if err := s2.db.QueryRow(`
SELECT (SELECT COUNT(*) FROM task WHERE slug IN ('parent','child'))
     + (SELECT COUNT(*) FROM dep) + (SELECT COUNT(*) FROM link)
     + (SELECT COUNT(*) FROM decision) + (SELECT COUNT(*) FROM journal)`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("cascade did not remove dependents: %d rows left (err %v)", remaining, err)
	}
	var shipped int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM task WHERE slug='shipped'`).Scan(&shipped); err != nil || shipped != 1 {
		t.Fatalf("unrelated task lost: n=%d err=%v", shipped, err)
	}
}

// TestOpenLegacyV2MigrationRollsBackOnFKViolation seeds a v2 database with an
// orphaned dep row (written with enforcement off), so the rebuild's
// foreign_key_check fails: Open must error and leave the database exactly as
// it was — CHECK intact, version 2, rows untouched.
func TestOpenLegacyV2MigrationRollsBackOnFKViolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	buildLegacyV2DB(t, path, false, legacyV2Seed+`INSERT INTO dep (task_id, blocked_by_id) VALUES (11, 999);`)

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	before := dumpAll(t, raw)
	beforeDDL := tableDDL(t, raw, "task")

	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("Open should fail when the rebuilt task table has foreign key violations")
	} else {
		// The orphan is dep's second row (rowid 2), referencing a missing task.
		for _, want := range []string{"foreign key check failed", "dep rowid 2 -> task", "deleted or repaired"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not mention %q", err, want)
			}
		}
	}

	if v := userVersion(t, raw); v != 2 {
		t.Fatalf("user_version = %d after failed migration, want 2", v)
	}
	if got := tableDDL(t, raw, "task"); got != beforeDDL {
		t.Fatalf("task DDL changed despite rollback: %s", got)
	}
	var n int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='task_new'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("task_new left behind: n=%d err=%v", n, err)
	}
	if after := dumpAll(t, raw); !reflect.DeepEqual(before, after) {
		t.Fatalf("data changed despite rollback:\nbefore %v\nafter  %v", before, after)
	}
}

// TestOpenFreshDBHasUnconstrainedStatus covers the fresh-database path, in
// memory (single pooled connection, where a held Conn would deadlock) and on
// disk: the migration step no-ops, the schema builds task without a status
// CHECK, and a custom status is accepted.
func TestOpenFreshDBHasUnconstrainedStatus(t *testing.T) {
	for _, path := range []string{":memory:", filepath.Join(t.TempDir(), "tasks.db")} {
		t.Run(path, func(t *testing.T) {
			s, err := Open(path)
			if err != nil {
				t.Fatalf("Open fresh: %v", err)
			}
			defer s.Close()
			if v := userVersion(t, s.db); v != schemaVersion {
				t.Fatalf("user_version = %d, want %d", v, schemaVersion)
			}
			if ddl := tableDDL(t, s.db, "task"); strings.Contains(ddl, "CHECK") || strings.Contains(ddl, "DEFAULT 'pending'") {
				t.Fatalf("fresh task DDL constrains status: %s", ddl)
			}
			if _, err := s.db.Exec(`INSERT INTO task (slug, title, status, created_at, updated_at) VALUES ('rev','Rev','review','t','t')`); err != nil {
				t.Fatalf("custom status rejected on fresh db: %v", err)
			}
			n := 4
			if path == ":memory:" {
				n = 1 // the in-memory pool holds a single connection
			}
			assertFKsOnEveryConn(t, s.db, n)
		})
	}
}

// TestOpenLegacyV2MigrationFKErrorCapsList seeds more orphaned rows than the
// error lists: the first fkViolationShown are named and the rest counted.
func TestOpenLegacyV2MigrationFKErrorCapsList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	var seed strings.Builder
	seed.WriteString(legacyV2Seed)
	const orphans = fkViolationShown + 3
	for i := 0; i < orphans; i++ {
		fmt.Fprintf(&seed, "INSERT INTO dep (task_id, blocked_by_id) VALUES (11, %d);\n", 900+i)
	}
	buildLegacyV2DB(t, path, false, seed.String())

	s, err := Open(path)
	if err == nil {
		s.Close()
		t.Fatal("Open should fail on foreign key violations")
	}
	msg := err.Error()
	if n := strings.Count(msg, " -> task (fkid "); n != fkViolationShown {
		t.Fatalf("error lists %d rows, want %d: %s", n, fkViolationShown, msg)
	}
	for _, want := range []string{fmt.Sprintf("%d row(s)", orphans), "and 3 more", "dep rowid 2 -> task"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not mention %q", msg, want)
		}
	}
}

// TestDropTaskStatusCheckRestoresForeignKeys runs the rebuild directly — not
// via Open, whose schema exec would switch foreign keys back on regardless —
// on a single-connection pool, so the connection the rebuild borrowed is the
// one queried afterwards: it must come back with enforcement on.
func TestDropTaskStatusCheckRestoresForeignKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	buildLegacyV2DB(t, path, true, legacyV2Seed)
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var on int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&on); err != nil || on != 1 {
		t.Fatalf("foreign_keys before = %d (err %v), want 1", on, err)
	}

	if err := dropTaskStatusCheck(db); err != nil {
		t.Fatalf("dropTaskStatusCheck: %v", err)
	}
	if ddl := tableDDL(t, db, "task"); strings.Contains(ddl, "CHECK") {
		t.Fatalf("rebuild did not run: %s", ddl)
	}
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&on); err != nil {
		t.Fatal(err)
	}
	if on != 1 {
		t.Fatalf("foreign_keys = %d on the pooled connection after the rebuild, want 1", on)
	}
}
