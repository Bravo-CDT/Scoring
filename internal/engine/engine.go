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
)

// Event actors.
const (
	actorAdmin  = "admin"
	actorSystem = "system"
)

// Engine is the control plane for one session. It is safe for concurrent use.
type Engine struct {
	store      *store.Store
	session    store.Session
	scheduler  *scheduler.Scheduler
	configPath string

	// mu serializes the control operations (everything but Status) and guards
	// desiredEnabled. Serializing is what keeps each operation's
	// check-then-act sound: Stop cannot interleave with a StartService that
	// has already seen the engine running and so leave a routine running
	// behind a stopped engine, and a routine is known to be running exactly
	// when the scheduler says so.
	//
	// It is held across scheduler calls that block, such as StopService
	// waiting for a routine to exit. That cannot deadlock: nothing a routine
	// does (running its check, the scorer recording the result) ever calls
	// back into the Engine, and the Engine never holds a database connection
	// (an open transaction or unclosed rows) while it waits. Status does not
	// take mu, so reads stay responsive while an operation waits.
	mu sync.Mutex
	// desiredEnabled holds each service's admin-chosen routine state; a
	// service absent from it is enabled.
	desiredEnabled map[int64]bool
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
// logged as an error event and skipped; it does not stop the others starting,
// and does not make Start fail. Start on a running engine starts any enabled
// routine that is not running and is otherwise a no-op.
func (e *Engine) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	rs, err := e.store.GetRunState()
	if err != nil {
		return fmt.Errorf("engine: starting: %w", err)
	}
	svcs, err := e.store.ListActiveServices(e.session.ID)
	if err != nil {
		return fmt.Errorf("engine: starting: %w", err)
	}
	if err := e.store.SetEngineState(store.EngineRunning); err != nil {
		return fmt.Errorf("engine: starting: %w", err)
	}

	for _, svc := range svcs {
		if e.enabled(svc.ID) {
			_ = e.startRoutine(svc) // a failure is logged by startRoutine
		}
	}

	if rs.EngineState == store.EngineRunning {
		return nil
	}
	return e.logEngineEvent(EventEngineStarted, "engine started")
}

// Stop marks the engine stopped and stops every routine, waiting for each to
// exit. Admins' per-service choices are kept for the next Start.
func (e *Engine) Stop() error {
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
	return e.logEngineEvent(EventEngineStopped, "engine stopped")
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

func (e *Engine) setGlobalScoringPaused(paused bool, eventType, message string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if err := e.store.SetGlobalScoringPaused(paused); err != nil {
		return fmt.Errorf("engine: %s: %w", message, err)
	}
	return e.logEngineEvent(eventType, message)
}

// StartService enables the named service's routine and, if the engine is
// running, starts it. While the engine is stopped it only records that the
// service should run once the engine starts.
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
		return e.logServiceEvent(svc, EventRoutineStarted,
			svc.Name+" routine enabled; it starts when the engine starts")
	}
	if err := e.startRoutine(svc); err != nil {
		return err
	}
	return e.logServiceEvent(svc, EventRoutineStarted, svc.Name+" routine started")
}

// StopService disables the named service's routine and stops it, waiting for
// it to exit. The service stays stopped across engine stops and starts until
// StartService or RestartService enables it again.
func (e *Engine) StopService(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	svc, err := e.lookup(name)
	if err != nil {
		return err
	}
	e.desiredEnabled[svc.ID] = false
	e.scheduler.StopService(svc.ID)
	return e.logServiceEvent(svc, EventRoutineStopped, svc.Name+" routine stopped")
}

// RestartService enables the named service's routine and, if the engine is
// running, restarts it with its entry freshly read from config.yaml — which is
// how an edit to that entry takes effect. With pauseScoring, the service's
// scoring pause is persisted first, before any restarted routine can run a
// check, so not one result from the new configuration moves points until an
// admin resumes the service.
//
// If the engine is stopped the routine is not started; the pause (if asked
// for) and the enabled state still apply.
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
		if err := e.logServiceEvent(svc, EventServiceScoringPaused,
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
		return e.logServiceEvent(svc, EventRoutineRestarted,
			svc.Name+" routine enabled; it starts when the engine starts")
	}
	if err := e.scheduler.RestartService(svc.ID, svc.Name); err != nil {
		e.logStartFailure(svc, actorAdmin, err)
		return fmt.Errorf("engine: restarting %s: %w", svc.Name, err)
	}
	e.snapshotConfig(svc)
	return e.logServiceEvent(svc, EventRoutineRestarted, svc.Name+" routine restarted with reloaded config")
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

