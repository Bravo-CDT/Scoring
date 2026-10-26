package engine

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Bravo-CDT/Scoring/internal/store"
)

// The teams an inject can be for.
const (
	TeamBlue = store.SideBlue
	TeamRed  = store.SideRed
)

// MaxUploadBytes bounds a submission file and an inject's PDF.
const MaxUploadBytes = 50 << 20

// Limits on what an admin and a team type in.
const (
	maxInjectTitle = 200
	MaxNoteBytes   = 2000
)

// Errors the inject operations return. Their messages are shown to the
// teams and the admin as they are.
var (
	ErrInjectNotFound    = errors.New("no such inject")
	ErrInjectClosed      = errors.New("this inject is not taking submissions")
	ErrInjectApproved    = errors.New("this inject has already been approved")
	ErrNotGradable       = errors.New("this inject can't be graded or approved yet")
	ErrInvalidGrade      = errors.New("the grade must be a whole number from 0 to 10")
	ErrInvalidInject     = errors.New("an inject needs a title of 1 to 200 characters, a team (blue or red) and a deadline")
	ErrInvalidSubmission = errors.New("the note must be at most 2000 bytes, and the file needs a name")
	ErrNotPDF            = errors.New("the instructions must be a PDF")
	ErrUploadTooLarge    = errors.New("the file is over 50 MB")
)

