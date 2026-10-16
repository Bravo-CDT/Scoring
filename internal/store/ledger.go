package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	sqlitedrv "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrDuplicateSourceRef reports that a ledger entry already exists for the
// (session, source type, source ref) being recorded. A retried or replayed
// scoring source produces it, so callers treat it as "already scored" rather
// than as a failure. Test for it with errors.Is.
var ErrDuplicateSourceRef = errors.New("store: ledger entry already recorded for this source")

// The two sides of a competition. Only blue starts with points (the session's
// starting score); red holds only what it has taken from blue.
const (
	sideBlue = "blue"
	sideRed  = "red"
)

// LedgerEntry is one point transfer between sides. The ledger is append-only
// and is the only record of score: a side's balance is always derived by
// summing its non-voided entries, never stored.
//
// NominalPoints is the rate the source asked to transfer (a service's
// points_per_failure, say) and is recorded as given. Points is what actually
// moved: NominalPoints floored at the sender's balance at the time, and never
// negative. The two differ once the sender runs dry.
type LedgerEntry struct {
	ID            int64
	SessionID     int64
	Timestamp     time.Time
	SourceType    string
	SourceRef     int64
	FromSide      string
	ToSide        string
	NominalPoints int
	Points        int
	Voided        bool
	Reason        string
}

// rowQuerier is the QueryRow method *sql.DB and *sql.Tx have in common, so the
// balance query can run inside RecordLedgerEntry's transaction or on its own.
type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// RecordLedgerEntry transfers up to nominalPoints from fromSide to toSide,
// flooring the transfer at fromSide's current balance (it never moves more
// than fromSide has, and never a negative amount), and returns the entry it
// stored.
//
// The balance read and the insert run in one transaction. The Store's single
// connection (see the package comment) is held by that transaction until it
// ends, so no other goroutine's statement can run between the read and the
// insert: two sources failing in the same instant can never both spend the
// same pre-transfer balance. For the same reason every statement between Begin
// and Commit must go through tx — a call on s.db there would wait forever for
// the connection the transaction is holding.
//
// A second entry for the same (sessionID, sourceType, sourceRef) is rejected
// with an error wrapping ErrDuplicateSourceRef and changes nothing.
//
// fromSide and toSide are stored as given; GetScore only reports "blue" and
// "red".
func (s *Store) RecordLedgerEntry(sessionID int64, sourceType string, sourceRef int64, fromSide, toSide string, nominalPoints int, reason string) (LedgerEntry, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("store: beginning ledger transaction: %w", err)
	}
	// Rollback after a successful Commit is a harmless no-op (sql.ErrTxDone).
	defer tx.Rollback()

	startingScore, err := sessionStartingScore(tx, sessionID)
	if err != nil {
		return LedgerEntry{}, err
	}
	balance, err := sideBalance(tx, sessionID, fromSide, startingScore)
	if err != nil {
		return LedgerEntry{}, err
	}
	points := max(0, min(nominalPoints, balance))

	now := time.Now().UTC().Truncate(time.Millisecond)
	res, err := tx.Exec(
		`INSERT INTO ledger_entries
		 (session_id, timestamp, source_type, source_ref, from_side, to_side, nominal_points, points, voided, reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, FALSE, ?)`,
		sessionID, now.UnixMilli(), sourceType, sourceRef, fromSide, toSide, nominalPoints, points, reason,
	)
	if isUniqueViolation(err) {
		return LedgerEntry{}, fmt.Errorf("%w: session %d, %s %d",
			ErrDuplicateSourceRef, sessionID, sourceType, sourceRef)
	}
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("store: inserting ledger entry for %s %d: %w", sourceType, sourceRef, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("store: reading new ledger entry id for %s %d: %w", sourceType, sourceRef, err)
	}
	if err := tx.Commit(); err != nil {
		return LedgerEntry{}, fmt.Errorf("store: committing ledger entry for %s %d: %w", sourceType, sourceRef, err)
	}

	return LedgerEntry{
		ID:            id,
		SessionID:     sessionID,
		Timestamp:     now,
		SourceType:    sourceType,
		SourceRef:     sourceRef,
		FromSide:      fromSide,
		ToSide:        toSide,
		NominalPoints: nominalPoints,
		Points:        points,
		Reason:        reason,
	}, nil
}

// GetScore returns each side's current balance: the session's starting score
// (blue only) adjusted by every non-voided ledger entry into or out of that
// side. It sums both directions per side, so it needs no change when sources
// that move points from red to blue are added.
//
// An unknown session yields an error wrapping sql.ErrNoRows.
func (s *Store) GetScore(sessionID int64) (blue, red int, err error) {
	startingScore, err := sessionStartingScore(s.db, sessionID)
	if err != nil {
		return 0, 0, err
	}
	if blue, err = sideBalance(s.db, sessionID, sideBlue, startingScore); err != nil {
		return 0, 0, err
	}
	if red, err = sideBalance(s.db, sessionID, sideRed, startingScore); err != nil {
		return 0, 0, err
	}
	return blue, red, nil
}

// sessionStartingScore loads the session's persisted starting score. The
// error wraps sql.ErrNoRows when there is no such session.
func sessionStartingScore(q rowQuerier, sessionID int64) (int, error) {
	var startingScore int
	err := q.QueryRow(`SELECT starting_score FROM sessions WHERE id = ?`, sessionID).Scan(&startingScore)
	if err != nil {
		return 0, fmt.Errorf("store: loading starting score of session %d: %w", sessionID, err)
	}
	return startingScore, nil
}

// sideBalance returns side's balance in the session: the starting score if
// side is blue, plus every non-voided transfer into it, minus every one out of
// it. Each row's two terms are computed independently, so a row whose from and
// to sides are the same nets to zero rather than counting one way.
func sideBalance(q rowQuerier, sessionID int64, side string, startingScore int) (int, error) {
	var net int
	err := q.QueryRow(
		`SELECT COALESCE(SUM(
		     (CASE WHEN to_side   = ? THEN points ELSE 0 END)
		   - (CASE WHEN from_side = ? THEN points ELSE 0 END)
		 ), 0)
		 FROM ledger_entries
		 WHERE session_id = ? AND voided = FALSE`,
		side, side, sessionID,
	).Scan(&net)
	if err != nil {
		return 0, fmt.Errorf("store: summing ledger for %s in session %d: %w", side, sessionID, err)
	}
	if side == sideBlue {
		net += startingScore
	}
	return net, nil
}

// isUniqueViolation reports whether err is SQLite rejecting a row under a
// UNIQUE constraint. On ledger_entries the only such constraint is
// idx_ledger_source, so there it means a duplicate source. Checking the
// driver's result code rather than the message text keeps this independent of
// how the driver words its errors.
func isUniqueViolation(err error) bool {
	var sqliteErr *sqlitedrv.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}
