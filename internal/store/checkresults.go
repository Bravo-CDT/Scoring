package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxDetailBytes is the hard limit on a stored detail string, in bytes. It
// applies to check_results.detail and events.detail alike: both carry text a
// scored service produced, which is untrusted input. A check that prints a
// megabyte per round would otherwise fill the database file within a
// competition.
const MaxDetailBytes = 4096

// truncationMarker ends a detail string that was too long, so a reader can see
// the text is incomplete rather than wondering why it stops mid-sentence.
const truncationMarker = "...[truncated]"

// invalidUTF8Replacement stands in for each run of bytes that is not valid
// UTF-8. Replacing rather than dropping them keeps the fact that something
// unprintable came back from the service, and the standard replacement
// character is what every JSON encoder and browser already renders.
const invalidUTF8Replacement = "�"

// defaultListLimit bounds a listing whose caller asked for a non-positive
// limit. SQLite reads a negative LIMIT as "no limit", so without this a
// zero-value or miscomputed limit would quietly select an entire table.
const defaultListLimit = 100

// sanitizeDetail makes detail safe to store: valid UTF-8 (so later JSON
// encoding cannot fail on it) and no longer than MaxDetailBytes including the
// truncation marker.
func sanitizeDetail(detail string) string {
	// ToValidUTF8 returns detail unchanged when it is already valid.
	detail = strings.ToValidUTF8(detail, invalidUTF8Replacement)
	if len(detail) <= MaxDetailBytes {
		return detail
	}
	// Back the cut up to a rune boundary: slicing at a fixed byte index would
	// split a multi-byte rune and reintroduce the invalid UTF-8 just removed.
	keep := MaxDetailBytes - len(truncationMarker)
	for keep > 0 && !utf8.RuneStart(detail[keep]) {
		keep--
	}
	return detail[:keep] + truncationMarker
}

// CheckResult is one recorded check of one service: whether it answered, how
// long it took, and what it said.
//
// Timestamp is when the check ran, stored as Unix milliseconds and always read
// back in UTC. It is the engine's clock, not the database's, so results from
// concurrent checks can reach the table slightly out of timestamp order; the
// row id breaks ties within a millisecond and keeps the ordering total.
//
// Detail is whatever the check reported (an error string, a response snippet).
// It is untrusted text, so InsertCheckResult stores it through sanitizeDetail
// and the stored value may differ from the one supplied.
type CheckResult struct {
	ID        int64
	SessionID int64
	ServiceID int64
	Timestamp time.Time
	Success   bool
	LatencyMS int64
	Detail    string
}

// checkResultColumns is the select list scanCheckResult expects, in order.
const checkResultColumns = `id, session_id, service_id, timestamp, success, latency_ms, detail`

// newestFirst orders the history queries. Timestamps are only millisecond
// precise, so the id is needed as a tiebreaker: without it two checks that
// finished in the same millisecond could swap places between two pages of the
// same listing and a row would be shown twice or skipped.
//
// It is for listings only. "Which result is a service's latest" is answered by
// lastInserted instead, because a backward clock step would make timestamp
// order disagree with what actually happened.
const newestFirst = `ORDER BY timestamp DESC, id DESC`

// lastInserted orders one service's results by causal order for the "latest" /
// "most recent" queries. Each service has a single routine that records its
// results one at a time, so the AUTOINCREMENT id is the order they happened
// in, whatever the engine's clock said (an NTP correction or VM resume can step
// it backwards).
const lastInserted = `ORDER BY id DESC`

