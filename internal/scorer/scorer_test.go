package scorer

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/scheduler"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

// The Scheduler reports into a Scorer; this keeps the two signatures in step.
var _ scheduler.ResultSink = (*Scorer)(nil)

var (
	fail = checks.Result{Success: false, Detail: "connection refused"}
	pass = checks.Result{Success: true, Detail: "ok"}
)

func openTestStore(t *testing.T, startingScore int) (*store.Store, store.Session) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "scoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	// Open forces scoring paused (the crash-safe default), and in production
	// it stays paused until an admin resumes it explicitly. Resume it here, as
	// that admin would, so each test begins with scoring live.
	if err := st.SetGlobalScoringPaused(false); err != nil {
		t.Fatalf("SetGlobalScoringPaused(false): %v", err)
	}
	sess, err := st.GetOrCreateSession("comp-1", startingScore)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	return st, sess
}

func newService(t *testing.T, st *store.Store, sessionID int64, name string) store.Service {
	t.Helper()
	svc, err := st.GetOrCreateService(sessionID, name)
	if err != nil {
		t.Fatalf("GetOrCreateService(%q): %v", name, err)
	}
	return svc
}

func record(t *testing.T, sc *Scorer, serviceID int64, points int, result checks.Result) {
	t.Helper()
	if err := sc.Record(serviceID, points, result, 5*time.Millisecond); err != nil {
		t.Fatalf("Record(service %d, success=%v): %v", serviceID, result.Success, err)
	}
}

func wantScore(t *testing.T, st *store.Store, sessionID int64, wantBlue, wantRed int) {
	t.Helper()
	blue, red, err := st.GetScore(sessionID)
	if err != nil {
		t.Fatalf("GetScore: %v", err)
	}
	if blue != wantBlue || red != wantRed {
		t.Errorf("score blue=%d red=%d, want %d/%d", blue, red, wantBlue, wantRed)
	}
}

