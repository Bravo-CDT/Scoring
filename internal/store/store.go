// Package store is the scoring engine's SQLite persistence layer: every
// session, service, check result, ledger entry and event the engine records
// lives in the single database file Open creates.
//
// Concurrency: a Store holds exactly one database connection
// (SetMaxOpenConns(1)). That is deliberate and is the engine's whole
// concurrency-safety mechanism — SQLite allows only one writer at a time, so
// funnelling every statement (transactions included) through one connection
// serializes the check routines, the HTTP handlers and the engine loop against
// each other without any application-level mutex. In particular the ledger's
// read-modify-write sequences rely on it: a transaction on the only connection
// cannot interleave with another goroutine's statements. Raising the limit, or
// adding a second *sql.DB on the same file, breaks that guarantee.
//
// This package owns the complete schema. Later features add query methods on
// *Store, never CREATE TABLE statements of their own.
package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is a handle on the engine's database. It is safe for concurrent use.
type Store struct {
	db   *sql.DB
	path string
}

// schema is applied on every Open. Every statement is IF NOT EXISTS, so
// reopening an existing database is a no-op — which also means a column added
// here never reaches a table an older engine already created. There are no
// migrations (the data is disposable before competition day); Open instead
// refuses such a file, see requireCurrentSchema.
//
// Conventions: all timestamps are Unix milliseconds (INTEGER), all booleans are
// 0/1 (INTEGER), and money-like values (points, scores) are whole INTEGERs — no
// floats anywhere in the scoring path.
const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	name           TEXT NOT NULL UNIQUE,
	starting_score INTEGER NOT NULL,
	created_at     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS services (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id     INTEGER NOT NULL REFERENCES sessions(id),
	name           TEXT NOT NULL,
	active         INTEGER NOT NULL DEFAULT 1,
	scoring_paused INTEGER NOT NULL DEFAULT 0,
	created_at     INTEGER NOT NULL,
	UNIQUE(session_id, name)
);

