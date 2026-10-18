// Package api is the scoring engine's HTTP interface: public read-only
// endpoints (status, events, check results) and admin control endpoints
// behind a bearer token from POST /login.
//
// Every response, errors included, is JSON, except those of the optional
// fallback handler (WithFallback) that unmatched reads go to. Check result
// detail text is public; event detail text is only exposed to a request
// carrying a valid admin token. Public endpoints report internal errors with a generic message
// (the cause is logged instead); admin endpoints report the cause directly.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/Bravo-CDT/Scoring/internal/auth"
	"github.com/Bravo-CDT/Scoring/internal/engine"
	"github.com/Bravo-CDT/Scoring/internal/store"
)

const (
	defaultPageLimit = 50
	maxPageLimit     = 500
	// maxBodyBytes bounds every request body the API reads; the largest
	// legitimate one is a login.
	maxBodyBytes = 4 << 10

	genericInternalError = "internal server error"
)

// server holds the API's dependencies; its methods are the handlers.
type server struct {
	engine     *engine.Engine
	store      *store.Store
	sessionID  int64
	auth       *auth.Auth
	configPath string
	logger     *log.Logger
	mux        *http.ServeMux
	// fallback, if set, serves GET and HEAD requests that match no route.
	fallback http.Handler
}

// Option configures NewServer.
type Option func(*server)

// WithFallback hands GET and HEAD requests whose path matches no API route
// to h instead of answering them with the API's JSON 404, so the same server
// can serve the frontend. Requests that match a route path but not its
// method still get the API's JSON 405, and other methods on unknown paths
// still get its JSON 404.
func WithFallback(h http.Handler) Option {
	return func(s *server) { s.fallback = h }
}

// NewServer builds the full route table: public read-only endpoints, admin
// endpoints behind a.RequireAdmin, and /login. configPath is threaded through
// to the /reload handler; empty means the path the engine was built with.
// Internal errors are logged with the standard logger.
func NewServer(e *engine.Engine, st *store.Store, sessionID int64, a *auth.Auth, configPath string, opts ...Option) http.Handler {
	return newServer(e, st, sessionID, a, configPath, log.Default(), opts...)
}

func newServer(e *engine.Engine, st *store.Store, sessionID int64, a *auth.Auth, configPath string, logger *log.Logger, opts ...Option) *server {
	s := &server{
		engine:     e,
		store:      st,
		sessionID:  sessionID,
		auth:       a,
		configPath: configPath,
		logger:     logger,
		mux:        http.NewServeMux(),
	}
	for _, opt := range opts {
		opt(s)
	}

	s.mux.HandleFunc("GET /status", s.handleStatus)
	s.mux.HandleFunc("GET /events", s.handleEvents)
	s.mux.HandleFunc("GET /services/{name}/results", s.handleResults)

	s.mux.HandleFunc("POST /login", s.handleLogin)

	admin := func(pattern string, h http.HandlerFunc) { s.mux.Handle(pattern, a.RequireAdmin(h)) }
	admin("POST /engine/start", s.handleEngineStart)
	admin("POST /engine/stop", s.engineAction(e.Stop))
	admin("POST /scoring/pause", s.engineAction(e.PauseScoring))
	admin("POST /scoring/resume", s.engineAction(e.ResumeScoring))
	admin("POST /countdown", s.handleCountdown)
	admin("POST /services/{name}/start", s.handleServiceStart)
	admin("POST /services/{name}/stop", s.serviceAction(e.StopService))
	admin("POST /services/{name}/restart", s.handleServiceRestart)
	admin("POST /services/{name}/scoring/pause", s.serviceAction(e.PauseServiceScoring))
	admin("POST /services/{name}/scoring/resume", s.serviceAction(e.ResumeServiceScoring))
	admin("POST /reload", s.handleReload)

	return s
}

// ServeHTTP dispatches to the mux, rewriting the mux's own plain-text 404 and
// 405 responses as JSON (keeping the 405's Allow header). An unmatched GET or
// HEAD goes to the fallback instead, if there is one.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, pattern := s.mux.Handler(r); pattern != "" {
		s.mux.ServeHTTP(w, r)
		return
	}
	rec := &headerRecorder{header: make(http.Header)}
	s.mux.ServeHTTP(rec, r)
	status := rec.status
	if status == 0 {
		status = http.StatusNotFound
	}
	if status == http.StatusNotFound && s.fallback != nil &&
		(r.Method == http.MethodGet || r.Method == http.MethodHead) {
		s.fallback.ServeHTTP(w, r)
		return
	}
	for _, h := range []string{"Allow", "Location"} {
		if v := rec.header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	writeError(w, status, http.StatusText(status))
}

// headerRecorder captures the status and headers of a response, discarding
// its body.
type headerRecorder struct {
	header http.Header
	status int
}

func (r *headerRecorder) Header() http.Header { return r.header }

func (r *headerRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func (r *headerRecorder) Write(p []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	return len(p), nil
}

type errorResponse struct {
	Error string `json:"error"`
}

// writeJSON writes v as the JSON response body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// Only a programming error (an unencodable type) gets here.
		status, body = http.StatusInternalServerError, []byte(`{"error":"`+genericInternalError+`"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// publicError answers a public request whose handling failed internally. The
// cause, which can name file paths or SQL, is logged rather than returned.
func (s *server) publicError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Printf("api: %s %s: %v", r.Method, r.URL.Path, err)
	writeError(w, http.StatusInternalServerError, genericInternalError)
}

// adminError answers a failed admin action: 404 for an unknown service,
// otherwise 500 with the cause, which the admin needs to act on.
func (s *server) adminError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, engine.ErrServiceNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	s.logger.Printf("api: %s %s: %v", r.Method, r.URL.Path, err)
	writeError(w, http.StatusInternalServerError, err.Error())
}

// parseLimit reads the limit query parameter: defaultPageLimit when absent or
// not a positive integer, capped at maxPageLimit.
func parseLimit(r *http.Request) int {
	n, err := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64)
	switch {
	case errors.Is(err, strconv.ErrRange) && n > 0: // too large for int64
		return maxPageLimit
	case err != nil || n < 1:
		return defaultPageLimit
	case n > maxPageLimit:
		return maxPageLimit
	default:
		return int(n)
	}
}

// parseBefore reads the before query parameter, the paging cursor: 0 (the
// first page) when absent or not a non-negative integer.
func parseBefore(r *http.Request) int64 {
	n, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

var (
	errEmptyBody    = errors.New("request body is required")
	errTrailingData = errors.New("request body must hold a single JSON value")
)

// decodeJSON decodes the request body, which must be exactly one JSON value
// of at most maxBodyBytes with no fields dst does not know, into dst. An
// empty body is errEmptyBody.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errEmptyBody
		}
		return err
	}
	switch err := dec.Decode(&json.RawMessage{}); {
	case errors.Is(err, io.EOF):
		return nil
	case err == nil:
		return errTrailingData
	default:
		return err
	}
}

// writeBodyError answers a request whose body decodeJSON rejected.
func writeBodyError(w http.ResponseWriter, err error) {
	if maxErr := (*http.MaxBytesError)(nil); errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
}