// eventTypes returns the session's event types, oldest first.
func eventTypes(t *testing.T, st *store.Store, sessionID int64) []string {
	t.Helper()
	events, err := st.ListEvents(sessionID, 1000, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	types := make([]string, len(events))
	for i, e := range events {
		types[len(events)-1-i] = e.Type
	}
	return types
}

func countEvents(t *testing.T, st *store.Store, sessionID int64, eventType string) int {
	t.Helper()
	n := 0
	for _, typ := range eventTypes(t, st, sessionID) {
		if typ == eventType {
			n++
		}
	}
	return n
}

func scoringPaused(t *testing.T, st *store.Store) bool {
	t.Helper()
	rs, err := st.GetRunState()
	if err != nil {
		t.Fatalf("GetRunState: %v", err)
	}
	return rs.ScoringPaused
}

func latestResult(t *testing.T, st *store.Store, serviceID int64) store.CheckResult {
	t.Helper()
	cr, err := st.GetLatestCheckResult(serviceID)
	if err != nil {
		t.Fatalf("GetLatestCheckResult: %v", err)
	}
	if cr == nil {
		t.Fatal("expected a recorded check result, got none")
	}
	return *cr
}

func fraction(v float64) *float64 { return &v }

func TestRecord_FailureTransfersPointsAndRecordsResult(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svc := newService(t, st, sess.ID, "apache-web")
	sc := New(st, sess.ID, nil)

	if err := sc.Record(svc.ID, 10, fail, 42*time.Millisecond); err != nil {
		t.Fatalf("Record: %v", err)
	}

	wantScore(t, st, sess.ID, 990, 10)

	cr := latestResult(t, st, svc.ID)
	if cr.Success || cr.LatencyMS != 42 || cr.Detail != "connection refused" || cr.SessionID != sess.ID {
		t.Errorf("recorded result = %+v, want failure, 42ms, detail kept, session %d", cr, sess.ID)
	}

	// The entry must cite the check result as its source: recording the same
	// source again is rejected as a duplicate.
	_, err := st.RecordLedgerEntry(sess.ID, "service_check", cr.ID, store.SideBlue, store.SideRed, 10, "replay")
	if !errors.Is(err, store.ErrDuplicateSourceRef) {
		t.Errorf("replaying service_check %d: err = %v, want ErrDuplicateSourceRef", cr.ID, err)
	}
}

func TestRecord_PassMovesNoPoints(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svc := newService(t, st, sess.ID, "apache-web")
	sc := New(st, sess.ID, nil)

	record(t, sc, svc.ID, 10, pass)
	record(t, sc, svc.ID, 10, pass)

	wantScore(t, st, sess.ID, 1000, 0)
	if cr := latestResult(t, st, svc.ID); !cr.Success {
		t.Errorf("recorded result success = false, want true")
	}
}

func TestRecord_EmitsEventsOnlyOnTransitions(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svc := newService(t, st, sess.ID, "apache-web")
	sc := New(st, sess.ID, nil)

	for _, r := range []checks.Result{fail, fail, pass, pass, fail} {
		record(t, sc, svc.ID, 10, r)
	}

	got := fmt.Sprint(eventTypes(t, st, sess.ID))
	want := fmt.Sprint([]string{"service_down", "service_up", "service_down"})
	if got != want {
		t.Errorf("events = %s, want %s", got, want)
	}

	events, err := st.ListEvents(sess.ID, 1, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	e := events[0]
	if e.ServiceID == nil || *e.ServiceID != svc.ID || e.Actor != "system" {
		t.Errorf("transition event = %+v, want service %d, actor system", e, svc.ID)
	}
}

func TestRecord_FirstResultIsATransition(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	up := newService(t, st, sess.ID, "svc-up")
	down := newService(t, st, sess.ID, "svc-down")
	sc := New(st, sess.ID, nil)

	record(t, sc, up.ID, 10, pass)
	record(t, sc, down.ID, 10, fail)

	got := fmt.Sprint(eventTypes(t, st, sess.ID))
	want := fmt.Sprint([]string{"service_up", "service_down"})
	if got != want {
		t.Errorf("events = %s, want %s", got, want)
	}
}

func TestRecord_GlobalPauseRecordsButDoesNotScore(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svc := newService(t, st, sess.ID, "apache-web")
	if err := st.SetGlobalScoringPaused(true); err != nil {
		t.Fatalf("SetGlobalScoringPaused: %v", err)
	}
	sc := New(st, sess.ID, nil)

	record(t, sc, svc.ID, 10, fail)

	wantScore(t, st, sess.ID, 1000, 0)
	results, err := st.ListCheckResults(svc.ID, 10, 0)
	if err != nil {
		t.Fatalf("ListCheckResults: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("recorded %d results while paused, want 1", len(results))
	}
	if n := countEvents(t, st, sess.ID, "service_down"); n != 1 {
		t.Errorf("service_down events while paused = %d, want 1", n)
	}
}

func TestRecord_PerServicePauseRecordsButDoesNotScore(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	paused := newService(t, st, sess.ID, "paused-svc")
	scored := newService(t, st, sess.ID, "scored-svc")
	if err := st.SetServiceScoringPaused(paused.ID, true); err != nil {
		t.Fatalf("SetServiceScoringPaused: %v", err)
	}
	sc := New(st, sess.ID, nil)

	record(t, sc, paused.ID, 10, fail)
	wantScore(t, st, sess.ID, 1000, 0)
	if n := countEvents(t, st, sess.ID, "service_down"); n != 1 {
		t.Errorf("service_down events = %d, want 1 (paused services still report transitions)", n)
	}

	// Pausing one service leaves the others scored.
	record(t, sc, scored.ID, 10, fail)
	wantScore(t, st, sess.ID, 990, 10)
}

// Both pause flags can change while the engine runs; the same Scorer must
// honor every change on its very next result.
func TestRecord_PauseFlagsAreReadFreshEachCall(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svc := newService(t, st, sess.ID, "apache-web")
	sc := New(st, sess.ID, nil)

	steps := []struct {
		name          string
		globalPaused  bool
		servicePaused bool
		wantBlue      int
	}{
		{"unpaused", false, false, 990},
		{"service paused", false, true, 990},
		{"service resumed", false, false, 980},
		{"globally paused", true, false, 980},
		{"globally resumed", false, false, 970},
	}
	for _, step := range steps {
		if err := st.SetGlobalScoringPaused(step.globalPaused); err != nil {
			t.Fatalf("%s: SetGlobalScoringPaused: %v", step.name, err)
		}
		if err := st.SetServiceScoringPaused(svc.ID, step.servicePaused); err != nil {
			t.Fatalf("%s: SetServiceScoringPaused: %v", step.name, err)
		}
		record(t, sc, svc.ID, 10, fail)
		blue, _, err := st.GetScore(sess.ID)
		if err != nil {
			t.Fatalf("GetScore: %v", err)
		}
		if blue != step.wantBlue {
			t.Errorf("after %s failure: blue = %d, want %d", step.name, blue, step.wantBlue)
		}
	}
}

func TestRecord_DuplicateSourceRefIsANoOp(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svc := newService(t, st, sess.ID, "apache-web")
	sc := New(st, sess.ID, nil)

	// check_results ids are AUTOINCREMENT, so in a fresh database the first
	// result is id 1. Score that source before the Scorer gets to it.
	if _, err := st.RecordLedgerEntry(sess.ID, "service_check", 1, store.SideBlue, store.SideRed, 10, "pre-scored"); err != nil {
		t.Fatalf("RecordLedgerEntry: %v", err)
	}

	record(t, sc, svc.ID, 10, fail)

	if id := latestResult(t, st, svc.ID).ID; id != 1 {
		t.Fatalf("first check result id = %d, test assumes 1", id)
	}
	wantScore(t, st, sess.ID, 990, 10) // the pre-scored transfer only, not a second one
}

func TestRecord_UnknownServiceIsAnError(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	logger, _ := quietLogger()
	sc := New(st, sess.ID, nil, WithLogger(logger))

	if err := sc.Record(9999, 10, fail, time.Millisecond); err == nil {
		t.Fatal("Record for an unknown service returned nil, want an error")
	}
	wantScore(t, st, sess.ID, 1000, 0)
}

// quietLogger returns a logger that writes into a buffer, so a test can both
// silence and inspect what the Scorer logs.
func quietLogger() (*log.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return log.New(&buf, "", 0), &buf
}

// The Scheduler discards Record's error, so the Scorer itself must leave a
// trace of every failed write: in the log always, and in the event log when
// the store can still take the event.
func TestRecord_StoreFailureIsReturnedAndLogged(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svc := newService(t, st, sess.ID, "apache-web")
	logger, logs := quietLogger()
	sc := New(st, sess.ID, nil, WithLogger(logger))
	st.Close()

	err := sc.Record(svc.ID, 10, fail, time.Millisecond)
	if err == nil {
		t.Fatal("Record on a closed store returned nil, want an error")
	}
	out := logs.String()
	if !strings.Contains(out, fmt.Sprintf("service %d", svc.ID)) || !strings.Contains(out, err.Error()) {
		t.Errorf("log = %q, want the service id and the error %q", out, err)
	}
	// The persistence_error event could not be written either; that failure
	// is logged too, rather than retried or recursed into.
	if !strings.Contains(out, EventPersistenceError) {
		t.Errorf("log = %q, want a note that the %s event failed too", out, EventPersistenceError)
	}
}

func TestRecord_FailureWritesPersistenceErrorEvent(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	logger, logs := quietLogger()
	sc := New(st, sess.ID, nil, WithLogger(logger))

	// A service id with no row: loading it fails, while the store itself is
	// still writable, so the error event can be recorded.
	const missing int64 = 9999
	err := sc.Record(missing, 10, fail, time.Millisecond)
	if err == nil {
		t.Fatal("Record for an unknown service returned nil, want an error")
	}
	if !strings.Contains(logs.String(), err.Error()) {
		t.Errorf("log = %q, want the error %q", logs.String(), err)
	}

	events, lerr := st.ListEvents(sess.ID, 10, 0)
	if lerr != nil {
		t.Fatalf("ListEvents: %v", lerr)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want exactly one persistence_error", events)
	}
	e := events[0]
	if e.Type != EventPersistenceError || e.Level != "error" || e.Actor != "system" ||
		e.ServiceID == nil || *e.ServiceID != missing || e.Detail != err.Error() {
		t.Errorf("event = %+v, want error-level system persistence_error for service %d with detail %q",
			e, missing, err)
	}
	// The message is public; the cause (which may name internals) stays in
	// the auth-gated detail.
	if strings.Contains(e.Message, err.Error()) {
		t.Errorf("event message %q leaks the error text", e.Message)
	}
}

func TestRecord_MassFailureAutopause(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svcA := newService(t, st, sess.ID, "svc-a")
	svcB := newService(t, st, sess.ID, "svc-b")
	sc := New(st, sess.ID, fraction(0.5))

	record(t, sc, svcB.ID, 10, pass)
	if scoringPaused(t, st) {
		t.Fatal("paused after a pass: 0 of 2 failing")
	}

	// 1 of 2 active services failing meets the 0.5 threshold. The failure
	// that crosses the threshold is itself still scored.
	record(t, sc, svcA.ID, 10, fail)
	if !scoringPaused(t, st) {
		t.Fatal("not paused with 1 of 2 services failing at threshold 0.5")
	}
	wantScore(t, st, sess.ID, 990, 10)
	if n := countEvents(t, st, sess.ID, "mass_failure_autopause"); n != 1 {
		t.Fatalf("mass_failure_autopause events = %d, want 1", n)
	}
	events, err := st.ListEvents(sess.ID, 1, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if e := events[0]; e.Type != "mass_failure_autopause" || e.Level != "warning" || e.Actor != "system" || e.ServiceID != nil {
		t.Errorf("autopause event = %+v, want warning from system with no service", e)
	}

	// Further failures while paused neither score nor repeat the event.
	record(t, sc, svcA.ID, 10, fail)
	record(t, sc, svcB.ID, 10, fail)
	wantScore(t, st, sess.ID, 990, 10)
	if n := countEvents(t, st, sess.ID, "mass_failure_autopause"); n != 1 {
		t.Errorf("mass_failure_autopause events = %d after more failures, want still 1", n)
	}
}

// Services that have not reported yet are unknown, not down: one early
// failure among many silent services must not pause the competition.
func TestRecord_MassFailureCountsUnreportedServicesAsNotFailing(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	var services []store.Service
	for i := range 10 {
		services = append(services, newService(t, st, sess.ID, fmt.Sprintf("svc-%d", i)))
	}
	sc := New(st, sess.ID, fraction(0.5))

	record(t, sc, services[0].ID, 10, fail)
	if scoringPaused(t, st) {
		t.Fatal("paused with 1 of 10 active services failing at threshold 0.5")
	}
	wantScore(t, st, sess.ID, 990, 10)

	for _, svc := range services[1:4] {
		record(t, sc, svc.ID, 10, fail)
	}
	if scoringPaused(t, st) {
		t.Fatal("paused with 4 of 10 failing at threshold 0.5")
	}
	record(t, sc, services[4].ID, 10, fail)
	if !scoringPaused(t, st) {
		t.Fatal("not paused with 5 of 10 failing at threshold 0.5")
	}
}

func TestRecord_MassFailureIgnoresInactiveServices(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svcA := newService(t, st, sess.ID, "svc-a")
	for i := range 3 {
		retired := newService(t, st, sess.ID, fmt.Sprintf("retired-%d", i))
		if err := st.SetServiceActive(retired.ID, false); err != nil {
			t.Fatalf("SetServiceActive: %v", err)
		}
	}
	sc := New(st, sess.ID, fraction(0.5))

	record(t, sc, svcA.ID, 10, pass) // arms the safety net: 0 of 1 failing
	record(t, sc, svcA.ID, 10, fail)
	if !scoringPaused(t, st) {
		t.Fatal("not paused with the only active service failing")
	}
}

// After an autopause, an admin who decides the failures are genuine red-team
// action resumes scoring. The safety net is edge-triggered, so it must not
// override that call while the same mass failure is still going on.
func TestRecord_MassFailureResumeIsNotOverridden(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svcA := newService(t, st, sess.ID, "svc-a")
	svcB := newService(t, st, sess.ID, "svc-b")
	sc := New(st, sess.ID, fraction(0.5))

	record(t, sc, svcB.ID, 10, pass)
	record(t, sc, svcA.ID, 10, fail) // 1 of 2: fires
	if !scoringPaused(t, st) {
		t.Fatal("not paused at 1 of 2 failing")
	}
	wantScore(t, st, sess.ID, 990, 10)

	if err := st.SetGlobalScoringPaused(false); err != nil { // the admin's resume
		t.Fatalf("SetGlobalScoringPaused(false): %v", err)
	}
	record(t, sc, svcA.ID, 10, fail)
	record(t, sc, svcB.ID, 10, fail) // 2 of 2, still above the threshold
	if scoringPaused(t, st) {
		t.Fatal("re-paused after the admin resumed during the same mass failure")
	}
	wantScore(t, st, sess.ID, 970, 30)
	if n := countEvents(t, st, sess.ID, EventMassFailureAutopause); n != 1 {
		t.Fatalf("mass_failure_autopause events = %d, want 1", n)
	}

	// Once the share drops below the threshold the net re-arms, and the next
	// crossing fires again, with its own single event.
	record(t, sc, svcA.ID, 10, pass)
	record(t, sc, svcB.ID, 10, pass)
	record(t, sc, svcA.ID, 10, fail)
	if !scoringPaused(t, st) {
		t.Fatal("not paused on a second crossing after recovering below the threshold")
	}
	record(t, sc, svcB.ID, 10, fail)
	if n := countEvents(t, st, sess.ID, EventMassFailureAutopause); n != 2 {
		t.Errorf("mass_failure_autopause events = %d, want 2 (one per crossing)", n)
	}
}

// A Scorer that first sees the session already above the threshold — a
// restart in the middle of a sweep the admin may already have judged genuine —
// has seen no crossing, so it must not fire until it has seen the share below
// the threshold.
func TestRecord_MassFailureNeedsABelowThresholdEvaluationToArm(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svcA := newService(t, st, sess.ID, "svc-a")
	svcB := newService(t, st, sess.ID, "svc-b")
	// Both were failing before the restart.
	for _, id := range []int64{svcA.ID, svcB.ID} {
		if _, err := st.InsertCheckResult(store.CheckResult{SessionID: sess.ID, ServiceID: id, Success: false}); err != nil {
			t.Fatalf("InsertCheckResult: %v", err)
		}
	}
	sc := New(st, sess.ID, fraction(0.5))

	record(t, sc, svcA.ID, 10, fail)
	record(t, sc, svcA.ID, 10, pass) // 1 of 2: still at the threshold
	record(t, sc, svcA.ID, 10, fail)
	if scoringPaused(t, st) {
		t.Fatal("paused without ever having seen the share below the threshold")
	}
	wantScore(t, st, sess.ID, 980, 20)

	record(t, sc, svcA.ID, 10, pass)
	record(t, sc, svcB.ID, 10, pass) // 0 of 2: arms
	record(t, sc, svcB.ID, 10, fail) // 1 of 2: fires
	if !scoringPaused(t, st) {
		t.Fatal("not paused on the first crossing after arming")
	}
	if n := countEvents(t, st, sess.ID, EventMassFailureAutopause); n != 1 {
		t.Errorf("mass_failure_autopause events = %d, want 1", n)
	}
}

// Only services whose routine is running count: a stopped routine's last
// result is stale (it may have been stopped precisely because it was failing),
// so it is left out of both the failing count and the total.
func TestRecord_MassFailureCountsOnlyRunningServices(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	var services []store.Service
	for i := range 10 {
		services = append(services, newService(t, st, sess.ID, fmt.Sprintf("svc-%d", i)))
	}
	// svc-8 and svc-9 were failing when their routines were stopped.
	stopped := map[int64]bool{services[8].ID: true, services[9].ID: true}
	for id := range stopped {
		if _, err := st.InsertCheckResult(store.CheckResult{SessionID: sess.ID, ServiceID: id, Success: false}); err != nil {
			t.Fatalf("InsertCheckResult: %v", err)
		}
	}
	running := func(id int64) bool { return !stopped[id] }
	sc := New(st, sess.ID, fraction(0.5), WithRunning(running))

	record(t, sc, services[0].ID, 10, pass) // 0 of 8 running: arms
	for _, svc := range services[1:4] {
		record(t, sc, svc.ID, 10, fail)
	}
	// 3 of 8 running services failing. Counting the stopped two would make
	// it 5 of 10 and pause.
	if scoringPaused(t, st) {
		t.Fatal("paused at 3 of 8 running services failing; stopped routines were counted")
	}
	record(t, sc, services[4].ID, 10, fail) // 4 of 8
	if !scoringPaused(t, st) {
		t.Fatal("not paused at 4 of 8 running services failing")
	}
}

func TestRecord_MassFailureDisabledWithoutFraction(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svcA := newService(t, st, sess.ID, "svc-a")
	svcB := newService(t, st, sess.ID, "svc-b")
	sc := New(st, sess.ID, nil)

	record(t, sc, svcA.ID, 10, fail)
	record(t, sc, svcB.ID, 10, fail)

	if scoringPaused(t, st) {
		t.Error("paused although the mass-failure safety net is disabled")
	}
	wantScore(t, st, sess.ID, 980, 20)
	if n := countEvents(t, st, sess.ID, "mass_failure_autopause"); n != 0 {
		t.Errorf("mass_failure_autopause events = %d, want 0", n)
	}
}

func TestRecord_MassFailureDoesNotFireWhenAlreadyPaused(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svc := newService(t, st, sess.ID, "svc-a")
	if err := st.SetGlobalScoringPaused(true); err != nil {
		t.Fatalf("SetGlobalScoringPaused: %v", err)
	}
	sc := New(st, sess.ID, fraction(0.5))

	record(t, sc, svc.ID, 10, fail)

	if n := countEvents(t, st, sess.ID, "mass_failure_autopause"); n != 0 {
		t.Errorf("mass_failure_autopause events = %d while an admin pause was set, want 0", n)
	}
}

// Many services crossing the threshold at once must still produce exactly
// one autopause event.
func TestRecord_MassFailureFiresOnceUnderConcurrency(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	var services []store.Service
	for i := range 8 {
		services = append(services, newService(t, st, sess.ID, fmt.Sprintf("svc-%d", i)))
	}
	sc := New(st, sess.ID, fraction(0.25))
	// Every service healthy first, so the safety net is armed before the
	// failures race each other.
	for _, svc := range services {
		record(t, sc, svc.ID, 10, pass)
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(services))
	for _, svc := range services {
		wg.Go(func() {
			errs <- sc.Record(svc.ID, 10, fail, time.Millisecond)
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Record: %v", err)
		}
	}

	if !scoringPaused(t, st) {
		t.Error("not paused with every service failing")
	}
	if n := countEvents(t, st, sess.ID, "mass_failure_autopause"); n != 1 {
		t.Errorf("mass_failure_autopause events = %d, want exactly 1", n)
	}
}

// Two services failing in the same instant against a balance that covers only
// one full transfer: the floor holds, so exactly the balance moves and blue
// never goes negative.
func TestRecord_ConcurrentFailuresRespectFloor(t *testing.T) {
	st, sess := openTestStore(t, 15)
	svcA := newService(t, st, sess.ID, "svc-a")
	svcB := newService(t, st, sess.ID, "svc-b")
	sc := New(st, sess.ID, nil)

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []int64{svcA.ID, svcB.ID} {
		wg.Go(func() {
			<-start
			errs <- sc.Record(id, 10, fail, time.Millisecond)
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Record: %v", err)
		}
	}

	wantScore(t, st, sess.ID, 0, 15)
}
