package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/config"
)

// newSession creates a session to hang services off, failing the test if that
// setup step does not work.
func newSession(t *testing.T, st *Store, name string) Session {
	t.Helper()
	sess, err := st.GetOrCreateSession(name, 1000)
	if err != nil {
		t.Fatalf("GetOrCreateSession(%q): %v", name, err)
	}
	return sess
}

// newService creates a service, failing the test if that setup step does not
// work.
func newService(t *testing.T, st *Store, sessionID int64, name string) Service {
	t.Helper()
	svc, err := st.GetOrCreateService(sessionID, name)
	if err != nil {
		t.Fatalf("GetOrCreateService(%d, %q): %v", sessionID, name, err)
	}
	return svc
}

func TestGetOrCreateService_CreatesNew(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")

	before := time.Now().Add(-time.Second)
	svc, err := st.GetOrCreateService(sess.ID, "apache-web")
	if err != nil {
		t.Fatalf("GetOrCreateService: %v", err)
	}
	if svc.ID == 0 {
		t.Error("expected non-zero service ID")
	}
	if svc.SessionID != sess.ID {
		t.Errorf("SessionID = %d, want %d", svc.SessionID, sess.ID)
	}
	if svc.Name != "apache-web" {
		t.Errorf("Name = %q, want apache-web", svc.Name)
	}
	if !svc.Active {
		t.Error("new service should default to Active = true")
	}
	if svc.ScoringPaused {
		t.Error("new service should default to ScoringPaused = false")
	}
	if svc.CreatedAt.Before(before) || svc.CreatedAt.After(time.Now().Add(time.Second)) {
		t.Errorf("CreatedAt = %v, want roughly now", svc.CreatedAt)
	}
}

func TestGetOrCreateService_IdempotentPreservesMutableState(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")
	first := newService(t, st, sess.ID, "apache-web")

	// Both mutable flags are moved away from their creation defaults: a repeat
	// call must return the row as it stands, never re-apply the defaults.
	// Operator intent (a paused or retired service) has to survive an engine
	// restart re-reading the same config.yaml.
	if err := st.SetServiceScoringPaused(first.ID, true); err != nil {
		t.Fatalf("SetServiceScoringPaused: %v", err)
	}
	if err := st.SetServiceActive(first.ID, false); err != nil {
		t.Fatalf("SetServiceActive: %v", err)
	}

	second, err := st.GetOrCreateService(sess.ID, "apache-web")
	if err != nil {
		t.Fatalf("second GetOrCreateService: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected same ID, got %d and %d", first.ID, second.ID)
	}
	if !second.ScoringPaused {
		t.Error("expected ScoringPaused to remain true across idempotent call")
	}
	if second.Active {
		t.Error("expected Active to remain false across idempotent call")
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("CreatedAt = %v, want unchanged %v", second.CreatedAt, first.CreatedAt)
	}

	var count int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM services`).Scan(&count); err != nil {
		t.Fatalf("counting services: %v", err)
	}
	if count != 1 {
		t.Errorf("services row count = %d, want 1", count)
	}
}

func TestGetOrCreateService_IdentityIsSessionAndName(t *testing.T) {
	st := openTemp(t)
	sessA := newSession(t, st, "comp-1")
	sessB := newSession(t, st, "comp-2")

	// The same service name in two sessions is two independent services, and two
	// names in one session are two rows.
	a := newService(t, st, sessA.ID, "apache-web")
	b := newService(t, st, sessB.ID, "apache-web")
	other := newService(t, st, sessA.ID, "ssh")

	if a.ID == b.ID {
		t.Errorf("same name in different sessions shares ID %d", a.ID)
	}
	if a.ID == other.ID {
		t.Errorf("different names in one session share ID %d", a.ID)
	}

	// Pausing one must not touch its namesake in the other session.
	if err := st.SetServiceScoringPaused(a.ID, true); err != nil {
		t.Fatalf("SetServiceScoringPaused: %v", err)
	}
	reloaded, err := st.GetService(b.ID)
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if reloaded.ScoringPaused {
		t.Error("pausing comp-1's apache-web also paused comp-2's")
	}
}

func TestGetOrCreateService_UnknownSessionFails(t *testing.T) {
	st := openTemp(t)

	// A stale or wrong session_id must be rejected by the foreign key rather
	// than creating an orphan service that drops out of every scoring query.
	svc, err := st.GetOrCreateService(424242, "apache-web")
	if err == nil {
		t.Fatalf("GetOrCreateService on a nonexistent session returned %+v, want error", svc)
	}
}

func TestGetOrCreateService_ConcurrentCallersShareOneRow(t *testing.T) {
	// Serializing statements on one connection does not make the lookup and the
	// insert atomic: several callers can all find no service and then all try to
	// create it. Racing a fresh database repeatedly makes that window reliably
	// hit; every caller must still come back with the same single row.
	const databases, callers = 10, 8
	for range databases {
		st := openTemp(t)
		sess := newSession(t, st, "comp-1")

		start := make(chan struct{})
		results := make(chan Service, callers)
		errs := make(chan error, callers)
		var wg sync.WaitGroup
		for range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				svc, err := st.GetOrCreateService(sess.ID, "apache-web")
				if err != nil {
					errs <- err
					return
				}
				results <- svc
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		close(errs)

		for err := range errs {
			t.Fatalf("concurrent GetOrCreateService: %v", err)
		}
		var first Service
		for svc := range results {
			if first.ID == 0 {
				first = svc
				continue
			}
			if svc.ID != first.ID {
				t.Fatalf("concurrent callers disagree: %+v vs %+v", svc, first)
			}
		}

		var count int
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM services`).Scan(&count); err != nil {
			t.Fatalf("counting services: %v", err)
		}
		if count != 1 {
			t.Fatalf("services row count = %d, want 1", count)
		}
	}
}

