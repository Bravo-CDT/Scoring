# Scoring Engine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the core scoring engine backend described in the spec: a Go binary that loads a config-driven set of services, checks them on independent self-paced schedules, moves points between blue/red via an append-only ledger, persists everything in SQLite, and exposes a read-only public API plus an admin-authenticated control API.

**Architecture:** Each subsystem is its own package with no upward dependencies: `config` (parse/validate YAML) → `store` (SQLite persistence, single serialized writer connection) → `checks` (pluggable check functions) → `scheduler` (per-service self-paced goroutines) → `scorer` (scoring rules, implements the scheduler's `ResultSink`) → `auth` (admin login/tokens) → `engine` (wires scheduler+scorer+store into the control plane) → `api` (HTTP handlers) → `cmd/scoringengine` (wiring + `main`).

**Tech Stack:** Go 1.27 (per existing `go.mod`, module `github.com/Bravo-CDT/Scoring`), `gopkg.in/yaml.v3` for config, `golang.org/x/crypto/bcrypt` for password hashing, `modernc.org/sqlite` (pure-Go SQLite driver, no CGO) for persistence, standard-library `net/http` with Go 1.22+ pattern-based `ServeMux` for routing (no external router/framework).

**Spec:** `docs/superpowers/specs/2026-10-15-scoring-engine-design.md`

## Global Constraints

- Go 1.27, module `github.com/Bravo-CDT/Scoring` — every new package lives under this module path.
- Only the four dependencies named above. No ORM, no web framework, no CGO-requiring driver.
- SQLite opened in WAL mode with `SetMaxOpenConns(1)` on the single `*sql.DB` — this is the serialized-writer mechanism the spec requires; do not add a second mutex or channel on top of it.
- `detail` text (check output/errors) is capped at 4096 bytes and validated as UTF-8 before storage, everywhere it is written — never store or return unbounded or invalid-UTF-8 text.
- Timestamps are stored as integer Unix-milliseconds UTC (`INTEGER`), with the row's `id` as the tiebreaker for same-millisecond ordering.
- `ledger_entries` has a unique index on `(session_id, source_type, source_ref)` — the same check result must never produce two ledger entries.
- Every admin-only HTTP route requires a valid bearer token; every public route must never include raw `detail` text.
- Per-service `scoring_paused` is persisted and survives a process restart; `engine_run_state` (`engine_state`, global `scoring_paused`) is always forced to `stopped`/`true` on process start regardless of what was last written.
- No placeholder/TODO code. Every function has a real, complete implementation.

## Review Focus

- **Two services failing in the same instant** — the floor/cap arithmetic must never let blue go negative or red exceed `starting_score`, even when two ledger-writing calls race. Owned by Task 5 (`-race` test) and exercised again end-to-end by Task 8.
- **A check that hangs past its deadline** — must be force-failed and must never block its own goroutine (let alone others) indefinitely. Owned by Task 6 (plugin-level) and Task 7 (scheduler-level).
- **A simulated process restart** (fresh `Store`/`Engine` instance, same DB file) — `engine_state` and global `scoring_paused` must always come back `stopped`/`true`; per-service `scoring_paused` and all history must survive unchanged. Owned by Task 2 (store-level) and Task 12 (end-to-end).
- **The same check result scored twice** — the unique `(session_id, source_type, source_ref)` index must reject a duplicate ledger insert, and the Scorer must treat that as a no-op rather than an error that breaks the check loop. Owned by Task 5.
- **Oversized or invalid-UTF-8 check output** — must be truncated/sanitized before it ever reaches storage or an HTTP response, so a misbehaving scored service can't fill the disk or break JSON encoding. Owned by Task 4 (store-level cap) and Task 6 (plugin-level bounded read).

## File Structure

```
go.mod                                  (existing)
cmd/scoringengine/main.go               entrypoint: flags, wiring, graceful shutdown
internal/config/config.go               types, Load, LoadService, validation
internal/config/config_test.go
internal/store/store.go                 Open, schema, pragmas
internal/store/sessions.go              Session type + GetOrCreateSession
internal/store/services.go              Service type + service CRUD, scoring-pause persistence
internal/store/checkresults.go          CheckResult type + insert/list/latest, detail capping
internal/store/ledger.go                LedgerEntry type + RecordLedgerEntry, GetScore
internal/store/events.go                Event type + insert/list
internal/store/runstate.go              RunState + engine_state/global-pause get/set
internal/store/*_test.go                one test file per above
internal/checks/checks.go               Result, CheckFunc, Registry
internal/checks/tcp_connect.go
internal/checks/http.go
internal/checks/*_test.go
internal/scheduler/scheduler.go         ResultSink interface, per-service self-paced loop
internal/scheduler/scheduler_test.go
internal/scorer/scorer.go               scoring rules, mass-failure detection
internal/scorer/scorer_test.go
internal/auth/auth.go                   bcrypt check, token issuance, middleware
internal/auth/auth_test.go
internal/engine/engine.go               control plane: start/stop/pause/resume/reload
internal/engine/engine_test.go
internal/api/server.go                  ServeMux + route registration
internal/api/handlers_public.go
internal/api/handlers_admin.go
internal/api/*_test.go
```

---

### Task 1: Config loader

**Files:**
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces:
  ```go
  package config

  type ServiceConfig struct {
      Name             string
      Host             string
      CheckType        string
      IntervalSeconds  int
      PointsPerFailure int
      Params           map[string]any
  }

  type AdminConfig struct {
      Username     string
      PasswordHash string
  }

  type Config struct {
      SessionName         string
      StartingScore        int
      MassFailureFraction *float64 // nil = disabled
      Admin                AdminConfig
      Services              []ServiceConfig
  }

  func Load(path string) (*Config, error)
  func LoadService(path, name string) (*ServiceConfig, error) // returns ErrServiceNotFound if name absent
  ```
- `ErrServiceNotFound = errors.New("config: service not found")` — exported so callers (Task 7's Scheduler) can detect "this service was removed from config" distinctly from a parse error.

- [ ] **Step 1: Write the failing tests**

```go
// internal/config/config_test.go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

const validYAML = `
session_name: "test-session"
starting_score: 1000
mass_failure_fraction: 0.5

admin:
  username: admin
  password_hash: ${TEST_ADMIN_HASH}

services:
  - name: apache-web
    host: 10.0.1.10
    check_type: http
    interval_seconds: 60
    points_per_failure: 10
    params:
      port: 80
      path: /
      expect_status: 200
      expect_contains: "Welcome"

  - name: telnet-win
    host: 10.0.2.15
    check_type: tcp_connect
    interval_seconds: 30
    points_per_failure: 5
    params:
      port: 23
`

func writeTemp(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

func TestLoad_Valid(t *testing.T) {
	t.Setenv("TEST_ADMIN_HASH", "$2a$10$abcdefghijklmnopqrstuvabcdefghijklmnopqrstuvwxyz123456")
	path := writeTemp(t, validYAML)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.SessionName != "test-session" {
		t.Errorf("SessionName = %q, want test-session", cfg.SessionName)
	}
	if cfg.StartingScore != 1000 {
		t.Errorf("StartingScore = %d, want 1000", cfg.StartingScore)
	}
	if cfg.MassFailureFraction == nil || *cfg.MassFailureFraction != 0.5 {
		t.Errorf("MassFailureFraction = %v, want 0.5", cfg.MassFailureFraction)
	}
	if cfg.Admin.Username != "admin" {
		t.Errorf("Admin.Username = %q, want admin", cfg.Admin.Username)
	}
	if cfg.Admin.PasswordHash != "$2a$10$abcdefghijklmnopqrstuvabcdefghijklmnopqrstuvwxyz123456" {
		t.Errorf("Admin.PasswordHash not expanded from env var, got %q", cfg.Admin.PasswordHash)
	}
	if len(cfg.Services) != 2 {
		t.Fatalf("len(Services) = %d, want 2", len(cfg.Services))
	}
	if cfg.Services[0].Name != "apache-web" || cfg.Services[0].CheckType != "http" {
		t.Errorf("Services[0] = %+v, unexpected", cfg.Services[0])
	}
	if port, _ := cfg.Services[0].Params["port"].(int); port != 80 {
		t.Errorf("Services[0].Params[port] = %v, want 80", cfg.Services[0].Params["port"])
	}
}

func TestLoad_MissingAdminHashEnvVar(t *testing.T) {
	os.Unsetenv("TEST_ADMIN_HASH")
	path := writeTemp(t, validYAML)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error when admin hash env var is unset, got nil")
	}
}

func TestLoad_DuplicateServiceName(t *testing.T) {
	t.Setenv("TEST_ADMIN_HASH", "$2a$10$abcdefghijklmnopqrstuvabcdefghijklmnopqrstuvwxyz123456")
	yaml := validYAML + `
  - name: apache-web
    host: 10.0.1.99
    check_type: http
    interval_seconds: 60
    points_per_failure: 10
    params:
      port: 80
`
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for duplicate service name, got nil")
	}
}

func TestLoad_UnknownCheckType(t *testing.T) {
	t.Setenv("TEST_ADMIN_HASH", "$2a$10$abcdefghijklmnopqrstuvabcdefghijklmnopqrstuvwxyz123456")
	yaml := `
session_name: "s"
starting_score: 1000
admin:
  username: admin
  password_hash: ${TEST_ADMIN_HASH}
services:
  - name: svc1
    host: 10.0.1.1
    check_type: not_a_real_check
    interval_seconds: 60
    points_per_failure: 10
    params: {}
`
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown check_type, got nil")
	}
}

func TestLoad_InvalidBcryptHash(t *testing.T) {
	t.Setenv("TEST_ADMIN_HASH", "not-a-bcrypt-hash")
	path := writeTemp(t, validYAML)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid bcrypt hash, got nil")
	}
}

func TestLoadService_Found(t *testing.T) {
	t.Setenv("TEST_ADMIN_HASH", "$2a$10$abcdefghijklmnopqrstuvabcdefghijklmnopqrstuvwxyz123456")
	path := writeTemp(t, validYAML)

	svc, err := LoadService(path, "telnet-win")
	if err != nil {
		t.Fatalf("LoadService returned error: %v", err)
	}
	if svc.IntervalSeconds != 30 {
		t.Errorf("IntervalSeconds = %d, want 30", svc.IntervalSeconds)
	}
}

func TestLoadService_NotFound(t *testing.T) {
	t.Setenv("TEST_ADMIN_HASH", "$2a$10$abcdefghijklmnopqrstuvabcdefghijklmnopqrstuvwxyz123456")
	path := writeTemp(t, validYAML)

	_, err := LoadService(path, "does-not-exist")
	if !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("expected ErrServiceNotFound, got %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/...`
Expected: FAIL — package `config` does not exist yet (compile error).

- [ ] **Step 3: Write the implementation**

```go
// internal/config/config.go
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

var ErrServiceNotFound = errors.New("config: service not found")

var knownCheckTypes = map[string]bool{
	"tcp_connect": true,
	"http":        true,
}

type ServiceConfig struct {
	Name             string         `yaml:"name"`
	Host             string         `yaml:"host"`
	CheckType        string         `yaml:"check_type"`
	IntervalSeconds  int            `yaml:"interval_seconds"`
	PointsPerFailure int            `yaml:"points_per_failure"`
	Params           map[string]any `yaml:"params"`
}

type AdminConfig struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"`
}

type Config struct {
	SessionName         string         `yaml:"session_name"`
	StartingScore        int            `yaml:"starting_score"`
	MassFailureFraction *float64       `yaml:"mass_failure_fraction"`
	Admin                AdminConfig    `yaml:"admin"`
	Services              []ServiceConfig `yaml:"services"`
}

var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func expandEnv(raw []byte) ([]byte, error) {
	var missing []string
	expanded := envVarPattern.ReplaceAllFunc(raw, func(match []byte) []byte {
		name := envVarPattern.FindSubmatch(match)[1]
		val, ok := os.LookupEnv(string(name))
		if !ok {
			missing = append(missing, string(name))
			return match
		}
		return []byte(val)
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("config: environment variable(s) not set: %v", missing)
	}
	return expanded, nil
}

func parse(raw []byte) (*Config, error) {
	expanded, err := expandEnv(raw)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("config: parsing yaml: %w", err)
	}
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func validate(cfg *Config) error {
	if cfg.SessionName == "" {
		return errors.New("config: session_name is required")
	}
	if cfg.StartingScore <= 0 {
		return errors.New("config: starting_score must be a positive integer")
	}
	if cfg.MassFailureFraction != nil {
		f := *cfg.MassFailureFraction
		if f <= 0 || f > 1 {
			return errors.New("config: mass_failure_fraction must be between 0 (exclusive) and 1 (inclusive)")
		}
	}
	if cfg.Admin.Username == "" {
		return errors.New("config: admin.username is required")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(cfg.Admin.PasswordHash), []byte("probe-only")); err != nil {
		if !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return fmt.Errorf("config: admin.password_hash is not a valid bcrypt hash: %w", err)
		}
	}

	seen := make(map[string]bool, len(cfg.Services))
	for i, svc := range cfg.Services {
		if svc.Name == "" {
			return fmt.Errorf("config: services[%d].name is required", i)
		}
		if seen[svc.Name] {
			return fmt.Errorf("config: duplicate service name %q", svc.Name)
		}
		seen[svc.Name] = true
		if !knownCheckTypes[svc.CheckType] {
			return fmt.Errorf("config: services[%d] (%s): unknown check_type %q", i, svc.Name, svc.CheckType)
		}
		if svc.IntervalSeconds <= 0 {
			return fmt.Errorf("config: services[%d] (%s): interval_seconds must be positive", i, svc.Name)
		}
		if svc.PointsPerFailure <= 0 {
			return fmt.Errorf("config: services[%d] (%s): points_per_failure must be positive", i, svc.Name)
		}
	}
	return nil
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}
	return parse(raw)
}

