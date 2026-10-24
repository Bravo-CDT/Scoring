package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/config"
)

// testRound is the round interval of most tests: short so they run quickly,
// long enough that a loaded machine still fits a fast check inside it.
const testRound = 50 * time.Millisecond

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

// recordedRound is one RecordRound call.
type recordedRound struct {
	round   int
	start   time.Time
	results []checks.RoundResult
	at      time.Time // when the call arrived
}

// has reports whether the round holds a result for serviceID.
func (r recordedRound) has(serviceID int64) bool {
	return slices.ContainsFunc(r.results, func(res checks.RoundResult) bool { return res.ServiceID == serviceID })
}

// result returns the round's result for serviceID; ok is false if it has none.
func (r recordedRound) result(serviceID int64) (res checks.RoundResult, ok bool) {
	i := slices.IndexFunc(r.results, func(res checks.RoundResult) bool { return res.ServiceID == serviceID })
	if i < 0 {
		return checks.RoundResult{}, false
	}
	return r.results[i], true
}

// fakeSink records every RecordRound call. If err is set, RecordRound still
// records the call but returns err. If during is set, RecordRound calls it
// first, from the round loop's goroutine.
type fakeSink struct {
	mu     sync.Mutex
	rounds []recordedRound
	err    error
	during func()
}

func (f *fakeSink) RecordRound(round int, roundStart time.Time, results []checks.RoundResult) error {
	f.mu.Lock()
	during := f.during
	f.mu.Unlock()
	if during != nil {
		during()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rounds = append(f.rounds, recordedRound{round, roundStart, slices.Clone(results), time.Now()})
	return f.err
}

func (f *fakeSink) setDuring(fn func()) {
	f.mu.Lock()
	f.during = fn
	f.mu.Unlock()
}

func (f *fakeSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rounds)
}

func (f *fakeSink) snapshot() []recordedRound {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.rounds)
}

// results returns every recorded result for serviceID, oldest first.
func (f *fakeSink) results(serviceID int64) []checks.RoundResult {
	var out []checks.RoundResult
	for _, r := range f.snapshot() {
		if res, ok := r.result(serviceID); ok {
			out = append(out, res)
		}
	}
	return out
}

// serviceYAML renders one service entry. Its host is its name, so a test's
// check function can tell services apart by host.
func serviceYAML(name string, pointsPerFailure int) string {
	return fmt.Sprintf(`
  - name: %s
    host: %s
    check_type: tcp_connect
    points_per_failure: %d
    params: {port: 9}
`, name, name, pointsPerFailure)
}

func configYAML(services ...string) string {
	return fmt.Sprintf(`
session_name: "s"
starting_score: 1000
round_interval_seconds: 1
admin:
  username: admin
  password_hash: %q
services:
%s`, adminHash, strings.Join(services, ""))
}

func writeConfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
}

// newConfigWith writes a config with the given services and returns its path.
func newConfigWith(t *testing.T, services ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, configYAML(services...))
	return path
}

// newConfig writes a config with a single service "svc1" (points_per_failure
// 10) and returns its path.
func newConfig(t *testing.T) string {
	t.Helper()
	return newConfigWith(t, serviceYAML("svc1", 10))
}

// registryWith returns a throwaway registry with fn registered as
// tcp_connect.
func registryWith(fn checks.CheckFunc) *checks.Registry {
	r := checks.NewRegistry()
	r.Register("tcp_connect", fn)
	return r
}

func okCheck(context.Context, string, map[string]any) checks.Result {
	return checks.Result{Success: true, Detail: "ok"}
}

// newScheduler builds a Scheduler with the given round interval without
// starting its loop, so a test can enable services before the first round.
func newScheduler(t *testing.T, registry *checks.Registry, sink ResultSink, path string, interval time.Duration) *Scheduler {
	t.Helper()
	return New(registry, sink, path, interval)
}

// run starts s's round loop at round 1 and stops it, waiting for it to exit,
// when the test ends.
func run(t *testing.T, s *Scheduler) {
	t.Helper()
	runFrom(t, s, 1)
}