// The per-service "latest" queries. They run on every check result (the
// scorer's mass-failure check reads every service's latest outcome each
// time), so each is written to be answered from idx_check_results_service —
// whose entries are (service_id, rowid), already in lastInserted order —
// reading only the newest rows rather than sorting or scanning a service's
// whole history. TestPerServiceLatestQueriesUseTheServiceIndex pins the plans.
const (
	latestCheckResultSQL = `SELECT ` + checkResultColumns + ` FROM check_results
		 WHERE service_id = ? ` + lastInserted + ` LIMIT 1`

	// The failures after the newest success: the subquery walks the index
	// backwards and stops at the first success, and the count is a range
	// scan from there.
	consecutiveFailuresSQL = `SELECT COUNT(*) FROM check_results
		 WHERE service_id = ? AND success = 0
		   AND id > COALESCE(
		       (SELECT id FROM check_results WHERE service_id = ? AND success = 1
		        ` + lastInserted + ` LIMIT 1), 0)`

	// The subquery matches on service_id alone (a service belongs to exactly
	// one session); also matching session_id would steer the planner to the
	// (session_id, service_id, timestamp) index, which then has to sort each
	// service's whole history by id.
	activeServiceOutcomesSQL = `SELECT sv.id, COALESCE(
		     (SELECT cr.success = 0 FROM check_results cr
		      WHERE cr.service_id = sv.id
		      ` + lastInserted + ` LIMIT 1), FALSE)
		 FROM services sv
		 WHERE sv.session_id = ? AND sv.active = 1
		 ORDER BY sv.id`
)

// scanCheckResult reads one checkResultColumns row. Its error is returned
// unwrapped so callers can recognise sql.ErrNoRows.
func scanCheckResult(sc rowScanner) (CheckResult, error) {
	var cr CheckResult
	var timestampMillis int64
	err := sc.Scan(&cr.ID, &cr.SessionID, &cr.ServiceID, &timestampMillis, &cr.Success, &cr.LatencyMS, &cr.Detail)
	if err != nil {
		return CheckResult{}, err
	}
	cr.Timestamp = time.UnixMilli(timestampMillis).UTC()
	return cr, nil
}

// InsertCheckResult records one check and returns the new row's id, which the
// ledger later cites as the source of the points the result moved.
//
// Detail is capped and scrubbed (see sanitizeDetail), and a zero Timestamp
// means now — storing the zero time as epoch 0 would bury the row at the wrong
// end of every newest-first listing.
func (s *Store) InsertCheckResult(cr CheckResult) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO check_results (session_id, service_id, timestamp, success, latency_ms, detail)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		cr.SessionID, cr.ServiceID, timestampMillis(cr.Timestamp), cr.Success, cr.LatencyMS,
		sanitizeDetail(cr.Detail),
	)
	if err != nil {
		return 0, fmt.Errorf("store: inserting check result for service %d: %w", cr.ServiceID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: reading new check result id for service %d: %w", cr.ServiceID, err)
	}
	return id, nil
}

// GetLatestCheckResult returns the service's most recent check result (the
// last one inserted, see lastInserted), or (nil, nil) when it has none yet. A
// service always has that state for a while — it is created from config
// before its first check runs — so "no result yet" is an ordinary answer
// rather than an error.
func (s *Store) GetLatestCheckResult(serviceID int64) (*CheckResult, error) {
	row := s.db.QueryRow(latestCheckResultSQL, serviceID)
	switch cr, err := scanCheckResult(row); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("store: getting latest check result for service %d: %w", serviceID, err)
	default:
		return &cr, nil
	}
}

