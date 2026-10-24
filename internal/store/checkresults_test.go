package store

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// setupServiceForResults creates the session and service that check results
// hang off, returning their ids.
func setupServiceForResults(t *testing.T, st *Store, serviceName string) (sessionID, serviceID int64) {
	t.Helper()
	sess, err := st.GetOrCreateSession("comp-1", 1000)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	svc, err := st.GetOrCreateService(sess.ID, serviceName)
	if err != nil {
		t.Fatalf("GetOrCreateService(%q): %v", serviceName, err)
	}
	return sess.ID, svc.ID
}

// insertResultAt records one check result at an explicit timestamp, so a test
// can control the (timestamp, id) ordering the list queries page by.
func insertResultAt(t *testing.T, st *Store, sessionID, serviceID int64, ts time.Time) int64 {
	t.Helper()
	id, err := st.InsertCheckResult(CheckResult{
		SessionID: sessionID, ServiceID: serviceID, Timestamp: ts,
		Success: true, LatencyMS: 7, Detail: "ok",
	})
	if err != nil {
		t.Fatalf("InsertCheckResult: %v", err)
	}
	return id
}

func collectResultIDs(results []CheckResult) []int64 {
	ids := make([]int64, len(results))
	for i, cr := range results {
		ids[i] = cr.ID
	}
	return ids
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestInsertAndGetLatestCheckResult(t *testing.T) {
	st := openTemp(t)
	sessionID, serviceID := setupServiceForResults(t, st, "apache-web")

	// Millisecond precision is all the schema stores, so compare against a
	// timestamp that survives the round trip exactly.
	ts := time.Now().UTC().Truncate(time.Millisecond)
	id, err := st.InsertCheckResult(CheckResult{
		SessionID: sessionID, ServiceID: serviceID, Timestamp: ts,
		Success: true, LatencyMS: 42, Detail: "OK",
	})
	if err != nil {
		t.Fatalf("InsertCheckResult: %v", err)
	}
	if id == 0 {
		t.Fatal("InsertCheckResult returned id 0, want the new row's id")
	}

	latest, err := st.GetLatestCheckResult(serviceID)
	if err != nil {
		t.Fatalf("GetLatestCheckResult: %v", err)
	}
	if latest == nil {
		t.Fatal("GetLatestCheckResult returned nil, want the inserted row")
	}
	want := CheckResult{
		ID: id, SessionID: sessionID, ServiceID: serviceID, Timestamp: ts,
		Success: true, LatencyMS: 42, Detail: "OK",
	}
	if *latest != want {
		t.Errorf("GetLatestCheckResult = %+v, want %+v", *latest, want)
	}
	if latest.Timestamp.Location() != time.UTC {
		t.Errorf("Timestamp location = %v, want UTC", latest.Timestamp.Location())
	}
}

func TestGetLatestCheckResult_NoneYet(t *testing.T) {
	st := openTemp(t)
	_, serviceID := setupServiceForResults(t, st, "apache-web")

	latest, err := st.GetLatestCheckResult(serviceID)
	if err != nil {
		t.Fatalf("GetLatestCheckResult: %v", err)
	}
	if latest != nil {
		t.Errorf("GetLatestCheckResult = %+v for a service with no results, want nil", latest)
	}
}

func TestInsertCheckResult_RoundRoundTrips(t *testing.T) {
	st := openTemp(t)
	sessionID, serviceID := setupServiceForResults(t, st, "apache-web")

	id, err := st.InsertCheckResult(CheckResult{
		SessionID: sessionID, ServiceID: serviceID, Round: 7, Success: true,
	})
	if err != nil {
		t.Fatalf("InsertCheckResult: %v", err)
	}

	latest, err := st.GetLatestCheckResult(serviceID)
	if err != nil {
		t.Fatalf("GetLatestCheckResult: %v", err)
	}
	if latest == nil || latest.ID != id || latest.Round != 7 {
		t.Errorf("GetLatestCheckResult = %+v, want row %d with Round 7", latest, id)
	}

	listed, err := st.ListCheckResults(serviceID, 10, 0)
	if err != nil {
		t.Fatalf("ListCheckResults: %v", err)
	}
	if len(listed) != 1 || listed[0].Round != 7 {
		t.Errorf("ListCheckResults = %+v, want one row with Round 7", listed)
	}
}

func TestNextRoundNumber_StartsAtOneWithNoHistory(t *testing.T) {
	st := openTemp(t)
	sessionID, _ := setupServiceForResults(t, st, "apache-web")

	n, err := st.NextRoundNumber(sessionID)
	if err != nil {
		t.Fatalf("NextRoundNumber: %v", err)
	}
	if n != 1 {
		t.Errorf("NextRoundNumber = %d for a session with no results, want 1", n)
	}
}

func TestNextRoundNumber_ResumesAfterHighestStoredRound(t *testing.T) {
	st := openTemp(t)
	sessionID, serviceID := setupServiceForResults(t, st, "apache-web")

	// Insert out of round order: the answer follows the highest round, not
	// the most recently inserted one.
	for _, round := range []int{3, 5, 4} {
		if _, err := st.InsertCheckResult(CheckResult{
			SessionID: sessionID, ServiceID: serviceID, Round: round, Success: true,
		}); err != nil {
			t.Fatalf("InsertCheckResult(round %d): %v", round, err)
		}
	}

	n, err := st.NextRoundNumber(sessionID)
	if err != nil {
		t.Fatalf("NextRoundNumber: %v", err)
	}
	if n != 6 {
		t.Errorf("NextRoundNumber = %d after rounds 3, 5 and 4, want 6", n)
	}
}

func TestNextRoundNumber_IsScopedToOneSession(t *testing.T) {
	st := openTemp(t)
	sessionID, serviceID := setupServiceForResults(t, st, "apache-web")
	if _, err := st.InsertCheckResult(CheckResult{
		SessionID: sessionID, ServiceID: serviceID, Round: 9, Success: true,
	}); err != nil {
		t.Fatalf("InsertCheckResult: %v", err)
	}

	other, err := st.GetOrCreateSession("comp-2", 1000)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	n, err := st.NextRoundNumber(other.ID)
	if err != nil {
		t.Fatalf("NextRoundNumber: %v", err)
	}
	if n != 1 {
		t.Errorf("NextRoundNumber = %d for a session whose only rounds belong to another, want 1", n)
	}
}

func TestGetLatestCheckResult_IsNewestForThatServiceOnly(t *testing.T) {
	st := openTemp(t)
	sessionID, webID := setupServiceForResults(t, st, "apache-web")
	_, dnsID := setupServiceForResults(t, st, "bind-dns")

	base := time.UnixMilli(1_700_000_000_000).UTC()
	insertResultAt(t, st, sessionID, webID, base)
	wantWeb := insertResultAt(t, st, sessionID, webID, base.Add(time.Second))
	// A newer row for another service must not win the web service's lookup.
	insertResultAt(t, st, sessionID, dnsID, base.Add(time.Hour))

	latest, err := st.GetLatestCheckResult(webID)
	if err != nil {
		t.Fatalf("GetLatestCheckResult: %v", err)
	}
	if latest == nil || latest.ID != wantWeb {
		t.Errorf("GetLatestCheckResult = %+v, want row %d", latest, wantWeb)
	}
}

func TestGetLatestCheckResult_BreaksMillisecondTiesByID(t *testing.T) {
	st := openTemp(t)
	sessionID, serviceID := setupServiceForResults(t, st, "apache-web")

	ts := time.UnixMilli(1_700_000_000_000).UTC()
	insertResultAt(t, st, sessionID, serviceID, ts)
	newest := insertResultAt(t, st, sessionID, serviceID, ts)

	latest, err := st.GetLatestCheckResult(serviceID)
	if err != nil {
		t.Fatalf("GetLatestCheckResult: %v", err)
	}
	if latest == nil || latest.ID != newest {
		t.Errorf("GetLatestCheckResult = %+v, want the higher id %d", latest, newest)
	}
}

// A backward clock step (NTP correction, VM resume) gives the newer row an
// earlier timestamp. "Latest" is causal order, which the row id records,
// because one routine per service inserts its results one at a time.
func TestGetLatestCheckResult_FollowsInsertOrderAcrossClockStep(t *testing.T) {
	st := openTemp(t)
	sessionID, serviceID := setupServiceForResults(t, st, "apache-web")

	base := time.UnixMilli(1_700_000_000_000).UTC()
	insertResultAt(t, st, sessionID, serviceID, base)
	newest := insertResultAt(t, st, sessionID, serviceID, base.Add(-time.Hour))

	latest, err := st.GetLatestCheckResult(serviceID)
	if err != nil {
		t.Fatalf("GetLatestCheckResult: %v", err)
	}
	if latest == nil || latest.ID != newest {
		t.Errorf("GetLatestCheckResult = %+v, want the last-inserted row %d", latest, newest)
	}
}

func TestInsertCheckResult_TruncatesOversizedDetail(t *testing.T) {
	st := openTemp(t)
	sessionID, serviceID := setupServiceForResults(t, st, "apache-web")

	huge := strings.Repeat("x", MaxDetailBytes*2)
	if _, err := st.InsertCheckResult(CheckResult{
		SessionID: sessionID, ServiceID: serviceID, Timestamp: time.Now().UTC(),
		Success: false, LatencyMS: 10, Detail: huge,
	}); err != nil {
		t.Fatalf("InsertCheckResult: %v", err)
	}

	stored, err := st.GetLatestCheckResult(serviceID)
	if err != nil {
		t.Fatalf("GetLatestCheckResult: %v", err)
	}
	if len(stored.Detail) > MaxDetailBytes {
		t.Errorf("len(Detail) = %d, want at most MaxDetailBytes (%d)", len(stored.Detail), MaxDetailBytes)
	}
	if !strings.Contains(stored.Detail, "truncated") {
		t.Errorf("Detail has no truncation marker; it ends %q", lastBytes(stored.Detail, 32))
	}
	if !strings.HasPrefix(stored.Detail, "xxx") {
		t.Errorf("Detail lost the start of the check output: %q", firstBytes(stored.Detail, 32))
	}
}

func TestInsertCheckResult_SanitizesInvalidUTF8(t *testing.T) {
	st := openTemp(t)
	sessionID, serviceID := setupServiceForResults(t, st, "apache-web")

	if _, err := st.InsertCheckResult(CheckResult{
		SessionID: sessionID, ServiceID: serviceID, Timestamp: time.Now().UTC(),
		Success: false, LatencyMS: 5, Detail: "valid-prefix-\xff\xfe-invalid",
	}); err != nil {
		t.Fatalf("InsertCheckResult: %v", err)
	}

	stored, err := st.GetLatestCheckResult(serviceID)
	if err != nil {
		t.Fatalf("GetLatestCheckResult: %v", err)
	}
	if !utf8.ValidString(stored.Detail) {
		t.Errorf("Detail is not valid UTF-8: %q", stored.Detail)
	}
	if !strings.Contains(stored.Detail, "valid-prefix-") || !strings.Contains(stored.Detail, "-invalid") {
		t.Errorf("Detail lost the valid text around the bad bytes: %q", stored.Detail)
	}
	// A byte-level search: ContainsAny would decode the cutset's bad bytes into
	// U+FFFD and match the replacement character itself.
	if strings.Contains(stored.Detail, "\xff") || strings.Contains(stored.Detail, "\xfe") {
		t.Errorf("Detail still carries the invalid bytes: %q", stored.Detail)
	}
}

func TestSanitizeDetail(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		check func(t *testing.T, got string)
	}{
		{
			name: "short text is untouched",
			in:   "connection refused",
			check: func(t *testing.T, got string) {
				if got != "connection refused" {
					t.Errorf("got %q, want it unchanged", got)
				}
			},
		},
		{
			name: "exactly the cap is untouched",
			in:   strings.Repeat("a", MaxDetailBytes),
			check: func(t *testing.T, got string) {
				if len(got) != MaxDetailBytes || strings.Contains(got, "truncated") {
					t.Errorf("len = %d, marker = %t; want an untouched %d bytes",
						len(got), strings.Contains(got, "truncated"), MaxDetailBytes)
				}
			},
		},
		{
			name: "one byte over the cap is truncated",
			in:   strings.Repeat("a", MaxDetailBytes+1),
			check: func(t *testing.T, got string) {
				if len(got) > MaxDetailBytes {
					t.Errorf("len = %d, want at most %d", len(got), MaxDetailBytes)
				}
				if !strings.Contains(got, "truncated") {
					t.Errorf("no truncation marker in %q", lastBytes(got, 32))
				}
			},
		},
		{
			name: "truncation cuts on a rune boundary",
			// Three-byte runes all the way, so the cut lands mid-rune: a blind
			// byte-index cut would slice one in half and reintroduce invalid
			// UTF-8.
			in: strings.Repeat("€", MaxDetailBytes),
			check: func(t *testing.T, got string) {
				if len(got) > MaxDetailBytes {
					t.Errorf("len = %d, want at most %d", len(got), MaxDetailBytes)
				}
				if !utf8.ValidString(got) {
					t.Errorf("truncation produced invalid UTF-8: %q", lastBytes(got, 32))
				}
			},
		},
		{
			name: "invalid bytes are replaced, not dropped silently",
			in:   "a\xffb",
			check: func(t *testing.T, got string) {
				if !utf8.ValidString(got) {
					t.Errorf("got invalid UTF-8: %q", got)
				}
				if got != "a�b" {
					t.Errorf("got %q, want %q", got, "a�b")
				}
			},
		},
		{
			name: "oversized invalid input is both sanitized and truncated",
			in:   strings.Repeat("\xff", MaxDetailBytes*2),
			check: func(t *testing.T, got string) {
				if len(got) > MaxDetailBytes {
					t.Errorf("len = %d, want at most %d", len(got), MaxDetailBytes)
				}
				if !utf8.ValidString(got) {
					t.Errorf("got invalid UTF-8: %q", lastBytes(got, 32))
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, sanitizeDetail(tc.in))
		})
	}
}

