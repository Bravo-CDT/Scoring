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

	if err := st.ResetRoundData(sess.ID); err != nil {
		t.Fatalf("ResetRoundData: %v", err)
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
