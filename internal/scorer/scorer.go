// Package scorer turns check results into score. It records every result,
// logs a service's up/down transitions, moves points from blue to red for each
// failure while scoring is not paused.
package scorer

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/checks"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

// Event types and source type the Scorer writes.
const (
	EventServiceUp   = "service_up"
	EventServiceDown = "service_down"
	// EventPersistenceError reports that recording or scoring a check result
	// failed, so the result or its points may be missing.
	EventPersistenceError = "persistence_error"

	// SourceServiceCheck is the ledger source type of a failed check; the
	// entry's source ref is the check_results row id.
	SourceServiceCheck = "service_check"
)

// Scorer applies the scoring rules to check results for one session, one
// scoring round at a time. The pause flags and previous results are read from
// the store on every call, so admin changes take effect on the next round.
//
// RecordRound assumes rounds arrive one at a time, as the Scheduler's single
// round loop guarantees: transition detection compares each result against
// its service's previous stored result.
type Scorer struct {
	store     *store.Store
	sessionID int64
	logger    *log.Logger
}

// Option configures a Scorer.
type Option func(*Scorer)

// WithLogger sets where the Scorer logs failures it cannot report any other
// way. The default is the standard logger.
func WithLogger(logger *log.Logger) Option {
	return func(sc *Scorer) { sc.logger = logger }
}

// New returns a Scorer for sessionID.
func New(st *store.Store, sessionID int64, opts ...Option) *Scorer {
	sc := &Scorer{store: st, sessionID: sessionID, logger: log.Default()}
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