// runFrom is run with an explicit first round number.
func runFrom(t *testing.T, s *Scheduler, firstRound int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := s.Run(ctx, firstRound)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(waitTimeout):
			t.Error("round loop did not exit after its context was cancelled")
		}
	})
}

// newTestScheduler builds a Scheduler with the testRound interval and starts
// its loop.
func newTestScheduler(t *testing.T, registry *checks.Registry, sink ResultSink, path string) *Scheduler {
	t.Helper()
	s := newScheduler(t, registry, sink, path, testRound)
	run(t, s)
	return s
}

// mustStart starts serviceID as name or fails the test.
func mustStart(t *testing.T, s *Scheduler, serviceID int64, name string) {
	t.Helper()
	if err := s.StartService(serviceID, name); err != nil {
		t.Fatalf("StartService(%d, %q): %v", serviceID, name, err)
	}
}

// waitFor polls cond until it holds or waitTimeout elapses.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// receive waits for a value on ch, failing the test after waitTimeout.
func receive[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitTimeout):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// inFlight tracks how many check invocations are running at once.
type inFlight struct {
	cur, max atomic.Int32
}

func (f *inFlight) enter() {
	n := f.cur.Add(1)
	for {
		m := f.max.Load()
		if n <= m || f.max.CompareAndSwap(m, n) {
			return
		}
	}
}

func (f *inFlight) exit() { f.cur.Add(-1) }

func TestStartService_RecordsEveryRoundWithConfigValues(t *testing.T) {
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(okCheck), sink, newConfig(t))

	mustStart(t, s, 7, "svc1")
	if !s.IsRunning(7) {
		t.Fatal("IsRunning(7) = false right after StartService")
	}
	waitFor(t, "3 recorded rounds", func() bool { return sink.count() >= 3 })

	for i, r := range sink.snapshot() {
		// Idle ticks before the start are not rounds: numbering starts at the
		// first round with something in it and has no gaps.
		if r.round != i+1 {
			t.Errorf("recorded round %d is numbered %d, want %d", i, r.round, i+1)
		}
		if len(r.results) != 1 {
			t.Fatalf("round %d results = %+v, want exactly one", r.round, r.results)
		}
		res := r.results[0]
		if res.ServiceID != 7 || res.PointsPerFailure != 10 || !res.Result.Success || res.Result.Detail != "ok" {
			t.Errorf("round %d result = %+v, want service 7, 10 points, successful \"ok\"", r.round, res)
		}
	}
}

func TestStartService_PassesHostAndParams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, configYAML(`
  - name: svc1
    host: 10.0.0.5
    check_type: tcp_connect
    points_per_failure: 10
    params:
      port: 22
`))
	type args struct {
		host   string
		params map[string]any
	}
	got := make(chan args, 1)
	s := newTestScheduler(t, registryWith(func(_ context.Context, host string, params map[string]any) checks.Result {
		select {
		case got <- args{host, params}:
		default:
		}
		return checks.Result{Success: true}
	}), &fakeSink{}, path)

	mustStart(t, s, 1, "svc1")
	a := receive(t, "the check to be called", got)
	if a.host != "10.0.0.5" || a.params["port"] != 22 {
		t.Errorf("check called with host %q params %v, want 10.0.0.5 and port 22", a.host, a.params)
	}
}

func TestRun_TwoServicesReportTheSameRoundTogether(t *testing.T) {
	sink := &fakeSink{}
	s := newScheduler(t, registryWith(okCheck), sink, newConfigWith(t, serviceYAML("a", 1), serviceYAML("b", 2)), testRound)
	mustStart(t, s, 1, "a")
	mustStart(t, s, 2, "b")
	run(t, s)

	waitFor(t, "3 recorded rounds", func() bool { return sink.count() >= 3 })
	for _, r := range sink.snapshot() {
		if len(r.results) != 2 || !r.has(1) || !r.has(2) {
			t.Errorf("round %d results = %+v, want one each for services 1 and 2", r.round, r.results)
		}
	}
}

