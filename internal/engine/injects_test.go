package engine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// newInjectsHarness is a harness whose config has blue_team and red_team
// sections plus extra (whose starting_score, if any, replaces the config's),
// with both logins on and uploads in a temp dir.
func newInjectsHarness(t *testing.T, extra string) *harness {
	t.Helper()
	h := newHarness(t, okCheck, "web", "db")
	cfg := credentialsConfig() + fmt.Sprintf("red_team:\n  username: red\n  password_hash: %q\n", adminHash) + extra
	if strings.Contains(extra, "starting_score:") {
		cfg = strings.Replace(cfg, "starting_score: 1000\n", "", 1)
	}
	h.writeConfig(t, cfg)
	if _, err := h.e.Reload(""); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	h.e.EnableBlueTeamLogin()
	h.e.EnableRedTeamLogin()
	h.e.SetUploadsDir(t.TempDir())
	return h
}

// openInject creates an inject for team, open until an hour from now.
func (h *harness) openInject(t *testing.T, team string) InjectView {
	t.Helper()
	in, err := h.e.CreateInject(InjectInput{Title: team + " report", Team: team, Deadline: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("CreateInject: %v", err)
	}
	in, err = h.e.UpdateInject(in.ID, in.Title, in.Deadline, true)
	if err != nil {
		t.Fatalf("opening inject: %v", err)
	}
	return in
}

// gradedInject is an inject for team with a submission, graded grade and
// closed, ready to approve.
func (h *harness) gradedInject(t *testing.T, team string, grade int) InjectView {
	t.Helper()
	in := h.openInject(t, team)
	if _, err := h.e.Submit(team, in.ID, "note", "a.txt", strings.NewReader("answer")); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := h.e.GradeInject(in.ID, grade); err != nil {
		t.Fatalf("GradeInject: %v", err)
	}
	in, err := h.e.UpdateInject(in.ID, in.Title, in.Deadline, false)
	if err != nil {
		t.Fatalf("closing inject: %v", err)
	}
	return in
}

func (h *harness) wantScore(t *testing.T, blue, red int) {
	t.Helper()
	if s := h.status(t); s.BlueScore != blue || s.RedScore != red {
		t.Fatalf("score = (%d, %d), want (%d, %d)", s.BlueScore, s.RedScore, blue, red)
	}
}

func uploadsLeft(t *testing.T, h *harness) []string {
	t.Helper()
	entries, err := os.ReadDir(h.e.uploadsDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestInjects_TeamSeesOnlyItsOwn(t *testing.T) {
	h := newInjectsHarness(t, "")
	h.openInject(t, TeamBlue)
	red := h.openInject(t, TeamRed)

	if got, err := h.e.Injects(TeamBlue); err != nil || len(got) != 1 || got[0].Team != TeamBlue {
		t.Fatalf("Injects(blue) = %+v, %v; want just the blue one", got, err)
	}
	if _, err := h.e.Inject(TeamBlue, red.ID); !errors.Is(err, ErrInjectNotFound) {
		t.Errorf("Inject(blue, red's id) err = %v, want ErrInjectNotFound", err)
	}
	if _, err := h.e.Submit(TeamBlue, red.ID, "", "a.txt", strings.NewReader("x")); !errors.Is(err, ErrInjectNotFound) {
		t.Errorf("Submit(blue, red's id) err = %v, want ErrInjectNotFound", err)
	}
}

func TestSubmit_OnlyWhileAccepting(t *testing.T) {
	h := newInjectsHarness(t, "")
	in, err := h.e.CreateInject(InjectInput{Title: "t", Team: TeamBlue, Deadline: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Submit(TeamBlue, in.ID, "", "a.txt", strings.NewReader("x")); !errors.Is(err, ErrInjectClosed) {
		t.Errorf("closed: err = %v, want ErrInjectClosed", err)
	}
	if _, err := h.e.UpdateInject(in.ID, "t", time.Now().Add(-time.Second), true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Submit(TeamBlue, in.ID, "", "a.txt", strings.NewReader("x")); !errors.Is(err, ErrInjectClosed) {
		t.Errorf("past deadline: err = %v, want ErrInjectClosed", err)
	}

	in = h.openInject(t, TeamBlue)
	if _, err := h.e.Submit(TeamBlue, in.ID, "first", "a.txt", strings.NewReader("one")); err != nil {
		t.Fatal(err)
	}
	got, err := h.e.Submit(TeamBlue, in.ID, "second", "b.txt", strings.NewReader("two"))
	if err != nil || got.Submission == nil || got.Submission.FileName != "b.txt" || got.Submission.Note != "second" {
		t.Fatalf("resubmit = %+v, %v", got, err)
	}
	path, name, err := h.e.SubmissionFile(TeamBlue, in.ID)
	if err != nil || name != "b.txt" {
		t.Fatalf("SubmissionFile = %q, %q, %v", path, name, err)
	}
	if b, _ := os.ReadFile(path); string(b) != "two" {
		t.Errorf("stored file = %q, want two", b)
	}
	if left := uploadsLeft(t, h); len(left) != 1 {
		t.Errorf("uploads = %v, want only the latest submission", left)
	}
}

func TestSubmit_TooLarge(t *testing.T) {
	h := newInjectsHarness(t, "")
	in := h.openInject(t, TeamBlue)
	big := io.LimitReader(zeros{}, MaxUploadBytes+1)
	if _, err := h.e.Submit(TeamBlue, in.ID, "", "big.bin", big); !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("err = %v, want ErrUploadTooLarge", err)
	}
	if left := uploadsLeft(t, h); len(left) != 0 {
		t.Errorf("uploads = %v, want none", left)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestApprove_MovesPoints(t *testing.T) {
	for _, tc := range []struct {
		name, team, extra string
		grade             int
		wantBlue, wantRed int
	}{
		{"blue inject", TeamBlue, "", 7, 1000 + 21000, 0},
		{"red inject", TeamRed, "starting_score: 100000\n", 7, 100000 - 21000, 21000},
		{"configured worth", TeamBlue, "inject_points: 10000\n", 5, 1000 + 5000, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newInjectsHarness(t, tc.extra)
			in := h.gradedInject(t, tc.team, tc.grade)
			got, err := h.e.ApproveInject(in.ID)
			if err != nil || got.Points == nil || got.ApprovedAt == nil {
				t.Fatalf("ApproveInject = %+v, %v", got, err)
			}
			h.wantScore(t, tc.wantBlue, tc.wantRed)
		})
	}
}

func TestApprove_Refusals(t *testing.T) {
	h := newInjectsHarness(t, "")
	ungraded := h.openInject(t, TeamBlue)
	if _, err := h.e.Submit(TeamBlue, ungraded.ID, "", "a.txt", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	accepting := h.openInject(t, TeamBlue)
	if _, err := h.e.Submit(TeamBlue, accepting.ID, "", "a.txt", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.GradeInject(accepting.ID, 5); err != nil {
		t.Fatal(err)
	}
	approved := h.gradedInject(t, TeamBlue, 5)
	if _, err := h.e.ApproveInject(approved.ID); err != nil {
		t.Fatal(err)
	}
	empty := h.openInject(t, TeamBlue)

	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"no grade", second(h.e.ApproveInject(ungraded.ID)), ErrNotGradable},
		{"still accepting", second(h.e.ApproveInject(accepting.ID)), ErrNotGradable},
		{"twice", second(h.e.ApproveInject(approved.ID)), ErrInjectApproved},
		{"grade 11", second(h.e.GradeInject(ungraded.ID, 11)), ErrInvalidGrade},
		{"grade with no submission", second(h.e.GradeInject(empty.ID, 5)), ErrNotGradable},
		{"regrade once approved", second(h.e.GradeInject(approved.ID, 9)), ErrInjectApproved},
	} {
		if !errors.Is(tc.err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, tc.err, tc.want)
		}
	}
}

func second[T any](_ T, err error) error { return err }

func TestDeleteInject_RestoresScoreAndFiles(t *testing.T) {
	h := newInjectsHarness(t, "")
	in := h.gradedInject(t, TeamRed, 7)
	if err := h.e.SetInjectPDF(in.ID, "task.pdf", strings.NewReader("%PDF-1.4 x")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.ApproveInject(in.ID); err != nil {
		t.Fatal(err)
	}
	h.wantScore(t, 0, 1000) // floored at blue's 1000

	if err := h.e.DeleteInject(in.ID); err != nil {
		t.Fatal(err)
	}
	h.wantScore(t, 1000, 0)
	if left := uploadsLeft(t, h); len(left) != 0 {
		t.Errorf("uploads = %v, want none", left)
	}
	if all, err := h.e.Injects(""); err != nil || len(all) != 0 {
		t.Errorf("Injects = %+v, %v; want none", all, err)
	}
}

func TestSetInjectPDF_RejectsNonPDF(t *testing.T) {
	h := newInjectsHarness(t, "")
	in := h.openInject(t, TeamBlue)
	if err := h.e.SetInjectPDF(in.ID, "x.pdf", strings.NewReader("hello")); !errors.Is(err, ErrNotPDF) {
		t.Errorf("err = %v, want ErrNotPDF", err)
	}
}

func TestResetRound_KeepsInjectsDropsSubmissions(t *testing.T) {
	h := newInjectsHarness(t, "")
	in := h.gradedInject(t, TeamBlue, 5)
	if _, err := h.e.ResetRound(false); err != nil {
		t.Fatal(err)
	}
	got, err := h.e.Inject("", in.ID)
	if err != nil || got.Submission != nil || got.Grade != nil || got.Open {
		t.Fatalf("after reset = %+v, %v; want kept, closed, no submission or grade", got, err)
	}
	if left := uploadsLeft(t, h); len(left) != 0 {
		t.Errorf("uploads = %v, want none", left)
	}
}

func TestScoreBreakdown_Totals(t *testing.T) {
	h := newInjectsHarness(t, "inject_points: 3000\n")
	in := h.gradedInject(t, TeamRed, 3)
	if _, err := h.e.ApproveInject(in.ID); err != nil {
		t.Fatal(err)
	}
	b, err := h.e.ScoreBreakdown()
	if err != nil {
		t.Fatal(err)
	}
	if b.Blue.Injects != -900 || b.Red.Injects != 900 || b.Red.ScavengerHunt != 0 {
		t.Errorf("breakdown = %+v, want injects -900/+900", b)
	}
	s := h.status(t)
	if b.Blue.Total != s.BlueScore || b.Red.Total != s.RedScore {
		t.Errorf("totals (%d, %d) != status (%d, %d)", b.Blue.Total, b.Red.Total, s.BlueScore, s.RedScore)
	}
}

func TestTeamEnabled(t *testing.T) {
	h := newHarness(t, okCheck, "web", "db")
	h.writeConfig(t, credentialsConfig()+fmt.Sprintf("red_team:\n  username: red\n  password_hash: %q\n", adminHash))
	if _, err := h.e.Reload(""); err != nil {
		t.Fatal(err)
	}
	if h.e.TeamEnabled(TeamRed) || h.status(t).InjectsEnabled {
		t.Error("red on without EnableRedTeamLogin")
	}
	h.e.EnableRedTeamLogin()
	if !h.e.TeamEnabled(TeamRed) || !h.status(t).InjectsEnabled {
		t.Error("red off with EnableRedTeamLogin and a red_team section")
	}
}
