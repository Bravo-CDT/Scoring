// Package scheduler runs each scored service's check on its own self-paced
// loop and reports every result to a ResultSink.
//
// Each service gets one goroutine that repeats: run the check under a
// deadline equal to the service's interval, report the result, sleep for
// whatever remains of the interval. That gives exactly one check per interval
// per service, with no overlap, by construction. A check still running at the
// deadline is cancelled and scored as a failure.
package scheduler

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/config"
)

// maxStagger bounds the random delay before a routine's first check, so
// services started together don't all hit the single-writer store at once.
const maxStagger = 5 * time.Second

// ResultSink is how the Scheduler reports a finished check. Implemented by the
// Scorer.
type ResultSink interface {
	Record(serviceID int64, pointsPerFailure int, result checks.Result, latency time.Duration) error
}

// routine is one running service loop.
type routine struct {
	cancel context.CancelFunc
	done   chan struct{} // closed when the loop's goroutine has exited
}

// Scheduler owns the per-service check routines. It is safe for concurrent
// use.
type Scheduler struct {
	registry   *checks.Registry
	sink       ResultSink
	configPath string

	// intervalUnit is the length of one configured interval_second. Always
	// time.Second outside tests.
	intervalUnit time.Duration

	// mu guards stagger, routines and opLocks. It is only ever held briefly:
	// never across config loading or while waiting for a routine to exit.
	mu sync.Mutex
	// stagger picks the delay before a routine's first check.
	stagger  func() time.Duration
	routines map[int64]*routine
	// opLocks serializes Start/Stop/Restart per service ID, so a
	// check-then-launch or a stop-then-wait is atomic with respect to other
	// operations on the same service, without blocking other services.
	opLocks map[int64]*sync.Mutex
}

// New returns a Scheduler that runs checks from registry, reports to sink,
// and reads each service's settings from the config file at configPath.
func New(registry *checks.Registry, sink ResultSink, configPath string) *Scheduler {
	return &Scheduler{
		registry:     registry,
		sink:         sink,
		configPath:   configPath,
		stagger:      randomStagger(maxStagger),
		intervalUnit: time.Second,
		routines:     make(map[int64]*routine),
		opLocks:      make(map[int64]*sync.Mutex),
	}
}

// SetMaxStagger changes the bound on the random delay before each newly
// started routine's first check (5s by default); zero or less removes the
// delay. It is a tuning and test hook — packages built on the Scheduler use it
// to make their tests' first checks immediate — and is safe to call at any
// time. Routines already started keep the delay they drew.
func (s *Scheduler) SetMaxStagger(limit time.Duration) {
	s.mu.Lock()
	s.stagger = randomStagger(limit)
	s.mu.Unlock()
}

// randomStagger returns a delay picker uniform over [0, limit), or one that
// always returns 0 for a non-positive limit.
func randomStagger(limit time.Duration) func() time.Duration {
	if limit <= 0 {
		return func() time.Duration { return 0 }
	}
	return func() time.Duration { return rand.N(limit) }
}

// StartService is idempotent: if serviceID is already running, it's a no-op.
// Otherwise it loads this service's entry fresh from the config file, looks
// up its check_type in the registry, and launches its self-paced loop.
func (s *Scheduler) StartService(serviceID int64, name string) error {
	unlock := s.lockService(serviceID)
	defer unlock()
	return s.start(serviceID, name)
}

// StopService is idempotent and synchronous: it blocks until the routine's
// goroutine has fully exited before returning. A check in flight when Stop is
// called is cancelled and its result discarded.
//
// Because it waits for the routine, it returns only once the in-flight
// CheckFunc honours its cancelled ctx; a check that ignores ctx holds it up
// until the check returns on its own. For the same reason the ResultSink must
// never call StopService or RestartService for the service whose result it is
// recording: that call runs on the routine's own goroutine and would wait
// forever for itself to exit.
func (s *Scheduler) StopService(serviceID int64) {
	unlock := s.lockService(serviceID)
	defer unlock()
	s.stop(serviceID)
}

