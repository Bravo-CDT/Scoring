// Package scorer turns check results into score. It records every result,
// logs a service's up/down transitions, moves points from blue to red for each
// failure while scoring is not paused, and auto-pauses all scoring when too
// many services start failing at once (which points at the scoring
// infrastructure, not the blue team).
package scorer

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

// Event types and source type the Scorer writes.
const (
	EventServiceUp            = "service_up"
	EventServiceDown          = "service_down"
	EventMassFailureAutopause = "mass_failure_autopause"
	// EventPersistenceError reports that recording or scoring a check result
	// failed, so the result or its points may be missing.
	EventPersistenceError = "persistence_error"

	// SourceServiceCheck is the ledger source type of a failed check; the
	// entry's source ref is the check_results row id.
	SourceServiceCheck = "service_check"
)

// Scorer applies the scoring rules to check results for one session, one
// scoring round at a time. The pause flags and previous results are read from
// the store on every call, so admin changes take effect on the next round; the
// only state it keeps itself is whether the mass-failure safety net is armed
// (see checkMassFailure).
//
// RecordRound assumes rounds arrive one at a time, as the Scheduler's single
// round loop guarantees: transition detection compares each result against
// its service's previous stored result.
type Scorer struct {
	store               *store.Store
	sessionID           int64
	massFailureFraction *float64
	// running reports whether a service is enabled in the Scheduler; nil
	// means every active service counts for the mass-failure check.
	running func(serviceID int64) bool
	logger  *log.Logger

	// massMu serializes the mass-failure evaluation and guards armed. The
	// Scheduler never overlaps RecordRound calls, but the safety net must not
	// double-fire even for a caller that does.
	massMu sync.Mutex
	// armed is set when an evaluation sees the failing share below the
	// threshold and cleared when the autopause fires: the safety net fires on
	// a crossing, not on every failure above the threshold.
	armed bool
}

// Option configures a Scorer.
type Option func(*Scorer)

// WithRunning restricts the mass-failure check to services the Scheduler is
// running, as reported by running (the Scheduler's IsRunning). A stopped
// service's last result is stale, so without this a service an admin stopped
// while it was down would count as failing for the rest of the competition.
// running must not call back into the Scorer.
func WithRunning(running func(serviceID int64) bool) Option {
	return func(sc *Scorer) { sc.running = running }
}

// WithLogger sets where the Scorer logs failures it cannot report any other
// way. The default is the standard logger.
func WithLogger(logger *log.Logger) Option {
	return func(sc *Scorer) { sc.logger = logger }
}

// New returns a Scorer for sessionID. massFailureFraction is the share of
// active services (0 to 1) that must be failing at once to auto-pause scoring;
// nil disables the safety net.
func New(st *store.Store, sessionID int64, massFailureFraction *float64, opts ...Option) *Scorer {
	sc := &Scorer{store: st, sessionID: sessionID, massFailureFraction: massFailureFraction, logger: log.Default()}
	for _, opt := range opts {
		opt(sc)
	}
	return sc
}

