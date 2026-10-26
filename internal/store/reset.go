package store

import "fmt"

// ResetRoundData deletes every check_results, ledger_entries,
// credential_overrides and inject_submissions row for sessionID -- this
// competition's score and check-result history, the blue team's credential
// changes, and the teams' inject submissions -- and puts every inject back to
// closed and ungraded. Events, sessions, services, service_config_snapshots
// and the injects themselves are kept. GetScore and the check-result queries
// derive entirely from the first two tables, so both read fresh afterward
// with no further change needed.
//
// It returns the stored names of the submission files whose rows it deleted,
// for the caller to remove from disk.
func (s *Store) ResetRoundData(sessionID int64) (deletedFiles []string, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: beginning round-reset transaction: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT stored_name FROM inject_submissions WHERE session_id = ?`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: listing submissions for session %d: %w", sessionID, err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: listing submissions for session %d: %w", sessionID, err)
		}
		deletedFiles = append(deletedFiles, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing submissions for session %d: %w", sessionID, err)
	}
	if _, err := tx.Exec(`DELETE FROM inject_submissions WHERE session_id = ?`, sessionID); err != nil {
		return nil, fmt.Errorf("store: resetting submissions for session %d: %w", sessionID, err)
	}
	if _, err := tx.Exec(`UPDATE injects SET open = 0, grade = NULL, points = NULL, approved_at = NULL WHERE session_id = ?`, sessionID); err != nil {
		return nil, fmt.Errorf("store: resetting injects for session %d: %w", sessionID, err)
	}

	if _, err := tx.Exec(`DELETE FROM ledger_entries WHERE session_id = ?`, sessionID); err != nil {
		return nil, fmt.Errorf("store: resetting ledger entries for session %d: %w", sessionID, err)
	}
	if _, err := tx.Exec(`DELETE FROM check_results WHERE session_id = ?`, sessionID); err != nil {
		return nil, fmt.Errorf("store: resetting check results for session %d: %w", sessionID, err)
	}
	if _, err := tx.Exec(`DELETE FROM credential_overrides WHERE session_id = ?`, sessionID); err != nil {
		return nil, fmt.Errorf("store: resetting credential overrides for session %d: %w", sessionID, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: committing round reset for session %d: %w", sessionID, err)
	}
	return deletedFiles, nil
}
