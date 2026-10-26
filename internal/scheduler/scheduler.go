// Package scheduler checks every enabled service once per shared scoring
// round and reports each completed round to a ResultSink.
//
// One round loop (Run) is the only authority on round numbers and timing; no
// service keeps any round state of its own, so services cannot drift apart.
// Each round the loop snapshots the enabled services, runs all of their checks
// at the same instant, waits for every one of them, and hands the whole
// round's results to the sink in one call. Starting a service only adds it to
// the set the next snapshot is taken from, so it joins at the next round
// boundary; stopping one mid-round removes it from the round in progress, and
// its result is discarded.
//
// Every check is hard-bounded (see RunCheck): it is given the round interval
// as its deadline, and the loop stops waiting for it a short grace period
// after that, however its CheckFunc behaves. That bound is what lets the loop
// simply wait for all of a round's checks — one service's hung check can delay
// a round by at most the grace period, and never blocks any other service.
package scheduler

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/config"
)

// checkGrace is how long RunCheck keeps waiting for a check after its
// deadline before giving up on it and reporting a failure.
const checkGrace = 100 * time.Millisecond

// ResultSink is how the Scheduler reports a completed round. Implemented by
// the Scorer.
//
// RecordRound is called from the round loop, one round at a time, never
// concurrently. results holds one entry per service checked in the round, in
// ascending service ID order; roundStart is the instant every one of those
// checks began. The sink must not call StopService or RestartService: those
// wait for an in-progress RecordRound to finish, so calling them from inside
// one would wait forever.
type ResultSink interface {
	RecordRound(round int, roundStart time.Time, results []checks.RoundResult) error
}

// enabledService is one service the next round will check, with the config
// it was started with.
type enabledService struct {
	name string
	cfg  config.ServiceConfig
	fn   checks.CheckFunc
}

// Scheduler owns the set of enabled services and the round loop that checks
// them. It is safe for concurrent use.
type Scheduler struct {
	registry      *checks.Registry
	sink          ResultSink
	configPath    string
	roundInterval time.Duration

	// mu guards enabled, inRound and opLocks. Holding one lock for both
	// enabled and inRound is what makes StopService atomic with respect to a
	// round's start and end: a service is either in a round's snapshot and
	// still in inRound when the round ends, or its result is discarded. mu is
	// only ever held briefly, never across config loading, a check, or the
	// sink.
	mu      sync.Mutex
	enabled map[int64]enabledService
	// inRound holds, for each service whose check is part of the round in
	// progress, the cancel func of that check's context. Membership also
	// means "still counts for this round": StopService deletes the entry to
	// mean both at once.
	inRound map[int64]context.CancelFunc
	// opLocks serializes Start/Stop/Restart per service ID, so a
	// check-then-enable is atomic with respect to other operations on the same
	// service, without blocking other services.
	opLocks map[int64]*sync.Mutex

	// recordMu is held by the round loop from the moment a round ends until
	// the sink has recorded it, and StopService waits on it, so once
	// StopService returns no result of that service is recorded any more —
	// not even one from a round that ended just before the stop.
	recordMu sync.Mutex

	// roundNow, when signalled, cuts the round loop's wait short so the next
	// round begins at once (see StartRoundNow). Buffered so a signal sent
	// while a round runs is taken as soon as it ends.
	roundNow chan struct{}

	// credentials, if set, supplies the blue team's credential overrides at
	// the start of each round. Guarded by mu.
	credentials CredentialSource

	// statusMu guards lastRound, nextRound and nextRoundAt.
	statusMu    sync.Mutex
	lastRound   int
	nextRound   int
	nextRoundAt time.Time
}

// New returns a Scheduler that runs checks from registry, reports to sink,
// reads each service's settings from the config file at configPath, and
// starts a round every roundInterval. Nothing is checked until Run is called.
func New(registry *checks.Registry, sink ResultSink, configPath string, roundInterval time.Duration) *Scheduler {
	return &Scheduler{
		registry:      registry,
		sink:          sink,
		configPath:    configPath,
		roundInterval: roundInterval,
		enabled:       make(map[int64]enabledService),
		inRound:       make(map[int64]context.CancelFunc),
		opLocks:       make(map[int64]*sync.Mutex),
		roundNow:      make(chan struct{}, 1),
	}
}

