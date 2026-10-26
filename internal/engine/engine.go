// Package engine is the scoring engine's control plane: it ties the store and
// the scheduler together behind the operations an admin performs — starting
// and stopping the engine, pausing scoring, managing individual services,
// reloading config.yaml — and reports the competition's current status.
//
// Run state lives in two places. The engine state (running/stopped) and the
// global and per-service scoring pauses are persisted in the store, which
// forces the engine stopped and scoring paused on every process start, so an
// admin must explicitly start the engine and, separately, resume scoring. A
// service's desired routine state (enabled unless an admin stopped it) is held
// only in memory: it survives an engine stop/start within a process and
// resets to enabled when the process restarts.
package engine

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/config"
	"github.com/Bravo-CDT/Scoring/internal/scheduler"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

// ErrServiceNotFound is wrapped into the error of every per-service method
// when the name matches no active service in the session. Callers (the HTTP
// API) use errors.Is to answer such requests with 404 rather than 500.
var ErrServiceNotFound = errors.New("engine: unknown service")

// ErrScoringNotPaused is returned by AdjustScore when the global scoring
// pause is not in effect.
var ErrScoringNotPaused = errors.New("engine: scoring must be paused for a manual adjustment")

// ErrEngineRunning is returned by BackupDatabase and ResetRound: both
// require the engine fully stopped first.
var ErrEngineRunning = errors.New("engine: must be stopped for this action")

// MaxAdjustmentPoints bounds a manual adjustment's magnitude. Without a
// bound, a typo (or a deliberately huge value) can push a side's ledger
// balance close enough to math.MaxInt64 that a later adjustment, or even
// ordinary scoring, overflows the SQL SUM in store.sideBalance, breaking
// GetScore (and so GET /status) until the session's round data is reset.
const MaxAdjustmentPoints = 1_000_000

// ErrInvalidAdjustment is returned by AdjustScore for a side other than
// blue/red, a zero delta, or a delta whose magnitude exceeds
// MaxAdjustmentPoints.
var ErrInvalidAdjustment = errors.New("engine: invalid score adjustment")

// Errors SetCredential returns for a change it refuses. Their messages are
// shown to the blue team as they are.
var (
	ErrNotCredentialed = errors.New("this service's check does not log in with a username and password")
	ErrUserNotAllowed  = errors.New("that user is not one of the allowed users")
	ErrInvalidPassword = errors.New("the password must be 1 to 256 bytes, with no control characters")
)

// maxCredentialPassword bounds a blue team password, in bytes.
const maxCredentialPassword = 256

// Event types the Engine writes.
const (
	EventEngineStarted         = "engine_started"
	EventEngineStopped         = "engine_stopped"
	EventScoringPaused         = "scoring_paused"
	EventScoringResumed        = "scoring_resumed"
	EventRoutineStarted        = "routine_started"
	EventRoutineStopped        = "routine_stopped"
	EventRoutineRestarted      = "routine_restarted"
	EventRoutineStartFailed    = "routine_start_failed"
	EventServiceScoringPaused  = "service_scoring_paused"
	EventServiceScoringResumed = "service_scoring_resumed"
	EventServiceAdded          = "service_added"
	EventServiceRemoved        = "service_removed"
	EventConfigReloaded        = "config_reloaded"
	EventConfigSnapshotFailed  = "config_snapshot_failed"
	EventCountdownChanged      = "countdown_changed"
	EventScoreAdjusted         = "score_adjusted"
	EventDatabaseBackedUp      = "database_backed_up"
	EventRoundReset            = "round_reset"
	EventStartingScoreChanged  = "starting_score_changed"
	EventCredentialChanged     = "credential_changed"
	EventCredentialReset       = "credential_reset"
)

// StartFailure reports one service whose routine failed to start, for the
// operations that start several (Start, Reload). The failure is also logged
// as a routine_start_failed event.
type StartFailure struct {
	Service string `json:"service"`
	Error   string `json:"error"`
}

// StartError is the error StartService and RestartService return when the
// service's routine failed to start. Everything else the call does has been
// applied by then (the service is enabled, a requested scoring pause is
// persisted), so the service starts with the next successful attempt.
type StartError struct {
	Service string
	Err     error
}

func (e *StartError) Error() string {
	return fmt.Sprintf("engine: %s routine failed to start: %v", e.Service, e.Err)
}

func (e *StartError) Unwrap() error { return e.Err }

// Failure returns e as a StartFailure.
func (e *StartError) Failure() StartFailure {
	return StartFailure{Service: e.Service, Error: e.Err.Error()}
}

// ReloadResult is what Reload changed: the services added and removed, in the
// orders Reload documents, and the added services whose routine failed to
// start. All three are non-nil.
type ReloadResult struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	// Updated lists running services whose config entry changed, which were
	// restarted so the change applies from the next round.
	Updated []string       `json:"updated"`
	Failed  []StartFailure `json:"failed"`
}

