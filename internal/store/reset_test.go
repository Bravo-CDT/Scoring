package store

import (
	"testing"
	"time"
)

func TestResetRoundData_ClearsScoreAndCheckHistoryButNothingElse(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")
	svc := newService(t, st, sess.ID, "svc1")
	insertResultAt(t, st, sess.ID, svc.ID, time.Now())
	if _, err := st.RecordLedgerEntry(sess.ID, "service_check", 1, SideBlue, SideRed, 10, "svc1 down"); err != nil {
		t.Fatalf("RecordLedgerEntry: %v", err)
	}
	insertEventAt(t, st, sess.ID, "something_happened", time.Now())
	if err := st.SetCredentialOverride(sess.ID, CredentialOverride{Service: "svc1", Username: "u", Password: "p", ChangedAt: time.Now()}); err != nil {
		t.Fatalf("SetCredentialOverride: %v", err)
	}

	in := newInject(t, st, sess.ID, SideBlue)
	if _, err := st.PutSubmission(sess.ID, Submission{InjectID: in.ID, StoredName: "sub-1", SubmittedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	in.Open, in.Grade = true, new(int)
	if err := st.UpdateInject(sess.ID, in); err != nil {
		t.Fatal(err)
	}

	deleted, err := st.ResetRoundData(sess.ID)
	if err != nil {
		t.Fatalf("ResetRoundData: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "sub-1" {
		t.Errorf("deleted files = %v, want [sub-1]", deleted)
	}
	if _, ok, _ := st.GetSubmission(sess.ID, in.ID); ok {
		t.Error("submission survived the reset")
	}
	if got, ok, _ := st.GetInject(sess.ID, in.ID); !ok || got.Open || got.Grade != nil {
		t.Errorf("inject after reset = %+v, %v; want it kept, closed and ungraded", got, ok)
	}
	if overrides, err := st.CredentialOverrides(sess.ID); err != nil || len(overrides) != 0 {
		t.Fatalf("credential overrides after reset = %v, %v; want none", overrides, err)
	}

	blue, red, err := st.GetScore(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if blue != 1000 || red != 0 {
		t.Fatalf("GetScore after reset = (%d, %d), want (1000, 0)", blue, red)
	}

	latest, err := st.GetLatestCheckResult(svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest != nil {
		t.Fatalf("expected no check results after reset, got %+v", latest)
	}

	events, err := st.ListEvents(sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("events must survive a round reset untouched")
	}

	svcAfter, found, err := st.GetServiceByName(sess.ID, "svc1")
	if err != nil || !found {
		t.Fatalf("the service row must survive a round reset untouched: found=%v, err=%v", found, err)
	}
	if svcAfter.ID != svc.ID {
		t.Fatalf("service id changed across reset: %d -> %d", svc.ID, svcAfter.ID)
	}
}
