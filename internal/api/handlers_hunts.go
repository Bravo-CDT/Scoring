package api

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/Bravo-CDT/Scoring/internal/auth"
	"github.com/Bravo-CDT/Scoring/internal/engine"
)

// routeHunts registers the scavenger hunt routes.
func (s *server) routeHunts(a *auth.Auth) {
	s.mux.HandleFunc("GET /team/hunts", s.handleTeamHunts)
	s.mux.HandleFunc("POST /team/hunts/{id}/claim", s.handleClaimHunt)

	admin := func(pattern string, h http.HandlerFunc) { s.mux.Handle(pattern, a.RequireAdmin(h)) }
	admin("POST /admin/hunts", s.handleCreateHunt)
	admin("PUT /admin/hunts/{id}", s.handleUpdateHunt)
	admin("DELETE /admin/hunts/{id}", s.handleDeleteHunt)
}

// handleTeamHunts serves GET /team/hunts: the team's hunts, or every hunt
// (with its code) for an admin token.
func (s *server) handleTeamHunts(w http.ResponseWriter, r *http.Request) {
	team, admin := s.team(r)
	if team == "" && !admin {
		unauthorized(w)
		return
	}
	list, err := s.engine.Hunts(team)
	if err != nil {
		s.huntError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

type claimRequest struct {
	Code string `json:"code"`
}

// handleClaimHunt serves POST /team/hunts/{id}/claim: {code}. A claim needs a
// team token; the admin can't claim.
func (s *server) handleClaimHunt(w http.ResponseWriter, r *http.Request) {
	team, _ := s.team(r)
	if team == "" {
		unauthorized(w)
		return
	}
	id, ok := huntID(w, r)
	if !ok {
		return
	}
	var req claimRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeBodyError(w, err)
		return
	}
	v, err := s.engine.ClaimHunt(team, id, req.Code)
	if err != nil {
		s.huntError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleCreateHunt serves POST /admin/hunts: {title, team, description, code,
// points, deadline} → the new, closed hunt.
func (s *server) handleCreateHunt(w http.ResponseWriter, r *http.Request) {
	var req engine.HuntInput
	if err := decodeJSON(w, r, &req); err != nil {
		writeBodyError(w, err)
		return
	}
	v, err := s.engine.CreateHunt(req)
	if err != nil {
		s.huntError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

type updateHuntRequest struct {
	engine.HuntInput
	Open bool `json:"open"`
}

// handleUpdateHunt serves PUT /admin/hunts/{id}: {title, description, code,
// points, deadline, open}. A hunt's team can't change.
func (s *server) handleUpdateHunt(w http.ResponseWriter, r *http.Request) {
	id, ok := huntID(w, r)
	if !ok {
		return
	}
	var req updateHuntRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeBodyError(w, err)
		return
	}
	v, err := s.engine.UpdateHunt(id, req.HuntInput, req.Open)
	if err != nil {
		s.huntError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleDeleteHunt serves DELETE /admin/hunts/{id} → the status, with any
// points the hunt moved taken back.
func (s *server) handleDeleteHunt(w http.ResponseWriter, r *http.Request) {
	id, ok := huntID(w, r)
	if !ok {
		return
	}
	if err := s.engine.DeleteHunt(id); err != nil {
		s.huntError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func huntID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusNotFound, engine.ErrHuntNotFound.Error())
		return 0, false
	}
	return id, true
}

// huntError answers a refused hunt operation with its status and the engine's
// message. Anything else is logged and, on the team routes, hidden.
func (s *server) huntError(w http.ResponseWriter, r *http.Request, err error) {
	var cool *engine.CooldownError
	switch {
	case errors.Is(err, engine.ErrHuntNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, engine.ErrHuntClosed), errors.Is(err, engine.ErrHuntClaimed):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, engine.ErrWrongCode), errors.Is(err, engine.ErrInvalidHunt):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.As(err, &cool):
		secs := max(int(math.Ceil(cool.RetryAfter.Seconds())), 1)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writeError(w, http.StatusTooManyRequests, cool.Error())
	case s.auth.IsAuthenticated(r):
		s.adminError(w, r, err)
	default:
		s.publicError(w, r, fmt.Errorf("hunts: %w", err))
	}
}
