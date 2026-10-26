package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newInject(t *testing.T, st *Store, sessionID int64, team string) Inject {
	t.Helper()
	in, err := st.CreateInject(sessionID, team, team+" inject", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateInject: %v", err)
	}
	return in
}

func wantScore(t *testing.T, st *Store, sessionID int64, wantBlue, wantRed int) {
	t.Helper()
	blue, red, err := st.GetScore(sessionID)
	if err != nil || blue != wantBlue || red != wantRed {
		t.Fatalf("GetScore = (%d, %d, %v), want (%d, %d)", blue, red, err, wantBlue, wantRed)
	}
}

func TestInjects_CreateListByTeam(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")
	blue := newInject(t, st, sess.ID, SideBlue)
	newInject(t, st, sess.ID, SideRed)

	got, err := st.ListInjects(sess.ID, SideBlue)
	if err != nil || len(got) != 1 || got[0].ID != blue.ID || got[0].Open {
		t.Fatalf("ListInjects(blue) = %+v, %v; want just the closed blue inject", got, err)
	}
	if all, err := st.ListInjects(sess.ID, ""); err != nil || len(all) != 2 {
		t.Fatalf("ListInjects(all) = %+v, %v; want both", all, err)
	}
}

func TestPutSubmission_ReplacesAndReturnsPrevious(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")
	in := newInject(t, st, sess.ID, SideBlue)

	first := Submission{InjectID: in.ID, Note: "a", FileName: "a.pdf", StoredName: "s1", Size: 1, SubmittedAt: time.Now()}
	if prev, err := st.PutSubmission(sess.ID, first); err != nil || prev != nil {
		t.Fatalf("first PutSubmission = %v, %v; want no previous", prev, err)
	}
	second := first
	second.StoredName, second.Note = "s2", "b"
	prev, err := st.PutSubmission(sess.ID, second)
	if err != nil || prev == nil || prev.StoredName != "s1" {
		t.Fatalf("second PutSubmission = %+v, %v; want previous s1", prev, err)
	}
	if got, ok, err := st.GetSubmission(sess.ID, in.ID); err != nil || !ok || got.Note != "b" {
		t.Fatalf("GetSubmission = %+v, %v, %v; want note b", got, ok, err)
	}
}

func TestApproveInject_RecordsLedgerOnce(t *testing.T) {
	st := openTemp(t)
	sess, err := st.GetOrCreateSession("comp-1", 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	in := newInject(t, st, sess.ID, SideRed)

	got, err := st.ApproveInject(sess.ID, in.ID, SideBlue, SideRed, 7, 21000, "inject 1")
	if err != nil || got.Points == nil || *got.Points != 21000 || got.ApprovedAt == nil {
		t.Fatalf("ApproveInject = %+v, %v", got, err)
	}
	wantScore(t, st, sess.ID, 1_000_000-21000, 21000)

	if _, err := st.ApproveInject(sess.ID, in.ID, SideBlue, SideRed, 7, 21000, "again"); !errors.Is(err, ErrInjectApproved) {
		t.Fatalf("second ApproveInject err = %v, want ErrInjectApproved", err)
	}
	wantScore(t, st, sess.ID, 1_000_000-21000, 21000)
}

func TestVoidLedgerEntry_RestoresScore(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")
	in := newInject(t, st, sess.ID, SideRed)
	if _, err := st.ApproveInject(sess.ID, in.ID, SideBlue, SideRed, 5, 500, "x"); err != nil {
		t.Fatal(err)
	}

	if ok, err := st.VoidLedgerEntry(sess.ID, SourceInject, in.ID, "deleted"); err != nil || !ok {
		t.Fatalf("VoidLedgerEntry = %v, %v; want true", ok, err)
	}
	wantScore(t, st, sess.ID, 1000, 0)
	if ok, err := st.VoidLedgerEntry(sess.ID, SourceInject, in.ID, "deleted"); err != nil || ok {
		t.Fatalf("second VoidLedgerEntry = %v, %v; want false", ok, err)
	}
}

func TestScoreBreakdown_SumsBySource(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")
	if _, err := st.RecordLedgerEntry(sess.ID, "service_check", 1, SideBlue, SideRed, 10, "down"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdjustScore(sess.ID, SideBlue, 5, "fix"); err != nil {
		t.Fatal(err)
	}
	in := newInject(t, st, sess.ID, SideBlue)
	if _, err := st.ApproveInject(sess.ID, in.ID, SideManual, SideBlue, 10, 3000, "x"); err != nil {
		t.Fatal(err)
	}

	blue, red, err := st.ScoreBreakdown(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := (SideBreakdown{Starting: 1000, Services: -10, Injects: 3000, Manual: 5}); blue != want {
		t.Errorf("blue = %+v, want %+v", blue, want)
	}
	if want := (SideBreakdown{Services: 10}); red != want {
		t.Errorf("red = %+v, want %+v", red, want)
	}
	b, r, _ := st.GetScore(sess.ID)
	if sum := blue.Starting + blue.Services + blue.Injects + blue.Manual; sum != b || red.Services+red.Injects+red.Manual != r {
		t.Errorf("breakdown sums (%d) don't match GetScore (%d, %d)", sum, b, r)
	}
}

// A database made before injects had a released column gets one on Open,
// set for every inject that was already open or approved.
func TestOpen_AddsReleasedToOlderInjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scoring.db")
	st := openAt(t, path)
	sess := newSession(t, st, "comp-1")
	in := newInject(t, st, sess.ID, SideBlue)
	in.Open = true
	if err := st.UpdateInject(sess.ID, in); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`ALTER TABLE injects DROP COLUMN released`); err != nil {
		t.Fatal(err)
	}

	st = openAt(t, path)
	if got, ok, err := st.GetInject(sess.ID, in.ID); err != nil || !ok || !got.Released {
		t.Fatalf("after reopening: %+v, %v, %v; want released", got, ok, err)
	}
}