// CredentialSource returns the credential overrides to check with, by service
// name: a check runs with its service's override in place of the username and
// password in its config, if it has one.
type CredentialSource func() (map[string]checks.Credential, error)

// SetCredentialSource sets where each round gets its credential overrides;
// nil means none, every check on its config. Each round asks once, so a
// change (or a removed override) applies from the next round on.
func (s *Scheduler) SetCredentialSource(src CredentialSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.credentials = src
}

// roundCredentials asks the credential source for this round's overrides. A
// failure is logged and the round runs on the config's credentials: checking
// with possibly stale credentials beats not checking.
func (s *Scheduler) roundCredentials() map[string]checks.Credential {
	s.mu.Lock()
	src := s.credentials
	s.mu.Unlock()
	if src == nil {
		return nil
	}
	creds, err := src()
	if err != nil {
		log.Printf("scheduler: reading credential overrides, checking with the config's this round: %v", err)
		return nil
	}
	return creds
}

// Run starts the round loop, numbering its first round firstRound, and
// returns a channel that is closed once the loop has exited. The loop runs
// until ctx is cancelled; a round in progress at that point is abandoned, not
// recorded. Call Run once. The caller must wait on the returned channel
// before closing anything the sink uses.
//
// A round is only counted, and its number used up, when at least one of its
// results reaches the sink. While no service is enabled the loop keeps
// ticking at the round interval, so a newly started service is first checked
// at the next tick.
func (s *Scheduler) Run(ctx context.Context, firstRound int) <-chan struct{} {
	s.statusMu.Lock()
	s.lastRound = firstRound - 1
	s.nextRound = firstRound
	s.statusMu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			start := time.Now()
			s.statusMu.Lock()
			round := s.nextRound
			s.statusMu.Unlock()
			if s.runRound(ctx, round, start) {
				s.statusMu.Lock()
				// A ResetRounds during the round wins over this advance.
				if s.nextRound == round {
					s.nextRound = round + 1
				}
				s.statusMu.Unlock()
			}
			if !s.waitForNextRound(ctx, time.Until(start.Add(s.roundInterval))) {
				return
			}
		}
	}()
	return done
}

// runRound runs one round that began at start and reports it to the sink as
// round number round, returning whether it did (false for a round with
// nothing to report, or one abandoned because ctx was cancelled).
func (s *Scheduler) runRound(ctx context.Context, round int, start time.Time) bool {
	batch, ctxs := s.beginRound(ctx)
	// Published even for an idle round, so the countdown to the next tick
	// stays live while no service is enabled (e.g. just after the engine
	// starts, before its services' first round).
	s.publishProjectedRoundStatus(start)
	var results []checks.RoundResult
	if len(batch) > 0 {
		results = s.runChecks(ctxs, batch, s.roundCredentials())
	}

	s.recordMu.Lock()
	defer s.recordMu.Unlock()
	stillIn := s.endRound()
	if ctx.Err() != nil {
		return false
	}
	// A service stopped while its check was running is left out entirely:
	// its check was cancelled by the stop, so the outcome is ours, not the
	// service's.
	results = slices.DeleteFunc(results, func(r checks.RoundResult) bool { return !stillIn[r.ServiceID] })
	if len(results) == 0 {
		return false
	}

	// The sink owns reporting its own failures (the Scorer logs each one and
	// records an event); a write that failed must not stop the next round.
	_ = s.sink.RecordRound(round, start, results)
	s.setRoundStatus(round, start)
	return true
}

// beginRound snapshots the enabled services as this round's batch and, under
// the same lock, marks each of them as in the round with a context of its own
// for StopService to cancel.
func (s *Scheduler) beginRound(ctx context.Context) (map[int64]enabledService, map[int64]context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := make(map[int64]enabledService, len(s.enabled))
	ctxs := make(map[int64]context.Context, len(s.enabled))
	for id, svc := range s.enabled {
		checkCtx, cancel := context.WithCancel(ctx)
		batch[id] = svc
		ctxs[id] = checkCtx
		s.inRound[id] = cancel
	}
	return batch, ctxs
}

// endRound ends the round in progress: it returns the services still part of
// it (those not stopped since beginRound) and releases every check context.
func (s *Scheduler) endRound() map[int64]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	stillIn := make(map[int64]bool, len(s.inRound))
	for id, cancel := range s.inRound {
		stillIn[id] = true
		cancel()
	}
	clear(s.inRound)
	return stillIn
}

