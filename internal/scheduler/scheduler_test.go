package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/config"
)

// testUnit replaces one "interval second" in tests, so a service configured
// with interval_seconds: 1 cycles every testUnit instead of every second.
const testUnit = 20 * time.Millisecond

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

type recordedCall struct {
	serviceID        int64
	pointsPerFailure int
	result           checks.Result
	latency          time.Duration
}

// fakeSink records every Record call. If err is set, Record still records the
// call but returns err.
type fakeSink struct {
	mu    sync.Mutex
	calls []recordedCall
	err   error
}

func (f *fakeSink) Record(serviceID int64, pointsPerFailure int, result checks.Result, latency time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordedCall{serviceID, pointsPerFailure, result, latency})
	return f.err
}

func (f *fakeSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSink) snapshot() []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedCall(nil), f.calls...)
}

// serviceYAML renders one service entry.
func serviceYAML(name string, intervalSeconds, pointsPerFailure int) string {
	return fmt.Sprintf(`
  - name: %s
    host: 127.0.0.1
    check_type: tcp_connect
    interval_seconds: %d
    points_per_failure: %d
    params: {port: 9}
`, name, intervalSeconds, pointsPerFailure)
}

func configYAML(services ...string) string {
	return fmt.Sprintf(`
session_name: "s"
starting_score: 1000
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

// newConfig writes a config with a single service "svc1" (interval 1,
// points_per_failure 10) and returns its path.
func newConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, configYAML(serviceYAML("svc1", 1, 10)))
	return path
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

// newTestScheduler builds a Scheduler with no startup stagger and a fast
// interval unit, and makes sure every routine is stopped when the test ends.
func newTestScheduler(t *testing.T, registry *checks.Registry, sink ResultSink, path string) *Scheduler {
	t.Helper()
	s := New(registry, sink, path)
	s.stagger = func() time.Duration { return 0 }
	s.intervalUnit = testUnit
	t.Cleanup(func() {
		for _, id := range s.RunningServiceIDs() {
			s.StopService(id)
		}
	})
	return s
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

func TestNew_Defaults(t *testing.T) {
	s := New(checks.NewRegistry(), &fakeSink{}, "unused.yaml")
	if s.intervalUnit != time.Second {
		t.Errorf("intervalUnit = %v, want 1s", s.intervalUnit)
	}
	for range 100 {
		if d := s.stagger(); d < 0 || d >= maxStagger {
			t.Fatalf("default stagger returned %v, want within [0, %v)", d, maxStagger)
		}
	}
}

func TestStartService_RecordsRepeatedlyWithConfigValues(t *testing.T) {
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(okCheck), sink, newConfig(t))

	if err := s.StartService(7, "svc1"); err != nil {
		t.Fatalf("StartService: %v", err)
	}
	if !s.IsRunning(7) {
		t.Fatal("IsRunning(7) = false right after StartService")
	}
	waitFor(t, "3 recorded checks", func() bool { return sink.count() >= 3 })

	for i, c := range sink.snapshot() {
		if c.serviceID != 7 || c.pointsPerFailure != 10 || !c.result.Success || c.result.Detail != "ok" {
			t.Errorf("call %d = %+v, want service 7, 10 points, successful \"ok\"", i, c)
		}
	}
}

func TestStartService_PassesHostAndParams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, configYAML(`
  - name: svc1
    host: 10.0.0.5
    check_type: tcp_connect
    interval_seconds: 1
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

	if err := s.StartService(1, "svc1"); err != nil {
		t.Fatalf("StartService: %v", err)
	}
	select {
	case a := <-got:
		if a.host != "10.0.0.5" || a.params["port"] != 22 {
			t.Errorf("check called with host %q params %v, want 10.0.0.5 and port 22", a.host, a.params)
		}
	case <-time.After(waitTimeout):
		t.Fatal("check never called")
	}
}

func TestStartService_PacesOneCheckPerInterval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, configYAML(serviceYAML("svc1", 5, 10))) // 5 * 20ms = 100ms
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(okCheck), sink, path)

	start := time.Now()
	if err := s.StartService(1, "svc1"); err != nil {
		t.Fatalf("StartService: %v", err)
	}
	waitFor(t, "3 recorded checks", func() bool { return sink.count() >= 3 })
	// Checks at ~0, ~100ms, ~200ms: the third cannot land before 200ms.
	if elapsed := time.Since(start); elapsed < 2*5*testUnit {
		t.Errorf("3 checks completed after %v, want at least %v (one per interval)", elapsed, 2*5*testUnit)
	}
}

