package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/scheduler"
	"github.com/Bravo-CDT/Scoring/internal/scorer"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

// waitTimeout bounds every polling wait; generous so a loaded machine does
// not flake, while a passing run returns as soon as the condition holds.
const waitTimeout = 5 * time.Second

var adminHash = func() string {
	h, err := bcrypt.GenerateFromPassword([]byte("competition"), bcrypt.MinCost)
	if err != nil {
		panic("generating test bcrypt hash: " + err.Error())
	}
	return string(h)
}()

// svcYAML renders one config entry. Every fixture uses tcp_connect (the only
// check type config validation accepts besides http) backed by a fake check
// registered on a throwaway registry.
func svcYAML(name string, pointsPerFailure int) string {
	return fmt.Sprintf(`
  - name: %s
    host: 127.0.0.1
    check_type: tcp_connect
    interval_seconds: 1
    points_per_failure: %d
    params: {}
`, name, pointsPerFailure)
}

// configYAML renders a whole config.yaml with the named services, each
// costing 10 points per failure.
func configYAML(names ...string) string {
	var services strings.Builder
	for _, n := range names {
		services.WriteString(svcYAML(n, 10))
	}
	return configWithServices(services.String())
}

func configWithServices(services string) string {
	return fmt.Sprintf(`
session_name: "comp-1"
starting_score: 1000
admin:
  username: admin
  password_hash: %q
services:%s`, adminHash, services)
}

// harness is one engine wired to a real store, scheduler and scorer, the way
// the binary assembles them.
type harness struct {
	e          *Engine
	st         *store.Store
	sess       store.Session
	configPath string
	dbPath     string
}

func okCheck(context.Context, string, map[string]any) checks.Result {
	return checks.Result{Success: true, Detail: "ok"}
}

func failCheck(context.Context, string, map[string]any) checks.Result {
	return checks.Result{Success: false, Detail: "connection refused"}
}

// newHarness writes a config with services, creates their rows the way
// startup does, and returns an engine whose checks run check with no startup
// stagger. The engine is stopped and the store closed when the test ends.
func newHarness(t *testing.T, check checks.CheckFunc, services ...string) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{
		configPath: filepath.Join(dir, "config.yaml"),
		dbPath:     filepath.Join(dir, "scoring.db"),
	}
	h.writeConfig(t, configYAML(services...))

	st, err := store.Open(h.dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	h.st = st

	h.sess, err = st.GetOrCreateSession("comp-1", 1000)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	for _, name := range services {
		if _, err := st.GetOrCreateService(h.sess.ID, name); err != nil {
			t.Fatalf("GetOrCreateService(%q): %v", name, err)
		}
	}

	registry := checks.NewRegistry()
	registry.Register("tcp_connect", check)
	sched := scheduler.New(registry, scorer.New(st, h.sess.ID, nil), h.configPath)
	sched.SetMaxStagger(0)
	h.e = New(st, h.sess, sched, h.configPath)
	// Registered after the store's Close, so it runs first.
	t.Cleanup(func() { h.e.Stop() })
	return h
}