func TestRun_PacesOneRoundPerInterval(t *testing.T) {
	const interval = 100 * time.Millisecond
	sink := &fakeSink{}
	s := newScheduler(t, registryWith(okCheck), sink, newConfig(t), interval)
	mustStart(t, s, 1, "svc1")
	run(t, s)

	waitFor(t, "3 recorded rounds", func() bool { return sink.count() >= 3 })
	rounds := sink.snapshot()
	for i := 1; i < len(rounds); i++ {
		// Rounds start on a fixed cadence, measured start to start.
		if gap := rounds[i].start.Sub(rounds[i-1].start); gap < interval {
			t.Errorf("round %d started %v after round %d, want at least the %v interval",
				rounds[i].round, gap, rounds[i-1].round, interval)
		}
	}
}

// A check that ignores its context and never returns costs its own service a
// failure for the round, and holds the round up by no more than the grace
// period: every other service's result still arrives on time.
func TestRun_AServiceThatNeverReturnsGetsAFailureAndOthersStillReportOnTime(t *testing.T) {
	const interval = 200 * time.Millisecond
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // let the leaked check goroutines exit
	sink := &fakeSink{}
	s := newScheduler(t, registryWith(func(_ context.Context, host string, _ map[string]any) checks.Result {
		if host == "stuck" {
			<-release // ignores ctx entirely
		}
		return checks.Result{Success: true, Detail: "ok"}
	}), sink, newConfigWith(t, serviceYAML("stuck", 1), serviceYAML("fine", 1)), interval)
	mustStart(t, s, 1, "stuck")
	mustStart(t, s, 2, "fine")

	runStart := time.Now()
	run(t, s)
	waitFor(t, "a recorded round", func() bool { return sink.count() >= 1 })

	r := sink.snapshot()[0]
	if took := r.at.Sub(runStart); took > interval+checkGrace+100*time.Millisecond {
		t.Errorf("first round recorded %v after the loop started, want about the %v interval plus at most the %v grace",
			took, interval, checkGrace)
	}
	stuck, ok := r.result(1)
	if !ok || stuck.Result.Success || !strings.Contains(stuck.Result.Detail, "did not return") {
		t.Errorf("stuck service's result = %+v (present %v), want a failure saying the check did not return", stuck, ok)
	}
	fine, ok := r.result(2)
	if !ok || !fine.Result.Success {
		t.Errorf("well-behaved service's result = %+v (present %v), want its normal success", fine, ok)
	}
}

func TestStartService_IsIdempotent(t *testing.T) {
	var flight inFlight
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(func(context.Context, string, map[string]any) checks.Result {
		flight.enter()
		defer flight.exit()
		time.Sleep(testRound / 4)
		return checks.Result{Success: true}
	}), sink, newConfig(t))

	for range 3 {
		mustStart(t, s, 1, "svc1")
	}
	waitFor(t, "3 recorded rounds", func() bool { return sink.count() >= 3 })
	if m := flight.max.Load(); m != 1 {
		t.Errorf("max concurrent checks = %d, want 1 (repeat StartService must not check twice per round)", m)
	}
	for _, r := range sink.snapshot() {
		if len(r.results) != 1 {
			t.Errorf("round %d results = %+v, want exactly one", r.round, r.results)
		}
	}
}

func TestStartService_ConcurrentCallsEnableOnce(t *testing.T) {
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(okCheck), sink, newConfig(t))

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if err := s.StartService(1, "svc1"); err != nil {
				t.Errorf("StartService: %v", err)
			}
		})
	}
	wg.Wait()
	waitFor(t, "2 recorded rounds", func() bool { return sink.count() >= 2 })
	for _, r := range sink.snapshot() {
		if len(r.results) != 1 {
			t.Errorf("round %d results = %+v, want exactly one", r.round, r.results)
		}
	}
	if ids := s.RunningServiceIDs(); len(ids) != 1 || ids[0] != 1 {
		t.Errorf("RunningServiceIDs = %v, want [1]", ids)
	}
}

