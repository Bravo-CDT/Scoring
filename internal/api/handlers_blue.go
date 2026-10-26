package api

import (
	"errors"
	"net/http"

	"github.com/Bravo-CDT/Scoring/internal/auth"
	"github.com/Bravo-CDT/Scoring/internal/engine"
)

// WithBlueTeam turns on the blue team credentials routes, with a as the blue
// team's login. Without it every /blue route answers 404.
func WithBlueTeam(a *auth.Auth) Option {
	return func(s *server) { s.blueAuth = a }
}

const blueTeamOff = "blue team credentials are not configured"

// routeBlueTeam registers the blue team credentials routes.
func (s *server) routeBlueTeam() {
	s.mux.HandleFunc("POST /blue/login", s.blueOnly(s.handleBlueLogin))
	s.mux.HandleFunc("GET /blue/credentials", s.blueOnly(s.requireToken(true, true, s.handleListCredentials)))
	s.mux.HandleFunc("PUT /blue/credentials/{service}", s.blueOnly(s.requireToken(true, false, s.handleSetCredential)))
	s.mux.HandleFunc("POST /blue/credentials/{service}/reset", s.blueOnly(s.requireToken(false, true, s.handleResetCredential)))
}

// blueOnly answers 404 while the blue team login is off, or the config last
// loaded has no blue_team section.
func (s *server) blueOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.blueAuth == nil || !s.engine.CredentialsEnabled() {
			writeError(w, http.StatusNotFound, blueTeamOff)
			return
		}
		h(w, r)
	}
}

// requireToken lets a request through only with a blue team token (if blue)
// or an admin token (if admin), answering 401 otherwise like RequireAdmin.
func (s *server) requireToken(blue, admin bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if (blue && s.blueAuth.IsAuthenticated(r)) || (admin && s.auth.IsAuthenticated(r)) {
			h(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
	}
}

// handleBlueLogin serves POST /blue/login: {username, password} → {token}.
func (s *server) handleBlueLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeBodyError(w, err)
		return
	}
	token, ok := s.blueAuth.Login(req.Username, req.Password)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{Token: token})
}

// handleListCredentials serves GET /blue/credentials.
func (s *server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	creds, err := s.engine.Credentials()
	if err != nil {
		s.adminError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, creds)
}

type setCredentialRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleSetCredential serves PUT /blue/credentials/{service}:
// {username, password} → the service's entry.
func (s *server) handleSetCredential(w http.ResponseWriter, r *http.Request) {
	var req setCredentialRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeBodyError(w, err)
		return
	}
	entry, err := s.engine.SetCredential(r.PathValue("service"), req.Username, req.Password)
	if err != nil {
		s.credentialError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// handleResetCredential serves POST /blue/credentials/{service}/reset → the
// service's entry, back on its defaults.
func (s *server) handleResetCredential(w http.ResponseWriter, r *http.Request) {
	entry, err := s.engine.ResetCredential(r.PathValue("service"))
	if err != nil {
		s.credentialError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// credentialError answers a refused credential change with 400 and the
// engine's message, and anything else like adminError.
func (s *server) credentialError(w http.ResponseWriter, r *http.Request, err error) {
	for _, refused := range []error{engine.ErrNotCredentialed, engine.ErrUserNotAllowed, engine.ErrInvalidPassword} {
		if errors.Is(err, refused) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	s.adminError(w, r, err)
}