// Actor is who an event attributes an operation to.
type Actor string

// Event actors. The exported control methods act for ActorAdmin; the app's
// own automatic operations (the startup config sync, the shutdown stop) use
// the ...As variants with ActorSystem.
const (
	ActorAdmin  Actor = "admin"
	ActorSystem Actor = "system"
	// ActorBlueTeam is the blue team, changing a check's credentials.
	ActorBlueTeam Actor = "blue_team"
)

// Engine is the control plane for one session. It is safe for concurrent use.
type Engine struct {
	store      *store.Store
	session    store.Session
	scheduler  *scheduler.Scheduler
	configPath string

	// mu serializes the control operations (everything but Status, the
	// scoring pause/resume levers and SetCountdownTarget, which must stay
	// usable while another operation is stuck; see setGlobalScoringPaused
	// and SetCountdownTarget) and guards
	// desiredEnabled. Serializing is what keeps each operation's
	// check-then-act sound: Stop cannot interleave with a StartService that
	// has already seen the engine running and so leave a routine running
	// behind a stopped engine, and a routine is known to be running exactly
	// when the scheduler says so.
	//
	// It is held across scheduler calls that can block, such as StopService
	// waiting out a round the scheduler is recording. That wait is bounded by
	// the scorer's local database writes, never by a check running, and it
	// cannot deadlock: nothing the round loop does (running checks, the scorer
	// recording the round) ever calls back into the Engine, and the Engine
	// never holds a database connection (an open transaction or unclosed rows)
	// while it waits. Status does not take mu, so reads stay responsive while
	// an operation waits.
	mu sync.Mutex
	// desiredEnabled holds each service's admin-chosen routine state; a
	// service absent from it is enabled.
	desiredEnabled map[int64]bool

	// configHashMu guards configHash, read far more often (every config
	// editor poll) than it is written (one Reload at a time), so it stays
	// separate from mu rather than requiring every reader to take the
	// control-operation lock.
	configHashMu sync.Mutex
	configHash   string

	// layout is the network diagram's labels and each service's host, from
	// the config last loaded (at startup and on every successful Reload).
	// Nil until then.
	layout atomic.Pointer[layout]
	// blueLogin is whether the app has a blue team login, which, like the
	// admin's, is read at startup only.
	blueLogin atomic.Bool
}

// layout is what Status reports about the network, beyond up/down.
type layout struct {
	networks []Network
	hosts    []Host
	services map[string]serviceLayout
	// blueTeam is whether the config has a blue_team section, and
	// allowedUsers its allowed_users.
	blueTeam     bool
	allowedUsers []string
}

type serviceLayout struct {
	host      string
	checkType string
	port      int
	// credentialed is whether the service's check logs in with a username
	// and password the blue team can change; defaultUser is the config's.
	credentialed bool
	defaultUser  string
}

// newLayout picks the diagram's details out of cfg.
func newLayout(cfg *config.Config) *layout {
	l := &layout{
		networks: make([]Network, 0, len(cfg.Networks)),
		hosts:    make([]Host, 0, len(cfg.Hosts)),
		services: make(map[string]serviceLayout, len(cfg.Services)),
	}
	for _, n := range cfg.Networks {
		l.networks = append(l.networks, Network{CIDR: n.CIDR, Name: n.Name})
	}
	for _, h := range cfg.Hosts {
		l.hosts = append(l.hosts, Host{Address: h.Address, Name: h.Name, OS: h.OS})
	}
	for _, sc := range cfg.Services {
		// Every check type with a port takes it as an integer "port" param.
		port, _ := sc.Params["port"].(int)
		user, credentialed := checks.DefaultUsername(sc.CheckType, sc.Params)
		l.services[sc.Name] = serviceLayout{
			host: sc.Host, checkType: sc.CheckType, port: port,
			credentialed: credentialed, defaultUser: user,
		}
	}
	if cfg.BlueTeam != nil {
		l.blueTeam = true
		l.allowedUsers = slices.Clone(cfg.BlueTeam.AllowedUsers)
	}
	return l
}

// New wires a store, a session and a scheduler together. configPath is the
// config file Reload reads by default and the one config snapshots are taken
// from; it should be the path the scheduler was built with, which reads its
// own copy whenever it (re)starts a routine.
func New(st *store.Store, sess store.Session, sched *scheduler.Scheduler, configPath string) *Engine {
	return &Engine{
		store:          st,
		session:        sess,
		scheduler:      sched,
		configPath:     configPath,
		desiredEnabled: make(map[int64]bool),
	}
}

