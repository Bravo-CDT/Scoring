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

// recordAlone records result as the only result of a round of its own,
// numbered as the Scheduler would number it, and returns RecordRound's error.
func recordAlone(t *testing.T, sc *Scorer, serviceID int64, points int, result checks.Result, latency time.Duration) error {
	t.Helper()
	round, err := sc.store.NextRoundNumber(sc.sessionID)
	if err != nil {
		t.Fatalf("NextRoundNumber: %v", err)
	}
	return sc.RecordRound(round, time.Now(), []checks.RoundResult{
		{ServiceID: serviceID, PointsPerFailure: points, Result: result, Latency: latency},
	})
}

// record is recordAlone for a call that must succeed.
func record(t *testing.T, sc *Scorer, serviceID int64, points int, result checks.Result) {
	t.Helper()
	if err := recordAlone(t, sc, serviceID, points, result, 5*time.Millisecond); err != nil {
		t.Fatalf("RecordRound(service %d, success=%v): %v", serviceID, result.Success, err)
	}
}

// recordRound records results as one round and fails the test on error.
func recordRound(t *testing.T, sc *Scorer, round int, roundStart time.Time, results ...checks.RoundResult) {
	t.Helper()
	if err := sc.RecordRound(round, roundStart, results); err != nil {
		t.Fatalf("RecordRound(round %d): %v", round, err)
	}
}