// SubmissionView is a team's latest submission for an inject.
type SubmissionView struct {
	Note        string    `json:"note"`
	FileName    string    `json:"file_name"`
	Size        int64     `json:"size"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// InjectView is an inject as its team and the admin see it.
type InjectView struct {
	ID       int64     `json:"id"`
	Team     string    `json:"team"`
	Title    string    `json:"title"`
	HasPDF   bool      `json:"has_pdf"`
	Open     bool      `json:"open"`
	Deadline time.Time `json:"deadline"`
	// Accepting is whether the team can submit now: open, before the
	// deadline, and not approved.
	Accepting bool `json:"accepting"`
	// Worth is what a 10/10 is worth under the config last loaded. Once
	// approved, Points is what it actually moved.
	Worth      int             `json:"worth"`
	Grade      *int            `json:"grade,omitempty"`
	Points     *int            `json:"points,omitempty"`
	ApprovedAt *time.Time      `json:"approved_at,omitempty"`
	Submission *SubmissionView `json:"submission,omitempty"`
}

// InjectInput is a new inject.
type InjectInput struct {
	Title    string    `json:"title"`
	Team     string    `json:"team"`
	Deadline time.Time `json:"deadline"`
}

// TeamBreakdown is one team's score split by where the points came from.
// Total is the team's score.
type TeamBreakdown struct {
	Starting      int `json:"starting"`
	Services      int `json:"services"`
	Injects       int `json:"injects"`
	Manual        int `json:"manual"`
	ScavengerHunt int `json:"scavenger_hunt"`
	Total         int `json:"total"`
}

// ScoreBreakdown is both teams' breakdowns.
type ScoreBreakdown struct {
	Blue TeamBreakdown `json:"blue"`
	Red  TeamBreakdown `json:"red"`
}

// SetUploadsDir sets the directory submissions and inject PDFs are kept in.
// It is created on the first upload.
func (e *Engine) SetUploadsDir(dir string) { e.uploadsDir = dir }

// EnableRedTeamLogin records that the app has a red team login. Until it is
// called red injects can't be seen or submitted by the red team.
func (e *Engine) EnableRedTeamLogin() { e.redLogin.Store(true) }

// TeamEnabled is whether team can log in to its injects page: the app has
// its login and the config last loaded has its section.
func (e *Engine) TeamEnabled(team string) bool {
	switch team {
	case TeamBlue:
		return e.CredentialsEnabled()
	case TeamRed:
		return e.redLogin.Load() && e.currentLayout().redTeam
	}
	return false
}

// Injects lists team's injects, or every inject when team is "" (the admin).
func (e *Engine) Injects(team string) ([]InjectView, error) {
	ins, err := e.store.ListInjects(e.session.ID, team)
	if err != nil {
		return nil, fmt.Errorf("engine: listing injects: %w", err)
	}
	out := make([]InjectView, 0, len(ins))
	for _, in := range ins {
		v, err := e.view(in, team != "")
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// Inject returns one inject for team, or for the admin when team is "". Another
// team's inject is ErrInjectNotFound, as if it didn't exist.
func (e *Engine) Inject(team string, id int64) (InjectView, error) {
	in, err := e.getInject(team, id)
	if err != nil {
		return InjectView{}, err
	}
	return e.view(in, team != "")
}

// CreateInject adds a closed inject, as the admin.
func (e *Engine) CreateInject(input InjectInput) (InjectView, error) {
	title := strings.TrimSpace(input.Title)
	if !validTitle(title) || (input.Team != TeamBlue && input.Team != TeamRed) || input.Deadline.IsZero() {
		return InjectView{}, ErrInvalidInject
	}
	e.injectMu.Lock()
	defer e.injectMu.Unlock()
	in, err := e.store.CreateInject(e.session.ID, input.Team, title, input.Deadline)
	if err != nil {
		return InjectView{}, fmt.Errorf("engine: creating inject: %w", err)
	}
	if err := e.logInjectEvent(ActorAdmin, EventInjectCreated, in.ID, "created",
		fmt.Sprintf("%s, for %s, due %s", in.Title, in.Team, in.Deadline.Format(time.RFC3339))); err != nil {
		return InjectView{}, err
	}
	return e.view(in, false)
}

// UpdateInject changes an inject's title, deadline and whether it's open, as
// the admin. An approved inject can't be changed.
func (e *Engine) UpdateInject(id int64, title string, deadline time.Time, open bool) (InjectView, error) {
	title = strings.TrimSpace(title)
	if !validTitle(title) || deadline.IsZero() {
		return InjectView{}, ErrInvalidInject
	}
	e.injectMu.Lock()
	defer e.injectMu.Unlock()
	in, err := e.getInject("", id)
	if err != nil {
		return InjectView{}, err
	}
	if in.ApprovedAt != nil {
		return InjectView{}, ErrInjectApproved
	}
	in.Title, in.Deadline, in.Open = title, deadline.UTC().Truncate(time.Millisecond), open
	if err := e.store.UpdateInject(e.session.ID, in); err != nil {
		return InjectView{}, fmt.Errorf("engine: updating inject: %w", err)
	}
	state := "closed"
	if open {
		state = "open"
	}
	if err := e.logInjectEvent(ActorAdmin, EventInjectUpdated, in.ID, "updated",
		fmt.Sprintf("%s: %s, due %s", in.Title, state, in.Deadline.Format(time.RFC3339))); err != nil {
		return InjectView{}, err
	}
	return e.view(in, false)
}

// SetInjectPDF stores the inject's instructions, replacing any earlier PDF,
// as the admin. Anything that doesn't start like a PDF is ErrNotPDF.
func (e *Engine) SetInjectPDF(id int64, name string, r io.Reader) error {
	if _, err := e.getInject("", id); err != nil {
		return err
	}
	head := make([]byte, 5)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return fmt.Errorf("engine: reading inject PDF: %w", err)
	}
	if !bytes.Equal(head[:n], []byte("%PDF-")) {
		return ErrNotPDF
	}
	tmp, _, err := e.receive(io.MultiReader(bytes.NewReader(head), r))
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // a no-op once renamed

	e.injectMu.Lock()
	defer e.injectMu.Unlock()
	in, err := e.getInject("", id)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, e.upload(pdfName(id))); err != nil {
		return fmt.Errorf("engine: saving inject PDF: %w", err)
	}
	in.PDFName = cleanFileName(name, "inject.pdf")
	if err := e.store.UpdateInject(e.session.ID, in); err != nil {
		return fmt.Errorf("engine: saving inject PDF: %w", err)
	}
	return e.logInjectEvent(ActorAdmin, EventInjectUpdated, in.ID, "updated", in.Title+": instructions uploaded")
}

// InjectPDF returns the path and original name of the inject's PDF, for team
// or, when team is "", the admin. A team gets it only once the inject has
// been opened.
func (e *Engine) InjectPDF(team string, id int64) (path, name string, err error) {
	in, err := e.getInject(team, id)
	if err != nil {
		return "", "", err
	}
	if !pdfVisible(in, team != "") {
		return "", "", ErrInjectNotFound
	}
	return e.upload(pdfName(id)), in.PDFName, nil
}

// Submit stores the team's file and note as its submission for the inject,
// replacing any earlier one, while the inject is accepting submissions.
func (e *Engine) Submit(team string, id int64, note, fileName string, r io.Reader) (InjectView, error) {
	if len(note) > MaxNoteBytes {
		return InjectView{}, ErrInvalidSubmission
	}
	// Checked before the upload is read, so a closed inject doesn't take
	// 50 MB of disk first, and again below before it's kept.
	in, err := e.getInject(team, id)
	if err != nil {
		return InjectView{}, err
	}
	if !accepting(in, time.Now()) {
		return InjectView{}, ErrInjectClosed
	}
	tmp, size, err := e.receive(r)
	if err != nil {
		return InjectView{}, err
	}
	defer os.Remove(tmp) // a no-op once renamed

	e.injectMu.Lock()
	defer e.injectMu.Unlock()
	if in, err = e.getInject(team, id); err != nil {
		return InjectView{}, err
	}
	if !accepting(in, time.Now()) {
		return InjectView{}, ErrInjectClosed
	}
	stored := "submission-" + strconv.FormatInt(id, 10) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.Rename(tmp, e.upload(stored)); err != nil {
		return InjectView{}, fmt.Errorf("engine: saving submission: %w", err)
	}
	prev, err := e.store.PutSubmission(e.session.ID, store.Submission{
		InjectID: id, Note: note, FileName: cleanFileName(fileName, "submission"),
		StoredName: stored, Size: size, SubmittedAt: time.Now().UTC(),
	})
	if err != nil {
		os.Remove(e.upload(stored))
		return InjectView{}, fmt.Errorf("engine: saving submission: %w", err)
	}
	if prev != nil {
		e.removeUpload(prev.StoredName)
	}
	actor := ActorBlueTeam
	if team == TeamRed {
		actor = ActorRedTeam
	}
	if err := e.logInjectEvent(actor, EventInjectSubmitted, in.ID, "submitted", in.Title); err != nil {
		return InjectView{}, err
	}
	return e.view(in, true)
}

// SubmissionFile returns the path and original name of the inject's
// submission, for team or, when team is "", the admin.
func (e *Engine) SubmissionFile(team string, id int64) (path, name string, err error) {
	if _, err := e.getInject(team, id); err != nil {
		return "", "", err
	}
	sub, ok, err := e.store.GetSubmission(e.session.ID, id)
	if err != nil {
		return "", "", fmt.Errorf("engine: loading submission: %w", err)
	}
	if !ok {
		return "", "", ErrInjectNotFound
	}
	return e.upload(sub.StoredName), sub.FileName, nil
}

// GradeInject sets the inject's grade out of 10, as the grey team. It can be
// changed until the inject is approved; nothing moves until then.
func (e *Engine) GradeInject(id int64, grade int) (InjectView, error) {
	if grade < 0 || grade > 10 {
		return InjectView{}, ErrInvalidGrade
	}
	e.injectMu.Lock()
	defer e.injectMu.Unlock()
	in, err := e.getInject("", id)
	if err != nil {
		return InjectView{}, err
	}
	if in.ApprovedAt != nil {
		return InjectView{}, ErrInjectApproved
	}
	if _, ok, err := e.store.GetSubmission(e.session.ID, id); err != nil {
		return InjectView{}, fmt.Errorf("engine: grading inject: %w", err)
	} else if !ok {
		return InjectView{}, ErrNotGradable
	}
	in.Grade = &grade
	if err := e.store.UpdateInject(e.session.ID, in); err != nil {
		return InjectView{}, fmt.Errorf("engine: grading inject: %w", err)
	}
	return e.view(in, false)
}

// ApproveInject moves the inject's points, as the grey team: grade/10 of
// inject_points, to blue for a blue inject, from blue to red for a red one.
// It needs a grade and the inject no longer accepting submissions, and is
// final.
func (e *Engine) ApproveInject(id int64) (InjectView, error) {
	e.injectMu.Lock()
	defer e.injectMu.Unlock()
	in, err := e.getInject("", id)
	if err != nil {
		return InjectView{}, err
	}
	if in.ApprovedAt != nil {
		return InjectView{}, ErrInjectApproved
	}
	if in.Grade == nil || accepting(in, time.Now()) {
		return InjectView{}, ErrNotGradable
	}
	grade := *in.Grade
	points := grade * e.currentLayout().injectWorth / 10
	from, to := store.SideManual, store.SideBlue
	if in.Team == TeamRed {
		from, to = store.SideBlue, store.SideRed
	}
	reason := fmt.Sprintf("inject %d: %s, graded %d/10", in.ID, in.Title, grade)
	if in, err = e.store.ApproveInject(e.session.ID, id, from, to, grade, points, reason); err != nil {
		if errors.Is(err, store.ErrInjectApproved) {
			return InjectView{}, ErrInjectApproved
		}
		return InjectView{}, fmt.Errorf("engine: approving inject: %w", err)
	}
	if err := e.logInjectEvent(ActorAdmin, EventInjectApproved, in.ID, "approved",
		fmt.Sprintf("%s: %d/10, %d points to %s", in.Title, grade, points, in.Team)); err != nil {
		return InjectView{}, err
	}
	return e.view(in, false)
}

// DeleteInject removes the inject, its submission and files, and takes back
// any points its approval moved, as the admin.
func (e *Engine) DeleteInject(id int64) error {
	e.injectMu.Lock()
	defer e.injectMu.Unlock()
	in, err := e.getInject("", id)
	if err != nil {
		return err
	}
	sub, hasSub, err := e.store.GetSubmission(e.session.ID, id)
	if err != nil {
		return fmt.Errorf("engine: deleting inject: %w", err)
	}
	if _, err := e.store.VoidLedgerEntry(e.session.ID, store.SourceInject, id, "inject deleted"); err != nil {
		return fmt.Errorf("engine: deleting inject: %w", err)
	}
	if err := e.store.DeleteInject(e.session.ID, id); err != nil {
		return fmt.Errorf("engine: deleting inject: %w", err)
	}
	if hasSub {
		e.removeUpload(sub.StoredName)
	}
	if in.PDFName != "" {
		e.removeUpload(pdfName(id))
	}
	detail := in.Title
	if in.Points != nil {
		detail += fmt.Sprintf(": its %d points taken back", *in.Points)
	}
	return e.logInjectEvent(ActorAdmin, EventInjectDeleted, in.ID, "deleted", detail)
}

// ScoreBreakdown splits each team's score by where its points came from.
func (e *Engine) ScoreBreakdown() (ScoreBreakdown, error) {
	blue, red, err := e.store.ScoreBreakdown(e.session.ID)
	if err != nil {
		return ScoreBreakdown{}, fmt.Errorf("engine: score breakdown: %w", err)
	}
	team := func(s store.SideBreakdown) TeamBreakdown {
		return TeamBreakdown{
			Starting: s.Starting, Services: s.Services, Injects: s.Injects, Manual: s.Manual,
			Total: s.Starting + s.Services + s.Injects + s.Manual,
		}
	}
	return ScoreBreakdown{Blue: team(blue), Red: team(red)}, nil
}

// getInject loads inject id for team, or for the admin when team is "".
func (e *Engine) getInject(team string, id int64) (store.Inject, error) {
	in, ok, err := e.store.GetInject(e.session.ID, id)
	if err != nil {
		return store.Inject{}, fmt.Errorf("engine: loading inject: %w", err)
	}
	if !ok || (team != "" && in.Team != team) {
		return store.Inject{}, ErrInjectNotFound
	}
	return in, nil
}

// view is in as the admin sees it, or as its team does when forTeam is set:
// without the grade until it's approved, since the grade is a draft until
// then, and without the instructions until it's first opened.
func (e *Engine) view(in store.Inject, forTeam bool) (InjectView, error) {
	v := InjectView{
		ID: in.ID, Team: in.Team, Title: in.Title, HasPDF: pdfVisible(in, forTeam),
		Open: in.Open, Deadline: in.Deadline, Accepting: accepting(in, time.Now()),
		Worth: e.currentLayout().injectWorth, Grade: in.Grade, Points: in.Points, ApprovedAt: in.ApprovedAt,
	}
	if forTeam && in.ApprovedAt == nil {
		v.Grade = nil
	}
	sub, ok, err := e.store.GetSubmission(e.session.ID, in.ID)
	if err != nil {
		return InjectView{}, fmt.Errorf("engine: loading submission: %w", err)
	}
	if ok {
		v.Submission = &SubmissionView{Note: sub.Note, FileName: sub.FileName, Size: sub.Size, SubmittedAt: sub.SubmittedAt}
	}
	return v, nil
}

// pdfVisible is whether the inject has instructions its viewer may download:
// the admin always, the team once the inject has been opened.
func pdfVisible(in store.Inject, forTeam bool) bool {
	return in.PDFName != "" && (in.Released || !forTeam)
}

// logInjectEvent logs an inject event. Events are public, so the message
// names the inject only by id; its title, team and grade go in the detail,
// which only admins see.
func (e *Engine) logInjectEvent(actor Actor, eventType string, id int64, verb, detail string) error {
	return e.logEvent(store.Event{
		Level: "info", Actor: string(actor), Type: eventType,
		Message: fmt.Sprintf("inject %d %s", id, verb), Detail: detail,
	})
}

func accepting(in store.Inject, now time.Time) bool {
	return in.Open && in.ApprovedAt == nil && now.Before(in.Deadline)
}

func validTitle(title string) bool {
	return title != "" && len(title) <= maxInjectTitle && !strings.ContainsFunc(title, unicode.IsControl)
}

// receive copies r into a new temp file in the uploads dir and returns its
// path and size. More than MaxUploadBytes is ErrUploadTooLarge; on any error
// the temp file is gone.
func (e *Engine) receive(r io.Reader) (path string, size int64, err error) {
	if err := os.MkdirAll(e.uploadsDir, 0o700); err != nil {
		return "", 0, fmt.Errorf("engine: creating uploads dir: %w", err)
	}
	f, err := os.CreateTemp(e.uploadsDir, "upload-*")
	if err != nil {
		return "", 0, fmt.Errorf("engine: receiving upload: %w", err)
	}
	size, err = io.Copy(f, io.LimitReader(r, MaxUploadBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && size > MaxUploadBytes {
		err = ErrUploadTooLarge
	}
	if err != nil {
		os.Remove(f.Name())
		if errors.Is(err, ErrUploadTooLarge) {
			return "", 0, err
		}
		return "", 0, fmt.Errorf("engine: receiving upload: %w", err)
	}
	return f.Name(), size, nil
}

func (e *Engine) upload(name string) string { return filepath.Join(e.uploadsDir, name) }

// removeUpload deletes an upload that's no longer referenced. A failure only
// leaves a stray file behind, so it's logged rather than returned.
func (e *Engine) removeUpload(name string) {
	if err := os.Remove(e.upload(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("engine: removing upload %s: %v", name, err)
	}
}

// moveOrRemoveUpload moves an upload into dir, or deletes it when dir is "".
// Like removeUpload, a failure is logged rather than returned.
func (e *Engine) moveOrRemoveUpload(name, dir string) {
	if dir == "" {
		e.removeUpload(name)
		return
	}
	err := os.MkdirAll(dir, 0o700)
	if err == nil {
		err = os.Rename(e.upload(name), filepath.Join(dir, name))
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("engine: keeping upload %s with the backup: %v", name, err)
	}
}

func pdfName(id int64) string { return "inject-" + strconv.FormatInt(id, 10) + ".pdf" }

// cleanFileName keeps just the base name of an uploaded file's name, without
// control characters, or fallback when nothing is left.
func cleanFileName(name, fallback string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || len(name) > 255 {
		return fallback
	}
	return name
}