CREATE TABLE IF NOT EXISTS service_config_snapshots (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	service_id         INTEGER NOT NULL REFERENCES services(id),
	applied_at         INTEGER NOT NULL,
	host               TEXT NOT NULL,
	check_type         TEXT NOT NULL,
	points_per_failure INTEGER NOT NULL,
	params_json        TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS check_results (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id  INTEGER NOT NULL REFERENCES sessions(id),
	service_id  INTEGER NOT NULL REFERENCES services(id),
	round       INTEGER NOT NULL DEFAULT 0,
	timestamp   INTEGER NOT NULL,
	success     INTEGER NOT NULL,
	latency_ms  INTEGER NOT NULL,
	detail      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_check_results_service_ts
	ON check_results(session_id, service_id, timestamp DESC);
-- Serves the per-service "latest" queries: its entries are (service_id, rowid),
-- so a service's newest results are read straight off its end. IF NOT EXISTS
-- adds it to an existing database on the next Open.
CREATE INDEX IF NOT EXISTS idx_check_results_service
	ON check_results(service_id);

CREATE TABLE IF NOT EXISTS ledger_entries (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id     INTEGER NOT NULL REFERENCES sessions(id),
	timestamp      INTEGER NOT NULL,
	source_type    TEXT NOT NULL,
	source_ref     INTEGER NOT NULL,
	from_side      TEXT NOT NULL,
	to_side        TEXT NOT NULL,
	nominal_points INTEGER NOT NULL,
	points         INTEGER NOT NULL,
	voided         INTEGER NOT NULL DEFAULT 0,
	voided_at      INTEGER,
	voided_reason  TEXT,
	reason         TEXT NOT NULL
);
-- One ledger entry per scoring source: the unique index is what makes writing a
-- transfer idempotent, so a retried check result cannot be scored twice.
CREATE UNIQUE INDEX IF NOT EXISTS idx_ledger_source
	ON ledger_entries(session_id, source_type, source_ref);
CREATE INDEX IF NOT EXISTS idx_ledger_session_ts
	ON ledger_entries(session_id, timestamp);

CREATE TABLE IF NOT EXISTS events (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id  INTEGER NOT NULL REFERENCES sessions(id),
	service_id  INTEGER,
	level       TEXT NOT NULL,
	actor       TEXT NOT NULL,
	type        TEXT NOT NULL,
	message     TEXT NOT NULL,
	detail      TEXT NOT NULL DEFAULT '',
	timestamp   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_session_ts
	ON events(session_id, service_id, timestamp DESC);

-- Singleton row (id = 1) holding the engine's run state; see runstate.go.
-- countdown_target is NULL when no countdown is set.
CREATE TABLE IF NOT EXISTS engine_run_state (
	id               INTEGER PRIMARY KEY CHECK (id = 1),
	engine_state     TEXT NOT NULL,
	scoring_paused   INTEGER NOT NULL,
	countdown_target INTEGER
);

-- Blue team changes to a service's check credentials, by service name; a
-- service with no row uses its config's. Passwords are plain text: the engine
-- runs on a closed network and the checks need them in the clear anyway.
CREATE TABLE IF NOT EXISTS credential_overrides (
	session_id INTEGER NOT NULL REFERENCES sessions(id),
	service    TEXT NOT NULL,
	username   TEXT NOT NULL,
	password   TEXT NOT NULL,
	changed_at INTEGER NOT NULL,
	PRIMARY KEY (session_id, service)
);

-- Injects: written tasks for one team, graded out of 10 by the grey team.
-- grade is set while grading; points and approved_at once approved, which
-- also records the inject's ledger entry (source type "inject", ref = id).
CREATE TABLE IF NOT EXISTS injects (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id  INTEGER NOT NULL REFERENCES sessions(id),
	team        TEXT NOT NULL,
	title       TEXT NOT NULL,
	pdf_name    TEXT NOT NULL DEFAULT '',
	open        INTEGER NOT NULL DEFAULT 0,
	deadline    INTEGER NOT NULL,
	created_at  INTEGER NOT NULL,
	grade       INTEGER,
	points      INTEGER,
	approved_at INTEGER
);

-- The team's latest submission for an inject; the file itself is on disk
-- under stored_name.
CREATE TABLE IF NOT EXISTS inject_submissions (
	inject_id    INTEGER PRIMARY KEY REFERENCES injects(id),
	session_id   INTEGER NOT NULL REFERENCES sessions(id),
	note         TEXT NOT NULL,
	file_name    TEXT NOT NULL,
	stored_name  TEXT NOT NULL,
	size         INTEGER NOT NULL,
	submitted_at INTEGER NOT NULL
);

-- Singleton counter backing AdjustScore's ledger source_ref, so every manual
-- adjustment gets a distinct, monotonically increasing reference.
CREATE TABLE IF NOT EXISTS manual_adjustment_seq (
	id   INTEGER PRIMARY KEY CHECK (id = 1),
	next INTEGER NOT NULL DEFAULT 1
);
`

// Open opens (creating it if needed) the database at path, applies the schema,
// and forces the run state to its safe default: engine stopped with scoring
// paused. That reset is unconditional, so an engine that crashed mid-round
// never resumes scoring by itself — an operator has to start it again.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", path, err)
	}

	// One connection serializes every statement across all goroutines; see the
	// package comment. Keeping it idle-pooled avoids reopening the file (and
	// re-running the DSN pragmas) between bursts of activity.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &Store{db: db, path: path}
	// sql.Open is lazy, so this is also where an unusable path surfaces.
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: applying schema to %s: %w", path, err)
	}
	if err := s.requireCurrentSchema(path); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.resetRunStateToSafeDefault(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// requireCurrentSchema fails when the database at path was created before
// scoring rounds: its check_results table has no round column, and every
// check result insert would fail with a raw SQL error. Saying so up front,
// with what to do about it, beats that. The round column stands for the whole
// change: engine_run_state.countdown_target arrived with it.
func (s *Store) requireCurrentSchema(path string) error {
	hasRound, err := s.hasColumn("check_results", "round")
	if err != nil {
		return fmt.Errorf("store: checking schema of %s: %w", path, err)
	}
	if !hasRound {
		return fmt.Errorf("store: %s predates scoring rounds (check_results has no round column) "+
			"and cannot be migrated; start the engine with a fresh -db path", path)
	}
	return nil
}

// hasColumn reports whether table has a column named column.
func (s *Store) hasColumn(table, column string) (bool, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count)
	return count > 0, err
}

// dsn builds the modernc.org/sqlite connection string for path. The pragmas
// travel in the DSN rather than being Exec'd once so that any connection the
// pool opens gets them, not just the first:
//
//   - journal_mode=WAL lets the dashboard's reads proceed while a check result
//     is being written.
//   - busy_timeout waits out a lock instead of failing the statement, which
//     covers the WAL checkpointer and anything else touching the file.
//   - foreign_keys turns the schema's REFERENCES clauses into real enforcement
//     (SQLite ignores them otherwise), so an insert carrying a wrong or stale
//     session_id or service_id fails immediately instead of leaving an orphan
//     row that quietly drops out of every scoring query.
func dsn(path string) string {
	u := url.URL{
		Scheme: "file",
		Opaque: (&url.URL{Path: path}).EscapedPath(),
		RawQuery: url.Values{
			"_pragma": {"journal_mode(WAL)", "busy_timeout(5000)", "foreign_keys(ON)"},
		}.Encode(),
	}
	return u.String()
}

// Close releases the database connection.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: closing database: %w", err)
	}
	return nil
}

// Backup writes a complete, transactionally-consistent snapshot of the
// database to a new file in a "backups" directory next to the database file
// (created if missing), named "<db base name>-<UTC timestamp to the millisecond>.db". It uses
// SQLite's VACUUM INTO, which produces a correct snapshot regardless of the
// WAL's state -- unlike copying the .db/-wal/-shm files directly, which
// could capture an inconsistent mid-checkpoint state. Returns the backup
// file's full path.
func (s *Store) Backup() (string, error) {
	dir := filepath.Join(filepath.Dir(s.path), "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("store: creating backups directory %s: %w", dir, err)
	}
	base := strings.TrimSuffix(filepath.Base(s.path), filepath.Ext(s.path))
	name := fmt.Sprintf("%s-%s.db", base, time.Now().UTC().Format("20060102T150405.000Z"))
	dest := filepath.Join(dir, name)
	if _, err := s.db.Exec(`VACUUM INTO ?`, dest); err != nil {
		return "", fmt.Errorf("store: backing up to %s: %w", dest, err)
	}
	return dest, nil
}