func TestStartService_UnknownCheckType(t *testing.T) {
	s := newTestScheduler(t, checks.NewRegistry(), &fakeSink{}, newConfig(t))

	err := s.StartService(1, "svc1")
	if err == nil || !strings.Contains(err.Error(), "tcp_connect") {
		t.Fatalf("StartService error = %v, want one naming check_type tcp_connect", err)
	}
	if s.IsRunning(1) {
		t.Error("IsRunning = true after failed StartService")
	}
}

func TestStartService_ServiceNotInConfig(t *testing.T) {
	s := newTestScheduler(t, registryWith(okCheck), &fakeSink{}, newConfig(t))

	err := s.StartService(1, "nope")
	if !errors.Is(err, config.ErrServiceNotFound) {
		t.Fatalf("StartService error = %v, want ErrServiceNotFound", err)
	}
	if s.IsRunning(1) {
		t.Error("IsRunning = true after failed StartService")
	}
}

// A service started while a round is in progress is not added to it: it is
// first checked in the next round.
func TestStartService_MidRoundJoinsOnlyTheNextRound(t *testing.T) {
	const interval = 300 * time.Millisecond
	started := make(chan struct{})
	release := make(chan struct{})
	var aCalls atomic.Int32
	sink := &fakeSink{}
	s := newScheduler(t, registryWith(func(_ context.Context, host string, _ map[string]any) checks.Result {
		if host == "a" && aCalls.Add(1) == 1 {
			close(started)
			<-release
		}
		return checks.Result{Success: true}
	}), sink, newConfigWith(t, serviceYAML("a", 1), serviceYAML("b", 1)), interval)
	mustStart(t, s, 1, "a")
	run(t, s)

	receive(t, "a's first check to start", started)
	mustStart(t, s, 2, "b") // while a's round-1 check is still running
	close(release)

	waitFor(t, "2 recorded rounds", func() bool { return sink.count() >= 2 })
	rounds := sink.snapshot()
	if first := rounds[0]; len(first.results) != 1 || !first.has(1) {
		t.Errorf("round %d (in progress when b started) results = %+v, want only service 1", first.round, first.results)
	}
	if second := rounds[1]; !second.has(1) || !second.has(2) {
		t.Errorf("round %d results = %+v, want both services", second.round, second.results)
	}
}

func TestStopService_HaltsRecordingAndIsIdempotent(t *testing.T) {
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(okCheck), sink, newConfig(t))

	s.StopService(1) // never started: no-op
	mustStart(t, s, 1, "svc1")
	waitFor(t, "a recorded round", func() bool { return sink.count() >= 1 })

	s.StopService(1)
	resultsAtStop := len(sink.results(1))
	if s.IsRunning(1) {
		t.Error("IsRunning = true after StopService")
	}
	time.Sleep(5 * testRound)
	if n := len(sink.results(1)); n != resultsAtStop {
		t.Errorf("results recorded after StopService returned: had %d, now %d", resultsAtStop, n)
	}
	s.StopService(1) // already stopped: no-op
}