func TestInsertCheckResult_DefaultsZeroTimestampToNow(t *testing.T) {
	st := openTemp(t)
	sessionID, serviceID := setupServiceForResults(t, st, "apache-web")

	before := time.Now().UTC().Add(-time.Second)
	if _, err := st.InsertCheckResult(CheckResult{
		SessionID: sessionID, ServiceID: serviceID,
		Success: true, LatencyMS: 1, Detail: "ok",
	}); err != nil {
		t.Fatalf("InsertCheckResult: %v", err)
	}

	stored, err := st.GetLatestCheckResult(serviceID)
	if err != nil {
		t.Fatalf("GetLatestCheckResult: %v", err)
	}
	// Storing the zero time as epoch 0 would push the row to the wrong end of
	// every newest-first listing.
	if stored.Timestamp.Before(before) {
		t.Errorf("Timestamp = %v, want roughly now (after %v)", stored.Timestamp, before)
	}
}

func TestInsertCheckResult_RejectsUnknownService(t *testing.T) {
	st := openTemp(t)
	sessionID, _ := setupServiceForResults(t, st, "apache-web")

	if _, err := st.InsertCheckResult(CheckResult{
		SessionID: sessionID, ServiceID: 424242, Timestamp: time.Now().UTC(),
		Success: true, LatencyMS: 1, Detail: "ok",
	}); err == nil {
		t.Error("InsertCheckResult for a nonexistent service succeeded, want a foreign-key error")
	}
}