func TestStartService_IsIdempotent(t *testing.T) {
	var flight inFlight
	s := newTestScheduler(t, registryWith(func(context.Context, string, map[string]any) checks.Result {
		flight.enter()
		defer flight.exit()
		time.Sleep(testUnit / 2)
		return checks.Result{Success: true}
	}), &fakeSink{}, newConfig(t))

	for range 3 {
		if err := s.StartService(1, "svc1"); err != nil {
			t.Fatalf("StartService: %v", err)
		}
	}
	time.Sleep(10 * testUnit)
	if m := flight.max.Load(); m != 1 {
		t.Errorf("max concurrent checks = %d, want 1 (repeat StartService must not launch a second routine)", m)
	}
}

func TestStartService_ConcurrentCallsLaunchOneRoutine(t *testing.T) {
	var flight inFlight
	s := newTestScheduler(t, registryWith(func(context.Context, string, map[string]any) checks.Result {
		flight.enter()
		defer flight.exit()
		time.Sleep(testUnit / 2)
		return checks.Result{Success: true}
	}), &fakeSink{}, newConfig(t))

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if err := s.StartService(1, "svc1"); err != nil {
				t.Errorf("StartService: %v", err)
			}
		})
	}
	wg.Wait()
	time.Sleep(10 * testUnit)
	if m := flight.max.Load(); m != 1 {
		t.Errorf("max concurrent checks = %d, want 1", m)
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

func TestStopService_HaltsRecordingAndIsIdempotent(t *testing.T) {
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(okCheck), sink, newConfig(t))

	s.StopService(1) // never started: no-op
	if err := s.StartService(1, "svc1"); err != nil {
		t.Fatalf("StartService: %v", err)
	}
	waitFor(t, "a recorded check", func() bool { return sink.count() >= 1 })

	s.StopService(1)
	countAtStop := sink.count()
	if s.IsRunning(1) {
		t.Error("IsRunning = true after StopService")
	}
	time.Sleep(5 * testUnit)
	if n := sink.count(); n != countAtStop {
		t.Errorf("checks recorded after StopService returned: had %d, now %d", countAtStop, n)
	}
	s.StopService(1) // already stopped: no-op
}

func TestStopService_WaitsForInFlightCheckAndDiscardsIt(t *testing.T) {
	started := make(chan struct{}, 1)
	var returned atomic.Bool
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(func(ctx context.Context, _ string, _ map[string]any) checks.Result {
		started <- struct{}{}
		<-ctx.Done()
		time.Sleep(testUnit) // slow to unwind: Stop must still wait for us
		returned.Store(true)
		return checks.Result{Success: false, Detail: "cancelled"}
	}), sink, newConfig(t))
	s.intervalUnit = time.Hour // the deadline must not be what ends this check

	if err := s.StartService(1, "svc1"); err != nil {
		t.Fatalf("StartService: %v", err)
	}
	<-started
	s.StopService(1)
	if !returned.Load() {
		t.Error("StopService returned before the in-flight check finished")
	}
	if n := sink.count(); n != 0 {
		t.Errorf("recorded %d results for a check cancelled by StopService, want 0", n)
	}
}

func TestStopService_DuringStagger(t *testing.T) {
	var calls atomic.Int32
	s := newTestScheduler(t, registryWith(func(context.Context, string, map[string]any) checks.Result {
		calls.Add(1)
		return checks.Result{Success: true}
	}), &fakeSink{}, newConfig(t))
	s.stagger = func() time.Duration { return time.Hour }

	if err := s.StartService(1, "svc1"); err != nil {
		t.Fatalf("StartService: %v", err)
	}
	stopped := make(chan struct{})
	go func() { s.StopService(1); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(waitTimeout):
		t.Fatal("StopService blocked on the startup stagger")
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("check ran %d times during the stagger, want 0", n)
	}
}

func TestStagger_DelaysFirstCheck(t *testing.T) {
	const stagger = 150 * time.Millisecond
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(okCheck), sink, newConfig(t))
	s.stagger = func() time.Duration { return stagger }

	start := time.Now()
	if err := s.StartService(1, "svc1"); err != nil {
		t.Fatalf("StartService: %v", err)
	}
	waitFor(t, "a recorded check", func() bool { return sink.count() >= 1 })
	if elapsed := time.Since(start); elapsed < stagger {
		t.Errorf("first check after %v, want no earlier than the %v stagger", elapsed, stagger)
	}
}

