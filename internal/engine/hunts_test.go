package engine

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func newHuntsHarness(t *testing.T) *harness { return newInjectsHarness(t, "") }

// openHunt creates a hunt for team, open until an hour from now.
func (h *harness) openHunt(t *testing.T, team, code string, points int) HuntView {
	t.Helper()
	in := HuntInput{Title: "find it", Team: team, Description: "look around", Code: code, Points: points, Deadline: time.Now().Add(time.Hour)}
	v, err := h.e.CreateHunt(in)
	if err != nil {
		t.Fatalf("CreateHunt: %v", err)
	}
	v, err = h.e.UpdateHunt(v.ID, in, true)
	if err != nil {
		t.Fatalf("opening hunt: %v", err)
	}
	return v
}

func TestHunts_Visibility(t *testing.T) {
	for _, tc := range []struct {
		team, huntTeam string
		sees           bool
	}{
		{TeamBlue, TeamEither, true},
		{TeamRed, TeamEither, true},
		{TeamBlue, TeamRed, false},
	} {
		t.Run(tc.team+"/"+tc.huntTeam, func(t *testing.T) {
			h := newHuntsHarness(t)
			in := HuntInput{Title: "t", Team: tc.huntTeam, Description: "secret place", Code: "abc", Points: 5, Deadline: time.Now().Add(time.Hour)}
			v, err := h.e.CreateHunt(in)
			if err != nil {
				t.Fatal(err)
			}
			got, err := h.e.Hunts(tc.team)
			if err != nil || (len(got) == 1) != tc.sees {
				t.Fatalf("Hunts(%s) = %+v, %v; sees = %v", tc.team, got, err, tc.sees)
			}
			if tc.sees && (got[0].Code != "" || got[0].Description != "") {
				t.Errorf("unopened hunt shows team code %q / description %q", got[0].Code, got[0].Description)
			}
			if _, err := h.e.UpdateHunt(v.ID, in, true); err != nil {
				t.Fatal(err)
			}
			got, _ = h.e.Hunts(tc.team)
			if tc.sees && (got[0].Code != "" || got[0].Description != "secret place") {
				t.Errorf("opened hunt = %+v, want description and no code", got[0])
			}
			if all, _ := h.e.Hunts(""); len(all) != 1 || all[0].Code != "abc" {
				t.Errorf("Hunts(admin) = %+v, want the code", all)
			}
		})
	}
}

func TestClaimHunt_MovesPoints(t *testing.T) {
	for _, tc := range []struct {
		name, claimer, huntTeam string
		blue, red               int
	}{
		{"blue", TeamBlue, TeamEither, 1100, 0},
		{"red", TeamRed, TeamRed, 900, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHuntsHarness(t)
			hu := h.openHunt(t, tc.huntTeam, "abc", 100)
			got, err := h.e.ClaimHunt(tc.claimer, hu.ID, "abc")
			if err != nil || got.ClaimedBy != tc.claimer || got.PointsMoved == nil || got.Claimable {
				t.Fatalf("ClaimHunt = %+v, %v", got, err)
			}
			h.wantScore(t, tc.blue, tc.red)
			b, err := h.e.ScoreBreakdown()
			if err != nil || b.Blue.ScavengerHunt != tc.blue-1000 || b.Red.ScavengerHunt != tc.red {
				t.Errorf("breakdown = %+v, %v", b, err)
			}
			if b.Blue.Total != tc.blue || b.Red.Total != tc.red {
				t.Errorf("totals = %d, %d; want %d, %d", b.Blue.Total, b.Red.Total, tc.blue, tc.red)
			}
		})
	}
}

