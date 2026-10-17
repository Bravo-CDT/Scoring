package api

import (
	"net/http"
	"time"
)

// handleStatus serves GET /status: the engine's point-in-time view, which by
// construction holds no detail text.
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.engine.Status()
	if err != nil {
		s.publicError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// eventResponse is one event as the API shows it. ServiceID is omitted for
// engine-level events; Detail is only filled in for admins. It is a separate
// type from store.Event (which has no JSON tags) so a field added to the row
// is never published by accident.
type eventResponse struct {
	ID        int64     `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Actor     string    `json:"actor"`
	Type      string    `json:"type"`
	Message   string    `json:"message"`
	ServiceID *int64    `json:"service_id,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

// handleEvents serves GET /events?limit=N&before=<id>, newest first.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.store.ListEvents(s.sessionID, parseLimit(r), parseBefore(r))
	if err != nil {
		s.publicError(w, r, err)
		return
	}
	includeDetail := s.auth.IsAuthenticated(r)
	out := make([]eventResponse, len(events))
	for i, ev := range events {
		out[i] = eventResponse{
			ID:        ev.ID,
			Timestamp: ev.Timestamp.UTC(),
			Level:     ev.Level,
			Actor:     ev.Actor,
			Type:      ev.Type,
			Message:   ev.Message,
			ServiceID: ev.ServiceID,
		}
		if includeDetail {
			out[i].Detail = ev.Detail
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// checkResultResponse is one check attempt as the API shows it. Detail, the
// check's explanation of its outcome, is public so the scoreboard can show
// why a service is down; it is omitted only when empty.
type checkResultResponse struct {
	ID        int64     `json:"id"`
	Round     int       `json:"round"`
	Timestamp time.Time `json:"timestamp"`
	Success   bool      `json:"success"`
	LatencyMS int64     `json:"latency_ms"`
	Detail    string    `json:"detail,omitempty"`
}

// handleResults serves GET /services/{name}/results?limit=N&before=<id>,
// newest first. A service retired by a reload still has its history served.
func (s *server) handleResults(w http.ResponseWriter, r *http.Request) {
	svc, found, err := s.store.GetServiceByName(s.sessionID, r.PathValue("name"))
	if err != nil {
		s.publicError(w, r, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "unknown service")
		return
	}
	results, err := s.store.ListCheckResults(svc.ID, parseLimit(r), parseBefore(r))
	if err != nil {
		s.publicError(w, r, err)
		return
	}
	out := make([]checkResultResponse, len(results))
	for i, cr := range results {
		out[i] = checkResultResponse{
			ID:        cr.ID,
			Round:     cr.Round,
			Timestamp: cr.Timestamp.UTC(),
			Success:   cr.Success,
			LatencyMS: cr.LatencyMS,
			Detail:    cr.Detail,
		}
	}
	writeJSON(w, http.StatusOK, out)
}