func TestSetMaxStagger(t *testing.T) {
	s := New(checks.NewRegistry(), &fakeSink{}, "unused.yaml")

	s.SetMaxStagger(0)
	for range 100 {
		if d := s.stagger(); d != 0 {
			t.Fatalf("SetMaxStagger(0): stagger returned %v, want 0", d)
		}
	}

	const limit = 40 * time.Millisecond
	s.SetMaxStagger(limit)
	for range 100 {
		if d := s.stagger(); d < 0 || d >= limit {
			t.Fatalf("SetMaxStagger(%v): stagger returned %v, want within [0, %v)", limit, d, limit)
		}
	}
}

func TestSetMaxStagger_ZeroMakesFirstCheckImmediate(t *testing.T) {
	sink := &fakeSink{}
	s := New(registryWith(okCheck), sink, newConfig(t))
	s.SetMaxStagger(0)
	t.Cleanup(func() { s.StopService(1) })

	start := time.Now()
	if err := s.StartService(1, "svc1"); err != nil {
		t.Fatalf("StartService: %v", err)
	}
	waitFor(t, "a recorded check", func() bool { return sink.count() >= 1 })
	// The default stagger is up to 5s; with none, the first check is prompt.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("first check after %v, want it immediately", elapsed)
	}
}

func TestHungCheckIsForceFailedAtDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, configYAML(serviceYAML("svc1", 3, 10))) // 60ms deadline
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(func(ctx context.Context, _ string, _ map[string]any) checks.Result {
		<-ctx.Done()
		// A misbehaving plugin that claims success after its deadline must
		// still be scored as a failure.
		return checks.Result{Success: true, Detail: "late"}
	}), sink, path)

	if err := s.StartService(1, "svc1"); err != nil {
		t.Fatalf("StartService: %v", err)
	}
	waitFor(t, "2 recorded checks", func() bool { return sink.count() >= 2 })

	for i, c := range sink.snapshot() {
		if c.result.Success {
			t.Errorf("call %d: hung check recorded as success", i)
		}
		if !strings.Contains(c.result.Detail, "timed out") {
			t.Errorf("call %d: detail %q, want it to mention the timeout", i, c.result.Detail)
		}
		if c.latency < 3*testUnit {
			t.Errorf("call %d: latency %v, want at least the %v deadline", i, c.latency, 3*testUnit)
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

	if err := s.StartService(1, "svc1"); err != nil {
		t.Fatalf("StartService: %v", err)
	}
	waitFor(t, "4 recorded checks", func() bool { return sink.count() >= 4 })
	if m := flight.max.Load(); m != 1 {
		t.Errorf("max concurrent checks = %d, want 1", m)
	}
}

func TestSinkErrorDoesNotStopLoop(t *testing.T) {
	sink := &fakeSink{err: errors.New("disk full")}
	s := newTestScheduler(t, registryWith(okCheck), sink, newConfig(t))

	if err := s.StartService(1, "svc1"); err != nil {
		t.Fatalf("StartService: %v", err)
	}
	waitFor(t, "3 recorded checks despite sink errors", func() bool { return sink.count() >= 3 })
	if !s.IsRunning(1) {
		t.Error("IsRunning = false after sink errors")
	}
}

func TestPanickingCheckIsRecordedAsFailure(t *testing.T) {
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(func(context.Context, string, map[string]any) checks.Result {
		panic("boom")
	}), sink, newConfig(t))

	if err := s.StartService(1, "svc1"); err != nil {
		t.Fatalf("StartService: %v", err)
	}
	waitFor(t, "2 recorded checks", func() bool { return sink.count() >= 2 })
	c := sink.snapshot()[0]
	if c.result.Success || !strings.Contains(c.result.Detail, "boom") {
		t.Errorf("result = %+v, want a failure mentioning the panic", c.result)
	}
}