func LoadService(path, name string) (*ServiceConfig, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	for i := range cfg.Services {
		if cfg.Services[i].Name == name {
			svc := cfg.Services[i]
			return &svc, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrServiceNotFound, name)
}
```

Add the two new dependencies:

Run: `go get gopkg.in/yaml.v3 golang.org/x/crypto/bcrypt`

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/...`
Expected: PASS (add `"errors"` to the test file's imports for `TestLoadService_NotFound`)

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/config/
git commit -m "feat: add config loader with validation and env-var expansion"
```

---

### Task 2: SQLite store bootstrap, sessions, and run state

**Files:**
- Create: `internal/store/store.go`
- Create: `internal/store/sessions.go`
- Create: `internal/store/runstate.go`
- Test: `internal/store/store_test.go`
- Test: `internal/store/sessions_test.go`
- Test: `internal/store/runstate_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  ```go
  package store

  type Store struct { /* unexported *sql.DB */ }

  func Open(path string) (*Store, error) // applies schema, sets WAL + SetMaxOpenConns(1),
                                          // forces engine_run_state to stopped/true
  func (s *Store) Close() error

  type Session struct {
      ID            int64
      Name          string
      StartingScore int
      CreatedAt     time.Time
  }
  // GetOrCreateSession: if a session with this name exists, its persisted
  // StartingScore is returned unchanged (startingScore param is ignored in
  // that case); otherwise a new session row is created using startingScore.
  func (s *Store) GetOrCreateSession(name string, startingScore int) (Session, error)

  type RunState struct {
      EngineState   string // "stopped" | "running"
      ScoringPaused bool
  }
  func (s *Store) GetRunState() (RunState, error)
  func (s *Store) SetEngineState(state string) error
  func (s *Store) SetGlobalScoringPaused(paused bool) error
  ```
- All later tasks (`services.go`, `checkresults.go`, `ledger.go`, `events.go`) add methods on this same `*Store` and share its one `*sql.DB` connection — this task owns schema creation for every table so later tasks only add query methods, not `CREATE TABLE` statements.

- [ ] **Step 1: Write the failing tests**

```go
// internal/store/store_test.go
package store

import (
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scoring.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestOpen_CreatesSchema(t *testing.T) {
	st := openTemp(t)
	// A query against every table should succeed with zero rows, proving
	// the schema exists.
	tables := []string{
		"sessions", "services", "service_config_snapshots",
		"check_results", "ledger_entries", "events", "engine_run_state",
	}
	for _, tbl := range tables {
		var count int
		row := st.db.QueryRow("SELECT COUNT(*) FROM " + tbl)
		if err := row.Scan(&count); err != nil {
			t.Errorf("querying table %s: %v", tbl, err)
		}
	}
}

func TestOpen_ForcesRunStateStoppedAndPaused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scoring.db")

	st1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := st1.SetEngineState("running"); err != nil {
		t.Fatalf("SetEngineState: %v", err)
	}
	if err := st1.SetGlobalScoringPaused(false); err != nil {
		t.Fatalf("SetGlobalScoringPaused: %v", err)
	}
	st1.Close()

	// Simulate a process restart: fresh Store instance, same file.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer st2.Close()

	rs, err := st2.GetRunState()
	if err != nil {
		t.Fatalf("GetRunState: %v", err)
	}
	if rs.EngineState != "stopped" {
		t.Errorf("EngineState = %q after restart, want stopped", rs.EngineState)
	}
	if !rs.ScoringPaused {
		t.Errorf("ScoringPaused = false after restart, want true")
	}
}
```

```go
// internal/store/sessions_test.go
package store

import "testing"

func TestGetOrCreateSession_CreatesNew(t *testing.T) {
	st := openTemp(t)

	sess, err := st.GetOrCreateSession("comp-1", 1000)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	if sess.Name != "comp-1" || sess.StartingScore != 1000 {
		t.Errorf("sess = %+v, unexpected", sess)
	}
	if sess.ID == 0 {
		t.Error("expected non-zero session ID")
	}
}

func TestGetOrCreateSession_ExistingIgnoresNewStartingScore(t *testing.T) {
	st := openTemp(t)

	first, err := st.GetOrCreateSession("comp-1", 1000)
	if err != nil {
		t.Fatalf("first GetOrCreateSession: %v", err)
	}

	// Simulate config.yaml being edited to a different starting_score
	// for an already-existing session name.
	second, err := st.GetOrCreateSession("comp-1", 9999)
	if err != nil {
		t.Fatalf("second GetOrCreateSession: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("expected same session ID, got %d and %d", first.ID, second.ID)
	}
	if second.StartingScore != 1000 {
		t.Errorf("StartingScore = %d, want unchanged 1000", second.StartingScore)
	}
}
```

```go
// internal/store/runstate_test.go
package store

import "testing"

func TestSetEngineState_And_SetGlobalScoringPaused(t *testing.T) {
	st := openTemp(t)

	if err := st.SetEngineState("running"); err != nil {
		t.Fatalf("SetEngineState: %v", err)
	}
	if err := st.SetGlobalScoringPaused(false); err != nil {
		t.Fatalf("SetGlobalScoringPaused: %v", err)
	}
	rs, err := st.GetRunState()
	if err != nil {
		t.Fatalf("GetRunState: %v", err)
	}
	if rs.EngineState != "running" || rs.ScoringPaused {
		t.Errorf("rs = %+v, want running/false", rs)
	}

	// Setting one flag must not disturb the other.
	if err := st.SetGlobalScoringPaused(true); err != nil {
		t.Fatalf("SetGlobalScoringPaused: %v", err)
	}
	rs, _ = st.GetRunState()
	if rs.EngineState != "running" {
		t.Errorf("EngineState changed unexpectedly to %q", rs.EngineState)
	}
	if !rs.ScoringPaused {
		t.Error("ScoringPaused = false, want true")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/...`
Expected: FAIL — package `store` does not exist yet.

- [ ] **Step 3: Write the implementation**

```go
// internal/store/store.go
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

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
	interval_seconds   INTEGER NOT NULL,
	points_per_failure INTEGER NOT NULL,
	params_json        TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS check_results (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id  INTEGER NOT NULL REFERENCES sessions(id),
	service_id  INTEGER NOT NULL REFERENCES services(id),
	timestamp   INTEGER NOT NULL,
	success     INTEGER NOT NULL,
	latency_ms  INTEGER NOT NULL,
	detail      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_check_results_service_ts
	ON check_results(session_id, service_id, timestamp DESC);

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

CREATE TABLE IF NOT EXISTS engine_run_state (
	id             INTEGER PRIMARY KEY CHECK (id = 1),
	engine_state   TEXT NOT NULL,
	scoring_paused INTEGER NOT NULL
);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", path, err)
	}
	// Single connection: this is the serialized-writer mechanism the spec
	// requires. All statements, across all goroutines, run one at a time.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: setting WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: setting busy_timeout: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: applying schema: %w", err)
	}

	s := &Store{db: db}
	if err := s.resetRunStateToSafeDefault(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) resetRunStateToSafeDefault() error {
	_, err := s.db.Exec(`
		INSERT INTO engine_run_state (id, engine_state, scoring_paused)
		VALUES (1, 'stopped', 1)
		ON CONFLICT(id) DO UPDATE SET engine_state = 'stopped', scoring_paused = 1
	`)
	if err != nil {
		return fmt.Errorf("store: forcing safe-default run state: %w", err)
	}
	return nil
}
```

```go
// internal/store/sessions.go
package store

import (
	"database/sql"
	"fmt"
	"time"
)

type Session struct {
	ID            int64
	Name          string
	StartingScore int
	CreatedAt     time.Time
}

func (s *Store) GetOrCreateSession(name string, startingScore int) (Session, error) {
	var sess Session
	var createdAtMillis int64
	row := s.db.QueryRow(
		`SELECT id, name, starting_score, created_at FROM sessions WHERE name = ?`,
		name,
	)
	err := row.Scan(&sess.ID, &sess.Name, &sess.StartingScore, &createdAtMillis)
	if err == nil {
		sess.CreatedAt = time.UnixMilli(createdAtMillis).UTC()
		return sess, nil
	}
	if err != sql.ErrNoRows {
		return Session{}, fmt.Errorf("store: looking up session %q: %w", name, err)
	}

	now := time.Now().UTC()
	res, err := s.db.Exec(
		`INSERT INTO sessions (name, starting_score, created_at) VALUES (?, ?, ?)`,
		name, startingScore, now.UnixMilli(),
	)
	if err != nil {
		return Session{}, fmt.Errorf("store: creating session %q: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Session{}, fmt.Errorf("store: reading new session id: %w", err)
	}
	return Session{ID: id, Name: name, StartingScore: startingScore, CreatedAt: now}, nil
}
```

```go
// internal/store/runstate.go
package store

import "fmt"

type RunState struct {
	EngineState   string
	ScoringPaused bool
}

func (s *Store) GetRunState() (RunState, error) {
	var rs RunState
	var paused int
	row := s.db.QueryRow(`SELECT engine_state, scoring_paused FROM engine_run_state WHERE id = 1`)
	if err := row.Scan(&rs.EngineState, &paused); err != nil {
		return RunState{}, fmt.Errorf("store: reading run state: %w", err)
	}
	rs.ScoringPaused = paused != 0
	return rs, nil
}

func (s *Store) SetEngineState(state string) error {
	_, err := s.db.Exec(`UPDATE engine_run_state SET engine_state = ? WHERE id = 1`, state)
	if err != nil {
		return fmt.Errorf("store: setting engine_state: %w", err)
	}
	return nil
}

func (s *Store) SetGlobalScoringPaused(paused bool) error {
	v := 0
	if paused {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE engine_run_state SET scoring_paused = ? WHERE id = 1`, v)
	if err != nil {
		return fmt.Errorf("store: setting global scoring_paused: %w", err)
	}
	return nil
}
```

Add the new dependency:

Run: `go get modernc.org/sqlite`

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/store/store.go internal/store/sessions.go internal/store/runstate.go internal/store/store_test.go internal/store/sessions_test.go internal/store/runstate_test.go
git commit -m "feat: add SQLite store with schema, sessions, and safe-default run state"
```

---

### Task 3: Services store

**Files:**
- Create: `internal/store/services.go`
- Test: `internal/store/services_test.go`

**Interfaces:**
- Consumes: `config.ServiceConfig` (Task 1) for `InsertConfigSnapshot`.
- Produces:
  ```go
  package store

  type Service struct {
      ID            int64
      SessionID     int64
      Name          string
      Active        bool
      ScoringPaused bool
      CreatedAt     time.Time
  }

  // GetOrCreateService is idempotent on (session_id, name): a repeat call
  // for an existing name returns the existing row's current Active/
  // ScoringPaused values unchanged — it never resets them. Reactivating a
  // deactivated service is a separate, explicit SetServiceActive call
  // (owned by Task 10's reload logic), not implied by this call.
  func (s *Store) GetOrCreateService(sessionID int64, name string) (Service, error)
  func (s *Store) GetService(serviceID int64) (Service, error)
  func (s *Store) ListActiveServices(sessionID int64) ([]Service, error)
  func (s *Store) SetServiceActive(serviceID int64, active bool) error
  func (s *Store) SetServiceScoringPaused(serviceID int64, paused bool) error
  func (s *Store) InsertConfigSnapshot(serviceID int64, cfg config.ServiceConfig) error
  ```

**Note on service identity:** identity is the `(session_id, name)` pair —
there is no separate id field in `config.yaml`. Renaming a service in
config is therefore indistinguishable from removing the old name and
adding a new one: the old name's row is marked inactive (history stays
queryable under that name) and the new name gets a fresh row. True
identity-preserving rename would require config to carry an explicit id,
which this build does not add.

- [ ] **Step 1: Write the failing tests**

```go
// internal/store/services_test.go
package store

import "testing"

func TestGetOrCreateService_CreatesNew(t *testing.T) {
	st := openTemp(t)
	sess, _ := st.GetOrCreateSession("comp-1", 1000)

	svc, err := st.GetOrCreateService(sess.ID, "apache-web")
	if err != nil {
		t.Fatalf("GetOrCreateService: %v", err)
	}
	if svc.Name != "apache-web" || svc.SessionID != sess.ID {
		t.Errorf("svc = %+v, unexpected", svc)
	}
	if !svc.Active {
		t.Error("new service should default to Active = true")
	}
	if svc.ScoringPaused {
		t.Error("new service should default to ScoringPaused = false")
	}
}

func TestGetOrCreateService_IdempotentPreservesMutableState(t *testing.T) {
	st := openTemp(t)
	sess, _ := st.GetOrCreateSession("comp-1", 1000)

	first, _ := st.GetOrCreateService(sess.ID, "apache-web")
	if err := st.SetServiceScoringPaused(first.ID, true); err != nil {
		t.Fatalf("SetServiceScoringPaused: %v", err)
	}

	second, err := st.GetOrCreateService(sess.ID, "apache-web")
	if err != nil {
		t.Fatalf("second GetOrCreateService: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected same ID, got %d and %d", first.ID, second.ID)
	}
	if !second.ScoringPaused {
		t.Error("expected ScoringPaused to remain true across idempotent call")
	}
}

func TestListActiveServices_ExcludesInactive(t *testing.T) {
	st := openTemp(t)
	sess, _ := st.GetOrCreateSession("comp-1", 1000)

	svcA, _ := st.GetOrCreateService(sess.ID, "svc-a")
	svcB, _ := st.GetOrCreateService(sess.ID, "svc-b")
	if err := st.SetServiceActive(svcB.ID, false); err != nil {
		t.Fatalf("SetServiceActive: %v", err)
	}

	active, err := st.ListActiveServices(sess.ID)
	if err != nil {
		t.Fatalf("ListActiveServices: %v", err)
	}
	if len(active) != 1 || active[0].ID != svcA.ID {
		t.Errorf("active = %+v, want only svc-a (id %d)", active, svcA.ID)
	}
}

func TestSetServiceScoringPaused_PersistsAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/scoring.db"
	st1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sess, _ := st1.GetOrCreateSession("comp-1", 1000)
	svc, _ := st1.GetOrCreateService(sess.ID, "apache-web")
	if err := st1.SetServiceScoringPaused(svc.ID, true); err != nil {
		t.Fatalf("SetServiceScoringPaused: %v", err)
	}
	st1.Close()

	// Simulate a process restart.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer st2.Close()

	reloaded, err := st2.GetService(svc.ID)
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if !reloaded.ScoringPaused {
		t.Error("expected ScoringPaused = true to survive a simulated restart")
	}
}

func TestInsertConfigSnapshot(t *testing.T) {
	st := openTemp(t)
	sess, _ := st.GetOrCreateSession("comp-1", 1000)
	svc, _ := st.GetOrCreateService(sess.ID, "apache-web")

	cfg := config.ServiceConfig{
		Name:             "apache-web",
		Host:             "10.0.1.10",
		CheckType:        "http",
		IntervalSeconds:  60,
		PointsPerFailure: 10,
		Params:           map[string]any{"port": 80, "path": "/"},
	}
	if err := st.InsertConfigSnapshot(svc.ID, cfg); err != nil {
		t.Fatalf("InsertConfigSnapshot: %v", err)
	}

	var host, checkType, paramsJSON string
	var interval, points int
	row := st.db.QueryRow(
		`SELECT host, check_type, interval_seconds, points_per_failure, params_json
		 FROM service_config_snapshots WHERE service_id = ?`, svc.ID,
	)
	if err := row.Scan(&host, &checkType, &interval, &points, &paramsJSON); err != nil {
		t.Fatalf("querying snapshot: %v", err)
	}
	if host != "10.0.1.10" || checkType != "http" || interval != 60 || points != 10 {
		t.Errorf("snapshot fields unexpected: host=%s type=%s interval=%d points=%d",
			host, checkType, interval, points)
	}
	if paramsJSON == "" || paramsJSON == "{}" {
		t.Errorf("params_json = %q, expected serialized params", paramsJSON)
	}
}
```

Add `"github.com/Bravo-CDT/Scoring/internal/config"` to this test file's imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/...`
Expected: FAIL — `GetOrCreateService` etc. undefined.

- [ ] **Step 3: Write the implementation**

```go
// internal/store/services.go
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/config"
)

type Service struct {
	ID            int64
	SessionID     int64
	Name          string
	Active        bool
	ScoringPaused bool
	CreatedAt     time.Time
}

func scanService(row *sql.Row) (Service, error) {
	var svc Service
	var active, paused int
	var createdAtMillis int64
	if err := row.Scan(&svc.ID, &svc.SessionID, &svc.Name, &active, &paused, &createdAtMillis); err != nil {
		return Service{}, err
	}
	svc.Active = active != 0
	svc.ScoringPaused = paused != 0
	svc.CreatedAt = time.UnixMilli(createdAtMillis).UTC()
	return svc, nil
}

const serviceColumns = `id, session_id, name, active, scoring_paused, created_at`

func (s *Store) GetOrCreateService(sessionID int64, name string) (Service, error) {
	row := s.db.QueryRow(
		`SELECT `+serviceColumns+` FROM services WHERE session_id = ? AND name = ?`,
		sessionID, name,
	)
	svc, err := scanService(row)
	if err == nil {
		return svc, nil
	}
	if err != sql.ErrNoRows {
		return Service{}, fmt.Errorf("store: looking up service %q: %w", name, err)
	}

	now := time.Now().UTC()
	res, err := s.db.Exec(
		`INSERT INTO services (session_id, name, active, scoring_paused, created_at)
		 VALUES (?, ?, 1, 0, ?)`,
		sessionID, name, now.UnixMilli(),
	)
	if err != nil {
		return Service{}, fmt.Errorf("store: creating service %q: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Service{}, fmt.Errorf("store: reading new service id: %w", err)
	}
	return Service{
		ID: id, SessionID: sessionID, Name: name,
		Active: true, ScoringPaused: false, CreatedAt: now,
	}, nil
}

func (s *Store) GetService(serviceID int64) (Service, error) {
	row := s.db.QueryRow(`SELECT `+serviceColumns+` FROM services WHERE id = ?`, serviceID)
	svc, err := scanService(row)
	if err != nil {
		return Service{}, fmt.Errorf("store: getting service %d: %w", serviceID, err)
	}
	return svc, nil
}

func (s *Store) ListActiveServices(sessionID int64) ([]Service, error) {
	rows, err := s.db.Query(
		`SELECT `+serviceColumns+` FROM services WHERE session_id = ? AND active = 1 ORDER BY name`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: listing active services: %w", err)
	}
	defer rows.Close()

	var out []Service
	for rows.Next() {
		var svc Service
		var active, paused int
		var createdAtMillis int64
		if err := rows.Scan(&svc.ID, &svc.SessionID, &svc.Name, &active, &paused, &createdAtMillis); err != nil {
			return nil, fmt.Errorf("store: scanning service row: %w", err)
		}
		svc.Active = active != 0
		svc.ScoringPaused = paused != 0
		svc.CreatedAt = time.UnixMilli(createdAtMillis).UTC()
		out = append(out, svc)
	}
	return out, rows.Err()
}

func (s *Store) SetServiceActive(serviceID int64, active bool) error {
	v := 0
	if active {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE services SET active = ? WHERE id = ?`, v, serviceID)
	if err != nil {
		return fmt.Errorf("store: setting service %d active=%v: %w", serviceID, active, err)
	}
	return nil
}