func TestClaimHunt_Refusals(t *testing.T) {
	h := newHuntsHarness(t)
	open := h.openHunt(t, TeamEither, "Code", 100)
	closed := h.openHunt(t, TeamEither, "Code", 100)
	if _, err := h.e.UpdateHunt(closed.ID, HuntInput{Title: "t", Description: "", Code: "Code", Points: 100, Deadline: time.Now().Add(time.Hour)}, false); err != nil {
		t.Fatal(err)
	}
	late := h.openHunt(t, TeamEither, "Code", 100)
	if _, err := h.e.UpdateHunt(late.ID, HuntInput{Title: "t", Code: "Code", Points: 100, Deadline: time.Now().Add(-time.Second)}, true); err != nil {
		t.Fatal(err)
	}
	redOnly := h.openHunt(t, TeamRed, "Code", 100)
	taken := h.openHunt(t, TeamEither, "Code", 100)
	if _, err := h.e.ClaimHunt(TeamRed, taken.ID, "Code"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		team  string
		id    int64
		code  string
		wantE error
	}{
		{"wrong code", TeamBlue, open.ID, "nope", ErrWrongCode},
		{"wrong case", TeamBlue, open.ID, "code", ErrWrongCode},
		{"closed", TeamBlue, closed.ID, "Code", ErrHuntClosed},
		{"past deadline", TeamBlue, late.ID, "Code", ErrHuntClosed},
		{"other team's", TeamBlue, redOnly.ID, "Code", ErrHuntNotFound},
		{"missing", TeamBlue, 9999, "Code", ErrHuntNotFound},
		{"claimed", TeamBlue, taken.ID, "Code", ErrHuntClaimed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := h.status(t)
			if _, err := h.e.ClaimHunt(tc.team, tc.id, tc.code); !errors.Is(err, tc.wantE) {
				t.Errorf("err = %v, want %v", err, tc.wantE)
			}
			if s := h.status(t); s.BlueScore != before.BlueScore || s.RedScore != before.RedScore {
				t.Errorf("score moved: (%d, %d) -> (%d, %d)", before.BlueScore, before.RedScore, s.BlueScore, s.RedScore)
			}
		})
	}
}

func TestClaimHunt_RaceOnlyOneWins(t *testing.T) {
	h := newHuntsHarness(t)
	hu := h.openHunt(t, TeamEither, "abc", 100)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, team := range []string{TeamBlue, TeamRed} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = h.e.ClaimHunt(team, hu.ID, "abc")
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, ErrHuntClaimed):
			t.Errorf("loser err = %v, want ErrHuntClaimed", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d claims succeeded, want 1 (%v)", wins, errs)
	}
	if s := h.status(t); !(s.BlueScore == 1100 && s.RedScore == 0) && !(s.BlueScore == 900 && s.RedScore == 100) {
		t.Errorf("score = (%d, %d), want exactly one claim's worth", s.BlueScore, s.RedScore)
	}
}

func TestClaimHunt_Cooldown(t *testing.T) {
	h := newHuntsHarness(t)
	hu := h.openHunt(t, TeamEither, "abc", 100)
	for i := 0; i < 5; i++ {
		if _, err := h.e.ClaimHunt(TeamBlue, hu.ID, "bad"); !errors.Is(err, ErrWrongCode) {
			t.Fatalf("attempt %d err = %v", i, err)
		}
	}
	_, err := h.e.ClaimHunt(TeamBlue, hu.ID, "abc")
	var ce *CooldownError
	if !errors.Is(err, ErrCooldown) || !errors.As(err, &ce) || ce.RetryAfter <= 0 {
		t.Fatalf("err = %v, want a CooldownError", err)
	}
	if got, _ := h.e.Hunts(""); got[0].ClaimedBy != "" {
		t.Error("hunt claimed during cooldown")
	}
	if _, err := h.e.ClaimHunt(TeamRed, hu.ID, "abc"); err != nil {
		t.Errorf("red was locked by blue's cooldown: %v", err)
	}
}

