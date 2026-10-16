package store

import (
	"database/sql"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

// ledgerSession creates the session every ledger test transfers points within.
func ledgerSession(t *testing.T, st *Store, startingScore int) Session {
	t.Helper()
	sess, err := st.GetOrCreateSession("comp-1", startingScore)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	return sess
}

// readLedgerEntry loads one stored row, so a test can check what was written
// rather than only what RecordLedgerEntry returned.
func readLedgerEntry(t *testing.T, st *Store, id int64) LedgerEntry {
	t.Helper()
	var e LedgerEntry
	var timestampMillis int64
	err := st.db.QueryRow(
		`SELECT id, session_id, timestamp, source_type, source_ref, from_side, to_side,
		        nominal_points, points, voided, reason
		 FROM ledger_entries WHERE id = ?`,
		id,
	).Scan(&e.ID, &e.SessionID, &timestampMillis, &e.SourceType, &e.SourceRef, &e.FromSide,
		&e.ToSide, &e.NominalPoints, &e.Points, &e.Voided, &e.Reason)
	if err != nil {
		t.Fatalf("reading ledger entry %d: %v", id, err)
	}
	e.Timestamp = time.UnixMilli(timestampMillis).UTC()
	return e
}

// countLedgerEntries reports how many rows the session has, voided included.
func countLedgerEntries(t *testing.T, st *Store, sessionID int64) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM ledger_entries WHERE session_id = ?`, sessionID,
	).Scan(&n); err != nil {
		t.Fatalf("counting ledger entries: %v", err)
	}
	return n
}

func TestRecordLedgerEntry_SimpleTransfer(t *testing.T) {
	st := openTemp(t)
	sess := ledgerSession(t, st, 1000)

	entry, err := st.RecordLedgerEntry(sess.ID, "service_check", 1, "blue", "red", 10, "apache-web down")
	if err != nil {
		t.Fatalf("RecordLedgerEntry: %v", err)
	}
	if entry.NominalPoints != 10 || entry.Points != 10 {
		t.Errorf("entry = %+v, want nominal=10 points=10", entry)
	}
	if entry.Voided {
		t.Error("new entry is voided, want not voided")
	}
	if entry.Timestamp.IsZero() {
		t.Error("entry.Timestamp is zero, want the time of the transfer")
	}

	// The returned value must describe the row that was actually stored.
	if stored := readLedgerEntry(t, st, entry.ID); stored != entry {
		t.Errorf("stored = %+v, want %+v", stored, entry)
	}

	blue, red, err := st.GetScore(sess.ID)
	if err != nil {
		t.Fatalf("GetScore: %v", err)
	}
	if blue != 990 || red != 10 {
		t.Errorf("blue=%d red=%d, want 990/10", blue, red)
	}
}

func TestRecordLedgerEntry_FloorsAtSenderBalance(t *testing.T) {
	st := openTemp(t)
	sess := ledgerSession(t, st, 5)

	first, err := st.RecordLedgerEntry(sess.ID, "service_check", 1, "blue", "red", 10, "r1")
	if err != nil {
		t.Fatalf("first RecordLedgerEntry: %v", err)
	}
	// The configured rate is recorded as-is; only the transfer is floored.
	if first.NominalPoints != 10 || first.Points != 5 {
		t.Errorf("first = %+v, want nominal=10 points=5 (floored)", first)
	}

	second, err := st.RecordLedgerEntry(sess.ID, "service_check", 2, "blue", "red", 10, "r2")
	if err != nil {
		t.Fatalf("second RecordLedgerEntry: %v", err)
	}
	if second.NominalPoints != 10 || second.Points != 0 {
		t.Errorf("second = %+v, want nominal=10 points=0 (blue already at floor)", second)
	}

	blue, red, err := st.GetScore(sess.ID)
	if err != nil {
		t.Fatalf("GetScore: %v", err)
	}
	if blue != 0 || red != 5 {
		t.Errorf("blue=%d red=%d, want 0/5", blue, red)
	}
}

func TestRecordLedgerEntry_NonPositiveNominalTransfersNothing(t *testing.T) {
	st := openTemp(t)
	sess := ledgerSession(t, st, 1000)

	// A nonsensical rate must never move points the wrong way: a negative
	// transfer would hand the sender points it never lost.
	entry, err := st.RecordLedgerEntry(sess.ID, "service_check", 1, "blue", "red", -5, "bad rate")
	if err != nil {
		t.Fatalf("RecordLedgerEntry: %v", err)
	}
	if entry.NominalPoints != -5 || entry.Points != 0 {
		t.Errorf("entry = %+v, want nominal=-5 points=0", entry)
	}

	blue, red, err := st.GetScore(sess.ID)
	if err != nil {
		t.Fatalf("GetScore: %v", err)
	}
	if blue != 1000 || red != 0 {
		t.Errorf("blue=%d red=%d, want 1000/0", blue, red)
	}
}

func TestRecordLedgerEntry_DuplicateSourceRefRejected(t *testing.T) {
	st := openTemp(t)
	sess := ledgerSession(t, st, 1000)

	if _, err := st.RecordLedgerEntry(sess.ID, "service_check", 42, "blue", "red", 10, "r1"); err != nil {
		t.Fatalf("first RecordLedgerEntry: %v", err)
	}
	_, err := st.RecordLedgerEntry(sess.ID, "service_check", 42, "blue", "red", 10, "r1-retry")
	if !errors.Is(err, ErrDuplicateSourceRef) {
		t.Fatalf("expected ErrDuplicateSourceRef, got %v", err)
	}

	if n := countLedgerEntries(t, st, sess.ID); n != 1 {
		t.Errorf("ledger holds %d entries, want 1 (the duplicate must not be stored)", n)
	}
	blue, red, err := st.GetScore(sess.ID)
	if err != nil {
		t.Fatalf("GetScore: %v", err)
	}
	if blue != 990 || red != 10 {
		t.Errorf("rejected duplicate must not double-score: blue=%d red=%d, want 990/10", blue, red)
	}
}

func TestRecordLedgerEntry_SameSourceRefAcrossSourceTypesAndSessions(t *testing.T) {
	st := openTemp(t)
	sess := ledgerSession(t, st, 1000)
	other, err := st.GetOrCreateSession("comp-2", 1000)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}

	// Uniqueness is scoped to (session, source_type, source_ref): ref 7 of a
	// different source kind, or of another competition, is a different source.
	if _, err := st.RecordLedgerEntry(sess.ID, "service_check", 7, "blue", "red", 10, "check 7"); err != nil {
		t.Fatalf("service_check entry: %v", err)
	}
	if _, err := st.RecordLedgerEntry(sess.ID, "inject", 7, "blue", "red", 10, "inject 7"); err != nil {
		t.Fatalf("inject entry with the same ref: %v", err)
	}
	if _, err := st.RecordLedgerEntry(other.ID, "service_check", 7, "blue", "red", 10, "other session"); err != nil {
		t.Fatalf("other session entry with the same ref: %v", err)
	}

	blue, red, err := st.GetScore(sess.ID)
	if err != nil {
		t.Fatalf("GetScore: %v", err)
	}
	if blue != 980 || red != 20 {
		t.Errorf("blue=%d red=%d, want 980/20", blue, red)
	}
}

func TestRecordLedgerEntry_UnknownSessionFails(t *testing.T) {
	st := openTemp(t)
	sess := ledgerSession(t, st, 1000)

	_, err := st.RecordLedgerEntry(424242, "service_check", 1, "blue", "red", 10, "ghost session")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want it to wrap sql.ErrNoRows", err)
	}
	if n := countLedgerEntries(t, st, sess.ID); n != 0 {
		t.Errorf("ledger holds %d entries, want 0", n)
	}
}

func TestRecordLedgerEntry_BidirectionalTransfers(t *testing.T) {
	st := openTemp(t)
	sess := ledgerSession(t, st, 1000)

	// Red holds nothing yet, so a red-to-blue transfer floors at zero rather
	// than letting red go negative.
	empty, err := st.RecordLedgerEntry(sess.ID, "inject", 1, "red", "blue", 10, "red has nothing")
	if err != nil {
		t.Fatalf("red-to-blue RecordLedgerEntry: %v", err)
	}
	if empty.Points != 0 {
		t.Errorf("empty.Points = %d, want 0 (red is at its floor)", empty.Points)
	}

	if _, err := st.RecordLedgerEntry(sess.ID, "service_check", 1, "blue", "red", 10, "blue loses 10"); err != nil {
		t.Fatalf("blue-to-red RecordLedgerEntry: %v", err)
	}
	// Now red has 10 to give back, so the return transfer is only floored at 4.
	back, err := st.RecordLedgerEntry(sess.ID, "inject", 2, "red", "blue", 4, "red gives back 4")
	if err != nil {
		t.Fatalf("return RecordLedgerEntry: %v", err)
	}
	if back.Points != 4 {
		t.Errorf("back.Points = %d, want 4", back.Points)
	}

	blue, red, err := st.GetScore(sess.ID)
	if err != nil {
		t.Fatalf("GetScore: %v", err)
	}
	if blue != 994 || red != 6 {
		t.Errorf("blue=%d red=%d, want 994/6", blue, red)
	}
}

// TestRecordLedgerEntry_ConcurrentCallsShareOneFloor is the floor/cap
// concurrency guarantee: two services failing in the same instant must not both
// read the same pre-transfer balance. Blue holds 10 and both calls ask for 10,
// so a non-transactional implementation would transfer 10 twice and drive blue
// to -10. Serialized, exactly one call sees the 10 and the other sees a drained
// sender.
func TestRecordLedgerEntry_ConcurrentCallsShareOneFloor(t *testing.T) {
	st := openTemp(t)
	sess := ledgerSession(t, st, 10)

	var (
		wg      sync.WaitGroup
		entries [2]LedgerEntry
		errs    [2]error
		start   = make(chan struct{})
	)
	for i := range entries {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release both goroutines as simultaneously as possible
			entries[i], errs[i] = st.RecordLedgerEntry(
				sess.ID, "service_check", int64(i+1), "blue", "red", 10, "simultaneous failure")
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("RecordLedgerEntry %d: %v", i, err)
		}
	}
	points := []int{entries[0].Points, entries[1].Points}
	sort.Ints(points)
	if points[0] != 0 || points[1] != 10 {
		t.Errorf("transferred points = %v, want one 10 and one 0 (both read the same balance)", points)
	}

	blue, red, err := st.GetScore(sess.ID)
	if err != nil {
		t.Fatalf("GetScore: %v", err)
	}
	if blue != 0 || red != 10 {
		t.Errorf("blue=%d red=%d, want 0/10", blue, red)
	}
}

// TestRecordLedgerEntry_ConcurrentCallsNeverOverdraw is the same guarantee under
// wider contention: whatever order the writers land in, they may transfer only
// what the sender actually had, so blue can never go negative and red can never
// gain more than blue lost.
func TestRecordLedgerEntry_ConcurrentCallsNeverOverdraw(t *testing.T) {
	const (
		startingScore = 10
		writers       = 8
		nominal       = 3
	)
	st := openTemp(t)
	sess := ledgerSession(t, st, startingScore)

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		total int
		start = make(chan struct{})
	)
	errs := make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			entry, err := st.RecordLedgerEntry(
				sess.ID, "service_check", int64(i+1), "blue", "red", nominal, "contended failure")
			if err != nil {
				errs <- err
				return
			}
			if entry.Points < 0 || entry.Points > nominal {
				errs <- errors.New("transferred points outside [0, nominal]")
				return
			}
			mu.Lock()
			total += entry.Points
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent RecordLedgerEntry: %v", err)
	}

	// writers*nominal = 24 points were asked for but blue only had 10.
	if total != startingScore {
		t.Errorf("total transferred = %d, want %d (blue's whole balance, no more)", total, startingScore)
	}
	blue, red, err := st.GetScore(sess.ID)
	if err != nil {
		t.Fatalf("GetScore: %v", err)
	}
	if blue != 0 || red != startingScore {
		t.Errorf("blue=%d red=%d, want 0/%d", blue, red, startingScore)
	}
}

func TestGetScore_NoEntriesYet(t *testing.T) {
	st := openTemp(t)
	sess := ledgerSession(t, st, 1000)

	blue, red, err := st.GetScore(sess.ID)
	if err != nil {
		t.Fatalf("GetScore: %v", err)
	}
	if blue != 1000 || red != 0 {
		t.Errorf("blue=%d red=%d, want 1000/0", blue, red)
	}
}

func TestGetScore_ExcludesVoidedEntries(t *testing.T) {
	st := openTemp(t)
	sess := ledgerSession(t, st, 1000)

	if _, err := st.RecordLedgerEntry(sess.ID, "service_check", 1, "blue", "red", 10, "r1"); err != nil {
		t.Fatalf("RecordLedgerEntry: %v", err)
	}
	// No API writes voided=1 yet (deferred per spec), but GetScore's SUM must
	// already respect it so a future admin-void feature works without touching
	// this query.
	if _, err := st.db.Exec(
		`INSERT INTO ledger_entries
		 (session_id, timestamp, source_type, source_ref, from_side, to_side, nominal_points, points, voided, reason)
		 VALUES (?, 0, 'service_check', 2, 'blue', 'red', 10, 10, 1, 'manually voided for test')`,
		sess.ID,
	); err != nil {
		t.Fatalf("inserting voided row directly: %v", err)
	}

	blue, red, err := st.GetScore(sess.ID)
	if err != nil {
		t.Fatalf("GetScore: %v", err)
	}
	if blue != 990 || red != 10 {
		t.Errorf("voided entry was counted: blue=%d red=%d, want 990/10", blue, red)
	}
}

// TestRecordLedgerEntry_IgnoresVoidedEntriesWhenFlooring pins the other half of
// voiding: a voided transfer never happened, so the points it moved are back in
// the sender's balance and available to the next transfer.
func TestRecordLedgerEntry_IgnoresVoidedEntriesWhenFlooring(t *testing.T) {
	st := openTemp(t)
	sess := ledgerSession(t, st, 10)

	if _, err := st.db.Exec(
		`INSERT INTO ledger_entries
		 (session_id, timestamp, source_type, source_ref, from_side, to_side, nominal_points, points, voided, reason)
		 VALUES (?, 0, 'service_check', 1, 'blue', 'red', 10, 10, 1, 'voided')`,
		sess.ID,
	); err != nil {
		t.Fatalf("inserting voided row directly: %v", err)
	}

	entry, err := st.RecordLedgerEntry(sess.ID, "service_check", 2, "blue", "red", 10, "r2")
	if err != nil {
		t.Fatalf("RecordLedgerEntry: %v", err)
	}
	if entry.Points != 10 {
		t.Errorf("entry.Points = %d, want 10 (the voided entry must not hold blue at its floor)", entry.Points)
	}
}

func TestGetScore_UnknownSessionFails(t *testing.T) {
	st := openTemp(t)

	if _, _, err := st.GetScore(424242); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want it to wrap sql.ErrNoRows", err)
	}
}
