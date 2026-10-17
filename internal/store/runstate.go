package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Engine states stored in engine_run_state.engine_state.
const (
	EngineStopped = "stopped"
	EngineRunning = "running"
)

// RunState is the engine's persisted run state: whether the check routines are
// meant to be running, and whether scoring is globally paused (which keeps the
// checks running but stops them moving points).
//
// CountdownTarget is the admin-set time a countdown runs to (stored as Unix
// milliseconds, read back in UTC), or nil when none is set. Unlike the other
// two fields it is not reset by Open: it is a schedule, not a claim about
// what the engine was doing when it last stopped.
type RunState struct {
	EngineState     string // EngineStopped | EngineRunning
	ScoringPaused   bool
	CountdownTarget *time.Time
}

// GetRunState reads the current run state.
func (s *Store) GetRunState() (RunState, error) {
	var rs RunState
	var paused bool
	var countdownMillis sql.NullInt64
	row := s.db.QueryRow(`SELECT engine_state, scoring_paused, countdown_target FROM engine_run_state WHERE id = 1`)
	if err := row.Scan(&rs.EngineState, &paused, &countdownMillis); err != nil {
		return RunState{}, fmt.Errorf("store: reading run state: %w", err)
	}
	rs.ScoringPaused = paused
	if countdownMillis.Valid {
		target := time.UnixMilli(countdownMillis.Int64).UTC()
		rs.CountdownTarget = &target
	}
	return rs, nil
}

// SetEngineState records whether the engine is running or stopped. It leaves
// the global scoring-paused flag alone.
func (s *Store) SetEngineState(state string) error {
	if _, err := s.db.Exec(`UPDATE engine_run_state SET engine_state = ? WHERE id = 1`, state); err != nil {
		return fmt.Errorf("store: setting engine_state to %q: %w", state, err)
	}
	return nil
}

// SetGlobalScoringPaused records whether scoring is globally paused. It leaves
// the engine state alone.
func (s *Store) SetGlobalScoringPaused(paused bool) error {
	if _, err := s.db.Exec(`UPDATE engine_run_state SET scoring_paused = ? WHERE id = 1`, paused); err != nil {
		return fmt.Errorf("store: setting global scoring_paused to %t: %w", paused, err)
	}
	return nil
}

// SetCountdownTarget records the time the countdown runs to; nil clears it. It
// leaves the engine state and scoring-paused flag alone.
func (s *Store) SetCountdownTarget(target *time.Time) error {
	var millis sql.NullInt64
	if target != nil {
		millis = sql.NullInt64{Int64: target.UnixMilli(), Valid: true}
	}
	if _, err := s.db.Exec(`UPDATE engine_run_state SET countdown_target = ? WHERE id = 1`, millis); err != nil {
		return fmt.Errorf("store: setting countdown target: %w", err)
	}
	return nil
}

// resetRunStateToSafeDefault forces the singleton run-state row to
// stopped/paused, creating it if this is a new database. Open calls it so a
// restart after a crash cannot silently resume scoring. The upsert names only
// those two columns, so it leaves countdown_target as it was.
func (s *Store) resetRunStateToSafeDefault() error {
	_, err := s.db.Exec(`
		INSERT INTO engine_run_state (id, engine_state, scoring_paused)
		VALUES (1, ?, TRUE)
		ON CONFLICT(id) DO UPDATE SET engine_state = excluded.engine_state,
		                              scoring_paused = excluded.scoring_paused
	`, EngineStopped)
	if err != nil {
		return fmt.Errorf("store: forcing safe-default run state: %w", err)
	}
	return nil
}