// runChecks runs every service in batch concurrently, each under its own
// context from ctxs and with its credential override from creds if it has
// one, and returns their results in ascending service ID order.
// It returns once every check has: RunCheck bounds each one at the round
// interval plus checkGrace, whatever its CheckFunc does.
func (s *Scheduler) runChecks(ctxs map[int64]context.Context, batch map[int64]enabledService, creds map[string]checks.Credential) []checks.RoundResult {
	var (
		mu      sync.Mutex
		results = make([]checks.RoundResult, 0, len(batch))
		wg      sync.WaitGroup
	)
	for id, svc := range batch {
		params := svc.cfg.Params
		if c, ok := creds[svc.name]; ok {
			params, _ = checks.WithCredential(svc.cfg.CheckType, params, c)
		}
		wg.Go(func() {
			start := time.Now()
			result := RunCheck(ctxs[id], s.roundInterval, svc.fn, svc.cfg.Host, params)
			r := checks.RoundResult{
				ServiceID:        id,
				PointsPerFailure: svc.cfg.PointsPerFailure,
				Result:           result,
				Latency:          time.Since(start),
			}
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		})
	}
	wg.Wait()
	slices.SortFunc(results, func(a, b checks.RoundResult) int { return cmp.Compare(a.ServiceID, b.ServiceID) })
	return results
}

// RoundStatus returns the number of the last round recorded and when the next
// round is due. Before any round of this process is recorded, round is the
// last one before Run's firstRound; nextRoundAt is the zero time until the
// loop's first tick. An idle loop (no service enabled) does not advance the
// round, but nextRoundAt still moves to each next tick.
func (s *Scheduler) RoundStatus() (round int, nextRoundAt time.Time) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return s.lastRound, s.nextRoundAt
}

// publishProjectedRoundStatus publishes the round's projected nextRoundAt
// (start.Add(roundInterval)) as soon as the round begins, instead of only
// once it finishes. setRoundStatus still runs at the end and corrects it
// (clamping to now if the round overran) and advances lastRound; this only
// ever moves nextRoundAt forward in the meantime, never backward. Without
// this, RoundStatus would keep reporting the previous round's value — frozen
// near the current round's start — for however long this round's checks take
// to run.
func (s *Scheduler) publishProjectedRoundStatus(start time.Time) {
	s.statusMu.Lock()
	s.nextRoundAt = start.Add(s.roundInterval)
	s.statusMu.Unlock()
}

// StartRoundNow begins the next round at once rather than at the next tick,
// and the round clock runs from it: rounds follow every round interval after
// it. If a round is in progress, the next one begins as soon as it ends.
func (s *Scheduler) StartRoundNow() {
	select {
	case s.roundNow <- struct{}{}:
	default: // already signalled
	}
}

// ResetRounds makes the next recorded round number 1 again and reports round
// 0 as the last one, for a reset of the session's round data. The caller must
// ensure no service is enabled (the engine is stopped), so no round is
// recorded under the old numbering afterwards.
func (s *Scheduler) ResetRounds() {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.lastRound = 0
	s.nextRound = 1
}

// ServiceConfig returns the config entry serviceID's routine is running with,
// and whether it is enabled at all.
func (s *Scheduler) ServiceConfig(serviceID int64) (config.ServiceConfig, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, ok := s.enabled[serviceID]
	return svc.cfg, ok
}

// setRoundStatus records that round, which began at start, has been recorded.
// A round that overran its interval is followed immediately, so the next one
// is due no earlier than now.
func (s *Scheduler) setRoundStatus(round int, start time.Time) {
	next := start.Add(s.roundInterval)
	if now := time.Now(); next.Before(now) {
		next = now
	}
	s.statusMu.Lock()
	s.lastRound = round
	s.nextRoundAt = next
	s.statusMu.Unlock()
}

// StartService is idempotent: if serviceID is already enabled, it's a no-op.
// Otherwise it loads this service's entry fresh from the config file, looks
// up its check_type in the registry, and enables it from the next round on —
// never the round already in progress.
func (s *Scheduler) StartService(serviceID int64, name string) error {
	unlock := s.lockService(serviceID)
	defer unlock()
	return s.start(serviceID, name)
}

