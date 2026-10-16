package api

import (
	"errors"
	"net/http"
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

type restartRequest struct {
	PauseScoring bool `json:"pause_scoring"`
}

// handleServiceRestart serves POST /services/{name}/restart with an optional
// {"pause_scoring": bool} body.
func (s *server) handleServiceRestart(w http.ResponseWriter, r *http.Request) {
	var req restartRequest
	if err := decodeJSON(w, r, &req); err != nil && !errors.Is(err, errEmptyBody) {
		writeBodyError(w, err)
		return
	}
	if err := s.engine.RestartService(r.PathValue("name"), req.PauseScoring); err != nil {
		s.adminError(w, r, err)
		return
	}
	s.writeCurrentStatus(w, r)
}

// writeCurrentStatus answers a successful admin action with the engine's
// status. Should reading it fail, the 500 says the action itself took effect.
func (s *server) writeCurrentStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.engine.Status()
	if err != nil {
		s.logger.Printf("api: %s %s: reading status after the action: %v", r.Method, r.URL.Path, err)
		writeError(w, http.StatusInternalServerError, "action applied, but reading the current status failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

type reloadResponse struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Error   string   `json:"error,omitempty"`
}

// handleReload serves POST /reload. On failure the 500 body carries, beside
// the error, whatever changes were made before it.
func (s *server) handleReload(w http.ResponseWriter, r *http.Request) {
	added, removed, err := s.engine.Reload(s.configPath)
	resp := reloadResponse{Added: nonNil(added), Removed: nonNil(removed)}
	if err != nil {
		s.logger.Printf("api: %s %s: %v", r.Method, r.URL.Path, err)
		resp.Error = err.Error()
		writeJSON(w, http.StatusInternalServerError, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
