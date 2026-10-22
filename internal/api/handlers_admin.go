package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/engine"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token string `json:"token"`
}

// handleLogin serves POST /login: {username, password} → {token}.
func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeBodyError(w, err)
		return
	}
	token, ok := s.auth.Login(req.Username, req.Password)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{Token: token})
}

// engineAction serves an engine-wide admin action (start, stop, pause or
// resume scoring). The engine's actions are idempotent, so repeating one is
// a 200 too; either way the response is the current status.
func (s *server) engineAction(action func() error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := action(); err != nil {
			s.adminError(w, r, err)
			return
		}
		s.writeCurrentStatus(w, r)
	}
}

// serviceAction serves a per-service admin action on the {name} path
// segment, answering like engineAction.
func (s *server) serviceAction(action func(name string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := action(r.PathValue("name")); err != nil {
			s.adminError(w, r, err)
			return
		}
		s.writeCurrentStatus(w, r)
	}
}

// The routes that start routines (engine start, service start and restart,
// reload) report the routines that failed to start in a "failed" list of
// {"service", "error"} objects. It is always present, empty when everything
// started, so a client can check it unconditionally; the error text is the
// cause (admin-only routes, so it is not hidden).

// startResponse is the status answer of a route that starts routines.
type startResponse struct {
	engine.Status
	Failed []engine.StartFailure `json:"failed"`
}

// startErrorResponse answers a single-service start or restart whose routine
// failed to start.
type startErrorResponse struct {
	Error  string                `json:"error"`
	Failed []engine.StartFailure `json:"failed"`
}

// handleEngineStart serves POST /engine/start. Routines that fail to start do
// not fail the request: the engine is running and the others are checking,
// so the answer is 200 with the current status and the failures listed.
func (s *server) handleEngineStart(w http.ResponseWriter, r *http.Request) {
	failed, err := s.engine.Start()
	if err != nil {
		s.adminError(w, r, err)
		return
	}
	s.writeStartStatus(w, r, failed)
}

// handleServiceStart serves POST /services/{name}/start; see serviceStarted.
func (s *server) handleServiceStart(w http.ResponseWriter, r *http.Request) {
	s.serviceStarted(w, r, s.engine.StartService(r.PathValue("name")))
}

type restartRequest struct {
	PauseScoring bool `json:"pause_scoring"`
}

// handleServiceRestart serves POST /services/{name}/restart with an optional
// {"pause_scoring": bool} body; see serviceStarted.
func (s *server) handleServiceRestart(w http.ResponseWriter, r *http.Request) {
	var req restartRequest
	if err := decodeJSON(w, r, &req); err != nil && !errors.Is(err, errEmptyBody) {
		writeBodyError(w, err)
		return
	}
	s.serviceStarted(w, r, s.engine.RestartService(r.PathValue("name"), req.PauseScoring))
}

// serviceStarted answers a single-service start or restart. Its routine
// failing to start is the one thing the admin asked for not happening, so it
// is a 500 whose body has the reason as "error" and in "failed". The rest of
// the request has still been applied: the service is enabled (it starts with
// the next engine start once its config is fixed) and a requested scoring
// pause is persisted. Success is 200 with the status and an empty "failed".
func (s *server) serviceStarted(w http.ResponseWriter, r *http.Request, err error) {
	var startErr *engine.StartError
	switch {
	case errors.As(err, &startErr):
		s.logger.Printf("api: %s %s: %v", r.Method, r.URL.Path, err)
		writeJSON(w, http.StatusInternalServerError, startErrorResponse{
			Error:  err.Error(),
			Failed: []engine.StartFailure{startErr.Failure()},
		})
	case err != nil:
		s.adminError(w, r, err)
	default:
		s.writeStartStatus(w, r, nil)
	}
}

// writeCurrentStatus answers a successful admin action with the engine's
// status. Should reading it fail, the 500 says the action itself took effect.
func (s *server) writeCurrentStatus(w http.ResponseWriter, r *http.Request) {
	if status, ok := s.statusAfterAction(w, r); ok {
		writeJSON(w, http.StatusOK, status)
	}
}

// writeStartStatus is writeCurrentStatus for the routes that start routines:
// the status plus the failed list (never null).
func (s *server) writeStartStatus(w http.ResponseWriter, r *http.Request, failed []engine.StartFailure) {
	if failed == nil {
		failed = []engine.StartFailure{}
	}
	if status, ok := s.statusAfterAction(w, r); ok {
		writeJSON(w, http.StatusOK, startResponse{Status: status, Failed: failed})
	}
}

// statusAfterAction reads the status to answer an admin action with. On
// failure it answers the request itself, saying the action took effect.
func (s *server) statusAfterAction(w http.ResponseWriter, r *http.Request) (engine.Status, bool) {
	status, err := s.engine.Status()
	if err != nil {
		s.logger.Printf("api: %s %s: reading status after the action: %v", r.Method, r.URL.Path, err)
		writeError(w, http.StatusInternalServerError, "action applied, but reading the current status failed: "+err.Error())
		return engine.Status{}, false
	}
	return status, true
}

