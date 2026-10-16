package store

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// insertEventAt records one engine-level event at an explicit timestamp, so a
// test can control the (timestamp, id) ordering ListEvents pages by. It returns
// the new row's id, read back from the listing since InsertEvent reports only
// an error.
func insertEventAt(t *testing.T, st *Store, sessionID int64, eventType string, ts time.Time) int64 {
	t.Helper()
	if err := st.InsertEvent(Event{
		SessionID: sessionID, Level: "info", Actor: "system",
		Type: eventType, Message: eventType, Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertEvent(%s): %v", eventType, err)
	}
	var id int64
	if err := st.db.QueryRow(`SELECT MAX(id) FROM events`).Scan(&id); err != nil {
		t.Fatalf("reading back the new event id: %v", err)
	}
	return id
}

func collectEventIDs(events []Event) []int64 {
	ids := make([]int64, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}
	return ids
}

func TestInsertAndListEvents(t *testing.T) {
	st := openTemp(t)
	sessionID, serviceID := setupServiceForResults(t, st, "apache-web")

	ts := time.UnixMilli(1_700_000_000_000).UTC()
	if err := st.InsertEvent(Event{
		SessionID: sessionID, ServiceID: &serviceID, Level: "info", Actor: "system",
		Type: "service_down", Message: "apache-web went down", Detail: "connect: refused",
		Timestamp: ts,
	}); err != nil {
		t.Fatalf("InsertEvent (service-scoped): %v", err)
	}
	if err := st.InsertEvent(Event{
		SessionID: sessionID, ServiceID: nil, Level: "warning", Actor: "admin",
		Type: "engine_stopped", Message: "engine stopped",
		Timestamp: ts.Add(time.Second),
	}); err != nil {
		t.Fatalf("InsertEvent (engine-level): %v", err)
	}

	events, err := st.ListEvents(sessionID, 10, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}

	// Newest first, and the engine-level event keeps a nil ServiceID.
	newest := events[0]
	if newest.Type != "engine_stopped" || newest.ServiceID != nil ||
		newest.Level != "warning" || newest.Actor != "admin" ||
		newest.Message != "engine stopped" || newest.Detail != "" ||
		!newest.Timestamp.Equal(ts.Add(time.Second)) {
		t.Errorf("events[0] = %+v, unexpected", newest)
	}
	if newest.SessionID != sessionID || newest.ID == 0 {
		t.Errorf("events[0] = %+v, want session %d and a non-zero id", newest, sessionID)
	}
	if newest.Timestamp.Location() != time.UTC {
		t.Errorf("events[0].Timestamp location = %v, want UTC", newest.Timestamp.Location())
	}

	oldest := events[1]
	if oldest.Type != "service_down" || oldest.ServiceID == nil || *oldest.ServiceID != serviceID ||
		oldest.Level != "info" || oldest.Actor != "system" ||
		oldest.Message != "apache-web went down" || oldest.Detail != "connect: refused" ||
		!oldest.Timestamp.Equal(ts) {
		t.Errorf("events[1] = %+v, unexpected", oldest)
	}
}

func TestInsertEvent_DefaultsZeroTimestampToNow(t *testing.T) {
	st := openTemp(t)
	sessionID, _ := setupServiceForResults(t, st, "apache-web")

	before := time.Now().UTC().Add(-time.Second)
	if err := st.InsertEvent(Event{
		SessionID: sessionID, Level: "info", Actor: "system",
		Type: "engine_started", Message: "engine started",
	}); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	events, err := st.ListEvents(sessionID, 10, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	if events[0].Timestamp.Before(before) {
		t.Errorf("Timestamp = %v, want roughly now (after %v)", events[0].Timestamp, before)
	}
}

func TestInsertEvent_CapsAndSanitizesDetail(t *testing.T) {
	st := openTemp(t)
	sessionID, _ := setupServiceForResults(t, st, "apache-web")

	// Event detail carries the same untrusted check output as a check result's,
	// so it gets the same cap and the same UTF-8 scrubbing.
	detail := "probe said: \xff\xfe " + strings.Repeat("y", MaxDetailBytes*2)
	if err := st.InsertEvent(Event{
		SessionID: sessionID, Level: "error", Actor: "system",
		Type: "check_failed", Message: "check failed", Detail: detail,
		Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	events, err := st.ListEvents(sessionID, 10, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	stored := events[0].Detail
	if len(stored) > MaxDetailBytes {
		t.Errorf("len(Detail) = %d, want at most MaxDetailBytes (%d)", len(stored), MaxDetailBytes)
	}
	if !strings.Contains(stored, "truncated") {
		t.Errorf("Detail has no truncation marker; it ends %q", lastBytes(stored, 32))
	}
	if !utf8.ValidString(stored) {
		t.Errorf("Detail is not valid UTF-8: %q", firstBytes(stored, 32))
	}
	if strings.Contains(stored, "\xff") || strings.Contains(stored, "\xfe") {
		t.Errorf("Detail still carries the invalid bytes: %q", firstBytes(stored, 32))
	}
	if !strings.HasPrefix(stored, "probe said: ") {
		t.Errorf("Detail lost the start of the message: %q", firstBytes(stored, 32))
	}
}

func TestListEvents_PagesNewestFirst(t *testing.T) {
	st := openTemp(t)
	sessionID, _ := setupServiceForResults(t, st, "apache-web")

	base := time.UnixMilli(1_700_000_000_000).UTC()
	var ids []int64 // oldest first
	for i := range 5 {
		ids = append(ids, insertEventAt(t, st, sessionID, "tick", base.Add(time.Duration(i)*time.Second)))
	}

	page, err := st.ListEvents(sessionID, 3, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if want := []int64{ids[4], ids[3], ids[2]}; !equalIDs(collectEventIDs(page), want) {
		t.Fatalf("first page ids = %v, want %v", collectEventIDs(page), want)
	}

	next, err := st.ListEvents(sessionID, 3, page[len(page)-1].ID)
	if err != nil {
		t.Fatalf("ListEvents page 2: %v", err)
	}
	if want := []int64{ids[1], ids[0]}; !equalIDs(collectEventIDs(next), want) {
		t.Fatalf("second page ids = %v, want %v", collectEventIDs(next), want)
	}

	last, err := st.ListEvents(sessionID, 3, next[len(next)-1].ID)
	if err != nil {
		t.Fatalf("ListEvents page 3: %v", err)
	}
	if len(last) != 0 {
		t.Errorf("third page = %v, want empty", collectEventIDs(last))
	}
}

func TestListEvents_PagesWhenTimestampsAreOutOfOrder(t *testing.T) {
	st := openTemp(t)
	sessionID, _ := setupServiceForResults(t, st, "apache-web")

	// Events are written by concurrent goroutines, so insertion order does not
	// have to match timestamp order; paging must still visit every row once.
	base := time.UnixMilli(1_700_000_000_000).UTC()
	seconds := []int{2, 0, 3, 1}
	byDelay := map[int]int64{}
	for _, sec := range seconds {
		byDelay[sec] = insertEventAt(t, st, sessionID, "tick", base.Add(time.Duration(sec)*time.Second))
	}
	want := []int64{byDelay[3], byDelay[2], byDelay[1], byDelay[0]}

	var got []int64
	beforeID := int64(0)
	for range len(seconds) {
		page, err := st.ListEvents(sessionID, 2, beforeID)
		if err != nil {
			t.Fatalf("ListEvents(beforeID=%d): %v", beforeID, err)
		}
		if len(page) == 0 {
			break
		}
		got = append(got, collectEventIDs(page)...)
		beforeID = page[len(page)-1].ID
	}
	if !equalIDs(got, want) {
		t.Errorf("paged ids = %v, want %v", got, want)
	}
}

func TestListEvents_BreaksMillisecondTiesByID(t *testing.T) {
	st := openTemp(t)
	sessionID, _ := setupServiceForResults(t, st, "apache-web")

	ts := time.UnixMilli(1_700_000_000_000).UTC()
	var ids []int64
	for range 3 {
		ids = append(ids, insertEventAt(t, st, sessionID, "tick", ts))
	}

	page, err := st.ListEvents(sessionID, 2, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if want := []int64{ids[2], ids[1]}; !equalIDs(collectEventIDs(page), want) {
		t.Fatalf("first page ids = %v, want %v", collectEventIDs(page), want)
	}

	next, err := st.ListEvents(sessionID, 2, page[1].ID)
	if err != nil {
		t.Fatalf("ListEvents page 2: %v", err)
	}
	if want := []int64{ids[0]}; !equalIDs(collectEventIDs(next), want) {
		t.Errorf("second page ids = %v, want %v", collectEventIDs(next), want)
	}
}

func TestListEvents_IsScopedToOneSession(t *testing.T) {
	st := openTemp(t)
	sessionOne, _ := setupServiceForResults(t, st, "apache-web")
	other, err := st.GetOrCreateSession("comp-2", 1000)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}

	base := time.UnixMilli(1_700_000_000_000).UTC()
	mine := insertEventAt(t, st, sessionOne, "tick", base)
	insertEventAt(t, st, other.ID, "tick", base.Add(time.Second))

	events, err := st.ListEvents(sessionOne, 10, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if want := []int64{mine}; !equalIDs(collectEventIDs(events), want) {
		t.Errorf("ids = %v, want %v", collectEventIDs(events), want)
	}
}

func TestListEvents_NoEventsIsEmptyNotAnError(t *testing.T) {
	st := openTemp(t)
	sessionID, _ := setupServiceForResults(t, st, "apache-web")

	events, err := st.ListEvents(sessionID, 10, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("ids = %v, want empty", collectEventIDs(events))
	}
}

func TestListEvents_NonPositiveLimitIsBounded(t *testing.T) {
	st := openTemp(t)
	sessionID, _ := setupServiceForResults(t, st, "apache-web")

	base := time.UnixMilli(1_700_000_000_000).UTC()
	for i := range 3 {
		insertEventAt(t, st, sessionID, "tick", base.Add(time.Duration(i)*time.Second))
	}

	for _, limit := range []int{0, -1} {
		events, err := st.ListEvents(sessionID, limit, 0)
		if err != nil {
			t.Fatalf("ListEvents(limit=%d): %v", limit, err)
		}
		if len(events) == 0 || len(events) > defaultListLimit {
			t.Errorf("ListEvents(limit=%d) returned %d rows, want between 1 and %d",
				limit, len(events), defaultListLimit)
		}
	}
}

func TestInsertEvent_RejectsUnknownSession(t *testing.T) {
	st := openTemp(t)

	if err := st.InsertEvent(Event{
		SessionID: 424242, Level: "info", Actor: "system",
		Type: "engine_started", Message: "engine started",
	}); err == nil {
		t.Error("InsertEvent for a nonexistent session succeeded, want a foreign-key error")
	}
}