func TestListCheckResults_PagesNewestFirst(t *testing.T) {
	st := openTemp(t)
	sessionID, serviceID := setupServiceForResults(t, st, "apache-web")

	base := time.UnixMilli(1_700_000_000_000).UTC()
	var ids []int64 // oldest first
	for i := range 5 {
		ids = append(ids, insertResultAt(t, st, sessionID, serviceID, base.Add(time.Duration(i)*time.Second)))
	}

	page, err := st.ListCheckResults(serviceID, 3, 0)
	if err != nil {
		t.Fatalf("ListCheckResults: %v", err)
	}
	if want := []int64{ids[4], ids[3], ids[2]}; !equalIDs(collectResultIDs(page), want) {
		t.Fatalf("first page ids = %v, want %v", collectResultIDs(page), want)
	}

	next, err := st.ListCheckResults(serviceID, 3, page[len(page)-1].ID)
	if err != nil {
		t.Fatalf("ListCheckResults page 2: %v", err)
	}
	if want := []int64{ids[1], ids[0]}; !equalIDs(collectResultIDs(next), want) {
		t.Fatalf("second page ids = %v, want %v", collectResultIDs(next), want)
	}

	last, err := st.ListCheckResults(serviceID, 3, next[len(next)-1].ID)
	if err != nil {
		t.Fatalf("ListCheckResults page 3: %v", err)
	}
	if len(last) != 0 {
		t.Errorf("third page = %v, want empty", collectResultIDs(last))
	}
}