// A service stopped while its check is part of the round in progress is left
// out of that round entirely — not even as a failure — and the rest of the
// round is still recorded.
func TestStopService_DuringAnInProgressRoundExcludesItFromThatRound(t *testing.T) {
	// Long enough that the deadline is not what ends the blocked check.
	const interval = 2 * time.Second
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	sink := &fakeSink{}
	s := newScheduler(t, registryWith(func(_ context.Context, host string, _ map[string]any) checks.Result {
		if host == "a" {
			started <- struct{}{}
			<-release // ignores ctx, so the stop cannot hurry it
			return checks.Result{Success: true, Detail: "too late"}
		}
		return checks.Result{Success: true}
	}), sink, newConfigWith(t, serviceYAML("a", 1), serviceYAML("b", 1)), interval)
	mustStart(t, s, 1, "a")
	mustStart(t, s, 2, "b")
	run(t, s)

	receive(t, "a's check to start", started)
	stopped := make(chan struct{})
	go func() { s.StopService(1); close(stopped) }()
	// StopService does not wait for the check it cancelled.
	receive(t, "StopService to return while a's check is still blocked", stopped)
	close(release)

	waitFor(t, "the round to be recorded", func() bool { return sink.count() >= 1 })
	r := sink.snapshot()[0]
	if r.has(1) {
		t.Errorf("round %d results = %+v, want none for service 1, stopped mid-round", r.round, r.results)
	}
	if !r.has(2) {
		t.Errorf("round %d results = %+v, want service 2's result still recorded", r.round, r.results)
	}
	// The stopped check is cancelled and given up on after the grace period,
	// so it does not hold the round to its 2s deadline.
	if took := r.at.Sub(r.start); took > interval/2 {
		t.Errorf("round took %v to record after its only blocked check was stopped, want well under the %v deadline", took, interval)
	}
}

// Once StopService has returned, nothing more is recorded for the service,
// even when the stop lands between a round's end and its recording.
func TestStopService_WaitsForARoundBeingRecorded(t *testing.T) {
	inSink := make(chan struct{})
	finishSink := make(chan struct{})
	var once sync.Once
	sink := &fakeSink{}
	sink.setDuring(func() {
		once.Do(func() {
			close(inSink)
			<-finishSink
		})
	})
	s := newScheduler(t, registryWith(okCheck), sink, newConfig(t), testRound)
	mustStart(t, s, 1, "svc1")
	run(t, s)

	receive(t, "the first round to reach the sink", inSink)
	stopped := make(chan struct{})
	go func() { s.StopService(1); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("StopService returned while a round holding the service's result was still being recorded")
	case <-time.After(5 * testRound):
	}
	close(finishSink)
	receive(t, "StopService to return once the round was recorded", stopped)
	if n := len(sink.results(1)); n != 1 {
		t.Errorf("results for service 1 = %d, want just the 1 from the round that ended before the stop", n)
	}
}

func TestRoundStatus_AdvancesOnlyAfterARoundWithSomethingEnabledCompletes(t *testing.T) {
	const interval = 100 * time.Millisecond
	sink := &fakeSink{}
	s := newScheduler(t, registryWith(okCheck), sink, newConfig(t), interval)
	runFrom(t, s, 5) // as if rounds 1-4 were recorded before a restart

	if round, _ := s.RoundStatus(); round != 4 {
		t.Fatalf("RoundStatus before any round = %d, want 4", round)
	}
	time.Sleep(4 * interval) // several idle ticks
	// Idle ticks keep the countdown to the next tick live but record nothing.
	if round, next := s.RoundStatus(); round != 4 || next.IsZero() || sink.count() != 0 {
		t.Fatalf("after idle ticks: RoundStatus = (%d, %v), %d rounds recorded; want 4, a next tick, and none",
			round, next, sink.count())
	}

	// The status must not name a round until the sink has it.
	statusInSink := make(chan int, 1)
	sink.setDuring(func() {
		round, _ := s.RoundStatus()
		select {
		case statusInSink <- round:
		default:
		}
	})
	mustStart(t, s, 1, "svc1")
	if during := receive(t, "the first round", statusInSink); during != 4 {
		t.Errorf("RoundStatus while round 5 was being recorded = %d, want still 4", during)
	}
	waitFor(t, "RoundStatus to report round 5", func() bool { round, _ := s.RoundStatus(); return round >= 5 })
	r := sink.snapshot()[0]
	round, next := s.RoundStatus()
	if round == 5 && !next.Equal(r.start.Add(interval)) {
		t.Errorf("nextRoundAt = %v, want round 5's start plus the interval (%v)", next, r.start.Add(interval))
	}

	s.StopService(1)
	stoppedAt, nextAt := s.RoundStatus()
	time.Sleep(4 * interval)
	// The round number holds; the next tick keeps moving forward.
	if round, next := s.RoundStatus(); round != stoppedAt || !next.After(nextAt) {
		t.Errorf("RoundStatus went from (%d, %v) to (%d, %v) with nothing enabled; want the same round and a later next tick",
			stoppedAt, nextAt, round, next)
	}
}

