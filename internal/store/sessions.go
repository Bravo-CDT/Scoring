package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Session is one competition run. Everything the engine scores hangs off a
// session, so restarting the engine with the same session_name continues the
// same competition rather than starting a new one.
type Session struct {
	ID            int64
	Name          string
	StartingScore int
	CreatedAt     time.Time
}

// GetOrCreateSession returns the session called name, creating it with
// startingScore if it does not exist yet.
//
// For an existing session the persisted StartingScore is returned unchanged and
// startingScore is ignored here; the engine applies an edited starting_score
// explicitly, and logs it, through SetSessionStartingScore on reload.
func (s *Store) GetOrCreateSession(name string, startingScore int) (Session, error) {
	sess, err := s.findSession(name)
	if err == nil {
		return sess, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Session{}, err
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	res, err := s.db.Exec(
		`INSERT INTO sessions (name, starting_score, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(name) DO NOTHING`,
		name, startingScore, now.UnixMilli(),
	)
	if err != nil {
		return Session{}, fmt.Errorf("store: creating session %q: %w", name, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Session{}, fmt.Errorf("store: creating session %q: %w", name, err)
	}
	if affected == 0 {
		// A concurrent caller created it first; DO NOTHING turns that into zero
		// affected rows rather than a unique-constraint error, so read the winner.
		sess, err := s.findSession(name)
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, fmt.Errorf("store: session %q was neither created nor found", name)
		}
		return sess, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Session{}, fmt.Errorf("store: reading new session id for %q: %w", name, err)
	}
	return Session{ID: id, Name: name, StartingScore: startingScore, CreatedAt: now}, nil
}

// findSession loads the session called name. The returned error is
// sql.ErrNoRows (unwrapped) when there is no such session, so callers can tell
// "absent" apart from "query failed".
func (s *Store) findSession(name string) (Session, error) {
	var sess Session
	var createdAtMillis int64
	row := s.db.QueryRow(
		`SELECT id, name, starting_score, created_at FROM sessions WHERE name = ?`,
		name,
	)
	switch err := row.Scan(&sess.ID, &sess.Name, &sess.StartingScore, &createdAtMillis); {
	case errors.Is(err, sql.ErrNoRows):
		return Session{}, sql.ErrNoRows
	case err != nil:
		return Session{}, fmt.Errorf("store: looking up session %q: %w", name, err)
	}
	sess.CreatedAt = time.UnixMilli(createdAtMillis).UTC()
	return sess, nil
}

// SessionStartingScore returns the session's current starting score.
func (s *Store) SessionStartingScore(sessionID int64) (int, error) {
	return sessionStartingScore(s.db, sessionID)
}

// SetSessionStartingScore changes the session's starting score. Blue's
// balance is the starting score plus the ledger, so it moves by the
// difference at once; red's is unaffected.
func (s *Store) SetSessionStartingScore(sessionID int64, startingScore int) error {
	if _, err := s.db.Exec(`UPDATE sessions SET starting_score = ? WHERE id = ?`, startingScore, sessionID); err != nil {
		return fmt.Errorf("store: setting starting score of session %d: %w", sessionID, err)
	}
	return nil
}
