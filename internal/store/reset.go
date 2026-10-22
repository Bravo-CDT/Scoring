package store

import "fmt"

// ResetRoundData deletes every check_results and ledger_entries row for
// sessionID -- this competition's score and check-result history -- leaving
// events, sessions, services, and service_config_snapshots completely
// untouched. GetScore and the check-result queries derive entirely from
// these two tables, so both read fresh afterward with no further change
// needed.
func (s *Store) ResetRoundData(sessionID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: beginning round-reset transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM ledger_entries WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("store: resetting ledger entries for session %d: %w", sessionID, err)
	}
	if _, err := tx.Exec(`DELETE FROM check_results WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("store: resetting check results for session %d: %w", sessionID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing round reset for session %d: %w", sessionID, err)
	}
	return nil
}