// RecordRound stores every result of one scoring round and applies the
// scoring rules to them. It satisfies scheduler.ResultSink.
//
// Every result is stored with round as its round number and roundStart as its
// timestamp, so all of a round's rows share one instant. Each result is
// recorded and scored on its own: one that fails does not stop the others.
// Within a result, once the result itself is stored, the transition event and
// ledger entry are each attempted even if the other fails, so a failed event
// write never costs a score.
//
// The mass-failure safety net is evaluated once, after the whole round is
// recorded, against the round's final state. So every failure of the round
// that crosses the threshold is scored, and the pause takes effect from the
// next round on.
//
// The Scheduler has nothing useful to do with an error, so RecordRound also
// reports every per-result one itself: it logs it, then makes a best-effort
// persistence_error event (which a full disk may refuse too; that is logged
// and left there). All errors are returned joined.
func (sc *Scorer) RecordRound(round int, roundStart time.Time, results []checks.RoundResult) error {
	var errs []error
	for _, r := range results {
		if err := sc.recordOne(round, roundStart, r); err != nil {
			sc.reportFailure(r.ServiceID, err)
			errs = append(errs, err)
		}
	}
	// Passing rounds are evaluated too: seeing the share below the threshold
	// is what arms the safety net.
	if err := sc.checkMassFailure(); err != nil {
		sc.logger.Printf("scorer: round %d: %v", round, err)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// reportFailure logs err, a failed recording of serviceID's result, and tries
// to add it to the event log. The event's message is fixed text because events
// are public; the error, which can name internals, goes in the detail.
func (sc *Scorer) reportFailure(serviceID int64, err error) {
	sc.logger.Printf("scorer: recording result for service %d: %v", serviceID, err)
	evErr := sc.store.InsertEvent(store.Event{
		SessionID: sc.sessionID,
		ServiceID: &serviceID,
		Level:     "error",
		Actor:     "system",
		Type:      EventPersistenceError,
		Message:   "failed to record a check result; its result or points may be missing",
		Detail:    err.Error(),
	})
	if evErr != nil {
		sc.logger.Printf("scorer: also failed to record the %s event for service %d: %v",
			EventPersistenceError, serviceID, evErr)
	}
}

// recordOne stores r as serviceID's result for round and scores it.
func (sc *Scorer) recordOne(round int, roundStart time.Time, r checks.RoundResult) error {
	serviceID, result := r.ServiceID, r.Result
	svc, err := sc.store.GetService(serviceID)
	if err != nil {
		return fmt.Errorf("scorer: loading service %d: %w", serviceID, err)
	}
	prev, err := sc.store.GetLatestCheckResult(serviceID)
	if err != nil {
		return fmt.Errorf("scorer: loading previous result of %s: %w", svc.Name, err)
	}

	resultID, err := sc.store.InsertCheckResult(store.CheckResult{
		SessionID: sc.sessionID,
		ServiceID: serviceID,
		Round:     round,
		Timestamp: roundStart.UTC(),
		Success:   result.Success,
		LatencyMS: r.Latency.Milliseconds(),
		Detail:    result.Detail,
	})
	if err != nil {
		return fmt.Errorf("scorer: recording result of %s: %w", svc.Name, err)
	}

	var errs []error
	if prev == nil || prev.Success != result.Success {
		errs = append(errs, sc.recordTransition(svc, result))
	}
	if !result.Success {
		errs = append(errs, sc.scoreFailure(svc, resultID, r.PointsPerFailure))
	}
	return errors.Join(errs...)
}

// recordTransition logs svc going up or down. A service's first result counts
// as a transition, so every service's state appears in the log.
func (sc *Scorer) recordTransition(svc store.Service, result checks.Result) error {
	e := store.Event{
		SessionID: sc.sessionID,
		ServiceID: &svc.ID,
		Level:     "info",
		Actor:     "system",
		Type:      EventServiceUp,
		Message:   svc.Name + " is up",
	}
	if !result.Success {
		e.Type = EventServiceDown
		e.Message = svc.Name + " is down"
		e.Detail = result.Detail
	}
	if err := sc.store.InsertEvent(e); err != nil {
		return fmt.Errorf("scorer: recording %s event for %s: %w", e.Type, svc.Name, err)
	}
	return nil
}

// scoreFailure moves pointsPerFailure from blue to red for the failed result
// resultID, unless scoring is paused globally or for svc. svc was just read
// from the store, so its pause flag is current; the global flag is read here.
func (sc *Scorer) scoreFailure(svc store.Service, resultID int64, pointsPerFailure int) error {
	if svc.ScoringPaused {
		return nil
	}
	rs, err := sc.store.GetRunState()
	if err != nil {
		return fmt.Errorf("scorer: loading run state to score %s: %w", svc.Name, err)
	}
	if rs.ScoringPaused {
		return nil
	}
	// The reason is fixed text: the check's untrusted detail stays in the
	// check result the entry cites.
	_, err = sc.store.RecordLedgerEntry(sc.sessionID, SourceServiceCheck, resultID,
		store.SideBlue, store.SideRed, pointsPerFailure, svc.Name+" check failed")
	if err != nil && !errors.Is(err, store.ErrDuplicateSourceRef) {
		return fmt.Errorf("scorer: scoring failure of %s: %w", svc.Name, err)
	}
	return nil
}

// checkMassFailure pauses all scoring, persistently, when the failing share of
// services crosses the configured fraction, and logs one warning when it does.
//
// It is edge-triggered. Every evaluation that sees the share below the
// threshold arms it; it fires only when armed and the share has reached the
// threshold, and firing disarms it. So once an admin has looked at an
// autopause and resumed scoring (judging the failures genuine red-team work),
// the same ongoing mass failure does not pause again; only a recovery below
// the threshold followed by a new crossing does. A new Scorer starts disarmed,
// so a restart in the middle of a mass failure (when the store forces scoring
// paused and an admin must resume it) does not count as a crossing either.
//
// A crossing seen while scoring is already paused (an admin's manual pause)
// is consumed: it disarms without an event or any state change. The admin's
// pause is left alone, and an admin who resumes during that same mass failure
// has made the same call as one resuming after an autopause, so it is not
// overridden either.
func (sc *Scorer) checkMassFailure() error {
	if sc.massFailureFraction == nil {
		return nil
	}
	threshold := *sc.massFailureFraction

	sc.massMu.Lock()
	defer sc.massMu.Unlock()

	active, failing, err := sc.countOutcomes()
	if err != nil {
		return err
	}
	if active == 0 {
		return nil
	}
	if share := float64(failing) / float64(active); share < threshold {
		sc.armed = true
		return nil
	}
	if !sc.armed {
		return nil
	}

	rs, err := sc.store.GetRunState()
	if err != nil {
		return fmt.Errorf("scorer: loading run state for mass-failure check: %w", err)
	}
	if rs.ScoringPaused {
		sc.armed = false
		return nil
	}
	if err := sc.store.SetGlobalScoringPaused(true); err != nil {
		return fmt.Errorf("scorer: auto-pausing scoring: %w", err)
	}
	sc.armed = false
	err = sc.store.InsertEvent(store.Event{
		SessionID: sc.sessionID,
		Level:     "warning",
		Actor:     "system",
		Type:      EventMassFailureAutopause,
		Message: fmt.Sprintf("%d of %d checked services failing (threshold %.0f%%); scoring auto-paused",
			failing, active, threshold*100),
	})
	if err != nil {
		return fmt.Errorf("scorer: recording mass-failure autopause event: %w", err)
	}
	return nil
}

// countOutcomes returns how many services the mass-failure check considers —
// active ones the Scheduler is running, or every active one without a running
// callback — and how many of those are failing. A service with no result yet
// is counted and is not failing.
func (sc *Scorer) countOutcomes() (considered, failing int, err error) {
	outcomes, err := sc.store.ActiveServiceOutcomes(sc.sessionID)
	if err != nil {
		return 0, 0, fmt.Errorf("scorer: loading service outcomes for mass-failure check: %w", err)
	}
	for _, o := range outcomes {
		if sc.running != nil && !sc.running(o.ServiceID) {
			continue
		}
		considered++
		if o.Failing {
			failing++
		}
	}
	return considered, failing, nil
}