func TestClaimHunt_ClosedAttemptsDontCount(t *testing.T) {
	h := newHuntsHarness(t)
	closed, err := h.e.CreateHunt(HuntInput{Title: "t", Team: TeamEither, Code: "abc", Points: 5, Deadline: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := h.e.ClaimHunt(TeamBlue, closed.ID, "bad"); !errors.Is(err, ErrHuntClosed) {
			t.Fatalf("err = %v, want ErrHuntClosed", err)
		}
	}
	hu := h.openHunt(t, TeamEither, "abc", 5)
	if _, err := h.e.ClaimHunt(TeamBlue, hu.ID, "abc"); err != nil {
		t.Errorf("claim after closed attempts: %v", err)
	}
}

func TestDeleteHunt_RestoresScore(t *testing.T) {
	h := newHuntsHarness(t)
	hu := h.openHunt(t, TeamEither, "abc", 100)
	if _, err := h.e.ClaimHunt(TeamBlue, hu.ID, "abc"); err != nil {
		t.Fatal(err)
	}
	h.wantScore(t, 1100, 0)
	if err := h.e.DeleteHunt(hu.ID); err != nil {
		t.Fatal(err)
	}
	h.wantScore(t, 1000, 0)
	if got, err := h.e.Hunts(""); err != nil || len(got) != 0 {
		t.Errorf("Hunts = %+v, %v; want none", got, err)
	}
	if err := h.e.DeleteHunt(hu.ID); !errors.Is(err, ErrHuntNotFound) {
		t.Errorf("second delete err = %v, want ErrHuntNotFound", err)
	}
}

func TestResetRound_ReopensHunts(t *testing.T) {
	h := newHuntsHarness(t)
	hu := h.openHunt(t, TeamEither, "abc", 100)
	if _, err := h.e.ClaimHunt(TeamBlue, hu.ID, "abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.ResetRound(false); err != nil {
		t.Fatal(err)
	}
	got, err := h.e.Hunts("")
	if err != nil || len(got) != 1 || got[0].ClaimedBy != "" || got[0].Open {
		t.Fatalf("after reset = %+v, %v; want kept, unclaimed, closed", got, err)
	}
	in := HuntInput{Title: "find it", Description: "x", Code: "abc", Points: 100, Deadline: time.Now().Add(time.Hour)}
	if _, err := h.e.UpdateHunt(hu.ID, in, true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.ClaimHunt(TeamRed, hu.ID, "abc"); err != nil {
		t.Errorf("claim after reset: %v", err)
	}
}

func TestUpdateHunt_RefusedOnceClaimed(t *testing.T) {
	h := newHuntsHarness(t)
	hu := h.openHunt(t, TeamEither, "abc", 100)
	if _, err := h.e.ClaimHunt(TeamBlue, hu.ID, "abc"); err != nil {
		t.Fatal(err)
	}
	in := HuntInput{Title: "t", Code: "new", Points: 1, Deadline: time.Now().Add(time.Hour)}
	if _, err := h.e.UpdateHunt(hu.ID, in, true); !errors.Is(err, ErrHuntClaimed) {
		t.Errorf("err = %v, want ErrHuntClaimed", err)
	}
}

func TestCreateHunt_Invalid(t *testing.T) {
	ok := HuntInput{Title: "t", Team: TeamBlue, Code: "abc", Points: 1, Deadline: time.Now().Add(time.Hour)}
	for _, tc := range []struct {
		name string
		edit func(*HuntInput)
	}{
		{"empty code", func(in *HuntInput) { in.Code = "" }},
		{"long code", func(in *HuntInput) { in.Code = strings.Repeat("a", 201) }},
		{"zero points", func(in *HuntInput) { in.Points = 0 }},
		{"bad team", func(in *HuntInput) { in.Team = "green" }},
		{"long description", func(in *HuntInput) { in.Description = strings.Repeat("a", MaxHuntDescription+1) }},
		{"no deadline", func(in *HuntInput) { in.Deadline = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHuntsHarness(t)
			in := ok
			tc.edit(&in)
			if _, err := h.e.CreateHunt(in); !errors.Is(err, ErrInvalidHunt) {
				t.Errorf("err = %v, want ErrInvalidHunt", err)
			}
		})
	}
}