func TestListCheckResults_PagesWhenTimestampsAreOutOfOrder(t *testing.T) {
	st := openTemp(t)
	sessionID, serviceID := setupServiceForResults(t, st, "apache-web")

	// Checks run concurrently, so the row written first does not always carry
	// the earliest timestamp. Paging keyed on the id alone would then skip
	// rows; every row must still be visited exactly once.
	base := time.UnixMilli(1_700_000_000_000).UTC()
	seconds := []int{2, 0, 3, 1}
	byDelay := map[int]int64{}
	for _, sec := range seconds {
		byDelay[sec] = insertResultAt(t, st, sessionID, serviceID, base.Add(time.Duration(sec)*time.Second))
	}
	want := []int64{byDelay[3], byDelay[2], byDelay[1], byDelay[0]}

	var got []int64
	beforeID := int64(0)
	for range len(seconds) { // one more pass than needed would re-page the tail
		page, err := st.ListCheckResults(serviceID, 2, beforeID)
		if err != nil {
			t.Fatalf("ListCheckResults(beforeID=%d): %v", beforeID, err)
		}
		if len(page) == 0 {
			break
		}
		got = append(got, collectResultIDs(page)...)
		beforeID = page[len(page)-1].ID
	}
	if !equalIDs(got, want) {
		t.Errorf("paged ids = %v, want %v", got, want)
	}
}

