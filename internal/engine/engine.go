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
	"strings"
	"sync"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/config"
	"github.com/Bravo-CDT/Scoring/internal/scheduler"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

// ErrServiceNotFound is wrapped into the error of every per-service method
// when the name matches no active service in the session. Callers (the HTTP
// API) use errors.Is to answer such requests with 404 rather than 500.
var ErrServiceNotFound = errors.New("engine: unknown service")

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
	Added   []string       `json:"added"`
	Removed []string       `json:"removed"`
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
// was: resuming scoring is a separate decision (ResumeScoring).
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
//   - A service in both is left alone, routine untouched: edits to its entry
//     take effect on RestartService.
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
	res := ReloadResult{Added: []string{}, Removed: []string{}, Failed: []StartFailure{}}

	e.mu.Lock()
	defer e.mu.Unlock()

	cfg, err := config.Load(configPath)
	if err != nil {
		return res, fmt.Errorf("engine: reloading config: %w", err)
	}
	e.recordLoadedConfigHash(configPath)
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

	if actor == ActorSystem && len(res.Added) == 0 && len(res.Removed) == 0 {
		return res, nil
	}
	msg := fmt.Sprintf("config reloaded: added [%s], removed [%s]",
		strings.Join(res.Added, ", "), strings.Join(res.Removed, ", "))
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
}

// Status reports the session's current state. It does not wait for control
// operations in progress; its reads are individually current but not one
// atomic snapshot.
func (e *Engine) Status() (Status, error) {
	blue, red, err := e.store.GetScore(e.session.ID)
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

	out := Status{
		SessionName:     e.session.Name,
		StartingScore:   e.session.StartingScore,
		BlueScore:       blue,
		RedScore:        red,
		EngineState:     rs.EngineState,
		ScoringPaused:   rs.ScoringPaused,
		ServerTime:      time.Now().UTC(),
		Round:           round,
		NextRoundAt:     nextRoundAt,
		CountdownTarget: rs.CountdownTarget,
		Services:        make([]ServiceStatus, 0, len(svcs)),
	}
	for _, svc := range svcs {
		ss := ServiceStatus{
			Name:           svc.Name,
			RoutineEnabled: e.scheduler.IsRunning(svc.ID),
			Checking:       e.scheduler.IsChecking(svc.ID),
			ScoringPaused:  svc.ScoringPaused,
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
