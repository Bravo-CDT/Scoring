package api

import (
	"errors"
	"net/http"

	"github.com/Bravo-CDT/Scoring/internal/engine"
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