func TestRoundStatus_PublishesProjectedNextRoundAtAsSoonAsTheRoundBegins(t *testing.T) {
	checkStarted := make(chan struct{})
	release := make(chan struct{})
	slowCheck := func(context.Context, string, map[string]any) checks.Result {
		close(checkStarted)
		<-release
		return checks.Result{Success: true}
	}
	sink := &fakeSink{}
	s := newScheduler(t, registryWith(slowCheck), sink, newConfig(t), testRound)
	run(t, s)
	mustStart(t, s, 1, "svc1")

	receive(t, "check to start", checkStarted)
	_, nextRoundAt := s.RoundStatus()
	close(release)

	if nextRoundAt.IsZero() {
		t.Fatal("RoundStatus returned a zero nextRoundAt while svc1's check was still running; " +
			"it should already reflect this round's projected end")
	}
}

func TestIsChecking_TrueOnlyWhileTheRoundsCheckIsRunning(t *testing.T) {
	checkStarted := make(chan struct{})
	release := make(chan struct{})
	slowCheck := func(context.Context, string, map[string]any) checks.Result {
		close(checkStarted)
		<-release
		return checks.Result{Success: true}
	}
	sink := &fakeSink{}
	s := newScheduler(t, registryWith(slowCheck), sink, newConfig(t), testRound)
	run(t, s)
	mustStart(t, s, 1, "svc1")

	if s.IsChecking(1) {
		t.Fatal("IsChecking is true before the service's first check has even started")
	}
	receive(t, "check to start", checkStarted)
	if !s.IsChecking(1) {
		t.Fatal("IsChecking is false while the check is still running")
	}
	close(release)
	waitFor(t, "the round to be recorded", func() bool { return sink.count() == 1 })
	if s.IsChecking(1) {
		t.Fatal("IsChecking is still true after the round finished")
	}
}

func TestHungCheckIsForceFailedAtDeadline(t *testing.T) {
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(func(ctx context.Context, _ string, _ map[string]any) checks.Result {
		<-ctx.Done()
		// A misbehaving plugin that claims success after its deadline must
		// still be scored as a failure.
		return checks.Result{Success: true, Detail: "late"}
	}), sink, newConfig(t))

	mustStart(t, s, 1, "svc1")
	waitFor(t, "2 recorded rounds", func() bool { return sink.count() >= 2 })

	for i, res := range sink.results(1) {
		if res.Result.Success {
			t.Errorf("result %d: hung check recorded as success", i)
		}
		if !strings.Contains(res.Result.Detail, "timed out") {
			t.Errorf("result %d: detail %q, want it to mention the timeout", i, res.Result.Detail)
		}
		if res.Latency < testRound {
			t.Errorf("result %d: latency %v, want at least the %v deadline", i, res.Latency, testRound)
		}
	}
}

func TestChecksNeverOverlap(t *testing.T) {
	var flight inFlight
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(func(ctx context.Context, _ string, _ map[string]any) checks.Result {
		flight.enter()
		defer flight.exit()
		<-ctx.Done() // always uses the full interval
		return checks.Result{}
	}), sink, newConfig(t))

	mustStart(t, s, 1, "svc1")
	waitFor(t, "4 recorded rounds", func() bool { return sink.count() >= 4 })
	if m := flight.max.Load(); m != 1 {
		t.Errorf("max concurrent checks = %d, want 1", m)
	}
}

func TestSinkErrorDoesNotStopLoop(t *testing.T) {
	sink := &fakeSink{err: errors.New("disk full")}
	s := newTestScheduler(t, registryWith(okCheck), sink, newConfig(t))

	mustStart(t, s, 1, "svc1")
	waitFor(t, "3 recorded rounds despite sink errors", func() bool { return sink.count() >= 3 })
	if !s.IsRunning(1) {
		t.Error("IsRunning = false after sink errors")
	}
}