// StopService is idempotent. The service is left out of every round from
// now on, including the one in progress: its check, if running, is cancelled
// and its result discarded. It does not wait for that check to return, but
// if the round loop is just recording a round that already ended, it waits
// for that, so once StopService returns no further result for the service
// is recorded.
func (s *Scheduler) StopService(serviceID int64) {
	unlock := s.lockService(serviceID)
	defer unlock()
	s.stop(serviceID)
}

// RestartService stops the service (if running) then starts it, always
// re-reading the config file — this is how a config edit takes effect, from
// the next round on. If the fresh config cannot be loaded, the service is left
// stopped and the error is returned.
func (s *Scheduler) RestartService(serviceID int64, name string) error {
	unlock := s.lockService(serviceID)
	defer unlock()
	s.stop(serviceID)
	return s.start(serviceID, name)
}

// IsRunning reports whether serviceID is enabled, i.e. will be checked in the
// next round.
func (s *Scheduler) IsRunning(serviceID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.enabled[serviceID]
	return ok
}

// IsChecking reports whether serviceID's check is part of the round in
// progress right now (it is in inRound from beginRound until endRound).
func (s *Scheduler) IsChecking(serviceID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.inRound[serviceID]
	return ok
}

// RunningServiceIDs returns the IDs of all enabled services, in ascending
// order.
func (s *Scheduler) RunningServiceIDs() []int64 {
	s.mu.Lock()
	ids := make([]int64, 0, len(s.enabled))
	for id := range s.enabled {
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

// start enables serviceID unless it is already enabled. The caller must hold
// serviceID's operation lock.
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
	s.mu.Lock()
	s.enabled[serviceID] = enabledService{name: name, cfg: *cfg, fn: fn}
	s.mu.Unlock()
	return nil
}

// stop disables serviceID and drops it from the round in progress, if it is
// part of it, then waits out any round being recorded. The caller must hold
// serviceID's operation lock.
func (s *Scheduler) stop(serviceID int64) {
	s.mu.Lock()
	delete(s.enabled, serviceID)
	cancel, inRound := s.inRound[serviceID]
	delete(s.inRound, serviceID)
	s.mu.Unlock()
	if inRound {
		cancel()
	}
	// A round that ended before the stop may hold this service's result and
	// be recording it right now; that result is legitimately the service's,
	// but it must land before StopService returns, not after.
	s.recordMu.Lock()
	s.recordMu.Unlock()
}

// RunCheck runs fn under a deadline of timeout and returns its result, or a
// failure if fn panics, runs past the deadline, or does not return within
// checkGrace of the deadline. It always returns by then, which is what lets
// the round loop wait for every check of a round. scoringengine check runs
// checks by hand through it too, so they get the answer a round would.
//
// fn runs on a goroutine of its own so that RunCheck can stop waiting for it.
// A CheckFunc that ignores its ctx (which checks.CheckFunc forbids) therefore
// leaks that goroutine, once per round it overruns, until it returns on its
// own; no shipped check does.
func RunCheck(ctx context.Context, timeout time.Duration, fn checks.CheckFunc, host string, params map[string]any) checks.Result {
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Buffered, so a check that returns after RunCheck has given up can
	// still send and exit.
	done := make(chan checks.Result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- checks.Result{Success: false, Detail: fmt.Sprintf("check panicked: %v", p)}
			}
		}()
		done <- fn(checkCtx, host, params)
	}()

	select {
	case result := <-done:
		return withTimeoutDetail(checkCtx, timeout, result)
	case <-checkCtx.Done():
		select {
		case result := <-done:
			return withTimeoutDetail(checkCtx, timeout, result)
		case <-time.After(checkGrace):
			return checks.Result{
				Success: false,
				Detail:  fmt.Sprintf("check did not return within %s of its %s deadline", checkGrace, timeout),
			}
		}
	}
}

// withTimeoutDetail turns result into a failure if checkCtx's deadline
// passed, whatever the check claimed: a check that only answered after its
// deadline did not answer in time.
func withTimeoutDetail(checkCtx context.Context, timeout time.Duration, result checks.Result) checks.Result {
	if !errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
		return result
	}
	detail := fmt.Sprintf("check timed out after %s", timeout)
	if result.Detail != "" {
		detail += ": " + result.Detail
	}
	return checks.Result{Success: false, Detail: detail}
}

// waitForNextRound waits for d, or less if StartRoundNow is called, and
// reports false once ctx is done. A non-positive d returns immediately.
func (s *Scheduler) waitForNextRound(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.roundNow:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}