// Start marks the engine running and starts the routine of every active
// service an admin has not stopped. It leaves scoring paused or not as it
// was: resuming scoring is a separate decision (ResumeScoring). Starting a
// stopped engine runs a round at once and times the following rounds from it.
//
// A service whose routine fails to start (its config entry is missing, say) is
// logged as an error event, skipped and returned in failed (non-nil, empty
// when every routine started); it does not stop the others starting, and does
// not make Start fail: the engine is running either way. Start on a running
// engine starts any enabled routine that is not running and is otherwise a
// no-op.
func (e *Engine) Start() (failed []StartFailure, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	failed = []StartFailure{}
	rs, err := e.store.GetRunState()
	if err != nil {
		return failed, fmt.Errorf("engine: starting: %w", err)
	}
	svcs, err := e.store.ListActiveServices(e.session.ID)
	if err != nil {
		return failed, fmt.Errorf("engine: starting: %w", err)
	}
	if err := e.store.SetEngineState(store.EngineRunning); err != nil {
		return failed, fmt.Errorf("engine: starting: %w", err)
	}

	for _, svc := range svcs {
		if !e.enabled(svc.ID) {
			continue
		}
		if startErr := e.startRoutine(svc); startErr != nil {
			failed = append(failed, startErr.Failure())
		}
	}

	if rs.EngineState == store.EngineRunning {
		return failed, nil
	}
	// Check right away rather than at the round loop's next tick, and time
	// the rounds from here.
	e.scheduler.StartRoundNow()
	return failed, e.logEngineEvent(ActorAdmin, EventEngineStarted, "engine started")
}

// Stop marks the engine stopped and stops every routine, waiting for each to
// exit. Admins' per-service choices are kept for the next Start. It acts for
// an admin; see StopAs.
func (e *Engine) Stop() error {
	return e.StopAs(ActorAdmin)
}

// StopAs is Stop with its engine_stopped event attributed to actor, so a
// shutdown the process performs itself is not logged as an admin's action.
func (e *Engine) StopAs(actor Actor) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	rs, err := e.store.GetRunState()
	if err != nil {
		return fmt.Errorf("engine: stopping: %w", err)
	}
	if err := e.store.SetEngineState(store.EngineStopped); err != nil {
		return fmt.Errorf("engine: stopping: %w", err)
	}
	for _, id := range e.scheduler.RunningServiceIDs() {
		e.scheduler.StopService(id)
	}

	if rs.EngineState == store.EngineStopped {
		return nil
	}
	return e.logEngineEvent(actor, EventEngineStopped, "engine stopped")
}

// PauseScoring globally stops results moving points. Checks keep running and
// their results are still recorded.
func (e *Engine) PauseScoring() error {
	return e.setGlobalScoringPaused(true, EventScoringPaused, "global scoring paused")
}

// ResumeScoring lifts the global scoring pause. Services paused individually
// stay paused.
func (e *Engine) ResumeScoring() error {
	return e.setGlobalScoringPaused(false, EventScoringResumed, "global scoring resumed")
}

// setGlobalScoringPaused deliberately does not take mu: pausing scoring is the
// admin's emergency lever and must work even while another control operation
// is held up in mu (say, a StopService waiting out a round the scheduler is
// recording, which a slow database write can stretch). It needs no
// serialization — it is one write of a single last-writer-wins flag, read
// fresh by the scorer on every result, and no control operation checks it
// before acting — plus an event.
func (e *Engine) setGlobalScoringPaused(paused bool, eventType, message string) error {
	if err := e.store.SetGlobalScoringPaused(paused); err != nil {
		return fmt.Errorf("engine: %s: %w", message, err)
	}
	return e.logEngineEvent(ActorAdmin, eventType, message)
}

// AdjustScore manually adjusts side's score by delta (nonzero), independent
// of the other side. It requires the global scoring pause (status.scoring_paused
// -- regardless of whether the engine itself is running or stopped), checked
// fresh on every call, the same lever-style pattern as setGlobalScoringPaused:
// it must stay usable even while a control operation is stuck in mu, so it
// does not take mu.
func (e *Engine) AdjustScore(side string, delta int, reason string) error {
	if (side != store.SideBlue && side != store.SideRed) ||
		delta == 0 || delta > MaxAdjustmentPoints || delta < -MaxAdjustmentPoints {
		return ErrInvalidAdjustment
	}
	rs, err := e.store.GetRunState()
	if err != nil {
		return fmt.Errorf("engine: adjusting score: %w", err)
	}
	if !rs.ScoringPaused {
		return ErrScoringNotPaused
	}
	if _, err := e.store.AdjustScore(e.session.ID, side, delta, reason); err != nil {
		return fmt.Errorf("engine: adjusting score: %w", err)
	}
	return e.logEngineEvent(ActorAdmin, EventScoreAdjusted,
		fmt.Sprintf("%s adjusted by %+d: %s", side, delta, reason))
}