func TestListCheckResults_BreaksMillisecondTiesByID(t *testing.T) {
	st := openTemp(t)
	sessionID, serviceID := setupServiceForResults(t, st, "apache-web")

	ts := time.UnixMilli(1_700_000_000_000).UTC()
	var ids []int64
	for range 3 {
		ids = append(ids, insertResultAt(t, st, sessionID, serviceID, ts))
	}

	page, err := st.ListCheckResults(serviceID, 2, 0)
	if err != nil {
		t.Fatalf("ListCheckResults: %v", err)
	}
	if want := []int64{ids[2], ids[1]}; !equalIDs(collectResultIDs(page), want) {
		t.Fatalf("first page ids = %v, want %v", collectResultIDs(page), want)
	}

	next, err := st.ListCheckResults(serviceID, 2, page[1].ID)
	if err != nil {
		t.Fatalf("ListCheckResults page 2: %v", err)
	}
	if want := []int64{ids[0]}; !equalIDs(collectResultIDs(next), want) {
		t.Errorf("second page ids = %v, want %v", collectResultIDs(next), want)
	}
}

func TestListCheckResults_IsScopedToOneService(t *testing.T) {
	st := openTemp(t)
	sessionID, webID := setupServiceForResults(t, st, "apache-web")
	_, dnsID := setupServiceForResults(t, st, "bind-dns")

	base := time.UnixMilli(1_700_000_000_000).UTC()
	webRow := insertResultAt(t, st, sessionID, webID, base)
	insertResultAt(t, st, sessionID, dnsID, base.Add(time.Second))

	page, err := st.ListCheckResults(webID, 10, 0)
	if err != nil {
		t.Fatalf("ListCheckResults: %v", err)
	}
	if want := []int64{webRow}; !equalIDs(collectResultIDs(page), want) {
		t.Errorf("ids = %v, want %v", collectResultIDs(page), want)
	}
}

