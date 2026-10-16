// Package scorer turns check results into score. It records every result,
// logs a service's up/down transitions, moves points from blue to red for each
// failure while scoring is not paused, and auto-pauses all scoring when too
// many services fail at once (which points at the scoring infrastructure, not
// the blue team).
package scorer

import (
	"errors"
	"fmt"
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

	// SourceServiceCheck is the ledger source type of a failed check; the
	// entry's source ref is the check_results row id.
	SourceServiceCheck = "service_check"
)

// Scorer applies the scoring rules to check results for one session. It is
// safe for concurrent use by the Scheduler's per-service routines. It holds no
// scoring state of its own: the pause flags and previous results are read
// from the store on every call, so admin changes take effect on the next
// result.
//
// Record assumes results for any one service arrive one at a time, as the
// Scheduler guarantees; transition detection compares against the service's
// previous stored result.
type Scorer struct {
	store               *store.Store
	sessionID           int64
	massFailureFraction *float64

	// massMu serializes the mass-failure check-then-pause, so services that
	// cross the threshold together produce one autopause event, not one each.
	massMu sync.Mutex
}

// New returns a Scorer for sessionID. massFailureFraction is the share of
// active services (0 to 1) that must be failing at once to auto-pause scoring;
// nil disables the safety net.
func New(st *store.Store, sessionID int64, massFailureFraction *float64) *Scorer {
	return &Scorer{store: st, sessionID: sessionID, massFailureFraction: massFailureFraction}
}

// Record stores one check result for serviceID and applies the scoring rules
// to it. It satisfies scheduler.ResultSink.
//
// Once the result itself is stored, the remaining steps (transition event,
// ledger entry, mass-failure check) are each attempted even if an earlier one
// fails, so a failed event write never costs a score; their errors are
// returned joined.
func (sc *Scorer) Record(serviceID int64, pointsPerFailure int, result checks.Result, latency time.Duration) error {
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
		Timestamp: time.Now().UTC(),
		Success:   result.Success,
		LatencyMS: latency.Milliseconds(),
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
		errs = append(errs, sc.scoreFailure(svc, resultID, pointsPerFailure))
		errs = append(errs, sc.checkMassFailure())
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

// checkMassFailure pauses all scoring, persistently, once the configured
// fraction of active services is failing, and logs one warning when it does.
// It does nothing while scoring is already paused, so the warning is never
// repeated and an admin's own pause is left alone.
func (sc *Scorer) checkMassFailure() error {
	if sc.massFailureFraction == nil {
		return nil
	}
	threshold := *sc.massFailureFraction

	sc.massMu.Lock()
	defer sc.massMu.Unlock()

	rs, err := sc.store.GetRunState()
	if err != nil {
		return fmt.Errorf("scorer: loading run state for mass-failure check: %w", err)
	}
	if rs.ScoringPaused {
		return nil
	}
	active, failing, err := sc.store.CountActiveServiceOutcomes(sc.sessionID)
	if err != nil {
		return fmt.Errorf("scorer: counting failing services: %w", err)
	}
	if active == 0 {
		return nil
	}
	share := float64(failing) / float64(active)
	if share < threshold {
		return nil
	}

	if err := sc.store.SetGlobalScoringPaused(true); err != nil {
		return fmt.Errorf("scorer: auto-pausing scoring: %w", err)
	}
	err = sc.store.InsertEvent(store.Event{
		SessionID: sc.sessionID,
		Level:     "warning",
		Actor:     "system",
		Type:      EventMassFailureAutopause,
		Message: fmt.Sprintf("%d of %d active services failing (threshold %.0f%%); scoring auto-paused",
			failing, active, threshold*100),
	})
	if err != nil {
		return fmt.Errorf("scorer: recording mass-failure autopause event: %w", err)
	}
	return nil
}