func TestRestartService_ReloadsConfig(t *testing.T) {
	path := newConfig(t) // points_per_failure: 10
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(okCheck), sink, path)

	if err := s.StartService(1, "svc1"); err != nil {
		t.Fatalf("StartService: %v", err)
	}
	waitFor(t, "a recorded check", func() bool { return sink.count() >= 1 })

	writeConfig(t, path, configYAML(serviceYAML("svc1", 1, 99)))
	if err := s.RestartService(1, "svc1"); err != nil {
		t.Fatalf("RestartService: %v", err)
	}
	before := sink.count()
	waitFor(t, "a check after restart", func() bool { return sink.count() > before })

	calls := sink.snapshot()
	for i, c := range calls[before:] {
		if c.pointsPerFailure != 99 {
			t.Errorf("post-restart call %d pointsPerFailure = %d, want 99", i, c.pointsPerFailure)
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
	waitFor(t, "a recorded check", func() bool { return sink.count() >= 1 })
}

func TestRestartService_BadConfigLeavesServiceStopped(t *testing.T) {
	path := newConfig(t)
	s := newTestScheduler(t, registryWith(okCheck), &fakeSink{}, path)

	if err := s.StartService(1, "svc1"); err != nil {
		t.Fatalf("StartService: %v", err)
	}
	writeConfig(t, path, configYAML(serviceYAML("other", 1, 10)))
	if err := s.RestartService(1, "svc1"); !errors.Is(err, config.ErrServiceNotFound) {
		t.Fatalf("RestartService error = %v, want ErrServiceNotFound", err)
	}
	if s.IsRunning(1) {
		t.Error("IsRunning = true after a RestartService whose config load failed")
	}
}

func TestServicesRunIndependently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, configYAML(serviceYAML("a", 1, 1), serviceYAML("b", 1, 2)))
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(okCheck), sink, path)

	if err := s.StartService(1, "a"); err != nil {
		t.Fatalf("StartService a: %v", err)
	}
	if err := s.StartService(2, "b"); err != nil {
		t.Fatalf("StartService b: %v", err)
	}
	s.StopService(1)
	if s.IsRunning(1) || !s.IsRunning(2) {
		t.Fatalf("IsRunning(1)=%v IsRunning(2)=%v, want false, true", s.IsRunning(1), s.IsRunning(2))
	}
	before := sink.count()
	waitFor(t, "service b to keep recording", func() bool { return sink.count() >= before+2 })
	for _, c := range sink.snapshot()[before:] {
		if c.serviceID != 2 {
			t.Errorf("recorded call for service %d after it was stopped", c.serviceID)
		}
	}
	if ids := s.RunningServiceIDs(); len(ids) != 1 || ids[0] != 2 {
		t.Errorf("RunningServiceIDs = %v, want [2]", ids)
	}
}

func TestRunningServiceIDs_Sorted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, configYAML(serviceYAML("a", 1, 1)))
	s := newTestScheduler(t, registryWith(okCheck), &fakeSink{}, path)

	if ids := s.RunningServiceIDs(); len(ids) != 0 {
		t.Errorf("RunningServiceIDs before any start = %v, want empty", ids)
	}
	for _, id := range []int64{5, 2, 9} {
		if err := s.StartService(id, "a"); err != nil {
			t.Fatalf("StartService(%d): %v", id, err)
		}
	}
	got := s.RunningServiceIDs()
	if fmt.Sprint(got) != "[2 5 9]" {
		t.Errorf("RunningServiceIDs = %v, want [2 5 9]", got)
	}
}

// TestConcurrentStartStopRestart hammers one service from many goroutines and
// checks the end state is coherent: after a final Stop nothing is running and
// nothing more is recorded, and at no point did two checks overlap.
func TestConcurrentStartStopRestart(t *testing.T) {
	var flight inFlight
	sink := &fakeSink{}
	s := newTestScheduler(t, registryWith(func(context.Context, string, map[string]any) checks.Result {
		flight.enter()
		defer flight.exit()
		time.Sleep(time.Millisecond)
		return checks.Result{Success: true}
	}), sink, newConfig(t))

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

	if m := flight.max.Load(); m > 1 {
		t.Errorf("max concurrent checks = %d, want at most 1", m)
	}
	s.StopService(1)
	if s.IsRunning(1) {
		t.Error("IsRunning = true after final StopService")
	}
	if n := flight.cur.Load(); n != 0 {
		t.Errorf("%d checks still in flight after StopService returned", n)
	}
	countAtStop := sink.count()
	time.Sleep(5 * testUnit)
	if n := sink.count(); n != countAtStop {
		t.Errorf("checks recorded after final StopService: had %d, now %d", countAtStop, n)
	}
}
