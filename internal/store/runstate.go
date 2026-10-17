package store

import "fmt"

// Engine states stored in engine_run_state.engine_state.
const (
	EngineStopped = "stopped"
	EngineRunning = "running"
)

// RunState is the engine's persisted run state: whether the check routines are
// meant to be running, and whether scoring is globally paused (which keeps the
// checks running but stops them moving points).
type RunState struct {
	EngineState   string // EngineStopped | EngineRunning
	ScoringPaused bool
}

// GetRunState reads the current run state.
func (s *Store) GetRunState() (RunState, error) {
	var rs RunState
	var paused bool
	row := s.db.QueryRow(`SELECT engine_state, scoring_paused FROM engine_run_state WHERE id = 1`)
	if err := row.Scan(&rs.EngineState, &paused); err != nil {
		return RunState{}, fmt.Errorf("store: reading run state: %w", err)
	}
	rs.ScoringPaused = paused
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

// resetRunStateToSafeDefault forces the singleton run-state row to
// stopped/paused, creating it if this is a new database. Open calls it so a
// restart after a crash cannot silently resume scoring.
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