func TestListCheckResults_NoResultsIsEmptyNotAnError(t *testing.T) {
	st := openTemp(t)
	_, serviceID := setupServiceForResults(t, st, "apache-web")

	page, err := st.ListCheckResults(serviceID, 10, 0)
	if err != nil {
		t.Fatalf("ListCheckResults: %v", err)
	}
	if len(page) != 0 {
		t.Errorf("ids = %v, want empty", collectResultIDs(page))
	}
}

func TestListCheckResults_NonPositiveLimitIsBounded(t *testing.T) {
	st := openTemp(t)
	sessionID, serviceID := setupServiceForResults(t, st, "apache-web")

	base := time.UnixMilli(1_700_000_000_000).UTC()
	for i := range 3 {
		insertResultAt(t, st, sessionID, serviceID, base.Add(time.Duration(i)*time.Second))
	}

	// SQLite reads a negative LIMIT as "no limit", so a zero-value or bogus
	// limit must not turn into an unbounded scan of the whole table.
	for _, limit := range []int{0, -1} {
		page, err := st.ListCheckResults(serviceID, limit, 0)
		if err != nil {
			t.Fatalf("ListCheckResults(limit=%d): %v", limit, err)
		}
		if len(page) == 0 || len(page) > defaultListLimit {
			t.Errorf("ListCheckResults(limit=%d) returned %d rows, want between 1 and %d",
				limit, len(page), defaultListLimit)
		}
	}
}

func firstBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func TestCountConsecutiveFailures(t *testing.T) {
	base := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		results []bool // success flags in timestamp order, oldest first
		want    int
	}{
		{"no results", nil, 0},
		{"latest succeeded", []bool{false, false, true}, 0},
		{"all failed", []bool{false, false, false}, 3},
		{"failures since last success", []bool{false, true, false, false}, 2},
		{"single failure", []bool{true, false}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := openTemp(t)
			sessionID, serviceID := setupServiceForResults(t, st, "svc")
			for i, ok := range tc.results {
				insertOutcomeAt(t, st, sessionID, serviceID, base.Add(time.Duration(i)*time.Second), ok)
			}
			got, err := st.CountConsecutiveFailures(serviceID)
			if err != nil {
				t.Fatalf("CountConsecutiveFailures: %v", err)
			}
			if got != tc.want {
				t.Errorf("CountConsecutiveFailures = %d, want %d", got, tc.want)
			}
		})
	}
}