// outcome builds a RoundResult worth 10 points per failure.
func outcome(serviceID int64, result checks.Result) checks.RoundResult {
	return checks.RoundResult{ServiceID: serviceID, PointsPerFailure: 10, Result: result, Latency: time.Millisecond}
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

func TestRecordRound_FailureTransfersPointsAndRecordsResult(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svc := newService(t, st, sess.ID, "apache-web")
	sc := New(st, sess.ID)

	if err := recordAlone(t, sc, svc.ID, 10, fail, 42*time.Millisecond); err != nil {
		t.Fatalf("RecordRound: %v", err)
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

func TestRecordRound_PassMovesNoPoints(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svc := newService(t, st, sess.ID, "apache-web")
	sc := New(st, sess.ID)

	record(t, sc, svc.ID, 10, pass)
	record(t, sc, svc.ID, 10, pass)

	wantScore(t, st, sess.ID, 1000, 0)
	if cr := latestResult(t, st, svc.ID); !cr.Success {
		t.Errorf("recorded result success = false, want true")
	}
}

func TestRecordRound_EmitsEventsOnlyOnTransitions(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svc := newService(t, st, sess.ID, "apache-web")
	sc := New(st, sess.ID)

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

func TestRecordRound_FirstResultIsATransition(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	up := newService(t, st, sess.ID, "svc-up")
	down := newService(t, st, sess.ID, "svc-down")
	sc := New(st, sess.ID)

	record(t, sc, up.ID, 10, pass)
	record(t, sc, down.ID, 10, fail)

	got := fmt.Sprint(eventTypes(t, st, sess.ID))
	want := fmt.Sprint([]string{"service_up", "service_down"})
	if got != want {
		t.Errorf("events = %s, want %s", got, want)
	}
}

func TestRecordRound_GlobalPauseRecordsButDoesNotScore(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svc := newService(t, st, sess.ID, "apache-web")
	if err := st.SetGlobalScoringPaused(true); err != nil {
		t.Fatalf("SetGlobalScoringPaused: %v", err)
	}
	sc := New(st, sess.ID)

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

func TestRecordRound_PerServicePauseRecordsButDoesNotScore(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	paused := newService(t, st, sess.ID, "paused-svc")
	scored := newService(t, st, sess.ID, "scored-svc")
	if err := st.SetServiceScoringPaused(paused.ID, true); err != nil {
		t.Fatalf("SetServiceScoringPaused: %v", err)
	}
	sc := New(st, sess.ID)

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
func TestRecordRound_PauseFlagsAreReadFreshEachCall(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svc := newService(t, st, sess.ID, "apache-web")
	sc := New(st, sess.ID)

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

func TestRecordRound_DuplicateSourceRefIsANoOp(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svc := newService(t, st, sess.ID, "apache-web")
	sc := New(st, sess.ID)

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

func TestRecordRound_UnknownServiceIsAnError(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	logger, _ := quietLogger()
	sc := New(st, sess.ID, WithLogger(logger))

	if err := recordAlone(t, sc, 9999, 10, fail, time.Millisecond); err == nil {
		t.Fatal("RecordRound for an unknown service returned nil, want an error")
	}
	wantScore(t, st, sess.ID, 1000, 0)
}

// quietLogger returns a logger that writes into a buffer, so a test can both
// silence and inspect what the Scorer logs.
func quietLogger() (*log.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return log.New(&buf, "", 0), &buf
}

// The Scheduler discards RecordRound's error, so the Scorer itself must leave a
// trace of every failed write: in the log always, and in the event log when
// the store can still take the event.
func TestRecordRound_StoreFailureIsReturnedAndLogged(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svc := newService(t, st, sess.ID, "apache-web")
	logger, logs := quietLogger()
	sc := New(st, sess.ID, WithLogger(logger))
	st.Close()

	err := sc.RecordRound(1, time.Now(), []checks.RoundResult{outcome(svc.ID, fail)})
	if err == nil {
		t.Fatal("RecordRound on a closed store returned nil, want an error")
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

func TestRecordRound_FailureWritesPersistenceErrorEvent(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	logger, logs := quietLogger()
	sc := New(st, sess.ID, WithLogger(logger))

	// A service id with no row: loading it fails, while the store itself is
	// still writable, so the error event can be recorded.
	const missing int64 = 9999
	err := recordAlone(t, sc, missing, 10, fail, time.Millisecond)
	if err == nil {
		t.Fatal("RecordRound for an unknown service returned nil, want an error")
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

// Two services failing in the same instant against a balance that covers only
// one full transfer: the floor holds, so exactly the balance moves and blue
// never goes negative.
func TestRecordRound_ConcurrentFailuresRespectFloor(t *testing.T) {
	st, sess := openTestStore(t, 15)
	svcA := newService(t, st, sess.ID, "svc-a")
	svcB := newService(t, st, sess.ID, "svc-b")
	sc := New(st, sess.ID)

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []int64{svcA.ID, svcB.ID} {
		wg.Go(func() {
			<-start
			errs <- sc.RecordRound(1, time.Now(), []checks.RoundResult{outcome(id, fail)})
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("RecordRound: %v", err)
		}
	}

	wantScore(t, st, sess.ID, 0, 15)
}

func TestRecordRound_TagsEveryResultWithTheRoundNumberAndSharesOneTimestamp(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svcA := newService(t, st, sess.ID, "svc-a")
	svcB := newService(t, st, sess.ID, "svc-b")
	sc := New(st, sess.ID)

	// Well in the past, so a row stamped with time.Now() at insert time could
	// not pass for it.
	roundStart := time.Now().Add(-time.Hour).Truncate(time.Millisecond).UTC()
	recordRound(t, sc, 7, roundStart, outcome(svcA.ID, pass), outcome(svcB.ID, fail))

	for _, id := range []int64{svcA.ID, svcB.ID} {
		cr := latestResult(t, st, id)
		if cr.Round != 7 {
			t.Errorf("service %d: Round = %d, want 7", id, cr.Round)
		}
		if !cr.Timestamp.Equal(roundStart) {
			t.Errorf("service %d: Timestamp = %v, want the round start %v", id, cr.Timestamp, roundStart)
		}
	}
}

// One result that cannot be recorded must not cost the rest of the round its
// results.
func TestRecordRound_OneBadResultDoesNotDropTheOthers(t *testing.T) {
	st, sess := openTestStore(t, 1000)
	svc := newService(t, st, sess.ID, "svc-a")
	logger, _ := quietLogger()
	sc := New(st, sess.ID, WithLogger(logger))

	const missing int64 = 9999
	err := sc.RecordRound(1, time.Now(), []checks.RoundResult{outcome(missing, fail), outcome(svc.ID, fail)})
	if err == nil {
		t.Fatal("RecordRound with an unknown service returned nil, want an error")
	}
	if cr := latestResult(t, st, svc.ID); cr.Round != 1 || cr.Success {
		t.Errorf("result after the bad one = %+v, want a round 1 failure", cr)
	}
	wantScore(t, st, sess.ID, 990, 10)
	if n := countEvents(t, st, sess.ID, EventPersistenceError); n != 1 {
		t.Errorf("persistence_error events = %d, want 1 (for the unknown service only)", n)
	}
}