func TestPanickingCheckIsRecordedAsFailure(t *testing.T) {
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(func(context.Context, string, map[string]any) checks.Result {
		panic("boom")
	}), sink, newConfig(t))

	mustStart(t, s, 1, "svc1")
	waitFor(t, "2 recorded rounds", func() bool { return sink.count() >= 2 })
	res := sink.results(1)[0]
	if res.Result.Success || !strings.Contains(res.Result.Detail, "boom") {
		t.Errorf("result = %+v, want a failure mentioning the panic", res.Result)
	}
}

func TestRun_StopsPromptlyWhenCancelledAndAbandonsTheRoundInProgress(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	sink := &fakeSink{}
	s := New(registryWith(func(context.Context, string, map[string]any) checks.Result {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release // ignores ctx
		return checks.Result{Success: true}
	}), sink, newConfig(t), time.Hour)
	mustStart(t, s, 1, "svc1")

	ctx, cancel := context.WithCancel(context.Background())
	done := s.Run(ctx, 1)
	receive(t, "the check to start", started)
	cancelledAt := time.Now()
	cancel()
	receive(t, "the round loop to exit", done)
	if took := time.Since(cancelledAt); took > checkGrace+200*time.Millisecond {
		t.Errorf("round loop took %v to exit after cancel, want about the %v grace", took, checkGrace)
	}
	if n := sink.count(); n != 0 {
		t.Errorf("recorded %d rounds, want 0: the round in progress at cancel is abandoned", n)
	}
}

func TestRestartService_ReloadsConfig(t *testing.T) {
	path := newConfig(t) // points_per_failure: 10
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(okCheck), sink, path)

	mustStart(t, s, 1, "svc1")
	waitFor(t, "a recorded round", func() bool { return sink.count() >= 1 })

	writeConfig(t, path, configYAML(serviceYAML("svc1", 99)))
	if err := s.RestartService(1, "svc1"); err != nil {
		t.Fatalf("RestartService: %v", err)
	}
	before := len(sink.results(1))
	waitFor(t, "a result after restart", func() bool { return len(sink.results(1)) > before })

	for i, res := range sink.results(1)[before:] {
		if res.PointsPerFailure != 99 {
			t.Errorf("post-restart result %d pointsPerFailure = %d, want 99", i, res.PointsPerFailure)
		}
	}
}

func TestRestartService_StartsWhenNotRunning(t *testing.T) {
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(okCheck), sink, newConfig(t))

	if err := s.RestartService(1, "svc1"); err != nil {
		t.Fatalf("RestartService: %v", err)
	}
	if !s.IsRunning(1) {
		t.Fatal("IsRunning = false after RestartService of a stopped service")
	}
	waitFor(t, "a recorded round", func() bool { return sink.count() >= 1 })
}

func TestRestartService_BadConfigLeavesServiceStopped(t *testing.T) {
	path := newConfig(t)
	s := newTestScheduler(t, registryWith(okCheck), &fakeSink{}, path)

	mustStart(t, s, 1, "svc1")
	writeConfig(t, path, configYAML(serviceYAML("other", 10)))
	if err := s.RestartService(1, "svc1"); !errors.Is(err, config.ErrServiceNotFound) {
		t.Fatalf("RestartService error = %v, want ErrServiceNotFound", err)
	}
	if s.IsRunning(1) {
		t.Error("IsRunning = true after a RestartService whose config load failed")
	}
}