func (h *harness) writeConfig(t *testing.T, contents string) {
	t.Helper()
	if err := os.WriteFile(h.configPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
}

func (h *harness) service(t *testing.T, name string) store.Service {
	t.Helper()
	svc, found, err := h.st.GetServiceByName(h.sess.ID, name)
	if err != nil || !found {
		t.Fatalf("GetServiceByName(%q): found=%v err=%v", name, found, err)
	}
	return svc
}

func (h *harness) status(t *testing.T) Status {
	t.Helper()
	s, err := h.e.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	return s
}

func (h *harness) running(t *testing.T, name string) bool {
	t.Helper()
	return h.e.scheduler.IsRunning(h.service(t, name).ID)
}

func (h *harness) events(t *testing.T) []store.Event {
	t.Helper()
	evs, err := h.st.ListEvents(h.sess.ID, 1000, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	return evs
}

// lastEvent returns the newest event of type typ, failing the test if there
// is none.
func (h *harness) lastEvent(t *testing.T, typ string) store.Event {
	t.Helper()
	for _, ev := range h.events(t) {
		if ev.Type == typ {
			return ev
		}
	}
	t.Fatalf("no %q event recorded", typ)
	return store.Event{}
}

func (h *harness) countResults(t *testing.T, name string) int {
	t.Helper()
	rs, err := h.st.ListCheckResults(h.service(t, name).ID, 1000, 0)
	if err != nil {
		t.Fatalf("ListCheckResults: %v", err)
	}
	return len(rs)
}

// snapshotPoints returns points_per_failure of every config snapshot recorded
// for name, oldest first. The store has no reader for snapshots, so this
// opens its own connection to the database file; it only reads.
func (h *harness) snapshotPoints(t *testing.T, name string) []int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+h.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening database for reading: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(
		`SELECT points_per_failure FROM service_config_snapshots WHERE service_id = ? ORDER BY id`,
		h.service(t, name).ID,
	)
	if err != nil {
		t.Fatalf("querying snapshots: %v", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scanning snapshot: %v", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading snapshots: %v", err)
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", waitTimeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func mustDo(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func TestStart_RunsActiveServicesAndLeavesScoringPaused(t *testing.T) {
	h := newHarness(t, okCheck, "svc1", "svc2")
	mustDo(t, "Start", h.e.Start())

	s := h.status(t)
	if s.EngineState != store.EngineRunning {
		t.Errorf("EngineState = %q, want %q", s.EngineState, store.EngineRunning)
	}
	// Opening the store pauses scoring; starting the engine must not undo
	// that — an admin resumes scoring separately.
	if !s.ScoringPaused {
		t.Error("ScoringPaused = false after Start, want scoring to stay paused")
	}
	if len(s.Services) != 2 {
		t.Fatalf("Services = %+v, want 2", s.Services)
	}
	for _, ss := range s.Services {
		if !ss.RoutineEnabled {
			t.Errorf("%s: RoutineEnabled = false, want true", ss.Name)
		}
	}

	ev := h.lastEvent(t, EventEngineStarted)
	if ev.Actor != "admin" || ev.ServiceID != nil {
		t.Errorf("engine_started event = %+v, want actor admin and no service", ev)
	}
}

func TestStart_IsIdempotent(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	mustDo(t, "Start", h.e.Start())
	mustDo(t, "second Start", h.e.Start())

	if !h.running(t, "svc1") {
		t.Error("svc1 not running")
	}
	// A routine that was already running is not restarted, so it gets no
	// second config snapshot.
	if got := h.snapshotPoints(t, "svc1"); len(got) != 1 {
		t.Errorf("snapshots = %v, want exactly one", got)
	}
}

func TestStart_OneServiceFailingDoesNotBlockOthers(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	// ghost is an active service with no entry in config.yaml, so its
	// routine cannot load a config and fails to start.
	ghost, err := h.st.GetOrCreateService(h.sess.ID, "aaa-ghost")
	mustDo(t, "GetOrCreateService", err)

	mustDo(t, "Start", h.e.Start())

	if !h.running(t, "svc1") {
		t.Error("svc1 not running; one service's failure blocked the others")
	}
	if h.e.scheduler.IsRunning(ghost.ID) {
		t.Error("ghost is running despite having no config")
	}
	ev := h.lastEvent(t, EventRoutineStartFailed)
	if ev.Level != "error" || ev.ServiceID == nil || *ev.ServiceID != ghost.ID {
		t.Errorf("routine_start_failed event = %+v, want level error for service %d", ev, ghost.ID)
	}
	if s := h.status(t); s.EngineState != store.EngineRunning {
		t.Errorf("EngineState = %q, want running", s.EngineState)
	}
}

func TestStart_RecordsConfigSnapshotPerRoutineStart(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	mustDo(t, "Start", h.e.Start())
	if got := h.snapshotPoints(t, "svc1"); !slices.Equal(got, []int{10}) {
		t.Fatalf("after Start: snapshots = %v, want [10]", got)
	}

	mustDo(t, "Stop", h.e.Stop())
	h.writeConfig(t, configWithServices(svcYAML("svc1", 25)))
	mustDo(t, "Start again", h.e.Start())
	if got := h.snapshotPoints(t, "svc1"); !slices.Equal(got, []int{10, 25}) {
		t.Errorf("after restart: snapshots = %v, want [10 25]", got)
	}
}

func TestStop_HaltsEveryRoutine(t *testing.T) {
	h := newHarness(t, okCheck, "svc1", "svc2")
	mustDo(t, "Start", h.e.Start())
	mustDo(t, "Stop", h.e.Stop())

	if ids := h.e.scheduler.RunningServiceIDs(); len(ids) != 0 {
		t.Errorf("running after Stop: %v, want none", ids)
	}
	s := h.status(t)
	if s.EngineState != store.EngineStopped {
		t.Errorf("EngineState = %q, want stopped", s.EngineState)
	}
	for _, ss := range s.Services {
		if ss.RoutineEnabled {
			t.Errorf("%s: RoutineEnabled = true after Stop", ss.Name)
		}
	}
	if ev := h.lastEvent(t, EventEngineStopped); ev.Actor != "admin" {
		t.Errorf("engine_stopped actor = %q, want admin", ev.Actor)
	}
}

func TestStopService_PersistsAcrossEngineStopStart(t *testing.T) {
	h := newHarness(t, okCheck, "svc1", "svc2")
	mustDo(t, "Start", h.e.Start())
	mustDo(t, "StopService", h.e.StopService("svc1"))
	if h.running(t, "svc1") {
		t.Fatal("svc1 still running after StopService")
	}

	mustDo(t, "Stop", h.e.Stop())
	mustDo(t, "second Start", h.e.Start())

	if h.running(t, "svc1") {
		t.Error("svc1 restarted by an engine stop/start; its per-service stop must persist")
	}
	if !h.running(t, "svc2") {
		t.Error("svc2 not running after engine restart")
	}

	ev := h.lastEvent(t, EventRoutineStopped)
	if ev.Actor != "admin" || ev.ServiceID == nil || *ev.ServiceID != h.service(t, "svc1").ID {
		t.Errorf("routine_stopped event = %+v, want actor admin for svc1", ev)
	}
}

func TestStopService_WhileEngineStoppedIsRememberedAtStart(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	mustDo(t, "StopService", h.e.StopService("svc1"))
	mustDo(t, "Start", h.e.Start())
	if h.running(t, "svc1") {
		t.Error("svc1 running; StopService before Start should keep it stopped")
	}
}

func TestStartService_WhileEngineStoppedOnlyRecordsDesiredState(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	mustDo(t, "Start", h.e.Start())
	mustDo(t, "StopService", h.e.StopService("svc1"))
	mustDo(t, "Stop", h.e.Stop())

	mustDo(t, "StartService", h.e.StartService("svc1"))
	if h.running(t, "svc1") {
		t.Fatal("StartService started a routine while the engine is stopped")
	}

	mustDo(t, "Start", h.e.Start())
	if !h.running(t, "svc1") {
		t.Error("svc1 not running after engine Start, despite StartService")
	}
}

func TestStartService_WhileRunningStartsRoutine(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	mustDo(t, "Start", h.e.Start())
	mustDo(t, "StopService", h.e.StopService("svc1"))
	mustDo(t, "StartService", h.e.StartService("svc1"))

	if !h.running(t, "svc1") {
		t.Error("svc1 not running after StartService")
	}
	if got := h.snapshotPoints(t, "svc1"); len(got) != 2 {
		t.Errorf("snapshots = %v, want one per routine start (2)", got)
	}
	ev := h.lastEvent(t, EventRoutineStarted)
	if ev.Actor != "admin" || ev.ServiceID == nil {
		t.Errorf("routine_started event = %+v, want actor admin with a service", ev)
	}
}

func TestStartService_ConfigErrorIsReturned(t *testing.T) {
	h := newHarness(t, okCheck, "svc1", "svc2")
	mustDo(t, "Start", h.e.Start())
	mustDo(t, "StopService", h.e.StopService("svc2"))
	// svc2 vanishes from config.yaml without a reload.
	h.writeConfig(t, configYAML("svc1"))

	if err := h.e.StartService("svc2"); err == nil {
		t.Error("StartService succeeded for a service missing from config")
	}
	if h.running(t, "svc2") {
		t.Error("svc2 running despite the failed start")
	}
}

func TestRestartService_RereadsConfig(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	mustDo(t, "Start", h.e.Start())
	h.writeConfig(t, configWithServices(svcYAML("svc1", 40)))

	mustDo(t, "RestartService", h.e.RestartService("svc1", false))

	if !h.running(t, "svc1") {
		t.Error("svc1 not running after RestartService")
	}
	if got := h.snapshotPoints(t, "svc1"); !slices.Equal(got, []int{10, 40}) {
		t.Errorf("snapshots = %v, want [10 40]", got)
	}
	if h.service(t, "svc1").ScoringPaused {
		t.Error("RestartService(pauseScoring=false) paused scoring")
	}
	if ev := h.lastEvent(t, EventRoutineRestarted); ev.Actor != "admin" || ev.ServiceID == nil {
		t.Errorf("routine_restarted event = %+v, want actor admin with a service", ev)
	}
}

func TestRestartService_WithPauseScoring_IsAtomic(t *testing.T) {
	// The check itself reports whether the service's scoring was paused when
	// it ran, so the test sees what the first post-restart check saw.
	var mu sync.Mutex
	var sawPaused []bool
	var h *harness
	check := func(context.Context, string, map[string]any) checks.Result {
		svc, _, err := h.st.GetServiceByName(h.sess.ID, "svc1")
		mu.Lock()
		sawPaused = append(sawPaused, err == nil && svc.ScoringPaused)
		mu.Unlock()
		return checks.Result{Success: false, Detail: "down"}
	}
	h = newHarness(t, check, "svc1")

	mustDo(t, "ResumeScoring", h.e.ResumeScoring())
	// Leave svc1's routine stopped so the restart launches a fresh routine
	// whose first check runs immediately: the pause must already be stored.
	mustDo(t, "StopService", h.e.StopService("svc1"))
	mustDo(t, "Start", h.e.Start())

	mustDo(t, "RestartService", h.e.RestartService("svc1", true))
	if !h.service(t, "svc1").ScoringPaused {
		t.Error("ScoringPaused = false immediately after RestartService(pauseScoring=true)")
	}

	waitFor(t, "a recorded check", func() bool { return h.countResults(t, "svc1") >= 1 })
	mu.Lock()
	first := sawPaused[0]
	mu.Unlock()
	if !first {
		t.Error("the restarted routine's first check ran before scoring was paused")
	}
	if blue, _, err := h.st.GetScore(h.sess.ID); err != nil || blue != 1000 {
		t.Errorf("blue score = %d (err %v), want 1000: a check scored before the pause applied", blue, err)
	}
	if ev := h.lastEvent(t, EventServiceScoringPaused); ev.Actor != "admin" || ev.ServiceID == nil {
		t.Errorf("service_scoring_paused event = %+v, want actor admin with a service", ev)
	}
}

func TestRestartService_WhileEngineStoppedPausesWithoutStarting(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	mustDo(t, "StopService", h.e.StopService("svc1"))

	mustDo(t, "RestartService", h.e.RestartService("svc1", true))

	if h.running(t, "svc1") {
		t.Error("RestartService started a routine while the engine is stopped")
	}
	if !h.service(t, "svc1").ScoringPaused {
		t.Error("pause not persisted by RestartService while the engine is stopped")
	}
	// Restart re-enables the service, so it runs once the engine starts.
	mustDo(t, "Start", h.e.Start())
	if !h.running(t, "svc1") {
		t.Error("svc1 not running after Start, despite RestartService enabling it")
	}
}

func TestPauseAndResumeServiceScoring(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")

	mustDo(t, "PauseServiceScoring", h.e.PauseServiceScoring("svc1"))
	if !h.service(t, "svc1").ScoringPaused || !h.status(t).Services[0].ScoringPaused {
		t.Error("svc1 not scoring-paused after PauseServiceScoring")
	}
	ev := h.lastEvent(t, EventServiceScoringPaused)
	if ev.Actor != "admin" || ev.ServiceID == nil || *ev.ServiceID != h.service(t, "svc1").ID {
		t.Errorf("service_scoring_paused event = %+v, want actor admin for svc1", ev)
	}

	mustDo(t, "ResumeServiceScoring", h.e.ResumeServiceScoring("svc1"))
	if h.service(t, "svc1").ScoringPaused {
		t.Error("svc1 still scoring-paused after ResumeServiceScoring")
	}
	if ev := h.lastEvent(t, EventServiceScoringResumed); ev.Actor != "admin" || ev.ServiceID == nil {
		t.Errorf("service_scoring_resumed event = %+v, want actor admin with a service", ev)
	}
}

func TestPauseAndResumeScoring(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")

	mustDo(t, "ResumeScoring", h.e.ResumeScoring())
	if h.status(t).ScoringPaused {
		t.Error("ScoringPaused = true after ResumeScoring")
	}
	mustDo(t, "PauseScoring", h.e.PauseScoring())
	if !h.status(t).ScoringPaused {
		t.Error("ScoringPaused = false after PauseScoring")
	}
	// Neither touches the engine state.
	if s := h.status(t); s.EngineState != store.EngineStopped {
		t.Errorf("EngineState = %q, want stopped", s.EngineState)
	}
	for _, typ := range []string{EventScoringPaused, EventScoringResumed} {
		if ev := h.lastEvent(t, typ); ev.Actor != "admin" {
			t.Errorf("%s actor = %q, want admin", typ, ev.Actor)
		}
	}
}

// TestScoringPausesNotBlockedByStuckControlOp: pausing scoring is the admin's
// emergency lever, so it must work even while another control operation is
// stuck — here a StopService waiting on a check that ignores cancellation.
func TestScoringPausesNotBlockedByStuckControlOp(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	inCheck := make(chan struct{}, 1)
	stuckCheck := func(context.Context, string, map[string]any) checks.Result {
		select {
		case inCheck <- struct{}{}:
		default:
		}
		<-release // deliberately ignores ctx
		return checks.Result{Success: true}
	}
	h := newHarness(t, stuckCheck, "svc1")
	// Runs before the harness's e.Stop cleanup, which would otherwise hang.
	t.Cleanup(unblock)

	mustDo(t, "Start", h.e.Start())
	select {
	case <-inCheck:
	case <-time.After(waitTimeout):
		t.Fatal("check never started")
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- h.e.StopService("svc1") }()
	// Wait until StopService holds the control lock (and is stuck in it).
	waitFor(t, "StopService to hold the control lock", func() bool {
		if h.e.mu.TryLock() {
			h.e.mu.Unlock()
			return false
		}
		return true
	})

	ops := []struct {
		name string
		call func() error
	}{
		{"PauseScoring", h.e.PauseScoring},
		{"ResumeScoring", h.e.ResumeScoring},
		{"PauseServiceScoring", func() error { return h.e.PauseServiceScoring("svc1") }},
		{"ResumeServiceScoring", func() error { return h.e.ResumeServiceScoring("svc1") }},
	}
	for _, op := range ops {
		done := make(chan error, 1)
		go func() { done <- op.call() }()
		select {
		case err := <-done:
			mustDo(t, op.name, err)
		case <-time.After(2 * time.Second):
			t.Fatalf("%s blocked behind a stuck control operation", op.name)
		}
	}
	select {
	case <-stopDone:
		t.Fatal("StopService finished before the check was released; the test proved nothing")
	default:
	}

	unblock()
	select {
	case err := <-stopDone:
		mustDo(t, "StopService", err)
	case <-time.After(waitTimeout):
		t.Fatal("StopService did not finish after the check was released")
	}
}

func TestFailuresScoreOnlyAfterResumeScoring(t *testing.T) {
	h := newHarness(t, failCheck, "svc1")
	mustDo(t, "Start", h.e.Start())

	waitFor(t, "a recorded check", func() bool { return h.countResults(t, "svc1") >= 1 })
	if s := h.status(t); s.BlueScore != 1000 || s.RedScore != 0 {
		t.Fatalf("score moved while globally paused: blue=%d red=%d", s.BlueScore, s.RedScore)
	}

	mustDo(t, "ResumeScoring", h.e.ResumeScoring())
	waitFor(t, "blue to lose points", func() bool { return h.status(t).BlueScore < 1000 })
}

func TestPerServiceMethods_UnknownNameIsErrServiceNotFound(t *testing.T) {
	h := newHarness(t, okCheck, "svc1", "retired")
	mustDo(t, "SetServiceActive", h.st.SetServiceActive(h.service(t, "retired").ID, false))

	methods := map[string]func(name string) error{
		"StartService":         h.e.StartService,
		"StopService":          h.e.StopService,
		"RestartService":       func(n string) error { return h.e.RestartService(n, true) },
		"PauseServiceScoring":  h.e.PauseServiceScoring,
		"ResumeServiceScoring": h.e.ResumeServiceScoring,
	}
	for method, call := range methods {
		for _, name := range []string{"ghost", "retired"} {
			err := call(name)
			if !errors.Is(err, ErrServiceNotFound) {
				t.Errorf("%s(%q) = %v, want ErrServiceNotFound", method, name, err)
			}
		}
	}

	// The lookups created nothing, and the retired service was not touched.
	if _, found, _ := h.st.GetServiceByName(h.sess.ID, "ghost"); found {
		t.Error("an unknown name created a service row")
	}
	if retired := h.service(t, "retired"); retired.Active || retired.ScoringPaused {
		t.Errorf("retired service modified: %+v", retired)
	}
}

func TestReload_AddsAndRemovesServicesWhileRunning(t *testing.T) {
	h := newHarness(t, okCheck, "svc1", "svc2")
	mustDo(t, "Start", h.e.Start())

	h.writeConfig(t, configYAML("svc2", "svc3"))
	added, removed, err := h.e.Reload(h.configPath)
	mustDo(t, "Reload", err)

	if !slices.Equal(added, []string{"svc3"}) {
		t.Errorf("added = %v, want [svc3]", added)
	}
	if !slices.Equal(removed, []string{"svc1"}) {
		t.Errorf("removed = %v, want [svc1]", removed)
	}

	s := h.status(t)
	var names []string
	for _, ss := range s.Services {
		names = append(names, ss.Name)
		if !ss.RoutineEnabled {
			t.Errorf("%s not running after reload", ss.Name)
		}
	}
	if !slices.Equal(names, []string{"svc2", "svc3"}) {
		t.Errorf("active services = %v, want [svc2 svc3]", names)
	}
	if svc1 := h.service(t, "svc1"); svc1.Active || h.e.scheduler.IsRunning(svc1.ID) {
		t.Errorf("svc1 still active or running after removal: %+v", svc1)
	}
	// svc2 was left alone: its routine was not restarted.
	if got := h.snapshotPoints(t, "svc2"); len(got) != 1 {
		t.Errorf("svc2 snapshots = %v, want 1 (untouched by reload)", got)
	}
	if got := h.snapshotPoints(t, "svc3"); len(got) != 1 {
		t.Errorf("svc3 snapshots = %v, want 1", got)
	}
	if s.EngineState != store.EngineRunning {
		t.Errorf("EngineState = %q, want running", s.EngineState)
	}

	for _, typ := range []string{EventServiceAdded, EventServiceRemoved} {
		if ev := h.lastEvent(t, typ); ev.Actor != "admin" || ev.ServiceID == nil {
			t.Errorf("%s event = %+v, want actor admin with a service", typ, ev)
		}
	}
	if ev := h.lastEvent(t, EventConfigReloaded); ev.Actor != "admin" {
		t.Errorf("config_reloaded actor = %q, want admin", ev.Actor)
	}
}

func TestReload_WhileStoppedStartsNothingAndLeavesRunState(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	h.writeConfig(t, configYAML("svc1", "svc2"))

	added, _, err := h.e.Reload(h.configPath)
	mustDo(t, "Reload", err)
	if !slices.Equal(added, []string{"svc2"}) {
		t.Errorf("added = %v, want [svc2]", added)
	}
	if ids := h.e.scheduler.RunningServiceIDs(); len(ids) != 0 {
		t.Errorf("Reload started routines %v while the engine is stopped", ids)
	}
	s := h.status(t)
	if s.EngineState != store.EngineStopped || !s.ScoringPaused {
		t.Errorf("run state = %q/paused=%v, want stopped/paused untouched", s.EngineState, s.ScoringPaused)
	}

	mustDo(t, "Start", h.e.Start())
	if !h.running(t, "svc2") {
		t.Error("svc2 added while stopped did not start with the engine")
	}
}

func TestReload_ReactivatesRemovedServiceWithHistory(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	original := h.service(t, "svc1")
	_, err := h.st.InsertCheckResult(store.CheckResult{SessionID: h.sess.ID, ServiceID: original.ID, Success: true})
	mustDo(t, "InsertCheckResult", err)
	mustDo(t, "PauseServiceScoring", h.e.PauseServiceScoring("svc1"))

	h.writeConfig(t, configYAML())
	_, removed, err := h.e.Reload(h.configPath)
	mustDo(t, "first Reload", err)
	if !slices.Equal(removed, []string{"svc1"}) {
		t.Fatalf("removed = %v, want [svc1]", removed)
	}

	h.writeConfig(t, configYAML("svc1"))
	added, _, err := h.e.Reload(h.configPath)
	mustDo(t, "second Reload", err)
	if !slices.Equal(added, []string{"svc1"}) {
		t.Fatalf("added = %v, want [svc1]", added)
	}

	back := h.service(t, "svc1")
	if back.ID != original.ID || !back.Active {
		t.Errorf("reactivated service = %+v, want the original row (id %d) active again", back, original.ID)
	}
	if !back.ScoringPaused {
		t.Error("reactivation reset the operator's scoring pause")
	}
	if n := h.countResults(t, "svc1"); n != 1 {
		t.Errorf("check results after reactivation = %d, want the original 1", n)
	}
}

func TestReload_InvalidConfigChangesNothing(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	mustDo(t, "Start", h.e.Start())
	h.writeConfig(t, "services: [not valid")

	if _, _, err := h.e.Reload(h.configPath); err == nil {
		t.Fatal("Reload of an invalid config succeeded")
	}
	if svc := h.service(t, "svc1"); !svc.Active || !h.running(t, "svc1") {
		t.Error("a failed reload deactivated or stopped svc1")
	}
}

func TestReload_NoChangesReturnsEmptySlices(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	added, removed, err := h.e.Reload(h.configPath)
	mustDo(t, "Reload", err)
	if added == nil || removed == nil || len(added) != 0 || len(removed) != 0 {
		t.Errorf("added=%#v removed=%#v, want empty non-nil slices", added, removed)
	}
}

func TestStatus_ReportsLatestResultAndConsecutiveFailures(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	svc := h.service(t, "svc1")
	base := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for i, ok := range []bool{true, false, false, false} {
		_, err := h.st.InsertCheckResult(store.CheckResult{
			SessionID: h.sess.ID, ServiceID: svc.ID,
			Timestamp: base.Add(time.Duration(i) * time.Second), Success: ok, LatencyMS: int64(10 + i),
		})
		mustDo(t, "InsertCheckResult", err)
	}

	s := h.status(t)
	if s.SessionName != "comp-1" || s.StartingScore != 1000 || s.BlueScore != 1000 || s.RedScore != 0 {
		t.Errorf("session fields = %+v", s)
	}
	if time.Since(s.ServerTime) > time.Minute || s.ServerTime.Location() != time.UTC {
		t.Errorf("ServerTime = %v, want now in UTC", s.ServerTime)
	}
	ss := s.Services[0]
	if ss.Up {
		t.Error("Up = true, want false (latest result failed)")
	}
	if ss.ConsecutiveFailures != 3 {
		t.Errorf("ConsecutiveFailures = %d, want 3", ss.ConsecutiveFailures)
	}
	if ss.LastCheckTime == nil || !ss.LastCheckTime.Equal(base.Add(3*time.Second)) {
		t.Errorf("LastCheckTime = %v, want %v", ss.LastCheckTime, base.Add(3*time.Second))
	}
	if ss.LatencyMS != 13 {
		t.Errorf("LatencyMS = %d, want 13", ss.LatencyMS)
	}
}

func TestStatus_JSONShape(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	raw, err := json.Marshal(h.status(t))
	mustDo(t, "Marshal", err)

	var got map[string]any
	mustDo(t, "Unmarshal", json.Unmarshal(raw, &got))
	for _, key := range []string{"session_name", "starting_score", "blue_score", "red_score",
		"engine_state", "scoring_paused", "server_time", "services"} {
		if _, ok := got[key]; !ok {
			t.Errorf("status JSON missing %q: %s", key, raw)
		}
	}
	svc := got["services"].([]any)[0].(map[string]any)
	for _, key := range []string{"name", "up", "routine_enabled", "scoring_paused", "latency_ms", "consecutive_failures"} {
		if _, ok := svc[key]; !ok {
			t.Errorf("service JSON missing %q: %s", key, raw)
		}
	}
	// No check has run yet, so there is no last check time to report.
	if _, ok := svc["last_check_time"]; ok {
		t.Errorf("last_check_time present before any check: %s", raw)
	}
}

func TestStatus_NoServicesIsEmptyList(t *testing.T) {
	h := newHarness(t, okCheck)
	raw, err := json.Marshal(h.status(t))
	mustDo(t, "Marshal", err)
	if !strings.Contains(string(raw), `"services":[]`) {
		t.Errorf("status JSON = %s, want services as an empty list", raw)
	}
}

// TestConcurrentControlOperations hammers the control plane from several
// goroutines; once it settles with a final Stop, no routine may be left
// running behind a stopped engine.
func TestConcurrentControlOperations(t *testing.T) {
	h := newHarness(t, okCheck, "svc1", "svc2")
	ops := []func() error{
		h.e.Start,
		h.e.Stop,
		func() error { return h.e.StartService("svc1") },
		func() error { return h.e.StopService("svc1") },
		func() error { return h.e.RestartService("svc2", false) },
		func() error { _, _, err := h.e.Reload(h.configPath); return err },
		func() error { _, err := h.e.Status(); return err },
	}
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Go(func() {
			for i := range 20 {
				if err := ops[(g+i)%len(ops)](); err != nil {
					t.Errorf("op %d: %v", (g+i)%len(ops), err)
				}
			}
		})
	}
	wg.Wait()

	mustDo(t, "final Stop", h.e.Stop())
	if ids := h.e.scheduler.RunningServiceIDs(); len(ids) != 0 {
		t.Errorf("routines %v left running after the engine stopped", ids)
	}
}