// ListCheckResults returns up to limit of the service's check results,
// newest first. beforeID pages: pass 0 for the first page, then the id of the
// last row of the page just read to get the next one. An empty slice means the
// end of the history.
func (s *Store) ListCheckResults(serviceID int64, limit int, beforeID int64) ([]CheckResult, error) {
	where, args, err := s.pageBefore("check_results", beforeID)
	if err != nil {
		return nil, fmt.Errorf("store: listing check results for service %d: %w", serviceID, err)
	}
	rows, err := s.db.Query(
		`SELECT `+checkResultColumns+` FROM check_results
		 WHERE service_id = ?`+where+` `+newestFirst+` LIMIT ?`,
		append(append([]any{serviceID}, args...), listLimit(limit))...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: listing check results for service %d: %w", serviceID, err)
	}
	defer rows.Close()

	var out []CheckResult
	for rows.Next() {
		cr, err := scanCheckResult(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scanning check result row: %w", err)
		}
		out = append(out, cr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing check results for service %d: %w", serviceID, err)
	}
	return out, nil
}

// CountConsecutiveFailures returns how many of the service's most recent check
// results are failures in an unbroken run, meaning the number of failures
// after its latest success (by the insert order of GetLatestCheckResult), or
// all of its failures if it has never succeeded. A service with no results, or
// whose latest result succeeded, has 0.
//
// It is counted in SQL rather than by walking the history newest-first, so the
// cost does not depend on reading a long failure streak row by row.
func (s *Store) CountConsecutiveFailures(serviceID int64) (int, error) {
	var count int
	err := s.db.QueryRow(consecutiveFailuresSQL, serviceID, serviceID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: counting consecutive failures for service %d: %w", serviceID, err)
	}
	return count, nil
}

// ServiceOutcome is one active service's current state for the mass-failure
// check: whether its most recent check result is a failure.
type ServiceOutcome struct {
	ServiceID int64
	// Failing is true when the latest result (by the same insert order as
	// GetLatestCheckResult) is a failure. A service with no result yet is
	// not failing: "no result yet" is unknown, not down.
	Failing bool
}

// ActiveServiceOutcomes returns the current outcome of every active service in
// the session, in service id order. Inactive services are left out.
//
// Services that have not reported yet are included (as not failing), so a
// caller dividing failures by the number of services does not mistake the
// first failure of a competition for 100% of services down. It returns every
// service rather than two counts so the caller can narrow the set further
// (the scorer keeps only services whose routine is running).
func (s *Store) ActiveServiceOutcomes(sessionID int64) ([]ServiceOutcome, error) {
	rows, err := s.db.Query(activeServiceOutcomesSQL, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: listing active service outcomes in session %d: %w", sessionID, err)
	}
	defer rows.Close()

	var out []ServiceOutcome
	for rows.Next() {
		var o ServiceOutcome
		if err := rows.Scan(&o.ServiceID, &o.Failing); err != nil {
			return nil, fmt.Errorf("store: scanning service outcome row: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing active service outcomes in session %d: %w", sessionID, err)
	}
	return out, nil
}

// timestampMillis is how every timestamp reaches the database: Unix
// milliseconds, with the zero time meaning now.
func timestampMillis(ts time.Time) int64 {
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	return ts.UnixMilli()
}

// listLimit bounds a caller's requested page size; see defaultListLimit.
func listLimit(limit int) int {
	if limit <= 0 {
		return defaultListLimit
	}
	return limit
}

// pageBefore builds the WHERE fragment (and its arguments) that restricts a
// newestFirst listing of table to the rows after the cursor row beforeID,
// returning an empty fragment for the first page (beforeID <= 0).
//
// The cursor is a row id but the ordering is (timestamp, id), so the fragment
// compares both: "id < beforeID" alone would be wrong whenever a row with a
// lower id carries a later timestamp, which concurrent checks make routine, and
// the rows between the two orderings would be skipped entirely.
//
// table is interpolated into SQL, so it must stay a literal written here — it
// never comes from a caller's input.
func (s *Store) pageBefore(table string, beforeID int64) (string, []any, error) {
	if beforeID <= 0 {
		return "", nil, nil
	}
	var cursorMillis int64
	err := s.db.QueryRow(`SELECT timestamp FROM `+table+` WHERE id = ?`, beforeID).Scan(&cursorMillis)
	if errors.Is(err, sql.ErrNoRows) {
		// The cursor names no row (a stale or made-up id from an admin
		// request); the id alone is the best ordering still available.
		return ` AND id < ?`, []any{beforeID}, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("reading page cursor %d: %w", beforeID, err)
	}
	return ` AND (timestamp < ? OR (timestamp = ? AND id < ?))`,
		[]any{cursorMillis, cursorMillis, beforeID}, nil
}