func TestStopService_LeavesOtherServicesRunning(t *testing.T) {
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(okCheck), sink, newConfigWith(t, serviceYAML("a", 1), serviceYAML("b", 2)))

	mustStart(t, s, 1, "a")
	mustStart(t, s, 2, "b")
	s.StopService(1)
	if s.IsRunning(1) || !s.IsRunning(2) {
		t.Fatalf("IsRunning(1)=%v IsRunning(2)=%v, want false, true", s.IsRunning(1), s.IsRunning(2))
	}
	before := sink.count()
	waitFor(t, "service b to keep recording", func() bool { return sink.count() >= before+2 })
	for _, r := range sink.snapshot()[before:] {
		if r.has(1) || !r.has(2) {
			t.Errorf("round %d results = %+v, want only service 2's", r.round, r.results)
		}
	}
	if ids := s.RunningServiceIDs(); len(ids) != 1 || ids[0] != 2 {
		t.Errorf("RunningServiceIDs = %v, want [2]", ids)
	}
}

func TestRunningServiceIDs_Sorted(t *testing.T) {
	s := newTestScheduler(t, registryWith(okCheck), &fakeSink{}, newConfigWith(t, serviceYAML("a", 1)))

	if ids := s.RunningServiceIDs(); len(ids) != 0 {
		t.Errorf("RunningServiceIDs before any start = %v, want empty", ids)
	}
	for _, id := range []int64{5, 2, 9} {
		mustStart(t, s, id, "a")
	}
	if got := s.RunningServiceIDs(); fmt.Sprint(got) != "[2 5 9]" {
		t.Errorf("RunningServiceIDs = %v, want [2 5 9]", got)
	}
}

// TestConcurrentStartStopRestart hammers one service from many goroutines and
// checks the end state is coherent: no round ever held two results for it,
// and after a final Stop nothing is enabled and nothing more is recorded.
func TestConcurrentStartStopRestart(t *testing.T) {
	sink := &fakeSink{}
	s := newScheduler(t, registryWith(func(context.Context, string, map[string]any) checks.Result {
		time.Sleep(time.Millisecond)
		return checks.Result{Success: true}
	}), sink, newConfig(t), 5*time.Millisecond)
	run(t, s)

	var wg sync.WaitGroup
	for g := range 12 {
		wg.Go(func() {
			for i := range 30 {
				switch (g + i) % 4 {
				case 0, 1:
					if err := s.StartService(1, "svc1"); err != nil {
						t.Errorf("StartService: %v", err)
					}
				case 2:
					s.StopService(1)
				case 3:
					if err := s.RestartService(1, "svc1"); err != nil {
						t.Errorf("RestartService: %v", err)
					}
				}
				_ = s.IsRunning(1)
			}
		})
	}
	wg.Wait()

	s.StopService(1)
	if s.IsRunning(1) {
		t.Error("IsRunning = true after final StopService")
	}
	countAtStop := sink.count()
	time.Sleep(10 * 5 * time.Millisecond)
	if n := sink.count(); n != countAtStop {
		t.Errorf("rounds recorded after final StopService: had %d, now %d", countAtStop, n)
	}
	for _, r := range sink.snapshot() {
		if len(r.results) != 1 {
			t.Errorf("round %d results = %+v, want exactly one", r.round, r.results)
		}
	}
}

// StartRoundNow runs a round at once instead of waiting out the current tick,
// and the rounds after it are timed from it.
func TestStartRoundNow_RunsARoundImmediatelyAndRetimesTheClock(t *testing.T) {
	sink := &fakeSink{}
	s := newScheduler(t, registryWith(okCheck), sink, newConfig(t), time.Hour)
	runFrom(t, s, 1)
	// Let the loop take its first (idle) tick, so it is waiting out the hour.
	waitFor(t, "the first tick", func() bool { _, next := s.RoundStatus(); return !next.IsZero() })
	mustStart(t, s, 1, "svc1")

	kicked := time.Now()
	s.StartRoundNow()
	waitFor(t, "a round right after StartRoundNow", func() bool { return sink.count() == 1 })
	_, next := s.RoundStatus()
	if next.Before(kicked.Add(time.Hour)) {
		t.Errorf("next round at %v, want an hour after the immediate round (after %v)", next, kicked.Add(time.Hour))
	}
}