func TestGetService_RoundTripsEveryField(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")
	created := newService(t, st, sess.ID, "apache-web")

	got, err := st.GetService(created.ID)
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	// created_at is stored as Unix milliseconds and the Service returned at
	// creation time is truncated to match, so the two structs must be equal.
	if got != created {
		t.Errorf("GetService = %+v, want %+v", got, created)
	}
}

func TestGetService_UnknownIDIsNoRows(t *testing.T) {
	st := openTemp(t)

	// Callers (an admin endpoint handed an id from a URL) need to tell "no such
	// service" apart from "the query failed".
	_, err := st.GetService(424242)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetService on unknown id: err = %v, want one matching sql.ErrNoRows", err)
	}
}

func TestListActiveServices_ExcludesInactive(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")

	svcA := newService(t, st, sess.ID, "svc-a")
	svcB := newService(t, st, sess.ID, "svc-b")
	if err := st.SetServiceActive(svcB.ID, false); err != nil {
		t.Fatalf("SetServiceActive: %v", err)
	}

	active, err := st.ListActiveServices(sess.ID)
	if err != nil {
		t.Fatalf("ListActiveServices: %v", err)
	}
	if len(active) != 1 || active[0].ID != svcA.ID {
		t.Fatalf("active = %+v, want only svc-a (id %d)", active, svcA.ID)
	}
	if active[0] != svcA {
		t.Errorf("active[0] = %+v, want %+v", active[0], svcA)
	}

	// A paused service is still active: pausing stops it moving points, it does
	// not retire the service, so the check routines must still see it.
	if err := st.SetServiceScoringPaused(svcA.ID, true); err != nil {
		t.Fatalf("SetServiceScoringPaused: %v", err)
	}
	active, err = st.ListActiveServices(sess.ID)
	if err != nil {
		t.Fatalf("ListActiveServices: %v", err)
	}
	if len(active) != 1 || !active[0].ScoringPaused {
		t.Errorf("active = %+v, want svc-a still listed with ScoringPaused = true", active)
	}
}

func TestListActiveServices_ScopedToSessionAndOrderedByName(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")
	other := newSession(t, st, "comp-2")

	for _, name := range []string{"zulu", "alpha", "mike"} {
		newService(t, st, sess.ID, name)
	}
	newService(t, st, other.ID, "bravo")

	active, err := st.ListActiveServices(sess.ID)
	if err != nil {
		t.Fatalf("ListActiveServices: %v", err)
	}
	var names []string
	for _, svc := range active {
		names = append(names, svc.Name)
	}
	if want := []string{"alpha", "mike", "zulu"}; !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v (another session's services excluded, sorted by name)", names, want)
	}
}