// SetCountdownTarget sets the time the scoreboard's countdown runs to, or
// clears it when target is nil. The target is display-only: nothing in the
// engine acts on it.
//
// It does not take mu, for the same reason as setGlobalScoringPaused: it is
// one write of a single last-writer-wins value that no control operation
// checks before acting, so it needs no serialization and should not wait
// behind an operation held up in mu.
func (e *Engine) SetCountdownTarget(target *time.Time) error {
	if err := e.store.SetCountdownTarget(target); err != nil {
		return fmt.Errorf("engine: setting countdown target: %w", err)
	}
	msg := "countdown target cleared"
	if target != nil {
		msg = "countdown target set to " + target.UTC().Format(time.RFC3339)
	}
	return e.logEngineEvent(ActorAdmin, EventCountdownChanged, msg)
}

// StartService enables the named service's routine and, if the engine is
// running, starts it. While the engine is stopped it only records that the
// service should run once the engine starts. If the routine fails to start
// the error is a *StartError, and the service stays enabled.
func (e *Engine) StartService(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	svc, err := e.lookup(name)
	if err != nil {
		return err
	}
	e.desiredEnabled[svc.ID] = true

	running, err := e.engineRunning()
	if err != nil {
		return err
	}
	if !running {
		return e.logServiceEvent(ActorAdmin, svc, EventRoutineStarted,
			svc.Name+" routine enabled; it starts when the engine starts")
	}
	if startErr := e.startRoutine(svc); startErr != nil {
		return startErr
	}
	return e.logServiceEvent(ActorAdmin, svc, EventRoutineStarted, svc.Name+" routine started")
}

// StopService disables the named service's routine and stops it: its check in
// the round in progress, if any, is cancelled and its result discarded. It can
// wait briefly for the scheduler to finish recording a round that already
// ended (bounded by local database writes, never by a check), so once it
// returns no further result for the service is recorded. The service stays
// stopped across engine stops and starts until StartService or RestartService
// enables it again.
func (e *Engine) StopService(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	svc, err := e.lookup(name)
	if err != nil {
		return err
	}
	e.desiredEnabled[svc.ID] = false
	e.scheduler.StopService(svc.ID)
	return e.logServiceEvent(ActorAdmin, svc, EventRoutineStopped, svc.Name+" routine stopped")
}

// RestartService enables the named service's routine and, if the engine is
// running, restarts it with its entry freshly read from config.yaml — which is
// how an edit to that entry takes effect. With pauseScoring, the service's
// scoring pause is persisted first, before any restarted routine can run a
// check, so not one result from the new configuration moves points until an
// admin resumes the service.
//
// If the engine is stopped the routine is not started; the pause (if asked
// for) and the enabled state still apply. If the routine fails to start again
// (the fresh config entry cannot be loaded, say) it is left stopped and the
// error is a *StartError; the pause and the enabled state still apply.
func (e *Engine) RestartService(name string, pauseScoring bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	svc, err := e.lookup(name)
	if err != nil {
		return err
	}
	if pauseScoring {
		if err := e.store.SetServiceScoringPaused(svc.ID, true); err != nil {
			return fmt.Errorf("engine: pausing scoring of %s: %w", svc.Name, err)
		}
		if err := e.logServiceEvent(ActorAdmin, svc, EventServiceScoringPaused,
			svc.Name+" scoring paused for routine restart"); err != nil {
			return err
		}
	}
	e.desiredEnabled[svc.ID] = true

	running, err := e.engineRunning()
	if err != nil {
		return err
	}
	if !running {
		return e.logServiceEvent(ActorAdmin, svc, EventRoutineRestarted,
			svc.Name+" routine enabled; it starts when the engine starts")
	}
	if err := e.scheduler.RestartService(svc.ID, svc.Name); err != nil {
		e.logStartFailure(svc, ActorAdmin, err)
		return &StartError{Service: svc.Name, Err: err}
	}
	e.snapshotConfig(svc)
	return e.logServiceEvent(ActorAdmin, svc, EventRoutineRestarted, svc.Name+" routine restarted with reloaded config")
}

// PauseServiceScoring stops the named service's results moving points. Its
// checks keep running. The pause is persisted and survives a restart.
func (e *Engine) PauseServiceScoring(name string) error {
	return e.setServiceScoringPaused(name, true, EventServiceScoringPaused, "scoring paused")
}

// ResumeServiceScoring lifts the named service's scoring pause. A global
// pause, if any, still applies.
func (e *Engine) ResumeServiceScoring(name string) error {
	return e.setServiceScoringPaused(name, false, EventServiceScoringResumed, "scoring resumed")
}

// setServiceScoringPaused does not take mu, for the same reason as
// setGlobalScoringPaused: the per-service pause is a single last-writer-wins
// flag read fresh by the scorer, so racing a RestartService (which may set the
// same flag) or a Reload is harmless, and the lever must not wait behind a
// stuck control operation. The lookup needs only the store. If a Reload
// retires the service concurrently, the flag is still written to its row,
// which keeps it for a later reactivation, exactly as if the pause came first.
func (e *Engine) setServiceScoringPaused(name string, paused bool, eventType, what string) error {
	svc, err := e.lookup(name)
	if err != nil {
		return err
	}
	if err := e.store.SetServiceScoringPaused(svc.ID, paused); err != nil {
		return fmt.Errorf("engine: %s %s: %w", svc.Name, what, err)
	}
	return e.logServiceEvent(ActorAdmin, svc, eventType, svc.Name+" "+what)
}

