package store

import (
	"errors"
	"testing"
	"time"
)

func newHunt(t *testing.T, st *Store, sessionID int64, team string, points int) Hunt {
	t.Helper()
	h, err := st.CreateHunt(sessionID, Hunt{Team: team, Title: team + " hunt", Code: "code-" + team, Points: points, Deadline: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("CreateHunt: %v", err)
	}
	return h
}

func TestHunts_ListByTeam(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")
	blue := newHunt(t, st, sess.ID, SideBlue, 100)
	red := newHunt(t, st, sess.ID, SideRed, 100)
	either := newHunt(t, st, sess.ID, "either", 100)

	for _, tc := range []struct {
		team string
		want []int64
	}{
		{SideBlue, []int64{blue.ID, either.ID}},
		{SideRed, []int64{red.ID, either.ID}},
		{"", []int64{blue.ID, red.ID, either.ID}},
	} {
		got, err := st.ListHunts(sess.ID, tc.team)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, h := range got {
			ids = append(ids, h.ID)
		}
		if len(ids) != len(tc.want) {
			t.Fatalf("ListHunts(%q) = %v, want %v", tc.team, ids, tc.want)
		}
		for i := range ids {
			if ids[i] != tc.want[i] {
				t.Fatalf("ListHunts(%q) = %v, want %v", tc.team, ids, tc.want)
			}
		}
	}
}

func TestClaimHunt_RecordsLedgerOnce(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")
	h := newHunt(t, st, sess.ID, SideRed, 300)

	got, err := st.ClaimHunt(sess.ID, h.ID, SideRed, SideBlue, SideRed, "found it")
	if err != nil {
		t.Fatalf("ClaimHunt: %v", err)
	}
	if got.ClaimedBy != SideRed || got.ClaimedAt == nil || got.PointsMoved == nil || *got.PointsMoved != 300 {
		t.Errorf("claimed hunt = %+v, want claimed by red moving 300", got)
	}
	wantScore(t, st, sess.ID, 700, 300)

	if _, err := st.ClaimHunt(sess.ID, h.ID, SideBlue, SideManual, SideBlue, "again"); !errors.Is(err, ErrHuntClaimed) {
		t.Errorf("second claim error = %v, want ErrHuntClaimed", err)
	}
	wantScore(t, st, sess.ID, 700, 300)
}
