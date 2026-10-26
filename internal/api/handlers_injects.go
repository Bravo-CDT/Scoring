package api

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/auth"
	"github.com/Bravo-CDT/Scoring/internal/engine"
)

// WithRedTeam turns on the red team's login to the injects routes, with a as
// its login.
func WithRedTeam(a *auth.Auth) Option {
	return func(s *server) { s.redAuth = a }
}

// maxUploadBody bounds a multipart upload: the file plus room for the note
// and the multipart framing.
const maxUploadBody = engine.MaxUploadBytes + 1<<20

// transferTimeout replaces the server's read and write timeouts, which are
// sized for small JSON bodies, on the routes that move whole files: 50 MB
// over a slow link takes minutes.
const transferTimeout = 10 * time.Minute

// allowSlowTransfer gives this request transferTimeout to read its body and
// write its response. Errors are ignored: a writer that can't set deadlines
// (a test recorder) has none to extend.
func allowSlowTransfer(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(transferTimeout)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)
}

// routeInjects registers the injects routes and the public score breakdown.
func (s *server) routeInjects(a *auth.Auth) {
	s.mux.HandleFunc("POST /team/login", s.handleTeamLogin)
	s.mux.HandleFunc("GET /team/injects", s.handleTeamInjects)
	s.mux.HandleFunc("GET /team/injects/{id}/pdf", s.teamFile(s.engine.InjectPDF, "application/pdf"))
	s.mux.HandleFunc("GET /team/injects/{id}/submission", s.teamFile(s.engine.SubmissionFile, "application/octet-stream"))
	s.mux.HandleFunc("PUT /team/injects/{id}/submission", s.handleSubmit)

	admin := func(pattern string, h http.HandlerFunc) { s.mux.Handle(pattern, a.RequireAdmin(h)) }
	admin("POST /admin/injects", s.handleCreateInject)
	admin("PUT /admin/injects/{id}", s.handleUpdateInject)
	admin("PUT /admin/injects/{id}/pdf", s.handleInjectPDF)
	admin("PUT /admin/injects/{id}/grade", s.handleGradeInject)
	admin("POST /admin/injects/{id}/approve", s.injectAction(s.engine.ApproveInject))
	admin("DELETE /admin/injects/{id}", s.handleDeleteInject)

	s.mux.HandleFunc("GET /score/breakdown", s.handleScoreBreakdown)
}

// team returns the team whose login issued the request's token, while that
// team's injects page is on, or admin if it's an admin token.
func (s *server) team(r *http.Request) (team string, admin bool) {
	if s.blueAuth != nil && s.engine.TeamEnabled(engine.TeamBlue) && s.blueAuth.IsAuthenticated(r) {
		return engine.TeamBlue, false
	}
	if s.redAuth != nil && s.engine.TeamEnabled(engine.TeamRed) && s.redAuth.IsAuthenticated(r) {
		return engine.TeamRed, false
	}
	return "", s.auth.IsAuthenticated(r)
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
}

type teamLoginResponse struct {
	Token string `json:"token"`
	Team  string `json:"team"`
}

// handleTeamLogin serves POST /team/login: {username, password} →
// {token, team}, trying the blue team's login and then the red team's.
func (s *server) handleTeamLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeBodyError(w, err)
		return
	}
	enabled := false
	for _, t := range []struct {
		team string
		a    *auth.Auth
	}{{engine.TeamBlue, s.blueAuth}, {engine.TeamRed, s.redAuth}} {
		if t.a == nil || !s.engine.TeamEnabled(t.team) {
			continue
		}
		enabled = true
		if token, ok := t.a.Login(req.Username, req.Password); ok {
			writeJSON(w, http.StatusOK, teamLoginResponse{Token: token, Team: t.team})
			return
		}
	}
	if !enabled {
		writeError(w, http.StatusNotFound, "team logins are not configured")
		return
	}
	writeError(w, http.StatusUnauthorized, "invalid credentials")
}

// handleTeamInjects serves GET /team/injects: the team's injects, or every
// inject for an admin token.
func (s *server) handleTeamInjects(w http.ResponseWriter, r *http.Request) {
	team, admin := s.team(r)
	if team == "" && !admin {
		unauthorized(w)
		return
	}
	list, err := s.engine.Injects(team)
	if err != nil {
		s.injectError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// teamFile serves one of an inject's files, found by find, to its team or an
// admin, as an attachment under its original name.
func (s *server) teamFile(find func(team string, id int64) (string, string, error), contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		team, admin := s.team(r)
		if team == "" && !admin {
			unauthorized(w)
			return
		}
		id, ok := injectID(w, r)
		if !ok {
			return
		}
		path, name, err := find(team, id)
		if err != nil {
			s.injectError(w, r, err)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			s.injectError(w, r, err)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			s.injectError(w, r, err)
			return
		}
		allowSlowTransfer(w)
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, "", info.ModTime(), f)
	}
}

// handleSubmit serves PUT /team/injects/{id}/submission, a multipart form with
// a "note" field and then a "file", for the team's own inject.
func (s *server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	team, _ := s.team(r)
	if team == "" {
		unauthorized(w)
		return
	}
	id, ok := injectID(w, r)
	if !ok {
		return
	}
	var note string
	err := readUpload(w, r, func(p *multipart.Part) error {
		if p.FormName() != "note" {
			return errUnexpectedPart
		}
		b, err := io.ReadAll(io.LimitReader(p, engine.MaxNoteBytes+1))
		note = string(b)
		return err
	}, func(name string, file io.Reader) error {
		v, err := s.engine.Submit(team, id, note, name, file)
		if err == nil {
			writeJSON(w, http.StatusOK, v)
		}
		return err
	})
	if err != nil {
		s.injectError(w, r, err)
	}
}