func TestListActiveServices_EmptySession(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")

	active, err := st.ListActiveServices(sess.ID)
	if err != nil {
		t.Fatalf("ListActiveServices: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("active = %+v, want empty", active)
	}
}

func TestSetServiceActive_TogglesBothWays(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")
	svc := newService(t, st, sess.ID, "apache-web")

	for _, active := range []bool{false, true, true} {
		if err := st.SetServiceActive(svc.ID, active); err != nil {
			t.Fatalf("SetServiceActive(%t): %v", active, err)
		}
		got, err := st.GetService(svc.ID)
		if err != nil {
			t.Fatalf("GetService: %v", err)
		}
		if got.Active != active {
			t.Errorf("Active = %t after SetServiceActive(%t)", got.Active, active)
		}
		if got.ScoringPaused {
			t.Error("SetServiceActive must not touch ScoringPaused")
		}
	}
}

func TestSetServiceScoringPaused_TogglesBothWaysAndLeavesActiveAlone(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")
	svc := newService(t, st, sess.ID, "apache-web")

	for _, paused := range []bool{true, false, false} {
		if err := st.SetServiceScoringPaused(svc.ID, paused); err != nil {
			t.Fatalf("SetServiceScoringPaused(%t): %v", paused, err)
		}
		got, err := st.GetService(svc.ID)
		if err != nil {
			t.Fatalf("GetService: %v", err)
		}
		if got.ScoringPaused != paused {
			t.Errorf("ScoringPaused = %t after SetServiceScoringPaused(%t)", got.ScoringPaused, paused)
		}
		if !got.Active {
			t.Error("SetServiceScoringPaused must not touch Active")
		}
	}
}

func TestSetServiceFlags_UnknownIDIsNoRows(t *testing.T) {
	st := openTemp(t)

	// An UPDATE matching nothing is not an SQL error, so without an explicit
	// check an admin endpoint handed a bogus id would report success having
	// changed nothing.
	if err := st.SetServiceActive(424242, false); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("SetServiceActive on unknown id: err = %v, want one matching sql.ErrNoRows", err)
	}
	if err := st.SetServiceScoringPaused(424242, true); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("SetServiceScoringPaused on unknown id: err = %v, want one matching sql.ErrNoRows", err)
	}
}

func TestSetServiceScoringPaused_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scoring.db")

	st1 := openAt(t, path)
	sess := newSession(t, st1, "comp-1")
	svc := newService(t, st1, sess.ID, "apache-web")
	if err := st1.SetServiceScoringPaused(svc.ID, true); err != nil {
		t.Fatalf("SetServiceScoringPaused: %v", err)
	}
	if err := st1.SetServiceActive(svc.ID, false); err != nil {
		t.Fatalf("SetServiceActive: %v", err)
	}
	if err := st1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Simulate a process restart. Unlike the global run state, which Open
	// deliberately resets, per-service operator decisions are meant to survive.
	st2 := openAt(t, path)
	reloaded, err := st2.GetService(svc.ID)
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if !reloaded.ScoringPaused {
		t.Error("expected ScoringPaused = true to survive a simulated restart")
	}
	if reloaded.Active {
		t.Error("expected Active = false to survive a simulated restart")
	}
	if !reloaded.CreatedAt.Equal(svc.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", reloaded.CreatedAt, svc.CreatedAt)
	}
}

func TestInsertConfigSnapshot(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")
	svc := newService(t, st, sess.ID, "apache-web")

	cfg := config.ServiceConfig{
		Name:             "apache-web",
		Host:             "10.0.1.10",
		CheckType:        "http",
		IntervalSeconds:  60,
		PointsPerFailure: 10,
		Params:           map[string]any{"port": 80, "path": "/"},
	}
	before := time.Now().Add(-time.Second).UnixMilli()
	if err := st.InsertConfigSnapshot(svc.ID, cfg); err != nil {
		t.Fatalf("InsertConfigSnapshot: %v", err)
	}

	var host, checkType, paramsJSON string
	var interval, points int
	var appliedAt int64
	row := st.db.QueryRow(
		`SELECT applied_at, host, check_type, interval_seconds, points_per_failure, params_json
		 FROM service_config_snapshots WHERE service_id = ?`, svc.ID,
	)
	if err := row.Scan(&appliedAt, &host, &checkType, &interval, &points, &paramsJSON); err != nil {
		t.Fatalf("querying snapshot: %v", err)
	}
	if host != "10.0.1.10" || checkType != "http" || interval != 60 || points != 10 {
		t.Errorf("snapshot fields unexpected: host=%s type=%s interval=%d points=%d",
			host, checkType, interval, points)
	}
	if appliedAt < before || appliedAt > time.Now().Add(time.Second).UnixMilli() {
		t.Errorf("applied_at = %d, want roughly now", appliedAt)
	}

	// The params are the check's whole configuration, so they have to come back
	// out intact — a snapshot that loses them cannot explain a past result.
	var params map[string]any
	if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
		t.Fatalf("params_json = %q, not valid JSON: %v", paramsJSON, err)
	}
	if want := map[string]any{"port": float64(80), "path": "/"}; !reflect.DeepEqual(params, want) {
		t.Errorf("params = %#v, want %#v", params, want)
	}
}