// Reload reads the config file at configPath (the path given to New when
// empty) and brings the set of active services in line with it:
//
//   - A service new to the file is activated (a name seen before gets its old
//     row back, history and scoring pause intact), enabled, and started if the
//     engine is running.
//   - An active service missing from the file has its routine stopped and is
//     retired; its history stays queryable.
//   - A service in both whose routine is running with a different entry is
//     restarted with the new one; otherwise it is left alone. (A stopped
//     routine reads its entry afresh whenever it starts.)
//   - A changed starting_score is applied, moving blue's score by the
//     difference.
//
// Reload never changes the engine state or the global scoring pause. An
// invalid file changes nothing. An added service whose routine fails to start
// is still added; the failure is logged and listed in the result's Failed,
// and does not fail the reload. If a store write fails partway the error is
// returned with the changes made so far. It acts for an admin; see ReloadAs.
func (e *Engine) Reload(configPath string) (ReloadResult, error) {
	return e.ReloadAs(ActorAdmin, configPath)
}

// ReloadAs is Reload with its events attributed to actor. A reload by
// ActorSystem (the app's startup sync) that changes nothing writes no
// config_reloaded event, so an ordinary boot does not add one every time; an
// admin's reload is always logged, being an admin action.
func (e *Engine) ReloadAs(actor Actor, configPath string) (ReloadResult, error) {
	if configPath == "" {
		configPath = e.configPath
	}
	res := ReloadResult{Added: []string{}, Removed: []string{}, Updated: []string{}, Failed: []StartFailure{}}

	e.mu.Lock()
	defer e.mu.Unlock()

	cfg, err := config.Load(configPath)
	if err != nil {
		return res, fmt.Errorf("engine: reloading config: %w", err)
	}
	e.recordLoadedConfigHash(configPath)
	e.layout.Store(newLayout(cfg))
	if err := e.applyStartingScore(actor, cfg.StartingScore); err != nil {
		return res, err
	}
	active, err := e.store.ListActiveServices(e.session.ID)
	if err != nil {
		return res, fmt.Errorf("engine: reloading config: %w", err)
	}
	running, err := e.engineRunning()
	if err != nil {
		return res, err
	}

	inConfig := make(map[string]bool, len(cfg.Services))
	for _, sc := range cfg.Services {
		inConfig[sc.Name] = true
	}
	isActive := make(map[string]bool, len(active))
	for _, svc := range active {
		isActive[svc.Name] = true
		if inConfig[svc.Name] {
			continue
		}
		if err := e.retire(actor, svc); err != nil {
			return res, err
		}
		res.Removed = append(res.Removed, svc.Name)
	}
	for _, svc := range active {
		if !inConfig[svc.Name] {
			continue
		}
		updated, startErr, err := e.restartIfChanged(actor, svc, cfg)
		if updated {
			res.Updated = append(res.Updated, svc.Name)
		}
		if startErr != nil {
			res.Failed = append(res.Failed, startErr.Failure())
		}
		if err != nil {
			return res, err
		}
	}
	for _, sc := range cfg.Services {
		if isActive[sc.Name] {
			continue
		}
		svc, err := e.activate(actor, sc.Name)
		if err != nil {
			return res, err
		}
		res.Added = append(res.Added, sc.Name)
		if running {
			if startErr := e.startRoutine(svc); startErr != nil {
				res.Failed = append(res.Failed, startErr.Failure())
			}
		}
	}

	if actor == ActorSystem && len(res.Added) == 0 && len(res.Removed) == 0 && len(res.Updated) == 0 {
		return res, nil
	}
	msg := fmt.Sprintf("config reloaded: added [%s], removed [%s], updated [%s]",
		strings.Join(res.Added, ", "), strings.Join(res.Removed, ", "), strings.Join(res.Updated, ", "))
	return res, e.logEngineEvent(actor, EventConfigReloaded, msg)
}

// LoadedConfigHash returns the hash of the config file this Engine last
// successfully loaded (at construction and on every successful Reload).
func (e *Engine) LoadedConfigHash() string {
	e.configHashMu.Lock()
	defer e.configHashMu.Unlock()
	return e.configHash
}