// handleInjectPDF serves PUT /admin/injects/{id}/pdf, a multipart form with a
// "file".
func (s *server) handleInjectPDF(w http.ResponseWriter, r *http.Request) {
	id, ok := injectID(w, r)
	if !ok {
		return
	}
	err := readUpload(w, r, func(*multipart.Part) error { return errUnexpectedPart }, func(name string, file io.Reader) error {
		if err := s.engine.SetInjectPDF(id, name, file); err != nil {
			return err
		}
		v, err := s.engine.Inject("", id)
		if err == nil {
			writeJSON(w, http.StatusOK, v)
		}
		return err
	})
	if err != nil {
		s.injectError(w, r, err)
	}
}

var (
	errUnexpectedPart = errors.New("unexpected form field")
	errNoFile         = errors.New("the form has no file")
)

// readUpload streams a multipart body, handing each part before "file" to
// field and then "file" itself to file, which must be the last part.
func readUpload(w http.ResponseWriter, r *http.Request, field func(*multipart.Part) error, file func(name string, r io.Reader) error) error {
	allowSlowTransfer(w)
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)
	mr, err := r.MultipartReader()
	if err != nil {
		return badUpload{err}
	}
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return badUpload{errNoFile}
		}
		if err != nil {
			return badUpload{err}
		}
		if p.FormName() == "file" {
			return file(p.FileName(), p)
		}
		if err := field(p); err != nil {
			return badUpload{err}
		}
	}
}

// badUpload is a malformed upload form, answered with 400.
type badUpload struct{ err error }

func (b badUpload) Error() string { return "invalid upload: " + b.err.Error() }
func (b badUpload) Unwrap() error { return b.err }

type updateInjectRequest struct {
	Title    string    `json:"title"`
	Deadline time.Time `json:"deadline"`
	Open     bool      `json:"open"`
}

// handleCreateInject serves POST /admin/injects: {title, team, deadline} →
// the new, closed inject.
func (s *server) handleCreateInject(w http.ResponseWriter, r *http.Request) {
	var req engine.InjectInput
	if err := decodeJSON(w, r, &req); err != nil {
		writeBodyError(w, err)
		return
	}
	v, err := s.engine.CreateInject(req)
	if err != nil {
		s.injectError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleUpdateInject serves PUT /admin/injects/{id}: {title, deadline, open}.
func (s *server) handleUpdateInject(w http.ResponseWriter, r *http.Request) {
	id, ok := injectID(w, r)
	if !ok {
		return
	}
	var req updateInjectRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeBodyError(w, err)
		return
	}
	v, err := s.engine.UpdateInject(id, req.Title, req.Deadline, req.Open)
	if err != nil {
		s.injectError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

type gradeRequest struct {
	Grade *int `json:"grade"`
}

// handleGradeInject serves PUT /admin/injects/{id}/grade: {grade}.
func (s *server) handleGradeInject(w http.ResponseWriter, r *http.Request) {
	id, ok := injectID(w, r)
	if !ok {
		return
	}
	var req gradeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeBodyError(w, err)
		return
	}
	if req.Grade == nil {
		writeError(w, http.StatusBadRequest, engine.ErrInvalidGrade.Error())
		return
	}
	v, err := s.engine.GradeInject(id, *req.Grade)
	if err != nil {
		s.injectError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// injectAction adapts an engine operation on one inject to a handler that
// answers with the inject.
func (s *server) injectAction(op func(int64) (engine.InjectView, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := injectID(w, r)
		if !ok {
			return
		}
		v, err := op(id)
		if err != nil {
			s.injectError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}

// handleDeleteInject serves DELETE /admin/injects/{id} → the status, with
// any points the inject moved taken back.
func (s *server) handleDeleteInject(w http.ResponseWriter, r *http.Request) {
	id, ok := injectID(w, r)
	if !ok {
		return
	}
	if err := s.engine.DeleteInject(id); err != nil {
		s.injectError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// handleScoreBreakdown serves GET /score/breakdown, public.
func (s *server) handleScoreBreakdown(w http.ResponseWriter, r *http.Request) {
	b, err := s.engine.ScoreBreakdown()
	if err != nil {
		s.publicError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func injectID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusNotFound, engine.ErrInjectNotFound.Error())
		return 0, false
	}
	return id, true
}

// injectError answers a refused inject operation with its status and the
// engine's message. Anything else is logged and, on the team routes, hidden.
func (s *server) injectError(w http.ResponseWriter, r *http.Request, err error) {
	var maxErr *http.MaxBytesError
	var bad badUpload
	switch {
	case errors.Is(err, engine.ErrInjectNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, engine.ErrInjectClosed), errors.Is(err, engine.ErrInjectApproved):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, engine.ErrInvalidGrade), errors.Is(err, engine.ErrInvalidInject),
		errors.Is(err, engine.ErrInvalidSubmission), errors.Is(err, engine.ErrNotPDF), errors.Is(err, engine.ErrNotGradable):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, engine.ErrUploadTooLarge), errors.As(err, &maxErr):
		writeError(w, http.StatusRequestEntityTooLarge, engine.ErrUploadTooLarge.Error())
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.Error())
	case s.auth.IsAuthenticated(r):
		s.adminError(w, r, err)
	default:
		s.publicError(w, r, fmt.Errorf("injects: %w", err))
	}
}
