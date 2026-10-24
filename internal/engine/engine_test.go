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
    points_per_failure: %d
    params: {port: 9}
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
round_interval_seconds: 1
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

// testRound is the harness scheduler's round interval: short, so a started
// service is checked within milliseconds.
const testRound = 20 * time.Millisecond

// newHarness writes a config with services, creates their rows the way
// startup does, and returns an engine whose checks run check in testRound
// rounds. The round loop and the engine are stopped and the store closed when
// the test ends.
func newHarness(t *testing.T, check checks.CheckFunc, services ...string) *harness {
	t.Helper()
	return newHarnessWithSink(t, check, nil, services...)
}

// newHarnessWithSink is newHarness with the scheduler reporting to
// wrap(scorer) instead of straight to the scorer, when wrap is non-nil.
func newHarnessWithSink(t *testing.T, check checks.CheckFunc, wrap func(scheduler.ResultSink) scheduler.ResultSink, services ...string) *harness {
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
	var sink scheduler.ResultSink = scorer.New(st, h.sess.ID)
	if wrap != nil {
		sink = wrap(sink)
	}
	sched := scheduler.New(registry, sink, h.configPath, testRound)
	h.e = New(st, h.sess, sched, h.configPath)
	// Registered after the store's Close, so it runs first.
	t.Cleanup(func() { h.e.Stop() })
	// Registered last, so it runs first: as in app.Close, the round loop is
	// gone before anything it records into is torn down.
	ctx, cancel := context.WithCancel(context.Background())
	done := sched.Run(ctx, 1)
	t.Cleanup(func() { cancel(); <-done })
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

// start starts the engine, failing the test on an error or on any routine
// that fails to start.
func (h *harness) start(t *testing.T) {
	t.Helper()
	failed, err := h.e.Start()
	mustDo(t, "Start", err)
	if failed == nil || len(failed) != 0 {
		t.Fatalf("Start: failed = %#v, want an empty list", failed)
	}
}

// reloadParts splits a Reload result for tests that look only at the added
// and removed names.
func reloadParts(res ReloadResult, err error) (added, removed []string, _ error) {
	return res.Added, res.Removed, err
}

// breakConfig rewrites the config so that it no longer validates (svc2 gets
// a misspelled param), which makes every routine (re)start fail to load its
// entry while routines already running carry on.
func (h *harness) breakConfig(t *testing.T) {
	t.Helper()
	h.writeConfig(t, configWithServices(svcYAML("svc1", 10)+
		strings.Replace(svcYAML("svc2", 10), "{port: 9}", "{prot: 9}", 1)))
}

// wantStartError checks err is a *StartError for service.
func wantStartError(t *testing.T, err error, service string) {
	t.Helper()
	var se *StartError
	if !errors.As(err, &se) || se.Service != service {
		t.Fatalf("error = %v, want a *StartError for %s", err, service)
	}
	if f := se.Failure(); f.Service != service || !strings.Contains(f.Error, "prot") {
		t.Errorf("Failure() = %+v, want %s with the config error naming prot", f, service)
	}
}

// A restart on a broken config leaves the service enabled but stopped; Start
// retries it and reports the failure, while svc1 keeps running untouched.
func TestStart_ReportsRoutinesThatFailToStart(t *testing.T) {
	h := newHarness(t, okCheck, "svc1", "svc2")
	h.start(t)
	h.breakConfig(t)

	err := h.e.RestartService("svc2", true)
	wantStartError(t, err, "svc2")
	if h.running(t, "svc2") || !h.running(t, "svc1") {
		t.Fatalf("running: svc1=%v svc2=%v, want true/false", h.running(t, "svc1"), h.running(t, "svc2"))
	}
	if !h.service(t, "svc2").ScoringPaused {
		t.Error("the requested scoring pause was not applied although the restart failed")
	}

	failed, err := h.e.Start()
	mustDo(t, "Start", err)
	if len(failed) != 1 || failed[0].Service != "svc2" || !strings.Contains(failed[0].Error, "prot") {
		t.Errorf("failed = %+v, want svc2 with the config error", failed)
	}
	if !h.running(t, "svc1") {
		t.Error("svc1 stopped by a failing Start")
	}

	// Once the config is fixed the next Start brings svc2 back, with an
	// empty failure list.
	h.writeConfig(t, configYAML("svc1", "svc2"))
	h.start(t)
	if !h.running(t, "svc2") {
		t.Error("svc2 not started after the config was fixed")
	}
}

func TestStartService_StartFailureIsAStartError(t *testing.T) {
	h := newHarness(t, okCheck, "svc1", "svc2")
	h.start(t)
	mustDo(t, "StopService", h.e.StopService("svc2"))
	h.breakConfig(t)

	wantStartError(t, h.e.StartService("svc2"), "svc2")
	if h.running(t, "svc2") || !h.running(t, "svc1") {
		t.Errorf("running: svc1=%v svc2=%v, want true/false", h.running(t, "svc1"), h.running(t, "svc2"))
	}
	// The enable took effect: svc2 starts with the engine once the config is
	// fixed.
	h.writeConfig(t, configYAML("svc1", "svc2"))
	h.start(t)
	if !h.running(t, "svc2") {
		t.Error("svc2 not enabled by the failed StartService")
	}
}

func TestReload_ReportsAddedRoutinesThatFailToStart(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	h.start(t)
	// svc3 is valid config, but the harness registry has no http check, so
	// its routine cannot start.
	h.writeConfig(t, configWithServices(svcYAML("svc1", 10)+`
  - name: svc3
    host: 127.0.0.1
    check_type: http
    points_per_failure: 10
    params: {port: 80}
`))

	res, err := h.e.Reload("")
	mustDo(t, "Reload", err)
	if !slices.Equal(res.Added, []string{"svc3"}) || len(res.Removed) != 0 {
		t.Fatalf("Reload = %+v, want svc3 added", res)
	}
	if len(res.Failed) != 1 || res.Failed[0].Service != "svc3" || !strings.Contains(res.Failed[0].Error, "http") {
		t.Errorf("Failed = %+v, want svc3 with an error naming the check type", res.Failed)
	}
	if !h.running(t, "svc1") || h.running(t, "svc3") {
		t.Errorf("running: svc1=%v svc3=%v, want true/false", h.running(t, "svc1"), h.running(t, "svc3"))
	}
	if !h.service(t, "svc3").Active {
		t.Error("svc3 not added although only its start failed")
	}
}

func TestStart_RunsActiveServicesAndLeavesScoringPaused(t *testing.T) {
	h := newHarness(t, okCheck, "svc1", "svc2")
	h.start(t)

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
	h.start(t)
	h.start(t)

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

	failed, err := h.e.Start()
	mustDo(t, "Start", err)
	if len(failed) != 1 || failed[0].Service != "aaa-ghost" {
		t.Errorf("failed = %+v, want aaa-ghost reported", failed)
	}

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
	h.start(t)
	if got := h.snapshotPoints(t, "svc1"); !slices.Equal(got, []int{10}) {
		t.Fatalf("after Start: snapshots = %v, want [10]", got)
	}

	mustDo(t, "Stop", h.e.Stop())
	h.writeConfig(t, configWithServices(svcYAML("svc1", 25)))
	h.start(t)
	if got := h.snapshotPoints(t, "svc1"); !slices.Equal(got, []int{10, 25}) {
		t.Errorf("after restart: snapshots = %v, want [10 25]", got)
	}
}

func TestStatus_CheckingReflectsAnInFlightCheck(t *testing.T) {
	checkStarted := make(chan struct{})
	release := make(chan struct{})
	slowCheck := func(context.Context, string, map[string]any) checks.Result {
		close(checkStarted)
		<-release
		return checks.Result{Success: true}
	}
	h := newHarness(t, slowCheck, "svc1")
	if h.status(t).Services[0].Checking {
		t.Fatal("Checking is true before the engine has started")
	}
	h.start(t) // starting the engine runs a round at once

	<-checkStarted
	if !h.status(t).Services[0].Checking {
		t.Fatal("Checking is false while svc1's check is still running")
	}
	close(release)
	waitFor(t, "svc1's check to finish", func() bool { return !h.status(t).Services[0].Checking })
}

func TestStop_HaltsEveryRoutine(t *testing.T) {
	h := newHarness(t, okCheck, "svc1", "svc2")
	h.start(t)
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
	h.start(t)
	mustDo(t, "StopService", h.e.StopService("svc1"))
	if h.running(t, "svc1") {
		t.Fatal("svc1 still running after StopService")
	}

	mustDo(t, "Stop", h.e.Stop())
	h.start(t)

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
	h.start(t)
	if h.running(t, "svc1") {
		t.Error("svc1 running; StopService before Start should keep it stopped")
	}
}

func TestStartService_WhileEngineStoppedOnlyRecordsDesiredState(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	h.start(t)
	mustDo(t, "StopService", h.e.StopService("svc1"))
	mustDo(t, "Stop", h.e.Stop())

	mustDo(t, "StartService", h.e.StartService("svc1"))
	if h.running(t, "svc1") {
		t.Fatal("StartService started a routine while the engine is stopped")
	}

	h.start(t)
	if !h.running(t, "svc1") {
		t.Error("svc1 not running after engine Start, despite StartService")
	}
}

func TestStartService_WhileRunningStartsRoutine(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	h.start(t)
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
	h.start(t)
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
	h.start(t)
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
	h.start(t)

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
	h.start(t)
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

func TestAdjustScore_RejectedUnlessScoringIsPaused(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	h.start(t)
	mustDo(t, "ResumeScoring", h.e.ResumeScoring())
	if err := h.e.AdjustScore(store.SideBlue, 10, "test"); !errors.Is(err, ErrScoringNotPaused) {
		t.Fatalf("AdjustScore while scoring is not paused: got %v, want ErrScoringNotPaused", err)
	}
}

func TestAdjustScore_RejectsDeltaBeyondTheMax(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	if err := h.e.AdjustScore(store.SideBlue, MaxAdjustmentPoints+1, "test"); !errors.Is(err, ErrInvalidAdjustment) {
		t.Fatalf("AdjustScore(MaxAdjustmentPoints+1): got %v, want ErrInvalidAdjustment", err)
	}
	if err := h.e.AdjustScore(store.SideBlue, -(MaxAdjustmentPoints + 1), "test"); !errors.Is(err, ErrInvalidAdjustment) {
		t.Fatalf("AdjustScore(-(MaxAdjustmentPoints+1)): got %v, want ErrInvalidAdjustment", err)
	}
	// Exactly the max, in either direction, is still allowed.
	if err := h.e.AdjustScore(store.SideBlue, MaxAdjustmentPoints, "test"); err != nil {
		t.Errorf("AdjustScore(MaxAdjustmentPoints): %v, want no error", err)
	}
}

func TestAdjustScore_RejectsAnUnknownSideAndZeroDelta(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	if err := h.e.AdjustScore("manual", 10, "test"); !errors.Is(err, ErrInvalidAdjustment) {
		t.Fatalf("AdjustScore(manual side): got %v, want ErrInvalidAdjustment", err)
	}
	if err := h.e.AdjustScore(store.SideBlue, 0, "test"); !errors.Is(err, ErrInvalidAdjustment) {
		t.Fatalf("AdjustScore(0 delta): got %v, want ErrInvalidAdjustment", err)
	}
}

func TestAdjustScore_AllowedWhileScoringPausedEvenIfEngineRunning(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	h.start(t)
	mustDo(t, "PauseScoring", h.e.PauseScoring())
	if err := h.e.AdjustScore(store.SideBlue, 10, "test"); err != nil {
		t.Fatalf("AdjustScore while paused: %v", err)
	}
	if got := h.status(t).BlueScore; got != 1010 {
		t.Fatalf("BlueScore = %d, want 1010", got)
	}
}

func TestBackupDatabase_RejectedWhileEngineIsRunning(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	h.start(t)
	if _, err := h.e.BackupDatabase(); !errors.Is(err, ErrEngineRunning) {
		t.Fatalf("BackupDatabase while running: got %v, want ErrEngineRunning", err)
	}
}

func TestBackupDatabase_SucceedsWhileStopped(t *testing.T) {
	h := newHarness(t, okCheck, "svc1") // engine starts stopped
	path, err := h.e.BackupDatabase()
	if err != nil {
		t.Fatalf("BackupDatabase: %v", err)
	}
	if path == "" {
		t.Fatal("expected a non-empty backup path")
	}
}

func TestResetRound_RejectedWhileEngineIsRunning(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	h.start(t)
	if _, err := h.e.ResetRound(true); !errors.Is(err, ErrEngineRunning) {
		t.Fatalf("ResetRound while running: got %v, want ErrEngineRunning", err)
	}
}

func TestResetRound_BacksUpFirstByDefaultAndThenResets(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	h.start(t)
	waitFor(t, "a check to be recorded", func() bool { return h.countResults(t, "svc1") > 0 })
	mustDo(t, "Stop", h.e.Stop())

	path, err := h.e.ResetRound(true)
	if err != nil {
		t.Fatalf("ResetRound: %v", err)
	}
	if path == "" {
		t.Fatal("expected a non-empty backup path when backup=true")
	}
	if got := h.status(t).BlueScore; got != 1000 {
		t.Fatalf("BlueScore after reset = %d, want 1000 (starting score)", got)
	}
	if got := h.countResults(t, "svc1"); got != 0 {
		t.Fatalf("check results after reset = %d, want 0", got)
	}
}

// A reset starts the round numbering over: the next round recorded is 1.
func TestResetRound_RestartsRoundNumbering(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	h.start(t)
	waitFor(t, "two rounds", func() bool { return h.status(t).Round >= 2 })
	mustDo(t, "Stop", h.e.Stop())

	if _, err := h.e.ResetRound(false); err != nil {
		t.Fatalf("ResetRound: %v", err)
	}
	if got := h.status(t).Round; got != 0 {
		t.Fatalf("Round after reset = %d, want 0", got)
	}
	h.start(t)
	waitFor(t, "a round after the reset", func() bool { return h.status(t).Round >= 1 })
	if got := h.status(t).Round; got != 1 {
		t.Errorf("first round after reset = %d, want 1", got)
	}
}

func TestResetRound_WithoutBackupSkipsIt(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	path, err := h.e.ResetRound(false)
	if err != nil {
		t.Fatalf("ResetRound: %v", err)
	}
	if path != "" {
		t.Fatalf("expected an empty backup path when backup=false, got %q", path)
	}
}

// stuckSink is a ResultSink whose first RecordRound signals inRecord and then
// blocks until release is closed, before passing the round on.
type stuckSink struct {
	inner    scheduler.ResultSink
	once     sync.Once
	inRecord chan struct{}
	release  chan struct{}
}

func (s *stuckSink) RecordRound(round int, roundStart time.Time, results []checks.RoundResult) error {
	s.once.Do(func() {
		close(s.inRecord)
		<-s.release
	})
	return s.inner.RecordRound(round, roundStart, results)
}

// TestScoringPausesNotBlockedByStuckControlOp: pausing scoring is the admin's
// emergency lever, so it must work even while another control operation is
// stuck — here a StopService waiting for a round that is stuck being
// recorded.
func TestScoringPausesNotBlockedByStuckControlOp(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	inRecord := make(chan struct{})
	h := newHarnessWithSink(t, okCheck, func(inner scheduler.ResultSink) scheduler.ResultSink {
		return &stuckSink{inner: inner, inRecord: inRecord, release: release}
	}, "svc1")
	// Runs before the harness's cleanups, which would otherwise hang.
	t.Cleanup(unblock)

	h.start(t)
	select {
	case <-inRecord:
	case <-time.After(waitTimeout):
		t.Fatal("no round was ever recorded")
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
		{"SetCountdownTarget", func() error { return h.e.SetCountdownTarget(nil) }},
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
		t.Fatal("StopService finished before the round was released; the test proved nothing")
	default:
	}

	unblock()
	select {
	case err := <-stopDone:
		mustDo(t, "StopService", err)
	case <-time.After(waitTimeout):
		t.Fatal("StopService did not finish after the round was released")
	}
}

func TestFailuresScoreOnlyAfterResumeScoring(t *testing.T) {
	h := newHarness(t, failCheck, "svc1")
	h.start(t)

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
	h.start(t)

	h.writeConfig(t, configYAML("svc2", "svc3"))
	added, removed, err := reloadParts(h.e.Reload(h.configPath))
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

	added, _, err := reloadParts(h.e.Reload(h.configPath))
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

	h.start(t)
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
	_, removed, err := reloadParts(h.e.Reload(h.configPath))
	mustDo(t, "first Reload", err)
	if !slices.Equal(removed, []string{"svc1"}) {
		t.Fatalf("removed = %v, want [svc1]", removed)
	}

	h.writeConfig(t, configYAML("svc1"))
	added, _, err := reloadParts(h.e.Reload(h.configPath))
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
	h.start(t)
	h.writeConfig(t, "services: [not valid")

	if _, _, err := reloadParts(h.e.Reload(h.configPath)); err == nil {
		t.Fatal("Reload of an invalid config succeeded")
	}
	if svc := h.service(t, "svc1"); !svc.Active || !h.running(t, "svc1") {
		t.Error("a failed reload deactivated or stopped svc1")
	}
}

func TestReload_UpdatesLoadedConfigHash(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	before := h.e.LoadedConfigHash()
	h.writeConfig(t, configYAML("svc1", "svc2"))
	if _, err := h.e.Reload(h.configPath); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	after := h.e.LoadedConfigHash()
	if after == "" || after == before {
		t.Fatalf("LoadedConfigHash did not change after a successful Reload with a changed file (before=%q, after=%q)", before, after)
	}
}

// The app's startup sync and shutdown stop are automatic, not an admin's
// doing, so their events must say so.
func TestReloadAs_SystemAttributesEveryEvent(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	h.writeConfig(t, configYAML("svc2"))

	added, removed, err := reloadParts(h.e.ReloadAs(ActorSystem, ""))
	mustDo(t, "ReloadAs", err)
	if !slices.Equal(added, []string{"svc2"}) || !slices.Equal(removed, []string{"svc1"}) {
		t.Fatalf("added=%v removed=%v, want [svc2] and [svc1]", added, removed)
	}
	for _, typ := range []string{EventServiceAdded, EventServiceRemoved, EventConfigReloaded} {
		if ev := h.lastEvent(t, typ); ev.Actor != "system" {
			t.Errorf("%s actor = %q, want system", typ, ev.Actor)
		}
	}
}

// An automatic reload that changes nothing (every ordinary boot) is not worth
// an event; an admin's reload is an admin action and is always logged.
func TestReload_ConfigReloadedEventOnlyWhenWorthLogging(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")

	_, _, err := reloadParts(h.e.ReloadAs(ActorSystem, ""))
	mustDo(t, "ReloadAs(system)", err)
	for _, ev := range h.events(t) {
		if ev.Type == EventConfigReloaded {
			t.Fatalf("system reload with no changes logged %+v", ev)
		}
	}

	_, _, err = reloadParts(h.e.Reload(""))
	mustDo(t, "Reload", err)
	if ev := h.lastEvent(t, EventConfigReloaded); ev.Actor != "admin" {
		t.Errorf("config_reloaded actor = %q, want admin", ev.Actor)
	}
}

func TestStopAs_SystemAttributesEngineStopped(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	h.start(t)

	mustDo(t, "StopAs", h.e.StopAs(ActorSystem))
	if h.running(t, "svc1") {
		t.Error("svc1 still running after StopAs")
	}
	if ev := h.lastEvent(t, EventEngineStopped); ev.Actor != "system" {
		t.Errorf("engine_stopped actor = %q, want system", ev.Actor)
	}
}

func TestReload_NoChangesReturnsEmptySlices(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	added, removed, err := reloadParts(h.e.Reload(h.configPath))
	mustDo(t, "Reload", err)
	if added == nil || removed == nil || len(added) != 0 || len(removed) != 0 {
		t.Errorf("added=%#v removed=%#v, want empty non-nil slices", added, removed)
	}
}

// Reload applies an edited starting score at once, and restarts a running
// service whose entry changed so the edit is used from its next round.
func TestReload_AppliesStartingScoreAndChangedEntries(t *testing.T) {
	h := newHarness(t, okCheck, "svc1", "svc2")
	h.start(t)
	cfg := strings.Replace(configWithServices(svcYAML("svc1", 10)+svcYAML("svc2", 250)),
		"starting_score: 1000", "starting_score: 1000000", 1)
	h.writeConfig(t, cfg)

	res, err := h.e.Reload(h.configPath)
	mustDo(t, "Reload", err)
	if !slices.Equal(res.Updated, []string{"svc2"}) {
		t.Errorf("Updated = %v, want [svc2]", res.Updated)
	}
	if got, _ := h.e.scheduler.ServiceConfig(h.service(t, "svc2").ID); got.PointsPerFailure != 250 {
		t.Errorf("svc2 runs with points_per_failure %d, want 250", got.PointsPerFailure)
	}
	if s := h.status(t); s.StartingScore != 1000000 || s.BlueScore != 1000000 {
		t.Errorf("StartingScore = %d, BlueScore = %d; want both 1000000", s.StartingScore, s.BlueScore)
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
		"engine_state", "scoring_paused", "server_time", "round", "next_round_at", "services"} {
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
	// No admin has set a countdown, so there is no target to report.
	if _, ok := got["countdown_target"]; ok {
		t.Errorf("countdown_target present before one was set: %s", raw)
	}
}

// Status reports the scheduler's round status: no round recorded before the
// first one, then what the round clock last recorded.
func TestStatus_IncludesRoundAndNextRoundAt(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	if s := h.status(t); s.Round != 0 {
		t.Errorf("before any round: Round = %d, want 0", s.Round)
	}

	h.start(t)
	waitFor(t, "a round to be recorded", func() bool { return h.status(t).Round >= 1 })
	// Stopping the engine waits out any round being recorded and leaves no
	// service enabled, so the round number holds still from here on (idle
	// ticks still move NextRoundAt forward).
	mustDo(t, "Stop", h.e.Stop())
	wantRound, wantNext := h.e.scheduler.RoundStatus()

	s := h.status(t)
	if s.Round != wantRound || s.NextRoundAt.Before(wantNext) {
		t.Errorf("Round = %d, NextRoundAt = %v; want %d, not before %v", s.Round, s.NextRoundAt, wantRound, wantNext)
	}
	if wantNext.IsZero() {
		t.Error("scheduler reports a zero NextRoundAt after a recorded round")
	}
}

func TestSetCountdownTarget_PersistsAndAppearsInStatus(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	if s := h.status(t); s.CountdownTarget != nil {
		t.Fatalf("CountdownTarget = %v before any was set, want nil", s.CountdownTarget)
	}

	target := time.Date(2026, 10, 17, 17, 0, 0, 0, time.UTC)
	mustDo(t, "SetCountdownTarget", h.e.SetCountdownTarget(&target))
	if s := h.status(t); s.CountdownTarget == nil || !s.CountdownTarget.Equal(target) {
		t.Fatalf("CountdownTarget = %v, want %v", s.CountdownTarget, target)
	}
	rs, err := h.st.GetRunState()
	mustDo(t, "GetRunState", err)
	if rs.CountdownTarget == nil || !rs.CountdownTarget.Equal(target) {
		t.Errorf("persisted CountdownTarget = %v, want %v", rs.CountdownTarget, target)
	}
}

func TestSetCountdownTarget_NilClearsIt(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	target := time.Date(2026, 10, 17, 17, 0, 0, 0, time.UTC)
	mustDo(t, "SetCountdownTarget", h.e.SetCountdownTarget(&target))
	mustDo(t, "SetCountdownTarget(nil)", h.e.SetCountdownTarget(nil))
	if s := h.status(t); s.CountdownTarget != nil {
		t.Errorf("CountdownTarget = %v after clearing, want nil", s.CountdownTarget)
	}
}

func TestSetCountdownTarget_LogsAnEvent(t *testing.T) {
	h := newHarness(t, okCheck, "svc1")
	// 12:00 in UTC-5, so the message must normalize it to 17:00Z.
	target := time.Date(2026, 10, 17, 12, 0, 0, 0, time.FixedZone("UTC-5", -5*60*60))
	mustDo(t, "SetCountdownTarget", h.e.SetCountdownTarget(&target))
	ev := h.lastEvent(t, EventCountdownChanged)
	if ev.Actor != "admin" || ev.ServiceID != nil || !strings.Contains(ev.Message, "2026-10-17T17:00:00Z") {
		t.Errorf("event = %+v, want an admin engine event naming 2026-10-17T17:00:00Z", ev)
	}

	mustDo(t, "SetCountdownTarget(nil)", h.e.SetCountdownTarget(nil))
	if ev := h.lastEvent(t, EventCountdownChanged); !strings.Contains(ev.Message, "cleared") {
		t.Errorf("event after clearing = %+v, want a message saying it was cleared", ev)
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
		func() error { _, err := h.e.Start(); return err },
		h.e.Stop,
		func() error { return h.e.StartService("svc1") },
		func() error { return h.e.StopService("svc1") },
		func() error { return h.e.RestartService("svc2", false) },
		func() error { _, _, err := reloadParts(h.e.Reload(h.configPath)); return err },
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