// recordLoadedConfigHash re-reads configPath (already known loadable, since
// the caller just parsed it) and records its hash as the one this Engine has
// loaded. Best effort, mirroring snapshotConfig's best-effort style: on a
// rare failure to re-read the same file moments after loading it (e.g. it
// was deleted in between), LoadedConfigHash simply keeps its previous value
// until the next successful reload. Not worth an event of its own: nothing
// scored is affected, only the config-editor's "stale" indicator for one
// cycle.
func (e *Engine) recordLoadedConfigHash(configPath string) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return
	}
	e.configHashMu.Lock()
	e.configHash = config.Hash(raw)
	e.configHashMu.Unlock()
}

// BackupDatabase snapshots the database, requiring the engine be fully
// stopped. Returns the backup file's path.
func (e *Engine) BackupDatabase() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.backupLocked()
}

// backupLocked is BackupDatabase's body, callable by ResetRound (which
// already holds mu) without re-locking it.
func (e *Engine) backupLocked() (string, error) {
	running, err := e.engineRunning()
	if err != nil {
		return "", err
	}
	if running {
		return "", ErrEngineRunning
	}
	path, err := e.store.Backup()
	if err != nil {
		return "", fmt.Errorf("engine: backing up database: %w", err)
	}
	if err := e.logEngineEvent(ActorAdmin, EventDatabaseBackedUp, "database backed up to "+path); err != nil {
		return path, err
	}
	return path, nil
}

// ResetRound deletes this session's score and check-result history,
// requiring the engine be fully stopped. If backup is true, it snapshots the
// database first; if that backup fails, nothing is reset and the error is
// returned. Returns the backup's path, or "" if backup was false.
//
// Admin event history, the session row, service definitions, and config
// snapshots are untouched, so the admin console and login keep working
// immediately -- no restart needed. "Resetting credentials to their
// defaults" needs no extra step here: every service routine already
// re-reads config.yaml fresh the moment it is (re)started
// (scheduler.start()), so the next "Start engine" after a reset naturally
// uses whatever the file currently contains.
func (e *Engine) ResetRound(backup bool) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	var path string
	if backup {
		var err error
		if path, err = e.backupLocked(); err != nil {
			return "", err
		}
	} else if running, err := e.engineRunning(); err != nil {
		return "", err
	} else if running {
		return "", ErrEngineRunning
	}

	if _, err := e.store.ResetRoundData(e.session.ID); err != nil {
		return path, fmt.Errorf("engine: resetting round data: %w", err)
	}
	// The engine is stopped (checked above), so no service is enabled and
	// no round can be recorded under the old numbering.
	e.scheduler.ResetRounds()
	if err := e.logEngineEvent(ActorAdmin, EventRoundReset, "round reset: score and check history cleared, credentials reset to defaults"); err != nil {
		return path, err
	}
	return path, nil
}

// applyStartingScore sets the session's starting score to startingScore if
// it differs, and logs the change. The caller holds mu.
func (e *Engine) applyStartingScore(actor Actor, startingScore int) error {
	current, err := e.store.SessionStartingScore(e.session.ID)
	if err != nil {
		return fmt.Errorf("engine: reloading config: %w", err)
	}
	if current == startingScore {
		return nil
	}
	if err := e.store.SetSessionStartingScore(e.session.ID, startingScore); err != nil {
		return fmt.Errorf("engine: reloading config: %w", err)
	}
	return e.logEngineEvent(actor, EventStartingScoreChanged,
		fmt.Sprintf("starting score changed from %d to %d", current, startingScore))
}

// restartIfChanged restarts svc's routine if it is running with an entry
// other than cfg's, reporting whether it did. A failed restart leaves the
// routine stopped, as RestartService does. The caller holds mu.
func (e *Engine) restartIfChanged(actor Actor, svc store.Service, cfg *config.Config) (bool, *StartError, error) {
	running, ok := e.scheduler.ServiceConfig(svc.ID)
	if !ok {
		return false, nil, nil
	}
	i := slices.IndexFunc(cfg.Services, func(sc config.ServiceConfig) bool { return sc.Name == svc.Name })
	if i < 0 || reflect.DeepEqual(running, cfg.Services[i]) {
		return false, nil, nil
	}
	if err := e.scheduler.RestartService(svc.ID, svc.Name); err != nil {
		e.logStartFailure(svc, actor, err)
		return true, &StartError{Service: svc.Name, Err: err}, nil
	}
	e.snapshotConfig(svc)
	return true, nil, e.logServiceEvent(actor, svc, EventRoutineRestarted,
		svc.Name+" routine restarted with its changed config entry")
}

// retire stops svc's routine and deactivates it. The caller holds mu.
func (e *Engine) retire(actor Actor, svc store.Service) error {
	e.scheduler.StopService(svc.ID)
	if err := e.store.SetServiceActive(svc.ID, false); err != nil {
		return fmt.Errorf("engine: retiring %s: %w", svc.Name, err)
	}
	// Re-adding the name later enables it afresh.
	delete(e.desiredEnabled, svc.ID)
	return e.logServiceEvent(actor, svc, EventServiceRemoved, svc.Name+" removed from config; deactivated")
}