// The streak follows insert order (the row id), not the timestamp, so a
// backward clock step cannot reorder a service's history.
func TestCountConsecutiveFailures_FollowsInsertOrderAcrossClockStep(t *testing.T) {
	st := openTemp(t)
	sessionID, serviceID := setupServiceForResults(t, st, "svc")
	base := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

	// The clock steps back an hour after the success: the two failures written
	// after it carry earlier timestamps but are still the newest results.
	insertOutcomeAt(t, st, sessionID, serviceID, base, true)
	insertOutcomeAt(t, st, sessionID, serviceID, base.Add(-time.Hour), false)
	insertOutcomeAt(t, st, sessionID, serviceID, base.Add(-time.Hour), false)

	if got, err := st.CountConsecutiveFailures(serviceID); err != nil || got != 2 {
		t.Errorf("CountConsecutiveFailures = %d, %v; want 2", got, err)
	}

	// A success written last ends the streak whatever its timestamp says.
	insertOutcomeAt(t, st, sessionID, serviceID, base.Add(-2*time.Hour), true)
	if got, err := st.CountConsecutiveFailures(serviceID); err != nil || got != 0 {
		t.Errorf("after the last success: CountConsecutiveFailures = %d, %v; want 0", got, err)
	}
}

func TestCountConsecutiveFailures_IsScopedToOneService(t *testing.T) {
	st := openTemp(t)
	sessionID, a := setupServiceForResults(t, st, "a")
	_, b := setupServiceForResults(t, st, "b")
	now := time.Now().UTC()
	insertOutcomeAt(t, st, sessionID, a, now, false)
	insertOutcomeAt(t, st, sessionID, a, now, false)
	insertOutcomeAt(t, st, sessionID, b, now, false)

	if got, err := st.CountConsecutiveFailures(b); err != nil || got != 1 {
		t.Errorf("CountConsecutiveFailures(b) = %d, %v; want 1", got, err)
	}
}

// planStep is one row of EXPLAIN QUERY PLAN output.
type planStep struct {
	id, parent int
	detail     string
}

func explainPlan(t *testing.T, st *Store, query string, args ...any) []planStep {
	t.Helper()
	rows, err := st.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var plan []planStep
	for rows.Next() {
		var s planStep
		var notUsed int
		if err := rows.Scan(&s.id, &s.parent, &notUsed, &s.detail); err != nil {
			t.Fatalf("scanning plan row: %v", err)
		}
		plan = append(plan, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading plan: %v", err)
	}
	return plan
}

// The per-service "latest" queries run on every check result, so their cost
// must not grow with the length of the history. This pins it without timing
// anything: every read of check_results must be an index SEARCH on the
// service id, and no sort may run anywhere in the plan (a sort would mean
// check_results rows are being sorted instead of answered straight from the
// index).
func TestPerServiceLatestQueriesUseTheServiceIndex(t *testing.T) {
	st := openTemp(t)
	tests := []struct {
		name  string
		query string
		args  []any
	}{
		{"GetLatestCheckResult", latestCheckResultSQL, []any{1}},
		{"CountConsecutiveFailures", consecutiveFailuresSQL, []any{1, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := explainPlan(t, st, tt.query, tt.args...)
			sawCheckResults := false
			for _, s := range plan {
				if strings.Contains(s.detail, "check_results") || strings.Contains(s.detail, " cr ") ||
					strings.HasSuffix(s.detail, " cr") {
					sawCheckResults = true
					if !strings.HasPrefix(s.detail, "SEARCH") || !strings.Contains(s.detail, "idx_check_results_service ") {
						t.Errorf("check_results step %q: want a SEARCH using idx_check_results_service", s.detail)
					}
				}
				if strings.Contains(s.detail, "TEMP B-TREE") {
					t.Errorf("unexpected sort: %q (plan %+v)", s.detail, plan)
				}
			}
			if !sawCheckResults {
				t.Fatalf("plan never reads check_results: %+v", plan)
			}
		})
	}
}

// insertOutcomeAt records one check result with the given outcome and
// timestamp.
func insertOutcomeAt(t *testing.T, st *Store, sessionID, serviceID int64, ts time.Time, success bool) {
	t.Helper()
	if _, err := st.InsertCheckResult(CheckResult{
		SessionID: sessionID, ServiceID: serviceID, Timestamp: ts, Success: success,
	}); err != nil {
		t.Fatalf("InsertCheckResult: %v", err)
	}
}
