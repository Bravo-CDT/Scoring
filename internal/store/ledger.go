package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
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
	SideBlue = "blue"
	SideRed  = "red"
)

// SideManual is the pseudo-side a manual score adjustment moves points
// to/from. GetScore only sums entries touching "blue" or "red", so an entry
// with "manual" on the other end moves only the named side's balance -- an
// independent correction, never a transfer between teams.
const SideManual = "manual"

// Ledger source types the store itself knows about. The scorer records
// SourceServiceCheck (it has its own copy of the name); AdjustScore records
// SourceManualAdjustment and ApproveInject SourceInject.
const (
	SourceServiceCheck     = "service_check"
	SourceManualAdjustment = "manual_adjustment"
	SourceInject           = "inject"
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
// balance helpers are not tied to RecordLedgerEntry's transaction.
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

	entry, err := recordLedgerEntryTx(tx, sessionID, sourceType, sourceRef, fromSide, toSide, nominalPoints, reason)
	if err != nil {
		return LedgerEntry{}, err
	}
	if err := tx.Commit(); err != nil {
		return LedgerEntry{}, fmt.Errorf("store: committing ledger entry for %s %d: %w", sourceType, sourceRef, err)
	}
	return entry, nil
}

// recordLedgerEntryTx is RecordLedgerEntry inside the caller's transaction, so
// a ledger entry can be written together with the row that caused it.
func recordLedgerEntryTx(tx *sql.Tx, sessionID int64, sourceType string, sourceRef int64, fromSide, toSide string, nominalPoints int, reason string) (LedgerEntry, error) {
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

// VoidLedgerEntry voids the entry recorded for (sourceType, sourceRef), taking
// its points back out of both sides' balances, and reports whether there was
// a non-voided entry to void.
func (s *Store) VoidLedgerEntry(sessionID int64, sourceType string, sourceRef int64, reason string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE ledger_entries SET voided = TRUE, voided_at = ?, voided_reason = ?
		 WHERE session_id = ? AND source_type = ? AND source_ref = ? AND voided = FALSE`,
		time.Now().UnixMilli(), reason, sessionID, sourceType, sourceRef)
	if err != nil {
		return false, fmt.Errorf("store: voiding ledger entry for %s %d: %w", sourceType, sourceRef, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: voiding ledger entry for %s %d: %w", sourceType, sourceRef, err)
	}
	return n > 0, nil
}

// SideBreakdown is one side's balance split by where the points came from.
// Its fields add up to the side's GetScore balance.
type SideBreakdown struct {
	Starting int // the session's starting score, blue only
	Services int // failed service checks
	Injects  int // approved injects
	Manual   int // manual adjustments
}

// ScoreBreakdown sums each side's non-voided ledger entries by source type.
func (s *Store) ScoreBreakdown(sessionID int64) (blue, red SideBreakdown, err error) {
	starting, err := sessionStartingScore(s.db, sessionID)
	if err != nil {
		return SideBreakdown{}, SideBreakdown{}, err
	}
	blue.Starting = starting
	rows, err := s.db.Query(
		`SELECT source_type,
		        COALESCE(SUM((CASE WHEN to_side = ? THEN points ELSE 0 END) - (CASE WHEN from_side = ? THEN points ELSE 0 END)), 0),
		        COALESCE(SUM((CASE WHEN to_side = ? THEN points ELSE 0 END) - (CASE WHEN from_side = ? THEN points ELSE 0 END)), 0)
		 FROM ledger_entries WHERE session_id = ? AND voided = FALSE
		 GROUP BY source_type`,
		SideBlue, SideBlue, SideRed, SideRed, sessionID)
	if err != nil {
		return SideBreakdown{}, SideBreakdown{}, fmt.Errorf("store: breaking down the score of session %d: %w", sessionID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var source string
		var b, r int
		if err := rows.Scan(&source, &b, &r); err != nil {
			return SideBreakdown{}, SideBreakdown{}, fmt.Errorf("store: breaking down the score of session %d: %w", sessionID, err)
		}
		switch source {
		case SourceServiceCheck:
			blue.Services, red.Services = blue.Services+b, red.Services+r
		case SourceInject:
			blue.Injects, red.Injects = blue.Injects+b, red.Injects+r
		default:
			blue.Manual, red.Manual = blue.Manual+b, red.Manual+r
		}
	}
	if err := rows.Err(); err != nil {
		return SideBreakdown{}, SideBreakdown{}, fmt.Errorf("store: breaking down the score of session %d: %w", sessionID, err)
	}
	return blue, red, nil
}

// GetScore returns each side's current balance: the session's starting score
// (blue only) adjusted by every non-voided ledger entry into or out of that
// side. It sums both directions per side, so it needs no change when sources
// that move points from red to blue are added.
//
// Both balances come from one SELECT, so they are read from the same snapshot:
// a transfer committed by another goroutine is either in both or in neither,
// and blue+red always equals the starting score. Two separate reads would let
// a transfer land between them on the shared connection.
//
// An unknown session yields an error wrapping sql.ErrNoRows.
func (s *Store) GetScore(sessionID int64) (blue, red int, err error) {
	err = s.db.QueryRow(
		`SELECT sv.starting_score + COALESCE(SUM(
		            (CASE WHEN le.to_side   = ? THEN le.points ELSE 0 END)
		          - (CASE WHEN le.from_side = ? THEN le.points ELSE 0 END)), 0),
		        COALESCE(SUM(
		            (CASE WHEN le.to_side   = ? THEN le.points ELSE 0 END)
		          - (CASE WHEN le.from_side = ? THEN le.points ELSE 0 END)), 0)
		 FROM sessions sv
		 LEFT JOIN ledger_entries le ON le.session_id = sv.id AND le.voided = FALSE
		 WHERE sv.id = ?
		 GROUP BY sv.id`,
		SideBlue, SideBlue, SideRed, SideRed, sessionID,
	).Scan(&blue, &red)
	if err != nil {
		return 0, 0, fmt.Errorf("store: loading score of session %d: %w", sessionID, err)
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
//
// A side other than blue or red (SideManual, say) is not one of the
// competition's two tracked balances: it is treated as an inexhaustible
// external source/sink, so RecordLedgerEntry's floor-at-sender-balance never
// floors a transfer out of it. Nothing it receives is ever read back either
// way -- GetScore only ever sums blue and red.
func sideBalance(q rowQuerier, sessionID int64, side string, startingScore int) (int, error) {
	if side != SideBlue && side != SideRed {
		return math.MaxInt, nil
	}
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
	if side == SideBlue {
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

// nextManualAdjustmentRef atomically reserves and returns the next manual
// adjustment reference, creating the counter row (starting at 1) on first
// use.
func (s *Store) nextManualAdjustmentRef() (int64, error) {
	var ref int64
	err := s.db.QueryRow(`
		INSERT INTO manual_adjustment_seq (id, next) VALUES (1, 1)
		ON CONFLICT(id) DO UPDATE SET next = next + 1
		RETURNING next
	`).Scan(&ref)
	if err != nil {
		return 0, fmt.Errorf("store: reserving the next manual adjustment reference: %w", err)
	}
	return ref, nil
}

// AdjustScore records a manual, independent correction of side's balance by
// delta (nonzero; positive adds, negative deducts). It reuses
// RecordLedgerEntry, which floors a deduction at side's current balance, so
// a correction can never push a side negative; SideManual on the other end
// of the transfer means nothing moves out of or into the other team.
func (s *Store) AdjustScore(sessionID int64, side string, delta int, reason string) (LedgerEntry, error) {
	ref, err := s.nextManualAdjustmentRef()
	if err != nil {
		return LedgerEntry{}, err
	}
	fromSide, toSide, nominalPoints := SideManual, side, delta
	if delta < 0 {
		fromSide, toSide, nominalPoints = side, SideManual, -delta
	}
	return s.RecordLedgerEntry(sessionID, SourceManualAdjustment, ref, fromSide, toSide, nominalPoints, reason)
}
