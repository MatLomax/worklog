// Package store is worklog's persistence layer: a per-project SQLite database
// (`.worklog/tasks.db`) holding the task tree, its blocking edges, and the
// durable record of what was decided and done across agent sessions.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
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
	// status is the project's status configuration with its SQL fragments;
	// SetConfig replaces it whole. nil means the default configuration.
	status *statusSet
}

const schema = `
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS task (
  id         INTEGER PRIMARY KEY,
  slug       TEXT NOT NULL UNIQUE,
  parent_id  INTEGER REFERENCES task(id) ON DELETE CASCADE,
  title      TEXT NOT NULL,
  body_md    TEXT NOT NULL DEFAULT '',
  status     TEXT NOT NULL,
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
// other. The status configuration is read from ConfigFileName beside the
// database (see LoadConfig); a config error fails the open. Pass ":memory:" for
// an ephemeral database (tests), which uses DefaultConfig.
func Open(path string) (*Store, error) {
	cfg := DefaultConfig()
	if path != ":memory:" {
		var err error
		if cfg, err = LoadConfig(filepath.Dir(path)); err != nil {
			return nil, err
		}
	}
	return OpenWithConfig(path, cfg)
}

// OpenWithConfig is Open with an explicit status configuration instead of the
// one beside the database. cfg is validated and copied, so later changes to it
// do not affect the store; nil means DefaultConfig.
func OpenWithConfig(path string, cfg *Config) (*Store, error) {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	st, err := newStatusSet(cfg)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
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
	// Migrate before applying the schema: a database predating a column brings
	// itself up to the current shape first, so the idempotent schema re-apply
	// (whose CREATE INDEX statements reference migration-added columns) never
	// runs against a table that still lacks them.
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db, status: st}, nil
}

// Config returns a copy of the status configuration the store runs with.
func (s *Store) Config() *Config { return s.statuses().cfg.clone() }

// schemaVersion is the migration level recorded in PRAGMA user_version.
const schemaVersion = 3

// migrate brings an existing database up to schemaVersion before the schema is
// applied; on a fresh (empty) database every step is a no-op and it simply
// stamps the version, leaving the schema exec that follows to build the tables.
// Migration 1 drops
// a legacy CHECK on journal.kind that rejected newer entry kinds (slug_change,
// ref_rewrite) — the kind is an internal enum written only by this package, so
// the constraint added maintenance cost without guarding against user input.
// Migration 2 adds journal.decision_id, linking a decision's mirror journal row
// back to the decision so an edit or delete of the decision can keep the mirror
// in sync instead of leaving it stale or orphaned.
// Migration 3 drops the CHECK on task.status (and its 'pending' default):
// statuses are configurable per project and validated by the app against the
// project config, so the database no longer constrains them.
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
	if err := addJournalDecisionID(db); err != nil {
		return err
	}
	if err := dropTaskStatusCheck(db); err != nil {
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

// addJournalDecisionID adds journal.decision_id to a database whose journal
// table predates it. It is a no-op on a database whose journal table already
// has the column (a fresh one, built from schema).
func addJournalDecisionID(db *sql.DB) error {
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='journal'`).Scan(&ddl); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if strings.Contains(ddl, "decision_id") {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE journal ADD COLUMN decision_id INTEGER REFERENCES decision(id) ON DELETE CASCADE`); err != nil {
		return err
	}
	_, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_journal_decision ON journal(decision_id)`)
	return err
}

// dropTaskStatusCheck rebuilds the task table without the legacy status CHECK
// and default. It is a no-op on a database with no task table (a fresh one,
// built from schema afterwards) or whose task table already lacks the CHECK.
//
// task is referenced by foreign keys from dep, link, decision, journal and
// itself, so the rebuild follows SQLite's documented procedure for schema
// changes ALTER TABLE cannot make: foreign keys are switched off on one
// dedicated connection (the pragma is per-connection and ignored inside a
// transaction) so dropping the old table neither cascades nor fails, the copy
// and rename run in one transaction, and foreign_key_check must come back
// empty before commit. The connection's foreign keys are switched back on
// before it returns to the pool; if that fails the connection is discarded
// rather than pooled, so no later query can run with enforcement off.
func dropTaskStatusCheck(db *sql.DB) (err error) {
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='task'`).Scan(&ddl); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if !strings.Contains(ddl, "CHECK (status") {
		return nil
	}

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Registered before the transaction's Rollback, so it runs after it (defers
	// are LIFO) — outside any transaction, where the pragma takes effect — and
	// before conn.Close returns the connection to the pool.
	defer func() {
		if _, onErr := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); onErr != nil {
			// Discard the connection instead of pooling it with enforcement off.
			conn.Raw(func(any) error { return driver.ErrBadConn })
			if err == nil {
				err = fmt.Errorf("re-enable foreign keys: %w", onErr)
			}
		}
	}()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}

	const cols = `id, slug, parent_id, title, body_md, status, priority, position, created_at, updated_at, closed_at`
	stmts := []string{
		`CREATE TABLE task_new (
  id         INTEGER PRIMARY KEY,
  slug       TEXT NOT NULL UNIQUE,
  parent_id  INTEGER REFERENCES task(id) ON DELETE CASCADE,
  title      TEXT NOT NULL,
  body_md    TEXT NOT NULL DEFAULT '',
  status     TEXT NOT NULL,
  priority   INTEGER NOT NULL DEFAULT 3,
  position   INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  closed_at  TEXT
)`,
		`INSERT INTO task_new (` + cols + `) SELECT ` + cols + ` FROM task`,
		`DROP TABLE task`,
		`ALTER TABLE task_new RENAME TO task`,
		`CREATE INDEX IF NOT EXISTS idx_task_parent ON task(parent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_task_status ON task(status)`,
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	if err := foreignKeyCheck(ctx, tx); err != nil {
		return fmt.Errorf("rebuild task table: %w", err)
	}
	return tx.Commit()
}

// fkViolationShown caps how many foreign_key_check rows an error lists.
const fkViolationShown = 10

// foreignKeyCheck runs PRAGMA foreign_key_check in tx and returns nil when it
// reports nothing. Otherwise the error lists the violating rows (table, rowid,
// the parent table referenced, and the foreign key's id; at most
// fkViolationShown, then a count of the rest) and how to fix them.
func foreignKeyCheck(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var shown []string
	total := 0
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int64
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return err
		}
		total++
		if len(shown) < fkViolationShown {
			id := "(no rowid)"
			if rowid.Valid {
				id = fmt.Sprintf("rowid %d", rowid.Int64)
			}
			shown = append(shown, fmt.Sprintf("%s %s -> %s (fkid %d)", table, id, parent, fkid))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if total == 0 {
		return nil
	}
	list := strings.Join(shown, "; ")
	if total > len(shown) {
		list += fmt.Sprintf("; and %d more", total-len(shown))
	}
	return fmt.Errorf("foreign key check failed: %d row(s) reference a missing parent row: %s. "+
		"These rows must be deleted or repaired (e.g. with sqlite3; PRAGMA foreign_key_check lists them all) "+
		"before worklog can upgrade the database; it was left unchanged", total, list)
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// now is the current instant in RFC3339 UTC, the format stored for every
// timestamp column.
func now() string { return time.Now().UTC().Format(time.RFC3339) }