func TestInsertConfigSnapshot_NilParamsStoresEmptyObject(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")
	svc := newService(t, st, sess.ID, "icmp-host")

	cfg := config.ServiceConfig{Name: "icmp-host", Host: "10.0.1.10", CheckType: "icmp", IntervalSeconds: 30}
	if err := st.InsertConfigSnapshot(svc.ID, cfg); err != nil {
		t.Fatalf("InsertConfigSnapshot: %v", err)
	}

	// A check with no params must not store the JSON literal "null": readers
	// should always find an object there.
	var paramsJSON string
	if err := st.db.QueryRow(
		`SELECT params_json FROM service_config_snapshots WHERE service_id = ?`, svc.ID,
	).Scan(&paramsJSON); err != nil {
		t.Fatalf("querying snapshot: %v", err)
	}
	if paramsJSON != "{}" {
		t.Errorf("params_json = %q, want {}", paramsJSON)
	}
}

func TestInsertConfigSnapshot_KeepsHistory(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")
	svc := newService(t, st, sess.ID, "apache-web")

	// Each applied config is appended, never overwritten: a result scored an
	// hour ago has to stay explainable by the config in force at the time.
	for _, points := range []int{10, 25} {
		cfg := config.ServiceConfig{
			Name: "apache-web", Host: "10.0.1.10", CheckType: "http",
			IntervalSeconds: 60, PointsPerFailure: points,
		}
		if err := st.InsertConfigSnapshot(svc.ID, cfg); err != nil {
			t.Fatalf("InsertConfigSnapshot(%d): %v", points, err)
		}
	}

	rows, err := st.db.Query(
		`SELECT points_per_failure FROM service_config_snapshots WHERE service_id = ? ORDER BY id`,
		svc.ID,
	)
	if err != nil {
		t.Fatalf("querying snapshots: %v", err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var points int
		if err := rows.Scan(&points); err != nil {
			t.Fatalf("scanning snapshot: %v", err)
		}
		got = append(got, points)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating snapshots: %v", err)
	}
	if want := []int{10, 25}; !reflect.DeepEqual(got, want) {
		t.Errorf("points_per_failure history = %v, want %v", got, want)
	}
}

func TestInsertConfigSnapshot_UnknownServiceFails(t *testing.T) {
	st := openTemp(t)

	cfg := config.ServiceConfig{Name: "apache-web", Host: "10.0.1.10", CheckType: "http", IntervalSeconds: 60}
	if err := st.InsertConfigSnapshot(424242, cfg); err == nil {
		t.Error("InsertConfigSnapshot for a nonexistent service succeeded, want foreign-key violation")
	}
}

func TestGetServiceByName_NotFoundDoesNotCreate(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")

	_, found, err := st.GetServiceByName(sess.ID, "ghost")
	if err != nil {
		t.Fatalf("GetServiceByName: %v", err)
	}
	if found {
		t.Fatal("expected found=false for an unknown name")
	}

	var count int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM services`).Scan(&count); err != nil {
		t.Fatalf("counting services: %v", err)
	}
	if count != 0 {
		t.Errorf("GetServiceByName must not create a row; found %d services", count)
	}
}

func TestGetServiceByName_Found(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")
	created := newService(t, st, sess.ID, "apache-web")

	svc, found, err := st.GetServiceByName(sess.ID, "apache-web")
	if err != nil || !found {
		t.Fatalf("GetServiceByName: svc=%+v found=%v err=%v", svc, found, err)
	}
	if !reflect.DeepEqual(svc, created) {
		t.Errorf("GetServiceByName = %+v, want %+v", svc, created)
	}
}

func TestGetServiceByName_ReturnsInactiveServices(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")
	created := newService(t, st, sess.ID, "apache-web")
	if err := st.SetServiceActive(created.ID, false); err != nil {
		t.Fatalf("SetServiceActive: %v", err)
	}

	// A retired service still exists; what inactive means is the caller's call.
	svc, found, err := st.GetServiceByName(sess.ID, "apache-web")
	if err != nil || !found {
		t.Fatalf("GetServiceByName: svc=%+v found=%v err=%v", svc, found, err)
	}
	if svc.Active {
		t.Error("Active = true, want false")
	}
}

func TestGetServiceByName_ScopedToSession(t *testing.T) {
	st := openTemp(t)
	a := newSession(t, st, "comp-a")
	b := newSession(t, st, "comp-b")
	newService(t, st, a.ID, "apache-web")

	_, found, err := st.GetServiceByName(b.ID, "apache-web")
	if err != nil {
		t.Fatalf("GetServiceByName: %v", err)
	}
	if found {
		t.Error("found a service from another session")
	}
}