// activate creates or reactivates the service called name and enables it,
// returning its row; starting its routine is up to the caller. The caller
// holds mu.
func (e *Engine) activate(actor Actor, name string) (store.Service, error) {
	svc, err := e.store.GetOrCreateService(e.session.ID, name)
	if err != nil {
		return store.Service{}, fmt.Errorf("engine: adding %s: %w", name, err)
	}
	msg := name + " added from config"
	if !svc.Active {
		if err := e.store.SetServiceActive(svc.ID, true); err != nil {
			return store.Service{}, fmt.Errorf("engine: reactivating %s: %w", name, err)
		}
		svc.Active = true
		msg = name + " re-added from config; reactivated with its history"
	}
	e.desiredEnabled[svc.ID] = true
	if err := e.logServiceEvent(actor, svc, EventServiceAdded, msg); err != nil {
		return store.Service{}, err
	}
	return svc, nil
}

// ServiceStatus is one active service's current state.
type ServiceStatus struct {
	Name string `json:"name"`
	// Up is whether the latest check succeeded; false before any check.
	Up bool `json:"up"`
	// RoutineEnabled is whether the service's check routine is running.
	RoutineEnabled bool `json:"routine_enabled"`
	// Checking is whether the service's check is part of the round in
	// progress right now, mid-flight.
	Checking bool `json:"checking"`
	// ScoringPaused is the service's own pause; the global one is in Status.
	ScoringPaused bool `json:"scoring_paused"`
	// LastCheckTime is when the latest check ran; omitted before any check.
	LastCheckTime       *time.Time `json:"last_check_time,omitempty"`
	LatencyMS           int64      `json:"latency_ms"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	// Host, CheckType and Port come from the service's config entry; Port is
	// omitted for a check type without one.
	Host      string `json:"host"`
	CheckType string `json:"check_type"`
	Port      int    `json:"port,omitempty"`
}

// Network is a named subnet for the scoreboard's network diagram.
type Network struct {
	CIDR string `json:"cidr"`
	Name string `json:"name,omitempty"`
}

// Host is a checked machine's name and operating system, for the
// scoreboard's network diagram.
type Host struct {
	Address string `json:"address"`
	Name    string `json:"name,omitempty"`
	OS      string `json:"os,omitempty"`
}

// Status is a point-in-time view of the session: score, run state, round
// clock, and every active service, ordered by name.
type Status struct {
	SessionName   string    `json:"session_name"`
	StartingScore int       `json:"starting_score"`
	BlueScore     int       `json:"blue_score"`
	RedScore      int       `json:"red_score"`
	EngineState   string    `json:"engine_state"`
	ScoringPaused bool      `json:"scoring_paused"`
	ServerTime    time.Time `json:"server_time"`
	// Round is the number of the last round recorded. NextRoundAt is when the
	// next round is due; it is the zero time until this process records a
	// round, and holds its last value while no service is being checked.
	Round       int       `json:"round"`
	NextRoundAt time.Time `json:"next_round_at"`
	// CountdownTarget is the admin-set time the countdown runs to; omitted
	// while unset.
	CountdownTarget *time.Time      `json:"countdown_target,omitempty"`
	Services        []ServiceStatus `json:"services"`
	Networks        []Network       `json:"networks"`
	Hosts           []Host          `json:"hosts"`
	// CredentialsEnabled is whether the blue team credentials page is on
	// (the config has a blue_team section).
	CredentialsEnabled bool `json:"credentials_enabled"`
}

// Status reports the session's current state. It does not wait for control
// operations in progress; its reads are individually current but not one
// atomic snapshot.
func (e *Engine) Status() (Status, error) {
	blue, red, err := e.store.GetScore(e.session.ID)
	if err != nil {
		return Status{}, fmt.Errorf("engine: status: %w", err)
	}
	startingScore, err := e.store.SessionStartingScore(e.session.ID)
	if err != nil {
		return Status{}, fmt.Errorf("engine: status: %w", err)
	}
	rs, err := e.store.GetRunState()
	if err != nil {
		return Status{}, fmt.Errorf("engine: status: %w", err)
	}
	svcs, err := e.store.ListActiveServices(e.session.ID)
	if err != nil {
		return Status{}, fmt.Errorf("engine: status: %w", err)
	}

	round, nextRoundAt := e.scheduler.RoundStatus()
	l := e.currentLayout()

	out := Status{
		SessionName:     e.session.Name,
		StartingScore:   startingScore,
		BlueScore:       blue,
		RedScore:        red,
		EngineState:     rs.EngineState,
		ScoringPaused:   rs.ScoringPaused,
		ServerTime:      time.Now().UTC(),
		Round:           round,
		NextRoundAt:     nextRoundAt,
		CountdownTarget: rs.CountdownTarget,
		Services:        make([]ServiceStatus, 0, len(svcs)),
		Networks:        l.networks,
		Hosts:           l.hosts,

		CredentialsEnabled: e.blueLogin.Load() && l.blueTeam,
	}
	for _, svc := range svcs {
		ss := ServiceStatus{
			Name:           svc.Name,
			RoutineEnabled: e.scheduler.IsRunning(svc.ID),
			Checking:       e.scheduler.IsChecking(svc.ID),
			ScoringPaused:  svc.ScoringPaused,
			Host:           l.services[svc.Name].host,
			CheckType:      l.services[svc.Name].checkType,
			Port:           l.services[svc.Name].port,
		}
		latest, err := e.store.GetLatestCheckResult(svc.ID)
		if err != nil {
			return Status{}, fmt.Errorf("engine: status: %w", err)
		}
		if latest != nil {
			ss.Up = latest.Success
			ss.LastCheckTime = &latest.Timestamp
			ss.LatencyMS = latest.LatencyMS
		}
		if ss.ConsecutiveFailures, err = e.store.CountConsecutiveFailures(svc.ID); err != nil {
			return Status{}, fmt.Errorf("engine: status: %w", err)
		}
		out.Services = append(out.Services, ss)
	}
	return out, nil
}

// lookup resolves an admin-supplied service name. Unknown and retired names
// both wrap ErrServiceNotFound: a retired service has no routine to manage.
func (e *Engine) lookup(name string) (store.Service, error) {
	svc, found, err := e.store.GetServiceByName(e.session.ID, name)
	if err != nil {
		return store.Service{}, fmt.Errorf("engine: looking up service %q: %w", name, err)
	}
	if !found || !svc.Active {
		return store.Service{}, fmt.Errorf("%w: %q", ErrServiceNotFound, name)
	}
	return svc, nil
}

// enabled reports a service's desired routine state. The caller holds mu.
func (e *Engine) enabled(serviceID int64) bool {
	enabled, known := e.desiredEnabled[serviceID]
	return !known || enabled
}

// engineRunning reads the persisted engine state. The caller holds mu, so the
// answer stays true for the rest of the operation.
func (e *Engine) engineRunning() (bool, error) {
	rs, err := e.store.GetRunState()
	if err != nil {
		return false, fmt.Errorf("engine: reading run state: %w", err)
	}
	return rs.EngineState == store.EngineRunning, nil
}

// startRoutine starts svc's routine unless it is already running, recording
// the config it starts with. A failure to start is logged as a system error
// event and returned. The caller holds mu, so no other operation can start or
// stop the routine between the check and the start.
func (e *Engine) startRoutine(svc store.Service) *StartError {
	if e.scheduler.IsRunning(svc.ID) {
		return nil
	}
	if err := e.scheduler.StartService(svc.ID, svc.Name); err != nil {
		e.logStartFailure(svc, ActorSystem, err)
		return &StartError{Service: svc.Name, Err: err}
	}
	e.snapshotConfig(svc)
	return nil
}

// snapshotConfig records the config entry svc's routine was just (re)started
// with, so each check result stays explainable by the config that produced
// it. The entry is re-read here rather than taken from the scheduler, so an
// edit landing in between could be recorded instead; the snapshot is
// history, not a scoring input. A failure is logged and otherwise ignored:
// missing history must not keep a service from being checked.
func (e *Engine) snapshotConfig(svc store.Service) {
	sc, err := config.LoadService(e.configPath, svc.Name)
	if err == nil {
		err = e.store.InsertConfigSnapshot(svc.ID, *sc)
	}
	if err != nil {
		_ = e.logEvent(store.Event{
			ServiceID: &svc.ID, Level: "error", Actor: string(ActorSystem), Type: EventConfigSnapshotFailed,
			Message: "could not record the config " + svc.Name + " started with", Detail: err.Error(),
		})
	}
}

// logStartFailure records that svc's routine failed to start. Best effort:
// the caller is already handling the start failure itself.
func (e *Engine) logStartFailure(svc store.Service, actor Actor, cause error) {
	_ = e.logEvent(store.Event{
		ServiceID: &svc.ID, Level: "error", Actor: string(actor), Type: EventRoutineStartFailed,
		Message: svc.Name + " routine failed to start", Detail: cause.Error(),
	})
}

// logEngineEvent records actor's action on the whole engine.
func (e *Engine) logEngineEvent(actor Actor, eventType, message string) error {
	return e.logEvent(store.Event{Level: "info", Actor: string(actor), Type: eventType, Message: message})
}

// logServiceEvent records actor's action on svc.
func (e *Engine) logServiceEvent(actor Actor, svc store.Service, eventType, message string) error {
	return e.logEvent(store.Event{
		ServiceID: &svc.ID, Level: "info", Actor: string(actor), Type: eventType, Message: message,
	})
}

func (e *Engine) logEvent(ev store.Event) error {
	ev.SessionID = e.session.ID
	if err := e.store.InsertEvent(ev); err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	return nil
}