// RestartService stops the service (if running) then starts it, always
// re-reading the config file — this is how a config edit takes effect. If the
// fresh config cannot be loaded, the service is left stopped and the error is
// returned.
//
// Its stop half blocks exactly as StopService does: until the in-flight
// CheckFunc honours ctx, and forever if called by the ResultSink for the
// service it is recording.
func (s *Scheduler) RestartService(serviceID int64, name string) error {
	unlock := s.lockService(serviceID)
	defer unlock()
	s.stop(serviceID)
	return s.start(serviceID, name)
}

// IsRunning reports whether serviceID has a routine. A routine being stopped
// counts as running until its goroutine has exited.
func (s *Scheduler) IsRunning(serviceID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.routines[serviceID]
	return ok
}

// RunningServiceIDs returns the IDs of all running services, in ascending
// order.
func (s *Scheduler) RunningServiceIDs() []int64 {
	s.mu.Lock()
	ids := make([]int64, 0, len(s.routines))
	for id := range s.routines {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	slices.SortFunc(ids, cmp.Compare)
	return ids
}

// lockService acquires serviceID's operation lock and returns its release.
func (s *Scheduler) lockService(serviceID int64) (unlock func()) {
	s.mu.Lock()
	l, ok := s.opLocks[serviceID]
	if !ok {
		l = new(sync.Mutex)
		s.opLocks[serviceID] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// start launches serviceID's routine unless it is already running. The caller
// must hold serviceID's operation lock.
func (s *Scheduler) start(serviceID int64, name string) error {
	if s.IsRunning(serviceID) {
		return nil
	}
	cfg, err := config.LoadService(s.configPath, name)
	if err != nil {
		return fmt.Errorf("scheduler: loading config for service %q: %w", name, err)
	}
	fn, ok := s.registry.Get(cfg.CheckType)
	if !ok {
		return fmt.Errorf("scheduler: service %q: no check registered for check_type %q", name, cfg.CheckType)
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := &routine{cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	delay := s.stagger()
	s.routines[serviceID] = r
	s.mu.Unlock()

	go s.run(ctx, r.done, delay, serviceID, *cfg, fn)
	return nil
}

// stop cancels serviceID's routine, if any, and waits for it to exit. The
// caller must hold serviceID's operation lock.
func (s *Scheduler) stop(serviceID int64) {
	s.mu.Lock()
	r, ok := s.routines[serviceID]
	s.mu.Unlock()
	if !ok {
		return
	}
	r.cancel()
	<-r.done
	s.mu.Lock()
	delete(s.routines, serviceID)
	s.mu.Unlock()
}

// run is one service's loop: it waits out the startup stagger delay, then
// checks once per interval. It returns only when ctx is cancelled.
func (s *Scheduler) run(ctx context.Context, done chan<- struct{}, delay time.Duration, serviceID int64, cfg config.ServiceConfig, fn checks.CheckFunc) {
	defer close(done)

	if !sleep(ctx, delay) {
		return
	}
	interval := time.Duration(cfg.IntervalSeconds) * s.intervalUnit
	for {
		start := time.Now()
		result := runCheck(ctx, interval, fn, cfg.Host, cfg.Params)
		latency := time.Since(start)
		if ctx.Err() != nil {
			// Stopped mid-check: the cancellation is ours, not the
			// service's fault, so don't score it.
			return
		}

		// A sink error must never stop this service's loop — one bad write
		// shouldn't end checking for the rest of the competition. The sink
		// owns logging its own failures.
		_ = s.sink.Record(serviceID, cfg.PointsPerFailure, result, latency)

		if !sleep(ctx, interval-time.Since(start)) {
			return
		}
	}
}

// runCheck runs fn under a deadline of timeout. A check still running at the
// deadline, or one that panics, is reported as a failure.
func runCheck(ctx context.Context, timeout time.Duration, fn checks.CheckFunc, host string, params map[string]any) (result checks.Result) {
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			result = checks.Result{Success: false, Detail: fmt.Sprintf("check panicked: %v", p)}
		}
	}()

	result = fn(checkCtx, host, params)
	if errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
		detail := fmt.Sprintf("check timed out after %s", timeout)
		if result.Detail != "" {
			detail += ": " + result.Detail
		}
		result = checks.Result{Success: false, Detail: detail}
	}
	return result
}

// sleep waits for d or until ctx is done, reporting whether it slept the full
// duration. A non-positive d returns immediately.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
