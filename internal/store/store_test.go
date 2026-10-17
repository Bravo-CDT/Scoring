package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// openTemp opens a store on a throwaway database file that is removed when the
// test finishes.
func openTemp(t *testing.T) *Store {
	t.Helper()
	return openAt(t, filepath.Join(t.TempDir(), "scoring.db"))
}

// openAt opens a store on a caller-chosen path, so a test can close it and
// reopen the same file to simulate an engine restart.
func openAt(t *testing.T, path string) *Store {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return st
}

func TestOpen_CreatesSchema(t *testing.T) {
	st := openTemp(t)

	// A count against every table must succeed, proving the schema exists and
	// that the columns later tasks rely on are spelled as documented.
	queries := map[string]string{
		"sessions":                 `SELECT COUNT(*) FROM sessions WHERE name = '' AND starting_score = 0 AND created_at = 0`,
		"services":                 `SELECT COUNT(*) FROM services WHERE session_id = 0 AND name = '' AND active = 0 AND scoring_paused = 0 AND created_at = 0`,
		"service_config_snapshots": `SELECT COUNT(*) FROM service_config_snapshots WHERE service_id = 0 AND applied_at = 0 AND host = '' AND check_type = '' AND interval_seconds = 0 AND points_per_failure = 0 AND params_json = ''`,
		"check_results":            `SELECT COUNT(*) FROM check_results WHERE session_id = 0 AND service_id = 0 AND timestamp = 0 AND success = 0 AND latency_ms = 0 AND detail = '' AND round = 0`,
		"ledger_entries":           `SELECT COUNT(*) FROM ledger_entries WHERE session_id = 0 AND timestamp = 0 AND source_type = '' AND source_ref = 0 AND from_side = '' AND to_side = '' AND nominal_points = 0 AND points = 0 AND voided = 0 AND voided_at IS NULL AND voided_reason IS NULL AND reason = ''`,
		"events":                   `SELECT COUNT(*) FROM events WHERE session_id = 0 AND service_id IS NULL AND level = '' AND actor = '' AND type = '' AND message = '' AND detail = '' AND timestamp = 0`,
		"engine_run_state":         `SELECT COUNT(*) FROM engine_run_state WHERE engine_state = '' AND scoring_paused = 0 AND countdown_target IS NULL`,
	}
	for table, query := range queries {
		var count int
		if err := st.db.QueryRow(query).Scan(&count); err != nil {
			t.Errorf("querying table %s: %v", table, err)
		}
	}
}

func TestOpen_ForcesRunStateStoppedAndPaused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scoring.db")

	st1 := openAt(t, path)
	if err := st1.SetEngineState("running"); err != nil {
		t.Fatalf("SetEngineState: %v", err)
	}
	if err := st1.SetGlobalScoringPaused(false); err != nil {
		t.Fatalf("SetGlobalScoringPaused: %v", err)
	}
	if err := st1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Simulate a process restart: fresh Store instance, same file. An engine
	// that died mid-round must never resume scoring on its own.
	st2 := openAt(t, path)
	rs, err := st2.GetRunState()
	if err != nil {
		t.Fatalf("GetRunState: %v", err)
	}
	if rs.EngineState != "stopped" {
		t.Errorf("EngineState = %q after restart, want stopped", rs.EngineState)
	}
	if !rs.ScoringPaused {
		t.Error("ScoringPaused = false after restart, want true")
	}
}