func (e *Engine) setServiceScoringPaused(name string, paused bool, eventType, what string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	svc, err := e.lookup(name)
	if err != nil {
		return err
	}
	if err := e.store.SetServiceScoringPaused(svc.ID, paused); err != nil {
		return fmt.Errorf("engine: %s %s: %w", svc.Name, what, err)
	}
	return e.logServiceEvent(svc, eventType, svc.Name+" "+what)
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
// invalid file changes nothing. Added names are in config order, removed ones
// in name order; both are non-nil. If a store write fails partway the error
// is returned with the changes made so far.
func (e *Engine) Reload(configPath string) (added, removed []string, err error) {
	if configPath == "" {
		configPath = e.configPath
	}
	added, removed = []string{}, []string{}

	e.mu.Lock()
	defer e.mu.Unlock()

	cfg, err := config.Load(configPath)
	if err != nil {
		return added, removed, fmt.Errorf("engine: reloading config: %w", err)
	}
	active, err := e.store.ListActiveServices(e.session.ID)
	if err != nil {
		return added, removed, fmt.Errorf("engine: reloading config: %w", err)
	}
	running, err := e.engineRunning()
	if err != nil {
		return added, removed, err
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
		if err := e.retire(svc); err != nil {
			return added, removed, err
		}
		removed = append(removed, svc.Name)
	}
	for _, sc := range cfg.Services {
		if isActive[sc.Name] {
			continue
		}
		if err := e.activate(sc.Name, running); err != nil {
			return added, removed, err
		}
		added = append(added, sc.Name)
	}

	msg := fmt.Sprintf("config reloaded: added [%s], removed [%s]",
		strings.Join(added, ", "), strings.Join(removed, ", "))
	return added, removed, e.logEngineEvent(EventConfigReloaded, msg)
}

// retire stops svc's routine and deactivates it. The caller holds mu.
func (e *Engine) retire(svc store.Service) error {
	e.scheduler.StopService(svc.ID)
	if err := e.store.SetServiceActive(svc.ID, false); err != nil {
		return fmt.Errorf("engine: retiring %s: %w", svc.Name, err)
	}
	// Re-adding the name later enables it afresh.
	delete(e.desiredEnabled, svc.ID)
	return e.logServiceEvent(svc, EventServiceRemoved, svc.Name+" removed from config; deactivated")
}

// activate creates or reactivates the service called name, enables it, and
// starts its routine if the engine is running. A routine that fails to start
// is logged and does not fail the reload. The caller holds mu.
func (e *Engine) activate(name string, running bool) error {
	svc, err := e.store.GetOrCreateService(e.session.ID, name)
	if err != nil {
		return fmt.Errorf("engine: adding %s: %w", name, err)
	}
	msg := name + " added from config"
	if !svc.Active {
		if err := e.store.SetServiceActive(svc.ID, true); err != nil {
			return fmt.Errorf("engine: reactivating %s: %w", name, err)
		}
		svc.Active = true
		msg = name + " re-added from config; reactivated with its history"
	}
	e.desiredEnabled[svc.ID] = true
	if err := e.logServiceEvent(svc, EventServiceAdded, msg); err != nil {
		return err
	}
	if running {
		_ = e.startRoutine(svc) // logged by startRoutine
	}
	return nil
}

// ServiceStatus is one active service's current state.
type ServiceStatus struct {
	Name string `json:"name"`
	// Up is whether the latest check succeeded; false before any check.
	Up bool `json:"up"`
	// RoutineEnabled is whether the service's check routine is running.
	RoutineEnabled bool `json:"routine_enabled"`
	// ScoringPaused is the service's own pause; the global one is in Status.
	ScoringPaused bool `json:"scoring_paused"`
	// LastCheckTime is when the latest check ran; omitted before any check.
	LastCheckTime       *time.Time `json:"last_check_time,omitempty"`
	LatencyMS           int64      `json:"latency_ms"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
}

// Status is a point-in-time view of the session: score, run state, and every
// active service, ordered by name.
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

	out := Status{
		SessionName:   e.session.Name,
		StartingScore: e.session.StartingScore,
		BlueScore:     blue,
		RedScore:      red,
		EngineState:   rs.EngineState,
		ScoringPaused: rs.ScoringPaused,
		ServerTime:    time.Now().UTC(),
		Services:      make([]ServiceStatus, 0, len(svcs)),
	}
	for _, svc := range svcs {
		ss := ServiceStatus{
			Name:           svc.Name,
			RoutineEnabled: e.scheduler.IsRunning(svc.ID),
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
func (e *Engine) startRoutine(svc store.Service) error {
	if e.scheduler.IsRunning(svc.ID) {
		return nil
	}
	if err := e.scheduler.StartService(svc.ID, svc.Name); err != nil {
		e.logStartFailure(svc, actorSystem, err)
		return fmt.Errorf("engine: starting %s: %w", svc.Name, err)
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
			ServiceID: &svc.ID, Level: "error", Actor: actorSystem, Type: EventConfigSnapshotFailed,
			Message: "could not record the config " + svc.Name + " started with", Detail: err.Error(),
		})
	}
}

// logStartFailure records that svc's routine failed to start. Best effort:
// the caller is already handling the start failure itself.
func (e *Engine) logStartFailure(svc store.Service, actor string, cause error) {
	_ = e.logEvent(store.Event{
		ServiceID: &svc.ID, Level: "error", Actor: actor, Type: EventRoutineStartFailed,
		Message: svc.Name + " routine failed to start", Detail: cause.Error(),
	})
}

// logEngineEvent records an admin action on the whole engine.
func (e *Engine) logEngineEvent(eventType, message string) error {
	return e.logEvent(store.Event{Level: "info", Actor: actorAdmin, Type: eventType, Message: message})
}

// logServiceEvent records an admin action on svc.
func (e *Engine) logServiceEvent(svc store.Service, eventType, message string) error {
	return e.logEvent(store.Event{
		ServiceID: &svc.ID, Level: "info", Actor: actorAdmin, Type: eventType, Message: message,
	})
}

func (e *Engine) logEvent(ev store.Event) error {
	ev.SessionID = e.session.ID
	if err := e.store.InsertEvent(ev); err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	return nil
}