type countdownRequest struct {
	// Target is an RFC 3339 timestamp, or null to clear the countdown.
	Target *string `json:"target"`
}

// handleCountdown serves POST /countdown with a {"target": "<RFC 3339>" | null}
// body, setting or clearing the time the scoreboard's countdown runs to, and
// answers with the current status.
func (s *server) handleCountdown(w http.ResponseWriter, r *http.Request) {
	var req countdownRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeBodyError(w, err)
		return
	}
	var target *time.Time
	if req.Target != nil {
		t, err := time.Parse(time.RFC3339, *req.Target)
		if err != nil {
			writeError(w, http.StatusBadRequest, "target must be an RFC 3339 timestamp or null")
			return
		}
		target = &t
	}
	if err := s.engine.SetCountdownTarget(target); err != nil {
		s.adminError(w, r, err)
		return
	}
	s.writeCurrentStatus(w, r)
}

type reloadResponse struct {
	engine.ReloadResult
	Error string `json:"error,omitempty"`
}

// handleReload serves POST /reload: {"added", "removed", "failed"}. An added
// service whose routine fails to start is still added, so that is a 200 with
// the failure in "failed". A reload that fails as a whole (an invalid file, a
// store error) is a 500 whose body carries, beside the error, whatever
// changes were made before it.
func (s *server) handleReload(w http.ResponseWriter, r *http.Request) {
	res, err := s.engine.Reload(s.configPath)
	resp := reloadResponse{ReloadResult: res}
	if err != nil {
		s.logger.Printf("api: %s %s: %v", r.Method, r.URL.Path, err)
		resp.Error = err.Error()
		writeJSON(w, http.StatusInternalServerError, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// writeConflict answers a precondition failure (scoring not paused, engine
// not stopped) with 409 and the error's message.
func (s *server) writeConflict(w http.ResponseWriter, err error) {
	writeError(w, http.StatusConflict, err.Error())
}

type adjustScoreRequest struct {
	Points int    `json:"points"`
	Reason string `json:"reason"`
}

// handleAdjustScore serves POST /teams/{side}/adjust: a 400 for an unknown
// side or zero points, a 409 if scoring is not paused, otherwise the
// current status.
func (s *server) handleAdjustScore(w http.ResponseWriter, r *http.Request) {
	side := r.PathValue("side")
	if side != store.SideBlue && side != store.SideRed {
		writeError(w, http.StatusBadRequest, `side must be "blue" or "red"`)
		return
	}
	var req adjustScoreRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeBodyError(w, err)
		return
	}
	if req.Points == 0 {
		writeError(w, http.StatusBadRequest, "points must not be zero")
		return
	}

	if err := s.engine.AdjustScore(side, req.Points, req.Reason); err != nil {
		if errors.Is(err, engine.ErrScoringNotPaused) {
			s.writeConflict(w, err)
			return
		}
		s.adminError(w, r, err)
		return
	}
	s.writeCurrentStatus(w, r)
}

// backupResponse is the status answer of POST /backup, plus the backup's
// path.
type backupResponse struct {
	engine.Status
	BackupPath string `json:"backup_path"`
}

// handleBackup serves POST /backup: a 409 while the engine is running,
// otherwise the backup's path alongside the current status.
func (s *server) handleBackup(w http.ResponseWriter, r *http.Request) {
	path, err := s.engine.BackupDatabase()
	if err != nil {
		if errors.Is(err, engine.ErrEngineRunning) {
			s.writeConflict(w, err)
			return
		}
		s.adminError(w, r, err)
		return
	}
	if status, ok := s.statusAfterAction(w, r); ok {
		writeJSON(w, http.StatusOK, backupResponse{Status: status, BackupPath: path})
	}
}

type resetRoundRequest struct {
	Backup bool `json:"backup"`
}

// resetRoundResponse is the status answer of POST /db/reset-round, plus the
// backup's path when one was taken (omitted otherwise).
type resetRoundResponse struct {
	engine.Status
	BackupPath string `json:"backup_path,omitempty"`
}

// handleResetRound serves POST /db/reset-round: a 409 while the engine is
// running, otherwise resets this session's score and check-result history,
// answering with the backup's path (if requested) alongside current status.
func (s *server) handleResetRound(w http.ResponseWriter, r *http.Request) {
	var req resetRoundRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeBodyError(w, err)
		return
	}

	path, err := s.engine.ResetRound(req.Backup)
	if err != nil {
		if errors.Is(err, engine.ErrEngineRunning) {
			s.writeConflict(w, err)
			return
		}
		s.adminError(w, r, err)
		return
	}
	if status, ok := s.statusAfterAction(w, r); ok {
		writeJSON(w, http.StatusOK, resetRoundResponse{Status: status, BackupPath: path})
	}
}