func TestOpen_RefusesADatabaseFromBeforeRounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scoring.db")

	// Build the tables that changed in their pre-rounds shape, as a database
	// file written by an older engine would have them. The schema's IF NOT
	// EXISTS then leaves them alone, so without a guard the first insert of a
	// check result would fail on the missing round column.
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE check_results (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id  INTEGER NOT NULL REFERENCES sessions(id),
			service_id  INTEGER NOT NULL REFERENCES services(id),
			timestamp   INTEGER NOT NULL,
			success     INTEGER NOT NULL,
			latency_ms  INTEGER NOT NULL,
			detail      TEXT NOT NULL
		);
		CREATE TABLE engine_run_state (
			id             INTEGER PRIMARY KEY CHECK (id = 1),
			engine_state   TEXT NOT NULL,
			scoring_paused INTEGER NOT NULL
		);`)
	if err != nil {
		t.Fatalf("creating pre-rounds tables: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing setup connection: %v", err)
	}

	st, err := Open(path)
	if err == nil {
		st.Close()
		t.Fatal("Open succeeded on a pre-rounds database, want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "round") || !strings.Contains(msg, path) {
		t.Errorf("Open error = %q, want it to explain the missing round column and name %s", msg, path)
	}
	if strings.Contains(msg, "no such column") {
		t.Errorf("Open error = %q, want an explanation rather than a raw SQL error", msg)
	}
}

func TestOpen_UsesSingleConnection(t *testing.T) {
	st := openTemp(t)

	// One connection is the engine's entire concurrency-safety mechanism: it
	// serializes every statement, transactions included, across goroutines.
	if got := st.db.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1", got)
	}
}

func TestOpen_EnforcesForeignKeys(t *testing.T) {
	st := openTemp(t)

	var foreignKeys bool
	if err := st.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if !foreignKeys {
		t.Error("foreign_keys = off, want on")
	}

	// SQLite ignores REFERENCES clauses unless the pragma is on, so prove
	// enforcement with a real dangling reference: a check result for a service
	// that does not exist must be rejected rather than silently stored.
	sess, err := st.GetOrCreateSession("comp-1", 1000)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	_, err = st.db.Exec(
		`INSERT INTO check_results (session_id, service_id, timestamp, success, latency_ms, detail)
		 VALUES (?, ?, 0, 1, 0, '')`,
		sess.ID, 424242,
	)
	if err == nil {
		t.Error("inserting a check result for a nonexistent service succeeded, want foreign-key violation")
	}
}

func TestOpen_SetsWALAndBusyTimeout(t *testing.T) {
	st := openTemp(t)

	var journalMode string
	if err := st.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	var busyTimeout int
	if err := st.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if busyTimeout == 0 {
		t.Error("busy_timeout = 0, want a non-zero timeout")
	}
}

func TestOpen_UsesTheGivenPathVerbatim(t *testing.T) {
	// The pragmas ride along in a file: URI, so the path has to be escaped into
	// it: "#" and "?" would otherwise be read as a fragment or query and the
	// engine would quietly score into the wrong file.
	dir := t.TempDir()
	for _, name := range []string{"scoring.db", "my db.db", "a#b?c.db", "a&b=c.db"} {
		path := filepath.Join(dir, name)
		st, err := Open(path)
		if err != nil {
			t.Errorf("Open(%q): %v", path, err)
			continue
		}
		if _, err := st.GetOrCreateSession("comp-1", 1000); err != nil {
			t.Errorf("GetOrCreateSession on %q: %v", path, err)
		}
		if err := st.Close(); err != nil {
			t.Errorf("Close on %q: %v", path, err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("no database at %q: %v", path, err)
		}
	}
}

func TestOpen_UnusablePathFails(t *testing.T) {
	// Parent directory does not exist, so the database file cannot be created.
	path := filepath.Join(t.TempDir(), "missing-dir", "scoring.db")

	st, err := Open(path)
	if err == nil {
		st.Close()
		t.Fatal("Open succeeded on an uncreatable path, want error")
	}
}

func TestStore_SerializesConcurrentWrites(t *testing.T) {
	st := openTemp(t)

	// Hammer the single connection from many goroutines. With SetMaxOpenConns(1)
	// every statement is serialized, so none of these may fail with a lock or
	// busy error.
	const goroutines, perGoroutine = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*perGoroutine)
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range perGoroutine {
				if _, err := st.GetOrCreateSession("comp", 1000); err != nil {
					errs <- err
				}
				if err := st.SetGlobalScoringPaused(i%2 == 0); err != nil {
					errs <- err
				}
				if _, err := st.GetRunState(); err != nil {
					errs <- err
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent store use: %v", err)
	}

	var sessions int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("counting sessions: %v", err)
	}
	if sessions != 1 {
		t.Errorf("sessions row count = %d, want 1", sessions)
	}
}