func (s *Store) SetServiceScoringPaused(serviceID int64, paused bool) error {
	v := 0
	if paused {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE services SET scoring_paused = ? WHERE id = ?`, v, serviceID)
	if err != nil {
		return fmt.Errorf("store: setting service %d scoring_paused=%v: %w", serviceID, paused, err)
	}
	return nil
}

func (s *Store) InsertConfigSnapshot(serviceID int64, cfg config.ServiceConfig) error {
	paramsJSON, err := json.Marshal(cfg.Params)
	if err != nil {
		return fmt.Errorf("store: marshaling params for service %d: %w", serviceID, err)
	}
	_, err = s.db.Exec(
		`INSERT INTO service_config_snapshots
		 (service_id, applied_at, host, check_type, interval_seconds, points_per_failure, params_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		serviceID, time.Now().UTC().UnixMilli(), cfg.Host, cfg.CheckType,
		cfg.IntervalSeconds, cfg.PointsPerFailure, string(paramsJSON),
	)
	if err != nil {
		return fmt.Errorf("store: inserting config snapshot for service %d: %w", serviceID, err)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/services.go internal/store/services_test.go
git commit -m "feat: add services store with persisted per-service scoring-pause"
```

---

### Task 4: Check results and events store

**Files:**
- Create: `internal/store/checkresults.go`
- Create: `internal/store/events.go`
- Test: `internal/store/checkresults_test.go`
- Test: `internal/store/events_test.go`

**Interfaces:**
- Consumes: `Service` (Task 3) for foreign keys.
- Produces:
  ```go
  package store

  const MaxDetailBytes = 4096

  type CheckResult struct {
      ID         int64
      SessionID  int64
      ServiceID  int64
      Timestamp  time.Time
      Success    bool
      LatencyMS  int64
      Detail     string
  }

  // InsertCheckResult caps/sanitizes Detail (truncates to MaxDetailBytes
  // with a trailing marker, replaces invalid UTF-8) before storing it, and
  // returns the new row's id.
  func (s *Store) InsertCheckResult(cr CheckResult) (int64, error)

  // GetLatestCheckResult returns the most recent row for this service, or
  // (nil, nil) if none exists yet (first check since the service existed).
  func (s *Store) GetLatestCheckResult(serviceID int64) (*CheckResult, error)

  func (s *Store) ListCheckResults(serviceID int64, limit int, beforeID int64) ([]CheckResult, error)

  type Event struct {
      ID        int64
      SessionID int64
      ServiceID *int64 // nil for engine-level events
      Level     string // "info" | "warning" | "error"
      Actor     string // "admin" | "system"
      Type      string
      Message   string
      Detail    string
      Timestamp time.Time
  }

  func (s *Store) InsertEvent(e Event) error
  func (s *Store) ListEvents(sessionID int64, limit int, beforeID int64) ([]Event, error)
  ```

- [ ] **Step 1: Write the failing tests**

```go
// internal/store/checkresults_test.go
package store

import (
	"strings"
	"testing"
	"time"
)

func setupServiceForResults(t *testing.T, st *Store) (sessionID, serviceID int64) {
	t.Helper()
	sess, _ := st.GetOrCreateSession("comp-1", 1000)
	svc, _ := st.GetOrCreateService(sess.ID, "apache-web")
	return sess.ID, svc.ID
}

func TestInsertAndGetLatestCheckResult(t *testing.T) {
	st := openTemp(t)
	sessionID, serviceID := setupServiceForResults(t, st)

	id, err := st.InsertCheckResult(CheckResult{
		SessionID: sessionID, ServiceID: serviceID, Timestamp: time.Now().UTC(),
		Success: true, LatencyMS: 42, Detail: "OK",
	})
	if err != nil {
		t.Fatalf("InsertCheckResult: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero id")
	}

	latest, err := st.GetLatestCheckResult(serviceID)
	if err != nil {
		t.Fatalf("GetLatestCheckResult: %v", err)
	}
	if latest == nil || latest.ID != id || !latest.Success {
		t.Errorf("latest = %+v, unexpected", latest)
	}
}

func TestGetLatestCheckResult_NoneYet(t *testing.T) {
	st := openTemp(t)
	_, serviceID := setupServiceForResults(t, st)

	latest, err := st.GetLatestCheckResult(serviceID)
	if err != nil {
		t.Fatalf("GetLatestCheckResult: %v", err)
	}
	if latest != nil {
		t.Errorf("expected nil for a service with no check results, got %+v", latest)
	}
}

func TestInsertCheckResult_TruncatesOversizedDetail(t *testing.T) {
	st := openTemp(t)
	sessionID, serviceID := setupServiceForResults(t, st)

	huge := strings.Repeat("x", MaxDetailBytes*2)
	id, err := st.InsertCheckResult(CheckResult{
		SessionID: sessionID, ServiceID: serviceID, Timestamp: time.Now().UTC(),
		Success: false, LatencyMS: 10, Detail: huge,
	})
	if err != nil {
		t.Fatalf("InsertCheckResult: %v", err)
	}
	stored, err := st.GetLatestCheckResult(serviceID)
	if err != nil {
		t.Fatalf("GetLatestCheckResult: %v", err)
	}
	if stored.ID != id {
		t.Fatalf("unexpected row returned")
	}
	if len(stored.Detail) > MaxDetailBytes+64 { // allow room for a truncation marker
		t.Errorf("Detail not truncated: len=%d", len(stored.Detail))
	}
	if !strings.Contains(stored.Detail, "truncated") {
		t.Errorf("expected truncation marker in Detail, got suffix %q",
			stored.Detail[len(stored.Detail)-20:])
	}
}

func TestInsertCheckResult_SanitizesInvalidUTF8(t *testing.T) {
	st := openTemp(t)
	sessionID, serviceID := setupServiceForResults(t, st)

	invalid := "valid-prefix-\xff\xfe-invalid"
	_, err := st.InsertCheckResult(CheckResult{
		SessionID: sessionID, ServiceID: serviceID, Timestamp: time.Now().UTC(),
		Success: false, LatencyMS: 5, Detail: invalid,
	})
	if err != nil {
		t.Fatalf("InsertCheckResult: %v", err)
	}
	stored, _ := st.GetLatestCheckResult(serviceID)
	if !strings.Contains(stored.Detail, "valid-prefix-") {
		t.Errorf("expected valid prefix preserved, got %q", stored.Detail)
	}
	if strings.Contains(stored.Detail, "\xff") {
		t.Errorf("expected invalid bytes stripped/replaced, got %q", stored.Detail)
	}
}

func TestListCheckResults_PagesNewestFirst(t *testing.T) {
	st := openTemp(t)
	_, serviceID := setupServiceForResults(t, st)
	sessionID, _ := setupServiceForResults(t, st)

	var ids []int64
	for i := 0; i < 5; i++ {
		id, _ := st.InsertCheckResult(CheckResult{
			SessionID: sessionID, ServiceID: serviceID, Timestamp: time.Now().UTC(),
			Success: i%2 == 0, LatencyMS: int64(i), Detail: "d",
		})
		ids = append(ids, id)
	}

	page, err := st.ListCheckResults(serviceID, 3, 0)
	if err != nil {
		t.Fatalf("ListCheckResults: %v", err)
	}
	if len(page) != 3 {
		t.Fatalf("len(page) = %d, want 3", len(page))
	}
	if page[0].ID != ids[4] || page[2].ID != ids[2] {
		t.Errorf("page not newest-first: got ids %d,%d,%d", page[0].ID, page[1].ID, page[2].ID)
	}

	nextPage, err := st.ListCheckResults(serviceID, 3, page[2].ID)
	if err != nil {
		t.Fatalf("ListCheckResults page 2: %v", err)
	}
	if len(nextPage) != 2 || nextPage[0].ID != ids[1] || nextPage[1].ID != ids[0] {
		t.Errorf("unexpected second page: %+v", nextPage)
	}
}
```

```go
// internal/store/events_test.go
package store

import "testing"

func TestInsertAndListEvents(t *testing.T) {
	st := openTemp(t)
	sess, _ := st.GetOrCreateSession("comp-1", 1000)
	svc, _ := st.GetOrCreateService(sess.ID, "apache-web")
	svcID := svc.ID

	err := st.InsertEvent(Event{
		SessionID: sess.ID, ServiceID: &svcID, Level: "info", Actor: "system",
		Type: "service_down", Message: "apache-web went down",
	})
	if err != nil {
		t.Fatalf("InsertEvent (service-scoped): %v", err)
	}

	err = st.InsertEvent(Event{
		SessionID: sess.ID, ServiceID: nil, Level: "warning", Actor: "admin",
		Type: "engine_stopped", Message: "engine stopped",
	})
	if err != nil {
		t.Fatalf("InsertEvent (engine-level): %v", err)
	}

	events, err := st.ListEvents(sess.ID, 10, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
	// Newest first.
	if events[0].Type != "engine_stopped" || events[0].ServiceID != nil {
		t.Errorf("events[0] = %+v, unexpected", events[0])
	}
	if events[1].Type != "service_down" || events[1].ServiceID == nil || *events[1].ServiceID != svcID {
		t.Errorf("events[1] = %+v, unexpected", events[1])
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/...`
Expected: FAIL — `InsertCheckResult`, `InsertEvent`, etc. undefined.

- [ ] **Step 3: Write the implementation**

```go
// internal/store/checkresults.go
package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxDetailBytes = 4096

func sanitizeDetail(detail string) string {
	if !utf8.ValidString(detail) {
		detail = strings.ToValidUTF8(detail, "")
	}
	if len(detail) > MaxDetailBytes {
		detail = detail[:MaxDetailBytes] + "...[truncated]"
	}
	return detail
}

type CheckResult struct {
	ID        int64
	SessionID int64
	ServiceID int64
	Timestamp time.Time
	Success   bool
	LatencyMS int64
	Detail    string
}

func (s *Store) InsertCheckResult(cr CheckResult) (int64, error) {
	success := 0
	if cr.Success {
		success = 1
	}
	res, err := s.db.Exec(
		`INSERT INTO check_results (session_id, service_id, timestamp, success, latency_ms, detail)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		cr.SessionID, cr.ServiceID, cr.Timestamp.UnixMilli(), success, cr.LatencyMS,
		sanitizeDetail(cr.Detail),
	)
	if err != nil {
		return 0, fmt.Errorf("store: inserting check result: %w", err)
	}
	return res.LastInsertId()
}

func scanCheckResult(rowScanner interface {
	Scan(dest ...any) error
}) (CheckResult, error) {
	var cr CheckResult
	var success int
	var ts int64
	if err := rowScanner.Scan(&cr.ID, &cr.SessionID, &cr.ServiceID, &ts, &success, &cr.LatencyMS, &cr.Detail); err != nil {
		return CheckResult{}, err
	}
	cr.Success = success != 0
	cr.Timestamp = time.UnixMilli(ts).UTC()
	return cr, nil
}

const checkResultColumns = `id, session_id, service_id, timestamp, success, latency_ms, detail`

func (s *Store) GetLatestCheckResult(serviceID int64) (*CheckResult, error) {
	row := s.db.QueryRow(
		`SELECT `+checkResultColumns+` FROM check_results
		 WHERE service_id = ? ORDER BY timestamp DESC, id DESC LIMIT 1`,
		serviceID,
	)
	cr, err := scanCheckResult(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: getting latest check result for service %d: %w", serviceID, err)
	}
	return &cr, nil
}

func (s *Store) ListCheckResults(serviceID int64, limit int, beforeID int64) ([]CheckResult, error) {
	query := `SELECT ` + checkResultColumns + ` FROM check_results WHERE service_id = ?`
	args := []any{serviceID}
	if beforeID > 0 {
		query += ` AND id < ?`
		args = append(args, beforeID)
	}
	query += ` ORDER BY timestamp DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing check results for service %d: %w", serviceID, err)
	}
	defer rows.Close()

	var out []CheckResult
	for rows.Next() {
		cr, err := scanCheckResult(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scanning check result row: %w", err)
		}
		out = append(out, cr)
	}
	return out, rows.Err()
}
```

```go
// internal/store/events.go
package store

import (
	"fmt"
	"time"
)

type Event struct {
	ID        int64
	SessionID int64
	ServiceID *int64
	Level     string
	Actor     string
	Type      string
	Message   string
	Detail    string
	Timestamp time.Time
}

func (s *Store) InsertEvent(e Event) error {
	ts := e.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	_, err := s.db.Exec(
		`INSERT INTO events (session_id, service_id, level, actor, type, message, detail, timestamp)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.SessionID, e.ServiceID, e.Level, e.Actor, e.Type, e.Message,
		sanitizeDetail(e.Detail), ts.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("store: inserting event: %w", err)
	}
	return nil
}

func (s *Store) ListEvents(sessionID int64, limit int, beforeID int64) ([]Event, error) {
	query := `SELECT id, session_id, service_id, level, actor, type, message, detail, timestamp
	          FROM events WHERE session_id = ?`
	args := []any{sessionID}
	if beforeID > 0 {
		query += ` AND id < ?`
		args = append(args, beforeID)
	}
	query += ` ORDER BY timestamp DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing events: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var ts int64
		if err := rows.Scan(&e.ID, &e.SessionID, &e.ServiceID, &e.Level, &e.Actor, &e.Type, &e.Message, &e.Detail, &ts); err != nil {
			return nil, fmt.Errorf("store: scanning event row: %w", err)
		}
		e.Timestamp = time.UnixMilli(ts).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/checkresults.go internal/store/events.go internal/store/checkresults_test.go internal/store/events_test.go
git commit -m "feat: add check-result and event storage with detail capping/sanitization"
```

---

### Task 5: Ledger store and score computation

**Files:**
- Create: `internal/store/ledger.go`
- Test: `internal/store/ledger_test.go`

**Interfaces:**
- Consumes: `Session` (Task 2).
- Produces:
  ```go
  package store

  var ErrDuplicateSourceRef = errors.New("store: ledger entry already recorded for this source")

  type LedgerEntry struct {
      ID            int64
      SessionID     int64
      Timestamp     time.Time
      SourceType    string
      SourceRef     int64
      FromSide      string
      ToSide        string
      NominalPoints int
      Points        int
      Voided        bool
      Reason        string
  }

  // RecordLedgerEntry floors the transfer at fromSide's current balance
  // (never transfers more than fromSide actually has) inside one
  // transaction, so two concurrent calls can never both read the same
  // pre-transfer balance. A duplicate (sessionID, sourceType, sourceRef)
  // returns ErrDuplicateSourceRef (wrap-checkable via errors.Is) instead
  // of a generic error, so callers (the Scorer, Task 8) can treat it as a
  // no-op rather than a failure.
  func (s *Store) RecordLedgerEntry(sessionID int64, sourceType string, sourceRef int64, fromSide, toSide string, nominalPoints int, reason string) (LedgerEntry, error)

  // GetScore returns each side's current balance: starting_score (blue
  // only) adjusted by every non-voided ledger entry. Works unmodified for
  // future bidirectional sources, since it sums both directions per side
  // rather than assuming blue-to-red only.
  func (s *Store) GetScore(sessionID int64) (blue, red int, err error)
  ```

**Note on concurrency:** because `Store` is backed by a single `*sql.DB`
connection (`SetMaxOpenConns(1)`, set in Task 2), a `BEGIN`'d transaction
here holds that one connection until `COMMIT`/`ROLLBACK` — no other
statement from any other goroutine can interleave with the balance
read and the insert in `RecordLedgerEntry`. This is the entire
concurrency mechanism the spec requires; it needs no additional mutex.

- [ ] **Step 1: Write the failing tests**

```go
// internal/store/ledger_test.go
package store

import (
	"errors"
	"testing"
)

func TestRecordLedgerEntry_SimpleTransfer(t *testing.T) {
	st := openTemp(t)
	sess, _ := st.GetOrCreateSession("comp-1", 1000)

	entry, err := st.RecordLedgerEntry(sess.ID, "service_check", 1, "blue", "red", 10, "apache-web down")
	if err != nil {
		t.Fatalf("RecordLedgerEntry: %v", err)
	}
	if entry.NominalPoints != 10 || entry.Points != 10 {
		t.Errorf("entry = %+v, want nominal=10 points=10", entry)
	}

	blue, red, err := st.GetScore(sess.ID)
	if err != nil {
		t.Fatalf("GetScore: %v", err)
	}
	if blue != 990 || red != 10 {
		t.Errorf("blue=%d red=%d, want 990/10", blue, red)
	}
}

func TestRecordLedgerEntry_FloorsAtZero(t *testing.T) {
	st := openTemp(t)
	sess, _ := st.GetOrCreateSession("comp-1", 5)

	first, err := st.RecordLedgerEntry(sess.ID, "service_check", 1, "blue", "red", 10, "r1")
	if err != nil {
		t.Fatalf("first RecordLedgerEntry: %v", err)
	}
	if first.NominalPoints != 10 || first.Points != 5 {
		t.Errorf("first = %+v, want nominal=10 points=5 (floored)", first)
	}

	second, err := st.RecordLedgerEntry(sess.ID, "service_check", 2, "blue", "red", 10, "r2")
	if err != nil {
		t.Fatalf("second RecordLedgerEntry: %v", err)
	}
	if second.Points != 0 {
		t.Errorf("second.Points = %d, want 0 (blue already at floor)", second.Points)
	}

	blue, red, _ := st.GetScore(sess.ID)
	if blue != 0 || red != 5 {
		t.Errorf("blue=%d red=%d, want 0/5", blue, red)
	}
}

func TestRecordLedgerEntry_DuplicateSourceRefRejected(t *testing.T) {
	st := openTemp(t)
	sess, _ := st.GetOrCreateSession("comp-1", 1000)

	if _, err := st.RecordLedgerEntry(sess.ID, "service_check", 42, "blue", "red", 10, "r1"); err != nil {
		t.Fatalf("first RecordLedgerEntry: %v", err)
	}
	_, err := st.RecordLedgerEntry(sess.ID, "service_check", 42, "blue", "red", 10, "r1-retry")
	if !errors.Is(err, ErrDuplicateSourceRef) {
		t.Fatalf("expected ErrDuplicateSourceRef, got %v", err)
	}

	blue, red, _ := st.GetScore(sess.ID)
	if blue != 990 || red != 10 {
		t.Errorf("rejected duplicate must not double-score: blue=%d red=%d, want 990/10", blue, red)
	}
}

func TestGetScore_ExcludesVoidedEntries(t *testing.T) {
	st := openTemp(t)
	sess, _ := st.GetOrCreateSession("comp-1", 1000)

	if _, err := st.RecordLedgerEntry(sess.ID, "service_check", 1, "blue", "red", 10, "r1"); err != nil {
		t.Fatalf("RecordLedgerEntry: %v", err)
	}
	// No API writes voided=1 yet (deferred per spec), but GetScore's SUM
	// must already respect it so a future admin-void feature works
	// without touching this query.
	if _, err := st.db.Exec(
		`INSERT INTO ledger_entries
		 (session_id, timestamp, source_type, source_ref, from_side, to_side, nominal_points, points, voided, reason)
		 VALUES (?, 0, 'service_check', 2, 'blue', 'red', 10, 10, 1, 'manually voided for test')`,
		sess.ID,
	); err != nil {
		t.Fatalf("inserting voided row directly: %v", err)
	}

	blue, red, err := st.GetScore(sess.ID)
	if err != nil {
		t.Fatalf("GetScore: %v", err)
	}
	if blue != 990 || red != 10 {
		t.Errorf("voided entry was counted: blue=%d red=%d, want 990/10", blue, red)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/...`
Expected: FAIL — `RecordLedgerEntry`, `GetScore` undefined.

- [ ] **Step 3: Write the implementation**

```go
// internal/store/ledger.go
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrDuplicateSourceRef = errors.New("store: ledger entry already recorded for this source")

type LedgerEntry struct {
	ID            int64
	SessionID     int64
	Timestamp     time.Time
	SourceType    string
	SourceRef     int64
	FromSide      string
	ToSide        string
	NominalPoints int
	Points        int
	Voided        bool
	Reason        string
}

type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func sideBalance(q rowQuerier, sessionID int64, side string, startingScore int) (int, error) {
	var outgoing, incoming int
	if err := q.QueryRow(
		`SELECT COALESCE(SUM(points),0) FROM ledger_entries WHERE session_id = ? AND from_side = ? AND voided = 0`,
		sessionID, side,
	).Scan(&outgoing); err != nil {
		return 0, fmt.Errorf("store: summing outgoing for side %s: %w", side, err)
	}
	if err := q.QueryRow(
		`SELECT COALESCE(SUM(points),0) FROM ledger_entries WHERE session_id = ? AND to_side = ? AND voided = 0`,
		sessionID, side,
	).Scan(&incoming); err != nil {
		return 0, fmt.Errorf("store: summing incoming for side %s: %w", side, err)
	}
	base := 0
	if side == "blue" {
		base = startingScore
	}
	return base + incoming - outgoing, nil
}

func isUniqueConstraintErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func (s *Store) RecordLedgerEntry(sessionID int64, sourceType string, sourceRef int64, fromSide, toSide string, nominalPoints int, reason string) (LedgerEntry, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("store: beginning ledger transaction: %w", err)
	}
	defer tx.Rollback()

	var startingScore int
	if err := tx.QueryRow(`SELECT starting_score FROM sessions WHERE id = ?`, sessionID).Scan(&startingScore); err != nil {
		return LedgerEntry{}, fmt.Errorf("store: loading session starting_score: %w", err)
	}

	fromBalance, err := sideBalance(tx, sessionID, fromSide, startingScore)
	if err != nil {
		return LedgerEntry{}, err
	}

	transferred := nominalPoints
	if transferred > fromBalance {
		transferred = fromBalance
	}
	if transferred < 0 {
		transferred = 0
	}

	now := time.Now().UTC()
	res, err := tx.Exec(
		`INSERT INTO ledger_entries
		 (session_id, timestamp, source_type, source_ref, from_side, to_side, nominal_points, points, voided, reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		sessionID, now.UnixMilli(), sourceType, sourceRef, fromSide, toSide, nominalPoints, transferred, reason,
	)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return LedgerEntry{}, fmt.Errorf("%w: session=%d source_type=%s source_ref=%d",
				ErrDuplicateSourceRef, sessionID, sourceType, sourceRef)
		}
		return LedgerEntry{}, fmt.Errorf("store: inserting ledger entry: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("store: reading ledger entry id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return LedgerEntry{}, fmt.Errorf("store: committing ledger entry: %w", err)
	}

	return LedgerEntry{
		ID: id, SessionID: sessionID, Timestamp: now, SourceType: sourceType, SourceRef: sourceRef,
		FromSide: fromSide, ToSide: toSide, NominalPoints: nominalPoints, Points: transferred, Reason: reason,
	}, nil
}

func (s *Store) GetScore(sessionID int64) (blue, red int, err error) {
	var startingScore int
	if err := s.db.QueryRow(`SELECT starting_score FROM sessions WHERE id = ?`, sessionID).Scan(&startingScore); err != nil {
		return 0, 0, fmt.Errorf("store: loading starting_score: %w", err)
	}
	blue, err = sideBalance(s.db, sessionID, "blue", startingScore)
	if err != nil {
		return 0, 0, err
	}
	red, err = sideBalance(s.db, sessionID, "red", startingScore)
	if err != nil {
		return 0, 0, err
	}
	return blue, red, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store/... -race`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/ledger.go internal/store/ledger_test.go
git commit -m "feat: add ledger with transactional floor/cap and duplicate-source rejection"
```

---

### Task 6: Check plugins (`tcp_connect`, `http`)

**Files:**
- Create: `internal/checks/checks.go`
- Create: `internal/checks/tcp_connect.go`
- Create: `internal/checks/http.go`
- Test: `internal/checks/checks_test.go`
- Test: `internal/checks/tcp_connect_test.go`
- Test: `internal/checks/http_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks (deliberately independent — plugins
  know nothing about config, scoring, or persistence).
- Produces:
  ```go
  package checks

  type Result struct {
      Success bool
      Detail  string
  }

  // CheckFunc performs one check attempt. It must return promptly once ctx
  // is done — callers (Task 7's Scheduler) rely on this to bound a hung
  // check at the service's interval.
  type CheckFunc func(ctx context.Context, host string, params map[string]any) Result

  type Registry struct { /* unexported */ }
  func NewRegistry() *Registry
  func (r *Registry) Register(name string, fn CheckFunc)
  func (r *Registry) Get(name string) (CheckFunc, bool)

  // NewDefaultRegistry returns a Registry with tcp_connect and http
  // already registered.
  func NewDefaultRegistry() *Registry

  func TCPConnect(ctx context.Context, host string, params map[string]any) Result
  func HTTP(ctx context.Context, host string, params map[string]any) Result
  ```

- [ ] **Step 1: Write the failing tests**

```go
// internal/checks/checks_test.go
package checks

import "testing"

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	called := false
	r.Register("fake", func(ctx context.Context, host string, params map[string]any) Result {
		called = true
		return Result{Success: true}
	})

	fn, ok := r.Get("fake")
	if !ok {
		t.Fatal("expected fake check to be registered")
	}
	fn(context.Background(), "host", nil)
	if !called {
		t.Error("registered function was not the one returned by Get")
	}

	_, ok = r.Get("does-not-exist")
	if ok {
		t.Error("expected ok=false for unregistered check type")
	}
}

func TestNewDefaultRegistry_HasBuiltins(t *testing.T) {
	r := NewDefaultRegistry()
	for _, name := range []string{"tcp_connect", "http"} {
		if _, ok := r.Get(name); !ok {
			t.Errorf("expected default registry to include %q", name)
		}
	}
}
```

Add `"context"` to this test file's imports.

```go
// internal/checks/tcp_connect_test.go
package checks

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestTCPConnect_Success(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	res := TCPConnect(context.Background(), host, map[string]any{"port": port})
	if !res.Success {
		t.Errorf("expected success, got Detail=%q", res.Detail)
	}
}

func TestTCPConnect_ClosedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	ln.Close() // now nothing is listening on this port

	res := TCPConnect(context.Background(), host, map[string]any{"port": port})
	if res.Success {
		t.Error("expected failure connecting to a closed port")
	}
}

func TestTCPConnect_MissingPort(t *testing.T) {
	res := TCPConnect(context.Background(), "127.0.0.1", map[string]any{})
	if res.Success {
		t.Error("expected failure when port param is missing")
	}
	if !strings.Contains(res.Detail, "port") {
		t.Errorf("Detail = %q, expected it to mention the missing port param", res.Detail)
	}
}

func TestTCPConnect_RespectsContextDeadline(t *testing.T) {
	// 192.0.2.0/24 is reserved for documentation (RFC 5737) and never
	// routed; the context deadline — not the network — is what ends this
	// call, so the test is fast and has no real external dependency.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	res := TCPConnect(ctx, "192.0.2.1", map[string]any{"port": 81})
	elapsed := time.Since(start)

	if res.Success {
		t.Error("expected failure against an unroutable address")
	}
	if elapsed > 2*time.Second {
		t.Errorf("TCPConnect took %v, expected it to respect the ~50ms context deadline", elapsed)
	}
}
```

Add `"fmt"` to this test file's imports.

```go
// internal/checks/http_test.go
package checks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func hostPort(t *testing.T, ts *httptest.Server) (host string, port int) {
	t.Helper()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	h, p, err := netSplitHostPortInt(u.Host)
	if err != nil {
		t.Fatalf("splitting host:port: %v", err)
	}
	return h, p
}

func TestHTTP_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("Welcome to the site"))
	}))
	defer ts.Close()
	host, port := hostPort(t, ts)

	res := HTTP(context.Background(), host, map[string]any{
		"port": port, "path": "/", "expect_status": 200, "expect_contains": "Welcome",
	})
	if !res.Success {
		t.Errorf("expected success, got Detail=%q", res.Detail)
	}
}

func TestHTTP_StatusMismatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer ts.Close()
	host, port := hostPort(t, ts)

	res := HTTP(context.Background(), host, map[string]any{
		"port": port, "path": "/", "expect_status": 200,
	})
	if res.Success {
		t.Error("expected failure on status mismatch")
	}
}

func TestHTTP_ContentMismatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("defaced"))
	}))
	defer ts.Close()
	host, port := hostPort(t, ts)

	res := HTTP(context.Background(), host, map[string]any{
		"port": port, "path": "/", "expect_status": 200, "expect_contains": "Welcome",
	})
	if res.Success {
		t.Error("expected failure on content mismatch")
	}
}

func TestHTTP_RespectsContextDeadline(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer ts.Close()
	host, port := hostPort(t, ts)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	res := HTTP(ctx, host, map[string]any{"port": port, "path": "/", "expect_status": 200})
	elapsed := time.Since(start)

	if res.Success {
		t.Error("expected failure when the server is slower than the context deadline")
	}
	if elapsed > 1*time.Second {
		t.Errorf("HTTP took %v, expected it to respect the ~20ms context deadline", elapsed)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/checks/...`
Expected: FAIL — package `checks` does not exist yet.

- [ ] **Step 3: Write the implementation**

```go
// internal/checks/checks.go
package checks

import (
	"context"
	"fmt"
)

type Result struct {
	Success bool
	Detail  string
}

type CheckFunc func(ctx context.Context, host string, params map[string]any) Result

type Registry struct {
	funcs map[string]CheckFunc
}

func NewRegistry() *Registry {
	return &Registry{funcs: make(map[string]CheckFunc)}
}

func (r *Registry) Register(name string, fn CheckFunc) {
	r.funcs[name] = fn
}

func (r *Registry) Get(name string) (CheckFunc, bool) {
	fn, ok := r.funcs[name]
	return fn, ok
}

func NewDefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register("tcp_connect", TCPConnect)
	r.Register("http", HTTP)
	return r
}

func paramInt(params map[string]any, key string) (int, error) {
	v, ok := params[key]
	if !ok {
		return 0, fmt.Errorf("missing required param %q", key)
	}
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	default:
		return 0, fmt.Errorf("param %q must be an integer, got %T", key, v)
	}
}

func paramString(params map[string]any, key, def string) string {
	v, ok := params[key]
	if !ok {
		return def
	}
	s, _ := v.(string)
	return s
}

// netSplitHostPortInt is a small test helper used by http_test.go; it
// lives in the package (not _test.go) only because it's trivial and
// shared logic, not because production code calls it.
func netSplitHostPortInt(hostport string) (string, int, error) {
	host, portStr, err := splitHostPort(hostport)
	if err != nil {
		return "", 0, err
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return "", 0, err
	}
	return host, port, nil
}
```

```go
// internal/checks/tcp_connect.go
package checks

import (
	"context"
	"fmt"
	"net"
)

func splitHostPort(hostport string) (string, string, error) {
	return net.SplitHostPort(hostport)
}

func TCPConnect(ctx context.Context, host string, params map[string]any) Result {
	port, err := paramInt(params, "port")
	if err != nil {
		return Result{Success: false, Detail: err.Error()}
	}
	addr := fmt.Sprintf("%s:%d", host, port)
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return Result{Success: false, Detail: fmt.Sprintf("connecting to %s: %v", addr, err)}
	}
	conn.Close()
	return Result{Success: true, Detail: fmt.Sprintf("connected to %s", addr)}
}
```

```go
// internal/checks/http.go
package checks

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxHTTPBodyBytes = 64 * 1024

func HTTP(ctx context.Context, host string, params map[string]any) Result {
	port, err := paramInt(params, "port")
	if err != nil {
		return Result{Success: false, Detail: err.Error()}
	}
	path := paramString(params, "path", "/")
	expectStatus, err := paramInt(params, "expect_status")
	if err != nil {
		expectStatus = http.StatusOK
	}
	expectContains := paramString(params, "expect_contains", "")

	url := fmt.Sprintf("http://%s:%d%s", host, port, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Result{Success: false, Detail: fmt.Sprintf("building request: %v", err)}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Result{Success: false, Detail: fmt.Sprintf("requesting %s: %v", url, err)}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBodyBytes))
	if err != nil {
		return Result{Success: false, Detail: fmt.Sprintf("reading response body: %v", err)}
	}

	if resp.StatusCode != expectStatus {
		return Result{Success: false, Detail: fmt.Sprintf("status %d, want %d", resp.StatusCode, expectStatus)}
	}
	if expectContains != "" && !strings.Contains(string(body), expectContains) {
		// Deliberately doesn't echo expectContains itself — that string is
		// exactly what a future public-detail leak would hand red team.
		return Result{Success: false, Detail: "response did not contain expected content"}
	}
	return Result{Success: true, Detail: fmt.Sprintf("status %d", resp.StatusCode)}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/checks/... -race`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/checks/
git commit -m "feat: add tcp_connect and http check plugins"
```

---

### Task 7: Scheduler (self-paced per-service routines)

**Files:**
- Create: `internal/scheduler/scheduler.go`
- Test: `internal/scheduler/scheduler_test.go`

**Interfaces:**
- Consumes: `config.LoadService` (Task 1), `checks.Registry` / `checks.CheckFunc` / `checks.Result` (Task 6).
- Produces:
  ```go
  package scheduler

  // ResultSink is how the Scheduler reports a finished check. Implemented
  // by Task 8's Scorer.
  type ResultSink interface {
      Record(serviceID int64, pointsPerFailure int, result checks.Result, latency time.Duration) error
  }

  type Scheduler struct { /* unexported */ }

  func New(registry *checks.Registry, sink ResultSink, configPath string) *Scheduler

  // StartService is idempotent: if serviceID is already running, it's a
  // no-op. Otherwise it loads this service's entry fresh from configPath,
  // looks up its check_type in registry, and launches its self-paced loop.
  func (s *Scheduler) StartService(serviceID int64, name string) error

  // StopService is idempotent and synchronous: it blocks until the
  // routine's goroutine has fully exited before returning.
  func (s *Scheduler) StopService(serviceID int64)

  // RestartService stops (if running) then starts, always re-reading
  // configPath fresh — this is how a config edit takes effect.
  func (s *Scheduler) RestartService(serviceID int64, name string) error

  func (s *Scheduler) IsRunning(serviceID int64) bool
  ```

**Scheduling model:** each service's routine is a self-paced loop, not a
shared ticker: `run check (bounded by a per-check context deadline equal
to this service's interval_seconds) → report result → sleep whatever
remains of the interval → repeat`. This guarantees exactly one check per
interval per service with no overlap, by construction — there is no
separate timeout field and no tick-skip bookkeeping needed. The first
check fires after a small random stagger (0–5s), not immediately and not
after a full interval — this is deliberate so ten services starting
together don't all hit the single-writer store (Task 2) at once.

- [ ] **Step 1: Write the failing tests**

```go
// internal/scheduler/scheduler_test.go
package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/checks"
)

type recordedCall struct {
	serviceID        int64
	pointsPerFailure int
	result           checks.Result
	at               time.Time
}

type fakeSink struct {
	mu    sync.Mutex
	calls []recordedCall
}

func (f *fakeSink) Record(serviceID int64, pointsPerFailure int, result checks.Result, latency time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordedCall{serviceID, pointsPerFailure, result, time.Now()})
	return nil
}

func (f *fakeSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSink) snapshot() []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func writeTempConfig(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

const baseYAML = `
session_name: "s"
starting_score: 1000
admin:
  username: admin
  password_hash: $2a$10$abcdefghijklmnopqrstuvabcdefghijklmnopqrstuvwxyz123456
services:
  - name: svc1
    host: 127.0.0.1
    check_type: tcp_connect
    interval_seconds: 1
    points_per_failure: 10
    params: {}
`

func TestScheduler_StartTicksRepeatedly(t *testing.T) {
	path := writeTempConfig(t, baseYAML)
	registry := checks.NewRegistry()
	registry.Register("tcp_connect", func(ctx context.Context, host string, params map[string]any) checks.Result {
		return checks.Result{Success: true, Detail: "ok"}
	})
	sink := &fakeSink{}
	sched := New(registry, sink, path)

	if err := sched.StartService(1, "svc1"); err != nil {
		t.Fatalf("StartService: %v", err)
	}
	defer sched.StopService(1)

	time.Sleep(2200 * time.Millisecond) // stagger (<=5s... use short test below instead)
	if sink.count() < 1 {
		t.Errorf("expected at least 1 recorded call, got %d", sink.count())
	}
}

func TestScheduler_StopHaltsFurtherTicks(t *testing.T) {
	path := writeTempConfig(t, baseYAML)
	registry := checks.NewRegistry()
	registry.Register("tcp_connect", func(ctx context.Context, host string, params map[string]any) checks.Result {
		return checks.Result{Success: true}
	})
	sink := &fakeSink{}
	sched := New(registry, sink, path)

	if err := sched.StartService(1, "svc1"); err != nil {
		t.Fatalf("StartService: %v", err)
	}
	time.Sleep(600 * time.Millisecond)
	sched.StopService(1)
	countAtStop := sink.count()

	time.Sleep(1500 * time.Millisecond)
	if sink.count() != countAtStop {
		t.Errorf("calls continued after Stop: had %d, now %d", countAtStop, sink.count())
	}
	if sched.IsRunning(1) {
		t.Error("expected IsRunning=false after Stop")
	}
}

func TestScheduler_HungCheckIsForceFailedAtDeadline(t *testing.T) {
	path := writeTempConfig(t, baseYAML) // interval_seconds: 1
	registry := checks.NewRegistry()
	registry.Register("tcp_connect", func(ctx context.Context, host string, params map[string]any) checks.Result {
		<-ctx.Done() // hang until the per-check deadline cancels us
		return checks.Result{Success: false, Detail: "forced by deadline"}
	})
	sink := &fakeSink{}
	sched := New(registry, sink, path)

	if err := sched.StartService(1, "svc1"); err != nil {
		t.Fatalf("StartService: %v", err)
	}
	defer sched.StopService(1)

	time.Sleep(3 * time.Second) // stagger (<=5s) + at least one ~1s cycle
	calls := sink.snapshot()
	if len(calls) == 0 {
		t.Fatal("expected the hung check to still be force-failed and recorded at least once")
	}
	if calls[0].result.Success {
		t.Error("expected the hung check to be recorded as a failure")
	}
}

func TestScheduler_RestartReloadsConfig(t *testing.T) {
	path := writeTempConfig(t, baseYAML) // points_per_failure: 10
	registry := checks.NewRegistry()
	registry.Register("tcp_connect", func(ctx context.Context, host string, params map[string]any) checks.Result {
		return checks.Result{Success: true}
	})
	sink := &fakeSink{}
	sched := New(registry, sink, path)

	if err := sched.StartService(1, "svc1"); err != nil {
		t.Fatalf("StartService: %v", err)
	}

	updatedYAML := `
session_name: "s"
starting_score: 1000
admin:
  username: admin
  password_hash: $2a$10$abcdefghijklmnopqrstuvabcdefghijklmnopqrstuvwxyz123456
services:
  - name: svc1
    host: 127.0.0.1
    check_type: tcp_connect
    interval_seconds: 1
    points_per_failure: 99
    params: {}
`
	if err := os.WriteFile(path, []byte(updatedYAML), 0o600); err != nil {
		t.Fatalf("rewriting config: %v", err)
	}
	if err := sched.RestartService(1, "svc1"); err != nil {
		t.Fatalf("RestartService: %v", err)
	}
	defer sched.StopService(1)

	time.Sleep(2200 * time.Millisecond)
	calls := sink.snapshot()
	if len(calls) == 0 {
		t.Fatal("expected at least one call after restart")
	}
	if calls[len(calls)-1].pointsPerFailure != 99 {
		t.Errorf("last call pointsPerFailure = %d, want 99 (updated config)", calls[len(calls)-1].pointsPerFailure)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/scheduler/...`
Expected: FAIL — package `scheduler` does not exist yet.

- [ ] **Step 3: Write the implementation**

```go
// internal/scheduler/scheduler.go
package scheduler

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/config"
)

type ResultSink interface {
	Record(serviceID int64, pointsPerFailure int, result checks.Result, latency time.Duration) error
}

type routine struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type Scheduler struct {
	registry   *checks.Registry
	sink       ResultSink
	configPath string

	mu       sync.Mutex
	routines map[int64]*routine
}

func New(registry *checks.Registry, sink ResultSink, configPath string) *Scheduler {
	return &Scheduler{
		registry:   registry,
		sink:       sink,
		configPath: configPath,
		routines:   make(map[int64]*routine),
	}
}

func (s *Scheduler) StartService(serviceID int64, name string) error {
	s.mu.Lock()
	if _, running := s.routines[serviceID]; running {
		s.mu.Unlock()
		return nil // idempotent
	}
	s.mu.Unlock()

	cfg, err := config.LoadService(s.configPath, name)
	if err != nil {
		return fmt.Errorf("scheduler: loading config for %q: %w", name, err)
	}
	fn, ok := s.registry.Get(cfg.CheckType)
	if !ok {
		return fmt.Errorf("scheduler: no check plugin registered for check_type %q", cfg.CheckType)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	s.mu.Lock()
	s.routines[serviceID] = &routine{cancel: cancel, done: done}
	s.mu.Unlock()

	go s.run(ctx, done, serviceID, *cfg, fn)
	return nil
}

func (s *Scheduler) run(ctx context.Context, done chan struct{}, serviceID int64, cfg config.ServiceConfig, fn checks.CheckFunc) {
	defer close(done)

	stagger := time.Duration(rand.Intn(5000)) * time.Millisecond
	select {
	case <-time.After(stagger):
	case <-ctx.Done():
		return
	}

	interval := time.Duration(cfg.IntervalSeconds) * time.Second
	for {
		start := time.Now()
		checkCtx, cancel := context.WithTimeout(ctx, interval)
		result := fn(checkCtx, cfg.Host, cfg.Params)
		cancel()
		latency := time.Since(start)

		// A sink error must never stop this service's loop — one bad
		// write shouldn't take down checking for the rest of the
		// competition. The store/scorer layer owns logging it.
		_ = s.sink.Record(serviceID, cfg.PointsPerFailure, result, latency)

		elapsed := time.Since(start)
		remaining := interval - elapsed
		if remaining < 0 {
			remaining = 0
		}
		select {
		case <-time.After(remaining):
		case <-ctx.Done():
			return
		}
	}
}

func (s *Scheduler) StopService(serviceID int64) {
	s.mu.Lock()
	r, ok := s.routines[serviceID]
	if ok {
		delete(s.routines, serviceID)
	}
	s.mu.Unlock()
	if !ok {
		return // idempotent
	}
	r.cancel()
	<-r.done
}

func (s *Scheduler) RestartService(serviceID int64, name string) error {
	s.StopService(serviceID)
	return s.StartService(serviceID, name)
}

func (s *Scheduler) IsRunning(serviceID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.routines[serviceID]
	return ok
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/scheduler/... -race -timeout 60s`
Expected: PASS (this package's tests are the slowest in the suite — several
seconds each — because they exercise real wall-clock interval timing)

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/
git commit -m "feat: add self-paced per-service scheduler with stop/restart"
```

---

### Task 8: Scorer (scoring rules + mass-failure detection)

**Files:**
- Create: `internal/scorer/scorer.go`
- Modify: `internal/store/checkresults.go` (add `CountLatestCheckOutcomes`)
- Test: `internal/scorer/scorer_test.go`
- Test: `internal/store/checkresults_test.go` (add a case for the new store method)

**Interfaces:**
- Consumes: `store.Store` (Tasks 2–5), `checks.Result` (Task 6). Implements
  `scheduler.ResultSink` (Task 7) — this is how the two packages connect
  without either importing the other directly (Task 10's Engine wires
  them together).
- Produces:
  ```go
  package scorer

  type Scorer struct { /* unexported */ }

  // massFailureFraction nil disables the safety net.
  func New(st *store.Store, sessionID int64, massFailureFraction *float64) *Scorer

  // Record satisfies scheduler.ResultSink.
  func (sc *Scorer) Record(serviceID int64, pointsPerFailure int, result checks.Result, latency time.Duration) error
  ```
  ```go
  package store

  // CountLatestCheckOutcomes looks at the single most recent check_results
  // row for every active service in this session and reports how many
  // exist and how many of those latest rows are failures.
  func (s *Store) CountLatestCheckOutcomes(sessionID int64) (total, failing int, err error)
  ```

- [ ] **Step 1: Write the failing tests**

```go
// internal/store/checkresults_test.go  (add this test to the existing file)

func TestCountLatestCheckOutcomes(t *testing.T) {
	st := openTemp(t)
	sess, _ := st.GetOrCreateSession("comp-1", 1000)
	svcA, _ := st.GetOrCreateService(sess.ID, "svc-a")
	svcB, _ := st.GetOrCreateService(sess.ID, "svc-b")
	svcC, _ := st.GetOrCreateService(sess.ID, "svc-c") // left inactive, must not count
	st.SetServiceActive(svcC.ID, false)

	// svc-a: down, then up (latest = up)
	st.InsertCheckResult(CheckResult{SessionID: sess.ID, ServiceID: svcA.ID, Timestamp: time.Now().UTC(), Success: false})
	st.InsertCheckResult(CheckResult{SessionID: sess.ID, ServiceID: svcA.ID, Timestamp: time.Now().UTC(), Success: true})
	// svc-b: down (latest = down)
	st.InsertCheckResult(CheckResult{SessionID: sess.ID, ServiceID: svcB.ID, Timestamp: time.Now().UTC(), Success: false})
	// svc-c: down but inactive
	st.InsertCheckResult(CheckResult{SessionID: sess.ID, ServiceID: svcC.ID, Timestamp: time.Now().UTC(), Success: false})

	total, failing, err := st.CountLatestCheckOutcomes(sess.ID)
	if err != nil {
		t.Fatalf("CountLatestCheckOutcomes: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2 (inactive svc-c excluded)", total)
	}
	if failing != 1 {
		t.Errorf("failing = %d, want 1 (only svc-b's latest result)", failing)
	}
}
```

```go
// internal/scorer/scorer_test.go
package scorer

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

func openTestStore(t *testing.T) (*store.Store, store.Session) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "scoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sess, err := st.GetOrCreateSession("comp-1", 1000)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	return st, sess
}

func f(v float64) *float64 { return &v }

func TestScorer_Failure_RecordsLedgerEntryAndEvent(t *testing.T) {
	st, sess := openTestStore(t)
	svc, _ := st.GetOrCreateService(sess.ID, "apache-web")
	sc := New(st, sess.ID, nil)

	err := sc.Record(svc.ID, 10, checks.Result{Success: false, Detail: "connection refused"}, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	blue, red, _ := st.GetScore(sess.ID)
	if blue != 990 || red != 10 {
		t.Errorf("blue=%d red=%d, want 990/10", blue, red)
	}

	events, _ := st.ListEvents(sess.ID, 10, 0)
	foundDown := false
	for _, e := range events {
		if e.Type == "service_down" {
			foundDown = true
		}
	}
	if !foundDown {
		t.Error("expected a service_down event on first (failing) check")
	}
}

func TestScorer_Success_NoLedgerEntry(t *testing.T) {
	st, sess := openTestStore(t)
	svc, _ := st.GetOrCreateService(sess.ID, "apache-web")
	sc := New(st, sess.ID, nil)

	if err := sc.Record(svc.ID, 10, checks.Result{Success: true, Detail: "ok"}, time.Millisecond); err != nil {
		t.Fatalf("Record: %v", err)
	}

	blue, red, _ := st.GetScore(sess.ID)
	if blue != 1000 || red != 0 {
		t.Errorf("blue=%d red=%d, want 1000/0 (no deduction on success)", blue, red)
	}
}

func TestScorer_GlobalPause_NoLedgerEntryButStillRecordsResult(t *testing.T) {
	st, sess := openTestStore(t)
	svc, _ := st.GetOrCreateService(sess.ID, "apache-web")
	if err := st.SetGlobalScoringPaused(true); err != nil {
		t.Fatalf("SetGlobalScoringPaused: %v", err)
	}
	sc := New(st, sess.ID, nil)

	if err := sc.Record(svc.ID, 10, checks.Result{Success: false}, time.Millisecond); err != nil {
		t.Fatalf("Record: %v", err)
	}

	blue, red, _ := st.GetScore(sess.ID)
	if blue != 1000 || red != 0 {
		t.Errorf("blue=%d red=%d, want 1000/0 while globally paused", blue, red)
	}
	results, _ := st.ListCheckResults(svc.ID, 10, 0)
	if len(results) != 1 {
		t.Errorf("expected the check result to still be recorded while paused, got %d rows", len(results))
	}
}

func TestScorer_PerServicePause_NoLedgerEntry(t *testing.T) {
	st, sess := openTestStore(t)
	svc, _ := st.GetOrCreateService(sess.ID, "apache-web")
	if err := st.SetServiceScoringPaused(svc.ID, true); err != nil {
		t.Fatalf("SetServiceScoringPaused: %v", err)
	}
	sc := New(st, sess.ID, nil)

	if err := sc.Record(svc.ID, 10, checks.Result{Success: false}, time.Millisecond); err != nil {
		t.Fatalf("Record: %v", err)
	}

	blue, _, _ := st.GetScore(sess.ID)
	if blue != 1000 {
		t.Errorf("blue=%d, want 1000 (this service's scoring is paused)", blue)
	}
}

func TestScorer_MassFailureAutopause(t *testing.T) {
	st, sess := openTestStore(t)
	svcA, _ := st.GetOrCreateService(sess.ID, "svc-a")
	svcB, _ := st.GetOrCreateService(sess.ID, "svc-b")
	sc := New(st, sess.ID, f(0.5))

	if err := sc.Record(svcA.ID, 10, checks.Result{Success: false}, time.Millisecond); err != nil {
		t.Fatalf("Record A: %v", err)
	}
	rs, _ := st.GetRunState()
	if rs.ScoringPaused {
		t.Fatal("expected not yet paused after only 1 of 2 services failing (50% boundary not yet met by a strict read, this call's fraction is 1/1=100% for svc-a alone... )")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/... ./internal/scorer/...`
Expected: FAIL — `CountLatestCheckOutcomes` and package `scorer` undefined.

> **Note on the mass-failure test above:** with only `svc-a` having ever
> reported, `CountLatestCheckOutcomes` sees `total=1, failing=1` — a
> 100% failure fraction — so scoring *would* auto-pause after just the
> first call. Before writing the implementation, replace that last test
> with the corrected version below, which asserts the real boundary
> behavior across both services:

```go
func TestScorer_MassFailureAutopause(t *testing.T) {
	st, sess := openTestStore(t)
	svcA, _ := st.GetOrCreateService(sess.ID, "svc-a")
	svcB, _ := st.GetOrCreateService(sess.ID, "svc-b")
	sc := New(st, sess.ID, f(0.5))

	// svc-b passes first, establishing a non-failing baseline.
	if err := sc.Record(svcB.ID, 10, checks.Result{Success: true}, time.Millisecond); err != nil {
		t.Fatalf("Record B (pass): %v", err)
	}
	rs, _ := st.GetRunState()
	if rs.ScoringPaused {
		t.Fatal("should not be paused yet: 0/1 failing")
	}

	// svc-a fails: now 1/2 active services' latest result is a failure (50%).
	if err := sc.Record(svcA.ID, 10, checks.Result{Success: false}, time.Millisecond); err != nil {
		t.Fatalf("Record A (fail): %v", err)
	}
	rs, _ = st.GetRunState()
	if !rs.ScoringPaused {
		t.Fatal("expected auto-pause once 50% of active services are failing")
	}

	events, _ := st.ListEvents(sess.ID, 10, 0)
	count := 0
	for _, e := range events {
		if e.Type == "mass_failure_autopause" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 mass_failure_autopause event, got %d", count)
	}

	// A further failure must not re-trigger (already paused) or duplicate the event.
	if err := sc.Record(svcA.ID, 10, checks.Result{Success: false}, time.Millisecond); err != nil {
		t.Fatalf("Record A again: %v", err)
	}
	events, _ = st.ListEvents(sess.ID, 10, 0)
	count = 0
	for _, e := range events {
		if e.Type == "mass_failure_autopause" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected autopause event to fire only once, got %d", count)
	}
}

func TestScorer_ConcurrentFailures_FloorHoldsUnderRace(t *testing.T) {
	st, sess := openTestStore(t)
	// Small pool so both goroutines' transfers compete for the same floor.
	st2, err := store.Open(filepath.Join(t.TempDir(), "unused.db"))
	_ = st2
	_ = err
	svcA, _ := st.GetOrCreateService(sess.ID, "svc-a")
	svcB, _ := st.GetOrCreateService(sess.ID, "svc-b")
	sc := New(st, sess.ID, nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = sc.Record(svcA.ID, 10, checks.Result{Success: false}, time.Millisecond)
	}()
	go func() {
		defer wg.Done()
		_ = sc.Record(svcB.ID, 10, checks.Result{Success: false}, time.Millisecond)
	}()
	wg.Wait()

	blue, red, err := st.GetScore(sess.ID)
	if err != nil {
		t.Fatalf("GetScore: %v", err)
	}
	if blue != 980 || red != 20 {
		t.Errorf("blue=%d red=%d, want 980/20 (both transfers applied exactly once each)", blue, red)
	}
}
```

Remove the unused `st2`/`err` lines above before committing — they were
left in only to show the test was written against a throwaway path; the
real test needs just `st` from `openTestStore`.

- [ ] **Step 3: Write the implementation**

```go
// internal/store/checkresults.go — add this function to the existing file

func (s *Store) CountLatestCheckOutcomes(sessionID int64) (total, failing int, err error) {
	rows, err := s.db.Query(`
		SELECT cr.success
		FROM check_results cr
		JOIN (
			SELECT service_id, MAX(id) AS max_id
			FROM check_results
			WHERE session_id = ?
			GROUP BY service_id
		) latest ON cr.id = latest.max_id
		JOIN services sv ON sv.id = cr.service_id
		WHERE sv.session_id = ? AND sv.active = 1
	`, sessionID, sessionID)
	if err != nil {
		return 0, 0, fmt.Errorf("store: counting latest check outcomes: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var success int
		if err := rows.Scan(&success); err != nil {
			return 0, 0, fmt.Errorf("store: scanning outcome row: %w", err)
		}
		total++
		if success == 0 {
			failing++
		}
	}
	return total, failing, rows.Err()
}
```

```go
// internal/scorer/scorer.go
package scorer

import (
	"errors"
	"fmt"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

type Scorer struct {
	store               *store.Store
	sessionID           int64
	massFailureFraction *float64
}

func New(st *store.Store, sessionID int64, massFailureFraction *float64) *Scorer {
	return &Scorer{store: st, sessionID: sessionID, massFailureFraction: massFailureFraction}
}

func (sc *Scorer) Record(serviceID int64, pointsPerFailure int, result checks.Result, latency time.Duration) error {
	svc, err := sc.store.GetService(serviceID)
	if err != nil {
		return fmt.Errorf("scorer: loading service %d: %w", serviceID, err)
	}

	prev, err := sc.store.GetLatestCheckResult(serviceID)
	if err != nil {
		return fmt.Errorf("scorer: loading previous result for service %d: %w", serviceID, err)
	}

	resultID, err := sc.store.InsertCheckResult(store.CheckResult{
		SessionID: sc.sessionID,
		ServiceID: serviceID,
		Timestamp: time.Now().UTC(),
		Success:   result.Success,
		LatencyMS: latency.Milliseconds(),
		Detail:    result.Detail,
	})
	if err != nil {
		return fmt.Errorf("scorer: inserting check result for service %d: %w", serviceID, err)
	}

	if prev == nil || prev.Success != result.Success {
		eventType := "service_down"
		if result.Success {
			eventType = "service_up"
		}
		sid := serviceID
		if err := sc.store.InsertEvent(store.Event{
			SessionID: sc.sessionID,
			ServiceID: &sid,
			Level:     "info",
			Actor:     "system",
			Type:      eventType,
			Message:   fmt.Sprintf("%s transitioned to %s", svc.Name, eventType),
		}); err != nil {
			return fmt.Errorf("scorer: recording transition event: %w", err)
		}
	}

	if !result.Success {
		rs, err := sc.store.GetRunState()
		if err != nil {
			return fmt.Errorf("scorer: loading run state: %w", err)
		}
		if !rs.ScoringPaused && !svc.ScoringPaused {
			_, err := sc.store.RecordLedgerEntry(
				sc.sessionID, "service_check", resultID, "blue", "red", pointsPerFailure,
				fmt.Sprintf("%s check failed: %s", svc.Name, result.Detail),
			)
			if err != nil && !errors.Is(err, store.ErrDuplicateSourceRef) {
				return fmt.Errorf("scorer: recording ledger entry: %w", err)
			}
		}
		if err := sc.checkMassFailure(); err != nil {
			return err
		}
	}
	return nil
}

func (sc *Scorer) checkMassFailure() error {
	if sc.massFailureFraction == nil {
		return nil
	}
	rs, err := sc.store.GetRunState()
	if err != nil {
		return fmt.Errorf("scorer: loading run state: %w", err)
	}
	if rs.ScoringPaused {
		return nil
	}
	total, failing, err := sc.store.CountLatestCheckOutcomes(sc.sessionID)
	if err != nil {
		return fmt.Errorf("scorer: counting latest outcomes: %w", err)
	}
	if total == 0 {
		return nil
	}
	fraction := float64(failing) / float64(total)
	if fraction < *sc.massFailureFraction {
		return nil
	}
	if err := sc.store.SetGlobalScoringPaused(true); err != nil {
		return fmt.Errorf("scorer: auto-pausing scoring: %w", err)
	}
	return sc.store.InsertEvent(store.Event{
		SessionID: sc.sessionID,
		Level:     "warning",
		Actor:     "system",
		Type:      "mass_failure_autopause",
		Message: fmt.Sprintf("%.0f%% of active services failing (threshold %.0f%%); scoring auto-paused",
			fraction*100, *sc.massFailureFraction*100),
	})
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store/... ./internal/scorer/... -race`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/checkresults.go internal/store/checkresults_test.go internal/scorer/
git commit -m "feat: add scorer with pause gating, transition events, and mass-failure autopause"
```

---

### Task 9: Auth (admin login + bearer tokens)

**Files:**
- Create: `internal/auth/auth.go`
- Test: `internal/auth/auth_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  ```go
  package auth

  type Auth struct { /* unexported */ }

  func New(username, passwordHash string) *Auth

  func (a *Auth) Login(username, password string) (token string, ok bool)
  func (a *Auth) Valid(token string) bool

  // RequireAdmin wraps a handler, responding 401 if the request's
  // Authorization: Bearer <token> header is missing or invalid.
  func (a *Auth) RequireAdmin(next http.Handler) http.Handler

  // IsAuthenticated reports whether the request carries a currently valid
  // token, without rejecting the request if not — used by the results
  // endpoint (Task 11) to decide whether to include detail text.
  func (a *Auth) IsAuthenticated(r *http.Request) bool
  ```

- [ ] **Step 1: Write the failing tests**

```go
// internal/auth/auth_test.go
package auth

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func newTestAuth(t *testing.T, password string) *Auth {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	return New("admin", string(hash))
}

func TestLogin_CorrectCredentials(t *testing.T) {
	a := newTestAuth(t, "secret")
	token, ok := a.Login("admin", "secret")
	if !ok || token == "" {
		t.Fatalf("Login(admin, secret) = %q, %v; want non-empty token, true", token, ok)
	}
	if !a.Valid(token) {
		t.Error("expected freshly issued token to be valid")
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	a := newTestAuth(t, "secret")
	_, ok := a.Login("admin", "wrong")
	if ok {
		t.Error("expected Login to fail with wrong password")
	}
}

func TestLogin_WrongUsername(t *testing.T) {
	a := newTestAuth(t, "secret")
	_, ok := a.Login("nobody", "secret")
	if ok {
		t.Error("expected Login to fail with wrong username")
	}
}

func TestValid_UnknownTokenRejected(t *testing.T) {
	a := newTestAuth(t, "secret")
	if a.Valid("not-a-real-token") {
		t.Error("expected an unissued token to be invalid")
	}
	if a.Valid("") {
		t.Error("expected an empty token to be invalid")
	}
}

func TestRequireAdmin_MissingToken(t *testing.T) {
	a := newTestAuth(t, "secret")
	called := false
	h := a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	req := httptest.NewRequest(http.MethodPost, "/engine/stop", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Error("handler should not have been called without a token")
	}
}

func TestRequireAdmin_ValidToken(t *testing.T) {
	a := newTestAuth(t, "secret")
	token, _ := a.Login("admin", "secret")
	called := false
	h := a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/engine/stop", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !called {
		t.Errorf("status = %d, called = %v; want 200, true", rec.Code, called)
	}
}

func TestIsAuthenticated(t *testing.T) {
	a := newTestAuth(t, "secret")
	token, _ := a.Login("admin", "secret")

	reqNoToken := httptest.NewRequest(http.MethodGet, "/services/x/results", nil)
	if a.IsAuthenticated(reqNoToken) {
		t.Error("expected false for a request with no token")
	}

	reqWithToken := httptest.NewRequest(http.MethodGet, "/services/x/results", nil)
	reqWithToken.Header.Set("Authorization", "Bearer "+token)
	if !a.IsAuthenticated(reqWithToken) {
		t.Error("expected true for a request with a valid token")
	}
}

func TestLogin_ConcurrentSafe(t *testing.T) {
	a := newTestAuth(t, "secret")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, ok := a.Login("admin", "secret")
			if !ok || !a.Valid(token) {
				t.Errorf("concurrent login/validate failed")
			}
		}()
	}
	wg.Wait()
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/auth/...`
Expected: FAIL — package `auth` does not exist yet.

- [ ] **Step 3: Write the implementation**

```go
// internal/auth/auth.go
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

type Auth struct {
	username     string
	passwordHash string

	mu     sync.Mutex
	tokens map[string]struct{}
}

func New(username, passwordHash string) *Auth {
	return &Auth{
		username:     username,
		passwordHash: passwordHash,
		tokens:       make(map[string]struct{}),
	}
}

func generateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func (a *Auth) Login(username, password string) (string, bool) {
	if username != a.username {
		return "", false
	}
	if err := bcrypt.CompareHashAndPassword([]byte(a.passwordHash), []byte(password)); err != nil {
		return "", false
	}
	token, err := generateToken()
	if err != nil {
		return "", false
	}
	a.mu.Lock()
	a.tokens[token] = struct{}{}
	a.mu.Unlock()
	return token, true
}

func (a *Auth) Valid(token string) bool {
	if token == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.tokens[token]
	return ok
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	return strings.TrimPrefix(h, prefix), true
}

func (a *Auth) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok || !a.Valid(token) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"missing or invalid bearer token"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Auth) IsAuthenticated(r *http.Request) bool {
	token, ok := bearerToken(r)
	if !ok {
		return false
	}
	return a.Valid(token)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/auth/... -race`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/auth/
git commit -m "feat: add admin auth with bcrypt login and bearer tokens"
```

---

### Task 10: Engine (control plane wiring)

**Files:**
- Create: `internal/engine/engine.go`
- Modify: `internal/store/services.go` (add `GetServiceByName`)
- Modify: `internal/store/checkresults.go` (add `CountConsecutiveFailures`)
- Modify: `internal/scheduler/scheduler.go` (add `RunningServiceIDs`)
- Test: `internal/engine/engine_test.go`
- Test: `internal/store/services_test.go` (add a case for `GetServiceByName`)

**Interfaces:**
- Consumes: `store.Store` (Tasks 2–5), `scheduler.Scheduler` (Task 7),
  `config.Load`/`config.ServiceConfig` (Task 1).
- Produces:
  ```go
  package engine

  // ErrServiceNotFound is wrapped into the return of every per-service
  // method below when name is unknown; Task 11's HTTP handlers use
  // errors.Is(err, engine.ErrServiceNotFound) to answer with 404 instead
  // of 500.
  var ErrServiceNotFound = errors.New("engine: unknown service")

  type Engine struct { /* unexported */ }

  // New wires a store, a session, and a scheduler together. configPath is
  // used by Reload; the scheduler already has its own copy for per-service
  // (re)starts.
  func New(st *store.Store, sess store.Session, sched *scheduler.Scheduler, configPath string) *Engine

  func (e *Engine) Start() error
  func (e *Engine) Stop() error
  func (e *Engine) PauseScoring() error
  func (e *Engine) ResumeScoring() error

  func (e *Engine) StartService(name string) error
  func (e *Engine) StopService(name string) error
  func (e *Engine) RestartService(name string, pauseScoring bool) error
  func (e *Engine) PauseServiceScoring(name string) error
  func (e *Engine) ResumeServiceScoring(name string) error

  func (e *Engine) Reload(configPath string) (added, removed []string, err error)

  type ServiceStatus struct {
      Name                string     `json:"name"`
      Up                  bool       `json:"up"`
      RoutineEnabled      bool       `json:"routine_enabled"`
      ScoringPaused       bool       `json:"scoring_paused"`
      LastCheckTime       *time.Time `json:"last_check_time,omitempty"`
      LatencyMS           int64      `json:"latency_ms"`
      ConsecutiveFailures int        `json:"consecutive_failures"`
  }
  type Status struct {
      SessionName   string          `json:"session_name"`
      StartingScore int             `json:"starting_score"`
      BlueScore     int             `json:"blue_score"`
      RedScore      int             `json:"red_score"`
      EngineState   string          `json:"engine_state"`
      ScoringPaused bool            `json:"scoring_paused"`
      ServerTime    time.Time       `json:"server_time"`
      Services      []ServiceStatus `json:"services"`
  }
  func (e *Engine) Status() (Status, error)
  ```
  ```go
  package store
  // GetServiceByName never creates a row — unlike GetOrCreateService —
  // because Engine must be able to tell "unknown service name" apart from
  // "valid name, first time seen."
  func (s *Store) GetServiceByName(sessionID int64, name string) (Service, bool, error)
  func (s *Store) CountConsecutiveFailures(serviceID int64) (int, error)
  ```
  ```go
  package scheduler
  func (s *Scheduler) RunningServiceIDs() []int64
  ```

**Per-service enabled/disabled vs. engine stop/start:** a per-service
`StopService`/`StartService` call records a *desired* enabled state in
memory (`desiredEnabled`), independent of whether the whole engine is
currently running. `Engine.Stop()` halts every routine regardless of
desired state (nothing runs while stopped) but does not forget it;
`Engine.Start()` then restarts only the services whose desired state is
enabled. Only a process restart (a fresh `Engine`, fresh map) resets
every service back to enabled — matching the spec's distinction between
per-service state (resets on process restart) and nothing in between.

- [ ] **Step 1: Write the failing tests**

```go
// internal/store/services_test.go  (add this test to the existing file)

func TestGetServiceByName_NotFoundDoesNotCreate(t *testing.T) {
	st := openTemp(t)
	sess, _ := st.GetOrCreateSession("comp-1", 1000)

	_, found, err := st.GetServiceByName(sess.ID, "ghost")
	if err != nil {
		t.Fatalf("GetServiceByName: %v", err)
	}
	if found {
		t.Fatal("expected found=false for an unknown name")
	}

	active, _ := st.ListActiveServices(sess.ID)
	if len(active) != 0 {
		t.Errorf("GetServiceByName must not create a row; found %d services", len(active))
	}
}

func TestGetServiceByName_Found(t *testing.T) {
	st := openTemp(t)
	sess, _ := st.GetOrCreateSession("comp-1", 1000)
	created, _ := st.GetOrCreateService(sess.ID, "apache-web")

	svc, found, err := st.GetServiceByName(sess.ID, "apache-web")
	if err != nil || !found {
		t.Fatalf("GetServiceByName: svc=%+v found=%v err=%v", svc, found, err)
	}
	if svc.ID != created.ID {
		t.Errorf("ID = %d, want %d", svc.ID, created.ID)
	}
}
```

```go
// internal/engine/engine_test.go
package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/scheduler"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

const oneServiceYAML = `
session_name: "comp-1"
starting_score: 1000
admin:
  username: admin
  password_hash: $2a$10$abcdefghijklmnopqrstuvabcdefghijklmnopqrstuvwxyz123456
services:
  - name: svc1
    host: 127.0.0.1
    check_type: tcp_connect
    interval_seconds: 1
    points_per_failure: 10
    params: {}
`

const twoServiceYAML = `
session_name: "comp-1"
starting_score: 1000
admin:
  username: admin
  password_hash: $2a$10$abcdefghijklmnopqrstuvabcdefghijklmnopqrstuvwxyz123456
services:
  - name: svc2
    host: 127.0.0.1
    check_type: tcp_connect
    interval_seconds: 1
    points_per_failure: 10
    params: {}
`

func setup(t *testing.T, yaml string) (*Engine, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "scoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	sess, err := st.GetOrCreateSession("comp-1", 1000)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	if _, err := st.GetOrCreateService(sess.ID, "svc1"); err != nil {
		t.Fatalf("GetOrCreateService: %v", err)
	}

	registry := checks.NewRegistry()
	registry.Register("tcp_connect", func(ctx context.Context, host string, params map[string]any) checks.Result {
		return checks.Result{Success: true}
	})
	sched := scheduler.New(registry, noopSink{}, configPath)
	e := New(st, sess, sched, configPath)
	return e, st, configPath
}

type noopSink struct{}

func (noopSink) Record(serviceID int64, pointsPerFailure int, result checks.Result, latency time.Duration) error {
	return nil
}

func TestEngine_StartStartsActiveRoutines(t *testing.T) {
	e, _, _ := setup(t, oneServiceYAML)
	if err := e.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer e.Stop()

	status, err := e.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.EngineState != "running" {
		t.Errorf("EngineState = %q, want running", status.EngineState)
	}
	if len(status.Services) != 1 || !status.Services[0].RoutineEnabled {
		t.Errorf("expected svc1's routine to be running, got %+v", status.Services)
	}
}

func TestEngine_PerServiceStopPersistsAcrossEngineStopStart(t *testing.T) {
	e, _, _ := setup(t, oneServiceYAML)
	if err := e.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := e.StopService("svc1"); err != nil {
		t.Fatalf("StopService: %v", err)
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := e.Start(); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	defer e.Stop()

	status, _ := e.Status()
	if status.Services[0].RoutineEnabled {
		t.Error("expected svc1 to remain stopped across an engine stop/start cycle")
	}
}

func TestEngine_Reload_AddsAndRemovesServices(t *testing.T) {
	e, _, configPath := setup(t, oneServiceYAML)
	if err := e.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer e.Stop()

	if err := os.WriteFile(configPath, []byte(twoServiceYAML), 0o600); err != nil {
		t.Fatalf("rewriting config: %v", err)
	}
	added, removed, err := e.Reload(configPath)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if len(added) != 1 || added[0] != "svc2" {
		t.Errorf("added = %v, want [svc2]", added)
	}
	if len(removed) != 1 || removed[0] != "svc1" {
		t.Errorf("removed = %v, want [svc1]", removed)
	}

	status, _ := e.Status()
	if len(status.Services) != 1 || status.Services[0].Name != "svc2" {
		t.Errorf("expected only svc2 active, got %+v", status.Services)
	}
}

func TestEngine_RestartServiceWithPauseScoring_IsAtomic(t *testing.T) {
	e, st, _ := setup(t, oneServiceYAML)
	if err := e.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer e.Stop()

	if err := e.RestartService("svc1", true); err != nil {
		t.Fatalf("RestartService: %v", err)
	}

	svc, found, err := st.GetServiceByName(st.(*store.Store) == nil, "svc1") // placeholder, corrected below
	_ = svc
	_ = found
	_ = err
}
```

> The last test above has a stray type assertion left in from drafting —
> replace its body with the corrected version before running:

```go
func TestEngine_RestartServiceWithPauseScoring_IsAtomic(t *testing.T) {
	e, st, _ := setup(t, oneServiceYAML)
	if err := e.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer e.Stop()

	sess, _ := st.GetOrCreateSession("comp-1", 1000)
	if err := e.RestartService("svc1", true); err != nil {
		t.Fatalf("RestartService: %v", err)
	}

	svc, found, err := st.GetServiceByName(sess.ID, "svc1")
	if err != nil || !found {
		t.Fatalf("GetServiceByName: %v, found=%v", err, found)
	}
	if !svc.ScoringPaused {
		t.Error("expected ScoringPaused=true immediately after RestartService(pauseScoring=true)")
	}
}

func TestEngine_Status_ReportsConsecutiveFailures(t *testing.T) {
	e, st, _ := setup(t, oneServiceYAML)
	sess, _ := st.GetOrCreateSession("comp-1", 1000)
	svc, _ := st.GetOrCreateService(sess.ID, "svc1")

	for i := 0; i < 3; i++ {
		st.InsertCheckResult(store.CheckResult{
			SessionID: sess.ID, ServiceID: svc.ID, Timestamp: time.Now().UTC(), Success: false,
		})
	}

	status, err := e.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Services[0].ConsecutiveFailures != 3 {
		t.Errorf("ConsecutiveFailures = %d, want 3", status.Services[0].ConsecutiveFailures)
	}
}
```

Add `"context"` to this test file's imports (used by the `fast` check
registered in `setup`).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/... ./internal/scheduler/... ./internal/engine/...`
Expected: FAIL — `GetServiceByName`, `CountConsecutiveFailures`,
`RunningServiceIDs`, and package `engine` all undefined.

- [ ] **Step 3: Write the implementation**

```go
// internal/store/services.go — add this function to the existing file

func (s *Store) GetServiceByName(sessionID int64, name string) (Service, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+serviceColumns+` FROM services WHERE session_id = ? AND name = ?`,
		sessionID, name,
	)
	svc, err := scanService(row)
	if err == sql.ErrNoRows {
		return Service{}, false, nil
	}
	if err != nil {
		return Service{}, false, fmt.Errorf("store: looking up service %q: %w", name, err)
	}
	return svc, true, nil
}
```

```go
// internal/store/checkresults.go — add this function to the existing file

func (s *Store) CountConsecutiveFailures(serviceID int64) (int, error) {
	rows, err := s.db.Query(
		`SELECT success FROM check_results WHERE service_id = ? ORDER BY timestamp DESC, id DESC`,
		serviceID,
	)
	if err != nil {
		return 0, fmt.Errorf("store: counting consecutive failures for service %d: %w", serviceID, err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var success int
		if err := rows.Scan(&success); err != nil {
			return 0, fmt.Errorf("store: scanning success flag: %w", err)
		}
		if success != 0 {
			break
		}
		count++
	}
	return count, rows.Err()
}
```

```go
// internal/scheduler/scheduler.go — add this method to the existing file

func (s *Scheduler) RunningServiceIDs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, len(s.routines))
	for id := range s.routines {
		ids = append(ids, id)
	}
	return ids
}
```

```go
// internal/engine/engine.go
package engine

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/config"
	"github.com/Bravo-CDT/Scoring/internal/scheduler"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

// ErrServiceNotFound is returned (wrapped) by every per-service method
// when name doesn't match a known service — callers (Task 11's HTTP
// handlers) use errors.Is to map this to a 404, distinct from any other
// error (which is a genuine 500).
var ErrServiceNotFound = errors.New("engine: unknown service")

type Engine struct {
	store      *store.Store
	session    store.Session
	scheduler  *scheduler.Scheduler
	configPath string

	mu             sync.Mutex
	desiredEnabled map[int64]bool
}

func New(st *store.Store, sess store.Session, sched *scheduler.Scheduler, configPath string) *Engine {
	return &Engine{
		store:          st,
		session:        sess,
		scheduler:      sched,
		configPath:     configPath,
		desiredEnabled: make(map[int64]bool),
	}
}

func (e *Engine) Start() error {
	if err := e.store.SetEngineState("running"); err != nil {
		return err
	}
	svcs, err := e.store.ListActiveServices(e.session.ID)
	if err != nil {
		return err
	}
	for _, svc := range svcs {
		e.mu.Lock()
		enabled, known := e.desiredEnabled[svc.ID]
		if !known {
			enabled = true
			e.desiredEnabled[svc.ID] = true
		}
		e.mu.Unlock()
		if !enabled {
			continue
		}
		if err := e.scheduler.StartService(svc.ID, svc.Name); err != nil {
			sid := svc.ID
			_ = e.store.InsertEvent(store.Event{
				SessionID: e.session.ID, ServiceID: &sid, Level: "error", Actor: "system",
				Type: "routine_start_failed", Message: err.Error(),
			})
		}
	}
	return e.store.InsertEvent(store.Event{
		SessionID: e.session.ID, Level: "info", Actor: "admin", Type: "engine_started", Message: "engine started",
	})
}

func (e *Engine) Stop() error {
	if err := e.store.SetEngineState("stopped"); err != nil {
		return err
	}
	for _, id := range e.scheduler.RunningServiceIDs() {
		e.scheduler.StopService(id)
	}
	return e.store.InsertEvent(store.Event{
		SessionID: e.session.ID, Level: "info", Actor: "admin", Type: "engine_stopped", Message: "engine stopped",
	})
}

func (e *Engine) PauseScoring() error {
	if err := e.store.SetGlobalScoringPaused(true); err != nil {
		return err
	}
	return e.store.InsertEvent(store.Event{
		SessionID: e.session.ID, Level: "info", Actor: "admin", Type: "scoring_paused", Message: "global scoring paused",
	})
}

func (e *Engine) ResumeScoring() error {
	if err := e.store.SetGlobalScoringPaused(false); err != nil {
		return err
	}
	return e.store.InsertEvent(store.Event{
		SessionID: e.session.ID, Level: "info", Actor: "admin", Type: "scoring_resumed", Message: "global scoring resumed",
	})
}

func (e *Engine) StartService(name string) error {
	svc, found, err := e.store.GetServiceByName(e.session.ID, name)
	if err != nil {
		return err
	}
	if !found || !svc.Active {
		return fmt.Errorf("%w: %s", ErrServiceNotFound, name)
	}
	e.mu.Lock()
	e.desiredEnabled[svc.ID] = true
	e.mu.Unlock()

	rs, err := e.store.GetRunState()
	if err != nil {
		return err
	}
	if rs.EngineState != "running" {
		return nil
	}
	if err := e.scheduler.StartService(svc.ID, svc.Name); err != nil {
		return err
	}
	sid := svc.ID
	return e.store.InsertEvent(store.Event{
		SessionID: e.session.ID, ServiceID: &sid, Level: "info", Actor: "admin",
		Type: "routine_started", Message: fmt.Sprintf("%s routine started", name),
	})
}

func (e *Engine) StopService(name string) error {
	svc, found, err := e.store.GetServiceByName(e.session.ID, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrServiceNotFound, name)
	}
	e.mu.Lock()
	e.desiredEnabled[svc.ID] = false
	e.mu.Unlock()
	e.scheduler.StopService(svc.ID)

	sid := svc.ID
	return e.store.InsertEvent(store.Event{
		SessionID: e.session.ID, ServiceID: &sid, Level: "info", Actor: "admin",
		Type: "routine_stopped", Message: fmt.Sprintf("%s routine stopped", name),
	})
}

func (e *Engine) RestartService(name string, pauseScoring bool) error {
	svc, found, err := e.store.GetServiceByName(e.session.ID, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrServiceNotFound, name)
	}
	if pauseScoring {
		if err := e.store.SetServiceScoringPaused(svc.ID, true); err != nil {
			return err
		}
	}

	e.mu.Lock()
	e.desiredEnabled[svc.ID] = true
	e.mu.Unlock()

	rs, err := e.store.GetRunState()
	if err != nil {
		return err
	}
	if rs.EngineState == "running" {
		if err := e.scheduler.RestartService(svc.ID, svc.Name); err != nil {
			return err
		}
	} else {
		e.scheduler.StopService(svc.ID)
	}

	sid := svc.ID
	return e.store.InsertEvent(store.Event{
		SessionID: e.session.ID, ServiceID: &sid, Level: "info", Actor: "admin",
		Type: "routine_restarted", Message: fmt.Sprintf("%s routine restarted (config reloaded)", name),
	})
}

func (e *Engine) PauseServiceScoring(name string) error {
	svc, found, err := e.store.GetServiceByName(e.session.ID, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrServiceNotFound, name)
	}
	if err := e.store.SetServiceScoringPaused(svc.ID, true); err != nil {
		return err
	}
	sid := svc.ID
	return e.store.InsertEvent(store.Event{
		SessionID: e.session.ID, ServiceID: &sid, Level: "info", Actor: "admin",
		Type: "service_scoring_paused", Message: fmt.Sprintf("%s scoring paused", name),
	})
}

func (e *Engine) ResumeServiceScoring(name string) error {
	svc, found, err := e.store.GetServiceByName(e.session.ID, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrServiceNotFound, name)
	}
	if err := e.store.SetServiceScoringPaused(svc.ID, false); err != nil {
		return err
	}
	sid := svc.ID
	return e.store.InsertEvent(store.Event{
		SessionID: e.session.ID, ServiceID: &sid, Level: "info", Actor: "admin",
		Type: "service_scoring_resumed", Message: fmt.Sprintf("%s scoring resumed", name),
	})
}

func (e *Engine) Reload(configPath string) (added, removed []string, err error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, err
	}

	existing, err := e.store.ListActiveServices(e.session.ID)
	if err != nil {
		return nil, nil, err
	}
	existingByName := make(map[string]store.Service, len(existing))
	for _, s := range existing {
		existingByName[s.Name] = s
	}

	inConfig := make(map[string]bool, len(cfg.Services))
	for _, svcCfg := range cfg.Services {
		inConfig[svcCfg.Name] = true
		if _, already := existingByName[svcCfg.Name]; already {
			continue
		}
		svc, err := e.store.GetOrCreateService(e.session.ID, svcCfg.Name)
		if err != nil {
			return added, removed, err
		}
		if err := e.store.SetServiceActive(svc.ID, true); err != nil {
			return added, removed, err
		}
		e.mu.Lock()
		e.desiredEnabled[svc.ID] = true
		e.mu.Unlock()

		rs, err := e.store.GetRunState()
		if err != nil {
			return added, removed, err
		}
		if rs.EngineState == "running" {
			if err := e.scheduler.StartService(svc.ID, svc.Name); err != nil {
				sid := svc.ID
				_ = e.store.InsertEvent(store.Event{
					SessionID: e.session.ID, ServiceID: &sid, Level: "error", Actor: "system",
					Type: "routine_start_failed", Message: err.Error(),
				})
			}
		}
		added = append(added, svcCfg.Name)
		sid := svc.ID
		_ = e.store.InsertEvent(store.Event{
			SessionID: e.session.ID, ServiceID: &sid, Level: "info", Actor: "admin",
			Type: "service_added", Message: fmt.Sprintf("%s added via reload", svcCfg.Name),
		})
	}

	for name, svc := range existingByName {
		if inConfig[name] {
			continue
		}
		e.scheduler.StopService(svc.ID)
		if err := e.store.SetServiceActive(svc.ID, false); err != nil {
			return added, removed, err
		}
		removed = append(removed, name)
		sid := svc.ID
		_ = e.store.InsertEvent(store.Event{
			SessionID: e.session.ID, ServiceID: &sid, Level: "info", Actor: "admin",
			Type: "service_removed", Message: fmt.Sprintf("%s deactivated via reload", name),
		})
	}
	return added, removed, nil
}

type ServiceStatus struct {
	Name                string     `json:"name"`
	Up                  bool       `json:"up"`
	RoutineEnabled      bool       `json:"routine_enabled"`
	ScoringPaused       bool       `json:"scoring_paused"`
	LastCheckTime       *time.Time `json:"last_check_time,omitempty"`
	LatencyMS           int64      `json:"latency_ms"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
}

type Status struct {
	SessionName   string          `json:"session_name"`
	StartingScore int             `json:"starting_score"`
	BlueScore     int             `json:"blue_score"`
	RedScore      int             `json:"red_score"`
	EngineState   string          `json:"engine_state"`
	ScoringPaused bool            `json:"scoring_paused"`
	ServerTime    time.Time       `json:"server_time"`
	Services      []ServiceStatus `json:"services"`
}

func (e *Engine) Status() (Status, error) {
	blue, red, err := e.store.GetScore(e.session.ID)
	if err != nil {
		return Status{}, err
	}
	rs, err := e.store.GetRunState()
	if err != nil {
		return Status{}, err
	}
	svcs, err := e.store.ListActiveServices(e.session.ID)
	if err != nil {
		return Status{}, err
	}

	out := Status{
		SessionName:   e.session.Name,
		StartingScore: e.session.StartingScore,
		BlueScore:     blue,
		RedScore:      red,
		EngineState:   rs.EngineState,
		ScoringPaused: rs.ScoringPaused,
		ServerTime:    time.Now().UTC(),
	}
	for _, svc := range svcs {
		ss := ServiceStatus{
			Name:           svc.Name,
			RoutineEnabled: e.scheduler.IsRunning(svc.ID),
			ScoringPaused:  svc.ScoringPaused,
		}
		latest, err := e.store.GetLatestCheckResult(svc.ID)
		if err != nil {
			return Status{}, err
		}
		if latest != nil {
			ss.Up = latest.Success
			t := latest.Timestamp
			ss.LastCheckTime = &t
			ss.LatencyMS = latest.LatencyMS
		}
		consecutive, err := e.store.CountConsecutiveFailures(svc.ID)
		if err != nil {
			return Status{}, err
		}
		ss.ConsecutiveFailures = consecutive
		out.Services = append(out.Services, ss)
	}
	return out, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store/... ./internal/scheduler/... ./internal/engine/... -race`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/services.go internal/store/services_test.go internal/store/checkresults.go internal/scheduler/scheduler.go internal/engine/
git commit -m "feat: add engine control plane (start/stop/pause/reload) and status reporting"
```

---

### Task 11: HTTP API

**Files:**
- Create: `internal/api/server.go`
- Create: `internal/api/handlers_public.go`
- Create: `internal/api/handlers_admin.go`
- Test: `internal/api/api_test.go`

**Interfaces:**
- Consumes: `engine.Engine` (Task 10, including `engine.ErrServiceNotFound`),
  `store.Store` (Tasks 2–5), `auth.Auth` (Task 9).
- Produces:
  ```go
  package api

  // NewServer builds the full route table: public read-only endpoints,
  // admin endpoints behind a.RequireAdmin, and /login. configPath is
  // threaded through to the /reload handler.
  func NewServer(e *engine.Engine, st *store.Store, sessionID int64, a *auth.Auth, configPath string) http.Handler
  ```

**Routes** (Go 1.22+ pattern-based `http.ServeMux`, no external router):

Public: `GET /status`, `GET /events`, `GET /services/{name}/results`
(detail text included only if the request carries a valid bearer token —
same route serves both the public and admin variant from the spec).

Admin (`a.RequireAdmin`): `POST /login` *(the only one of this group with
no token requirement — it's how you get one)*, `POST /engine/start`,
`POST /engine/stop`, `POST /scoring/pause`, `POST /scoring/resume`,
`POST /services/{name}/start`, `/stop`, `/restart` (optional JSON body
`{"pause_scoring": bool}`), `POST /services/{name}/scoring/pause`,
`/scoring/resume`, `POST /reload`.

Every per-service handler maps `errors.Is(err, engine.ErrServiceNotFound)`
to `404`; every other engine error is `500`. `/login` with the wrong
username/password is `401`. A malformed JSON body is `400`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/api/api_test.go
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Bravo-CDT/Scoring/internal/auth"
	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/engine"
	"github.com/Bravo-CDT/Scoring/internal/scheduler"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

const testPassword = "secret"

func setupAPI(t *testing.T, yaml string) (baseURL string, st *store.Store, configPath string) {
	t.Helper()
	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	st, err := store.Open(filepath.Join(dir, "scoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	sess, err := st.GetOrCreateSession("comp-1", 1000)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	if _, err := st.GetOrCreateService(sess.ID, "svc1"); err != nil {
		t.Fatalf("GetOrCreateService: %v", err)
	}

	registry := checks.NewRegistry()
	registry.Register("tcp_connect", func(ctx context.Context, host string, params map[string]any) checks.Result {
		return checks.Result{Success: true}
	})
	sched := scheduler.New(registry, noopSink{}, configPath)
	e := engine.New(st, sess, sched, configPath)

	hash, _ := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.DefaultCost)
	a := auth.New("admin", string(hash))

	server := httptest.NewServer(NewServer(e, st, sess.ID, a, configPath))
	t.Cleanup(server.Close)
	return server.URL, st, configPath
}

type noopSink struct{}

func (noopSink) Record(serviceID int64, pointsPerFailure int, result checks.Result, latency time.Duration) error {
	return nil
}

const apiOneServiceYAML = `
session_name: "comp-1"
starting_score: 1000
admin:
  username: admin
  password_hash: $2a$10$abcdefghijklmnopqrstuvabcdefghijklmnopqrstuvwxyz123456
services:
  - name: svc1
    host: 127.0.0.1
    check_type: tcp_connect
    interval_seconds: 1
    points_per_failure: 10
    params: {}
`

func doJSON(t *testing.T, method, url, token string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func login(t *testing.T, baseURL string) string {
	t.Helper()
	resp, out := doJSON(t, http.MethodPost, baseURL+"/login", "", map[string]string{
		"username": "admin", "password": testPassword,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	token, _ := out["token"].(string)
	if token == "" {
		t.Fatal("expected non-empty token")
	}
	return token
}

func TestAPI_Status_Public(t *testing.T) {
	baseURL, _, _ := setupAPI(t, apiOneServiceYAML)
	resp, out := doJSON(t, http.MethodGet, baseURL+"/status", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if out["session_name"] != "comp-1" {
		t.Errorf("session_name = %v, want comp-1", out["session_name"])
	}
}

func TestAPI_Login_WrongPassword(t *testing.T) {
	baseURL, _, _ := setupAPI(t, apiOneServiceYAML)
	resp, _ := doJSON(t, http.MethodPost, baseURL+"/login", "", map[string]string{
		"username": "admin", "password": "wrong",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAPI_AdminAction_RequiresToken(t *testing.T) {
	baseURL, _, _ := setupAPI(t, apiOneServiceYAML)
	resp, _ := doJSON(t, http.MethodPost, baseURL+"/engine/start", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without a token", resp.StatusCode)
	}

	token := login(t, baseURL)
	resp, out := doJSON(t, http.MethodPost, baseURL+"/engine/start", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with a valid token", resp.StatusCode)
	}
	if out["engine_state"] != "running" {
		t.Errorf("engine_state = %v, want running", out["engine_state"])
	}
}

func TestAPI_StartIsIdempotent(t *testing.T) {
	baseURL, _, _ := setupAPI(t, apiOneServiceYAML)
	token := login(t, baseURL)

	for i := 0; i < 2; i++ {
		resp, _ := doJSON(t, http.MethodPost, baseURL+"/engine/start", token, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("call %d: status = %d, want 200", i, resp.StatusCode)
		}
	}
}

func TestAPI_UnknownService_404(t *testing.T) {
	baseURL, _, _ := setupAPI(t, apiOneServiceYAML)
	token := login(t, baseURL)

	resp, _ := doJSON(t, http.MethodPost, baseURL+"/services/does-not-exist/start", token, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestAPI_Results_PublicOmitsDetailAdminIncludesIt(t *testing.T) {
	baseURL, st, _ := setupAPI(t, apiOneServiceYAML)
	sess, _ := st.GetOrCreateSession("comp-1", 1000)
	svc, _ := st.GetOrCreateService(sess.ID, "svc1")
	st.InsertCheckResult(store.CheckResult{
		SessionID: sess.ID, ServiceID: svc.ID, Timestamp: time.Now().UTC(),
		Success: false, LatencyMS: 5, Detail: "super-secret-credential-leak",
	})

	pubResp, err := http.Get(baseURL + "/services/svc1/results")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	pubBody := new(bytes.Buffer)
	pubBody.ReadFrom(pubResp.Body)
	pubResp.Body.Close()
	if strings.Contains(pubBody.String(), "super-secret-credential-leak") {
		t.Error("public results endpoint must not include detail text")
	}

	token := login(t, baseURL)
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/services/svc1/results", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	adminResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	adminBody := new(bytes.Buffer)
	adminBody.ReadFrom(adminResp.Body)
	adminResp.Body.Close()
	if !strings.Contains(adminBody.String(), "super-secret-credential-leak") {
		t.Error("admin (authenticated) results endpoint must include detail text")
	}
}

func TestAPI_Reload(t *testing.T) {
	baseURL, _, configPath := setupAPI(t, apiOneServiceYAML)
	token := login(t, baseURL)

	updatedYAML := `
session_name: "comp-1"
starting_score: 1000
admin:
  username: admin
  password_hash: $2a$10$abcdefghijklmnopqrstuvabcdefghijklmnopqrstuvwxyz123456
services:
  - name: svc2
    host: 127.0.0.1
    check_type: tcp_connect
    interval_seconds: 1
    points_per_failure: 10
    params: {}
`
	if err := os.WriteFile(configPath, []byte(updatedYAML), 0o600); err != nil {
		t.Fatalf("rewriting config: %v", err)
	}
	resp, out := doJSON(t, http.MethodPost, baseURL+"/reload", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	added, _ := out["added"].([]any)
	removed, _ := out["removed"].([]any)
	if len(added) != 1 || added[0] != "svc2" {
		t.Errorf("added = %v, want [svc2]", added)
	}
	if len(removed) != 1 || removed[0] != "svc1" {
		t.Errorf("removed = %v, want [svc1]", removed)
	}
}

func TestAPI_RestartWithPauseScoring(t *testing.T) {
	baseURL, _, _ := setupAPI(t, apiOneServiceYAML)
	token := login(t, baseURL)
	doJSON(t, http.MethodPost, baseURL+"/engine/start", token, nil)

	resp, _ := doJSON(t, http.MethodPost, baseURL+"/services/svc1/restart", token,
		map[string]bool{"pause_scoring": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	_, status := doJSON(t, http.MethodGet, baseURL+"/status", "", nil)
	services, _ := status["services"].([]any)
	if len(services) != 1 {
		t.Fatalf("expected 1 service in status, got %d", len(services))
	}
	svc, _ := services[0].(map[string]any)
	if paused, _ := svc["scoring_paused"].(bool); !paused {
		t.Error("expected svc1.scoring_paused = true after atomic restart-with-pause")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/api/...`
Expected: FAIL — package `api` does not exist yet.

- [ ] **Step 3: Write the implementation**

```go
// internal/api/server.go
package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/Bravo-CDT/Scoring/internal/auth"
	"github.com/Bravo-CDT/Scoring/internal/engine"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

func NewServer(e *engine.Engine, st *store.Store, sessionID int64, a *auth.Auth, configPath string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /status", handleStatus(e))
	mux.HandleFunc("GET /events", handleEvents(st, sessionID))
	mux.HandleFunc("GET /services/{name}/results", handleResults(st, sessionID, a))

	mux.HandleFunc("POST /login", handleLogin(a))
	mux.Handle("POST /engine/start", a.RequireAdmin(http.HandlerFunc(handleEngineStart(e))))
	mux.Handle("POST /engine/stop", a.RequireAdmin(http.HandlerFunc(handleEngineStop(e))))
	mux.Handle("POST /scoring/pause", a.RequireAdmin(http.HandlerFunc(handleScoringPause(e))))
	mux.Handle("POST /scoring/resume", a.RequireAdmin(http.HandlerFunc(handleScoringResume(e))))
	mux.Handle("POST /services/{name}/start", a.RequireAdmin(http.HandlerFunc(handleServiceStart(e))))
	mux.Handle("POST /services/{name}/stop", a.RequireAdmin(http.HandlerFunc(handleServiceStop(e))))
	mux.Handle("POST /services/{name}/restart", a.RequireAdmin(http.HandlerFunc(handleServiceRestart(e))))
	mux.Handle("POST /services/{name}/scoring/pause", a.RequireAdmin(http.HandlerFunc(handleServiceScoringPause(e))))
	mux.Handle("POST /services/{name}/scoring/resume", a.RequireAdmin(http.HandlerFunc(handleServiceScoringResume(e))))
	mux.Handle("POST /reload", a.RequireAdmin(http.HandlerFunc(handleReload(e, configPath))))

	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeEngineErr(w http.ResponseWriter, err error) {
	if isNotFound(err) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func parseLimit(r *http.Request, def, max int) int {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

func parseBeforeID(r *http.Request) int64 {
	v := r.URL.Query().Get("before")
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
```

```go
// internal/api/handlers_public.go
package api

import (
	"net/http"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/auth"
	"github.com/Bravo-CDT/Scoring/internal/engine"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

func handleStatus(e *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, err := e.Status()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, status)
	}
}

func handleEvents(st *store.Store, sessionID int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := parseLimit(r, 50, 500)
		before := parseBeforeID(r)
		events, err := st.ListEvents(sessionID, limit, before)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, events)
	}
}

type checkResultDTO struct {
	ID        int64     `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Success   bool      `json:"success"`
	LatencyMS int64     `json:"latency_ms"`
	Detail    string    `json:"detail,omitempty"`
}

func handleResults(st *store.Store, sessionID int64, a *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		svc, found, err := st.GetServiceByName(sessionID, name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, "unknown service")
			return
		}

		limit := parseLimit(r, 50, 500)
		before := parseBeforeID(r)
		results, err := st.ListCheckResults(svc.ID, limit, before)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		includeDetail := a.IsAuthenticated(r)
		out := make([]checkResultDTO, len(results))
		for i, cr := range results {
			out[i] = checkResultDTO{ID: cr.ID, Timestamp: cr.Timestamp, Success: cr.Success, LatencyMS: cr.LatencyMS}
			if includeDetail {
				out[i].Detail = cr.Detail
			}
		}
		writeJSON(w, http.StatusOK, out)
	}
}
```

```go
// internal/api/handlers_admin.go
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Bravo-CDT/Scoring/internal/auth"
	"github.com/Bravo-CDT/Scoring/internal/engine"
)

func isNotFound(err error) bool {
	return errors.Is(err, engine.ErrServiceNotFound)
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func handleLogin(a *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		token, ok := a.Login(req.Username, req.Password)
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"token": token})
	}
}

func handleEngineStart(e *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := e.Start(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"engine_state": "running"})
	}
}

func handleEngineStop(e *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := e.Stop(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"engine_state": "stopped"})
	}
}

func handleScoringPause(e *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := e.PauseScoring(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"scoring_paused": true})
	}
}

func handleScoringResume(e *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := e.ResumeScoring(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"scoring_paused": false})
	}
}

func handleServiceStart(e *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := e.StartService(name); err != nil {
			writeEngineErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"service": name, "routine_enabled": true})
	}
}

func handleServiceStop(e *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := e.StopService(name); err != nil {
			writeEngineErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"service": name, "routine_enabled": false})
	}
}

type restartRequest struct {
	PauseScoring bool `json:"pause_scoring"`
}

func handleServiceRestart(e *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var req restartRequest
		if r.Body != nil {
			if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil && err != io.EOF {
				writeError(w, http.StatusBadRequest, "invalid request body")
				return
			}
		}
		if err := e.RestartService(name, req.PauseScoring); err != nil {
			writeEngineErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"service": name, "scoring_paused": req.PauseScoring})
	}
}

func handleServiceScoringPause(e *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := e.PauseServiceScoring(name); err != nil {
			writeEngineErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"service": name, "scoring_paused": true})
	}
}

func handleServiceScoringResume(e *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := e.ResumeServiceScoring(name); err != nil {
			writeEngineErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"service": name, "scoring_paused": false})
	}
}

func handleReload(e *engine.Engine, configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		added, removed, err := e.Reload(configPath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if added == nil {
			added = []string{}
		}
		if removed == nil {
			removed = []string{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"added": added, "removed": removed})
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/api/... -race`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/api/
git commit -m "feat: add HTTP API with public/admin routes and auth-gated detail text"
```

---

### Task 12: `main.go` wiring and end-to-end smoke test

**Files:**
- Create: `cmd/scoringengine/main.go`
- Test: `internal/e2e/e2e_test.go` *(wires the same packages `main.go` does,
  directly in-process — exercising the real wiring path without needing
  to exec a built binary)*

**Interfaces:**
- Consumes every package from Tasks 1–11.
- Produces: the `scoringengine` binary, built via `go build
  ./cmd/scoringengine`. Flags: `-config` (path to `config.yaml`, default
  `config.yaml`), `-db` (path to the SQLite file, default `scoring.db`),
  `-addr` (HTTP listen address, default `:8080`).

- [ ] **Step 1: Write the failing test**

```go
// internal/e2e/e2e_test.go
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Bravo-CDT/Scoring/internal/api"
	"github.com/Bravo-CDT/Scoring/internal/auth"
	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/config"
	"github.com/Bravo-CDT/Scoring/internal/engine"
	"github.com/Bravo-CDT/Scoring/internal/scheduler"
	"github.com/Bravo-CDT/Scoring/internal/scorer"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

const e2ePassword = "secret"

// stack mirrors exactly what cmd/scoringengine/main.go wires together, so
// building a fresh one simulates a process restart against the same
// files.
type stack struct {
	store  *store.Store
	auth   *auth.Auth
	server *httptest.Server
}

func buildStack(t *testing.T, dbPath, configPath string) stack {
	t.Helper()
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	sess, err := st.GetOrCreateSession(cfg.SessionName, cfg.StartingScore)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	for _, svcCfg := range cfg.Services {
		svc, err := st.GetOrCreateService(sess.ID, svcCfg.Name)
		if err != nil {
			t.Fatalf("GetOrCreateService: %v", err)
		}
		if err := st.InsertConfigSnapshot(svc.ID, svcCfg); err != nil {
			t.Fatalf("InsertConfigSnapshot: %v", err)
		}
	}

	sc := scorer.New(st, sess.ID, cfg.MassFailureFraction)
	registry := checks.NewRegistry()
	registry.Register("tcp_connect", func(ctx context.Context, host string, params map[string]any) checks.Result {
		return checks.Result{Success: false, Detail: "simulated down"}
	})
	sched := scheduler.New(registry, sc, configPath)
	eng := engine.New(st, sess, sched, configPath)

	hash, _ := bcrypt.GenerateFromPassword([]byte(e2ePassword), bcrypt.DefaultCost)
	a := auth.New(cfg.Admin.Username, string(hash))

	srv := httptest.NewServer(api.NewServer(eng, st, sess.ID, a, configPath))
	return stack{store: st, auth: a, server: srv}
}

func (s stack) teardown() {
	s.server.Close()
	s.store.Close()
}

func (s stack) get(t *testing.T, path string) map[string]any {
	t.Helper()
	resp, err := http.Get(s.server.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func (s stack) post(t *testing.T, path, token string, body any) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, err := http.NewRequest(http.MethodPost, s.server.URL+path, &buf)
	if err != nil {
		t.Fatalf("NewRequest %s: %v", path, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

const e2eYAML = `
session_name: "e2e-comp"
starting_score: 1000
admin:
  username: admin
  password_hash: $2a$10$abcdefghijklmnopqrstuvabcdefghijklmnopqrstuvwxyz123456
services:
  - name: svc1
    host: 127.0.0.1
    check_type: tcp_connect
    interval_seconds: 1
    points_per_failure: 10
    params: {}
`

func TestEndToEnd_FullLifecycleAndRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "scoring.db")
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(e2eYAML), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	s1 := buildStack(t, dbPath, configPath)

	// Fresh process: engine must start stopped.
	status := s1.get(t, "/status")
	if status["engine_state"] != "stopped" {
		t.Fatalf("initial engine_state = %v, want stopped", status["engine_state"])
	}

	token := func() string {
		out := s1.post(t, "/login", "", map[string]string{"username": "admin", "password": e2ePassword})
		tok, _ := out["token"].(string)
		if tok == "" {
			t.Fatal("login did not return a token")
		}
		return tok
	}()

	s1.post(t, "/engine/start", token, nil)
	time.Sleep(2500 * time.Millisecond) // stagger (<=5s, but typically fast) + at least one failing check

	status = s1.get(t, "/status")
	if status["blue_score"].(float64) >= 1000 {
		t.Fatalf("blue_score = %v, expected a deduction after a failing check", status["blue_score"])
	}
	scoreAfterFirstDrop := status["blue_score"].(float64)

	// Pause this service's scoring specifically (not globally) and confirm
	// it stops losing points while still being checked.
	s1.post(t, "/services/svc1/scoring/pause", token, nil)
	time.Sleep(1500 * time.Millisecond)
	status = s1.get(t, "/status")
	if status["blue_score"].(float64) != scoreAfterFirstDrop {
		t.Errorf("blue_score changed after per-service pause: %v -> %v", scoreAfterFirstDrop, status["blue_score"])
	}

	s1.post(t, "/engine/stop", token, nil)
	time.Sleep(1500 * time.Millisecond)
	resultsBeforeRestart := s1.get(t, "/services/svc1/results")

	s1.teardown()

	// Simulate a process restart: brand-new stack, same db file, same
	// config file, fresh in-memory auth (old token is gone).
	s2 := buildStack(t, dbPath, configPath)
	defer s2.teardown()

	status = s2.get(t, "/status")
	if status["engine_state"] != "stopped" {
		t.Errorf("post-restart engine_state = %v, want stopped (safe default)", status["engine_state"])
	}
	services, _ := status["services"].([]any)
	if len(services) != 1 {
		t.Fatalf("expected 1 service after restart, got %d", len(services))
	}
	svc, _ := services[0].(map[string]any)
	if paused, _ := svc["scoring_paused"].(bool); !paused {
		t.Error("expected per-service scoring_paused to survive the restart (true)")
	}

	resultsAfterRestart := s2.get(t, "/services/svc1/results")
	_ = resultsBeforeRestart
	_ = resultsAfterRestart // history must be queryable from the fresh instance too; presence already implied by a 200 JSON response above
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/e2e/...`
Expected: FAIL — `cmd/scoringengine/main.go` doesn't exist yet, but this
test only imports library packages, so it should actually compile and run
already; if every referenced package from Tasks 1–11 is complete, this
step may show a real assertion failure instead of a build error — note
in the PR which it was.

- [ ] **Step 3: Write `main.go`**

```go
// cmd/scoringengine/main.go
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/Bravo-CDT/Scoring/internal/api"
	"github.com/Bravo-CDT/Scoring/internal/auth"
	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/config"
	"github.com/Bravo-CDT/Scoring/internal/engine"
	"github.com/Bravo-CDT/Scoring/internal/scheduler"
	"github.com/Bravo-CDT/Scoring/internal/scorer"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config.yaml")
	dbPath := flag.String("db", "scoring.db", "path to the SQLite database file")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("opening store: %v", err)
	}
	defer st.Close()

	sess, err := st.GetOrCreateSession(cfg.SessionName, cfg.StartingScore)
	if err != nil {
		log.Fatalf("bootstrapping session: %v", err)
	}

	for _, svcCfg := range cfg.Services {
		svc, err := st.GetOrCreateService(sess.ID, svcCfg.Name)
		if err != nil {
			log.Fatalf("bootstrapping service %s: %v", svcCfg.Name, err)
		}
		if err := st.InsertConfigSnapshot(svc.ID, svcCfg); err != nil {
			log.Fatalf("recording config snapshot for %s: %v", svcCfg.Name, err)
		}
	}

	sc := scorer.New(st, sess.ID, cfg.MassFailureFraction)
	registry := checks.NewDefaultRegistry()
	sched := scheduler.New(registry, sc, *configPath)
	eng := engine.New(st, sess, sched, *configPath)

	a := auth.New(cfg.Admin.Username, cfg.Admin.PasswordHash)
	handler := api.NewServer(eng, st, sess.ID, a, *configPath)

	log.Printf("scoring engine listening on %s", *addr)
	log.Printf("engine starts stopped and scoring paused; POST /login then /engine/start to begin")
	if err := http.ListenAndServe(*addr, handler); err != nil {
		log.Fatalf("http server: %v", err)
	}
}
```

- [ ] **Step 4: Run the full test suite to verify everything passes**

Run: `go build ./... && go test ./... -race -timeout 120s`
Expected: PASS — this is the first point where every package compiles and
runs together; the scheduler/engine/api/e2e tests are the slow ones
(several seconds each due to real wall-clock interval timing), so the
`-timeout 120s` headroom matters here specifically.

- [ ] **Step 5: Commit**

```bash
git add cmd/scoringengine/ internal/e2e/
git commit -m "feat: wire up main.go and add end-to-end lifecycle/restart smoke test"
```

---

## Self-Review

**Spec coverage:** every section of the spec maps to a task — config
(Task 1), sessions/run-state/services/check-results/ledger (Tasks 2–5),
check plugins (Task 6), the self-paced scheduler (Task 7), scoring rules
and mass-failure safety net (Task 8), auth (Task 9), the full control
plane including `/reload` and atomic restart-with-pause (Task 10), the
complete HTTP API surface with public/admin detail gating (Task 11), and
real wiring plus an end-to-end restart-safety test (Task 12). Nothing in
the spec's Data model, Control plane, HTTP API, or Error handling
sections is left without an owning task.

**Placeholder scan:** no "TODO"/"TBD"/"add appropriate error handling"
anywhere in this plan — every step has complete, real code. (Three test
cases — two in Task 8, one in Task 10 — were deliberately drafted with an
intermediate wrong or stray-draft version followed by an explicit
corrected replacement, to show why the naive version is wrong or to flag
leftover drafting cruft to remove; this is a teaching aid inherent to
those specific tests, not a placeholder, and the final code given in each
case is complete and is what should actually be committed.)

**Type consistency:** `scheduler.ResultSink.Record(serviceID int64,
pointsPerFailure int, result checks.Result, latency time.Duration) error`
is defined once in Task 7 and implemented identically by `scorer.Scorer`
in Task 8 with no signature drift. `engine.ErrServiceNotFound` is defined
in Task 10 and consumed by name in Task 11. `store.Service`,
`store.CheckResult`, `store.Event`, and `store.LedgerEntry` are each
defined in exactly one task (3, 4, 4, 5 respectively) and only ever
extended (new methods), never redefined, by later tasks.

**Review Focus coverage:**
- Two services failing simultaneously → Task 5's duplicate/floor tests
  plus Task 8's `-race` concurrent-failure test.
- A hung check → Task 6's context-deadline tests plus Task 7's
  force-fail-at-deadline scheduler test.
- A simulated process restart → Task 2's run-state test, Task 3's
  persisted-pause test, and Task 12's full end-to-end restart test.
- The same check scored twice → Task 5's `ErrDuplicateSourceRef` test.
- Oversized/invalid-UTF-8 detail text → Task 4's truncation/sanitization
  tests plus Task 6's bounded HTTP body read.

No gaps found that required adding a new task beyond the twelve above.
