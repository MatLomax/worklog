// Package store is worklog's persistence layer: a per-project SQLite database
// (`.worklog/tasks.db`) holding the task tree, its blocking edges, and the
// durable record of what was decided and done across agent sessions.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// DirName is the per-project directory that holds the database.
const DirName = ".worklog"

// FileName is the database file within DirName.
const FileName = "tasks.db"

// ErrNotFound is returned when a task or other record does not exist.
var ErrNotFound = errors.New("not found")

// Store is a handle to one project's worklog database.
type Store struct {
	db        *sql.DB
	sessionID int64 // the current agent session, attributed to writes; 0 if none
}

const schema = `
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
  id         INTEGER PRIMARY KEY,
  task_id    INTEGER REFERENCES task(id) ON DELETE CASCADE,
  session_id INTEGER REFERENCES session(id) ON DELETE SET NULL,
  ts         TEXT NOT NULL,
  kind       TEXT NOT NULL DEFAULT 'note',
  text_md    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_journal_task ON journal(task_id);
CREATE INDEX IF NOT EXISTS idx_journal_ts   ON journal(ts);
`

// Resolve returns the database path for the project containing dir, walking up
// from dir to find an existing DirName; if none is found it returns the path
// dir/DirName/FileName so a fresh database is created in place.
func Resolve(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	cur := abs
	for {
		cand := filepath.Join(cur, DirName)
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return filepath.Join(cand, FileName), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break // reached filesystem root
		}
		cur = parent
	}
	return filepath.Join(abs, DirName, FileName), nil
}

// Open opens (creating if needed) the database at path, applies the schema, and
// enables WAL so concurrent agent sessions sharing the tree do not clobber each
// other. Pass ":memory:" for an ephemeral database (tests).
func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	// busy_timeout lets a writer wait out another session's lock instead of
	// failing immediately; foreign_keys must be set per-connection.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	if path != ":memory:" {
		dsn += "&_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single in-memory database only survives on one connection.
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// schemaVersion is the migration level recorded in PRAGMA user_version.
const schemaVersion = 1

// migrate brings an existing database up to schemaVersion; a fresh one already
// matches the current schema and only has its version stamped. Migration 1 drops
// a legacy CHECK on journal.kind that rejected newer entry kinds (slug_change,
// ref_rewrite) — the kind is an internal enum written only by this package, so
// the constraint added maintenance cost without guarding against user input.
func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v >= schemaVersion {
		return nil
	}
	if err := dropJournalKindCheck(db); err != nil {
		return err
	}
	_, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion))
	return err
}

// dropJournalKindCheck rebuilds the journal table without the legacy kind CHECK.
// It is a no-op on a database whose journal table already lacks it (a fresh one).
func dropJournalKindCheck(db *sql.DB) error {
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='journal'`).Scan(&ddl); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if !strings.Contains(ddl, "CHECK (kind") {
		return nil
	}
	// journal is a leaf table (nothing references it), so a straight rebuild is
	// safe with foreign keys enforced.
	stmts := []string{
		`CREATE TABLE journal_new (
  id         INTEGER PRIMARY KEY,
  task_id    INTEGER REFERENCES task(id) ON DELETE CASCADE,
  session_id INTEGER REFERENCES session(id) ON DELETE SET NULL,
  ts         TEXT NOT NULL,
  kind       TEXT NOT NULL DEFAULT 'note',
  text_md    TEXT NOT NULL DEFAULT ''
)`,
		`INSERT INTO journal_new (id, task_id, session_id, ts, kind, text_md)
   SELECT id, task_id, session_id, ts, kind, text_md FROM journal`,
		`DROP TABLE journal`,
		`ALTER TABLE journal_new RENAME TO journal`,
		`CREATE INDEX IF NOT EXISTS idx_journal_task ON journal(task_id)`,
		`CREATE INDEX IF NOT EXISTS idx_journal_ts ON journal(ts)`,
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// now is the current instant in RFC3339 UTC, the format stored for every
// timestamp column.
func now() string { return time.Now().UTC().Format(time.RFC3339) }
