// Package app wires the scoring engine's packages into one runnable stack:
// config → store → session → scorer → scheduler → engine → auth → HTTP API.
//
// It is the single place that wiring lives. cmd/scoringengine calls New and
// serves the Handler; the end-to-end tests call the same New, so they exercise
// exactly what production runs.
package app

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/api"
	"github.com/Bravo-CDT/Scoring/internal/auth"
	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/config"
	"github.com/Bravo-CDT/Scoring/internal/engine"
	"github.com/Bravo-CDT/Scoring/internal/scheduler"
	"github.com/Bravo-CDT/Scoring/internal/scorer"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

// EventProcessStarted is the event New writes once per boot.
const EventProcessStarted = "process_started"

// Options configures New. ConfigPath and DBPath are required; the rest are
// overrides for tests and default to production behavior.
type Options struct {
	// ConfigPath is the config.yaml the whole stack reads: at startup, on
	// every routine (re)start, and on /reload.
	ConfigPath string
	// DBPath is the SQLite database file. It is created if missing; its
	// directory must already exist.
	DBPath string

	// Registry supplies the check implementations. Nil means
	// checks.NewDefaultRegistry().
	Registry *checks.Registry
	// MaxStagger, if set, overrides the scheduler's bound on the random delay
	// before each routine's first check (zero removes it).
	MaxStagger *time.Duration
}

// App is a fully wired scoring engine. The engine starts stopped and scoring
// starts paused (the store forces both on open); an admin must start the
// engine and resume scoring over the API.
type App struct {
	Config  *config.Config
	Session store.Session
	Store   *store.Store
	Engine  *engine.Engine
	Auth    *auth.Auth
	// Handler serves the full HTTP API.
	Handler http.Handler
}

// New loads and validates the config, opens the store, syncs the session's
// services with the config, and builds the stack on top. On error nothing is
// left open.
func New(opts Options) (*App, error) {
	if opts.ConfigPath == "" || opts.DBPath == "" {
		return nil, errors.New("app: config path and database path are required")
	}

	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	// store.Open would create the file but not its directory, and SQLite's
	// own error for a missing directory ("unable to open database file") does
	// not say what is wrong. Refuse up front instead: silently creating a
	// directory would hide a typo in -db and scatter databases around.
	dir := filepath.Dir(opts.DBPath)
	if info, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("database directory %s: %w (create it first)", dir, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("database directory %s is not a directory", dir)
	}

	st, err := store.Open(opts.DBPath)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	a, err := build(opts, cfg, st)
	if err != nil {
		st.Close()
		return nil, err
	}
	return a, nil
}

func build(opts Options, cfg *config.Config, st *store.Store) (*App, error) {
	sess, err := st.GetOrCreateSession(cfg.SessionName, cfg.StartingScore)
	if err != nil {
		return nil, fmt.Errorf("bootstrapping session %q: %w", cfg.SessionName, err)
	}

	registry := opts.Registry
	if registry == nil {
		registry = checks.NewDefaultRegistry()
	}
	// The scorer and scheduler refer to each other: the scheduler reports into
	// the scorer, and the scorer's mass-failure check counts only services the
	// scheduler is running. The closure reads sched only once results flow,
	// which needs the scheduler, so it is always set by then.
	var sched *scheduler.Scheduler
	running := func(serviceID int64) bool { return sched.IsRunning(serviceID) }
	sc := scorer.New(st, sess.ID, cfg.MassFailureFraction, scorer.WithRunning(running))
	sched = scheduler.New(registry, sc, opts.ConfigPath)
	if opts.MaxStagger != nil {
		sched.SetMaxStagger(*opts.MaxStagger)
	}
	eng := engine.New(st, sess, sched, opts.ConfigPath)

	// Sync the session's services with the config through the same path as
	// /reload: services new to the file are created (or reactivated with their
	// history), services dropped from it since the last run are retired, and
	// each change is logged as an event by the system, not an admin. The
	// store has just forced the engine stopped, so no routine starts here;
	// per-service scoring pauses persist untouched. Config snapshots are
	// recorded when routines start.
	if _, err := eng.ReloadAs(engine.ActorSystem, opts.ConfigPath); err != nil {
		return nil, fmt.Errorf("syncing services from config: %w", err)
	}
	// One event per boot, so the log shows the restart and why scoring stopped
	// moving: whatever state the previous process left, this one comes up
	// stopped and paused.
	err = st.InsertEvent(store.Event{
		SessionID: sess.ID,
		Level:     "info",
		Actor:     string(engine.ActorSystem),
		Type:      EventProcessStarted,
		Message: fmt.Sprintf("scoring engine process started for session %q; engine stopped and scoring paused until an admin starts and resumes them",
			sess.Name),
	})
	if err != nil {
		return nil, fmt.Errorf("recording process start: %w", err)
	}

	au := auth.New(cfg.Admin.Username, cfg.Admin.PasswordHash)
	return &App{
		Config:  cfg,
		Session: sess,
		Store:   st,
		Engine:  eng,
		Auth:    au,
		Handler: api.NewServer(eng, st, sess.ID, au, opts.ConfigPath),
	}, nil
}

// Close stops the engine, waiting for every check routine to exit, then closes
// the store. Call it only after the HTTP server has stopped handling requests.
//
// Stopping the engine here persists engine_state=stopped and logs an
// engine_stopped event, attributed to the system, if it was running; the store
// would force the same state on the next start anyway, so this just makes the
// shutdown explicit in the event history.
func (a *App) Close() error {
	stopErr := a.Engine.StopAs(engine.ActorSystem)
	if stopErr != nil {
		stopErr = fmt.Errorf("stopping engine: %w", stopErr)
	}
	return errors.Join(stopErr, a.Store.Close())
}
